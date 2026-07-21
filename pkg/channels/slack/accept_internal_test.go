package slack

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAcceptEvent(t *testing.T) {
	now := time.Now().Unix()
	newTS := strconv.FormatInt(now+5, 10) + ".000100"
	oldTS := strconv.FormatInt(now-3600, 10) + ".000100"

	cases := []struct {
		name string
		ev   slackInnerEvent
		want bool
	}{
		{"dm fresh accepted", slackInnerEvent{ChannelType: "im", Channel: "D1", TS: newTS}, true},
		{"channel fresh accepted", slackInnerEvent{ChannelType: "channel", Channel: "C1", TS: newTS}, true},
		{"stale dm dropped", slackInnerEvent{ChannelType: "im", Channel: "D1", TS: oldTS}, false},
		{"missing ts not treated as stale", slackInnerEvent{ChannelType: "im", Channel: "D1", TS: ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{Logger: slog.Default(), DropStaleEvents: true, startUnix: now}
			if got := a.acceptEvent(tc.ev); got != tc.want {
				t.Fatalf("acceptEvent(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}

func TestChannelServed(t *testing.T) {
	cases := []struct {
		name      string
		mode      ChannelMode
		allowlist []string
		channel   string
		want      bool
	}{
		{"empty mode serves all", "", nil, "C1", true},
		{"all serves any channel", ChannelModeAll, nil, "C1", true},
		{"none serves nothing", ChannelModeNone, nil, "C1", false},
		{"allowlist hit", ChannelModeAllowlist, []string{"C1", "C2"}, "C2", true},
		{"allowlist miss", ChannelModeAllowlist, []string{"C1"}, "C9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{ChannelMode: tc.mode, ChannelAllowlist: tc.allowlist}
			require.Equal(t, tc.want, a.channelServed(tc.channel))
		})
	}
}

func TestToInboundMessageRejectsEmptyUser(t *testing.T) {
	event := slackInnerEvent{Type: evtAppMention, Text: "hello", Channel: "C1", TS: "1.2"}
	_, ok := event.toInboundMessage(false)
	require.False(t, ok, "an event without a Slack user must never become a turn")

	event.User = "U123"
	msg, ok := event.toInboundMessage(false)
	require.True(t, ok)
	require.Equal(t, "U123", msg.Subject)
}
