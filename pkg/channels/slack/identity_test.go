package slack_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// stubCards is a fake AgentCardResolver returning fixed branding.
type stubCards struct {
	username string
	iconURL  string
}

func (s stubCards) CardIdentity(_ context.Context, _ string) (username, iconURL string) {
	return s.username, s.iconURL
}

// Clicking "Chat" on an approval prompt holds the paused task and swaps the
// buttons for a reply hint; the next in-thread reply is routed to the task as a
// reject carrying the question (kagent resolves the gate, the agent re-proposes).
func TestChat_HoldsPromptThenRoutesQuestionAsReject(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var decisions []*channels.HitlDecision
	gw := &stubGateway{
		sendQueue: [][]channels.OutboundDelta{
			{{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"}}},
			{{Content: "here are the configmaps"}, {Done: true}},
		},
		onResolve: func(m channels.InboundMessage) {
			mu.Lock()
			decisions = append(decisions, m.Decision)
			mu.Unlock()
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL) // DM-only default

	// Turn 1: the tool prompt surfaces for approval.
	sendEvent(t, srv, dmEvent("U1", "clean up configmaps", "400.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Waiting for approval")
	}, 2*time.Second, 20*time.Millisecond, "the approval prompt is posted")

	// Click Chat: the prompt is held and the buttons become a reply hint.
	sendInteraction(t, srv, "hitl_chat", "400.000")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.update")), "Ask your question")
	}, 2*time.Second, 20*time.Millisecond, "Chat swaps the buttons for a reply hint")

	// Reply with a question: resolves the paused task as a reject carrying it.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"which ones exactly?","channel":"D1","ts":"401.000","thread_ts":"400.000"}}`)
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 }, 2*time.Second, 20*time.Millisecond, "the reply resumes the paused task")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, decisions)
	last := decisions[len(decisions)-1]
	require.NotNil(t, last, "the follow-up carries a HITL decision")
	require.Equal(t, channels.DecisionReject, last.Type)
	require.Contains(t, last.RejectionReason, "which ones exactly?")
}

// usernamesOf returns the username each recorded chat.postMessage carried.
func usernamesOf(calls []recordedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		u, _ := c.params["username"].(string)
		out = append(out, u)
	}
	return out
}

// awaitAgentReply drives one DM turn and waits for the agent's answer to be posted.
func awaitAgentReply(t *testing.T, srv *httptest.Server, fake *fakeSlackAPI, ts string) {
	t.Helper()
	sendEvent(t, srv, dmEvent("U1", "status?", ts))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "all good")
	}, 2*time.Second, 20*time.Millisecond, "the agent answer is posted")
}

func replyGateway() *stubGateway {
	return &stubGateway{sendQueue: [][]channels.OutboundDelta{{{Content: "all good"}, {Done: true}}}}
}

// The agent's reply posts under the display-name annotation the roster reports —
// never under the AgentCard name, which kagent generates with underscores. The
// card is still consulted for the icon, and omitting icon_url when it has none
// keeps the app's own icon.
func TestBranding_AgentReplyCarriesDisplayName(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "test-agent", DisplayName: "SRE Assistant"},
	}}
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.Roster = roster
		a.AgentCards = stubCards{username: "test_agent"}
	})

	awaitAgentReply(t, srv, fake, "500.000")

	names := usernamesOf(fake.pathCalls("chat.postMessage"))
	require.Contains(t, names, "SRE Assistant", "the reply carries the display-name annotation")
	require.NotContains(t, names, "test_agent", "the AgentCard name is never shown")
	for _, c := range fake.pathCalls("chat.postMessage") {
		if u, _ := c.params["username"].(string); u == "SRE Assistant" {
			_, hasIcon := c.params["icon_url"]
			require.False(t, hasIcon, "no card icon means the app icon is kept (icon_url omitted)")
		}
	}
}

// Without a display-name annotation the reply carries the Agent resource's own
// spelling — the hyphenated technical name, which is what /agent accepts — and
// specifically not the card's underscored form.
func TestBranding_NoAnnotationFallsBackToTechnicalName(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{{Name: "test-agent"}}}
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.Roster = roster
		a.AgentCards = stubCards{username: "test_agent"}
	})

	awaitAgentReply(t, srv, fake, "501.000")

	names := usernamesOf(fake.pathCalls("chat.postMessage"))
	require.Contains(t, names, "test-agent", "the hyphenated resource name is used")
	require.NotContains(t, names, "test_agent", "the AgentCard name is never shown")
}

// An all-whitespace annotation counts as absent rather than posting a blank name.
func TestBranding_WhitespaceAnnotationCountsAsAbsent(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "test-agent", DisplayName: "   "},
	}}
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.Roster = roster
	})

	awaitAgentReply(t, srv, fake, "502.000")

	require.Contains(t, usernamesOf(fake.pathCalls("chat.postMessage")), "test-agent",
		"a blank annotation falls through to the technical name")
}

// A namespace-qualified ref posts under the bare resource name: the namespace is
// deployment configuration, not something a Slack reader should see.
func TestBranding_QualifiedRefPostsBareName(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.DefaultAgent = "kagent/test-agent"
	})

	awaitAgentReply(t, srv, fake, "503.000")

	names := usernamesOf(fake.pathCalls("chat.postMessage"))
	require.Contains(t, names, "test-agent", "the namespace qualifier is stripped")
	require.NotContains(t, names, "kagent/test-agent", "no namespace reaches Slack")
}

// With no roster wired at all, branding still names the agent rather than
// falling back to the app's own identity.
func TestBranding_NoRosterStillNamesTheAgent(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL)

	awaitAgentReply(t, srv, fake, "504.000")

	require.Contains(t, usernamesOf(fake.pathCalls("chat.postMessage")), "test-agent",
		"the technical name is used when no roster is configured")
}

// A roster failure costs the nice name, never the reply — and it is remembered,
// so a later turn falls straight through instead of paying the timeout again.
func TestBranding_RosterFailureDeliversReplyAndIsNotRetried(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{err: errors.New("controller unreachable")}
	gw := &stubGateway{sendQueue: [][]channels.OutboundDelta{
		{{Content: "all good"}, {Done: true}},
		{{Content: "all good"}, {Done: true}},
	}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.Roster = roster
	})

	awaitAgentReply(t, srv, fake, "505.000")
	require.Contains(t, usernamesOf(fake.pathCalls("chat.postMessage")), "test-agent",
		"a failed lookup degrades to the technical name, the reply still arrives")

	after := roster.listCalls()
	require.Positive(t, after, "branding did consult the roster, so the next assertion is not vacuous")
	sendEvent(t, srv, dmEvent("U1", "status?", "506.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls("chat.postMessage")) > 1
	}, 2*time.Second, 20*time.Millisecond, "the second turn is answered too")
	require.Equal(t, after, roster.listCalls(),
		"the failure is cached, so branding does not re-ask the controller")
}

// The bot being added to a channel posts exactly one Swarmgeist intro.
func TestMemberJoined_SelfJoinPostsIntro(t *testing.T) {
	fake := newFakeSlackAPI() // botUserID "UBOT"
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"member_joined_channel","user":"UBOT","channel":"C1"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Swarmgeist")
	}, 2*time.Second, 20*time.Millisecond, "the bot's own join posts an intro")
	require.Len(t, fake.pathCalls("chat.postMessage"), 1, "exactly one intro")
}

// Another user joining the channel does not post an intro.
func TestMemberJoined_OtherUserNoIntro(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"member_joined_channel","user":"U999","channel":"C1"}}`)
	fake.waitForPath(t, "auth.test", 1) // the bot-ID lookup ran and did not match
	require.Empty(t, fake.pathCalls("chat.postMessage"), "another user's join must not post an intro")
}

// A DM, while the adapter serves channels, gets a redirect and never reaches
// the agent. A follow-up DM in the same conversation (e.g. a reply to the
// redirect itself) does not get another redirect.
func TestDM_RedirectInChannelMode(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, dmEvent("U1", "hey", "600.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "I work in channels")
	}, 2*time.Second, 20*time.Millisecond, "a DM in channel mode is redirected")
	require.Zero(t, gw.resolveCount(), "a redirected DM never reaches the agent")

	sendEvent(t, srv, dmEvent("U1", "why not?", "601.000"))
	time.Sleep(150 * time.Millisecond)
	redirects := 0
	for _, call := range fake.pathCalls("chat.postMessage") {
		if text, _ := call.params["text"].(string); strings.Contains(text, "I work in channels") {
			redirects++
		}
	}
	require.Equal(t, 1, redirects, "a second DM within the guard window must not post another redirect")
	require.Zero(t, gw.resolveCount())
}

// The launch announcement posts only after the agent resolves: a channel root
// mention announces the handoff, while a resolve failure stays silent instead
// of announcing an agent that never arrives.
func TestLaunchAnnouncement_PostsAfterResolveSucceeds(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "on it"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@UBOT> check the cluster","channel":"C1","ts":"700.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Bringing in")
	}, 2*time.Second, 20*time.Millisecond, "a new channel thread announces the agent handoff")
}

// The launch announcement posts under the agent's identity, not the app
// default, so it shares one authoring bot with the agent's replies and the
// channel thread face pile collapses to a single avatar instead of two. Its
// name comes from the display-name annotation and its icon from the card, so
// the announcement text and the username that carries it always agree.
func TestLaunchAnnouncement_CarriesAgentIdentity(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "on it"}, {Done: true}}}
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "test-agent", DisplayName: "SRE Assistant"},
	}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, func(a *slackadapter.Adapter) {
		a.Roster = roster
		a.AgentCards = stubCards{username: "test_agent", iconURL: "https://example.test/sre.png"}
	})

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@UBOT> check the cluster","channel":"C1","ts":"720.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Bringing in")
	}, 2*time.Second, 20*time.Millisecond, "a new channel thread announces the agent handoff")

	var announced bool
	for _, c := range fake.pathCalls("chat.postMessage") {
		if text, _ := c.params["text"].(string); !strings.Contains(text, "Bringing in") {
			continue
		}
		announced = true
		require.Contains(t, c.params["text"], "SRE Assistant", "the announcement names the agent by display name")
		require.Equal(t, "SRE Assistant", c.params["username"], "the announcement posts under the agent name")
		require.Equal(t, "https://example.test/sre.png", c.params["icon_url"], "the announcement carries the agent icon")
	}
	require.True(t, announced, "the launch announcement was posted")
}

func TestLaunchAnnouncement_SkippedWhenResolveFails(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{resolveErr: errors.New("stub: resolve failed")}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@UBOT> check the cluster","channel":"C1","ts":"710.000"}}`)
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 20*time.Millisecond, "the mention reaches Resolve")
	time.Sleep(150 * time.Millisecond)
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Bringing in",
		"a failed resolve must not announce a launch")
}
