package refusals_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/klaus-gateway/pkg/refusals"
)

type fakeSource struct{ series []refusals.Series }

func (f *fakeSource) Refusals(context.Context) ([]refusals.Series, error) { return f.series, nil }

type fakeSessions map[string][2]string

func (f fakeSessions) SessionAgent(_ context.Context, id string) (string, string, error) {
	if a, ok := f[id]; ok {
		return a[0], a[1], nil
	}
	return "", "", refusals.ErrNotSession
}

type fakeAgents map[string]refusals.Agent

func (f fakeAgents) Agent(_ context.Context, namespace, name string) (refusals.Agent, error) {
	return f[namespace+"/"+name], nil
}

type fakeNotifier struct {
	noted []channels.EgressRefusal
	err   error
}

func (f *fakeNotifier) NoteRefusal(_ context.Context, r channels.EgressRefusal) error {
	if f.err != nil {
		return f.err
	}
	f.noted = append(f.noted, r)
	return nil
}

func series(actor, host string, value float64) refusals.Series {
	return refusals.Series{Source: "10.0.0.1", Atespace: "org-otter", Actor: actor, Scheme: "https", Host: host, Port: 443, Value: value}
}

func watcher(src *fakeSource, n *fakeNotifier, outcomes *[]string) *refusals.Watcher {
	return &refusals.Watcher{
		Source: src,
		Sessions: fakeSessions{
			"s1": {"org-otter", "otter-coder"},
			"s2": {"org-otter", "otter-coder"},
			"s3": {"org-otter", "orphan"},
		},
		Agents: fakeAgents{
			"org-otter/otter-coder": {DisplayName: "Otter Coder", Creator: "badger@example.com"},
		},
		Notifier: n,
		AgentURL: "https://portal.example.com/agents/{namespace}/{name}",
		Record:   func(o string) { *outcomes = append(*outcomes, o) },
		Now:      func() time.Time { return time.Date(2026, 10, 19, 10, 42, 0, 0, time.UTC) },
	}
}

func TestWatcher_SumsAgentAndHostAcrossSessionsAndCountsIncreases(t *testing.T) {
	src := &fakeSource{series: []refusals.Series{
		series("session-s1", "proxy.golang.org", 2),
		series("session-s2", "proxy.golang.org", 1),
		series("session-s1", "sum.golang.org", 1),
	}}
	n := &fakeNotifier{}
	var outcomes []string
	w := watcher(src, n, &outcomes)

	w.Observe(context.Background())
	require.Len(t, n.noted, 2)
	first := n.noted[0]
	require.Equal(t, "otter-coder", first.Agent)
	require.Equal(t, "Otter Coder", first.DisplayName)
	require.Equal(t, "badger@example.com", first.Creator)
	require.Equal(t, "proxy.golang.org", first.Host)
	require.Equal(t, 3, first.Count, "a series seen for the first time counts whole")
	require.Equal(t, []string{"s1", "s2"}, first.Sessions)
	require.Equal(t, "https://portal.example.com/agents/org-otter/otter-coder", first.AgentURL)
	require.Equal(t, "sum.golang.org", n.noted[1].Host)

	n.noted = nil
	w.Observe(context.Background())
	require.Empty(t, n.noted, "no increase, nothing to note")

	src.series[0].Value = 5
	w.Observe(context.Background())
	require.Len(t, n.noted, 1)
	require.Equal(t, 3, n.noted[0].Count, "only the increase since the previous read")
	require.Equal(t, []string{"s1"}, n.noted[0].Sessions)
}

func TestWatcher_SkipsWhatItCannotName(t *testing.T) {
	src := &fakeSource{series: []refusals.Series{
		series("sandbox-x", "proxy.golang.org", 1),
		series("session-gone", "proxy.golang.org", 1),
		series("other", "proxy.golang.org", 1),
		series("session-s1", "other", 1),
		series("session-s1", "evil`*host*", 1),
		series("session-s3", "proxy.golang.org", 1),
	}}
	n := &fakeNotifier{}
	var outcomes []string
	watcher(src, n, &outcomes).Observe(context.Background())
	require.Empty(t, n.noted)
	require.Equal(t, []string{refusals.OutcomeNotSession, refusals.OutcomeNoCreator}, outcomes)
}

func TestWatcher_RetriesWhatTheNotifierCouldNotNote(t *testing.T) {
	src := &fakeSource{series: []refusals.Series{series("session-s1", "proxy.golang.org", 2)}}
	n := &fakeNotifier{err: errors.New("slack is down")}
	var outcomes []string
	w := watcher(src, n, &outcomes)

	w.Observe(context.Background())
	require.Equal(t, []string{refusals.OutcomeFailed}, outcomes)

	n.err = nil
	src.series[0].Value = 3
	w.Observe(context.Background())
	require.Len(t, n.noted, 1)
	require.Equal(t, 3, n.noted[0].Count, "the failed read's refusals are counted on the next one")

	n.noted = nil
	n.err = channels.ErrAddresseeNotFound
	src.series[0].Value = 4
	w.Observe(context.Background())
	n.err = nil
	w.Observe(context.Background())
	require.Empty(t, n.noted, "a creator Slack does not know is not retried")
}

func TestWatcher_CounterResetCountsTheNewValue(t *testing.T) {
	src := &fakeSource{series: []refusals.Series{series("session-s1", "proxy.golang.org", 5)}}
	n := &fakeNotifier{}
	var outcomes []string
	w := watcher(src, n, &outcomes)
	w.Observe(context.Background())

	src.series[0].Value = 2
	n.noted = nil
	w.Observe(context.Background())
	require.Len(t, n.noted, 1)
	require.Equal(t, 2, n.noted[0].Count)
}

func TestParseRefusals(t *testing.T) {
	page := `# HELP agentgateway_atenet_egress_refusals_total Refusals.
# TYPE agentgateway_atenet_egress_refusals_total counter
agentgateway_atenet_egress_refusals_total{ate_atespace="org-otter",ate_actor_name="session-abc",scheme="https",host="proxy.golang.org",port="443"} 14
agentgateway_atenet_egress_refusals_total{ate_atespace="org-otter",ate_actor_name="session-abc",scheme="tcp",host="github.com",port="22"} 1
# TYPE agentgateway_requests_total counter
agentgateway_requests_total{status="403"} 99
`
	got, err := refusals.ParseRefusals("10.0.0.1", strings.NewReader(page))
	require.NoError(t, err)
	require.Equal(t, []refusals.Series{
		{Source: "10.0.0.1", Atespace: "org-otter", Actor: "session-abc", Scheme: "https", Host: "proxy.golang.org", Port: 443, Value: 14},
		{Source: "10.0.0.1", Atespace: "org-otter", Actor: "session-abc", Scheme: "tcp", Host: "github.com", Port: 22, Value: 1},
	}, got)

	none, err := refusals.ParseRefusals("10.0.0.1", strings.NewReader("# TYPE x counter\nx 1\n"))
	require.NoError(t, err)
	require.Empty(t, none)
}

// The egress serves OpenMetrics: the counter's family is named without
// _total, an info family sits on the same page and the page ends in # EOF.
func TestParseRefusalsOpenMetrics(t *testing.T) {
	page := `# HELP agentgateway_build Agentgateway build information.
# TYPE agentgateway_build info
agentgateway_build_info{tag="dev"} 1
# HELP agentgateway_atenet_egress_refusals Refusals.
# TYPE agentgateway_atenet_egress_refusals counter
agentgateway_atenet_egress_refusals_total{ate_atespace="kagent",ate_actor_name="session-abc",scheme="https",host="example.org",port="443"} 3 # {trace_id="x"} 1
agentgateway_atenet_egress_refusals_total{ate_atespace="kagent",ate_actor_name="session-abc",scheme="https",host="a # \"b}",port="443"} 2
agentgateway_atenet_egress_refusals_created{ate_atespace="kagent",ate_actor_name="session-abc",scheme="https",host="example.org",port="443"} 1.7e9
# TYPE agentgateway_requests counter
agentgateway_requests_total{status="403"} 99
# EOF
`
	got, err := refusals.ParseRefusals("10.0.0.1", strings.NewReader(page))
	require.NoError(t, err)
	require.Equal(t, []refusals.Series{
		{Source: "10.0.0.1", Atespace: "kagent", Actor: "session-abc", Scheme: "https", Host: "example.org", Port: 443, Value: 3},
		{Source: "10.0.0.1", Atespace: "kagent", Actor: "session-abc", Scheme: "https", Host: `a # "b}`, Port: 443, Value: 2},
	}, got, "a host carrying the exemplar marker, a quote or a brace stays one sample")
}

// The issue's case: refusal-probe is refused in two Sessions, and the first
// is deleted before the watcher's read. Its tombstone still names the Agent,
// so both refusals count into one notice.
func TestWatcher_SessionDeletedBeforeTheReadStillCountsIntoTheNotice(t *testing.T) {
	controller := &fakeController{sessions: map[string]*apiv1alpha1.Session{
		"gone": {
			Id:    "gone",
			State: apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED,
			Agent: &apiv1alpha1.ResourceReference{Namespace: "org-otter", Name: "otter-coder"},
		},
		"kept": {
			Id:    "kept",
			State: apiv1alpha1.RuntimeState_RUNTIME_STATE_READY,
			Agent: &apiv1alpha1.ResourceReference{Namespace: "org-otter", Name: "otter-coder"},
		},
	}}
	src := &fakeSource{series: []refusals.Series{
		series("session-gone", "example.net", 1),
		series("session-kept", "example.net", 1),
	}}
	n := &fakeNotifier{}
	var outcomes []string
	w := watcher(src, n, &outcomes)
	w.Sessions = kagentSessions(t, controller)

	w.Observe(context.Background())
	require.Len(t, n.noted, 1, "exactly one notice for the agent and destination")
	require.Equal(t, "otter-coder", n.noted[0].Agent)
	require.Equal(t, 2, n.noted[0].Count, "the deleted Session's refusal is counted")
	require.Equal(t, []string{"gone", "kept"}, n.noted[0].Sessions)
	require.Equal(t, []string{refusals.OutcomeNoted}, outcomes)

	n.noted = nil
	w.Observe(context.Background())
	require.Empty(t, n.noted, "no second notice for the same refusals")
}

func TestWatcher_LogsARefusalItCannotAttributeOnce(t *testing.T) {
	src := &fakeSource{series: []refusals.Series{series("session-gone", "example.net", 1)}}
	n := &fakeNotifier{}
	var outcomes []string
	var logs bytes.Buffer
	w := watcher(src, n, &outcomes)
	w.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	w.Observe(context.Background())
	w.Observe(context.Background())
	require.Empty(t, n.noted)
	require.Equal(t, []string{refusals.OutcomeNotSession}, outcomes)
	require.Equal(t, 1, strings.Count(logs.String(), "no Session kagent knows"), "logged once, not on every read")
	for _, field := range []string{"actor=session-gone", "host=example.net", "port=443", "count=1", "reason="} {
		require.Contains(t, logs.String(), field)
	}

	src.series[0].Value = 3
	w.Observe(context.Background())
	require.Contains(t, logs.String(), "count=2", "a new refusal is logged with the increase")
}
