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

	a.accessPolicy().SetInitiator("T001", "U_OTHER") // clicker owns the thread; isolate the token gate
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
	}, 2*time.Second, 10*time.Millisecond, "a linked clicker must resume the paused task")

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

	// U001 owns the thread; a pending tool call awaits a decision.
	a.accessPolicy().SetInitiator("T001", "U001")
	a.storePendingTask("T001", &pendingTask{TaskID: "task-abc", AgentRef: "worker", Channel: "C001", ChannelID: "C001"})

	// An onlooker (U999) clicks Approve.
	serveInteraction(t, a, secret, "hitl_approve", "T001", "C001", "MSG001", "U999")

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

	require.Eventually(t, func() bool { return gw.sendCount() >= 1 }, 2*time.Second, 10*time.Millisecond)

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
	}, 2*time.Second, 10*time.Millisecond, "incomplete form must nudge the user")
	require.Zero(t, gw.sendCount(), "incomplete form must not resume the task")
	require.True(t, a.hasPendingTask("T001"), "incomplete form must leave the task pending")
	posts, updates, _ := sink.counts()
	require.Zero(t, posts, "incomplete form must not mint a token or prompt sign-in")
	require.Zero(t, updates, "incomplete form must not overwrite the form message")
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

	require.Eventually(t, func() bool { return gw.sendCount() >= 1 }, 2*time.Second, 10*time.Millisecond)

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
	}, 2*time.Second, 10*time.Millisecond, "empty submit must nudge the user")
	require.Zero(t, gw.sendCount(), "empty submit must not resume the task")
	require.True(t, a.hasPendingTask("T001"), "empty submit must leave the task pending")
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

func TestSignInClickThenNotifyLinkedReplacesPrompt(t *testing.T) {
	const secret = "test-secret"

	var (
		mu    sync.Mutex
		calls []map[string]any
	)
	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		calls = append(calls, v)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(respSrv.Close)

	a := &Adapter{
		Secrets:      Secrets{BotToken: "test-bot-token", SigningSecret: secret}, //nolint:gosec
		DefaultAgent: "worker",
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))

	inner := map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": "U001"},
		"response_url": respSrv.URL,
		"actions":      []any{map[string]any{"action_id": oboSignIn}},
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

	// The click is processed asynchronously; wait for the response_url capture.
	require.Eventually(t, func() bool {
		a.signInMu.Lock()
		defer a.signInMu.Unlock()
		return len(a.signInPrompts) > 0
	}, 2*time.Second, 10*time.Millisecond)

	a.notifyLinked(t.Context(), "U001", "alice@example.com")

	mu.Lock()
	require.Len(t, calls, 1)
	require.Equal(t, true, calls[0]["replace_original"])
	require.Contains(t, calls[0]["text"], "alice@example.com")
	mu.Unlock()

	// The stored response_url is single-use: a second notification is a no-op.
	a.notifyLinked(t.Context(), "U001", "alice@example.com")
	mu.Lock()
	require.Len(t, calls, 1)
	mu.Unlock()
}

// OnUserLinked is the musterlink OnLinked hook, whose contract is that it must
// not block: HandleCallback renders the "signed in, close this tab" page only
// after it returns. The confirmation it posts is a Slack round-trip (up to a 10s
// client timeout), so it must run off the callback goroutine — a slow or hung
// hooks.slack.com must not delay the user's success page.
func TestOnUserLinkedDoesNotBlockOnConfirmationPOST(t *testing.T) {
	release := make(chan struct{})
	hit := make(chan struct{}, 1)
	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		<-release // hold the POST open, mimicking a slow hooks.slack.com
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(release)
		respSrv.Close()
	})

	a := &Adapter{}
	a.storeSignInPrompt("U001", respSrv.URL)

	returned := make(chan struct{})
	go func() {
		a.OnUserLinked(context.Background(), "U001", "alice@example.com")
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("OnUserLinked blocked on the confirmation POST")
	}

	// The confirmation still fires, just asynchronously.
	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation POST never ran")
	}
}

func TestNotifyLinkedWithoutClickIsNoOp(t *testing.T) {
	a := &Adapter{}
	a.notifyLinked(t.Context(), "U-never-clicked", "alice@example.com")
}

// A link path that cannot resolve the email (the park-after-link re-check)
// still replaces the prompt, with a generic confirmation.
func TestNotifyLinkedEmptyEmailUsesGenericText(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []map[string]any
	)
	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		calls = append(calls, v)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(respSrv.Close)

	a := &Adapter{}
	a.storeSignInPrompt("U001", respSrv.URL)
	a.notifyLinked(t.Context(), "U001", "")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 1)
	text, _ := calls[0]["text"].(string)
	require.Contains(t, text, "Signed in")
	require.NotContains(t, text, "Signed in as")
}

func TestTakeSignInPromptExpired(t *testing.T) {
	a := &Adapter{signInPrompts: map[string]ttlEntry[string]{
		"U001": {value: "http://unused.invalid", expires: time.Now().Add(-time.Minute)},
	}}
	require.Empty(t, a.takeSignInPrompt("U001"))
}

func TestStoreSignInPromptReClickOverwrites(t *testing.T) {
	a := &Adapter{}
	a.storeSignInPrompt("U001", "http://first.invalid")
	a.storeSignInPrompt("U001", "http://second.invalid")
	require.Equal(t, "http://second.invalid", a.takeSignInPrompt("U001"))
	require.Empty(t, a.takeSignInPrompt("U001"))
}

func TestStoreSignInPromptSweepsExpiredEntries(t *testing.T) {
	a := &Adapter{}
	a.storeSignInPrompt("U-old", "http://old.invalid")
	a.signInMu.Lock()
	a.signInPrompts["U-old"] = ttlEntry[string]{value: "http://old.invalid", expires: time.Now().Add(-time.Minute)}
	a.signInMu.Unlock()

	a.storeSignInPrompt("U-new", "http://new.invalid")

	a.signInMu.Lock()
	_, oldKept := a.signInPrompts["U-old"]
	a.signInMu.Unlock()
	require.False(t, oldKept, "expired stored response_url must be swept")
	require.Equal(t, "http://new.invalid", a.takeSignInPrompt("U-new"))
}
