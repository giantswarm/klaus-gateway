package slack_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// fakeCards is an AgentCardResolver with the card-info extension: known refs
// resolve to a display name, unknown refs fail the info lookup (the validation
// path), mirroring pkg/a2a.AgentCardClient at the seam the adapter uses.
type fakeCards struct {
	mu    sync.Mutex
	known map[string]string // agentRef -> display name
}

func (f *fakeCards) CardIdentity(_ context.Context, ref string) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.known[ref], ""
}

func (f *fakeCards) CardInfo(_ context.Context, ref string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.known[ref]
	if !ok {
		return "", "", errors.New("agent card: unexpected status 404")
	}
	return name, "", nil
}

// fakeRoster fakes the kagent list-agents boundary.
type fakeRoster struct {
	mu     sync.Mutex
	agents []pkga2a.AgentInfo
	err    error
	calls  int
}

func (f *fakeRoster) ListAgents(context.Context) ([]pkga2a.AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.agents, f.err
}

func (f *fakeRoster) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// withSelection wires the selection collaborators into the harness adapter.
func withSelection(roster *fakeRoster, cards *fakeCards) func(*slackadapter.Adapter) {
	return func(a *slackadapter.Adapter) {
		// Qualified default: refs resolve namespace-qualified, prod-shaped.
		// Individual tests override with a bare default to exercise the
		// namespace-in-URL deployment shape.
		a.DefaultAgent = "kagent/swarmgeist"
		a.Roster = roster
		a.AgentCards = cards
	}
}

// capturingGateway records every message dispatched to the gateway, keyed for
// assertions on which agent ref each turn carried.
func capturingGateway() (*stubGateway, func() []channels.InboundMessage) {
	var mu sync.Mutex
	var dispatched []channels.InboundMessage
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}},
		onDispatch: func(msg channels.InboundMessage) {
			mu.Lock()
			dispatched = append(dispatched, msg)
			mu.Unlock()
		},
	}
	return gw, func() []channels.InboundMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]channels.InboundMessage(nil), dispatched...)
	}
}

// A bare (namespace-less) default agent is a supported shape: the namespace
// lives in the configured A2A base URL, and resolution keeps refs bare to
// match. The adapter starts with or without a roster.
func TestStart_AllowsBareDefaultAgentWithRoster(t *testing.T) {
	newAdapter := func(defaultAgent string) *slackadapter.Adapter {
		return &slackadapter.Adapter{
			Mode:         slackadapter.ModeEvents,
			Secrets:      slackadapter.Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
			DefaultAgent: defaultAgent,
			Roster:       &fakeRoster{},
		}
	}

	require.NoError(t, newAdapter("swarmgeist").Start(t.Context(), &stubGateway{}))
	require.NoError(t, newAdapter("kagent/swarmgeist").Start(t.Context(), &stubGateway{}))
}

// The adapter refuses to start without a gateway to dispatch into, and without
// a default agent: a turn has nothing to run on in either case, so the wiring
// error surfaces at boot instead of on the first mention.
func TestStart_RefusesNilGatewayAndMissingDefaultAgent(t *testing.T) {
	adapter := func() *slackadapter.Adapter {
		return &slackadapter.Adapter{
			Mode:         slackadapter.ModeEvents,
			Secrets:      slackadapter.Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
			DefaultAgent: "swarmgeist",
		}
	}

	require.ErrorContains(t, adapter().Start(t.Context(), nil), "nil gateway")

	noAgent := adapter()
	noAgent.DefaultAgent = ""
	require.ErrorContains(t, noAgent.Start(t.Context(), &stubGateway{}), "DefaultAgent")
}

// The /agent read-only forms are deliberately ungated, like /help: a
// non-initiator in someone else's thread can list the roster (global
// information, not thread state) and gets the switch refusal — neither
// changes any state or dispatches anything, and the thread's binding and
// ownership are untouched.
func TestAgentSelection_OnlookerReadOnlyFormsUngated(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent", "kagent/k8s-agent": "K8s Agent"}}
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{{Name: "sre-agent", Namespace: "kagent"}}}
	gw, dispatched := capturingGateway()
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, cards))

	// U1 starts and owns the conversation.
	sendEvent(t, srv, mention("U1", "start here", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond)
	waitThreadIdle(t, a, "100.000")
	bound := dispatched()[0].AgentRef

	// An onlooker lists the roster in the thread: allowed, dispatches nothing.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","user":"U2","text":"agents","channel":"C1","ts":"200.000","thread_ts":"100.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Available agents")
	}, flowWait, 50*time.Millisecond, "the roster listing is ungated, like the help reply")
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, gw.dispatchCount(), "the listing dispatches nothing")

	// The initiator's follow-up still resolves the original binding.
	sendEvent(t, srv, mention("U1", "continue", "400.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 50*time.Millisecond)
	require.Equal(t, bound, dispatched()[1].AgentRef, "the binding is untouched by an onlooker's listing")
}

// A bare /agent lists the roster: display names (from the CR annotation) and
// descriptions only — no technical names, no namespaces.
func TestAgentSelection_BareAgentListsRoster(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent", Description: "Investigates infra issues"},
		{Name: "k8s-agent", Namespace: "kagent", Description: "Kubernetes specialist"},
	}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, mention("U1", "agents", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Available agents")
	}, flowWait, 50*time.Millisecond, "the roster listing posts")
	listing := allText(fake.pathCalls("chat.postMessage"))
	require.NotContains(t, listing, "/agent",
		"the listing advertises no command the gateway stopped serving")
	require.Contains(t, listing, "*SRE Agent* — Investigates infra issues",
		"display name and description")
	require.Contains(t, listing, "*k8s-agent* — Kubernetes specialist",
		"an agent without a display-name annotation lists by technical name")
	require.NotContains(t, listing, "kagent", "namespaces never appear in the roster")
	require.Zero(t, gw.dispatchCount(), "listing the roster dispatches nothing")

	// A second listing within the cache window is served without another
	// controller call.
	sendEvent(t, srv, mention("U1", "agents", "200.000", ""))
	require.Eventually(t, func() bool {
		return strings.Count(allText(fake.pathCalls("chat.postMessage")), "Available agents") == 2
	}, flowWait, 50*time.Millisecond)
	require.Equal(t, 1, roster.listCalls(), "the roster is briefly cached")
}

// A failing roster fetch degrades to an honest error, not an empty listing.
func TestAgentSelection_RosterFetchFailure(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{err: errors.New("kagent agents: unexpected status 502")}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, mention("U1", "agents", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "agents cannot be listed")
	}, flowWait, 50*time.Millisecond)
}

// The discovery flow in a fresh pane chat: "agents" lists the roster, and a
// Select click in the SAME chat still opens the picker — the consumed listing
// never started a conversation, so the click must not be refused as a thread
// that already talks to an agent.
func TestAgentSelection_PaneRosterThenSelectOpensThePicker(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, dmThreadEvent("U1", "agents", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Available agents")
	}, flowWait, 50*time.Millisecond, "the roster listing posts")

	sendRosterSelect(t, srv, "D1", "U1", "100.000", "kagent/sre-agent", api.URL+"/response_url")
	view := openedView(t, fake)
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), "already talks to",
		"a consumed listing must not make the click a refused switch")
	agentSelect := view["blocks"].([]any)[1].(map[string]any)["element"].(map[string]any)
	require.Equal(t, "kagent/sre-agent", agentSelect["initial_option"].(map[string]any)["value"],
		"the clicked agent is preselected")
	require.Zero(t, gw.dispatchCount(), "opening the picker dispatches nothing")
}

// A new pane chat is not greeted with the "starts fresh" resume notice: its
// first message arrives as a thread reply (the chat's Slack-created anchor is
// its thread_ts) but it OPENS the conversation — there is no earlier session
// to resume. A genuine reply into a thread the process does not know still
// announces when the session is conclusively gone.
func TestPane_NewChatNotGreetedWithStartingFresh(t *testing.T) {
	sessionGone := func(channels.InboundMessage) (bool, bool) { return false, true }

	t.Run("first pane message is silent", func(t *testing.T) {
		fake := newFakeSlackAPI()
		gw := &stubGateway{
			deltas:             []channels.OutboundDelta{{Content: "hi"}, {Done: true}},
			onSessionResumable: sessionGone,
		}
		_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

		sendEvent(t, srv, dmThreadEvent("U1", "hello", "200.000", "100.000"))
		require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
			flowWait, 50*time.Millisecond)
		time.Sleep(150 * time.Millisecond)
		require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "starts fresh",
			"a conversation-opening pane message must not be greeted with the resume notice")
	})

	t.Run("genuine reply still announces", func(t *testing.T) {
		fake := newFakeSlackAPI()
		gw := &stubGateway{
			deltas:             []channels.OutboundDelta{{Content: "hi"}, {Done: true}},
			onSessionResumable: sessionGone,
		}
		// The conversation exists — the thread's record names an agent — while
		// nobody has instructed in it yet: a resume, not an opener.
		require.NoError(t, gw.rec().UpdateThreadRecord(context.Background(), "slack", "D1", "100.000", func(e *store.Entry, _ bool) bool {
			e.AgentRef = "test-agent"
			return true
		}))
		_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

		sendEvent(t, srv, dmThreadEvent("U1", "are you still there?", "300.000", "100.000"))
		require.Eventually(t, func() bool {
			return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "starts fresh")
		}, flowWait, 50*time.Millisecond, "a real resume with a gone session still gets the notice")
	})
}

// The help reply lists the agents command when selection is available, and
// not when it is not (no card client to validate names against).
func TestAgentSelection_HelpListsTheAgentsCommand(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, &fakeCards{}))

	sendEvent(t, srv, dmEvent("U1", "help", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allBlockText(fake.pathCalls("chat.postMessage")), "List the agents")
	}, flowWait, 50*time.Millisecond, "the help reply lists the agents command when selection is available")
	require.NotContains(t, allBlockText(fake.pathCalls("chat.postMessage")), "/agent",
		"no command carries a slash any more")

	fakeOff := newFakeSlackAPI()
	_, srvOff := newEventsAdapter(t, &stubGateway{}, fakeOff.server(t).URL)
	sendEvent(t, srvOff, dmEvent("U1", "help", "100.000"))
	fakeOff.waitForPath(t, "chat.postMessage", 1)
	require.NotContains(t, allBlockText(fakeOff.pathCalls("chat.postMessage")), "List the agents",
		"the help reply omits the agents command when selection is unavailable")
}

// Without an agent-card client the gateway lists no agents, so the word is not
// a command: it reaches the agent like any other message.
func TestAgentSelection_WordReachesTheAgentWithoutCards(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "agents", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond,
		"without agent selection the word belongs to the agent")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Available agents")
}

// The listing reads the catalogue as the caller: the roster carries the
// caller's linked token, so a cold roster cache on a fresh pod refuses nothing
// a linked user may pick.
func TestAgentSelection_ListingReadsTheRosterAsCaller(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &tokenRoster{agents: []pkga2a.AgentInfo{{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"}}}
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, func(a *slackadapter.Adapter) {
		a.DefaultAgent = "kagent/swarmgeist"
		a.Roster = roster
		a.AgentCards = cards
		a.OBO = oneUserOBO{user: "U1", token: "tok-u1"}
	})

	sendEvent(t, srv, mention("U1", "agents", "100.000", ""))
	require.Eventually(t, func() bool { return len(roster.seen()) == 1 },
		flowWait, 50*time.Millisecond, "the bare listing reads the roster")
	require.Equal(t, []string{"tok-u1"}, roster.seen(), "the listing runs as the caller")
	require.Zero(t, gw.dispatchCount(), "the listing dispatches nothing")
}

// A bare /agent from a caller the gateway cannot identify asks them to sign
// in: the controller serves the roster to a human identity, and "try again in
// a moment" would send an unlinked person in circles.
func TestAgentSelection_UnlinkedListingAsksToSignIn(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{err: pkga2a.ErrNoIdentity}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, mention("U1", "agents", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "listed with your permissions, so sign in first")
	}, flowWait, 50*time.Millisecond, "the unlinked caller is told to sign in")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "agents cannot be listed")
}

// notRunnableCards is a card client whose template exists but cannot start a
// conversation: no Harness admits it, the reason the a2a layer reports.
type notRunnableCards struct{}

func (notRunnableCards) CardIdentity(context.Context, string) (string, string) { return "", "" }
func (notRunnableCards) CardInfo(context.Context, string) (string, string, error) {
	return "", "", &pkga2a.AgentUnavailableError{
		Ref: "kagent/sre-agent",
		// Free text from a condition message: lines and a trailing period, which
		// the notice must fold into one sentence.
		Reason: "no Harness admits this AgentTemplate\n  (it carries no admission label\n  a platform Harness selects).",
	}
}
