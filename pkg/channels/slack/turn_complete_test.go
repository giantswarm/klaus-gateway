package slack

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// installTracer gives the test a recording tracer provider, the way
// observability.SetupTracing does for the gateway: spans have real ids even
// with no exporter, so the records carry a trace_id.
func installTracer(t *testing.T) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
}

// fakeTurnRecorder keeps what the adapter reports to the turn metrics.
type fakeTurnRecorder struct {
	mu       sync.Mutex
	channels []string
	outcomes []string
	classes  []channels.FailureClass
	phases   []map[string]time.Duration
}

func (r *fakeTurnRecorder) RecordTurn(channel, outcome string, class channels.FailureClass, phases map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = append(r.channels, channel)
	r.outcomes = append(r.outcomes, outcome)
	r.classes = append(r.classes, class)
	r.phases = append(r.phases, phases)
}

// lastClass is the failure class of the last recorded turn.
func (r *fakeTurnRecorder) lastClass() channels.FailureClass {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.classes) == 0 {
		return channels.FailureNone
	}
	return r.classes[len(r.classes)-1]
}

func (r *fakeTurnRecorder) last() (string, string, map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.outcomes) == 0 {
		return "", "", nil
	}
	i := len(r.outcomes) - 1
	return r.channels[i], r.outcomes[i], r.phases[i]
}

func newRecordedAdapter(t *testing.T, gw channels.Gateway) (*Adapter, *recordingHandler, *fakeTurnRecorder) {
	t.Helper()
	installTracer(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(fake.Close)
	h := &recordingHandler{}
	rec := &fakeTurnRecorder{}
	a := &Adapter{
		Logger:       slog.New(h),
		Mode:         ModeEvents,
		Secrets:      Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
		APIBase:      fake.URL,
		DefaultAgent: "agent-1",
		OBO:          identOBO{},
		Turns:        rec,
	}
	require.NoError(t, a.Start(t.Context(), gw))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, h, rec
}

// Every dispatched turn ends with exactly one turn_complete record carrying
// the audit-join fields of turn_dispatch, the outcome, the task, the tool
// calls and streamed characters, and the phases as <phase>_ms measured from
// the moment Slack's event arrived; the turn metrics see the same outcome and
// phases, and the dispatch record carries the trace id the complete record
// carries.
func TestDispatch_EmitsTurnCompleteRecord(t *testing.T) {
	gw := &fakeGateway{deltas: []channels.OutboundDelta{
		{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: "get_pods", Kind: channels.ToolCall, CallID: "c1"}},
		{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: "get_pods", Kind: channels.ToolResult, CallID: "c1"}},
		{Content: "pong"},
		{Content: "!"},
		{Done: true},
	}}
	a, h, rec := newRecordedAdapter(t, gw)

	received := time.Now().Add(-300 * time.Millisecond)
	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T1", MessageID: "M1", Subject: "U1", Text: "hello", ReceivedAt: received}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))

	records := h.findAll("record", channels.RecordTurnComplete)
	require.Len(t, records, 1, "one turn, one turn_complete record")
	r := records[0]
	require.Equal(t, ChannelName, r["channel"])
	require.Equal(t, channels.OutcomeCompleted, r["outcome"])
	require.Equal(t, "agent-1", r["agent"])
	require.Equal(t, "U1", r["slack_user"])
	require.Equal(t, "D1", r["channel_id"])
	require.Equal(t, "T1", r["thread_id"])
	require.Equal(t, "M1", r["message_id"])
	require.Equal(t, int64(1), r["tool_calls"], "a call counts, its result does not")
	require.Equal(t, int64(5), r["streamed_chars"])
	for _, phase := range []string{"token_mint_ms", "roster_ms", "dispatch_ms", "first_text_ms", "final_flush_ms", "total_ms"} {
		require.Contains(t, r, phase, "phase %s missing from the record", phase)
	}
	require.GreaterOrEqual(t, r["total_ms"].(int64), int64(300), "the timeline starts when the event arrived, not at dispatch")
	require.LessOrEqual(t, r["dispatch_ms"].(int64), r["first_text_ms"].(int64))
	require.LessOrEqual(t, r["first_text_ms"].(int64), r["final_flush_ms"].(int64))
	require.NotEmpty(t, r["trace_id"])

	dispatch := h.find("record", "turn_dispatch")
	require.NotNil(t, dispatch)
	require.Equal(t, r["trace_id"], dispatch["trace_id"], "dispatch and complete records join on the trace id")
	require.Contains(t, dispatch, "dispatch_ms")

	channel, outcome, phases := rec.last()
	require.Equal(t, ChannelName, channel)
	require.Equal(t, channels.OutcomeCompleted, outcome)
	require.Contains(t, phases, channels.PhaseTotal)
	require.Contains(t, phases, channels.PhaseFirstText)
	require.Contains(t, phases, channels.PhaseTokenMint)
}

// A turn the controller refuses before its stream starts still leaves a
// record, with the send_failed outcome and the error; so does one whose
// stream fails, with failed; a turn paused on a prompt ends with
// input_required. A failed turn's record and metric carry its failure class,
// any other turn's none.
func TestDispatch_TurnCompleteOutcomes(t *testing.T) {
	const toolSet = `failed to extract tools from the tool set "mcp_tool_set": failed to list MCP tools: failed to init MCP session: calling "initialize": read: connection reset by peer`
	cases := []struct {
		name    string
		gw      *fakeGateway
		outcome string
		hasErr  bool
		class   channels.FailureClass
	}{
		{"send refused", &fakeGateway{sendErr: errors.New("instance busy")}, channels.OutcomeSendFailed, true, channels.FailureUnknown},
		{"send unreachable", &fakeGateway{sendErr: errors.New("rpc error: code = Unavailable desc = connection refused")}, channels.OutcomeSendFailed, true, channels.FailurePlatform},
		{"stream failed", &fakeGateway{deltas: []channels.OutboundDelta{{Content: "par"}, {Err: errors.New("task failed")}}}, channels.OutcomeFailed, true, channels.FailureUnknown},
		{"tool set failed", &fakeGateway{deltas: []channels.OutboundDelta{{Err: errors.New(toolSet)}}}, channels.OutcomeFailed, true, channels.FailureTools},
		{"prompt", &fakeGateway{deltas: []channels.OutboundDelta{{Kind: channels.DeltaPrompt, Content: "approve?", TaskID: "task-1"}}}, channels.OutcomeInputRequired, false, channels.FailureNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, rec := newRecordedAdapter(t, tc.gw)
			msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T-" + tc.name, MessageID: "M1", Subject: "U1", Text: "hello"}
			_ = a.dispatch(t.Context(), msg, "D1")
			records := h.findAll("record", channels.RecordTurnComplete)
			require.Len(t, records, 1)
			require.Equal(t, tc.outcome, records[0]["outcome"])
			if tc.hasErr {
				require.NotEmpty(t, records[0]["error"])
			} else {
				require.NotContains(t, records[0], "error")
			}
			if tc.class == channels.FailureNone {
				require.NotContains(t, records[0], "failure_class")
			} else {
				require.Equal(t, string(tc.class), records[0]["failure_class"])
			}
			_, outcome, _ := rec.last()
			require.Equal(t, tc.outcome, outcome)
			require.Equal(t, tc.class, rec.lastClass())
		})
	}
}

// A message that never becomes a turn — a signed-out user's message parked
// for the sign-in — leaves no turn_complete record and no metric.
func TestDispatch_ParkedMessageLeavesNoTurnRecord(t *testing.T) {
	gw := &fakeGateway{deltas: []channels.OutboundDelta{{Content: "never"}, {Done: true}}}
	a, h, rec := newRecordedAdapter(t, gw)
	a.OBO = deadLinkOBO{}

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T1", MessageID: "M1", Subject: "U1", Text: "hello"}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))
	require.Empty(t, h.findAll("record", channels.RecordTurnComplete))
	_, outcome, _ := rec.last()
	require.Empty(t, outcome)
	require.Equal(t, 0, gw.sends)
}

// findAll returns every collected record with the given key=value field.
func (h *recordingHandler) findAll(key string, value any) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, r := range h.records {
		if r[key] == value {
			out = append(out, r)
		}
	}
	return out
}
