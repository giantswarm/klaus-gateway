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

func TestSecretStoreRoundTripAndSharedBackend(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	s := newSecretStoreFor(t, client, key32())

	n, err := s.Check()
	require.NoError(t, err)
	require.Equal(t, 0, n)

	link := &Link{Sub: "muster-1", Email: "a@example.com", RefreshToken: "rt-secret", LinkedAt: time.Now().UTC().Truncate(time.Second)}
	s.Put("U1", link)

	got, ok := s.Get("U1")
	require.True(t, ok)
	require.Equal(t, link.Sub, got.Sub)
	require.Equal(t, link.RefreshToken, got.RefreshToken)
	require.True(t, link.LinkedAt.Equal(got.LinkedAt))

	_, ok = s.Get("missing")
	require.False(t, ok)

	// A second store on the same Secret (a second replica) sees the link: the
	// backend is shared, not process-local.
	s2 := newSecretStoreFor(t, client, key32())
	got2, ok := s2.Get("U1")
	require.True(t, ok)
	require.Equal(t, "rt-secret", got2.RefreshToken)

	s2.Delete("U1")
	_, ok = s.Get("U1")
	require.False(t, ok)
	// Deleting a missing key writes nothing.
	before := updates(client)
	s.Delete("U1")
	require.Equal(t, before, updates(client))
}

func TestSecretStoreEncryptedAtRest(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	s := newSecretStoreFor(t, client, key32())
	s.Put("U1", &Link{RefreshToken: "topsecret-refresh-token", Email: "a@example.com"})

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
	newSecretStoreFor(t, client, key32()).Put("U1", &Link{RefreshToken: "rt"})

	wrong := key32()
	wrong[0] ^= 0xff
	_, ok := newSecretStoreFor(t, client, wrong).Get("U1")
	require.False(t, ok)
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
	s.Put("U1", &Link{RefreshToken: "rt"})
	require.Equal(t, 0, conflicts, "every simulated conflict must have been retried")
	got, ok := s.Get("U1")
	require.True(t, ok)
	require.Equal(t, "rt", got.RefreshToken)
}

func TestSecretStoreGivesUpAfterRetryLimit(t *testing.T) {
	client := fake.NewClientset(linkSecret(map[string][]byte{}))
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, testSecret, errors.New("busy"))
	})
	s, err := NewSecretStore(client, key32(), SecretStoreOptions{Namespace: testNS, Name: testSecret, Retries: 2}, nil)
	require.NoError(t, err)
	s.Put("U1", &Link{RefreshToken: "rt"}) // logged, not stored
	_, ok := s.Get("U1")
	require.False(t, ok)
}

func TestSecretStoreMissingSecretIsNotCreated(t *testing.T) {
	client := fake.NewClientset()
	s := newSecretStoreFor(t, client, key32())

	_, err := s.Check()
	require.Error(t, err, "a missing Secret must fail the startup check")
	require.True(t, apierrors.IsNotFound(errors.Unwrap(err)) || apierrors.IsNotFound(err))

	s.Put("U1", &Link{RefreshToken: "rt"})
	_, err = client.CoreV1().Secrets(testNS).Get(context.Background(), testSecret, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "the store must not create the Secret: the chart owns it and the Role has no create")
	_, ok := s.Get("U1")
	require.False(t, ok)
}

func TestSecretStoreImportKeepsExistingEntries(t *testing.T) {
	client := fake.NewClientset(linkSecret(nil))
	s := newSecretStoreFor(t, client, key32())
	s.Put("U1", &Link{RefreshToken: "rt-rotated"})
	before := updates(client)

	added, err := s.Import(map[string]*Link{
		"U1": {RefreshToken: "rt-stale"},
		"U2": {RefreshToken: "rt-2"},
		"U3": {RefreshToken: "rt-3"},
	})
	require.NoError(t, err)
	require.Equal(t, 2, added)
	got, _ := s.Get("U1")
	require.Equal(t, "rt-rotated", got.RefreshToken, "the entry already in the Secret wins")
	got, ok := s.Get("U3")
	require.True(t, ok)
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
	bs.Put("U1", &Link{Sub: "s1", Email: "a@example.com", RefreshToken: "rt-1"})
	bs.Put("U2", &Link{Sub: "s2", Email: "b@example.com", RefreshToken: "rt-2"})
	require.NoError(t, bs.Close())
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file it just created under t.TempDir()
	require.NoError(t, err)

	client := fake.NewClientset(linkSecret(nil))
	dst := newSecretStoreFor(t, client, key32())
	dst.Put("U2", &Link{Sub: "s2", Email: "b@example.com", RefreshToken: "rt-2b"})

	added, total, err := ImportBoltFile(path, key32(), dst, nil)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Equal(t, 1, added)
	got, ok := dst.Get("U1")
	require.True(t, ok)
	require.Equal(t, "rt-1", got.RefreshToken)
	got, _ = dst.Get("U2")
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
	bs.Put("U1", &Link{RefreshToken: "rt-alien"})
	require.NoError(t, bs.Close())
	bs, err = OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	bs.Put("U2", &Link{RefreshToken: "rt-2"})
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
	bs.Put("U1", &Link{RefreshToken: "rt-1"})
	bs.Put("U2", &Link{RefreshToken: "rt-2"})
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
