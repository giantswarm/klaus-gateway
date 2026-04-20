// Package observability wires OpenTelemetry (traces) and Prometheus metrics.
package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// TracerProviderShutdown is a function the caller runs at shutdown. It is
// returned from SetupTracing and is safe to call multiple times.
type TracerProviderShutdown func(context.Context) error

// SetupTracing installs a tracer provider that exports over OTLP gRPC when
// endpoint is non-empty. When endpoint is empty the tracer provider is a
// no-op and Shutdown is a no-op — traces are still created in the code path
// but go nowhere.
func SetupTracing(ctx context.Context, endpoint, serviceVersion string) (TracerProviderShutdown, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("klaus-gateway"),
			semconv.ServiceVersion(serviceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	var tp *sdktrace.TracerProvider
	if endpoint == "" {
		tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res))
	} else {
		exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		))
		if err != nil {
			return nil, fmt.Errorf("otlp grpc: %w", err)
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		)
	}

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(shutdownCtx context.Context) error { return tp.Shutdown(shutdownCtx) }, nil
}
