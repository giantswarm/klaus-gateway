package slack_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// allowlistMode serves only the given channels and redirects DMs.
func allowlistMode(allowed ...string) func(*slackadapter.Adapter) {
	return func(a *slackadapter.Adapter) {
		a.DMMode = slackadapter.DMModeRedirect
		a.ChannelMode = slackadapter.ChannelModeAllowlist
		a.ChannelAllowlist = allowed
	}
}

// A mention in an allowlisted channel dispatches; a mention outside the
// allowlist is dropped with one ephemeral notice per (channel, user).
func TestEventsHandler_ChannelAllowlist(t *testing.T) {
	gw := &stubGateway{}
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, allowlistMode("C1"))

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> hi","channel":"C1","ts":"111.222"}}`)
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 20*time.Millisecond, "a mention in an allowlisted channel dispatches")

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> hi","channel":"C9","ts":"333.444"}}`)
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == 1 },
		10*time.Second, 20*time.Millisecond, "a mention outside the allowlist gets an ephemeral notice")
	require.Equal(t, 1, gw.resolveCount(), "a mention outside the allowlist must not dispatch")

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> again","channel":"C9","ts":"555.666"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Len(t, fake.pathCalls("chat.postEphemeral"), 1, "repeated mentions nudge once per window")
	require.Equal(t, 1, gw.resolveCount())
}

// With DMs served alongside channels, both surfaces dispatch and no DM
// redirect is posted.
func TestEventsHandler_DMServedAlongsideChannels(t *testing.T) {
	gw := &stubGateway{}
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.DMMode = slackadapter.DMModeServe
		a.ChannelMode = slackadapter.ChannelModeAll
	})

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi","channel":"D1","ts":"111.000"}}`)
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 20*time.Millisecond, "a DM dispatches in serve mode")

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@BOT> hi","channel":"C1","ts":"222.000"}}`)
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		10*time.Second, 20*time.Millisecond, "a channel mention dispatches alongside DMs")
}

// DMModeIgnore drops DMs silently: no dispatch, no redirect.
func TestEventsHandler_DMIgnored(t *testing.T) {
	gw := &stubGateway{}
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.DMMode = slackadapter.DMModeIgnore
		a.ChannelMode = slackadapter.ChannelModeAll
	})

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi","channel":"D1","ts":"111.000"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "an ignored DM must not dispatch")
	require.Empty(t, fake.pathCalls("chat.postMessage"), "an ignored DM must not be answered or redirected")
}

// The bot's own join posts the intro only in served channels.
func TestMemberJoined_IntroSkippedInUnservedChannel(t *testing.T) {
	fake := newFakeSlackAPI() // botUserID "UBOT"
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, allowlistMode("C1"))

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"member_joined_channel","user":"UBOT","channel":"C9"}}`)
	fake.waitForPath(t, "auth.test", 1) // the bot-ID lookup ran; the channel gate then dropped the intro
	require.Empty(t, fake.pathCalls("chat.postMessage"), "no intro outside the allowlist")

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"member_joined_channel","user":"UBOT","channel":"C1"}}`)
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postMessage")) == 1 },
		10*time.Second, 20*time.Millisecond, "the intro posts in an allowlisted channel")
}

// Unknown mode strings are rejected at Start.
func TestStart_RejectsUnknownModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*slackadapter.Adapter)
	}{
		{"dm mode", func(a *slackadapter.Adapter) { a.DMMode = "sometimes" }},
		{"channel mode", func(a *slackadapter.Adapter) { a.ChannelMode = "most" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &slackadapter.Adapter{
				Mode:         slackadapter.ModeEvents,
				Secrets:      slackadapter.Secrets{BotToken: "dummy-bot-token", SigningSecret: "signing-secret"}, //nolint:gosec
				DefaultAgent: "test-agent",
			}
			tc.mut(a)
			err := a.Start(t.Context(), &stubGateway{})
			require.Error(t, err)
			require.Contains(t, fmt.Sprint(err), "unknown")
		})
	}
}

var _ channels.Gateway = (*stubGateway)(nil)
