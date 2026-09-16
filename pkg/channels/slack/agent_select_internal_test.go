package slack

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

func TestSplitAgentCommand(t *testing.T) {
	cases := []struct {
		in, name string
		quoted   bool
		question string
	}{
		{"/agent sre-agent why are pods crashlooping?", "sre-agent", false, "why are pods crashlooping?"},
		{"/agent sre-agent", "sre-agent", false, ""},
		{"/agent", "", false, ""},
		{"  /agent   sre-agent   spaced out  ", "sre-agent", false, "spaced out"},
		// The question keeps its original formatting; Fields would collapse it.
		{"/agent sre-agent line one\nline two", "sre-agent", false, "line one\nline two"},
		{"/AGENT SRE-Agent hi", "SRE-Agent", false, "hi"},
		// parseCommand tolerates whitespace after the slash; the splitter must too.
		{"/ agent sre-agent do things", "sre-agent", false, "do things"},
		// A quoted name may contain spaces: display-name selection.
		{`/agent "SRE Agent" tell a joke`, "SRE Agent", true, "tell a joke"},
		{`/agent "sre-agent" tell a joke`, "sre-agent", true, "tell a joke"},
		{`/agent "SRE Agent"`, "SRE Agent", true, ""},
		// Slack autoformats straight quotes into curly ones as the user types.
		{"/agent “SRE Agent” what is up?", "SRE Agent", true, "what is up?"},
		{"/agent ‘SRE Agent’ what is up?", "SRE Agent", true, "what is up?"},
		{"/agent 'SRE Agent' what is up?", "SRE Agent", true, "what is up?"},
		// An unterminated quote falls back to the token split, which fails
		// resolution loudly instead of guessing where the name ends.
		{`/agent "SRE Agent tell a joke`, `"SRE`, false, "Agent tell a joke"},
		{`/agent "" tell a joke`, "", true, "tell a joke"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, quoted, question := splitAgentCommand(tc.in)
			require.Equal(t, tc.name, name)
			require.Equal(t, tc.quoted, quoted)
			require.Equal(t, tc.question, question)
		})
	}
}

func TestAgentRefFromName(t *testing.T) {
	withNS := &Adapter{DefaultAgent: "kagent/swarmgeist"}
	bare := &Adapter{DefaultAgent: "test-agent"}

	cases := []struct {
		adapter *Adapter
		in, ref string
		ok      bool
	}{
		{withNS, "sre-agent", "kagent/sre-agent", true},
		{withNS, "SRE-Agent", "kagent/sre-agent", true},
		{withNS, "other/lab-agent", "other/lab-agent", true},
		{bare, "sre-agent", "sre-agent", true},
		{bare, "kagent/sre-agent", "kagent/sre-agent", true},
		// Not a DNS-1123 label: rejected before it can reach a URL path.
		{bare, "../../etc", "", false},
		{bare, "a b", "", false},
		{bare, "-agent", "", false},
		{bare, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			ref, ok := tc.adapter.agentRefFromName(tc.in)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.ref, ref)
		})
	}
}

// staticRoster is the minimal AgentRosterSource for internal resolution tests.
type staticRoster struct {
	agents []pkga2a.AgentInfo
	err    error
}

func (s *staticRoster) ListAgents(context.Context) ([]pkga2a.AgentInfo, error) {
	return s.agents, s.err
}

func selectionAdapter(roster AgentRosterSource) *Adapter {
	return &Adapter{
		DefaultAgent: "kagent/swarmgeist",
		Roster:       roster,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// agentRefsForSelector matches display names and technical names in one pass,
// with no precedence: a selector naming two different agents is ambiguous.
func TestAgentRefsForSelector(t *testing.T) {
	a := selectionAdapter(&staticRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
		{Name: "swarmgeist", Namespace: "kagent", DisplayName: "Swarmgeist"},
		{Name: "twin", Namespace: "kagent", DisplayName: "Doppelgänger"},
		{Name: "doppelganger", Namespace: "other", DisplayName: "Doppelganger"},
		{Name: "plain", Namespace: "kagent"},
	}})
	ctx := context.Background()

	refs, err := a.agentRefsForSelector(ctx, "SRE Agent")
	require.NoError(t, err)
	require.Equal(t, []string{"kagent/sre-agent"}, refs)

	// Case-insensitive, whitespace collapsed.
	refs, err = a.agentRefsForSelector(ctx, "  sre   AGENT ")
	require.NoError(t, err)
	require.Equal(t, []string{"kagent/sre-agent"}, refs)

	// A quoted technical name still resolves.
	refs, err = a.agentRefsForSelector(ctx, "sre-agent")
	require.NoError(t, err)
	require.Equal(t, []string{"kagent/sre-agent"}, refs)

	// No display-name annotation: the technical name is the display name.
	refs, err = a.agentRefsForSelector(ctx, "plain")
	require.NoError(t, err)
	require.Equal(t, []string{"kagent/plain"}, refs)

	// Matching an agent by both its display and technical name is one match.
	refs, err = a.agentRefsForSelector(ctx, "Swarmgeist")
	require.NoError(t, err)
	require.Equal(t, []string{"kagent/swarmgeist"}, refs)

	refs, err = a.agentRefsForSelector(ctx, "nobody")
	require.NoError(t, err)
	require.Empty(t, refs)

	refs, err = a.agentRefsForSelector(ctx, "")
	require.NoError(t, err)
	require.Empty(t, refs)
}

// A bare default agent means the namespace lives in the configured A2A base
// URL, so resolved refs stay bare too.
func TestAgentRefsForSelectorBareDefault(t *testing.T) {
	a := selectionAdapter(&staticRoster{agents: []pkga2a.AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"},
	}})
	a.DefaultAgent = "swarmgeist"

	refs, err := a.agentRefsForSelector(context.Background(), "SRE Agent")
	require.NoError(t, err)
	require.Equal(t, []string{"sre-agent"}, refs)
}

func TestAgentRefFromName_FollowsDeploymentRefShape(t *testing.T) {
	cases := []struct {
		name, defaultAgent, namespace, input, want string
	}{
		{"bare default, bare name", "sre-agent", "kagent", "sre-agent", "sre-agent"},
		{"bare default, served namespace collapses", "sre-agent", "kagent", "kagent/sre-agent", "sre-agent"},
		{"bare default, foreign namespace kept", "sre-agent", "kagent", "other/sre-agent", "other/sre-agent"},
		{"qualified default, bare name gets the namespace", "kagent/sre-agent", "kagent", "sre-agent", "kagent/sre-agent"},
		{"qualified default, qualified name kept", "kagent/sre-agent", "kagent", "kagent/sre-agent", "kagent/sre-agent"},
		{"namespace unknown to the adapter: typed namespace kept", "sre-agent", "", "kagent/sre-agent", "kagent/sre-agent"},
		{"uppercase folded", "sre-agent", "kagent", "Kagent/SRE-Agent", "sre-agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{DefaultAgent: tc.defaultAgent, Namespace: tc.namespace}
			got, ok := a.agentRefFromName(tc.input)
			require.True(t, ok, "agentRefFromName(%q) rejected the name", tc.input)
			require.Equal(t, tc.want, got, "agentRefFromName(%q)", tc.input)
		})
	}
}

func TestAgentInfoRef_FollowsDeploymentRefShape(t *testing.T) {
	ag := pkga2a.AgentInfo{Name: "sre-agent", Namespace: "kagent"}
	bare := &Adapter{DefaultAgent: "sre-agent", Namespace: "kagent"}
	require.Equal(t, "sre-agent", bare.agentInfoRef(ag), "bare default")
	qualified := &Adapter{DefaultAgent: "kagent/sre-agent", Namespace: "kagent"}
	require.Equal(t, "kagent/sre-agent", qualified.agentInfoRef(ag), "qualified default")
}
