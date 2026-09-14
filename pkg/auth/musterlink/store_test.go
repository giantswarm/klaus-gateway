package musterlink

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestBoltStoreRoundTripAndPersistence(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	s, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)

	link := &Link{Sub: "muster-1", Email: "a@example.com", RefreshToken: "rt-secret", LinkedAt: time.Now().UTC().Truncate(time.Second)}
	require.NoError(t, s.Put("U1", link))

	got, err := s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, link.Sub, got.Sub)
	require.Equal(t, link.RefreshToken, got.RefreshToken)

	_, err = s.Get("missing")
	require.ErrorIs(t, err, ErrNotLinked)

	require.NoError(t, s.Close())

	// Survives a reopen with the same key.
	s2, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	got2, err := s2.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt-secret", got2.RefreshToken)

	require.NoError(t, s2.Delete("U1"))
	_, err = s2.Get("U1")
	require.ErrorIs(t, err, ErrNotLinked)
}

func TestBoltStoreEncryptedAtRest(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	s, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "topsecret-refresh-token"}))
	require.NoError(t, s.Close())

	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file it just created under t.TempDir()
	require.NoError(t, err)
	require.NotContains(t, string(raw), "topsecret-refresh-token", "refresh token must not appear in plaintext on disk")
}

func TestBoltStoreWrongKeyFailsClosed(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	s, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt"}))
	require.NoError(t, s.Close())

	wrong := key32()
	wrong[0] ^= 0xff
	s2, err := OpenBoltStore(path, wrong, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	// GCM auth tag fails -> reported as unlinked (a re-link overwrites the
	// record), not a panic, garbage, or a failed store.
	_, err = s2.Get("U1")
	require.ErrorIs(t, err, ErrNotLinked)
}

// A write the file cannot take is an error the caller sees and acts on (it
// keeps the link and retries), not a logged miss that loses a rotated token.
func TestBoltStoreReadOnlyRefusesWrites(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	s, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt"}))
	require.NoError(t, s.Close())

	ro, err := OpenBoltStoreReadOnly(path, key32(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	got, err := ro.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt", got.RefreshToken)

	err = ro.Put("U1", &Link{RefreshToken: "rt-2"})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotLinked, "a refused write is a store failure, not an absent link")
	require.Error(t, ro.Delete("U1"))
	got, err = ro.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt", got.RefreshToken, "the refused write left the record as it was")
}

func TestNewGCMRejectsBadKeyLength(t *testing.T) {
	_, err := OpenBoltStore(t.TempDir()+"/x.bolt", []byte("tooshort"), nil)
	require.Error(t, err)
}

func TestNormalizeStoreKey(t *testing.T) {
	raw := key32()

	t.Run("raw 32 bytes used verbatim", func(t *testing.T) {
		got, err := normalizeStoreKey(raw)
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("base64 std (the SOPS-staged form) decodes to 32 bytes", func(t *testing.T) {
		enc := base64.StdEncoding.EncodeToString(raw) // 44 chars, the real gazelle case
		require.Len(t, enc, 44)
		got, err := normalizeStoreKey([]byte(enc))
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("trailing newline is trimmed", func(t *testing.T) {
		got, err := normalizeStoreKey([]byte(base64.StdEncoding.EncodeToString(raw) + "\n"))
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("hex decodes to 32 bytes", func(t *testing.T) {
		got, err := normalizeStoreKey([]byte(hex.EncodeToString(raw)))
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("garbage that is not 32 bytes is rejected", func(t *testing.T) {
		_, err := normalizeStoreKey([]byte("nope"))
		require.Error(t, err)
	})
}

func TestBoltStoreAcceptsBase64Key(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	encKey := []byte(base64.StdEncoding.EncodeToString(key32()))

	s, err := OpenBoltStore(path, encKey, nil)
	require.NoError(t, err)
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt"}))
	require.NoError(t, s.Close())

	// Reopen with the raw form of the same key: it must decrypt what the
	// base64 form wrote, proving both forms resolve to identical key material.
	s2, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt", got.RefreshToken)
}

func TestMemStore(t *testing.T) {
	s := NewMemStore()
	_, err := s.Get("U1")
	require.ErrorIs(t, err, ErrNotLinked)
	require.NoError(t, s.Put("U1", &Link{RefreshToken: "rt"}))
	got, err := s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt", got.RefreshToken)
	// Returned value is a copy: mutating it must not affect the store.
	got.RefreshToken = "mutated"
	again, err := s.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "rt", again.RefreshToken)
	require.NoError(t, s.Delete("U1"))
	_, err = s.Get("U1")
	require.ErrorIs(t, err, ErrNotLinked)
}
