package slack

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// slowRowWrites answers every thread row write late, as a store some way off
// does, so whatever is not ordered after the write is seen before it lands.
type slowRowWrites struct{ *fakeGateway }

func (g slowRowWrites) UpdateThreadRecord(ctx context.Context, channel, channelID, threadID string, mutate func(*store.Entry, bool) bool) error {
	select {
	case <-time.After(200 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.fakeGateway.UpdateThreadRecord(ctx, channel, channelID, threadID, mutate)
}

// A Socket Mode event is acknowledged once its message is in the thread's
// inbox: Slack sends nothing again after the ack, so the row must hold the
// message when the ack goes out.
func TestSocketMode_AcksAnEventAfterItsMessageIsQueued(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.2"})
	}))
	t.Cleanup(api.Close)
	gw := slowRowWrites{&fakeGateway{Facade: newMemoryRecorder(), deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}}
	a := &Adapter{
		APIBase:      api.URL,
		Secrets:      Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		DefaultAgent: "worker",
		Logger:       slog.New(slog.DiscardHandler),
	}
	require.NoError(t, a.Start(t.Context(), gw))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	require.NoError(t, a.bindThreadAgent(t.Context(), "D1", "100.000", "worker"))
	// A turn holds the thread, so the message waits in the inbox.
	require.True(t, a.acquireThread("100.000"))
	t.Cleanup(func() { a.releaseThread("100.000") })

	queuedAtAck := make(chan bool, 1)
	slack := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		envelope := `{"envelope_id":"env-1","type":"events_api","payload":{"event_id":"Ev1","event":{"type":"message","channel_type":"im","user":"U1","text":"and the pods?","channel":"D1","ts":"100.001","thread_ts":"100.000"}}}`
		if err := websocket.Message.Send(ws, envelope); err != nil { //nolint:staticcheck
			queuedAtAck <- false
			return
		}
		var ack string
		if err := websocket.Message.Receive(ws, &ack); err != nil || !strings.Contains(ack, "env-1") { //nolint:staticcheck
			queuedAtAck <- false
			return
		}
		row, _, err := gw.ThreadRecord(context.Background(), ChannelName, "D1", "100.000")
		queuedAtAck <- err == nil && strings.Contains(string(row.Held), "and the pods?")
	}))
	t.Cleanup(slack.Close)
	ws, err := websocket.Dial("ws"+strings.TrimPrefix(slack.URL, "http"), "", slack.URL) //nolint:staticcheck
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	go (&socketModeClient{adapter: a, logger: a.Logger}).readLoop(t.Context(), ws)

	select {
	case queued := <-queuedAtAck:
		require.True(t, queued, "the message is in the thread's inbox when the envelope is acknowledged")
	case <-time.After(flowWait):
		t.Fatal("the envelope was not acknowledged")
	}
}

// A Socket Mode events_api payload carries the same event_id as the Events API
// callback. Parsing it is what lets handleInbound dedup Socket Mode deliveries;
// before this, the id was dropped and only the Events API path deduped (#131).
func TestSocketModePayloadCarriesEventID(t *testing.T) {
	raw := []byte(`{
		"event_id":"Ev-socket",
		"event":{"type":"app_mention","user":"U1","text":"<@BOT> hi","channel":"C1","ts":"1.2"}
	}`)

	var payload smEventPayload
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, "Ev-socket", payload.EventID)
	require.Equal(t, "app_mention", payload.Event.Type)
}
