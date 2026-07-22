package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

func TestSplitAgentCommand(t *testing.T) {
	cases := []struct {
		in, name, question string
	}{
		{"/agent sre-agent why are pods crashlooping?", "sre-agent", "why are pods crashlooping?"},
		{"/agent sre-agent", "sre-agent", ""},
		{"/agent", "", ""},
		{"  /agent   sre-agent   spaced out  ", "sre-agent", "spaced out"},
		// The question keeps its original formatting; Fields would collapse it.
		{"/agent sre-agent line one\nline two", "sre-agent", "line one\nline two"},
		{"/AGENT SRE-Agent hi", "SRE-Agent", "hi"},
		// parseCommand tolerates whitespace after the slash; the splitter must too.
		{"/ agent sre-agent do things", "sre-agent", "do things"},
		// No quoting grammar: selection is by technical name, so quotes are
		// just characters. The quote-carrying token fails validation and lands
		// on the did-you-mean suggestion (whose normalization strips quotes).
		{`/agent "SRE Agent" tell a joke`, `"SRE`, `Agent" tell a joke`},
		{`/agent "sre-agent" tell a joke`, `"sre-agent"`, "tell a joke"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, question := splitAgentCommand(tc.in)
			require.Equal(t, tc.name, name)
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

// staticRoster and staticCards are minimal in-package fakes for the
// did-you-mean matcher.
type staticRoster []pkga2a.AgentInfo

func (r staticRoster) ListAgents(context.Context) ([]pkga2a.AgentInfo, error) { return r, nil }

type staticCards map[string]string // ref -> display name

func (c staticCards) CardIdentity(_ context.Context, ref string) (string, string) {
	return c[ref], ""
}

// The did-you-mean matcher fires only on an exact normalized match of the
// typed name (extended into the question) against a roster agent's display or
// technical name — never on a fuzzy guess.
func TestDidYouMeanSuggestion(t *testing.T) {
	a := &Adapter{
		DefaultAgent: "kagent/swarmgeist",
		Roster:       staticRoster{{Name: "sre-agent", Namespace: "kagent"}},
		AgentCards:   staticCards{"kagent/sre-agent": "SRE Agent"},
	}

	// Unquoted display name: "SRE" + the question's first word "Agent".
	suggestion, ok := a.didYouMeanSuggestion(t.Context(), "SRE", "Agent tell me a joke")
	require.True(t, ok)
	require.Equal(t, "Did you mean `/agent sre-agent tell me a joke`?", suggestion)

	// A quoted display name parses as a quote-carrying token; normalization
	// strips the quotes so the match still lands.
	suggestion, ok = a.didYouMeanSuggestion(t.Context(), `"SRE`, `Agent" tell me a joke`)
	require.True(t, ok)
	require.Equal(t, "Did you mean `/agent sre-agent tell me a joke`?", suggestion)

	// Separator differences normalize away.
	_, ok = a.didYouMeanSuggestion(t.Context(), "sre_agent", "hi")
	require.True(t, ok)

	// A consumed question leaves a placeholder so the suggestion stays complete.
	suggestion, ok = a.didYouMeanSuggestion(t.Context(), "SRE", "Agent")
	require.True(t, ok)
	require.Equal(t, "Did you mean `/agent sre-agent <question>`?", suggestion)

	// A plain typo is not "one normalization away": no guess.
	_, ok = a.didYouMeanSuggestion(t.Context(), "sre-agnt", "tell me a joke")
	require.False(t, ok)
}

// openingAgentRef only binds for a complete "/agent <name> <question>"
// opening message: a name-only or malformed prefix never started a
// conversation.
func TestOpeningAgentRef(t *testing.T) {
	a := &Adapter{DefaultAgent: "kagent/swarmgeist"}

	require.Equal(t, "kagent/sre-agent", a.openingAgentRef("<@UBOT> /agent sre-agent why?"))
	require.Equal(t, "kagent/sre-agent", a.openingAgentRef("/agent sre-agent why?"))
	// Consistent with the live selection: a quoted name fails validation there
	// (no quoting grammar), so it never bound a conversation — recovery binds
	// nothing for it either.
	require.Empty(t, a.openingAgentRef(`/agent "sre-agent" why?`))
	require.Empty(t, a.openingAgentRef("<@UBOT> plain question"))
	require.Empty(t, a.openingAgentRef("/agent sre-agent"), "a name-only opener selected nothing")
	require.Empty(t, a.openingAgentRef("/agent ../../etc oops"), "a malformed name binds nothing")
	require.Empty(t, a.openingAgentRef(""))
}
