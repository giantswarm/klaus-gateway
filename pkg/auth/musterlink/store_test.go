package musterlink

import (
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
	s.Put("U1", link)

	got, ok := s.Get("U1")
	require.True(t, ok)
	require.Equal(t, link.Sub, got.Sub)
	require.Equal(t, link.RefreshToken, got.RefreshToken)

	_, ok = s.Get("missing")
	require.False(t, ok)

	require.NoError(t, s.Close())

	// Survives a reopen with the same key.
	s2, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	got2, ok := s2.Get("U1")
	require.True(t, ok)
	require.Equal(t, "rt-secret", got2.RefreshToken)

	s2.Delete("U1")
	_, ok = s2.Get("U1")
	require.False(t, ok)
}

func TestBoltStoreEncryptedAtRest(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	s, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	s.Put("U1", &Link{RefreshToken: "topsecret-refresh-token"})
	require.NoError(t, s.Close())

	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file it just created under t.TempDir()
	require.NoError(t, err)
	require.NotContains(t, string(raw), "topsecret-refresh-token", "refresh token must not appear in plaintext on disk")
}

func TestBoltStoreWrongKeyFailsClosed(t *testing.T) {
	path := t.TempDir() + "/links.bolt"
	s, err := OpenBoltStore(path, key32(), nil)
	require.NoError(t, err)
	s.Put("U1", &Link{RefreshToken: "rt"})
	require.NoError(t, s.Close())

	wrong := key32()
	wrong[0] ^= 0xff
	s2, err := OpenBoltStore(path, wrong, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	_, ok := s2.Get("U1") // GCM auth tag fails -> miss, not a panic or garbage
	require.False(t, ok)
}

func TestNewGCMRejectsBadKeyLength(t *testing.T) {
	_, err := OpenBoltStore(t.TempDir()+"/x.bolt", []byte("tooshort"), nil)
	require.Error(t, err)
}

func TestMemStore(t *testing.T) {
	s := NewMemStore()
	_, ok := s.Get("U1")
	require.False(t, ok)
	s.Put("U1", &Link{RefreshToken: "rt"})
	got, ok := s.Get("U1")
	require.True(t, ok)
	require.Equal(t, "rt", got.RefreshToken)
	// Returned value is a copy: mutating it must not affect the store.
	got.RefreshToken = "mutated"
	again, _ := s.Get("U1")
	require.Equal(t, "rt", again.RefreshToken)
	s.Delete("U1")
	_, ok = s.Get("U1")
	require.False(t, ok)
}
