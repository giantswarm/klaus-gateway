package refusals_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
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
