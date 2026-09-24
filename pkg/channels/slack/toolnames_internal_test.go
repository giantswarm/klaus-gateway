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

// A cut never lands inside an escaped entity, so a truncated payload ends in
// "…" and not in a fragment such as "&am…" (raised reviewing klaus-gateway#322).
func TestStepField_CutNeverSplitsAnEntity(t *testing.T) {
	// Fill the budget so the "&amp;" of the escaped "&" straddles the cut.
	got := stepField(strings.Repeat("a", stepFieldMax-3) + "& more")
	body := strings.TrimSuffix(got, "…")
	for _, fragment := range []string{"&", "&a", "&am", "&amp"} {
		require.False(t, strings.HasSuffix(body, fragment), "partial entity at the cut: %q", got)
	}
	require.LessOrEqual(t, len([]rune(got)), stepFieldMax)
}

func TestTruncateEntityAware(t *testing.T) {
	// A naive cut at 10 keeps "abcdef&am" and splits the entity.
	require.Equal(t, "abcdef…", truncateEntityAware("abcdef&amp;xyz", 10))
	// A whole entity right before the cut is kept.
	require.Equal(t, "ab&lt;…", truncateEntityAware("ab&lt;cdefgh", 7))
	// Under the cap nothing changes.
	require.Equal(t, "a&amp;b", truncateEntityAware("a&amp;b", 10))
}

// A tool's own emphasis markers are left alone, on purpose: Slack has no escape
// for them in plain text, and the alternatives — a code span, a fence, invisible
// characters — all read worse on this surface (graveler, 2026-09-24). This pins
// the decision so a later change to it is deliberate.
func TestStepField_LeavesEmphasisAlone(t *testing.T) {
	require.Equal(t, "*tenant_id*=gs", stepField("*tenant_id*=gs"))
}
