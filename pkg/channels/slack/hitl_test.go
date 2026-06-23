package slack

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func askUserPrompt(multiple bool, choices ...string) *channels.HitlPrompt {
	return &channels.HitlPrompt{
		ToolName:  channels.AskUserToolName,
		Questions: []channels.HitlQuestion{{Question: "q?", Multiple: multiple, Choices: choices}},
	}
}

func TestDecisionFromText_AskUserSingle(t *testing.T) {
	d := decisionFromText(askUserPrompt(false, "A", "B"), "Health check")
	require.NotNil(t, d)
	require.Equal(t, channels.DecisionApprove, d.Type)
	require.Equal(t, [][]string{{"Health check"}}, d.AskUserAnswers)
}

func TestDecisionFromText_AskUserMultiSelect(t *testing.T) {
	d := decisionFromText(askUserPrompt(true, "Auth", "Caching", "Logging"), "Auth, Caching")
	require.Equal(t, [][]string{{"Auth", "Caching"}}, d.AskUserAnswers)
}

func TestDecisionFromText_AskUserMultiQuestion(t *testing.T) {
	p := &channels.HitlPrompt{
		ToolName: channels.AskUserToolName,
		Questions: []channels.HitlQuestion{
			{Question: "db?"},
			{Question: "features?", Multiple: true},
		},
	}
	d := decisionFromText(p, "PostgreSQL\nAuth, Caching")
	require.Equal(t, [][]string{{"PostgreSQL"}, {"Auth", "Caching"}}, d.AskUserAnswers)
}

func TestDecisionFromText_GenericApproveDeny(t *testing.T) {
	generic := &channels.HitlPrompt{ToolName: "delete_file"}

	require.Equal(t, channels.DecisionApprove, decisionFromText(generic, "yes").Type)
	require.Equal(t, channels.DecisionReject, decisionFromText(generic, "no").Type)

	// Ambiguous text never silently approves a side-effecting tool.
	amb := decisionFromText(generic, "maybe later")
	require.Equal(t, channels.DecisionReject, amb.Type)
	require.Equal(t, "maybe later", amb.RejectionReason)
}

func TestDecisionFromText_NilPromptIsPlainText(t *testing.T) {
	require.Nil(t, decisionFromText(nil, "anything"))
}

func TestBuildButtonDecision_Choice(t *testing.T) {
	prompt := askUserPrompt(false, "Investigate", "Health check")
	act := hitlAction{kind: hitlChoice, choice: choiceValue{Question: 0, Choice: 1}}

	decision, resume, display := buildButtonDecision(act, prompt)
	require.Equal(t, channels.DecisionApprove, decision.Type)
	require.Equal(t, [][]string{{"Health check"}}, decision.AskUserAnswers)
	require.Equal(t, "Health check", resume)
	require.Contains(t, display, "Health check")
}

func TestBuildButtonDecision_ApproveDeny(t *testing.T) {
	approve, _, _ := buildButtonDecision(hitlAction{kind: hitlApprove}, nil)
	require.Equal(t, channels.DecisionApprove, approve.Type)

	deny, _, _ := buildButtonDecision(hitlAction{kind: hitlDeny}, nil)
	require.Equal(t, channels.DecisionReject, deny.Type)
}

func TestChoiceValueRoundTrip(t *testing.T) {
	v := encodeChoiceValue("1700.0001", 2, 3)
	got, ok := decodeChoiceValue(v)
	require.True(t, ok)
	require.Equal(t, choiceValue{Thread: "1700.0001", Question: 2, Choice: 3}, got)

	_, ok = decodeChoiceValue("not json")
	require.False(t, ok)
}
