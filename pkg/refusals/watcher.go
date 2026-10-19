// Package refusals tells an agent's creator when the Substrate egress refuses
// a destination the agent's egress does not declare. It reads the egress's
// refusal counter (agentgateway_atenet_egress_refusals_total, one series per
// actor and destination) from every egress pod, resolves each refusing actor
// — kagent names a Session's actor session-<Session id> in the Agent's
// namespace — to its Session's Agent and the Agent to its creator, and hands
// the refusals of one observation, summed per agent and destination, to a
// channels.RefusalNotifier, which keeps one notice per agent and destination
// within its window.
package refusals

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// DefaultInterval is how often the egress is read when Interval is unset.
const DefaultInterval = time.Minute

// Series is one refusal series an egress pod exports: the actor, the
// destination, and the requests refused since the series appeared.
type Series struct {
	// Source names the pod the series was read from: a counter belongs to one
	// process, so the increase is taken per pod.
	Source   string
	Atespace string
	Actor    string
	Scheme   string
	Host     string
	Port     int
	Value    float64
}

// Source reads the refusal series of every egress pod.
type Source interface {
	Refusals(ctx context.Context) ([]Series, error)
}

// ErrNotSession is a Sessions answer for an actor that is not a Session
// kagent knows: a sandbox's, or one of a Session since deleted.
var ErrNotSession = errors.New("refusals: the actor is not a known Session")

// Sessions resolves a Session to the Agent it runs, as namespace and name.
type Sessions interface {
	SessionAgent(ctx context.Context, sessionID string) (namespace, name string, err error)
}

// Agent is what the notice needs of a kagent Agent.
type Agent struct {
	DisplayName string
	// Creator is the email of the person who created the Agent; "" when it
	// records none (an Agent applied without agent-manager).
	Creator string
}

// Agents reads a kagent Agent.
type Agents interface {
	Agent(ctx context.Context, namespace, name string) (Agent, error)
}

// Watcher reads the egress every Interval and notes what it refused since
// the previous read.
type Watcher struct {
	Source   Source
	Sessions Sessions
	Agents   Agents
	Notifier channels.RefusalNotifier
	// AgentURL opens an agent where its egress is edited; "{namespace}" and
	// "{name}" are replaced. "" offers no link.
	AgentURL string
	Interval time.Duration
	Logger   *slog.Logger
	// Record, when set, counts each agent and destination an observation
	// handled, by outcome: noted, failed, no_creator, not_session.
	Record func(outcome string)
	Now    func() time.Time

	// last is each series' value at the previous read, by seriesKey.
	last map[string]float64
}

// The outcomes Record is called with.
const (
	OutcomeNoted      = "noted"
	OutcomeFailed     = "failed"
	OutcomeNoCreator  = "no_creator"
	OutcomeNotSession = "not_session"
)

// sessionActorPrefix starts the name kagent gives a Session's actor.
const sessionActorPrefix = "session-"

// foldedValue is the label value the egress gives a destination or an actor
// beyond its series bounds: nothing to name, so nothing to notify.
const foldedValue = "other"

// Run reads the egress until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		w.Observe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// group is the refusals of one agent and destination in one observation.
type group struct {
	namespace, agent string
	scheme, host     string
	port             int
	count            float64
	sessions         []string
	// keys are the series counted into the group: a group that could not be
	// noted keeps their previous values, so the next read counts it again.
	keys []string
}

// Observe reads the egress once and notes the refusals since the previous
// read. A series seen for the first time counts whole: the egress drops a
// series an hour after its last refusal, so what it carries is recent, and
// the notifier's record keeps a restart from noting it twice.
func (w *Watcher) Observe(ctx context.Context) {
	series, err := w.Source.Refusals(ctx)
	if err != nil {
		w.logger().Warn("refusals: reading the egress failed", "error", err)
		return
	}
	prev := w.last
	seen := make(map[string]float64, len(series))
	groups := map[string]*group{}
	var order []string
	for _, s := range series {
		key := seriesKey(s)
		seen[key] = s.Value
		increase := s.Value
		if p, ok := prev[key]; ok && s.Value >= p {
			increase = s.Value - p
		}
		if increase <= 0 || s.Host == foldedValue || s.Actor == foldedValue {
			continue
		}
		sessionID, ok := strings.CutPrefix(s.Actor, sessionActorPrefix)
		if !ok || sessionID == "" {
			continue
		}
		namespace, agent, err := w.Sessions.SessionAgent(ctx, sessionID)
		if errors.Is(err, ErrNotSession) {
			w.record(OutcomeNotSession)
			continue
		}
		if err != nil {
			w.logger().Warn("refusals: resolving the actor's Session failed", "atespace", s.Atespace, "actor", s.Actor, "error", err)
			w.record(OutcomeFailed)
			keepPrevious(seen, prev, key)
			continue
		}
		gk := namespace + "/" + agent + "|" + s.Scheme + "://" + s.Host + ":" + strconv.Itoa(s.Port)
		g, ok := groups[gk]
		if !ok {
			g = &group{namespace: namespace, agent: agent, scheme: s.Scheme, host: s.Host, port: s.Port}
			groups[gk] = g
			order = append(order, gk)
		}
		g.count += increase
		g.keys = append(g.keys, key)
		if !slices.Contains(g.sessions, sessionID) {
			g.sessions = append(g.sessions, sessionID)
		}
	}
	now := w.now()
	for _, gk := range order {
		g := groups[gk]
		if !w.note(ctx, g, now) {
			for _, key := range g.keys {
				keepPrevious(seen, prev, key)
			}
		}
	}
	w.last = seen
}

// keepPrevious puts back the value key had at the previous read, so the
// next read counts the increase since then.
func keepPrevious(seen, prev map[string]float64, key string) {
	if p, ok := prev[key]; ok {
		seen[key] = p
	} else {
		delete(seen, key)
	}
}

// note hands one group to the notifier; false when it should be retried.
func (w *Watcher) note(ctx context.Context, g *group, now time.Time) bool {
	log := w.logger().With("agent", g.namespace+"/"+g.agent, "destination", g.scheme+"://"+g.host+":"+strconv.Itoa(g.port))
	agent, err := w.Agents.Agent(ctx, g.namespace, g.agent)
	if err != nil {
		log.Warn("refusals: reading the Agent failed", "error", err)
		w.record(OutcomeFailed)
		return false
	}
	if agent.Creator == "" {
		log.Info("refusals: the Agent records no creator to tell", "count", int(g.count))
		w.record(OutcomeNoCreator)
		return true
	}
	err = w.Notifier.NoteRefusal(ctx, channels.EgressRefusal{
		Namespace: g.namespace, Agent: g.agent, DisplayName: agent.DisplayName, Creator: agent.Creator,
		Scheme: g.scheme, Host: g.host, Port: g.port,
		Count: int(g.count), Sessions: g.sessions, At: now,
		AgentURL: w.agentURL(g.namespace, g.agent),
	})
	if err != nil {
		log.Warn("refusals: noting the refusal failed", "creator", agent.Creator, "error", err)
		w.record(OutcomeFailed)
		// A creator Slack does not know will not be found on the next read.
		return errors.Is(err, channels.ErrAddresseeNotFound)
	}
	w.record(OutcomeNoted)
	return true
}

func (w *Watcher) agentURL(namespace, name string) string {
	if w.AgentURL == "" {
		return ""
	}
	return strings.NewReplacer("{namespace}", namespace, "{name}", name).Replace(w.AgentURL)
}

func seriesKey(s Series) string {
	return strings.Join([]string{s.Source, s.Atespace, s.Actor, s.Scheme, s.Host, strconv.Itoa(s.Port)}, "|")
}

func (w *Watcher) record(outcome string) {
	if w.Record != nil {
		w.Record(outcome)
	}
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
