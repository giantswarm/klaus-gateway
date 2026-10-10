package slack_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// runtimeRoster is a roster that answers the collaborator policy the way
// pkg/a2a.Client does, from the Harness runtime a test sets and changes.
type runtimeRoster struct {
	mu      sync.Mutex
	runtime string
	err     error
}

func (r *runtimeRoster) set(runtime string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtime, r.err = runtime, err
}

func (r *runtimeRoster) ListAgents(context.Context) ([]pkga2a.AgentInfo, error) {
	return nil, nil
}

func (r *runtimeRoster) RefusesCollaborators(context.Context, string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runtime == pkga2a.HarnessRuntimeClaude, r.err
}

func withRuntimeRoster(r *runtimeRoster) func(*slackadapter.Adapter) {
	return func(a *slackadapter.Adapter) { a.Roster = r }
}

const startYourOwnThread = "Start your own thread to work with it."

// A newcomer in the thread of an agent on a claude Harness is refused at once
// and pointed at a thread of their own: the initiator is not asked to let them
// in, and nothing reaches the agent. The initiator's own turns run.
func TestCollaborators_NewcomerOnCodingAgentIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	roster := &runtimeRoster{runtime: pkga2a.HarnessRuntimeClaude}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, withRuntimeRoster(roster))

	sendEvent(t, srv, mention("U001", "fix the flaky test", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")

	sendEvent(t, srv, mention("U999", "also bump the chart", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	ephemeral := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, ephemeral, startYourOwnThread)
	require.NotContains(t, ephemeral, "wants to join this thread", "the initiator is not asked to let them in")

	sendEvent(t, srv, mention("U001", "and the docs", "300.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 50*time.Millisecond, "the initiator keeps instructing the agent")
	require.Len(t, fake.pathCalls("chat.postEphemeral"), 1, "the refusal goes to the newcomer once")
}

// A collaborator the thread admitted is refused on the turn itself once the
// agent runs on a claude Harness: the turn check holds whatever the access
// gate let in.
func TestCollaborators_GrantedCollaboratorOnCodingAgentIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	roster := &runtimeRoster{runtime: "kagent"}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, withRuntimeRoster(roster))

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)

	roster.set(pkga2a.HarnessRuntimeClaude, nil)
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	fake.waitForPath(t, "chat.postEphemeral", 3)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), startYourOwnThread)
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.dispatchCount(), "the collaborator's turn never reaches the agent")
}

// An agent that cannot be looked up is not trusted to share its Session: a
// collaborator's turn is not passed on and the sender is told why.
func TestCollaborators_LookupFailureRefusesTheCollaboratorTurn(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	roster := &runtimeRoster{runtime: "kagent"}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, withRuntimeRoster(roster))

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)

	roster.set("", errors.New("controller unreachable"))
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	fake.waitForPath(t, "chat.postEphemeral", 3)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "could not be looked up")
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.dispatchCount(), "the collaborator's turn never reaches the agent")
}

// A Declarative agent keeps its thread's collaborators: the newcomer is held
// for the initiator's consent and, once let in, runs as a collaborator on the
// initiator's shared Session.
func TestCollaborators_DeclarativeAgentKeepsCollaborators(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	var mu sync.Mutex
	var msgs []channels.InboundMessage
	gw := &stubGateway{
		deltas:     []channels.OutboundDelta{{Content: "ok", Done: true}},
		onDispatch: func(m channels.InboundMessage) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() },
	}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	roster := &runtimeRoster{runtime: "kagent"}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, withRuntimeRoster(roster),
		func(a *slackadapter.Adapter) { a.OBO = obo })

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "wants to join this thread")

	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 50*time.Millisecond, "approval replays the collaborator's message")
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), startYourOwnThread)

	mu.Lock()
	defer mu.Unlock()
	require.True(t, msgs[1].Collaborator)
	require.Equal(t, "tok-collab", msgs[1].BearerToken)
	require.Equal(t, "tok-initiator", msgs[1].OwnerToken)
}
