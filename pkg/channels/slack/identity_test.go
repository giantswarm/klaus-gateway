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
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL) // DM-only default

	// Turn 1: the tool prompt surfaces for approval.
	sendEvent(t, srv, dmEvent("U1", "clean up configmaps", "400.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Waiting for approval")
	}, flowWait, 20*time.Millisecond, "the approval prompt is posted")

	// Click Chat: the prompt is held and the buttons become a reply hint.
	sendInteraction(t, srv, "hitl_chat", "400.000")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.update")), "Ask your question")
	}, flowWait, 20*time.Millisecond, "Chat swaps the buttons for a reply hint")

	// Reply with a question: resolves the paused task as a reject carrying it.
	waitThreadIdle(t, a, "400.000")
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"which ones exactly?","channel":"D1","ts":"401.000","thread_ts":"400.000"}}`)
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 }, flowWait, 20*time.Millisecond, "the reply resumes the paused task")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, decisions)
	last := decisions[len(decisions)-1]
	require.NotNil(t, last, "the follow-up carries a HITL decision")
	require.Equal(t, channels.DecisionReject, last.Type)
	require.Contains(t, last.RejectionReason, "which ones exactly?")
}

// usernamesOf returns the username each recorded call carried.
func usernamesOf(calls []recordedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		u, _ := c.params["username"].(string)
		out = append(out, u)
	}
	return out
}

// awaitAgentReply drives one DM turn and waits for the agent's answer to be
// streamed into the thread.
func awaitAgentReply(t *testing.T, srv *httptest.Server, fake *fakeSlackAPI, ts string) {
	t.Helper()
	sendEvent(t, srv, dmEvent("U1", "status?", ts))
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "all good")
	}, flowWait, 20*time.Millisecond, "the agent answer is streamed")
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

	names := usernamesOf(fake.pathCalls(pathStartStream))
	require.Contains(t, names, "SRE Assistant", "the reply carries the display-name annotation")
	require.NotContains(t, names, "test_agent", "the AgentCard name is never shown")
	for _, c := range fake.pathCalls(pathStartStream) {
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

	names := usernamesOf(fake.pathCalls(pathStartStream))
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

	require.Contains(t, usernamesOf(fake.pathCalls(pathStartStream)), "test-agent",
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

	names := usernamesOf(fake.pathCalls(pathStartStream))
	require.Contains(t, names, "test-agent", "the namespace qualifier is stripped")
	require.NotContains(t, names, "kagent/test-agent", "no namespace reaches Slack")
}

// With no roster wired at all, branding still names the agent rather than
// falling back to the app's own identity.
func TestBranding_NoRosterStillNamesTheAgent(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL)

	awaitAgentReply(t, srv, fake, "504.000")

	require.Contains(t, usernamesOf(fake.pathCalls(pathStartStream)), "test-agent",
		"the technical name is used when no roster is configured")
}

// A workspace whose install predates chat:write.customize rejects branded posts
// with missing_scope. The reply must still arrive — retried under the app
// identity — and the downgrade latches so later turns skip the doomed branded
// attempt entirely. Same auto-downgrade shape as reactions.
func TestBranding_MissingScopeFallsBackToAppIdentity(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.failIf = func(path string, params map[string]any) string {
		if u, _ := params["username"].(string); path == pathStartStream && u != "" {
			return "missing_scope"
		}
		return ""
	}
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "test-agent", DisplayName: "SRE Assistant"},
	}}
	gw := &stubGateway{sendQueue: [][]channels.OutboundDelta{
		{{Content: "all good"}, {Done: true}},
		{{Content: "all good"}, {Done: true}},
	}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.Roster = roster
	})

	awaitAgentReply(t, srv, fake, "600.000")

	branded := 0
	for _, u := range usernamesOf(fake.pathCalls(pathStartStream)) {
		if u != "" {
			branded++
		}
	}
	require.Positive(t, branded, "the first stream attempts branding")
	var unbranded bool
	for _, c := range fake.pathCalls(pathStartStream) {
		if u, _ := c.params["username"].(string); u == "" {
			unbranded = true
		}
	}
	require.True(t, unbranded, "the rejected stream is retried under the app identity")
	require.Contains(t, fake.streamedText(), "all good", "the reply arrives")

	// Second turn: the latched downgrade skips the branded attempt entirely.
	sendEvent(t, srv, dmEvent("U1", "status?", "601.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls(pathStopStream)) >= 2
	}, flowWait, 20*time.Millisecond, "the second reply arrives too")
	after := 0
	for _, u := range usernamesOf(fake.pathCalls(pathStartStream)) {
		if u != "" {
			after++
		}
	}
	require.Equal(t, branded, after, "no further branded attempts after the downgrade latched")
}

// The display name is sanitized on its way to the username param: a multi-line
// annotation is legal Kubernetes, but control characters must cost the label's
// shape, never the post carrying it.
func TestBranding_DisplayNameIsSanitized(t *testing.T) {
	fake := newFakeSlackAPI()
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "test-agent", DisplayName: "SRE\nAssistant\x07 (on-call)"},
	}}
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.Roster = roster
	})

	awaitAgentReply(t, srv, fake, "602.000")

	require.Contains(t, usernamesOf(fake.pathCalls(pathStartStream)), "SRE Assistant (on-call)",
		"control characters become spaces and runs collapse")
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
	require.Contains(t, usernamesOf(fake.pathCalls(pathStartStream)), "test-agent",
		"a failed lookup degrades to the technical name, the reply still arrives")

	after := roster.listCalls()
	require.Positive(t, after, "branding did consult the roster, so the next assertion is not vacuous")
	sendEvent(t, srv, dmEvent("U1", "status?", "506.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls(pathStartStream)) > 1
	}, flowWait, 20*time.Millisecond, "the second turn is answered too")
	require.Equal(t, after, roster.listCalls(),
		"the failure is cached, so branding does not re-ask the controller")
}

// An agent that carries the app's own name posts under the app identity: no
// username, no icon_url, so the reply wears the app's face like Slack's own
// thread furniture does. The match is case-insensitive — the fake's auth.test
// handle is `swarmgeist`, the agent's display name "Swarmgeist".
func TestBranding_AppNamesakeAgentPostsAsTheApp(t *testing.T) {
	fake := newFakeSlackAPI() // auth.test user "swarmgeist"
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "swarmgeist", DisplayName: "Swarmgeist"},
	}}
	_, srv := newEventsAdapter(t, replyGateway(), fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.DefaultAgent = "swarmgeist"
		a.Roster = roster
		a.AgentCards = stubCards{username: "swarmgeist", iconURL: "https://avatars.test/v1/swarmgeist.png"}
	})

	awaitAgentReply(t, srv, fake, "507.000")

	reply := replyPost(t, fake)
	_, hasName := reply.params["username"]
	_, hasIcon := reply.params["icon_url"]
	require.False(t, hasName, "the namesake agent posts under the app's own name")
	require.False(t, hasIcon, "and under the app's own icon")
}

// replyPost returns the chat.startStream that opened the stub agent's answer.
// The answer itself arrives in pieces across that call and the ones after it,
// so the text is asserted on the stream as a whole.
func replyPost(t *testing.T, fake *fakeSlackAPI) recordedCall {
	t.Helper()
	require.Contains(t, fake.streamedText(), "all good", "the agent's answer was streamed")
	calls := fake.pathCalls(pathStartStream)
	require.NotEmpty(t, calls, "no chat.startStream carried the agent's answer")
	return calls[0]
}

// Without `users:read` the bot's own users.info is refused with missing_scope
// on every call. The namesake check then works off the auth.test handle and,
// since the refusal is final, asks users.info once per process — not once per
// post.
func TestBranding_NamesakeCheckAsksUsersInfoOnceWithoutScope(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.failIf = func(path string, params map[string]any) string {
		if user, _ := params["user"].(string); path == "users.info" && user == "UBOT" {
			return "missing_scope"
		}
		return ""
	}
	roster := &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "swarmgeist", DisplayName: "Swarmgeist"},
	}}
	gw := &stubGateway{sendQueue: [][]channels.OutboundDelta{
		{{Content: "all good"}, {Done: true}},
		{{Content: "all good"}, {Done: true}},
	}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.DefaultAgent = "swarmgeist"
		a.Roster = roster
	})

	awaitAgentReply(t, srv, fake, "508.000")
	sendEvent(t, srv, dmEvent("U1", "status?", "509.000"))
	require.Eventually(t, func() bool {
		return strings.Count(fake.streamedText(), "all good") >= 2
	}, flowWait, 20*time.Millisecond, "the second reply arrives too")

	for _, u := range usernamesOf(fake.pathCalls(pathStartStream)) {
		require.Empty(t, u, "the namesake agent posts as the app on the auth.test handle alone")
	}
	botLookups := 0
	for _, c := range fake.pathCalls("users.info") {
		if user, _ := c.params["user"].(string); user == "UBOT" {
			botLookups++
		}
	}
	require.Equal(t, 1, botLookups, "a missing_scope refusal is final and not retried per post")
}

// The bot being added to a channel posts exactly one Swarmgeist intro.
func TestMemberJoined_SelfJoinPostsIntro(t *testing.T) {
	fake := newFakeSlackAPI() // botUserID "UBOT"
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"member_joined_channel","user":"UBOT","channel":"C1"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Swarmgeist")
	}, flowWait, 20*time.Millisecond, "the bot's own join posts an intro")
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
	}, flowWait, 20*time.Millisecond, "a DM in channel mode is redirected")
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
