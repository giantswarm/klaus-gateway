package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolTitle(t *testing.T) {
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
		{"core_auth_login", "Core auth login"},
		{"skills", "Skills"},
		// call_tool is unwrapped to the inner tool before a title is asked for
		// (approvalToolTitles); the wrapper itself humanises like any other name.
		{"call_tool", "Call tool"},
		// A prefix on its own leaves nothing to say.
		{"x_", unnamedToolTitle},
		{"", unnamedToolTitle},
		{"___", unnamedToolTitle},
		// The name is agent-controlled text: mrkdwn control sequences are
		// escaped and the title stays one line.
		{"notify <!channel> &\nnow", "Notify &lt;!channel&gt; &amp; now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, toolTitle(tc.name))
		})
	}
}

// An absurd tool name is cut rather than filling the approval card.
func TestToolTitle_Truncates(t *testing.T) {
	title := toolTitle(strings.Repeat("a", toolTitleMax*2))
	require.Len(t, []rune(title), toolTitleMax)
}
