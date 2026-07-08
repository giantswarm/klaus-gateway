package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// toolActivityDeltas is a turn that invokes a tool and then answers.
func toolActivityDeltas() []channels.OutboundDelta {
	return []channels.OutboundDelta{
		{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Name: "list_pods", Kind: channels.ToolCall,
			Args: map[string]any{"namespace": "kube-system"},
		}},
		{Content: "Found 3 pods."},
		{Done: true},
	}
}

func TestDetails_DefaultOn_RendersToolActivity(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: toolActivityDeltas()}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"list pods","channel":"D1","ts":"111.000"}}`)

	require.Eventually(t, func() bool {
		text := allText(fake.pathCalls("chat.postMessage"))
		return strings.Contains(text, "list_pods") && strings.Contains(text, "Found 3 pods.")
	}, 2*time.Second, 20*time.Millisecond, "default-on details should render the tool call and the answer")
}

func TestDetails_Off_SuppressesToolActivity(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: toolActivityDeltas()}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// Quiet the thread first (same thread_ts as the turn below).
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"/details off","channel":"D1","ts":"100.000","thread_ts":"100.000"}}`)
	fake.waitForPath(t, "chat.postMessage", 1)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"list pods","channel":"D1","ts":"101.000","thread_ts":"100.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Found 3 pods.")
	}, 2*time.Second, 20*time.Millisecond, "the answer should still be posted")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "list_pods",
		"details off must not render tool activity")
}

func TestResume_PostsStartingFreshWhenSessionGone(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return false, true }}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A reply into a thread this process never started (thread_ts != ts).
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi again","channel":"D1","ts":"201.000","thread_ts":"100.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "starting fresh")
	}, 2*time.Second, 20*time.Millisecond, "a gone session should trigger the starting-fresh notice")
	require.Equal(t, 1, gw.resumeCount())
}

func TestResume_SilentWhenSessionPresent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return true, true }}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi again","channel":"D1","ts":"201.000","thread_ts":"100.000"}}`)

	// Wait for the turn to complete (empty-output note), then assert no notice.
	fake.waitForPath(t, "chat.postMessage", 1)
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "starting fresh")
	require.Equal(t, 1, gw.resumeCount())
}

func TestResume_SkippedForRootMessage(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return false, true }}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A fresh root message (no thread_ts) starts a new session: no resume check.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"brand new","channel":"D1","ts":"300.000"}}`)

	fake.waitForPath(t, "chat.postMessage", 1)
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "starting fresh")
	require.Equal(t, 0, gw.resumeCount(), "root messages must not trigger the resume check")
}
