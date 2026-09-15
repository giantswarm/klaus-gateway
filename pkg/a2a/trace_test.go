package a2a_test

import (
	"context"
	"strings"
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// The A2A stream carries the caller's trace context to the controller as the
// W3C traceparent, with the turn's trace id, so the controller's
// SendStreamingMessage trace continues the gateway's; the call itself is a
// client span under the caller's.
func TestClient_Stream_PropagatesTraceContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})

	f := readyFake(t)
	info := a2apkg.TaskInfo{TaskID: "task-1", ContextID: "ctx-1"}
	f.events = []a2apkg.Event{a2apkg.NewStatusUpdateEvent(info, a2apkg.TaskStateCompleted, nil)}
	client := f.serve(t, pkga2a.Config{})

	ctx, turn := tp.Tracer("test").Start(asUser(t.Context(), userToken), "slack.turn")
	for _, err := range client.Stream(ctx, instanceID, a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("ping"))) {
		require.NoError(t, err)
	}
	turn.End()

	md := f.lastMD("SendStreamingMessage")
	traceparent := md.Get("traceparent")
	require.Len(t, traceparent, 1, "the turn's trace context rides on the A2A call")
	require.Contains(t, traceparent[0], turn.SpanContext().TraceID().String())
	require.True(t, strings.HasSuffix(traceparent[0], "-01"), "the sampled flag travels: %s", traceparent[0])

	var clientSpan bool
	for _, s := range exporter.GetSpans() {
		if strings.HasSuffix(s.Name, "A2AService/SendStreamingMessage") && s.SpanContext.TraceID() == turn.SpanContext().TraceID() {
			clientSpan = true
			require.Equal(t, turn.SpanContext().SpanID(), s.Parent.SpanID(), "the RPC span hangs off the turn's span")
		}
	}
	require.True(t, clientSpan, "the A2A call is a client span in the turn's trace")
}
