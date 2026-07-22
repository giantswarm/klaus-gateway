package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// resolveErr, when set, is returned by every Resolve call.
	resolveErr error
	// onResetSession, when set, backs ResetSession; nil reports the reset as
	// unavailable (false, nil).
	onResetSession func(msg channels.InboundMessage) (bool, error)

	mu      sync.Mutex
	sent    []channels.InboundMessage
	sends   int
	lastMsg channels.InboundMessage
}

func (g *fakeGateway) ResetSession(_ context.Context, msg channels.InboundMessage) (bool, error) {
	if g.onResetSession == nil {
		return false, nil
	}
	return g.onResetSession(msg)
}

func (g *fakeGateway) Resolve(_ context.Context, _ channels.InboundMessage) (channels.InstanceRef, error) {
	if g.resolveErr != nil {
		return channels.InstanceRef{}, g.resolveErr
	}
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
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

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
	}, 10*time.Second, 10*time.Millisecond)

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
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
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

// multiUserOBO mints a distinct token per Slack user; a user absent from the
// map is treated as not linked.
type multiUserOBO map[string]string

func (o multiUserOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	if tok, ok := o[slackUserID]; ok {
		return tok, nil
	}
	return "", musterlink.ErrNotLinked
}
func (multiUserOBO) LinkURL(string) string { return "https://gw.example.com/link" }
func (multiUserOBO) Unlink(string)         {}

// A granted collaborator's button decision resumes the shared session under the
// thread initiator's token, not the clicker's, with the clicker attached as
// attribution — matching the typed-turn path in dispatch so a click and a typed
// "approve" reply cannot fork the session differently.
func TestHandleDecision_CollaboratorClickForwardsInitiatorToken(t *testing.T) {
	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	// newDecisionAdapter makes U001 the initiator; U002 is a granted collaborator.
	a, _ := newDecisionAdapter(t, gw, multiUserOBO{"U001": "tok-initiator", "U002": "tok-collab"})
	a.accessPolicy().Grant("T001", "U002")

	err := a.handleDecision(t.Context(), "C001", "T001", "MSG001", "U002", hitlAction{kind: hitlApprove})
	require.NoError(t, err)

	sent := gw.sentMessages()
	require.Len(t, sent, 1)
	require.Equal(t, "tok-initiator", sent[0].BearerToken,
		"collaborator's click must run under the initiator's token, not the clicker's")
	require.Equal(t, "clicker@example.com", sent[0].Author,
		"the real clicker is attached as attribution")
}

// When the initiator's token cannot be minted (unlinked), a granted
// collaborator's click falls back to the clicker's own identity rather than the
// gateway service account, and carries no attribution.
func TestHandleDecision_CollaboratorClickFallsBackWhenInitiatorUnavailable(t *testing.T) {
	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	// U001 is the initiator but unlinked; U002 is a granted, linked collaborator.
	a, _ := newDecisionAdapter(t, gw, multiUserOBO{"U002": "tok-collab"})
	a.accessPolicy().Grant("T001", "U002")

	err := a.handleDecision(t.Context(), "C001", "T001", "MSG001", "U002", hitlAction{kind: hitlApprove})
	require.NoError(t, err)

	sent := gw.sentMessages()
	require.Len(t, sent, 1)
	require.Equal(t, "tok-collab", sent[0].BearerToken,
		"fallback runs under the clicker's own token when the initiator's is unavailable")
	require.Empty(t, sent[0].Author, "fallback turn is not delegated, so no attribution")
}

// A corrupt-history failure on a button resume resets the session like a
// typed turn does, and the reset presents the identity the turn ran under
// (the initiator's token for a collaborator's click, since kagent keys the
// session lookup on the token's principal), so it deletes the session the
// turn actually hit.
func TestHandleDecision_CorruptSessionResetsUnderTurnIdentity(t *testing.T) {
	corrupt := errors.New("a2a error -32603: Error code: 400 - {'type': 'error', 'error': {'type': 'invalid_request_error', 'message': '`tool_use` ids were found without `tool_result` blocks'}}")
	var (
		mu     sync.Mutex
		resets []channels.InboundMessage
	)
	gw := &fakeGateway{
		deltas: []channels.OutboundDelta{{Err: corrupt}},
		onResetSession: func(msg channels.InboundMessage) (bool, error) {
			mu.Lock()
			resets = append(resets, msg)
			mu.Unlock()
			return true, nil
		},
	}
	a, paths := newDecisionAdapter(t, gw, multiUserOBO{"U001": "tok-initiator", "U002": "tok-collab"})
	a.accessPolicy().Grant("T001", "U002")

	err := a.handleDecision(t.Context(), "C001", "T001", "MSG001", "U002", hitlAction{kind: hitlApprove})
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, resets, 1, "the corrupt session is deleted once")
	require.Equal(t, "T001", resets[0].ThreadID)
	require.Equal(t, "tok-initiator", resets[0].BearerToken,
		"the reset must present the token the turn ran under, not the clicker's own")
	require.Contains(t, paths(), "/chat.postMessage", "the thread is told the session was reset")
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
	// U-unlinked is not allowed on the thread (U-linked would be the
	// initiator), so the notice hit here is the access refusal, not the
	// sign-in prompt; the sign-in path is pinned by
	// TestHandleDecision_OBO_TokenMintFailurePreservesTask.
	require.Contains(t, paths(), "/chat.postEphemeral", "the clicker must be told why nothing happened")
}

// The sign-in URL button opens its link in the browser; its block_actions
// payload is acked without action: no Slack API call, no pending-task
// consumption.
func TestSignInClickIsBareAck(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	a.storePendingTask("T001", &pendingTask{TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001"})

	serveInteraction(t, a, secret, oboSignIn, "T001", "C001", "MSG001", "U001")

	time.Sleep(100 * time.Millisecond)
	posts, updates, ephemeral := sink.counts()
	require.Zero(t, posts+updates+ephemeral, "a sign-in click must trigger no Slack call")
	require.True(t, a.hasPendingTask("T001"), "a sign-in click must not consume the pending task")
}

// A button decision that fails before the stream starts must tell the thread:
// the prompt message already shows the decision text, so silence reads as
// success while the task quietly went nowhere.
func TestHandleDecision_ResumeFailurePostsNote(t *testing.T) {
	gw := &fakeGateway{resolveErr: errors.New("kagent down")}
	a, paths := newDecisionAdapter(t, gw, linkedOBO{user: "U001", token: "human-token"})

	err := a.handleDecision(t.Context(), "C001", "T001", "MSG001", "U001", hitlAction{kind: hitlApprove})
	require.Error(t, err)
	require.NotNil(t, a.takePendingTask("T001"), "the task must be re-stored so a typed reply can retry")
	require.Contains(t, paths(), "/chat.postMessage", "a failure note must reach the thread")
}

func TestInteractionsHandler_InvalidSignature(t *testing.T) {
	a := &Adapter{
		Secrets:      Secrets{SigningSecret: "correct-secret"},
		DefaultAgent: "worker",
	}
	a.ixHandler = &interactionsHandler{signingSecret: "correct-secret", adapter: a}

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
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	a.accessPolicy().SetInitiator("T_NONE", "U001") // clicker is permitted; exercise the no-pending-task path

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

func (s *ixSink) postTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.posts))
	for _, p := range s.posts {
		if t, ok := p["text"].(string); ok {
			out = append(out, t)
		}
	}
	return out
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

func (s *ixSink) ephemeralTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.ephemeral))
	for _, e := range s.ephemeral {
		if t, ok := e["text"].(string); ok {
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
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	a.accessPolicy().SetInitiator("T001", "U_OTHER") // clicker owns the thread; isolate the token gate
	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
	})

	serveInteraction(t, a, secret, "hitl_approve", "T001", "C001", "MSG001", "U_OTHER")

	// The sign-in prompt (a real in-thread message) is the terminal action on
	// the failure path.
	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(sink.postTexts(), "\n"), "Sign in so I can act as you")
	}, 10*time.Second, 10*time.Millisecond, "token-mint failure must drive a sign-in prompt")

	posts, updates, _ := sink.counts()
	require.Zero(t, updates, "buttons must not be rewritten on token-mint failure")
	require.Equal(t, 1, posts, "the sign-in prompt must be the only message posted (no resume placeholder)")
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
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	a.accessPolicy().SetInitiator("T001", "U_LINKED") // clicker owns the thread
	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
	})

	serveInteraction(t, a, secret, "hitl_approve", "T001", "C001", "MSG001", "U_LINKED")

	require.Eventually(t, func() bool {
		return gw.sendCount() >= 1
	}, 10*time.Second, 10*time.Millisecond, "a linked clicker must resume the paused task")

	require.Contains(t, sink.updateTexts(), "✅ _Approved._", "success path must rewrite the message to the approval text")
	require.False(t, a.hasPendingTask("T001"), "resumed task must be consumed")
	require.Equal(t, "human-token", gw.lastCompletion().BearerToken, "the resume must carry the clicker's human token")
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
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	// U001 owns the thread; a pending tool call awaits a decision.
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001"})

	// An onlooker (U999) clicks Approve.
	serveInteraction(t, a, secret, "hitl_approve", "T001", "C001", "MSG001", "U999")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ephemeral > 0
	}, 10*time.Second, 10*time.Millisecond, "onlooker gets an ephemeral refusal")

	// Give any erroneous resume a chance to land, then confirm none did and the
	// pending task is intact for the real owner.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	require.Zero(t, resumes, "an onlooker click must not resume or cancel the task")
	mu.Unlock()
	require.NotNil(t, a.takePendingTask("T001"), "pending task left intact for the owner")
}

func TestSelectedChoiceIndices(t *testing.T) {
	raw := `{"values":{
		"` + hitlGroupBlock + `":{"` + hitlGroup + `":{
			"selected_options":[{"value":"2"},{"value":"0"}]}},
		"` + hitlGroupBlock + `_1":{"` + hitlGroup + `":{
			"selected_option":{"value":"1"}}},
		"noise":{"other":{"selected_option":{"value":"not-an-index"}}}
	}}`
	var state struct {
		Values map[string]map[string]blockActionState `json:"values"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	flat, _ := choiceSelections(state)
	require.Equal(t, []int{0, 1, 2}, flat)
}

func TestSelectedChoicesByQuestion(t *testing.T) {
	raw := `{"values":{
		"` + hitlQGroupPrefix + `_0":{"` + hitlGroup + `":{
			"selected_options":[{"value":"2"},{"value":"0"}]}},
		"` + hitlQGroupPrefix + `_1":{"` + hitlGroup + `":{
			"selected_option":{"value":"1"}}},
		"` + hitlGroupBlock + `":{"` + hitlGroup + `":{"selected_option":{"value":"9"}}},
		"noise":{"other":{"selected_option":{"value":"3"}}}
	}}`
	var state struct {
		Values map[string]map[string]blockActionState `json:"values"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	_, byQuestion := choiceSelections(state)
	require.Equal(t, map[int][]int{0: {0, 2}, 1: {1}}, byQuestion, "only hitl_q_<qi> blocks are grouped per question")
}

// A multi-question ask_user form resumes with one answer slot per question, in
// question order, read out of the per-question state.values blocks.
func TestHandleDecision_FormResumesWithPerQuestionAnswers(t *testing.T) {
	const secret = "test-secret"
	srv, _ := newIxSlackServer(t)

	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))

	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
		Prompt: &channels.HitlPrompt{
			ToolName: channels.AskUserToolName,
			Questions: []channels.HitlQuestion{
				{Question: "Database?", Choices: []string{"PostgreSQL", "MySQL"}},
				{Question: "Features?", Multiple: true, Choices: []string{"Auth", "Logging", "Caching"}},
			},
		},
	})

	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": "U001"},
		"channel":   map[string]any{"id": "C001"},
		"container": map[string]any{"message_ts": "MSG001"},
		"message":   map[string]any{"thread_ts": "T001"},
		"actions":   []any{map[string]any{"action_id": hitlSubmit, "value": "T001"}},
		"state": map[string]any{"values": map[string]any{
			hitlQGroupPrefix + "_0": map[string]any{hitlGroup: map[string]any{
				"selected_option": map[string]any{"value": "1"},
			}},
			hitlQGroupPrefix + "_1": map[string]any{hitlGroup: map[string]any{
				"selected_options": []any{
					map[string]any{"value": "0"},
					map[string]any{"value": "2"},
				},
			}},
		}},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)
	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	require.Eventually(t, func() bool { return gw.sendCount() >= 1 }, 10*time.Second, 10*time.Millisecond)

	msg := gw.lastCompletion()
	require.NotNil(t, msg.Decision)
	require.Equal(t, channels.DecisionApprove, msg.Decision.Type)
	require.Equal(t, [][]string{{"MySQL"}, {"Auth", "Caching"}}, msg.Decision.AskUserAnswers)
}

// A Submit on a multi-question form with a question still unanswered must leave
// the task pending and nudge, not resume with an empty answer slot.
func TestHandleDecision_FormIncompleteNudges(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001",
		Prompt: &channels.HitlPrompt{
			ToolName: channels.AskUserToolName,
			Questions: []channels.HitlQuestion{
				{Question: "Database?", Choices: []string{"PostgreSQL", "MySQL"}},
				{Question: "Features?", Choices: []string{"Auth", "Logging"}},
			},
		},
	})

	// Only the first question is answered.
	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": "U001"},
		"channel":   map[string]any{"id": "C001"},
		"container": map[string]any{"message_ts": "MSG001"},
		"message":   map[string]any{"thread_ts": "T001"},
		"actions":   []any{map[string]any{"action_id": hitlSubmit, "value": "T001"}},
		"state": map[string]any{"values": map[string]any{
			hitlQGroupPrefix + "_0": map[string]any{hitlGroup: map[string]any{
				"selected_option": map[string]any{"value": "0"},
			}},
		}},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)
	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	require.Eventually(t, func() bool {
		_, _, eph := sink.counts()
		return eph >= 1
	}, 10*time.Second, 10*time.Millisecond, "incomplete form must nudge the user")
	require.Zero(t, gw.sendCount(), "incomplete form must not resume the task")
	require.True(t, a.hasPendingTask("T001"), "incomplete form must leave the task pending")
	require.Contains(t, sink.ephemeralTexts(), formIncompleteNudge, "partial form must use the answer-every-question nudge")
	posts, updates, _ := sink.counts()
	require.Zero(t, posts, "incomplete form must not mint a token or prompt sign-in")
	require.Zero(t, updates, "incomplete form must not overwrite the form message")
}

// A multi-question form Submit with nothing selected at all must use the
// answer-every-question nudge, not the single-question pick-an-option nudge.
func TestHandleDecision_FormEmptyUsesFormNudge(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001",
		Prompt: &channels.HitlPrompt{
			ToolName: channels.AskUserToolName,
			Questions: []channels.HitlQuestion{
				{Question: "Database?", Choices: []string{"PostgreSQL", "MySQL"}},
				{Question: "Features?", Choices: []string{"Auth", "Logging"}},
			},
		},
	})

	// hitl_submit with no state → nothing selected on any question.
	serveInteraction(t, a, secret, hitlSubmit, "T001", "C001", "MSG001", "U001")

	require.Eventually(t, func() bool {
		return slices.Contains(sink.ephemeralTexts(), formIncompleteNudge)
	}, 10*time.Second, 10*time.Millisecond, "empty form must use the answer-every-question nudge")
	require.Zero(t, gw.sendCount(), "empty form must not resume the task")
	require.True(t, a.hasPendingTask("T001"), "empty form must leave the task pending")
}

// A Submit whose selected indices are all out of range for the pending prompt
// (e.g. a click on a stale form message) must be treated as incomplete and
// nudged, never resumed with an empty answer slot.
func TestHandleDecision_FormStaleSelectionNudges(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001",
		Prompt: &channels.HitlPrompt{
			ToolName: channels.AskUserToolName,
			Questions: []channels.HitlQuestion{
				{Question: "Database?", Choices: []string{"PostgreSQL", "MySQL"}},
				{Question: "Features?", Choices: []string{"Auth", "Logging"}},
			},
		},
	})

	// Both questions "answered", but every index is out of range for the two
	// two-choice questions of the current prompt.
	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": "U001"},
		"channel":   map[string]any{"id": "C001"},
		"container": map[string]any{"message_ts": "MSG001"},
		"message":   map[string]any{"thread_ts": "T001"},
		"actions":   []any{map[string]any{"action_id": hitlSubmit, "value": "T001"}},
		"state": map[string]any{"values": map[string]any{
			hitlQGroupPrefix + "_0": map[string]any{hitlGroup: map[string]any{
				"selected_option": map[string]any{"value": "5"},
			}},
			hitlQGroupPrefix + "_1": map[string]any{hitlGroup: map[string]any{
				"selected_option": map[string]any{"value": "9"},
			}},
		}},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)
	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	require.Eventually(t, func() bool {
		return slices.Contains(sink.ephemeralTexts(), formIncompleteNudge)
	}, 10*time.Second, 10*time.Millisecond, "stale form selection must nudge, not resume")
	require.Zero(t, gw.sendCount(), "stale form selection must not resume the task")
	require.True(t, a.hasPendingTask("T001"), "stale form selection must leave the task pending")
}

// A Submit click on a multi-select ask_user widget resumes the paused task with
// the selected choice labels, read out of state.values.
func TestHandleDecision_SubmitResumesWithSelectedAnswers(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-abc",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
		Prompt:    askUserPrompt(true, "Auth", "Logging", "Caching"),
	})

	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": "U001"},
		"channel":   map[string]any{"id": "C001"},
		"container": map[string]any{"message_ts": "MSG001"},
		"message":   map[string]any{"thread_ts": "T001"},
		"actions":   []any{map[string]any{"action_id": hitlSubmit, "value": "T001"}},
		"state": map[string]any{"values": map[string]any{
			hitlGroupBlock: map[string]any{hitlGroup: map[string]any{
				"selected_options": []any{
					map[string]any{"value": "0"},
					map[string]any{"value": "2"},
				},
			}},
		}},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)
	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	require.Eventually(t, func() bool { return gw.sendCount() >= 1 }, 10*time.Second, 10*time.Millisecond)

	msg := gw.lastCompletion()
	require.NotNil(t, msg.Decision)
	require.Equal(t, channels.DecisionApprove, msg.Decision.Type)
	require.Equal(t, [][]string{{"Auth", "Caching"}}, msg.Decision.AskUserAnswers)
	require.Contains(t, sink.updateTexts(), "👉 _Auth, Caching_", "message rewritten to the selection")
}

// A Submit click with nothing selected must leave the task pending and nudge the
// user, not resume with an empty answer.
func TestHandleDecision_SubmitWithNoSelectionIsNudged(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001",
		Prompt: askUserPrompt(false, "A", "B"),
	})

	// hitl_submit with no state → no selection.
	serveInteraction(t, a, secret, hitlSubmit, "T001", "C001", "MSG001", "U001")

	require.Eventually(t, func() bool {
		_, _, eph := sink.counts()
		return eph >= 1
	}, 10*time.Second, 10*time.Millisecond, "empty submit must nudge the user")
	require.Zero(t, gw.sendCount(), "empty submit must not resume the task")
	require.True(t, a.hasPendingTask("T001"), "empty submit must leave the task pending")
}

// An onlooker clicking Submit with nothing selected must be refused by the
// access gate in handleDecision, not handed the incomplete-form nudge (which
// would let them probe the pending widget). Mirrors
// TestInteractionsHandler_OnlookerCannotDecide for the empty-submit path.
func TestHandleDecision_OnlookerEmptySubmitIsRefused(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001",
		Prompt: askUserPrompt(false, "A", "B"),
	})

	// U999 does not own the thread and was never granted access.
	serveInteraction(t, a, secret, hitlSubmit, "T001", "C001", "MSG001", "U999")

	require.Eventually(t, func() bool {
		_, _, eph := sink.counts()
		return eph >= 1
	}, 10*time.Second, 10*time.Millisecond, "onlooker gets an ephemeral response")
	require.Contains(t, sink.ephemeralTexts(), accessDecisionRefusal, "onlooker must be refused, not nudged")
	require.NotContains(t, sink.ephemeralTexts(), choiceSelectNudge, "onlooker must not see the choice-select nudge")
	require.Zero(t, gw.sendCount(), "onlooker submit must not resume the task")
	require.True(t, a.hasPendingTask("T001"), "onlooker submit must leave the task pending")
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

// TestParkPendingLogin_OrderedAndCappedPerThread verifies the login buffer keeps
// messages per thread in arrival order, caps each thread's queue (dropping the
// oldest), and isolates threads from one another.
func TestParkPendingLogin_OrderedAndCappedPerThread(t *testing.T) {
	a := &Adapter{}
	for i := 1; i <= maxParkedPerThread+2; i++ {
		a.parkPendingLogin("U1", &pendingLoginReq{
			msg: channels.InboundMessage{ThreadID: "T1", Text: fmt.Sprintf("m%d", i)},
		})
	}
	a.parkPendingLogin("U1", &pendingLoginReq{msg: channels.InboundMessage{ThreadID: "T2", Text: "other"}})

	got := a.takePendingLogin("U1")
	require.Len(t, got["T1"], maxParkedPerThread, "each thread's queue is capped")
	var texts []string
	for _, r := range got["T1"] {
		texts = append(texts, r.msg.Text)
	}
	require.Equal(t, []string{"m3", "m4", "m5", "m6", "m7"}, texts, "oldest dropped past the cap, order preserved")
	require.Len(t, got["T2"], 1, "other threads are independent")
	require.Nil(t, a.takePendingLogin("U1"), "take clears the user")
}

// captureChatUpdates spins up a fake Slack API that records chat.update JSON
// bodies and returns ok for everything else.
func captureChatUpdates(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var updates []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chat.update") {
			var v map[string]any
			_ = json.NewDecoder(r.Body).Decode(&v)
			mu.Lock()
			updates = append(updates, v)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), updates...)
	}
}

// A recorded sign-in prompt is rewritten in place (chat.update on its anchor)
// once the link completes, and the anchor is drained so a second link event is
// a no-op.
func TestOnUserLinkedRewritesSignInAnchor(t *testing.T) {
	srv, updates := captureChatUpdates(t)

	a := &Adapter{
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase: srv.URL,
	}
	a.recordSignInAnchor("U001", "T1", signInAnchor{channel: "C1", ts: "111.111"})

	a.OnUserLinked(t.Context(), "U001", "alice@example.com")
	require.Eventually(t, func() bool { return len(updates()) == 1 },
		10*time.Second, 10*time.Millisecond, "the anchor must be rewritten after linking")
	call := updates()[0]
	require.Equal(t, "C1", call["channel"])
	require.Equal(t, "111.111", call["ts"])
	text, _ := call["text"].(string)
	require.Contains(t, text, "Signed in")
	require.NotContains(t, text, "@", "the in-thread rewrite carries no email; identity is confirmed on the browser page")
	require.NotContains(t, text, "Bringing in", "no parked replay, no handoff wording")

	// Anchors are drained on first use.
	a.OnUserLinked(t.Context(), "U001", "alice@example.com")
	time.Sleep(100 * time.Millisecond)
	require.Len(t, updates(), 1, "a second link event must not rewrite again")
}

// A failed anchor rewrite re-records the anchor: the drained entry would
// otherwise leave a live sign-in button for a linked user forever, with no
// later pass able to converge it.
func TestOnUserLinkedFailedRewriteRerecordsAnchor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"internal_error"}`))
	}))
	t.Cleanup(srv.Close)

	a := &Adapter{
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase: srv.URL,
		Logger:  slog.New(slog.DiscardHandler),
	}
	a.recordSignInAnchor("U001", "T1", signInAnchor{channel: "C1", ts: "111.111"})

	a.OnUserLinked(t.Context(), "U001", "alice@example.com")
	require.Eventually(t, func() bool {
		a.signInPromptedMu.Lock()
		defer a.signInPromptedMu.Unlock()
		entry, ok := a.signInPrompted["U001\x00T1"]
		return ok && entry.value.ts == "111.111"
	}, 10*time.Second, 10*time.Millisecond, "the anchor must survive a failed rewrite")
}

// An anchor re-recorded after a failed rewrite is converged by the user's next
// successful token use, so the stale "Sign in" button does not outlive the
// transient Slack error that stranded it.
func TestTokenUseConvergesFailedAnchorRewrite(t *testing.T) {
	var updates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "chat.update") && updates.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"ok":false,"error":"internal_error"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1234.5678"}`))
	}))
	t.Cleanup(srv.Close)

	a := &Adapter{
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase: srv.URL,
		Logger:  slog.New(slog.DiscardHandler),
		OBO:     linkedOBO{user: "U001", token: "human-token"},
	}
	a.recordSignInAnchor("U001", "T1", signInAnchor{channel: "C1", ts: "111.111"})

	a.OnUserLinked(t.Context(), "U001", "alice@example.com")
	// updates==1 proves the link-time rewrite drained the anchor and failed;
	// an entry present after that is therefore the re-record, not the original.
	require.Eventually(t, func() bool {
		if updates.Load() != 1 {
			return false
		}
		a.signInPromptedMu.Lock()
		defer a.signInPromptedMu.Unlock()
		entry, ok := a.signInPrompted["U001\x00T1"]
		return ok && entry.value.ts == "111.111"
	}, 10*time.Second, 10*time.Millisecond, "the first rewrite fails and re-records the anchor")

	_, ok, _ := a.humanToken(t.Context(), "C1", "T1", "U001")
	require.True(t, ok)
	require.Eventually(t, func() bool {
		if updates.Load() != 2 {
			return false
		}
		a.signInPromptedMu.Lock()
		defer a.signInPromptedMu.Unlock()
		_, stillThere := a.signInPrompted["U001\x00T1"]
		return !stillThere
	}, 10*time.Second, 10*time.Millisecond, "a successful token use must retry the rewrite and drain the anchor")
}

// A link that is about to replay a parked question folds the agent handoff
// notice into the rewritten prompt.
func TestOnUserLinkedAnchorAnnouncesHandoffWhenReplaying(t *testing.T) {
	srv, updates := captureChatUpdates(t)

	a := &Adapter{
		Secrets:      Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase:      srv.URL,
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	a.accessPolicy().SetInitiator("T1", "U001") // the parked user owns the thread, so the replay reaches the agent
	a.recordSignInAnchor("U001", "T1", signInAnchor{channel: "C1", ts: "111.111"})
	a.parkPendingLogin("U001", &pendingLoginReq{
		msg:          channels.InboundMessage{Subject: "U001", ThreadID: "T1", MessageID: "T1", Text: "what failed?"},
		slackChannel: "C1",
	})

	a.OnUserLinked(t.Context(), "U001", "alice@example.com")
	require.Eventually(t, func() bool { return len(updates()) == 1 },
		10*time.Second, 10*time.Millisecond, "the anchor must be rewritten after linking")
	text, _ := updates()[0]["text"].(string)
	require.Contains(t, text, "Signed in")
	require.NotContains(t, text, "@", "the in-thread rewrite carries no email")
	require.Contains(t, text, "worker", "the rewrite announces the agent handoff")
}

// A newcomer's replay lands at the initiator's consent prompt, not the agent,
// so their rewritten prompt keeps the plain confirmation.
func TestOnUserLinkedAnchorPlainForUnapprovedNewcomer(t *testing.T) {
	srv, updates := captureChatUpdates(t)

	a := &Adapter{
		Secrets:      Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase:      srv.URL,
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	a.accessPolicy().SetInitiator("T1", "U_OWNER") // someone else owns the thread
	a.recordSignInAnchor("U999", "T1", signInAnchor{channel: "C1", ts: "111.111"})
	a.parkPendingLogin("U999", &pendingLoginReq{
		msg:          channels.InboundMessage{Subject: "U999", ThreadID: "T1", MessageID: "222.222", Text: "me too"},
		slackChannel: "C1",
	})

	a.OnUserLinked(t.Context(), "U999", "bob@example.com")
	require.Eventually(t, func() bool { return len(updates()) == 1 },
		10*time.Second, 10*time.Millisecond)
	text, _ := updates()[0]["text"].(string)
	require.Contains(t, text, "Signed in")
	require.NotContains(t, text, "@", "the in-thread rewrite carries no email")
	require.NotContains(t, text, "Bringing in", "a consent-gated replay must not promise the agent")
}

// A parked queue that is nothing but bare auth utterances is satisfied by the
// link itself: the rewritten prompt keeps the plain confirmation wording.
func TestOnUserLinkedAnchorPlainWhenOnlyBareAuthParked(t *testing.T) {
	srv, updates := captureChatUpdates(t)

	a := &Adapter{
		Secrets:      Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase:      srv.URL,
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	a.recordSignInAnchor("U001", "T1", signInAnchor{channel: "C1", ts: "111.111"})
	a.parkPendingLogin("U001", &pendingLoginReq{
		msg:          channels.InboundMessage{Subject: "U001", ThreadID: "T1", MessageID: "T1", Text: "login"},
		slackChannel: "C1",
	})

	a.OnUserLinked(t.Context(), "U001", "alice@example.com")
	require.Eventually(t, func() bool { return len(updates()) == 1 },
		10*time.Second, 10*time.Millisecond)
	text, _ := updates()[0]["text"].(string)
	require.NotContains(t, text, "Bringing in", "a satisfied bare login replays nothing")
}

// OnUserLinked is the musterlink OnLinked hook, whose contract is that it must
// not block: HandleCallback renders the "signed in, close this tab" page only
// after it returns. The anchor rewrite is a Slack round-trip (up to a 10s
// client timeout), so it must run off the callback goroutine — a slow or hung
// Slack API must not delay the user's success page.
func TestOnUserLinkedDoesNotBlockOnAnchorRewrite(t *testing.T) {
	release := make(chan struct{})
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chat.update") {
			select {
			case hit <- struct{}{}:
			default:
			}
			<-release // hold the call open, mimicking a slow Slack API
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	a := &Adapter{
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase: srv.URL,
	}
	a.recordSignInAnchor("U001", "T1", signInAnchor{channel: "C1", ts: "111.111"})

	returned := make(chan struct{})
	go func() {
		a.OnUserLinked(context.Background(), "U001", "alice@example.com")
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("OnUserLinked blocked on the anchor rewrite")
	}

	// The rewrite still fires, just asynchronously.
	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("anchor rewrite never ran")
	}
}

// A link with no recorded prompt (e.g. the park-after-link race drain) has
// nothing to rewrite and must not call the Slack API.
func TestOnUserLinkedWithoutPromptIsNoOp(t *testing.T) {
	a := &Adapter{}
	a.OnUserLinked(t.Context(), "U-never-prompted", "alice@example.com")
}

// A link path that cannot resolve the email (the park-after-link re-check)
// still rewrites the prompt, with a generic confirmation.
func TestOnUserLinkedEmptyEmailUsesGenericText(t *testing.T) {
	srv, updates := captureChatUpdates(t)

	a := &Adapter{
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		APIBase: srv.URL,
	}
	a.recordSignInAnchor("U001", "T1", signInAnchor{channel: "C1", ts: "111.111"})
	a.OnUserLinked(t.Context(), "U001", "")

	require.Eventually(t, func() bool { return len(updates()) == 1 },
		10*time.Second, 10*time.Millisecond)
	text, _ := updates()[0]["text"].(string)
	require.Contains(t, text, "Signed in")
	require.NotContains(t, text, "Signed in as")
}

func TestTakeSignInAnchorsSkipsExpiredAndUnposted(t *testing.T) {
	a := &Adapter{signInPrompted: map[string]ttlEntry[signInAnchor]{
		"U1\x00T1": {value: signInAnchor{channel: "C1", ts: "1.1"}, expires: time.Now().Add(-time.Minute)},
		"U1\x00T2": {expires: time.Now().Add(time.Hour)}, // reserved but the post failed: no ts
		"U1\x00T3": {value: signInAnchor{channel: "C1", ts: "3.3"}, expires: time.Now().Add(time.Hour)},
		"U2\x00T1": {value: signInAnchor{channel: "C1", ts: "9.9"}, expires: time.Now().Add(time.Hour)},
	}}
	require.Equal(t, []signInAnchor{{channel: "C1", ts: "3.3", threadID: "T3"}}, a.takeSignInAnchors("U1"))
	require.Empty(t, a.takeSignInAnchors("U1"), "take clears the user's entries")
	require.Len(t, a.takeSignInAnchors("U2"), 1, "other users' entries are untouched")
}

func TestRecordSignInAnchorRePromptOverwrites(t *testing.T) {
	a := &Adapter{}
	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ts: "1.1"})
	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ts: "2.2"})
	anchors := a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.False(t, anchors[0].nudgedAt.IsZero(), "recording arms the nudge throttle")
	anchors[0].nudgedAt = time.Time{}
	require.Equal(t, []signInAnchor{{channel: "C1", ts: "2.2", threadID: "T1"}}, anchors)
}

// A click on a prompt message whose task was already resumed (the thread
// paused again on a different prompt) must be refused as superseded: its raw
// selection indices would otherwise map onto the newer prompt's choices and
// deliver answers the user never saw.
func TestHandleDecision_StaleSubmitRefusedAsSuperseded(t *testing.T) {
	const secret = "test-secret"
	srv, sink := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))

	a.accessPolicy().SetInitiator("T001", "U001")
	// The thread's CURRENT pending prompt (task-B): one question, choices x/y.
	a.storePendingTask("T001", &pendingTask{
		TaskID:    "task-B",
		AgentRef:  "worker",
		Channel:   "C001",
		ChannelID: "C001",
		Prompt: &channels.HitlPrompt{
			ToolName:  channels.AskUserToolName,
			Questions: []channels.HitlQuestion{{Question: "Proceed how?", Choices: []string{"x-fast", "y-safe"}}},
		},
	})

	// Submit clicked on the OLD form message rendered for task-A, with an
	// in-range selection that would resolve against task-B's choices.
	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": "U001"},
		"channel":   map[string]any{"id": "C001"},
		"container": map[string]any{"message_ts": "MSG-OLD-FORM"},
		"message":   map[string]any{"thread_ts": "T001"},
		"actions":   []any{map[string]any{"action_id": hitlSubmit, "value": encodeHitlValue("T001", "task-A")}},
		"state": map[string]any{"values": map[string]any{
			hitlQGroupPrefix + "_0": map[string]any{hitlGroup: map[string]any{
				"selected_option": map[string]any{"value": "0"},
			}},
		}},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	req := httptest.NewRequest(http.MethodPost, "/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, body, secret)
	rr := httptest.NewRecorder()
	a.ixHandler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	require.Eventually(t, func() bool {
		for _, text := range sink.updateTexts() {
			if strings.Contains(text, "superseded") {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "the stale prompt message is rewritten as superseded")
	require.Zero(t, gw.sendCount(), "a stale click must not resume the newer pending task")
	require.True(t, a.hasPendingTask("T001"), "the pending task stays resumable")
}

// A legacy button value (raw threadID, no task binding) still routes: the
// staleness check is skipped, not failed closed, so buttons posted by older
// gateway versions keep working across a deploy.
func TestHandleDecision_LegacyRawValueStillRoutes(t *testing.T) {
	const secret = "test-secret"
	srv, _ := newIxSlackServer(t)

	gw := &fakeGateway{}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), gw))

	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{
		TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001",
	})

	serveInteraction(t, a, secret, hitlApprove, "T001", "C001", "MSG001", "U001")

	require.Eventually(t, func() bool { return gw.sendCount() == 1 },
		10*time.Second, 10*time.Millisecond, "the legacy approve click resumes the task")
	require.Equal(t, "task-abc", gw.lastCompletion().TaskID)
}
