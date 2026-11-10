package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

// inboxHarness is a started adapter on a memory recorder whose thread
// 100.000 in C1 is bound to an agent, and the ephemerals it posts.
type inboxHarness struct {
	adapter *Adapter
	gw      *fakeGateway

	mu         sync.Mutex
	ephemerals []map[string]any
}

func newInboxHarness(t *testing.T) *inboxHarness {
	t.Helper()
	h := &inboxHarness{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.mu.Lock()
			h.ephemerals = append(h.ephemerals, body)
			h.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.2"})
	}))
	t.Cleanup(srv.Close)
	h.gw = &fakeGateway{Facade: newMemoryRecorder(), deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	h.adapter = &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		DefaultAgent: "worker",
		Logger:       slog.New(slog.DiscardHandler),
	}
	require.NoError(t, h.adapter.Start(t.Context(), h.gw))
	t.Cleanup(func() { _ = h.adapter.Stop(context.Background()) })
	require.NoError(t, h.adapter.bindThreadAgent(t.Context(), "C1", "100.000", "worker"))
	return h
}

// queue writes messages into the thread's inbox in the row.
func (h *inboxHarness) queue(t *testing.T, messages ...queuedMessage) {
	t.Helper()
	held, err := heldState{Inbox: messages}.marshal()
	require.NoError(t, err)
	require.NoError(t, h.gw.UpdateThreadRecord(t.Context(), ChannelName, "C1", "100.000", func(e *store.Entry, _ bool) bool {
		e.Held = held
		return true
	}))
}

func (h *inboxHarness) inboxEmpty(t *testing.T) bool {
	row, _, err := h.gw.ThreadRecord(t.Context(), ChannelName, "C1", "100.000")
	return err == nil && len(h.adapter.heldOf("100.000", row.Held).Inbox) == 0
}

func (h *inboxHarness) sentEphemerals() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.ephemerals)
}

func queuedAt(text, ts string, at time.Time) queuedMessage {
	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "C1", ThreadID: "100.000", MessageID: ts, Text: text, Subject: "U1"}
	return queuedMessage{heldMessage: heldMessageOf(msg, "C1", at)}
}

// A message that waited in a thread's inbox longer than pendingTTL is not run
// when a new process reads the inbox back, and its sender is told; the fresh
// one beside it runs, and both leave the row.
func TestRecoverTurns_ExpiredQueuedMessageIsNotRun(t *testing.T) {
	old := restoredInboxGrace
	restoredInboxGrace = 0
	t.Cleanup(func() { restoredInboxGrace = old })
	h := newInboxHarness(t)
	h.queue(t,
		queuedAt("stale", "100.001", time.Now().Add(-pendingTTL-time.Minute)),
		queuedAt("fresh", "100.002", time.Now()),
	)

	h.adapter.RecoverTurns()

	require.Eventually(t, func() bool { return h.gw.sendCount() > 0 }, flowWait, 10*time.Millisecond)
	require.Never(t, func() bool { return h.gw.sendCount() > 1 }, 300*time.Millisecond, 10*time.Millisecond,
		"the expired message is not run")
	require.Equal(t, "fresh", h.gw.sentMessages()[0].Text)
	require.Eventually(t, func() bool { return h.inboxEmpty(t) }, flowWait, 10*time.Millisecond, "both leave the row")
	ephemerals := h.sentEphemerals()
	require.Len(t, ephemerals, 1, "the sender of the expired message is told, once")
	require.Equal(t, "U1", ephemerals[0]["user"])
	require.Equal(t, queueExpiredNotice, ephemerals[0]["text"])
}

// A message that waits behind a prompt nobody answers expires on time: its
// sender is told when it has waited pendingTTL, with no further event in the
// thread, and it leaves the row without running.
func TestQueue_MessageBehindAnUnansweredPromptExpiresOnTime(t *testing.T) {
	h := newInboxHarness(t)
	const expiresIn = 300 * time.Millisecond
	h.queue(t, queuedAt("before the prompt", "100.001", time.Now().Add(-pendingTTL+expiresIn)))
	h.adapter.storePendingTask("100.000", &pendingTask{TaskID: "task-1", AgentRef: "worker", Channel: "C1", ChannelID: "C1"})

	h.adapter.kickInbox("C1", "100.000")

	require.Eventually(t, func() bool { return len(h.sentEphemerals()) == 1 }, flowWait+expiresIn, 10*time.Millisecond,
		"the sender is told once the message expired")
	require.Equal(t, queueExpiredNotice, h.sentEphemerals()[0]["text"])
	require.Eventually(t, func() bool { return h.inboxEmpty(t) }, flowWait, 10*time.Millisecond, "it leaves the row")
	require.Zero(t, h.gw.sendCount(), "it does not run")
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
