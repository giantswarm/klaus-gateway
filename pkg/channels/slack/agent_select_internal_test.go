package slack

import (
	"testing"

	"github.com/stretchr/testify/require"
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

// rootAgentRef only binds for a complete "/agent <name> <question>" root: a
// name-only or malformed prefix never started a conversation.
func TestRootAgentRef(t *testing.T) {
	a := &Adapter{DefaultAgent: "kagent/swarmgeist"}

	require.Equal(t, "kagent/sre-agent", a.rootAgentRef("<@UBOT> /agent sre-agent why?"))
	require.Equal(t, "kagent/sre-agent", a.rootAgentRef("/agent sre-agent why?"))
	require.Empty(t, a.rootAgentRef("<@UBOT> plain question"))
	require.Empty(t, a.rootAgentRef("/agent sre-agent"), "a name-only root selected nothing")
	require.Empty(t, a.rootAgentRef("/agent ../../etc oops"), "a malformed name binds nothing")
	require.Empty(t, a.rootAgentRef(""))
}
