package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// A conversation opened with /agent INSIDE an existing thread (root by someone
// else), a grant to a colleague, then a "restart": a second adapter over the
// same records. The colleague's reply runs on the same agent with no consent
// prompt, and Slack history is never read.
func TestThreadRecord_AgentInitiatorAndGrantSurviveRestart(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()
	cards := func() *fakeCards {
		return &fakeCards{known: map[string]string{"sre-agent": "SRE Agent", "issue-agent": "Issue Agent"}}
	}
	gw1, resolved1 := capturingGateway()
	gw1.records = shared
	_, srv1 := newEventsAdapter(t, gw1, api.URL, channelMode, withSelection(&fakeRoster{}, cards()),
		func(a *slackadapter.Adapter) { a.DefaultAgent = "sre-agent" })

	// A reply inside an existing thread: the /agent prefix still opens the
	// conversation, because nothing is recorded for the thread yet.
	sendEvent(t, srv1, mention("U1", "/agent issue-agent what happened?", "900.2", "900.1"))
	require.Eventually(t, func() bool { return gw1.resolveCount() == 1 }, 2*time.Second, 50*time.Millisecond)
	require.Equal(t, "issue-agent", resolved1()[0].AgentRef)
	sendAccessInteraction(t, srv1, "U1", accessAllowAction, "900.1", "U2", api.URL+"/response")

	gw2, resolved2 := capturingGateway()
	gw2.records = shared
	_, srv2 := newEventsAdapter(t, gw2, api.URL, channelMode, withSelection(&fakeRoster{}, cards()),
		func(a *slackadapter.Adapter) { a.DefaultAgent = "sre-agent" })

	sendEvent(t, srv2, mention("U2", "and now?", "900.3", "900.1"))
	require.Eventually(t, func() bool { return gw2.resolveCount() == 1 }, 2*time.Second, 50*time.Millisecond)
	require.Equal(t, "issue-agent", resolved2()[0].AgentRef, "the restarted gateway routes to the recorded agent")
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), "waiting for the thread owner",
		"the grant survived: no consent prompt")
	require.Empty(t, fake.pathCalls("conversations.replies"), "no Slack history read")
}

// A thread whose record expired after 30 idle days but whose instance binding
// (never expires) names an agent: the reply continues on that agent, not the
// default.
func TestThreadRecord_ExpiredRecordContinuesOnTheBoundAgent(t *testing.T) {
	shared := slackadapter.NewMemoryRecorder()
	shared.SetBinding("slack", "C1", "900.1", "issue-agent")
	fake := newFakeSlackAPI()
	gw, resolved := capturingGateway()
	gw.records = shared
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode,
		withSelection(&fakeRoster{}, &fakeCards{known: map[string]string{"sre-agent": "SRE Agent", "issue-agent": "Issue Agent"}}),
		func(a *slackadapter.Adapter) { a.DefaultAgent = "sre-agent" })

	sendEvent(t, srv, mention("U7", "still there?", "900.9", "900.1"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 }, 2*time.Second, 50*time.Millisecond)
	require.Equal(t, "issue-agent", resolved()[0].AgentRef)
}

// The launch announcement posts on the first turn inside an existing thread,
// not on the second.
func TestLaunchAnnouncement_PostsOnFirstTurnInsideExistingThread(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode,
		withSelection(&fakeRoster{}, &fakeCards{known: map[string]string{"sre-agent": "SRE Agent"}}),
		func(a *slackadapter.Adapter) { a.DefaultAgent = "sre-agent" })

	sendEvent(t, srv, mention("U1", "help", "900.2", "900.1"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 }, 2*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Bringing in")
	}, 2*time.Second, 50*time.Millisecond)

	sendEvent(t, srv, mention("U1", "more", "900.3", "900.1"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 }, 2*time.Second, 50*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, strings.Count(allText(fake.pathCalls("chat.postMessage")), "Bringing in"))
}
