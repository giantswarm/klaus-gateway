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

// The details and output of a step take the same treatment as its title, cut to
// the chunk size Slack documents for task_update.
func TestStepField_EscapesFlattensAndTruncates(t *testing.T) {
	require.Equal(t, "a &amp; b", stepField("a &\nb"))
	require.Len(t, []rune(stepField(strings.Repeat("x", stepFieldMax*2))), stepFieldMax)
	require.Empty(t, stepField("   "))
}
