package slack_test

import (
	"errors"
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
	require.Eventually(t, func() bool { return gw1.resolveCount() == 1 }, flowWait, 50*time.Millisecond)
	require.Equal(t, "issue-agent", resolved1()[0].AgentRef)
	sendAccessInteraction(t, srv1, "U1", accessAllowAction, "900.1", "U2", api.URL+"/response")

	gw2, resolved2 := capturingGateway()
	gw2.records = shared
	_, srv2 := newEventsAdapter(t, gw2, api.URL, channelMode, withSelection(&fakeRoster{}, cards()),
		func(a *slackadapter.Adapter) { a.DefaultAgent = "sre-agent" })

	sendEvent(t, srv2, mention("U2", "and now?", "900.3", "900.1"))
	require.Eventually(t, func() bool { return gw2.resolveCount() == 1 }, flowWait, 50*time.Millisecond)
	require.Equal(t, "issue-agent", resolved2()[0].AgentRef, "the restarted gateway routes to the recorded agent")
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), "waiting for the thread owner",
		"the grant survived: no consent prompt")
	require.Empty(t, fake.pathCalls("conversations.replies"), "no Slack history read")
}

// When the routing store is down the access policy cannot record the
// initiator. The turn must stop with a notice to its author — not park the
// message and ask that same author to approve themself.
func TestThreadRecord_StoreOutageStopsTheTurn(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	gw.recordsErr = errors.New("valkey: connection refused")
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "check the cluster", "910.1", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "thread memory")
	}, flowWait, 50*time.Millisecond, "the author gets the store-unavailable notice")
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "no turn runs without a thread record")
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), "allowed to instruct",
		"no consent prompt is posted about the author to the author")
}
