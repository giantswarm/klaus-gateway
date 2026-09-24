package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// metricNamespace prefixes every metric name the gateway exposes.
const metricNamespace = "klaus_gateway"

const (
	labelRoute   = "route"
	labelMethod  = "method"
	labelStatus  = "status"
	labelChannel = "channel"
	labelOutcome = "outcome"
	labelClass   = "failure_class"
	labelPhase   = "phase"
	labelEvent   = "event"
)

// turnPhaseBuckets spans a turn's phases: a cache hit in single-digit
// milliseconds, a token refresh or an instance create in seconds, a
// tool-calling turn in minutes.
var turnPhaseBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300}

// Metrics holds the Prometheus collectors used by the gateway.
type Metrics struct {
	Registry        *prometheus.Registry
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	// TurnsTotal counts the channel turns by their outcome; TurnPhase is one
	// histogram per phase of a turn's timeline (channels.Phase*), fed by
	// RecordTurn when a turn ends.
	TurnsTotal *prometheus.CounterVec
	TurnPhase  *prometheus.HistogramVec
	// SlackStreamsTotal counts the lifecycle events of Slack's streamed
	// replies, fed by RecordSlackStream. chat.startStream and chat.stopStream
	// are tier-2 methods (about 20 calls a minute for the whole app), so the
	// started/stopped rates are what says how close a workspace is to that
	// ceiling.
	SlackStreamsTotal *prometheus.CounterVec
	// SlackRateLimitedTotal counts the Slack Web API calls answered with HTTP
	// 429, by method and by what the client did about it, fed by
	// RecordSlackRateLimit. A 429 the client waits out and retries leaves no
	// other trace, so this counter is what says whether a workspace is being
	// throttled at all.
	SlackRateLimitedTotal *prometheus.CounterVec
}

// NewMetrics builds and registers the default set of collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	reqs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "requests_total",
		Help:      "Total HTTP requests on the public mux, labelled by route and status.",
	}, []string{labelRoute, labelMethod, labelStatus})

	dur := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "request_duration_seconds",
		Help:      "HTTP request latency on the public mux, labelled by route and status.",
		Buckets:   prometheus.DefBuckets,
	}, []string{labelRoute, labelMethod, labelStatus})

	turns := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "turn_total",
		Help:      "Channel turns that ended, labelled by channel, outcome (completed, input_required, canceled, shutdown, timeout, failed, render_failed, send_failed) and, for a failed or send_failed turn, failure_class (tools, platform, model, policy, unknown).",
	}, []string{labelChannel, labelOutcome, labelClass})

	phase := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "turn_phase_seconds",
		Help:      "Phases of a channel turn's timeline, labelled by channel and phase: marks since the message arrived (dispatch, first_event, first_text, task_done, stream_end, final_flush, total) and the duration of steps (token_mint, roster, create_instance).",
		Buckets:   turnPhaseBuckets,
	}, []string{labelChannel, labelPhase})

	streams := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "slack_stream_total",
		Help:      "Slack streamed replies, labelled by lifecycle event (started, stopped, stopped_by_user, recovered).",
	}, []string{labelEvent})

	rateLimited := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "slack_rate_limited_total",
		Help:      "Slack Web API calls answered with HTTP 429, labelled by method and by what the client did about it (retried, exhausted).",
	}, []string{labelMethod, labelOutcome})

	reg.MustRegister(reqs, dur, turns, phase, streams, rateLimited)

	return &Metrics{
		Registry:              reg,
		RequestsTotal:         reqs,
		RequestDuration:       dur,
		TurnsTotal:            turns,
		TurnPhase:             phase,
		SlackStreamsTotal:     streams,
		SlackRateLimitedTotal: rateLimited,
	}
}

// RecordTurn counts a finished turn under its outcome and failure class and
// observes each of its phases. It implements channels.TurnRecorder.
func (m *Metrics) RecordTurn(channel, outcome string, class channels.FailureClass, phases map[string]time.Duration) {
	m.TurnsTotal.WithLabelValues(channel, outcome, string(class)).Inc()
	for phase, d := range phases {
		m.TurnPhase.WithLabelValues(channel, phase).Observe(d.Seconds())
	}
}

// RecordSlackStream counts one lifecycle event of a Slack streamed reply. It
// implements the Slack adapter's StreamRecorder.
func (m *Metrics) RecordSlackStream(event string) {
	m.SlackStreamsTotal.WithLabelValues(event).Inc()
}

// RecordSlackRateLimit counts one Slack Web API call Slack answered with a
// 429, under the method and the outcome (retried, exhausted). It implements
// the Slack adapter's RateLimitRecorder.
func (m *Metrics) RecordSlackRateLimit(method, outcome string) {
	m.SlackRateLimitedTotal.WithLabelValues(method, outcome).Inc()
}

// Handler exposes the Prometheus /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// Middleware records a counter + histogram sample for every request, labelled
// with route (the name of the mux it wraps), the method and the status.
func (m *Metrics) Middleware(route string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			labels := prometheus.Labels{
				labelRoute:  route,
				labelMethod: r.Method,
				labelStatus: strconv.Itoa(rw.status),
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
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
