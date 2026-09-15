// Package observability wires OpenTelemetry (traces) and Prometheus metrics.
package observability

import (
	"context"
	"fmt"
	"strings"
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

// TracingConfig configures the OTLP trace export.
type TracingConfig struct {
	// Endpoint is the OTLP gRPC collector: a URL (`http://otlp-gateway.kube-system.svc:4317`,
	// whose scheme decides TLS) or a bare host:port (plaintext). Empty exports
	// nothing.
	Endpoint string
	// Headers ride on every export request (`X-Scope-OrgID` selects the
	// tenant on a multi-tenant gateway).
	Headers map[string]string
	// ServiceVersion is the gateway's version on the resource.
	ServiceVersion string
}

// SetupTracing installs the tracer provider. With an endpoint spans are
// exported over OTLP gRPC in batches; without one the provider still
// records spans — every turn has a valid trace id in its log records, and the
// A2A calls carry a traceparent — but exports nowhere. It also installs the
// W3C trace-context and baggage propagators the gRPC and HTTP clients inject.
func SetupTracing(ctx context.Context, cfg TracingConfig) (TracerProviderShutdown, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("klaus-gateway"),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	var tp *sdktrace.TracerProvider
	if cfg.Endpoint == "" {
		tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res))
	} else {
		exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(exporterOptions(cfg)...))
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

// exporterOptions maps the config onto the OTLP gRPC client: a URL endpoint
// keeps its scheme (http is plaintext, https TLS), a bare host:port is
// plaintext, and the headers ride on every export.
func exporterOptions(cfg TracingConfig) []otlptracegrpc.Option {
	var opts []otlptracegrpc.Option
	if strings.Contains(cfg.Endpoint, "://") {
		opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
	} else {
		opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint), otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}
	return opts
}

// ParseHeaders reads the `key=value,key=value` form of OTEL_EXPORTER_OTLP_HEADERS.
// An empty string is no headers; an entry without `=` is an error.
func ParseHeaders(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	headers := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("otlp headers: %q is not key=value", pair)
		}
		headers[key] = strings.TrimSpace(value)
	}
	return headers, nil
}
