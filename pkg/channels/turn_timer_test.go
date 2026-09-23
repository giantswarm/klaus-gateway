package channels

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordingHandler collects slog records so tests can assert on structured
// log output.
type recordingHandler struct {
	mu      sync.Mutex
	records []map[string]any
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, fields)
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) find(key string, value any) []map[string]any {
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

// fakeRecorder is a TurnRecorder that keeps what it was given.
type fakeRecorder struct {
	mu      sync.Mutex
	channel string
	outcome string
	class   FailureClass
	phases  map[string]time.Duration
	calls   int
}

func (r *fakeRecorder) RecordTurn(channel, outcome string, class FailureClass, phases map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channel, r.outcome, r.class, r.phases = channel, outcome, class, phases
	r.calls++
}

// installTestTracer makes spans real (recorded, exported in memory) for the
// duration of the test and returns the exporter to read them from.
func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return exporter
}

// A mark is recorded once (the first text delta stays the first), a span adds
// up over its calls, and the nil timer of a context without one takes every
// call without effect.
func TestTurnTimer_MarksOnceSpansAccumulate(t *testing.T) {
	timer := NewTurnTimer(time.Now().Add(-time.Second))
	timer.Mark(PhaseFirstText)
	first, ok := timer.Phase(PhaseFirstText)
	require.True(t, ok)
	require.GreaterOrEqual(t, first, time.Second, "a mark is measured from the turn's start, not from the timer's creation")
	time.Sleep(2 * time.Millisecond)
	timer.Mark(PhaseFirstText)
	again, _ := timer.Phase(PhaseFirstText)
	require.Equal(t, first, again, "a second mark of the same phase is ignored")

	end := timer.Span(PhaseTokenMint)
	time.Sleep(2 * time.Millisecond)
	end()
	end = timer.Span(PhaseTokenMint)
	time.Sleep(2 * time.Millisecond)
	end()
	mint, _ := timer.Phase(PhaseTokenMint)
	require.GreaterOrEqual(t, mint, 4*time.Millisecond, "spans of the same phase add up")

	timer.AddToolCall()
	timer.AddToolCall()
	timer.AddChars(5)
	timer.AddChars(0)
	tools, chars := timer.Counters()
	require.Equal(t, 2, tools)
	require.Equal(t, 5, chars)
	timer.SetTaskID("")
	require.Empty(t, timer.TaskID())
	timer.SetTaskID("task-1")
	require.Equal(t, "task-1", timer.TaskID())

	var none *TurnTimer
	none.Mark(PhaseDispatch)
	none.Span(PhaseRoster)()
	none.AddToolCall()
	none.AddChars(3)
	none.SetTaskID("x")
	require.Empty(t, none.TaskID())
	require.Nil(t, none.Phases())
	require.Nil(t, none.LogAttrs())
	require.Nil(t, TurnTimerFromContext(context.Background()))
	require.Equal(t, context.Background(), WithTurnTimer(context.Background(), nil))
}

// A turn begun on a context ends once: the record carries the outcome, the
// task, the counters, every phase as <phase>_ms and the trace id of the root
// span, the recorder sees the same phases, the span ends with the outcome, and
// a second completion (or an abandon after it) does nothing.
func TestBeginCompleteTurn_RecordMetricsAndSpan(t *testing.T) {
	exporter := installTestTracer(t)
	h := &recordingHandler{}
	rec := &fakeRecorder{}

	ctx, timer := BeginTurn(context.Background(), "slack", time.Now().Add(-500*time.Millisecond))
	require.Same(t, timer, TurnTimerFromContext(ctx))
	require.NotEmpty(t, timer.TraceID())
	timer.Mark(PhaseDispatch)
	timer.SetTaskID("task-9")
	timer.AddToolCall()
	timer.AddChars(12)
	timer.Span(PhaseTokenMint)()

	CompleteTurn(ctx, slog.New(h), rec, "slack", OutcomeCompleted, nil, "thread_id", "T1")
	CompleteTurn(ctx, slog.New(h), rec, "slack", OutcomeFailed, errors.New("late"), "thread_id", "T1")
	AbandonTurn(ctx, "too late")

	records := h.find("record", RecordTurnComplete)
	require.Len(t, records, 1, "one turn, one record")
	r := records[0]
	require.Equal(t, "slack", r["channel"])
	require.Equal(t, OutcomeCompleted, r["outcome"])
	require.Equal(t, "task-9", r["task_id"])
	require.Equal(t, int64(1), r["tool_calls"])
	require.Equal(t, int64(12), r["streamed_chars"])
	require.Equal(t, "T1", r["thread_id"])
	require.Equal(t, timer.TraceID(), r["trace_id"])
	require.NotContains(t, r, "error")
	require.NotContains(t, r, "failure_class", "a completed turn has no failure class")
	require.Equal(t, int64(0), r["retries"])
	require.Contains(t, r, "dispatch_ms")
	require.Contains(t, r, "token_mint_ms")
	require.Contains(t, r, "total_ms")
	require.GreaterOrEqual(t, r["total_ms"].(int64), int64(500), "total is measured from the turn's start")

	require.Equal(t, 1, rec.calls)
	require.Equal(t, "slack", rec.channel)
	require.Equal(t, OutcomeCompleted, rec.outcome)
	require.Equal(t, FailureNone, rec.class)
	require.Contains(t, rec.phases, PhaseTotal)
	require.Contains(t, rec.phases, PhaseDispatch)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "slack.turn", spans[0].Name)
	attrs := map[string]any{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	require.Equal(t, "slack", attrs["klaus_gateway.channel"])
	require.Equal(t, OutcomeCompleted, attrs["klaus_gateway.turn.outcome"])
	require.Equal(t, "task-9", attrs["a2a.task_id"])
	require.Equal(t, int64(1), attrs["klaus_gateway.turn.tool_calls"])
	require.Equal(t, timer.TraceID(), spans[0].SpanContext.TraceID().String())
}

// A failed turn's record and span carry the error; a turn that never ran
// (abandoned) ends its span without a record; a context without a turn takes
// both calls without effect.
func TestCompleteTurn_ErrorAbandonAndNoTimer(t *testing.T) {
	exporter := installTestTracer(t)
	h := &recordingHandler{}
	rec := &fakeRecorder{}

	ctx, _ := BeginTurn(context.Background(), "slack", time.Time{})
	CompleteTurn(ctx, slog.New(h), rec, "slack", OutcomeSendFailed, errors.New("controller refused"))
	records := h.find("record", RecordTurnComplete)
	require.Len(t, records, 1)
	require.Equal(t, OutcomeSendFailed, records[0]["outcome"])
	require.Equal(t, "controller refused", records[0]["error"])
	require.Equal(t, OutcomeSendFailed, rec.outcome)

	actx, _ := BeginTurn(context.Background(), "slack", time.Time{})
	AbandonTurn(actx, "parked")
	CompleteTurn(actx, slog.New(h), rec, "slack", OutcomeCompleted, nil)
	require.Len(t, h.find("record", RecordTurnComplete), 1, "an abandoned turn leaves no record, also not on a later complete")
	require.Equal(t, 1, rec.calls)

	spans := exporter.GetSpans()
	require.Len(t, spans, 2)
	byOutcome := map[string]string{}
	for _, s := range spans {
		attrs := map[string]any{}
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.AsInterface()
		}
		byOutcome[attrs["klaus_gateway.turn.outcome"].(string)] = s.Name
	}
	require.Contains(t, byOutcome, OutcomeSendFailed)
	require.Contains(t, byOutcome, "abandoned")

	CompleteTurn(context.Background(), slog.New(h), rec, "slack", OutcomeCompleted, nil)
	AbandonTurn(context.Background(), "nothing")
	require.Equal(t, 1, rec.calls)
	require.Len(t, h.find("record", RecordTurnComplete), 1)
}

// A turn sent a second time counts the retry, and its task_done and
// stream_end are the second attempt's; a failed turn's record, metric and
// span carry the class of its failure.
func TestCompleteTurn_RetryAndFailureClass(t *testing.T) {
	exporter := installTestTracer(t)
	h := &recordingHandler{}
	rec := &fakeRecorder{}

	ctx, timer := BeginTurn(context.Background(), "slack", time.Time{})
	timer.Mark(PhaseFirstEvent)
	timer.Mark(PhaseTaskDone)
	timer.Mark(PhaseStreamEnd)
	timer.Retry()
	for _, phase := range []string{PhaseTaskDone, PhaseStreamEnd} {
		_, ok := timer.Phase(phase)
		require.False(t, ok, "the failed attempt's %s is forgotten", phase)
	}
	_, ok := timer.Phase(PhaseFirstEvent)
	require.True(t, ok, "the controller answered the first attempt")
	timer.Mark(PhaseTaskDone)
	_, ok = timer.Phase(PhaseTaskDone)
	require.True(t, ok, "the second attempt marks its own end")

	CompleteTurn(ctx, slog.New(h), rec, "slack", OutcomeFailed, errors.New(`failed to extract tools from the tool set "mcp_tool_set": failed to list MCP tools: failed to init MCP session: read: connection reset by peer`))
	records := h.find("record", RecordTurnComplete)
	require.Len(t, records, 1)
	require.Equal(t, string(FailureTools), records[0]["failure_class"])
	require.Equal(t, int64(1), records[0]["retries"])
	require.Equal(t, FailureTools, rec.class)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := map[string]any{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	require.Equal(t, string(FailureTools), attrs["klaus_gateway.turn.failure_class"])
	require.Equal(t, int64(1), attrs["klaus_gateway.turn.retries"])
}
