package slack

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// stepTitleMax is Slack's cap on a task_update chunk's title.
const stepTitleMax = 256

// unnamedStepTitle stands in when a tool name humanises to nothing (an empty
// name, or one made of separators alone). A task_update chunk must carry a
// title, so there is always one.
const unnamedStepTitle = "Running a tool"

// metaToolTitles names muster's meta-tools in plain language. They are the
// calls an agent makes before it reaches the tool it actually wants, and their
// raw names say nothing to the person reading the thread. call_tool is absent
// on purpose: the caller unwraps it to the inner tool first (unwrapCallTool),
// so the step names what is really being run.
var metaToolTitles = map[string]string{
	"filter_tools":  "Finding the right tool",
	"describe_tool": "Reading a tool's schema",
	"list_tools":    "Listing the available tools",
	// Not a muster tool: the runtime's question to the person, which the
	// prompt under the reply asks. Its raw name reads as an instruction.
	channels.AskUserToolName: "Question for you",
}

// toolNamespacePrefixes are the namespaces muster puts in front of a tool name.
// They say where a tool comes from, not what it does, so the step title drops
// them.
var toolNamespacePrefixes = []string{"x_", "workflow_"}

// stepTitle renders a tool name as the plain-language title of one step in the
// reply's task list. Slack's agent design guide asks for what the agent is
// doing ("Listing the available tools"), not the API name, so the muster
// meta-tools get phrases of their own and every other name is humanised: the
// namespace prefix goes, underscores become spaces and the first word is
// capitalised ("x_kubernetes_list" → "Kubernetes list"). The raw name stays
// available in the step's details.
//
// The name is agent- and MCP-server-controlled text, so mrkdwn control
// sequences are escaped and newlines flattened before it is cut to Slack's
// title limit.
//
// The chat.appendStream reference does not say whether a task_update's title,
// details and output are parsed as mrkdwn, but graveler answered it on
// 2026-09-22: a result preview sent with &gt; rendered as ">" on screen. The
// fields decode entities, so they follow mrkdwn semantics and the escaping is
// necessary — a raw <…> would be read as a link or a mention.
func stepTitle(name string) string {
	title, ok := metaToolTitles[name]
	if !ok {
		title = humaniseToolName(name)
	}
	return truncateRunes(stepSafeText(title), stepTitleMax)
}

// humaniseToolName turns a raw tool name into a readable phrase.
func humaniseToolName(name string) string {
	s := strings.TrimSpace(name)
	for _, prefix := range toolNamespacePrefixes {
		if rest := strings.TrimPrefix(s, prefix); rest != s {
			s = rest
			break
		}
	}
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "_", " ")), " ")
	if s == "" {
		return unnamedStepTitle
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// stepSafeText prepares agent-controlled text for a task_update chunk field:
// mrkdwn control sequences escaped so quoted content cannot trigger
// notifications, and whitespace collapsed to single spaces so a step stays one
// line.
func stepSafeText(s string) string {
	return strings.Join(strings.Fields(escapeMrkdwn(s)), " ")
}
