package slack_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// Opening the assistant Messages tab greets the user once; reopening the pane
// does not re-greet. Suggested prompts are manifest-declared, so no
// assistant.threads.* call is made.
func TestAssistantSurface_HomeOpenedGreetsOnce(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_home_opened","user":"U1","channel":"D1","tab":"messages","event_ts":"111.000"}}`)
	fake.waitForPath(t, "chat.postMessage", 1)
	greeting := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "D1", greeting.params["channel"])
	require.Contains(t, greeting.params["text"], "Swarmgeist")

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_home_opened","user":"U1","channel":"D1","tab":"messages","event_ts":"112.000"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Len(t, fake.pathCalls("chat.postMessage"), 1, "reopening the pane must not re-greet")
}

// app_home_opened for a tab other than the assistant Messages tab is ignored.
func TestAssistantSurface_HomeOpenedOtherTabIgnored(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_home_opened","user":"U1","channel":"D1","tab":"home","event_ts":"111.000"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, fake.pathCalls("chat.postMessage"))
}

// With DMs ignored the assistant pane stays silent.
func TestAssistantSurface_HomeOpenedDMModeIgnoreSilent(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.DMMode = slackadapter.DMModeIgnore
	})

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_home_opened","user":"U1","channel":"D1","tab":"messages","event_ts":"111.000"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, fake.pathCalls("chat.postMessage"))
}

// With DMs in redirect mode, opening the assistant pane points the user to
// channels instead of greeting.
func TestAssistantSurface_HomeOpenedDMModeRedirects(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.DMMode = slackadapter.DMModeRedirect
	})

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_home_opened","user":"U1","channel":"D1","tab":"messages","event_ts":"111.000"}}`)
	fake.waitForPath(t, "chat.postMessage", 1)
	redirect := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "D1", redirect.params["channel"])
	require.Contains(t, redirect.params["text"], "channels, not direct messages")
}

// app_context_changed is consumed cleanly (with and without entities): no API
// calls, no dispatch, and the adapter keeps routing normal messages after it.
func TestAssistantSurface_ContextChangedNoOp(t *testing.T) {
	gw := &stubGateway{}
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_context_changed","context":{"entities":[{"type":"slack#/types/channel_id","value":"C42","team_id":"T1"}]},"event_ts":"111.000"}}`)
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_context_changed","context":{},"event_ts":"112.000"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "context changes must not dispatch")
	require.Empty(t, fake.pathCalls("chat.postMessage"))

	sendEvent(t, srv, dmEvent("U1", "hello", "113.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 20*time.Millisecond, "a DM after context changes still dispatches")
}
