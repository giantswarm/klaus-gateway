package slack

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

func TestSanitizeDisplayName(t *testing.T) {
	long := strings.Repeat("a", displayNameMaxRunes) + " tail"
	for name, tc := range map[string]struct{ in, want string }{
		"plain":               {"SRE Assistant", "SRE Assistant"},
		"trimmed":             {"  SRE Assistant  ", "SRE Assistant"},
		"whitespace only":     {" \t\n ", ""},
		"newlines to spaces":  {"SRE\nAssistant", "SRE Assistant"},
		"control chars":       {"SRE\x07 \x00Assistant", "SRE Assistant"},
		"runs collapsed":      {"SRE \n\t Assistant", "SRE Assistant"},
		"capped":              {long, strings.Repeat("a", displayNameMaxRunes)},
		"cap keeps runes":     {strings.Repeat("ä", displayNameMaxRunes+5), strings.Repeat("ä", displayNameMaxRunes)},
		"emphasis kept as-is": {"SRE *Ops*", "SRE *Ops*"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, sanitizeDisplayName(tc.in))
			require.LessOrEqual(t, len([]rune(sanitizeDisplayName(tc.in))), displayNameMaxRunes)
		})
	}
}

func TestBareAgentName(t *testing.T) {
	for in, want := range map[string]string{
		"sre-agent":        "sre-agent",
		"kagent/sre-agent": "sre-agent",
		"a/b/c":            "c", // never a slash in the result, whatever shape an unvalidated ref has
		"":                 "",
	} {
		require.Equal(t, want, bareAgentName(in), "ref %q", in)
	}
}

// rosterErrThenAgents fails until unblocked, then serves agents.
type rosterErrThenAgents struct {
	mu     sync.Mutex
	agents []pkga2a.AgentInfo
	fail   bool
	calls  int
}

func (r *rosterErrThenAgents) ListAgents(context.Context) ([]pkga2a.AgentInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fail {
		return nil, errors.New("controller unreachable")
	}
	return r.agents, nil
}

func testAdapterWithRoster(r AgentRosterSource) *Adapter {
	return &Adapter{Roster: r, Logger: slog.New(slog.DiscardHandler)}
}

// A stale roster is served as the last known good one even while the negative
// cache is armed: a transient fetch failure must not flap an agent's name
// mid-conversation (the label keeps its last good value; only a roster never
// fetched at all falls back to the technical name).
func TestRosterBestEffort_ServesStaleCacheThroughFailure(t *testing.T) {
	roster := &rosterErrThenAgents{fail: true}
	a := testAdapterWithRoster(roster)
	known := []pkga2a.AgentInfo{{Name: "sre-agent", DisplayName: "SRE Assistant"}}
	a.rosterCached = known
	a.rosterExpires = time.Now().Add(-time.Minute)         // positive cache expired
	a.rosterFailedUntil = time.Now().Add(rosterFailureTTL) // negative cache armed
	got, err := a.rosterAgentsBestEffort(context.Background())
	require.NoError(t, err, "the last known roster is served, not refused")
	require.Equal(t, known, got)
	require.Zero(t, roster.calls, "the armed negative cache defers the refresh")
}

// A stale roster triggers exactly one background refresh while the stale data
// is returned immediately — branding never waits on the controller.
func TestRosterBestEffort_StaleServesImmediatelyAndRefreshesOnce(t *testing.T) {
	fresh := []pkga2a.AgentInfo{{Name: "sre-agent", DisplayName: "SRE Assistant v2"}}
	roster := &rosterErrThenAgents{agents: fresh}
	a := testAdapterWithRoster(roster)
	old := []pkga2a.AgentInfo{{Name: "sre-agent", DisplayName: "SRE Assistant"}}
	a.rosterCached = old
	a.rosterExpires = time.Now().Add(-time.Minute)

	got, err := a.rosterAgentsBestEffort(context.Background())
	require.NoError(t, err)
	require.Equal(t, old, got, "the stale roster is served without waiting")

	require.Eventually(t, func() bool {
		a.rosterMu.Lock()
		defer a.rosterMu.Unlock()
		return len(a.rosterCached) == 1 && a.rosterCached[0].DisplayName == "SRE Assistant v2"
	}, 2*time.Second, 10*time.Millisecond, "the background refresh lands")

	roster.mu.Lock()
	calls := roster.calls
	roster.mu.Unlock()
	require.Equal(t, 1, calls, "one refresh, not one per caller")
}

// With no roster ever fetched, an armed negative cache short-circuits: the
// cold path is the only one allowed to refuse.
func TestRosterBestEffort_ColdWithArmedNegativeCacheRefuses(t *testing.T) {
	roster := &rosterErrThenAgents{fail: true}
	a := testAdapterWithRoster(roster)
	a.rosterFailedUntil = time.Now().Add(rosterFailureTTL)
	_, err := a.rosterAgentsBestEffort(context.Background())
	require.Error(t, err)
	require.Zero(t, roster.calls, "no fetch while the failure is remembered")
}
