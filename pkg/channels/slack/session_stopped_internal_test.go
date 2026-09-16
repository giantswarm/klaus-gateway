package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	stopEventChannel  = "C0123ABC456"
	stopEventThreadTS = "1782234671.392669"
)

// sessionStoppedEvent is a recorded agent_session_stopped delivery. team_id
// rides the envelope, not the event, and streaming_message_ts lists the streams
// Slack halted — this adapter starts none, so it is decoded and ignored.
const sessionStoppedEvent = `{
	"type":"event_callback",
	"team_id":"T0123ABC456",
	"event_id":"Ev-stop-1",
	"event":{
		"type":"agent_session_stopped",
		"channel":"C0123ABC456",
		"thread_ts":"1782234671.392669",
		"user":"U123ABC456",
		"event_ts":"1783536983.783769",
		"streaming_message_ts":["1782234987.693923"]
	}
}`

// stopEventUser is the presser in the recorded payload.
const stopEventUser = "U123ABC456"

// stopAPIRecorder is a fake Slack Web API recording the three calls the stop
// path can make: the in-thread notice, the ephemeral refusal, and the session
// status.
type stopAPIRecorder struct {
	mu             sync.Mutex
	postTexts      []string
	ephemeralTexts []string
	ephemeralUsers []string
	statuses       []string
}

func (r *stopAPIRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		r.mu.Lock()
		r.postTexts = append(r.postTexts, req.PostFormValue("text"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	})
	mux.HandleFunc("/chat.postEphemeral", func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		text, _ := body[paramText].(string)
		user, _ := body[paramUser].(string)
		r.mu.Lock()
		r.ephemeralTexts = append(r.ephemeralTexts, text)
		r.ephemeralUsers = append(r.ephemeralUsers, user)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/"+methodSetSessionStatus, func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		status, _ := body[paramStatus].(string)
		r.mu.Lock()
		r.statuses = append(r.statuses, status)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	return mux
}

func (r *stopAPIRecorder) snapshot() (posts, ephemerals, statuses []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.postTexts...),
		append([]string(nil), r.ephemeralTexts...),
		append([]string(nil), r.statuses...)
}

// deliverStopEvent drives the recorded payload through the Events API handler,
// signature check included. Handling is asynchronous behind the ack, so the
// caller waits on the effect.
func deliverStopEvent(t *testing.T, a *Adapter) {
	t.Helper()
	h := &eventsHandler{signingSecret: "signing-secret", adapter: a, logger: a.Logger}

	body := []byte(sessionStoppedEvent)
	stamp := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte("signing-secret"))
	mac.Write([]byte("v0:" + stamp + ":" + string(body)))

	req := httptest.NewRequest(http.MethodPost, "/channels/slack/events", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "Slack needs the ack before the work")
}

// newStopTestAdapter builds an adapter pointed at the recording fake API.
func newStopTestAdapter(t *testing.T) (*Adapter, *stopAPIRecorder) {
	t.Helper()
	rec := &stopAPIRecorder{}
	ts := httptest.NewServer(rec.handler())
	t.Cleanup(ts.Close)
	a := &Adapter{
		APIBase: ts.URL,
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec // dummy value used only in tests
		Logger:  slog.New(slog.DiscardHandler),
	}
	return a, rec
}

// Slack's native stop button must interrupt the turn exactly like /stop: the
// in-flight turn is cancelled and the thread gets the same confirmation. The
// idle status comes from the cancelled turn's own exit path, so the handler
// must not send one itself.
func TestSessionStopped_CancelsRunningTurn(t *testing.T) {
	a, rec := newStopTestAdapter(t)

	cancelled := make(chan struct{})
	a.threadsMu.Lock()
	a.threads = map[string]*threadState{
		stopEventThreadTS: {slot: &turnSlot{turn: &turn{cancel: func() { close(cancelled) }}}},
	}
	a.threadsMu.Unlock()

	deliverStopEvent(t, a)

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the stop button to cancel the in-flight turn")
	}
	require.Eventually(t, func() bool {
		posts, _, _ := rec.snapshot()
		return len(posts) == 1
	}, 2*time.Second, 10*time.Millisecond, "expected the stopped notice in the thread")

	posts, ephemerals, statuses := rec.snapshot()
	require.Equal(t, []string{fmt.Sprintf(stopStoppedByNotice, stopEventUser)}, posts,
		"the press leaves no message of its own, so the notice names the presser")
	require.Empty(t, ephemerals)
	require.Empty(t, statuses, "the cancelled turn's exit path owns the idle status")
}

// A stop click with nothing left to cancel has no turn exit to ride, so the
// handler sends the idle status itself — Slack keeps the indicator spinning
// for up to an hour otherwise — and says nothing in the thread.
func TestSessionStopped_NoRunningTurnSetsActive(t *testing.T) {
	a, rec := newStopTestAdapter(t)

	deliverStopEvent(t, a)

	require.Eventually(t, func() bool {
		_, _, statuses := rec.snapshot()
		return len(statuses) == 1
	}, 2*time.Second, 10*time.Millisecond, "expected the session to be set back to active")

	posts, ephemerals, statuses := rec.snapshot()
	require.Equal(t, []string{string(sessionActive)}, statuses)
	require.Empty(t, posts, "nothing was running, so nothing is confirmed stopped")
	require.Empty(t, ephemerals)
}

// A thread waiting on an approval prompt must be left exactly as it is. The
// button cannot normally reach one — Slack draws it only while the session is
// processing — but a turn that pauses as the press lands has already released
// its slot, so stopThread reports nothing to stop. Writing active there would
// erase the suspended state the exit path just wrote and claim the thread is
// idle while a tool call waits on the user's answer.
func TestSessionStopped_PendingTaskLeavesStatusAlone(t *testing.T) {
	a, rec := newStopTestAdapter(t)

	a.storePendingTask(stopEventThreadTS, &pendingTask{
		TaskID: "task-1", AgentRef: "worker", ChannelID: stopEventChannel,
	})

	deliverStopEvent(t, a)

	// The handler acts on its own goroutine; give it room to do the wrong thing.
	require.Never(t, func() bool {
		posts, ephemerals, statuses := rec.snapshot()
		return len(posts)+len(ephemerals)+len(statuses) > 0
	}, 500*time.Millisecond, 25*time.Millisecond,
		"a thread waiting on a prompt must be left untouched")
	require.True(t, a.hasPendingTask(stopEventThreadTS), "the prompt is still waiting for an answer")
}

// The button carries the per-thread access rule /stop carries: an onlooker the
// thread owner never let in cannot interrupt the agent. The refusal is
// ephemeral to the presser, and no status is sent — the session stays in
// processing because the turn really is still running.
func TestSessionStopped_NotPermittedUserIsRefused(t *testing.T) {
	a, rec := newStopTestAdapter(t)

	// Someone else owns the thread, so the presser is a mere onlooker.
	a.accessPolicy().SetInitiator(t.Context(), stopEventChannel, stopEventThreadTS, "U-owner")

	cancelled := make(chan struct{})
	a.threadsMu.Lock()
	a.threads = map[string]*threadState{
		stopEventThreadTS: {slot: &turnSlot{turn: &turn{cancel: func() { close(cancelled) }}}},
	}
	a.threadsMu.Unlock()

	deliverStopEvent(t, a)

	require.Eventually(t, func() bool {
		_, ephemerals, _ := rec.snapshot()
		return len(ephemerals) == 1
	}, 2*time.Second, 10*time.Millisecond, "expected the refusal to reach the presser")

	posts, ephemerals, statuses := rec.snapshot()
	require.Equal(t, []string{notPermittedNotice}, ephemerals)
	rec.mu.Lock()
	require.Equal(t, []string{stopEventUser}, rec.ephemeralUsers, "the refusal goes to the presser")
	rec.mu.Unlock()
	require.Empty(t, posts, "the refusal stays private to the presser")
	require.Empty(t, statuses, "a refused press leaves the running turn's status alone")
	select {
	case <-cancelled:
		t.Fatal("an onlooker's press must not cancel the turn")
	default:
	}
}

// Socket Mode delivers the event in an events_api payload carrying the same
// event_id, so it reaches handleInbound — and the dedup there — exactly like
// the Events API callback.
func TestSocketModePayloadCarriesSessionStopped(t *testing.T) {
	raw := []byte(`{
		"event_id":"Ev-stop-socket",
		"event":{
			"type":"agent_session_stopped",
			"channel":"C0123ABC456",
			"thread_ts":"1782234671.392669",
			"user":"U123ABC456",
			"streaming_message_ts":["1782234987.693923"]
		}
	}`)

	var payload smEventPayload
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, "Ev-stop-socket", payload.EventID)
	require.Equal(t, evtAgentSessionStopped, payload.Event.Type)
	require.Equal(t, stopEventChannel, payload.Event.Channel)
	require.Equal(t, stopEventThreadTS, payload.Event.ThreadTS)
	require.Equal(t, []string{"1782234987.693923"}, payload.Event.StreamingMessageTS)
}
