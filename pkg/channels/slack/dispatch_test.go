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
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	a.storePendingTask("T1", &pendingTask{TaskID: "task-1", AgentRef: "agent", Channel: "D1", ChannelID: "D1"})

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T1", Subject: "U1", Text: "my answer"}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))

	require.NotNil(t, a.takePendingTask("T1"), "transient OBO error must not consume the paused task")
}
