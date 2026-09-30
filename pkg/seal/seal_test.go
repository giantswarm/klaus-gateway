package seal

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func key32() []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestNormalizeKey(t *testing.T) {
	raw := key32()

	t.Run("raw 32 bytes used verbatim", func(t *testing.T) {
		got, err := NormalizeKey(raw)
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("base64 std (the SOPS-staged form) decodes to 32 bytes", func(t *testing.T) {
		enc := base64.StdEncoding.EncodeToString(raw)
		require.Len(t, enc, 44)
		got, err := NormalizeKey([]byte(enc))
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("trailing newline is trimmed", func(t *testing.T) {
		got, err := NormalizeKey([]byte(base64.StdEncoding.EncodeToString(raw) + "\n"))
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("hex decodes to 32 bytes", func(t *testing.T) {
		got, err := NormalizeKey([]byte(hex.EncodeToString(raw)))
		require.NoError(t, err)
		require.Equal(t, raw, got)
	})

	t.Run("garbage that is not 32 bytes is rejected", func(t *testing.T) {
		_, err := NormalizeKey([]byte("nope"))
		require.Error(t, err)
	})
}

func TestSealRoundTrip(t *testing.T) {
	s, err := Derive(key32(), "test")
	require.NoError(t, err)

	record, err := s.Seal([]byte("secret"), []byte("where"))
	require.NoError(t, err)
	require.NotContains(t, string(record), "secret")

	got, err := s.Open(record, []byte("where"))
	require.NoError(t, err)
	require.Equal(t, "secret", string(got))
}

func TestOpenRefusesAnotherBinding(t *testing.T) {
	s, err := Derive(key32(), "test")
	require.NoError(t, err)
	record, err := s.Seal([]byte("secret"), []byte("where"))
	require.NoError(t, err)

	_, err = s.Open(record, []byte("elsewhere"))
	require.Error(t, err)
}

func TestDeriveSeparatesPurposes(t *testing.T) {
	a, err := Derive(key32(), "a")
	require.NoError(t, err)
	b, err := Derive(key32(), "b")
	require.NoError(t, err)
	record, err := a.Seal([]byte("secret"), nil)
	require.NoError(t, err)

	_, err = b.Open(record, nil)
	require.Error(t, err)
}

func TestEphemeralKeysDiffer(t *testing.T) {
	a, err := Ephemeral()
	require.NoError(t, err)
	b, err := Ephemeral()
	require.NoError(t, err)
	record, err := a.Seal([]byte("secret"), nil)
	require.NoError(t, err)

	_, err = b.Open(record, nil)
	require.Error(t, err)
}

func TestOpenRejectsShortRecord(t *testing.T) {
	s, err := Ephemeral()
	require.NoError(t, err)
	_, err = s.Open([]byte("x"), nil)
	require.Error(t, err)
}
