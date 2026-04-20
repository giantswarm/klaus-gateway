package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the Prometheus collectors used by the gateway.
type Metrics struct {
	Registry        *prometheus.Registry
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
}

// NewMetrics builds and registers the default set of collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	reqs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "klaus_gateway",
		Name:      "requests_total",
		Help:      "Total HTTP requests on the public mux, labelled by route and status.",
	}, []string{"route", "method", "status"})

	dur := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "klaus_gateway",
		Name:      "request_duration_seconds",
		Help:      "HTTP request latency on the public mux, labelled by route and status.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"route", "method", "status"})

	reg.MustRegister(reqs, dur)

	return &Metrics{Registry: reg, RequestsTotal: reqs, RequestDuration: dur}
}

// Handler exposes the Prometheus /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// Middleware records a counter + histogram sample for every request. The
// route label is derived from the chi RouteContext when present, otherwise
// falls back to the URL path.
func (m *Metrics) Middleware(route string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			labels := prometheus.Labels{
				"route":  route,
				"method": r.Method,
				"status": strconv.Itoa(rw.status),
			}
			m.RequestsTotal.With(labels).Inc()
			m.RequestDuration.With(labels).Observe(time.Since(start).Seconds())
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to an underlying Flusher if the writer supports streaming.
// Keeps SSE happy when metrics middleware wraps a streaming handler.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
