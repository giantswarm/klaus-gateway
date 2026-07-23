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
		// Qualified default: refs resolve namespace-qualified, prod-shaped.
		// Individual tests override with a bare default to exercise the
		// namespace-in-URL deployment shape.
		a.DefaultAgent = "kagent/swarmgeist"
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

// A prefixed channel mention binds the new conversation to the named agent and
// dispatches the remainder of the message as its first turn; an unprefixed
// reply in that conversation inherits the same agent without re-prefixing.
func TestAgentSelection_PrefixBindsConversationAndRepliesInherit(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/agent sre-agent why are pods crashlooping?", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "prefixed mention dispatches")

	sendEvent(t, srv, mention("U1", "and the nodes?", "200.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "unprefixed reply dispatches")

	msgs := resolved()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef, "the conversation binds to the named agent")
	require.Equal(t, "why are pods crashlooping?", msgs[0].Text, "the prefix is stripped; the question is the first turn")
	require.Equal(t, "kagent/sre-agent", msgs[1].AgentRef, "replies inherit the conversation's agent")
	require.Equal(t, "and the nodes?", msgs[1].Text)
}

// The same selection works identically in a DM: the prefix on the first
// message of a new conversation binds it, and replies inherit.
func TestAgentSelection_DMPrefixBindsAndRepliesInherit(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, dmEvent("U1", "/agent sre-agent hello there", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "prefixed DM dispatches")

	sendEvent(t, srv, dmThreadEvent("U1", "follow up", "200.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "DM reply dispatches")

	msgs := resolved()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef)
	require.Equal(t, "hello there", msgs[0].Text)
	require.Equal(t, "kagent/sre-agent", msgs[1].AgentRef, "DM replies inherit the conversation's agent")
}

// An unknown agent fails loudly: nothing is dispatched, and the reply names
// the problem and includes the current roster. It never substitutes.
func TestAgentSelection_UnknownAgentFailsLoudlyWithRoster(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{}}
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", Description: "Investigates infra issues"},
	}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, cards))

	sendEvent(t, srv, mention("U1", "/agent no-such-agent do things", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "I don't know an agent named `no-such-agent`")
	}, 2*time.Second, 50*time.Millisecond, "the failure is loud")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "*sre-agent* — Investigates infra issues",
		"the failure reply includes the current roster")
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "nothing is dispatched for an unknown agent")
}

// Whitespace between the slash and the verb ("/ agent …") parses like the
// plain form: parseCommand tolerates it, so the splitter must too.
func TestAgentSelection_WhitespaceAfterSlash(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/ agent sre-agent do things", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)
	msgs := resolved()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef)
	require.Equal(t, "do things", msgs[0].Text)
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
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, cards))

	// U1 starts and owns the conversation.
	sendEvent(t, srv, mention("U1", "/agent sre-agent start here", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)

	// An onlooker lists the roster in the thread: allowed, dispatches nothing.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","user":"U2","text":"/agent","channel":"C1","ts":"200.000","thread_ts":"100.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Available agents")
	}, 2*time.Second, 50*time.Millisecond, "the roster listing is ungated, like /help")

	// An onlooker's switch attempt is refused, changes nothing.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","user":"U2","text":"/agent k8s-agent take over","channel":"C1","ts":"300.000","thread_ts":"100.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "already has its agent")
	}, 2*time.Second, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount(), "neither read-only form dispatches")

	// The initiator's follow-up still resolves the original binding.
	sendEvent(t, srv, mention("U1", "continue", "400.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond)
	require.Equal(t, "kagent/sre-agent", resolved()[1].AgentRef, "the binding is untouched by onlooker commands")
}

// "/agent <name>" with no question selects nothing: the hint says so, and the
// user's next unprefixed message goes to the DEFAULT agent (no binding was
// created).
func TestAgentSelection_NameOnlySelectsNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
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
	require.Equal(t, "kagent/swarmgeist", resolved()[0].AgentRef, "no binding was created; the default agent applies")
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

	sendEvent(t, srv, mention("U1", "/agent", "100.000", ""))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Available agents")
	}, 2*time.Second, 50*time.Millisecond, "the roster listing posts")
	listing := allText(fake.pathCalls("chat.postMessage"))
	require.Contains(t, listing, "`/agent \"<name>\" <question>`",
		"the listing advertises the quoted display-name form")
	require.Contains(t, listing, "*SRE Agent* — Investigates infra issues",
		"display name and description")
	require.Contains(t, listing, "*k8s-agent* — Kubernetes specialist",
		"an agent without a display-name annotation lists by technical name")
	require.NotContains(t, listing, "kagent", "namespaces never appear in the roster")
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
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent", "kagent/k8s-agent": "K8s Agent"}}
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
	require.Equal(t, "kagent/sre-agent", msgs[1].AgentRef, "the binding is unchanged after a refused switch")
}

// The discovery flow in a fresh pane chat: a bare /agent lists the roster,
// and the follow-up quoted selection in the SAME chat still binds — the
// consumed roster request never started a conversation, so it must not turn
// the selection into a refused mid-conversation switch.
func TestAgentSelection_PaneRosterThenSelectBinds(t *testing.T) {
	fake := newFakeSlackAPI()
	// The thread as Slack reports it: the pane anchor, the consumed roster
	// request, and the selection message itself.
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[
			{"user":"UBOT","subtype":"assistant_app_thread","ts":"100.000","text":"New Assistant Thread"},
			{"user":"U1","text":"/agent","ts":"200.000"},
			{"user":"U1","text":"/agent \"SRE Agent\" check crashing pods in gazelle","ts":"300.000"}]}`)
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
	}}
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(roster, cards))

	sendEvent(t, srv, dmThreadEvent("U1", "/agent", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Available agents")
	}, 2*time.Second, 50*time.Millisecond, "the roster listing posts")

	sendEvent(t, srv, dmThreadEvent("U1", `/agent "SRE Agent" check crashing pods in gazelle`, "300.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the selection after the roster listing dispatches")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "already has its agent",
		"a consumed roster request must not make the selection a refused switch")
	require.Equal(t, "kagent/sre-agent", resolved()[0].AgentRef)
	require.Equal(t, "check crashing pods in gazelle", resolved()[0].Text)
}

// The same conversation shape survives a restart: recovery derives the
// binding from the first DISPATCHED human message, skipping the consumed
// roster request that precedes it.
func TestAgentSelection_RecoverySkipsConsumedCommands(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[
			{"user":"UBOT","subtype":"assistant_app_thread","ts":"100.000","text":"New Assistant Thread"},
			{"user":"U1","text":"/agent","ts":"200.000"},
			{"user":"U1","text":"/agent \"SRE Agent\" original question","ts":"300.000"}]}`)
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
	}}
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(roster, cards))

	sendEvent(t, srv, dmThreadEvent("U1", "still there?", "400.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)
	require.Equal(t, "kagent/sre-agent", resolved()[0].AgentRef,
		"recovery skips the consumed roster request and finds the real opener")
}

// Assistant-pane semantics: a pane chat's first message arrives with
// thread_ts set to a Slack-managed anchor allocated at chat-open (it is NOT
// its own thread root — observed live, klaus-gateway#157). Selection on that
// first message must bind, replies must inherit, and a mid-chat switch must
// still be refused.
func TestAgentSelection_PaneFirstMessageBindsAndInherits(t *testing.T) {
	fake := newFakeSlackAPI()
	// The conversation-start check scans the thread. The anchor is a REAL
	// message (subtype assistant_app_thread) authored under the app's user id
	// with no bot_id — the scan must not mistake it for a human message.
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[
			{"user":"UBOT","subtype":"assistant_app_thread","ts":"100.000","text":"New Assistant Thread"},
			{"user":"U1","text":"/agent sre-agent hello there","ts":"200.000"}]}`)
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent", "kagent/k8s-agent": "K8s Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, cards))

	// First message of a new pane chat: ts 200.000, thread anchor 100.000.
	sendEvent(t, srv, dmThreadEvent("U1", "/agent sre-agent hello there", "200.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the pane's first message binds and dispatches")

	sendEvent(t, srv, dmThreadEvent("U1", "follow up", "300.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond)

	msgs := resolved()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef, "the pane conversation binds to the named agent")
	require.Equal(t, "hello there", msgs[0].Text)
	require.Equal(t, "kagent/sre-agent", msgs[1].AgentRef, "pane replies inherit the binding")

	// A mid-chat switch is still refused: the binding already exists.
	sendEvent(t, srv, dmThreadEvent("U1", "/agent k8s-agent switch now", "400.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "already has its agent")
	}, 2*time.Second, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 2, gw.resolveCount(), "the refused switch dispatches nothing")
}

// A new pane chat is not greeted with the "starting fresh" resume notice: its
// first message arrives as a thread reply (the chat's Slack-created anchor is
// its thread_ts) but it OPENS the conversation — there is no earlier session
// to resume. A genuine reply into a thread the process does not know still
// announces when the session is conclusively gone.
func TestPane_NewChatNotGreetedWithStartingFresh(t *testing.T) {
	sessionGone := func(channels.InboundMessage) (bool, bool) { return false, true }

	t.Run("first pane message is silent", func(t *testing.T) {
		fake := newFakeSlackAPI()
		fake.setResponse("conversations.replies", `{"ok":true,"messages":[
			{"user":"UBOT","subtype":"assistant_app_thread","ts":"100.000","text":"New Assistant Thread"},
			{"user":"U1","ts":"200.000","text":"hello"}]}`)
		gw := &stubGateway{
			deltas:             []channels.OutboundDelta{{Content: "hi"}, {Done: true}},
			onSessionResumable: sessionGone,
		}
		_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

		sendEvent(t, srv, dmThreadEvent("U1", "hello", "200.000", "100.000"))
		require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
			2*time.Second, 50*time.Millisecond)
		time.Sleep(150 * time.Millisecond)
		require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "starting fresh",
			"a conversation-opening pane message must not be greeted with the resume notice")
	})

	t.Run("genuine reply still announces", func(t *testing.T) {
		fake := newFakeSlackAPI()
		// The thread's opening human message predates this reply: the process
		// is resuming a conversation it does not know.
		fake.setResponse("conversations.replies", `{"ok":true,"messages":[
			{"user":"UBOT","subtype":"assistant_app_thread","ts":"100.000","text":"New Assistant Thread"},
			{"user":"U1","ts":"150.000","text":"earlier question"}]}`)
		gw := &stubGateway{
			deltas:             []channels.OutboundDelta{{Content: "hi"}, {Done: true}},
			onSessionResumable: sessionGone,
		}
		_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

		sendEvent(t, srv, dmThreadEvent("U1", "are you still there?", "300.000", "100.000"))
		require.Eventually(t, func() bool {
			return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "starting fresh")
		}, 2*time.Second, 50*time.Millisecond, "a real resume with a gone session still gets the notice")
	})
}

// A pane selection whose conversation-start check cannot run (thread scan
// fails) is refused with an honest transient error, not silently accepted:
// accepting blindly could rebind an existing conversation and fork its session.
func TestAgentSelection_PaneStartCheckFailure(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("conversations.replies", "internal_error")
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, dmThreadEvent("U1", "/agent sre-agent hello", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "couldn't check this conversation")
	}, 2*time.Second, 50*time.Millisecond)
	require.Zero(t, gw.resolveCount(), "an unverifiable selection dispatches nothing")
}

// After a restart (no in-memory binding) a reply still inherits its
// conversation's agent: the binding is re-derived from the conversation's
// opening message (the thread root in channels; the first human message in
// the pane), where the prefix is visible.
func TestAgentSelection_ReplyInheritsBindingFromRootAfterRestart(t *testing.T) {
	fake := newFakeSlackAPI()
	// The fresh process has never seen thread 100.000; the root text carries
	// the original prefix (with the mention token Slack includes).
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[{"user":"U1","text":"<@UBOT> /agent sre-agent original question","ts":"100.000"}]}`)
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, dmThreadEvent("U1", "are you still there?", "300.000", "100.000"))

	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the reply dispatches")
	require.Equal(t, "kagent/sre-agent", resolved()[0].AgentRef,
		"the binding is recovered from the conversation root")
}

// Live selection and recovery must agree on every input. A quoted opener
// selected by display name; after a restart (no in-memory binding) a reply
// re-resolves that display name against the live roster and inherits the same
// agent.
func TestAgentSelection_QuotedOpenerRecoversBindingFromRoster(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[{"user":"U1","text":"/agent \"SRE Agent\" original question","ts":"100.000"}]}`)
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
	}}
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(roster, cards))

	sendEvent(t, srv, dmThreadEvent("U1", "still there?", "300.000", "100.000"))

	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)
	require.Equal(t, "kagent/sre-agent", resolved()[0].AgentRef,
		"the display-name binding is recovered from the opening message via the roster")
}

// A quoted opener whose display name no longer resolves to exactly one agent
// refuses the reply loudly — never a silent re-route to the default agent —
// and caches nothing, so a later reply re-resolves.
func TestAgentSelection_QuotedOpenerGoneRefusesLoudly(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[{"user":"U1","text":"/agent \"SRE Agent\" original question","ts":"100.000"}]}`)
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "swarmgeist", Namespace: "kagent", DisplayName: "Swarmgeist"},
	}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, dmThreadEvent("U1", "still there?", "300.000", "100.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "doesn't match exactly one agent anymore")
	}, 2*time.Second, 50*time.Millisecond, "the refusal is loud and names the problem")
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "the turn is never re-routed to another agent")
}

// A transient roster failure during recovery refuses the turn with a
// try-again notice instead of guessing an agent.
func TestAgentSelection_QuotedOpenerRosterFailureRefusesTurn(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setResponse("conversations.replies",
		`{"ok":true,"messages":[{"user":"U1","text":"/agent \"SRE Agent\" original question","ts":"100.000"}]}`)
	roster := &fakeRoster{err: errors.New("kagent agents: unexpected status 502")}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, dmThreadEvent("U1", "still there?", "300.000", "100.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "couldn't check which agent this conversation uses")
	}, 2*time.Second, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "nothing is dispatched on an unverifiable binding")
}

// A quoted display name selects the agent: the roster resolves it to the
// technical ref, the conversation binds, and replies inherit.
func TestAgentSelection_QuotedDisplayNameBindsConversation(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
	}}
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, cards))

	sendEvent(t, srv, mention("U1", `/agent "SRE Agent" why are pods crashing in gazelle?`, "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "quoted display-name selection dispatches")

	sendEvent(t, srv, mention("U1", "and the nodes?", "200.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond)

	msgs := resolved()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef, "the display name resolves to the technical ref")
	require.Equal(t, "why are pods crashing in gazelle?", msgs[0].Text)
	require.Equal(t, "kagent/sre-agent", msgs[1].AgentRef, "replies inherit the conversation's agent")
}

// With a bare default agent (namespace-in-URL deployment) a quoted selection
// resolves to a bare ref, matching the shape the default route uses.
func TestAgentSelection_QuotedDisplayNameBareDefault(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
	}}
	cards := &fakeCards{known: map[string]string{"sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, cards),
		func(a *slackadapter.Adapter) { a.DefaultAgent = "swarmgeist" })

	sendEvent(t, srv, mention("U1", `/agent "SRE Agent" check gazelle`, "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)
	require.Equal(t, "sre-agent", resolved()[0].AgentRef, "bare default keeps refs bare")
}

// A quoted name matching no agent fails loudly with the roster; a quoted name
// matching more than one refuses with the technical selectors that
// disambiguate. Neither dispatches anything.
func TestAgentSelection_QuotedDisplayNameNoMatchAndAmbiguous(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
		{Name: "sre-agent", Namespace: "other", DisplayName: "SRE Agent"},
	}}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, mention("U1", `/agent "No Such Agent" do things`, "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "I don't know an agent named `No Such Agent`")
	}, 2*time.Second, 50*time.Millisecond, "no match fails loudly")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "*SRE Agent*",
		"the failure reply includes the roster")

	sendEvent(t, srv, mention("U1", `/agent "SRE Agent" do things`, "200.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "matches more than one agent")
	}, 2*time.Second, 50*time.Millisecond, "ambiguity refuses instead of picking")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "`/agent kagent/sre-agent <question>`",
		"the ambiguity reply lists the technical selectors")
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "neither failure dispatches anything")
}

// A roster fetch failure during a quoted selection refuses with a try-again
// notice; it never guesses.
func TestAgentSelection_QuotedDisplayNameRosterFailure(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{err: errors.New("kagent agents: unexpected status 502")}
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(roster, &fakeCards{}))

	sendEvent(t, srv, mention("U1", `/agent "SRE Agent" do things`, "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "couldn't check the available agents")
	}, 2*time.Second, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount())
}

// Two users' prefixed conversations in one channel are independent: each new
// conversation binds to its own agent and neither redirects the other.
func TestAgentSelection_TwoUsersIndependentConversations(t *testing.T) {
	fake := newFakeSlackAPI()
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent", "kagent/k8s-agent": "K8s Agent"}}
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
		"100.000": "kagent/sre-agent",
		"200.000": "kagent/k8s-agent",
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
	cards := &fakeCards{known: map[string]string{"kagent/sre-agent": "SRE Agent"}}
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(&fakeRoster{}, cards))

	sendEvent(t, srv, mention("U1", "/agent SRE-Agent why?", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond)
	require.Equal(t, "kagent/sre-agent", resolved()[0].AgentRef)
}

// /help mentions the /agent command when selection is available, and not when
// it is not (no card client to validate names against).
func TestAgentSelection_HelpMentionsAgentCommand(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, withSelection(&fakeRoster{}, &fakeCards{}))

	sendEvent(t, srv, dmEvent("U1", "/help", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "/agent \"<name>\" <question>")
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
