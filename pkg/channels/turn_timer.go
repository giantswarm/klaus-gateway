package channels

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The phases of a turn's timeline. A mark is the time since the turn started
// (the channel received the message) at which the phase was reached; a span
// is how long a step took. Both are reported as `<phase>_ms` on the
// turn_complete record and as `klaus_gateway_turn_phase_seconds{phase}`.
const (
	// PhaseTokenMint is the time spent minting the person's muster token
	// (musterlink.TokenFor: a cache hit is ~0, a refresh a round trip to
	// muster's token endpoint). A span, summed over the turn's mints.
	PhaseTokenMint = "token_mint"
	// PhaseRoster is the time spent resolving the turn's agent: the roster
	// lookup and the conversation's binding. A span.
	PhaseRoster = "roster"
	// PhaseCreateInstance is the controller's CreateAgentInstance on a
	// thread's first turn. A span; absent on a follow-up.
	PhaseCreateInstance = "create_instance"
	// PhaseDispatch marks the turn_dispatch record: admission, identity and
	// agent resolved, the completion about to be sent.
	PhaseDispatch = "dispatch"
	// PhaseFirstEvent marks the first A2A event of the stream (the task the
	// controller placed).
	PhaseFirstEvent = "first_event"
	// PhaseFirstText marks the first text delta of the answer.
	PhaseFirstText = "first_text"
	// PhaseTaskDone marks the task's terminal (or waiting) state: the agent
	// stopped working.
	PhaseTaskDone = "task_done"
	// PhaseStreamEnd marks the end of the A2A stream (the controller closed
	// it).
	PhaseStreamEnd = "stream_end"
	// PhaseFinalFlush marks the last edit of the answer in the channel: the
	// person has everything.
	PhaseFinalFlush = "final_flush"
	// PhaseTotal marks the emission of the turn_complete record.
	PhaseTotal = "total"
)

// The outcomes a turn ends with (`klaus_gateway_turn_total{outcome}`).
const (
	OutcomeCompleted     = "completed"      // the task completed and the answer landed
	OutcomeInputRequired = "input_required" // the task paused on a prompt to the person
	OutcomeCanceled      = "canceled"       // the person stopped the turn (/stop, the stop button, a closed stream)
	OutcomeShutdown      = "shutdown"       // the gateway's shutdown cut the turn short
	OutcomeTimeout       = "timeout"        // the turn ran into the gateway's turn deadline
	OutcomeFailed        = "failed"         // the task failed, or the stream broke
	OutcomeRenderFailed  = "render_failed"  // the task completed but the channel refused (part of) the answer
	OutcomeResolveFailed = "resolve_failed" // the turn died before it was sent: the agent did not resolve
	OutcomeSendFailed    = "send_failed"    // the turn died before it was sent: the controller refused it
)

// RecordTurnComplete is the `record` value of the log line every turn ends
// with.
const RecordTurnComplete = "turn_complete"

// TurnTimer is the telemetry handle of one turn: when it started, which phase
// was reached when, how long the steps took, the counters the turn_complete
// record carries, and the turn's root span. It travels on the context from
// the channel adapter that starts the turn (BeginTurn) through the facade and
// the A2A client, so every layer marks its own phase; CompleteTurn writes the
// record and the metrics and ends the span. Methods are safe on a nil
// receiver (a context without a timer) and for concurrent use.
type TurnTimer struct {
	start time.Time
	span  trace.Span
	ended atomic.Bool

	mu        sync.Mutex
	phases    map[string]time.Duration
	taskID    string
	toolCalls int
	chars     int
}

// NewTurnTimer starts a timeline at start (the moment the channel received
// the message; the zero time means now).
func NewTurnTimer(start time.Time) *TurnTimer {
	if start.IsZero() {
		start = time.Now()
	}
	return &TurnTimer{start: start, phases: map[string]time.Duration{}}
}

// Start is when the turn started.
func (t *TurnTimer) Start() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.start
}

// Mark records that phase was reached now, once: a later Mark of the same
// phase is ignored, so the first text delta stays the first.
func (t *TurnTimer) Mark(phase string) {
	if t == nil {
		return
	}
	since := time.Since(t.start)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.phases[phase]; !ok {
		t.phases[phase] = since
	}
}

// Span times a step: call it when the step starts and the returned func when
// it ends. A step that runs several times in a turn (a second token mint for
// the thread's initiator) adds up.
func (t *TurnTimer) Span(phase string) func() {
	if t == nil {
		return func() {}
	}
	began := time.Now()
	return func() {
		took := time.Since(began)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.phases[phase] += took
	}
}

// SetTaskID records the A2A task the turn ran as, once it is known.
func (t *TurnTimer) SetTaskID(id string) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	t.taskID = id
	t.mu.Unlock()
	if t.span != nil {
		t.span.SetAttributes(attribute.String("a2a.task_id", id))
	}
}

// TaskID is the A2A task the turn ran as, or "" before the controller named
// it.
func (t *TurnTimer) TaskID() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.taskID
}

// AddToolCall counts one tool call the agent made during the turn.
func (t *TurnTimer) AddToolCall() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toolCalls++
}

// AddChars counts n characters of answer text streamed to the channel.
func (t *TurnTimer) AddChars(n int) {
	if t == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chars += n
}

// Counters reports the tool calls made and the characters streamed so far.
func (t *TurnTimer) Counters() (toolCalls, chars int) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.toolCalls, t.chars
}

// Phases is a copy of the recorded phases.
func (t *TurnTimer) Phases() map[string]time.Duration {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]time.Duration, len(t.phases))
	for k, v := range t.phases {
		out[k] = v
	}
	return out
}

// Phase is the recorded value of one phase and whether it was recorded.
func (t *TurnTimer) Phase(phase string) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.phases[phase]
	return d, ok
}

// TraceID is the hex trace id of the turn's span, or "" without a valid one.
func (t *TurnTimer) TraceID() string {
	if t == nil || t.span == nil {
		return ""
	}
	if sc := t.span.SpanContext(); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

// LogAttrs renders the phases as `<phase>_ms` slog attributes in a stable
// order, so a turn_complete record reads the same way every time.
func (t *TurnTimer) LogAttrs() []any {
	if t == nil {
		return nil
	}
	phases := t.Phases()
	names := make([]string, 0, len(phases))
	for name := range phases {
		names = append(names, name)
	}
	sort.Strings(names)
	attrs := make([]any, 0, len(names))
	for _, name := range names {
		attrs = append(attrs, slog.Int64(name+"_ms", phases[name].Milliseconds()))
	}
	return attrs
}

// TurnRecorder takes the outcome and the phases of a finished turn; the
// observability package implements it with a per-outcome counter and
// per-phase histograms. Nil is fine everywhere a recorder is optional.
type TurnRecorder interface {
	RecordTurn(channel, outcome string, phases map[string]time.Duration)
}

type turnTimerKey struct{}

// WithTurnTimer attaches the turn's timeline to ctx, for the layers below the
// channel adapter to mark their phases.
func WithTurnTimer(ctx context.Context, t *TurnTimer) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, turnTimerKey{}, t)
}

// TurnTimerFromContext is the turn's timeline, or nil (whose methods are
// no-ops) when ctx carries none.
func TurnTimerFromContext(ctx context.Context) *TurnTimer {
	t, _ := ctx.Value(turnTimerKey{}).(*TurnTimer)
	return t
}

// tracerName names the gateway's spans in the trace backend.
const tracerName = "github.com/giantswarm/klaus-gateway/pkg/channels"

// BeginTurn opens a turn's telemetry: the timeline from start (zero = now)
// and the root span `<channel>.turn` under which every call the turn makes —
// the A2A stream to the controller, the channel's API calls, a token refresh —
// is traced, so the controller's SendStreamingMessage trace hangs off the
// gateway's. The returned context carries both.
func BeginTurn(ctx context.Context, channel string, start time.Time, attrs ...attribute.KeyValue) (context.Context, *TurnTimer) {
	t := NewTurnTimer(start)
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(append([]attribute.KeyValue{attribute.String("klaus_gateway.channel", channel)}, attrs...)...),
	}
	if !start.IsZero() {
		opts = append(opts, trace.WithTimestamp(start))
	}
	ctx, t.span = otel.Tracer(tracerName).Start(ctx, channel+".turn", opts...)
	return WithTurnTimer(ctx, t), t
}

// CompleteTurn ends the turn on ctx: it writes the turn_complete record
// (the outcome, the task, the counters, every phase as `<phase>_ms` and the
// trace id, plus the channel's own attrs), feeds the recorder and ends the
// span. Idempotent; a nil recorder records nothing, a nil logger uses the
// default. Without a timer on ctx nothing happens.
func CompleteTurn(ctx context.Context, logger *slog.Logger, rec TurnRecorder, channel, outcome string, err error, attrs ...any) {
	t := TurnTimerFromContext(ctx)
	if t == nil || t.ended.Swap(true) {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	t.Mark(PhaseTotal)
	phases := t.Phases()
	toolCalls, chars := t.Counters()
	fields := []any{
		"record", RecordTurnComplete,
		"channel", channel,
		"outcome", outcome,
		"task_id", t.TaskID(),
		"tool_calls", toolCalls,
		"streamed_chars", chars,
	}
	if id := t.TraceID(); id != "" {
		fields = append(fields, "trace_id", id)
	}
	if err != nil {
		fields = append(fields, "error", err.Error())
	}
	fields = append(fields, attrs...)
	fields = append(fields, t.LogAttrs()...)
	logger.Info(channel+": turn complete", fields...)
	if rec != nil {
		rec.RecordTurn(channel, outcome, phases)
	}
	if t.span != nil {
		t.span.SetAttributes(
			attribute.String("klaus_gateway.turn.outcome", outcome),
			attribute.Int("klaus_gateway.turn.tool_calls", toolCalls),
			attribute.Int("klaus_gateway.turn.streamed_chars", chars),
		)
		if err != nil {
			t.span.RecordError(err)
			t.span.SetStatus(codes.Error, outcome)
		}
		t.span.End()
	}
}

// AbandonTurn ends the span of a turn that never ran (the message was parked
// for a sign-in, the thread was busy, a command consumed it) without a
// record: it was not a turn. Idempotent, and a no-op after CompleteTurn.
func AbandonTurn(ctx context.Context, reason string) {
	t := TurnTimerFromContext(ctx)
	if t == nil || t.ended.Swap(true) {
		return
	}
	if t.span != nil {
		t.span.SetAttributes(attribute.String("klaus_gateway.turn.outcome", "abandoned"), attribute.String("klaus_gateway.turn.abandoned", reason))
		t.span.End()
	}
}
