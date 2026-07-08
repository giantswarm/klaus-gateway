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
	"strings"
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

	mu      sync.Mutex
	sent    []channels.InboundMessage
	sends   int
	lastMsg channels.InboundMessage
}

func (g *fakeGateway) Resolve(_ context.Context, _ channels.InboundMessage) (channels.InstanceRef, error) {
	return channels.InstanceRef{Name: "i1"}, nil
}
func (g *fakeGateway) SendCompletion(_ context.Context, _ channels.InstanceRef, msg channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	g.mu.Lock()
	g.sent = append(g.sent, msg)
	g.sends++
	g.lastMsg = msg
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

// sendCount reports how many times SendCompletion (a task resume) was called.
func (g *fakeGateway) sendCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sends
}

// lastCompletion returns the message of the most recent SendCompletion call.
func (g *fakeGateway) lastCompletion() channels.InboundMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastMsg
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

	body := slackInteractionPayload(t, "hitl_deny", "T_NONE", "C001", "MSG001", "U001")
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)

	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
}

// --- OBO-enabled button-resume (handleDecision) ---

// decisionOBO is a test OBOTokenSource for the button-resume path: it mints
// token for linkedUser and returns musterlink.ErrNotLinked for anyone else (an
// unlinked / different clicker), which drives the sign-in prompt.
type decisionOBO struct {
	linkedUser string
	token      string
}

func (o *decisionOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	if slackUserID == o.linkedUser {
		return o.token, nil
	}
	return "", musterlink.ErrNotLinked
}

func (o *decisionOBO) LinkURL(slackUserID string) string {
	return "https://gw.example.com/auth/slack/link?u=signed-" + slackUserID
}

func (o *decisionOBO) Unlink(string) {}

// ixSink records the Slack Web API calls the interactions path makes.
type ixSink struct {
	mu        sync.Mutex
	posts     []map[string]any
	updates   []map[string]any
	ephemeral []map[string]any
}

func (s *ixSink) counts() (posts, updates, ephemeral int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts), len(s.updates), len(s.ephemeral)
}

func (s *ixSink) updateTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.updates))
	for _, u := range s.updates {
		if t, ok := u["text"].(string); ok {
			out = append(out, t)
		}
	}
	return out
}

// newIxSlackServer starts a fake Slack Web API that records chat.postMessage,
// chat.update, and chat.postEphemeral calls and acks everything.
func newIxSlackServer(t *testing.T) (*httptest.Server, *ixSink) {
	t.Helper()
	s := &ixSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body) //nolint:errcheck,gosec
		var v map[string]any
		_ = json.Unmarshal(buf.Bytes(), &v)
		switch {
		case strings.HasSuffix(r.URL.Path, "chat.postMessage"):
			s.mu.Lock()
			s.posts = append(s.posts, v)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1000.0001"})
		case strings.HasSuffix(r.URL.Path, "chat.update"):
			s.mu.Lock()
			s.updates = append(s.updates, v)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "2000.0001"})
		case strings.HasSuffix(r.URL.Path, "chat.postEphemeral"):
			s.mu.Lock()
			s.ephemeral = append(s.ephemeral, v)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "users.info"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"profile": map[string]any{"email": "clicker@example.com"}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

// serveInteraction signs and drives a block_actions payload through the
// interactions HTTP handler.
func serveInteraction(t *testing.T, a *Adapter, secret, actionID, threadID, channelID, messageTS, userID string) {
	t.Helper()
	body := slackInteractionPayload(t, actionID, threadID, channelID, messageTS, userID)
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)
	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
}

// A button click whose clicker cannot mint a human token (here: an unlinked /
// different clicker) must leave the pending task AND its buttons intact and drive
// the sign-in prompt, so the click stays retryable — never consume the task or
// rewrite the message to "Approved" before the token gate.
func TestHandleDecision_OBO_TokenMintFailurePreservesTask(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
		OBO:          &decisionOBO{linkedUser: "U_LINKED", token: "human-token"},
	}
	require.NoError(t, a.Start(t.Context(), gw))

	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
	})

	serveInteraction(t, a, secret, "hitl_approve", "T001", "C001", "MSG001", "U_OTHER")

	// The sign-in prompt (ephemeral) is the terminal action on the failure path.
	require.Eventually(t, func() bool {
		_, _, eph := sink.counts()
		return eph >= 1
	}, 2*time.Second, 10*time.Millisecond, "token-mint failure must drive a sign-in prompt")

	posts, updates, _ := sink.counts()
	require.Zero(t, updates, "buttons must not be rewritten on token-mint failure")
	require.Zero(t, posts, "no resume placeholder must be posted on token-mint failure")
	require.True(t, a.hasPendingTask("T001"), "pending task must be preserved for retry")
	require.Zero(t, gw.sendCount(), "the paused task must not be resumed on token-mint failure")
}

// On a successful token mint the button-resume path is unchanged: the message is
// rewritten to the approval text, the task is consumed, and the resume carries
// the clicker's human token.
func TestHandleDecision_OBO_SuccessResumes(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
		OBO:          &decisionOBO{linkedUser: "U_LINKED", token: "human-token"},
	}
	require.NoError(t, a.Start(t.Context(), gw))

	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
	})

	serveInteraction(t, a, secret, "hitl_approve", "T001", "C001", "MSG001", "U_LINKED")

	require.Eventually(t, func() bool {
		return gw.sendCount() >= 1
	}, 2*time.Second, 10*time.Millisecond, "a linked clicker must resume the paused task")

	require.Contains(t, sink.updateTexts(), "✅ _Approved._", "success path must rewrite the message to the approval text")
	require.False(t, a.hasPendingTask("T001"), "resumed task must be consumed")
	require.Equal(t, "human-token", gw.lastCompletion().BearerToken, "the resume must carry the clicker's human token")
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
