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
		a.Roster = roster
		a.AgentCards = cards
	}
}

// captureResolves records every message reaching the gateway, keyed for
// assertions on which agent ref each turn carried.
func capturingGateway() (*stubGateway, func() []channels.InboundMessage) {
	var mu sync.Mutex
	var resolved []channels.InboundMessage
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}},
		onResolve: func(msg channels.InboundMessage) {
			mu.Lock()
			resolved = append(resolved, msg)
			mu.Unlock()
		},
	}
	return gw, func() []channels.InboundMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]channels.InboundMessage(nil), resolved...)
	}
}

// A prefixed channel mention binds the new conversation to the named agent and
// dispatches the remainder of the message as its first turn; an unprefixed
// reply in that conversation inherits the same agent without re-prefixing.
func TestAgentSelection_PrefixBindsConversationAndRepliesInherit(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/agent sre-agent why are pods crashlooping?", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "prefixed mention dispatches")

	sendEvent(t, srv, mention("U1", "and the nodes?", "200.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "unprefixed reply dispatches")

	msgs := resolved()
	require.Equal(t, "sre-agent", msgs[0].AgentRef, "the conversation binds to the named agent")
	require.Equal(t, "why are pods crashlooping?", msgs[0].Text, "the prefix is stripped; the question is the first turn")
	require.Equal(t, "sre-agent", msgs[1].AgentRef, "replies inherit the conversation's agent")
	require.Equal(t, "and the nodes?", msgs[1].Text)
}

// The same selection works identically in a DM: the prefix on the first
// message of a new conversation binds it, and replies inherit.
func TestAgentSelection_DMPrefixBindsAndRepliesInherit(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, dmEvent("U1", "/agent sre-agent hello there", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "prefixed DM dispatches")

	sendEvent(t, srv, dmThreadEvent("U1", "follow up", "200.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "DM reply dispatches")

	msgs := resolved()
	require.Equal(t, "sre-agent", msgs[0].AgentRef)
	require.Equal(t, "hello there", msgs[0].Text)
	require.Equal(t, "sre-agent", msgs[1].AgentRef, "DM replies inherit the conversation's agent")
}

// An unknown agent fails loudly: nothing is dispatched, and the reply names
// the problem and includes the current roster. It never substitutes.
func TestAgentSelection_UnknownAgentFailsLoudlyWithRoster(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{}}
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Description: "Investigates infra issues"},
	}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, cards))

	sendEvent(t, srv, mention("U1", "/agent no-such-agent do things", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "I don't know an agent named `no-such-agent`")
	}, 2*time.Second, 50*time.Millisecond, "the failure is loud")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "`sre-agent` — Investigates infra issues",
		"the failure reply includes the current roster")
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "nothing is dispatched for an unknown agent")
}

// "/agent <name>" with no question selects nothing: the hint says so, and the
// user's next unprefixed message goes to the DEFAULT agent (no binding was
// created).
func TestAgentSelection_NameOnlySelectsNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/agent sre-agent", "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")),
			"Nothing was selected — include your question in the same message: `/agent sre-agent <question>`")
	}, 2*time.Second, 50*time.Millisecond, "the hint says explicitly that nothing was selected")
	require.Zero(t, gw.resolveCount(), "a name-only /agent starts no conversation")

	// The next message (a fresh mention) goes to the default agent, not the
	// one just named.
	sendEvent(t, srv, mention("U1", "so what now?", "200.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the follow-up dispatches")
	require.Equal(t, "test-agent", resolved()[0].AgentRef, "no binding was created; the default agent applies")
}

// A bare /agent lists the roster: display names from the AgentCards, technical
// names, and descriptions.
func TestAgentSelection_BareAgentListsRoster(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent", "k8s-agent": ""}}
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Description: "Investigates infra issues"},
		{Name: "k8s-agent", Description: "Kubernetes specialist"},
	}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, cards))

	sendEvent(t, srv, mention("U1", "/agent", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Available agents")
	}, 2*time.Second, 50*time.Millisecond, "the roster listing posts")
	listing := allText(fake.pathCalls("chat.postMessage"))
	require.Contains(t, listing, "*SRE Agent* (`sre-agent`) — Investigates infra issues",
		"display name, technical name, and description")
	require.Contains(t, listing, "`k8s-agent` — Kubernetes specialist",
		"an agent without a card display name lists by technical name")
	require.Zero(t, gw.resolveCount(), "listing the roster dispatches nothing")

	// A second listing within the cache window is served without another
	// controller call.
	sendEvent(t, srv, mention("U1", "/agent", "200.000", ""))
	require.Eventually(t, func() bool {
		return strings.Count(allText(fake.pathCalls("chat.postMessage")), "Available agents") == 2
	}, 2*time.Second, 50*time.Millisecond)
	require.Equal(t, 1, roster.listCalls(), "the roster is briefly cached")
}

// A failing roster fetch degrades to an honest error, not an empty listing.
func TestAgentSelection_RosterFetchFailure(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{err: errors.New("kagent agents: unexpected status 502")}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, mention("U1", "/agent", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "can't list the available agents")
	}, 2*time.Second, 50*time.Millisecond)
}

// An /agent prefix inside an existing conversation is politely refused: the
// conversation is already bound, and its agent does not change.
func TestAgentSelection_RefusedInsideExistingConversation(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent", "k8s-agent": "K8s Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/agent sre-agent start here", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)

	// Mid-conversation switch attempt: refused, nothing dispatched for it.
	sendEvent(t, srv, mention("U1", "/agent k8s-agent take over", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "already has its agent")
	}, 2*time.Second, 50*time.Millisecond, "the switch is refused with an explanation")
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount(), "the refused switch dispatches nothing")

	// The conversation stays with its original agent.
	sendEvent(t, srv, mention("U1", "continue", "300.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond)
	msgs := resolved()
	require.Equal(t, "sre-agent", msgs[1].AgentRef, "the binding is unchanged after a refused switch")
}

// After a restart (no in-memory binding) a reply still inherits its
// conversation's agent: the binding is re-derived from the conversation's root
// message, where the prefix is visible.
func TestAgentSelection_ReplyInheritsBindingFromRootAfterRestart(t *testing.T) {
	fake := newFakeSlackAPI()
	// The fresh process has never seen thread 100.000; the root text carries
	// the original prefix (with the mention token Slack includes).
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[{"user":"U1","text":"<@UBOT> /agent sre-agent original question","ts":"100.000"}]}`)
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, dmThreadEvent("U1", "are you still there?", "300.000", "100.000"))

	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the reply dispatches")
	require.Equal(t, "sre-agent", resolved()[0].AgentRef,
		"the binding is recovered from the conversation root")
}

// Two users' prefixed conversations in one channel are independent: each new
// conversation binds to its own agent and neither redirects the other.
func TestAgentSelection_TwoUsersIndependentConversations(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent", "k8s-agent": "K8s Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/agent sre-agent pods are crashing", "100.000", ""))
	sendEvent(t, srv, mention("U2", "/agent k8s-agent explain operators", "200.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond)

	byThread := map[string]string{}
	for _, msg := range resolved() {
		byThread[msg.ThreadID] = msg.AgentRef
	}
	require.Equal(t, map[string]string{
		"100.000": "sre-agent",
		"200.000": "k8s-agent",
	}, byThread, "each conversation binds to its own agent")
}

// A bare agent name resolves in the default agent's namespace; an explicit
// namespace/name is used as typed.
func TestAgentSelection_NamespaceCompletion(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{
		"kagent/sre-agent": "SRE Agent",
		"other/lab-agent":  "Lab Agent",
	}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards),
		func(a *slackadapter.Adapter) { a.DefaultAgent = "kagent/swarmgeist" })

	sendEvent(t, srv, mention("U1", "/agent sre-agent check gazelle", "100.000", ""))
	sendEvent(t, srv, mention("U2", "/agent other/lab-agent check the lab", "200.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond)

	byThread := map[string]string{}
	for _, msg := range resolved() {
		byThread[msg.ThreadID] = msg.AgentRef
	}
	require.Equal(t, map[string]string{
		"100.000": "kagent/sre-agent",
		"200.000": "other/lab-agent",
	}, byThread)
}

// Case-insensitive matching on the technical name.
func TestAgentSelection_CaseInsensitiveName(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/agent SRE-Agent why?", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)
	require.Equal(t, "sre-agent", resolved()[0].AgentRef)
}

// /help mentions the /agent command when selection is available, and not when
// it is not (no card client to validate names against).
func TestAgentSelection_HelpMentionsAgentCommand(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, &fakeCards{}))

	sendEvent(t, srv, dmEvent("U1", "/help", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "/agent <name> <question>")
	}, 2*time.Second, 50*time.Millisecond, "/help lists the /agent command when selection is available")

	fakeOff := newFakeSlackAPI()
	_, srvOff := newEventsAdapter(t, &stubGateway{}, fakeOff.server(t).URL)
	sendEvent(t, srvOff, dmEvent("U1", "/help", "100.000"))
	fakeOff.waitForPath(t, "chat.postMessage", 1)
	require.NotContains(t, allText(fakeOff.pathCalls("chat.postMessage")), "/agent",
		"/help omits /agent when selection is unavailable")
}

// Without an agent-card client the /agent command reports that selection is
// unavailable instead of guessing.
func TestAgentSelection_UnavailableWithoutCards(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "/agent sre-agent hello", "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "selection isn't available")
	}, 2*time.Second, 50*time.Millisecond)
	require.Zero(t, gw.resolveCount())
}
