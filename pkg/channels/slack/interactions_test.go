package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// signRequest adds the Slack signature headers to a request using signingSecret.
func signRequest(t *testing.T, req *http.Request, body []byte, signingSecret string) {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	base := "v0:" + ts + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	_, _ = mac.Write([]byte(base))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
}

// slackInteractionPayload builds a minimal block_actions payload encoded as
// the Slack interactions form field.
func slackInteractionPayload(t *testing.T, actionID, threadID, channelID, messageTS, userID string) []byte {
	t.Helper()
	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": userID},
		"channel":   map[string]any{"id": channelID},
		"container": map[string]any{"message_ts": messageTS},
		"message":   map[string]any{"thread_ts": threadID},
		"actions": []any{
			map[string]any{
				"action_id": actionID,
				"value":     threadID,
			},
		},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	return []byte("payload=" + url.QueryEscape(string(data)))
}

// fakeGateway captures SendCompletion calls.
type fakeGateway struct {
	deltas []channels.OutboundDelta

	mu   sync.Mutex
	sent []channels.InboundMessage
}

func (g *fakeGateway) Resolve(_ context.Context, _ channels.InboundMessage) (channels.InstanceRef, error) {
	return channels.InstanceRef{Name: "i1"}, nil
}
func (g *fakeGateway) SendCompletion(_ context.Context, _ channels.InstanceRef, msg channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	g.mu.Lock()
	g.sent = append(g.sent, msg)
	g.mu.Unlock()
	ch := make(chan channels.OutboundDelta, len(g.deltas)+1)
	for _, d := range g.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func (g *fakeGateway) sentMessages() []channels.InboundMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]channels.InboundMessage(nil), g.sent...)
}
func (g *fakeGateway) FetchHistory(_ context.Context, _ channels.InstanceRef) ([]channels.Message, error) {
	return nil, nil
}

func TestInteractionsHandler_Approve(t *testing.T) {
	const secret = "test-secret"

	// The interactions handler processes the approval asynchronously (Slack
	// requires a fast ack), so the mock server runs in a separate goroutine
	// from the assertions below. Guard the captured calls with a mutex.
	var (
		mu          sync.Mutex
		postCalls   []map[string]any
		updateCalls []map[string]any
	)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body) //nolint:errcheck,gosec

		switch r.URL.Path {
		case "/chat.postMessage":
			var v map[string]any
			_ = json.Unmarshal(buf.Bytes(), &v)
			mu.Lock()
			postCalls = append(postCalls, v)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1000.0001"})
		case "/chat.update":
			var v map[string]any
			_ = json.Unmarshal(buf.Bytes(), &v)
			mu.Lock()
			updateCalls = append(updateCalls, v)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "2000.0001"})
		case "/users.info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"profile": map[string]any{"email": "clicker@example.com"}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("unknown path %s", r.URL.Path)})
		}
	}))
	t.Cleanup(apiSrv.Close)

	gw := &fakeGateway{deltas: []channels.OutboundDelta{
		{Content: "done"},
		{Done: true},
	}}

	a := &Adapter{
		APIBase:      apiSrv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))

	// The clicker must be permitted in the thread (the initiator, here, whose
	// turn created the pending task) or the decision is refused.
	a.accessPolicy().SetInitiator("T001", "U001")

	// Seed a pending task.
	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
	})

	body := slackInteractionPayload(t, "hitl_approve", "T001", "C001", "MSG001", "U001")
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)

	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	// The goroutine runs synchronously-enough for the test but let's give it a moment.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(updateCalls) > 0
	}, 2*time.Second, 10*time.Millisecond)

	// chat.update should have replaced buttons with approval text.
	mu.Lock()
	gotText := updateCalls[0]["text"]
	mu.Unlock()
	require.Equal(t, "✅ _Approved._", gotText)

	// Pending task should be cleared.
	require.Nil(t, a.takePendingTask("T001"))
}

// linkedOBO returns a fixed token for one linked Slack user and
// musterlink.ErrNotLinked for everyone else.
type linkedOBO struct {
	user  string
	token string
}

func (o linkedOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	if slackUserID == o.user {
		return o.token, nil
	}
	return "", musterlink.ErrNotLinked
}
func (o linkedOBO) LinkURL(string) string { return "https://gw.example.com/link" }
func (o linkedOBO) Unlink(string)         {}

// newDecisionAdapter builds an adapter whose Slack API accepts every call,
// recording request paths, with a pending task seeded on thread T001.
func newDecisionAdapter(t *testing.T, gw channels.Gateway, obo OBOTokenSource) (*Adapter, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/users.info" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"profile": map[string]any{"email": "clicker@example.com"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1000.0001"})
	}))
	t.Cleanup(apiSrv.Close)

	a := &Adapter{
		APIBase:      apiSrv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: "test-secret"}, //nolint:gosec
		DefaultAgent: "worker",
		OBO:          obo,
	}
	require.NoError(t, a.Start(t.Context(), gw))
	// The clicker must be permitted; the first user to interact becomes the
	// thread initiator.
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
	})
	return a, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// A button-click resume must carry the clicker's human muster token: the
// approved tool call executes under the approver's identity, never the gateway
// service account (klaus-gateway#116).
func TestHandleDecision_ForwardsHumanToken(t *testing.T) {
	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	a, _ := newDecisionAdapter(t, gw, linkedOBO{user: "U001", token: "human-token"})

	err := a.handleDecision(t.Context(), "C001", "T001", "MSG001", "U001", hitlAction{kind: hitlApprove})
	require.NoError(t, err)

	sent := gw.sentMessages()
	require.Len(t, sent, 1)
	require.Equal(t, "human-token", sent[0].BearerToken,
		"button resume must run under the clicker's human token")
	require.NotNil(t, sent[0].Decision)
}

// An unlinked clicker must not resume the task: the pending task stays stored
// (buttons keep working for a linked user), the gateway sends nothing to the
// agent, and the clicker gets the sign-in prompt.
func TestHandleDecision_UnlinkedClicker_PreservesPendingTask(t *testing.T) {
	gw := &fakeGateway{}
	a, paths := newDecisionAdapter(t, gw, linkedOBO{user: "U-linked", token: "human-token"})

	err := a.handleDecision(t.Context(), "C001", "T001", "MSG001", "U-unlinked", hitlAction{kind: hitlApprove})
	require.NoError(t, err)

	require.Empty(t, gw.sentMessages(), "aborted resume must not reach the agent")
	require.NotNil(t, a.takePendingTask("T001"), "aborted resume must leave the pending task intact")
	require.Contains(t, paths(), "/chat.postEphemeral", "unlinked clicker must be prompted to sign in")
}

func TestInteractionsHandler_InvalidSignature(t *testing.T) {
	a := &Adapter{
		Secrets:      Secrets{SigningSecret: "correct-secret"},
		DefaultAgent: "worker",
	}
	a.ixHandler = &interactionsHandler{signingSecret: "correct-secret", adapter: a, ctx: t.Context()}

	body := []byte("payload=%7B%7D")
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", "1000000")
	req.Header.Set("X-Slack-Signature", "v0=badhash")

	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestInteractionsHandler_NoPendingTask(t *testing.T) {
	// If no pending task, handleApproval should be a no-op (no panic).
	const secret = "test-secret"

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Accept chat.update (button replacement) but no resume calls expected.
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.1"})
	}))
	t.Cleanup(apiSrv.Close)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      apiSrv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))
	a.accessPolicy().SetInitiator("T_NONE", "U001") // clicker is permitted; exercise the no-pending-task path

	body := slackInteractionPayload(t, "hitl_deny", "T_NONE", "C001", "MSG001", "U001")
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)

	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
}

func TestInteractionsHandler_OnlookerCannotDecide(t *testing.T) {
	const secret = "test-secret"

	var (
		mu        sync.Mutex
		ephemeral int
		resumes   int
	)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postEphemeral":
			mu.Lock()
			ephemeral++
			mu.Unlock()
		case "/chat.update", "/chat.postMessage":
			mu.Lock()
			resumes++
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.1"})
	}))
	t.Cleanup(apiSrv.Close)

	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	a := &Adapter{
		APIBase:      apiSrv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))

	// U001 owns the thread; a pending tool call awaits a decision.
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001"})

	// An onlooker (U999) clicks Approve.
	body := slackInteractionPayload(t, "hitl_approve", "T001", "C001", "MSG001", "U999")
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)
	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ephemeral > 0
	}, 2*time.Second, 10*time.Millisecond, "onlooker gets an ephemeral refusal")

	// Give any erroneous resume a chance to land, then confirm none did and the
	// pending task is intact for the real owner.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	require.Zero(t, resumes, "an onlooker click must not resume or cancel the task")
	mu.Unlock()
	require.NotNil(t, a.takePendingTask("T001"), "pending task left intact for the owner")
}

func TestIsActiveThread(t *testing.T) {
	a := &Adapter{}

	require.False(t, a.isActiveThread("T001"))

	// A known initiator makes it active.
	a.accessPolicy().SetInitiator("T001", "U001")
	require.True(t, a.isActiveThread("T001"))

	// Pending task on a different thread.
	a.storePendingTask("T002", &pendingTask{TaskID: "x"})
	require.True(t, a.isActiveThread("T002"))
	require.False(t, a.isActiveThread("T003"))
}

func TestThreadReplyRoutedWithoutMention(t *testing.T) {
	// A message event with thread_ts set to an active thread should pass
	// toInboundMessage with threadReplyOnly=true.
	ev := slackInnerEvent{
		Type:     "message",
		User:     "U001",
		Text:     "yes please",
		Channel:  "C001",
		TS:       "2000.001",
		ThreadTS: "1000.001",
	}
	msg, ok := ev.toInboundMessage(true)
	require.True(t, ok)
	require.Equal(t, "1000.001", msg.ThreadID)
	require.Equal(t, "yes please", msg.Text)
}

func TestThreadReplyDroppedWhenNotInThread(t *testing.T) {
	// Top-level message.channels event should be dropped in threadReplyOnly mode.
	ev := slackInnerEvent{
		Type:    "message",
		User:    "U001",
		Text:    "hello",
		Channel: "C001",
		TS:      "2000.001",
	}
	_, ok := ev.toInboundMessage(true)
	require.False(t, ok)
}

// An Approve click on buttons posted before a gateway restart must recover the
// paused task from the kagent task store and resume it under the clicker's
// decision, instead of replying "Already answered.".
func TestHandleDecision_RecoversPendingTaskAfterRestart(t *testing.T) {
	gw := &recoveringGateway{
		fakeGateway: fakeGateway{deltas: []channels.OutboundDelta{{Content: "resumed"}, {Done: true}}},
		taskID:      "task-restored",
		prompt:      &channels.HitlPrompt{ToolName: "kubectl_delete"},
	}
	a, _ := newDecisionAdapter(t, gw, nil)
	// Simulate the restart: no in-memory pending task for the thread.
	require.NotNil(t, a.takePendingTask("T001"))

	err := a.handleDecision(t.Context(), "C001", "T001", "MSG001", "U001", hitlAction{kind: hitlApprove})
	require.NoError(t, err)

	sent := gw.sentMessages()
	require.Len(t, sent, 1)
	require.Equal(t, "task-restored", sent[0].TaskID)
	require.NotNil(t, sent[0].Decision)
	require.Equal(t, channels.DecisionApprove, sent[0].Decision.Type)
}
