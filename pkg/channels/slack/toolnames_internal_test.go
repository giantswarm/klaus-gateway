package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStepTitle(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		// muster's meta-tools get phrases of their own: their raw names say
		// nothing to the person reading the thread.
		{"filter_tools", "Finding the right tool"},
		{"describe_tool", "Reading a tool's schema"},
		{"list_tools", "Listing the available tools"},
		// The namespace prefix says where a tool comes from, not what it does.
		{"x_kubernetes_list", "Kubernetes list"},
		{"x_prometheus_query", "Prometheus query"},
		{"workflow_cluster_health", "Cluster health"},
		// Everything else is humanised as it stands.
		{"ask_user", "Question for you"},
		{"core_auth_login", "Core auth login"},
		{"skills", "Skills"},
		// call_tool is unwrapped to the inner tool before a title is asked for
		// (renderToolActivity); the wrapper itself humanises like any other name.
		{"call_tool", "Call tool"},
		// A prefix on its own leaves nothing to say.
		{"x_", unnamedStepTitle},
		{"", unnamedStepTitle},
		{"___", unnamedStepTitle},
		// The name is agent-controlled text: mrkdwn control sequences are
		// escaped and the title stays one line.
		{"notify <!channel> &\nnow", "Notify &lt;!channel&gt; &amp; now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, stepTitle(tc.name))
		})
	}
}

// Slack caps a task_update title, so an absurd tool name is cut rather than
// refused.
func TestStepTitle_TruncatesToSlacksLimit(t *testing.T) {
	title := stepTitle(strings.Repeat("a", stepTitleMax*2))
	require.Len(t, []rune(title), stepTitleMax)
}

// The details and output of a step are a fenced code block cut to the chunk
// size Slack documents for task_update, with the six fence characters inside
// that budget. Unlike the title they are not mrkdwn-escaped: Slack renders code
// verbatim, so an escaped "&" would show as "&amp;" (graveler, 2026-09-24).
func TestStepField_IsAFlattenedCodeBlock(t *testing.T) {
	require.Equal(t, "```a & b```", stepField("a &\nb"))
	require.Len(t, []rune(stepField(strings.Repeat("x", stepFieldMax*2))), stepFieldMax)
	require.Empty(t, stepField("   "), "an empty payload sends no field, not an empty block")
}

// Inside the block a tool's own text shows as typed: emphasis markers cannot
// style the reply (graveler, 2026-09-23: muster's prometheus tool emphasised a
// label and it rendered bold in a plain field), mention syntax stays text, and
// backticks become apostrophes so they cannot close the fence early.
func TestStepField_RendersTheTextVerbatim(t *testing.T) {
	require.Equal(t, "```node_dmi_info{*tenant_id*=\"gs\"}```", stepField(`node_dmi_info{*tenant_id*="gs"}`))
	require.Equal(t, "```<@U123> hi```", stepField("<@U123> hi"))
	require.Equal(t, "```run 'ls' now```", stepField("run `ls` now"))
	require.Equal(t, "```fence ''' inside```", stepField("fence ``` inside"))
}

// A payload cut at the limit ends in the ellipsis, inside the budget.
func TestStepField_CutEndsCleanly(t *testing.T) {
	got := stepField(strings.Repeat("a", stepFieldMax*2) + "& more")
	require.LessOrEqual(t, len([]rune(got)), stepFieldMax)
	require.True(t, strings.HasSuffix(got, "…```"), "%q", got)
}
