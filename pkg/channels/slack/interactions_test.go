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
}

func (g *fakeGateway) Resolve(_ context.Context, _ channels.InboundMessage) (channels.InstanceRef, error) {
	return channels.InstanceRef{Name: "i1"}, nil
}
func (g *fakeGateway) SendCompletion(_ context.Context, _ channels.InstanceRef, _ channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	ch := make(chan channels.OutboundDelta, len(g.deltas)+1)
	for _, d := range g.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
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

func TestIsActiveThread(t *testing.T) {
	a := &Adapter{}

	require.False(t, a.isActiveThread("T001"))

	// Access state makes it active.
	_ = a.getAccess("T001", "U001")
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
