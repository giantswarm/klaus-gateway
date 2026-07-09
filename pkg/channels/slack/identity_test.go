package slack_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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

// The agent's reply posts under the AgentCard display name, and without an
// icon_url (so Slack keeps the app's own icon) when the card exposes no icon.
func TestBranding_AgentReplyCarriesCardName(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{sendQueue: [][]channels.OutboundDelta{{{Content: "all good"}, {Done: true}}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.AgentCards = stubCards{username: "SRE agent"}
	})

	sendEvent(t, srv, dmEvent("U1", "status?", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "all good")
	}, 2*time.Second, 20*time.Millisecond, "the agent answer is posted")

	var branded bool
	for _, c := range fake.pathCalls("chat.postMessage") {
		if u, _ := c.params["username"].(string); u != "SRE agent" {
			continue
		}
		branded = true
		_, hasIcon := c.params["icon_url"]
		require.False(t, hasIcon, "no card icon means the app icon is kept (icon_url omitted)")
	}
	require.True(t, branded, "the agent reply carries the AgentCard username")
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

// A DM, while the adapter serves channels, gets a redirect and never reaches the agent.
func TestDM_RedirectInChannelMode(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, dmEvent("U1", "hey", "600.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "I work in channels")
	}, 2*time.Second, 20*time.Millisecond, "a DM in channel mode is redirected")
	require.Zero(t, gw.resolveCount(), "a redirected DM never reaches the agent")
}
