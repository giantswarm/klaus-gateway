package slack

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

func pickerAdapter(defaultAgent string) *Adapter {
	return &Adapter{DefaultAgent: defaultAgent, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func modalOptions(t *testing.T, view map[string]any) (labels, values []string, initial string) {
	t.Helper()
	sel := view[bkBlocks].([]any)[1].(map[string]any)[bkElement].(map[string]any)
	for _, o := range sel[bkOptions].([]any) {
		opt := o.(map[string]any)
		labels = append(labels, opt[bkText].(map[string]any)[bkText].(string))
		values = append(values, opt[bkValue].(string))
	}
	if init, ok := sel[bkInitialOption].(map[string]any); ok {
		initial = init[bkValue].(string)
	}
	return labels, values, initial
}

// The option list is capped at Slack's static_select limit, deduped by ref,
// labelled by display name (technical name when none, long names cut), and
// the default agent is preselected when it is on the roster.
func TestAskAgentModal_OptionsCapDedupAndDefault(t *testing.T) {
	a := pickerAdapter("kagent/a-050")
	agents := make([]pkga2a.AgentInfo, 0, modalMaxAgents+5)
	for i := 0; i < modalMaxAgents+3; i++ {
		agents = append(agents, pkga2a.AgentInfo{Name: fmt.Sprintf("a-%03d", i), Namespace: "kagent", DisplayName: fmt.Sprintf("Agent %03d", i)})
	}
	agents = append(agents, agents[0], agents[1]) // duplicates by ref
	agents[7].DisplayName = ""                    // falls back to the technical name
	agents[8].DisplayName = strings.Repeat("x", 120)

	view, err := a.askAgentModal(agents, askAgentRequest{Channel: "C1", User: "U1", ResponseURL: "https://hooks/r"})
	require.NoError(t, err)

	labels, values, initial := modalOptions(t, view)
	require.Len(t, values, modalMaxAgents, "the list is cut at the static_select cap")
	require.Equal(t, "kagent/a-000", values[0])
	require.Equal(t, "a-007", labels[7], "no display name: the technical name labels the option")
	require.Equal(t, modalOptionLabelMax, len([]rune(labels[8])), "long labels are cut to Slack's option label cap")
	require.Equal(t, "kagent/a-050", initial, "the default agent is preselected")
	seen := map[string]bool{}
	for _, v := range values {
		require.False(t, seen[v], "duplicate option value %s", v)
		seen[v] = true
	}
}

// A default agent that is not on the roster leaves the select without a
// preselection or hint; the command's text prefills the prompt, cut to the
// input's max length; an empty text leaves the box empty.
func TestAskAgentModal_PrefillAndNoDefault(t *testing.T) {
	a := pickerAdapter("kagent/elsewhere")
	agents := []pkga2a.AgentInfo{{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"}}

	view, err := a.askAgentModal(agents, askAgentRequest{Channel: "C1", User: "U1", Prefill: "  " + strings.Repeat("q", modalQuestionMax+10) + "  "})
	require.NoError(t, err)
	_, _, initial := modalOptions(t, view)
	require.Empty(t, initial)
	_, hinted := view[bkBlocks].([]any)[1].(map[string]any)[bkHint]
	require.False(t, hinted, "no default on the list, no hint naming one")
	question := view[bkBlocks].([]any)[2].(map[string]any)[bkElement].(map[string]any)
	require.Equal(t, modalQuestionMax, len([]rune(question[bkInitialValue].(string))))

	view, err = a.askAgentModal(agents, askAgentRequest{Channel: "C1", User: "U1", Prefill: "   "})
	require.NoError(t, err)
	question = view[bkBlocks].([]any)[2].(map[string]any)[bkElement].(map[string]any)
	_, has := question[bkInitialValue]
	require.False(t, has, "no text, no prefill")

	var pm askAgentPrivateMetadata
	require.NoError(t, json.Unmarshal([]byte(view[bkPrivateMetadata].(string)), &pm))
	require.Equal(t, askAgentPrivateMetadata{Channel: "C1", User: "U1"}, pm)
}

// The Socket Mode slash_commands payload decodes into the same struct the HTTP
// form fills, so both transports feed one handler.
func TestSlashCommandPayload_DecodesSocketModeEnvelope(t *testing.T) {
	raw := []byte(`{"command":"/swarmgeist","text":"hi there","user_id":"U1","channel_id":"C1","trigger_id":"1.2.abc","response_url":"https://hooks.slack.com/commands/T/1/x","team_id":"T1"}`)
	var p slashCommandPayload
	require.NoError(t, json.Unmarshal(raw, &p))
	require.Equal(t, slashCommandPayload{Command: "/swarmgeist", Text: "hi there", UserID: "U1", ChannelID: "C1", TriggerID: "1.2.abc", ResponseURL: "https://hooks.slack.com/commands/T/1/x"}, p)
}

// A default agent past the option cap is moved to the front of the list, so
// the cut never drops the preselected option.
func TestAskAgentModal_DefaultPastCapIsKept(t *testing.T) {
	a := pickerAdapter("kagent/a-102")
	agents := make([]pkga2a.AgentInfo, 0, modalMaxAgents+3)
	for i := 0; i < modalMaxAgents+3; i++ {
		agents = append(agents, pkga2a.AgentInfo{Name: fmt.Sprintf("a-%03d", i), Namespace: "kagent"})
	}
	view, err := a.askAgentModal(agents, askAgentRequest{Channel: "C1", User: "U1"})
	require.NoError(t, err)
	_, values, initial := modalOptions(t, view)
	require.Len(t, values, modalMaxAgents)
	require.Equal(t, "kagent/a-102", initial, "the default stays preselected")
	require.Equal(t, "kagent/a-102", values[0], "the default is moved to the front of the cut list")
	require.Equal(t, "kagent/a-000", values[1], "the rest keeps its order")
}
