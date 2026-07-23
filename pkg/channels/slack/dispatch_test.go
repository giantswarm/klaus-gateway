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

// A message arriving while another turn holds the thread slot pays the
// sender's token mint before the slot check, so a transient mint failure on a
// busy thread surfaces as the token error (ephemeral to the sender), not as
// the busy notice a signed-in sender would get.
func TestDispatch_BusyThread_TransientTokenErrorSurfaces(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.Mode = ModeEvents
	a.DefaultAgent = "agent"
	a.OBO = transientOBO{}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	require.True(t, a.acquireThread("T1"))
	t.Cleanup(func() { a.releaseThread("T1") })

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "C1", ThreadID: "T1", MessageID: "M2", Subject: "U1", Text: "follow-up"}
	err := a.dispatch(t.Context(), msg, "C1")
	require.NoError(t, err, "the turn aborts on the token error; it must not surface as thread-busy")

	require.Equal(t, int32(0), srv.posts.Load(), "no busy notice for a turn that never reached the slot")
	require.Equal(t, int32(1), srv.ephemerals.Load())
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Equal(t, tokenErrorNotice, srv.ephemeralTexts[0])
}
