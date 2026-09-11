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
	sel := view[bkBlocks].([]any)[0].(map[string]any)[bkElement].(map[string]any)
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

	view, err := a.askAgentModal(agents, slashCommandPayload{ChannelID: "C1", UserID: "U1", ResponseURL: "https://hooks/r"})
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
// preselection; the command's text prefills the question, cut to the input's
// max length; an empty text leaves the box empty.
func TestAskAgentModal_PrefillAndNoDefault(t *testing.T) {
	a := pickerAdapter("kagent/elsewhere")
	agents := []pkga2a.AgentInfo{{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"}}

	view, err := a.askAgentModal(agents, slashCommandPayload{ChannelID: "C1", UserID: "U1", Text: "  " + strings.Repeat("q", modalQuestionMax+10) + "  "})
	require.NoError(t, err)
	_, _, initial := modalOptions(t, view)
	require.Empty(t, initial)
	question := view[bkBlocks].([]any)[1].(map[string]any)[bkElement].(map[string]any)
	require.Equal(t, modalQuestionMax, len([]rune(question[bkInitialValue].(string))))

	view, err = a.askAgentModal(agents, slashCommandPayload{ChannelID: "C1", UserID: "U1", Text: "   "})
	require.NoError(t, err)
	question = view[bkBlocks].([]any)[1].(map[string]any)[bkElement].(map[string]any)
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

// The conversation marker round-trips through a block_id; anything without
// the prefix, malformed, or naming no agent yields nil rather than a
// half-filled binding.
func TestConversationMarker_EncodeDecode(t *testing.T) {
	m := conversationMarker{AgentRef: "kagent/sre", Initiator: "U1", EntryPoint: entryPointSlashCommand}
	id := m.encode()
	require.True(t, strings.HasPrefix(id, conversationMarkerPrefix))
	require.LessOrEqual(t, len(id), 255, "block_id cap")
	require.Equal(t, &m, decodeConversationMarker(id))

	require.Nil(t, decodeConversationMarker(`{"a":"kagent/sre","u":"U1"}`), "no prefix: not ours")
	require.Nil(t, decodeConversationMarker(conversationMarkerPrefix+`{"a":`), "malformed")
	require.Nil(t, decodeConversationMarker(conversationMarkerPrefix+`{"u":"U1"}`), "no agent")
	require.Nil(t, decodeConversationMarker(""))

	require.Equal(t, &m, conversationMarkerFromBlocks([]messageBlock{{BlockID: "other"}, {BlockID: id}}))
	require.Nil(t, conversationMarkerFromBlocks(nil))
}

func TestQuoteMrkdwn_PrefixesEveryLine(t *testing.T) {
	require.Equal(t, "> one\n> two", quoteMrkdwn("one\ntwo"))
	require.Equal(t, "> single", quoteMrkdwn("single"))
}
