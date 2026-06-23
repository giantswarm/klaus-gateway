package slack

import (
	"log/slog"
	"strconv"
	"testing"
	"time"
)

func TestAcceptEvent(t *testing.T) {
	now := time.Now().Unix()
	newTS := strconv.FormatInt(now+5, 10) + ".000100"
	oldTS := strconv.FormatInt(now-3600, 10) + ".000100"

	cases := []struct {
		name   string
		dmOnly bool
		ev     slackInnerEvent
		want   bool
	}{
		{"dm fresh accepted", false, slackInnerEvent{ChannelType: "im", Channel: "D1", TS: newTS}, true},
		{"channel accepted when not dm-only", false, slackInnerEvent{ChannelType: "channel", Channel: "C1", TS: newTS}, true},
		{"channel dropped in dm-only", true, slackInnerEvent{ChannelType: "channel", Channel: "C1", TS: newTS}, false},
		{"dm accepted in dm-only", true, slackInnerEvent{ChannelType: "im", Channel: "D1", TS: newTS}, true},
		{"dm by channel-id prefix in dm-only", true, slackInnerEvent{Channel: "D2", TS: newTS}, true},
		{"stale dm dropped", true, slackInnerEvent{ChannelType: "im", Channel: "D1", TS: oldTS}, false},
		{"missing ts not treated as stale", true, slackInnerEvent{ChannelType: "im", Channel: "D1", TS: ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{Logger: slog.Default(), DMOnly: tc.dmOnly, DropStaleEvents: true, startUnix: now}
			if got := a.acceptEvent(tc.ev); got != tc.want {
				t.Fatalf("acceptEvent(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}
