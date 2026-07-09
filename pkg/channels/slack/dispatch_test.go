package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// transientOBO returns a non-ErrNotLinked (transient) error for every token
// request, exercising the "human token unavailable, abort" branch.
type transientOBO struct{}

func (transientOBO) TokenFor(context.Context, string) (string, error) {
	return "", errors.New("transient token-mint failure")
}
func (transientOBO) LinkURL(string) string { return "" }
func (transientOBO) Unlink(string)         {}

// A typed reply that aborts on a transient OBO token error must not consume the
// thread's paused input-required task, or the pending A2A task would dangle
// (a later button click finds nothing to resume).
func TestDispatch_TransientOBOError_PreservesPendingTask(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(fake.Close)

	a := &Adapter{
		Mode:         ModeEvents,
		Secrets:      Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
		APIBase:      fake.URL,
		DefaultAgent: "agent",
		OBO:          transientOBO{},
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))

	a.storePendingTask("T1", &pendingTask{TaskID: "task-1", AgentRef: "agent", Channel: "D1", ChannelID: "D1"})

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T1", Subject: "U1", Text: "my answer"}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))

	require.NotNil(t, a.takePendingTask("T1"), "transient OBO error must not consume the paused task")
}

// recoveringGateway wraps fakeGateway with the pendingRecoverer capability,
// simulating a paused task that only exists in the kagent task store.
type recoveringGateway struct {
	fakeGateway
	taskID string
	prompt *channels.HitlPrompt
}

func (g *recoveringGateway) PendingHITL(_ context.Context, _ channels.InboundMessage) (string, *channels.HitlPrompt, bool) {
	if g.taskID == "" {
		return "", nil, false
	}
	return g.taskID, g.prompt, true
}

// A typed reply into a thread this process did not start (gateway restarted
// after posting a HITL prompt) must resolve the store-recovered paused task
// with a structured decision instead of starting a fresh turn.
func TestDispatch_RecoversPendingTaskAfterRestart(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(fake.Close)

	gw := &recoveringGateway{
		fakeGateway: fakeGateway{deltas: []channels.OutboundDelta{{Content: "resumed"}, {Done: true}}},
		taskID:      "task-restored",
		prompt:      &channels.HitlPrompt{ToolName: "kubectl_delete"},
	}
	a := &Adapter{
		Mode:         ModeEvents,
		Secrets:      Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
		APIBase:      fake.URL,
		DefaultAgent: "agent",
	}
	require.NoError(t, a.Start(t.Context(), gw))

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T1", MessageID: "M2", Subject: "U1", Text: "approve"}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))

	sent := gw.sentMessages()
	require.Len(t, sent, 1)
	require.Equal(t, "task-restored", sent[0].TaskID, "the recovered task id must resume the paused A2A task")
	require.NotNil(t, sent[0].Decision)
	require.Equal(t, channels.DecisionApprove, sent[0].Decision.Type)
}
