package musterlink

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNS     = "agent-platform"
	testSecret = "klaus-gateway-obo-links"
)

func linkSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testNS},
		Data:       data,
	}
}

func newSecretStoreFor(t *testing.T, client kubernetes.Interface, key []byte) *SecretStore {
	t.Helper()
	s, err := NewSecretStore(client, key, SecretStoreOptions{Namespace: testNS, Name: testSecret}, nil)
	require.NoError(t, err)
	return s
}

func readSecret(t *testing.T, client kubernetes.Interface) *corev1.Secret {
	t.Helper()
	sec, err := client.CoreV1().Secrets(testNS).Get(context.Background(), testSecret, metav1.GetOptions{})
	require.NoError(t, err)
	return sec
}

// updates counts the Secret writes the fake API server has seen; the fake
// tracker does not bump resourceVersion, so this is how a test tells "wrote"
// from "skipped the write".
func updates(client *fake.Clientset) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "secrets" {
			n++
		}
	}
	return n
}

// failVerb makes the fake API server fail every verb call on Secrets with err
// while *on is true (a reactor cannot be removed, so the switch is the way to
// bring the apiserver back).
func failVerb(client *fake.Clientset, verb string, on *bool, err error) {
	client.PrependReactor(verb, "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if *on {
			return true, nil, err
		}
		return false, nil, nil
	})
}

func TestSecretStoreRoundTripAndSharedBackend(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	s := newSecretStoreFor(t, client, key32())

	n, err := s.Check()
	require.NoError(t, err)
	require.Equal(t, 0, n)

	link := &Link{Sub: "muster-1", Email: "a@example.com", RefreshToken: "rt-secret", LinkedAt: time.Now().UTC().Truncate(time.Second)}
	require.NoError(t, s.Put("U1", link))

	got, err := s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, link.Sub, got.Sub)
	require.Equal(t, link.RefreshToken, got.RefreshToken)
	require.True(t, link.LinkedAt.Equal(got.LinkedAt))

	_, err = s.Get("missing")
	require.ErrorIs(t, err, ErrNotLinked)

	// A second store on the same Secret (a second replica) sees the link: the
	// backend is shared, not process-local.
	s2 := newSecretStoreFor(t, client, key32())
	got2, err := s2.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt-secret", got2.RefreshToken)

	require.NoError(t, s2.Delete("U1"))
	_, err = s.Get("U1")
	require.ErrorIs(t, err, ErrNotLinked)
	// Deleting a missing key writes nothing.
	before := updates(client)
	require.NoError(t, s.Delete("U1"))
	require.Equal(t, before, updates(client))
}

func TestSecretStoreEncryptedAtRest(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	s := newSecretStoreFor(t, client, key32())
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "topsecret-refresh-token", Email: "a@example.com"}))

	sec := readSecret(t, client)
	require.Contains(t, sec.Data, "U1")
	require.NotContains(t, string(sec.Data["U1"]), "topsecret-refresh-token", "refresh token must not appear in plaintext in the Secret")
	require.NotContains(t, string(sec.Data["U1"]), "a@example.com")

	// The record is the bolt store's format: the same key decrypts it there.
	c, err := newLinkCipher(key32())
	require.NoError(t, err)
	link, err := c.openLink(sec.Data["U1"])
	require.NoError(t, err)
	require.Equal(t, "topsecret-refresh-token", link.RefreshToken)
}

func TestSecretStoreWrongKeyFailsClosed(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	require.NoError(t, newSecretStoreFor(t, client, key32()).Put("U1", &Link{RefreshToken: "rt"}))

	wrong := key32()
	wrong[0] ^= 0xff
	// An undecodable record reads as unlinked (a re-link overwrites it), not
	// as a failed store.
	_, err := newSecretStoreFor(t, client, wrong).Get("U1")
	require.ErrorIs(t, err, ErrNotLinked)
}

func TestSecretStoreRetriesOnConflict(t *testing.T) {
	client := fake.NewClientset(linkSecret(map[string][]byte{}))
	conflicts := 2
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, testSecret, errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	s := newSecretStoreFor(t, client, key32())
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt"}))
	require.Equal(t, 0, conflicts, "every simulated conflict must have been retried")
	got, err := s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt", got.RefreshToken)
}

func TestSecretStoreGivesUpAfterRetryLimit(t *testing.T) {
	client := fake.NewClientset(linkSecret(map[string][]byte{}))
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, testSecret, errors.New("busy"))
	})
	s, err := NewSecretStore(client, key32(), SecretStoreOptions{Namespace: testNS, Name: testSecret, Retries: 2}, nil)
	require.NoError(t, err)
	// Exhausted retries are the caller's to handle: the write is reported
	// failed, and the Secret holds nothing for the user.
	err = s.Put("U1", &Link{RefreshToken: "rt"})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotLinked)
	_, err = s.Get("U1")
	require.ErrorIs(t, err, ErrNotLinked)
}

func TestSecretStoreMissingSecretIsNotCreated(t *testing.T) {
	client := fake.NewClientset()
	s := newSecretStoreFor(t, client, key32())

	_, err := s.Check()
	require.Error(t, err, "a missing Secret must fail the startup check")
	require.True(t, apierrors.IsNotFound(err))

	err = s.Put("U1", &Link{RefreshToken: "rt"})
	require.True(t, apierrors.IsNotFound(err), "the failed write names its cause: %v", err)
	_, err = client.CoreV1().Secrets(testNS).Get(context.Background(), testSecret, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "the store must not create the Secret: the chart owns it and the Role has no create")
	// A missing Secret is a failed store, not "nobody is linked".
	_, err = s.Get("U1")
	require.True(t, apierrors.IsNotFound(err))
	require.NotErrorIs(t, err, ErrNotLinked)
}

// The apiserver failing is reported as what it is -- on every operation -- so
// the Linker can serve the link it knows instead of treating the person as
// unlinked, and keep a rotated token the store refused to take.
func TestSecretStoreReportsAPIFailures(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	s := newSecretStoreFor(t, client, key32())
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt-1"}))

	// Reads fail (the apiserver away): no operation can go through, none of
	// them reads as an absent link.
	getDown := true
	failVerb(client, "get", &getDown, apierrors.NewInternalError(errors.New("connection refused")))
	_, err := s.Get("U1")
	require.True(t, apierrors.IsInternalError(err), "got %v", err)
	require.NotErrorIs(t, err, ErrNotLinked)
	err = s.Put("U1", &Link{RefreshToken: "rt-2"})
	require.True(t, apierrors.IsInternalError(err), "a write re-reads first and reports that failure: %v", err)
	require.NotErrorIs(t, err, ErrNotLinked)
	require.True(t, apierrors.IsInternalError(s.Delete("U1")))
	getDown = false

	// Only writes fail (the ServiceAccount lost update on the Secret): reads
	// still answer, and the refused write leaves the record as it was.
	updateDown := true
	failVerb(client, "update", &updateDown, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, testSecret, errors.New("update is not allowed")))
	err = s.Put("U1", &Link{RefreshToken: "rt-2"})
	require.True(t, apierrors.IsForbidden(err), "got %v", err)
	require.True(t, apierrors.IsForbidden(s.Delete("U1")))
	got, err := s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt-1", got.RefreshToken, "the refused write left the record as it was")

	// The apiserver takes writes again: the same write lands.
	updateDown = false
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt-2"}))
	got, err = s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt-2", got.RefreshToken)
}

func TestSecretStoreImportKeepsExistingEntries(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	s := newSecretStoreFor(t, client, key32())
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt-rotated"}))
	before := updates(client)

	added, err := s.Import(map[string]*Link{
		"U1": {RefreshToken: "rt-stale"},
		"U2": {RefreshToken: "rt-2"},
		"U3": {RefreshToken: "rt-3"},
	})
	require.NoError(t, err)
	require.Equal(t, 2, added)
	got, err := s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt-rotated", got.RefreshToken, "the entry already in the Secret wins")
	got, err = s.Get("U3")
	require.NoError(t, err)
	require.Equal(t, "rt-3", got.RefreshToken)
	require.Equal(t, before+1, updates(client), "the import is one write")

	// Nothing new: no write at all.
	before = updates(client)
	added, err = s.Import(map[string]*Link{"U2": {RefreshToken: "x"}})
	require.NoError(t, err)
	require.Equal(t, 0, added)
	require.Equal(t, before, updates(client))
}

func TestNewSecretStoreRejectsBadInput(t *testing.T) {
	_, err := NewSecretStore(fake.NewClientset(), []byte("tooshort"), SecretStoreOptions{Namespace: testNS}, nil)
	require.Error(t, err)
	_, err = NewSecretStore(fake.NewClientset(), key32(), SecretStoreOptions{}, nil)
	require.Error(t, err, "namespace is required")
	s, err := NewSecretStore(fake.NewClientset(), key32(), SecretStoreOptions{Namespace: testNS}, nil)
	require.NoError(t, err)
	require.Equal(t, testNS+"/"+DefaultSecretStoreName, s.Ref())
}

func TestImportBoltFileLeavesTheFileUntouched(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	bs, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	require.NoError(t, bs.Put("U1", &Link{Sub: "s1", Email: "a@example.com", RefreshToken: "rt-1"}))
	require.NoError(t, bs.Put("U2", &Link{Sub: "s2", Email: "b@example.com", RefreshToken: "rt-2"}))
	require.NoError(t, bs.Close())
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file it just created under t.TempDir()
	require.NoError(t, err)

	client := fake.NewClientset(linkSecret(nil))
	dst := newSecretStoreFor(t, client, key32())
	require.NoError(t, dst.Put("U2", &Link{Sub: "s2", Email: "b@example.com", RefreshToken: "rt-2b"}))

	added, total, err := ImportBoltFile(path, key32(), dst, nil)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Equal(t, 1, added)
	got, err := dst.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt-1", got.RefreshToken)
	got, err = dst.Get("U2")
	require.NoError(t, err)
	require.Equal(t, "rt-2b", got.RefreshToken)

	after, err := os.ReadFile(path) //nolint:gosec // G304: same test-owned file
	require.NoError(t, err)
	require.Equal(t, raw, after, "the import must not write to the bolt file")

	// A second run finds everything present.
	added, total, err = ImportBoltFile(path, key32(), dst, nil)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Equal(t, 0, added)

	// The file still opens for writing afterwards (no lock or mode left behind).
	bs2, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	require.NoError(t, bs2.Close())
}

func TestImportBoltFileSkipsUndecodableRecordsAndMissingFile(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	other := key32()
	other[0] ^= 0xff
	bs, err := OpenBoltStore(path, other, nil)
	require.NoError(t, err)
	require.NoError(t, bs.Put("U1", &Link{RefreshToken: "rt-alien"}))
	require.NoError(t, bs.Close())
	bs, err = OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	require.NoError(t, bs.Put("U2", &Link{RefreshToken: "rt-2"}))
	require.NoError(t, bs.Close())

	dst := newSecretStoreFor(t, fake.NewClientset(linkSecret(nil)), key32())
	added, total, err := ImportBoltFile(path, key32(), dst, nil)
	require.NoError(t, err)
	require.Equal(t, 1, total, "the record under another key is skipped, not fatal")
	require.Equal(t, 1, added)

	_, _, err = ImportBoltFile(t.TempDir()+"/absent.bolt", key32(), dst, nil)
	require.Error(t, err)
	_, statErr := os.Stat(t.TempDir() + "/absent.bolt")
	require.True(t, os.IsNotExist(statErr), "a read-only open must not create the file")
}

func TestBoltStoreEach(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	bs, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	require.NoError(t, bs.Put("U1", &Link{RefreshToken: "rt-1"}))
	require.NoError(t, bs.Put("U2", &Link{RefreshToken: "rt-2"}))
	seen := map[string]string{}
	require.NoError(t, bs.Each(func(id string, l *Link) error {
		seen[id] = l.RefreshToken
		return nil
	}))
	require.Equal(t, map[string]string{"U1": "rt-1", "U2": "rt-2"}, seen)
	stop := errors.New("stop")
	require.ErrorIs(t, bs.Each(func(string, *Link) error { return stop }), stop)
	require.NoError(t, bs.Close())
}
