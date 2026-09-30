package slack

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// toolTitleMax caps one tool's title, so an absurd tool name cannot fill the
// approval card on its own.
const toolTitleMax = 256

// unnamedToolTitle stands in when a tool name humanises to nothing (an empty
// name, or one made of separators alone), so a tool always has a title.
const unnamedToolTitle = "Running a tool"

// metaToolTitles names muster's meta-tools in plain language. They are the
// calls an agent makes before it reaches the tool it actually wants, and their
// raw names say nothing to the person reading the thread. call_tool is absent
// on purpose: the caller unwraps it to the inner tool first (unwrapCallTool),
// so the title names what is really being run.
var metaToolTitles = map[string]string{
	"filter_tools":  "Finding the right tool",
	"describe_tool": "Reading a tool's schema",
	"list_tools":    "Listing the available tools",
}

// toolNamespacePrefixes are the namespaces muster puts in front of a tool name.
// They say where a tool comes from, not what it does, so the title drops them.
var toolNamespacePrefixes = []string{"x_", "workflow_"}

// toolTitle renders a tool name as the plain-language title an approval card
// names the call by. Slack's agent design guide asks for what the agent is
// doing ("Listing the available tools"), not the API name, so the muster
// meta-tools get phrases of their own and every other name is humanised: the
// namespace prefix goes, underscores become spaces and the first word is
// capitalised ("x_kubernetes_list" → "Kubernetes list").
//
// The name is agent- and MCP-server-controlled text, so mrkdwn control
// sequences are escaped — a raw <…> would be read as a link or a mention — and
// whitespace is collapsed so the title stays one line, before it is cut to
// toolTitleMax.
func toolTitle(name string) string {
	title, ok := metaToolTitles[name]
	if !ok {
		title = humaniseToolName(name)
	}
	return truncateRunes(strings.Join(strings.Fields(escapeMrkdwn(title)), " "), toolTitleMax)
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
		return unnamedToolTitle
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}
