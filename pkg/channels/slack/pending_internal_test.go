package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func TestStorePendingTask_SweepsExpiredEntries(t *testing.T) {
	a := &Adapter{}
	a.storePendingTask("t-old", &pendingTask{TaskID: "old"})
	a.threadsMu.Lock()
	a.threads["t-old"].pending.storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.threadsMu.Unlock()

	a.storePendingTask("t-new", &pendingTask{TaskID: "new"})

	require.False(t, a.hasPendingTask("t-old"), "expired pending task must be swept")
	require.True(t, a.hasPendingTask("t-new"))
}

func TestStorePendingAccess_SweepsExpiredEntries(t *testing.T) {
	a := &Adapter{}
	a.storePendingAccess("t1", "U-old", &pendingAccessReq{msg: channels.InboundMessage{Text: "old"}})
	a.pendingAccessMu.Lock()
	a.pendingAccess["t1"]["U-old"][0].storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.pendingAccessMu.Unlock()

	a.storePendingAccess("t2", "U-new", &pendingAccessReq{msg: channels.InboundMessage{Text: "new"}})

	require.Empty(t, a.takePendingAccess("t1", "U-old"), "expired parked request must be swept")
	require.Len(t, a.takePendingAccess("t2", "U-new"), 1)
}

// storePendingAccess reports new only for the first parked request per (thread,
// user), so a burst replayed after sign-in posts the consent prompt once.
func TestStorePendingAccess_ReportsNewOnlyOnce(t *testing.T) {
	a := &Adapter{}
	first, _ := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: "m1"}})
	require.True(t, first, "first parked request for a (thread, user) is new")

	second, _ := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: "m2"}})
	require.False(t, second, "a repeat before approval must not re-prompt")

	other, _ := a.storePendingAccess("t1", "U2", &pendingAccessReq{msg: channels.InboundMessage{Text: "m3"}})
	require.True(t, other, "a different user in the same thread is prompted independently")
}

// The pending-access queue keeps every message a newcomer sends before the
// grant, in order, bounded by maxParkedPerThread (oldest dropped).
func TestStorePendingAccess_QueueOrderAndCap(t *testing.T) {
	a := &Adapter{}
	for i := 1; i <= maxParkedPerThread+2; i++ {
		_, dropped := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: fmt.Sprintf("m%d", i)}})
		require.Equal(t, i > maxParkedPerThread, dropped, "an eviction is reported exactly when the cap overflows (message %d)", i)
	}

	got := a.takePendingAccess("t1", "U1")
	require.Len(t, got, maxParkedPerThread, "the queue is capped")
	var texts []string
	for _, r := range got {
		texts = append(texts, r.msg.Text)
	}
	require.Equal(t, []string{"m3", "m4", "m5", "m6", "m7"}, texts, "oldest dropped past the cap, order preserved")
	require.Empty(t, a.takePendingAccess("t1", "U1"), "take clears the queue")
}

// takePendingLogin drops entries older than pendingTTL, so a sign-in completed
// long after the message was parked does not replay a stale question.
func TestTakePendingLogin_DropsExpiredEntries(t *testing.T) {
	a := &Adapter{}
	a.parkPendingLogin("U1", &pendingLoginReq{msg: channels.InboundMessage{ThreadID: "T1", Text: "stale"}})
	a.parkPendingLogin("U1", &pendingLoginReq{msg: channels.InboundMessage{ThreadID: "T1", Text: "fresh"}})
	a.pendingLoginMu.Lock()
	a.pendingLogin["U1"]["T1"][0].storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.pendingLoginMu.Unlock()

	got := a.takePendingLogin("U1")
	require.Len(t, got["T1"], 1)
	require.Equal(t, "fresh", got["T1"][0].msg.Text, "the expired entry is dropped, the fresh one survives")
}

// A message that waited in a thread's inbox longer than pendingTTL is not run
// when a new process reads the inbox back, and its sender is told; the fresh
// one beside it runs, and both leave the row.
func TestRecoverTurns_ExpiredQueuedMessageIsNotRun(t *testing.T) {
	old := restoredInboxGrace
	restoredInboxGrace = 0
	t.Cleanup(func() { restoredInboxGrace = old })
	var mu sync.Mutex
	var ephemerals []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			ephemerals = append(ephemerals, body)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.2"})
	}))
	t.Cleanup(srv.Close)
	gw := &fakeGateway{Facade: newMemoryRecorder(), deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		DefaultAgent: "worker",
		Logger:       slog.New(slog.DiscardHandler),
	}
	require.NoError(t, a.Start(t.Context(), gw))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	require.NoError(t, a.bindThreadAgent(t.Context(), "C1", "100.000", "worker"))

	queued := func(text, ts string, at time.Time) queuedMessage {
		msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "C1", ThreadID: "100.000", MessageID: ts, Text: text, Subject: "U1"}
		return queuedMessage{heldMessage: heldMessageOf(msg, "C1", at)}
	}
	held, err := heldState{Inbox: []queuedMessage{
		queued("stale", "100.001", time.Now().Add(-pendingTTL-time.Minute)),
		queued("fresh", "100.002", time.Now()),
	}}.marshal()
	require.NoError(t, err)
	require.NoError(t, gw.UpdateThreadRecord(t.Context(), ChannelName, "C1", "100.000", func(e *store.Entry, _ bool) bool {
		e.Held = held
		return true
	}))

	a.RecoverTurns()

	require.Eventually(t, func() bool { return gw.sendCount() > 0 }, flowWait, 10*time.Millisecond)
	require.Never(t, func() bool { return gw.sendCount() > 1 }, 300*time.Millisecond, 10*time.Millisecond,
		"the expired message is not run")
	require.Equal(t, "fresh", gw.sentMessages()[0].Text)
	require.Eventually(t, func() bool {
		row, _, err := gw.ThreadRecord(t.Context(), ChannelName, "C1", "100.000")
		return err == nil && len(row.Held) == 0
	}, flowWait, 10*time.Millisecond, "both leave the row")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, ephemerals, 1, "the sender of the expired message is told, once")
	require.Equal(t, "U1", ephemerals[0]["user"])
	require.Equal(t, queueExpiredNotice, ephemerals[0]["text"])
}

func TestIsBareAuthUtterance(t *testing.T) {
	bare := []string{
		"login",
		"Login",
		"log in",
		"Sign In",
		"signin",
		"connect",
		"/login",
		"login!",
		"  sign in.  ",
		"<@BOT123> login",
	}
	for _, text := range bare {
		require.True(t, isBareAuthUtterance(text), "expected bare auth utterance: %q", text)
	}
	replayable := []string{
		"login to grafana is broken",
		"why does login fail",
		"connect the dots",
		"can you sign in to the cluster registry",
		"",
	}
	for _, text := range replayable {
		require.False(t, isBareAuthUtterance(text), "expected replayable message: %q", text)
	}
}
