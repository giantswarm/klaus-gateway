package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

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

// rosterPerCaller answers each caller, known by the token their context
// carries, with their own Agents, as the controller does.
type rosterPerCaller struct {
	mu     sync.Mutex
	byUser map[string][]pkga2a.AgentInfo
	calls  int
}

func (r *rosterPerCaller) ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	token := pkga2a.ForwardedTokenFromContext(ctx)
	if token == "" {
		return nil, pkga2a.ErrNoIdentity
	}
	return r.byUser[token], nil
}

// Two people asking at once get their own rosters: nothing one was answered
// is served to the other, so every listing asks.
func TestRosterAgents_EachCallersOwn(t *testing.T) {
	alice := []pkga2a.AgentInfo{{Name: "billing", Namespace: "org-acme", Tenant: "organization acme"}}
	bob := []pkga2a.AgentInfo{{Name: "billing", Namespace: "org-beta", Tenant: "organization beta"}}
	roster := &rosterPerCaller{byUser: map[string][]pkga2a.AgentInfo{"alice": alice, "bob": bob}}
	a := &Adapter{Roster: roster, Logger: slog.New(slog.DiscardHandler)}

	var wg sync.WaitGroup
	got := make([][]pkga2a.AgentInfo, 2)
	errs := make([]error, 2)
	for i, user := range []string{"alice", "bob"} {
		wg.Go(func() {
			got[i], errs[i] = a.rosterAgents(pkga2a.WithForwardedToken(context.Background(), user))
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, alice, got[0])
	require.Equal(t, bob, got[1])

	_, err := a.rosterAgents(context.Background())
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity, "no roster without a caller")
	require.Equal(t, 3, roster.calls)
}

// A ref keeps the deployment's shape for an Agent of the platform tenant, so
// the refs threads already carry keep their spelling, and is namespace/name
// for an Agent of any other tenant, so two Agents of one name stay two refs.
func TestAgentInfoRef(t *testing.T) {
	acme := pkga2a.AgentInfo{Name: "billing", Namespace: "org-acme"}
	platform := pkga2a.AgentInfo{Name: "sre-agent", Namespace: "kagent"}
	bare := &Adapter{DefaultAgent: "sre-agent", Namespace: "kagent"}
	require.Equal(t, "sre-agent", bare.agentInfoRef(platform))
	require.Equal(t, "org-acme/billing", bare.agentInfoRef(acme))
	qualified := &Adapter{DefaultAgent: "kagent/sre-agent", Namespace: "kagent"}
	require.Equal(t, "kagent/sre-agent", qualified.agentInfoRef(platform))
	require.Equal(t, "org-acme/billing", qualified.agentInfoRef(acme))
	elsewhere := &Adapter{DefaultAgent: "ops/sre-agent", Namespace: "kagent"}
	require.Equal(t, "kagent/sre-agent", elsewhere.agentInfoRef(platform), "a qualified default elsewhere keeps the Agent's own namespace")
	unset := &Adapter{DefaultAgent: "kagent/sre-agent"}
	require.Equal(t, "kagent/sre-agent", unset.agentInfoRef(platform), "the default's namespace is the platform's when Namespace is unset")
}

// The roster rows: a header and a summary naming the default, the default
// first then A–Z, one sentence of each description, a Select button carrying
// the ref, and past rosterRowsMax the rest named in the footer.
func TestRosterBlocks(t *testing.T) {
	a := &Adapter{DefaultAgent: "zeta"}
	agents := []pkga2a.AgentInfo{{Name: "beta", Description: "Checks things. Also other things."}, {Name: "zeta", DisplayName: "Zeta"}, {Name: "alpha"}}
	for i := range 8 {
		agents = append(agents, pkga2a.AgentInfo{Name: fmt.Sprintf("x%d", i)})
	}
	blocks := a.rosterBlocks(agents, true)

	text := func(b any) string {
		m := b.(map[string]any)
		if m[bkType] == bkContext {
			return m[bkElements].([]any)[0].(map[string]any)[bkText].(string)
		}
		return m[bkText].(map[string]any)[bkText].(string)
	}
	require.Equal(t, "header", blocks[0].(map[string]any)[bkType])
	require.Equal(t, "11 agents available · Zeta is preselected in the picker", text(blocks[1]))
	var rows []any
	for _, b := range blocks {
		if _, ok := b.(map[string]any)[bkAccessory]; ok {
			rows = append(rows, b)
		}
	}
	require.Len(t, rows, rosterRowsMax)
	require.Equal(t, "*Zeta* · platform · default", text(rows[0]))
	require.Equal(t, "*alpha* · platform", text(rows[1]))
	require.Equal(t, "*beta* · platform\nChecks things.", text(rows[2]), "one sentence of the description")
	button := rows[0].(map[string]any)[bkAccessory].(map[string]any)
	require.Equal(t, agentSelectAction, button[bkActionID])
	require.Equal(t, "zeta", button[bkValue])
	require.Equal(t, "3 more: x5, x6, x7 — start one of those from the app's slash command, or the ⋯ menu's *Ask an agent here*. Select starts a conversation with that agent in this thread.", text(blocks[len(blocks)-1]))

	// In a thread that already has its conversation the picker refuses, so
	// the rows carry no button and the footer points at a new thread.
	blocks = a.rosterBlocks(agents[:3], false)
	for _, b := range blocks {
		_, button := b.(map[string]any)[bkAccessory]
		require.False(t, button, "no Select button in a bound thread")
	}
	require.Equal(t, "This thread already has its agent. To talk to another one, start a new conversation with `/swarmgeist` and pick it there.", text(blocks[len(blocks)-1]))
}

func TestFirstSentence(t *testing.T) {
	require.Equal(t, "Checks things.", firstSentence("Checks things. Also other things."))
	require.Equal(t, "One line", firstSentence("One line\nsecond line"))
	require.Equal(t, "v1.2 is fine", firstSentence("  v1.2   is fine "), "a dot without a space is not a sentence end")
	require.Equal(t, "", firstSentence(""))
}
