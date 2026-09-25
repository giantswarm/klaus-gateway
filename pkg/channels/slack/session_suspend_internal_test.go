package slack

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// suspendAPIRecorder is a fake Slack Web API that records the session status
// calls made outside a turn, with the thread each one addresses. Everything
// else (chat.update on the prompt message) is answered ok and ignored.
type suspendAPIRecorder struct {
	mu    sync.Mutex
	calls []statusCall
}

func (r *suspendAPIRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/"+methodSetSessionStatus, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			ChannelID string `json:"channel_id"`
			ThreadTS  string `json:"thread_ts"`
			Status    string `json:"status"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.calls = append(r.calls, statusCall{channelID: body.ChannelID, threadTS: body.ThreadTS, status: body.Status})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.1"})
	})
	return mux
}

func (r *suspendAPIRecorder) snapshot() []statusCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]statusCall(nil), r.calls...)
}

func newSuspendTestAdapter(t *testing.T) (*Adapter, *suspendAPIRecorder) {
	t.Helper()
	rec := &suspendAPIRecorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	a := &Adapter{
		APIBase: srv.URL,
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec // dummy value used only in tests
		Logger:  slog.New(slog.DiscardHandler),
	}
	a.gw = newMemoryRecorder()
	return a, rec
}

// A prompt the TTL sweep drops is dead: its buttons lead nowhere, and the
// thread nobody answered gets no next turn to correct the session, so the
// sweep itself hands that thread back as idle.
func TestStorePendingTask_SweptPromptReleasesTheSession(t *testing.T) {
	a, rec := newSuspendTestAdapter(t)

	a.storePendingTask("1.0", &pendingTask{TaskID: "old", Channel: "C-old"})
	a.threadsMu.Lock()
	a.threads["1.0"].pending.storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.threadsMu.Unlock()

	a.storePendingTask("2.0", &pendingTask{TaskID: "new", Channel: "C-new"})

	require.Eventually(t, func() bool { return len(rec.snapshot()) == 1 },
		flowWait, 10*time.Millisecond, "expected the swept thread's session to be released")
	require.Equal(t, []statusCall{{channelID: "C-old", threadTS: "1.0", status: string(sessionActive)}}, rec.snapshot(),
		"only the swept thread is released; the thread that just paused stays suspended")
}

// A click on a prompt the gateway no longer holds — the task expired, or a
// restart dropped it — is the only moment the gateway learns of such a thread,
// so it is where the session is handed back as idle.
func TestHandleDecision_DeadPromptReleasesTheSession(t *testing.T) {
	a, rec := newSuspendTestAdapter(t)
	a.accessPolicy().SetInitiator(t.Context(), "C1", "1.0", "U1")

	require.NoError(t, a.handleDecision(t.Context(), "C1", "1.0", "msg-1", "U1", hitlAction{kind: hitlApprove}))

	require.Equal(t, []statusCall{{channelID: "C1", threadTS: "1.0", status: string(sessionActive)}}, rec.snapshot())
}

// A click on a superseded prompt message finds a NEWER pending task on the
// thread: the agent is still waiting for an answer, so the session must stay
// suspended.
func TestHandleDecision_SupersededPromptKeepsTheSession(t *testing.T) {
	a, rec := newSuspendTestAdapter(t)
	a.accessPolicy().SetInitiator(t.Context(), "C1", "1.0", "U1")
	a.storePendingTask("1.0", &pendingTask{TaskID: "task-2", Channel: "C1"})

	require.NoError(t, a.handleDecision(t.Context(), "C1", "1.0", "msg-1", "U1",
		hitlAction{kind: hitlApprove, taskID: "task-1"}))

	require.Empty(t, rec.snapshot(), "a newer prompt is still waiting for the user")
}
