package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// RecordTurn counts the turn under its channel and outcome and observes each
// phase in its own histogram series; the collectors are on the registry
// /metrics serves.
func TestMetrics_RecordTurn(t *testing.T) {
	m := NewMetrics()
	m.RecordTurn("slack", "completed", map[string]time.Duration{
		"total":      4200 * time.Millisecond,
		"first_text": 1500 * time.Millisecond,
	})
	m.RecordTurn("slack", "completed", map[string]time.Duration{"total": 3 * time.Second})
	m.RecordTurn("web", "failed", map[string]time.Duration{"total": time.Second})

	require.Equal(t, float64(2), testutil.ToFloat64(m.TurnsTotal.WithLabelValues("slack", "completed")))
	require.Equal(t, float64(1), testutil.ToFloat64(m.TurnsTotal.WithLabelValues("web", "failed")))
	require.Equal(t, 3, testutil.CollectAndCount(m.TurnPhase, "klaus_gateway_turn_phase_seconds"), "one series per (channel, phase): slack/total, slack/first_text, web/total")

	families, err := m.Registry.Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	require.True(t, names["klaus_gateway_turn_total"])
	require.True(t, names["klaus_gateway_turn_phase_seconds"])
}

// RecordSlackStream counts each streamed reply under its lifecycle event, on
// the registry /metrics serves.
func TestMetrics_RecordSlackStream(t *testing.T) {
	m := NewMetrics()
	m.RecordSlackStream("started")
	m.RecordSlackStream("started")
	m.RecordSlackStream("stopped_by_user")

	require.Equal(t, float64(2), testutil.ToFloat64(m.SlackStreamsTotal.WithLabelValues("started")))
	require.Equal(t, float64(1), testutil.ToFloat64(m.SlackStreamsTotal.WithLabelValues("stopped_by_user")))
	require.Equal(t, 2, testutil.CollectAndCount(m.SlackStreamsTotal, "klaus_gateway_slack_stream_total"))
}

// RecordSlackRateLimit counts each rate-limited Web API call under its method
// and what the client did about it, on the registry /metrics serves.
func TestMetrics_RecordSlackRateLimit(t *testing.T) {
	m := NewMetrics()
	m.RecordSlackRateLimit("chat.appendStream", "retried")
	m.RecordSlackRateLimit("chat.appendStream", "retried")
	m.RecordSlackRateLimit("chat.appendStream", "exhausted")
	m.RecordSlackRateLimit("chat.startStream", "retried")

	require.Equal(t, float64(2), testutil.ToFloat64(m.SlackRateLimitedTotal.WithLabelValues("chat.appendStream", "retried")))
	require.Equal(t, float64(1), testutil.ToFloat64(m.SlackRateLimitedTotal.WithLabelValues("chat.appendStream", "exhausted")))
	require.Equal(t, float64(1), testutil.ToFloat64(m.SlackRateLimitedTotal.WithLabelValues("chat.startStream", "retried")))
	require.Equal(t, 3, testutil.CollectAndCount(m.SlackRateLimitedTotal, "klaus_gateway_slack_rate_limited_total"), "one series per (method, outcome)")
}

// ParseHeaders reads the OTEL_EXPORTER_OTLP_HEADERS form and refuses an entry
// that is not key=value.
func TestParseHeaders(t *testing.T) {
	h, err := ParseHeaders(" X-Scope-OrgID=giantswarm, Authorization=Bearer x ")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"X-Scope-OrgID": "giantswarm", "Authorization": "Bearer x"}, h)

	h, err = ParseHeaders("")
	require.NoError(t, err)
	require.Nil(t, h)

	_, err = ParseHeaders("no-equals")
	require.Error(t, err)
}

// exporterOptions keeps a URL endpoint's scheme (plaintext http, TLS https)
// and treats a bare host:port as plaintext; the option list is what the
// exporter is built from, so a nil-safe build is all that can be asserted
// without a collector.
func TestExporterOptions(t *testing.T) {
	require.Len(t, exporterOptions(TracingConfig{Endpoint: "http://otlp-gateway.kube-system.svc:4317"}), 1)
	require.Len(t, exporterOptions(TracingConfig{Endpoint: "otlp-gateway.kube-system.svc:4317"}), 2, "a bare host:port adds the insecure option")
	require.Len(t, exporterOptions(TracingConfig{Endpoint: "http://c:4317", Headers: map[string]string{"X-Scope-OrgID": "giantswarm"}}), 2)
}
