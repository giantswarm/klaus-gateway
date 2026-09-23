package store_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func TestKey_RoundTrip(t *testing.T) {
	t.Run("three parts", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: "ctx-1", ThreadID: ""}
		got, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, got)
	})

	t.Run("pipe and backslash survive the escape round-trip", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: `c|1\2`, ThreadID: "t"}
		got, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, got)
	})

	t.Run("invalid key rejected", func(t *testing.T) {
		_, err := store.ParseKey("only|two")
		require.Error(t, err)
		// The 4-part key of the user-slot layout is no longer a routing key: a
		// row written by an older gateway is skipped, not misread.
		_, err = store.ParseKey("slack|C1||1700000000.000100")
		require.Error(t, err)
	})
}

// The rows of the old key layout are dropped on every List, which readiness
// runs on every probe for the memory and bolt stores. So the count is recorded
// at Debug each time, and the fact is stated once per process at Info — the
// level installations run at, where the first start after the upgrade is the
// moment an operator needs the evidence.
func TestLogSkippedKeys_InfoOncePerProcess(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	store.ResetSkippedKeysOnce()
	t.Cleanup(store.ResetSkippedKeysOnce)

	store.LogSkippedKeys("memory", 0)
	require.Empty(t, buf.String(), "a List that skipped nothing says nothing")

	store.LogSkippedKeys("memory", 3)
	store.LogSkippedKeys("memory", 3)

	var infos, debugs int
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Level   string `json:"level"`
			Backend string `json:"backend"`
			Skipped int    `json:"skipped"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		require.Equal(t, "memory", rec.Backend)
		require.Equal(t, 3, rec.Skipped)
		switch rec.Level {
		case "INFO":
			infos++
		case "DEBUG":
			debugs++
		}
	}
	require.Equal(t, 1, infos, "the fact is stated once per process")
	require.Equal(t, 2, debugs, "the count is recorded on every List that skipped rows")
}
