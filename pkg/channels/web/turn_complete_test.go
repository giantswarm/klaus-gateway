package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/channels/web"
)

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

func (h *recordingHandler) turnRecords() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, r := range h.records {
		if r["record"] == channels.RecordTurnComplete {
			out = append(out, r)
		}
	}
	return out
}

type fakeTurnRecorder struct {
	mu       sync.Mutex
	outcomes []string
}

func (r *fakeTurnRecorder) RecordTurn(_, outcome string, _ map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

// A web turn ends with one turn_complete record naming the channel, the
// outcome, the thread and the phases from the request's arrival to the last
// SSE frame; the recorder sees the outcome. A refused send is recorded too.
func TestPostMessages_EmitsTurnCompleteRecord(t *testing.T) {
	// A recording tracer provider, the way observability.SetupTracing installs
	// one for the gateway: spans have real ids even with no exporter.
	tp := sdktrace.NewTracerProvider()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	h := &recordingHandler{}
	rec := &fakeTurnRecorder{}
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: "t", Kind: channels.ToolCall}},
		{Content: "hel"}, {Content: "lo"}, {Done: true},
	}}
	a := &web.Adapter{Logger: slog.New(h), Turns: rec, DefaultAgent: "agent-1"}
	require.NoError(t, a.Start(t.Context(), gw))
	r := chi.NewRouter()
	a.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(`{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi"}`))
	require.NoError(t, err)
	// The record is written when the handler returns, which is when the
	// stream ends: read it to EOF before looking.
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	records := h.turnRecords()
	require.Len(t, records, 1)
	rr := records[0]
	require.Equal(t, "web", rr["channel"])
	require.Equal(t, channels.OutcomeCompleted, rr["outcome"])
	require.Equal(t, "t1", rr["thread_id"])
	require.Equal(t, "agent-1", rr["agent"])
	require.Equal(t, int64(1), rr["tool_calls"])
	require.Equal(t, int64(5), rr["streamed_chars"])
	require.Contains(t, rr, "first_text_ms")
	require.Contains(t, rr, "final_flush_ms")
	require.Contains(t, rr, "total_ms")
	require.NotEmpty(t, rr["trace_id"])
	require.Equal(t, []string{channels.OutcomeCompleted}, rec.outcomes)

	gw.sendErr = context.DeadlineExceeded
	resp, err = http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(`{"channelId":"c1","userId":"u1","threadId":"t2","text":"hi"}`))
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	records = h.turnRecords()
	require.Len(t, records, 2)
	require.Equal(t, channels.OutcomeSendFailed, records[1]["outcome"])
	require.NotEmpty(t, records[1]["error"])
}
