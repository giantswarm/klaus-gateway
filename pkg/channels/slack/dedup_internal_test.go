package slack

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// A mention inside a channel thread is delivered as both app_mention and
// message.channels with distinct event_ids. Only one may start a turn; the
// twin must be dropped by the (channel, ts) message dedup, whichever event
// type arrives first (klaus-gateway#159).
func TestHandleInbound_CrossEventTypeTwinDeduped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.2"})
	}))
	t.Cleanup(srv.Close)

	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	a := &Adapter{
		APIBase:      srv.URL,
		Secrets:      Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		DefaultAgent: "worker",
		Logger:       slog.New(slog.DiscardHandler),
	}
	require.NoError(t, a.Start(t.Context(), gw))
	a.accessPolicy().SetInitiator("100.000", "U1")

	mentionTwin := slackInnerEvent{
		Type: evtAppMention, User: "U1", Text: "<@BOT> again",
		Channel: "C1", TS: "200.000", ThreadTS: "100.000",
	}
	messageTwin := slackInnerEvent{
		Type: evtMessage, ChannelType: "channel", User: "U1", Text: "<@BOT> again",
		Channel: "C1", TS: "200.000", ThreadTS: "100.000",
	}

	a.handleInbound(t.Context(), mentionTwin, "Ev-twin-1")
	require.Equal(t, 1, gw.sendCount(), "the first delivery dispatches")

	a.handleInbound(t.Context(), messageTwin, "Ev-twin-2")
	require.Equal(t, 1, gw.sendCount(), "the message twin must not start a second turn")

	// A genuinely new message in the same thread still dispatches.
	next := mentionTwin
	next.TS = "201.000"
	a.handleInbound(t.Context(), next, "Ev-next")
	require.Equal(t, 2, gw.sendCount())
}

// A mention in a thread arrives as both a message and an app_mention event in
// arbitrary order. When the message copy lands first and is dropped by the
// inactive-thread gate, it must not claim the dedup slot, or the app_mention
// copy is discarded as a duplicate and the mention is silently swallowed.
func TestHandleInbound_ThreadMentionSurvivesMessageCopyGateDrop(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = identOBO{}
	a.started.Store(true)

	messageCopy := slackInnerEvent{
		Type: evtMessage, Channel: "C1", TS: "9.100", ThreadTS: "T9",
		User: "U1", Text: "<@UBOT> /login",
	}
	a.handleInbound(t.Context(), messageCopy, "Ev1")

	mentionCopy := messageCopy
	mentionCopy.Type = evtAppMention
	a.handleInbound(t.Context(), mentionCopy, "Ev2")

	require.Equal(t, int32(1), srv.ephemerals.Load(),
		"the app_mention copy must be handled, not dropped as a duplicate")
}
