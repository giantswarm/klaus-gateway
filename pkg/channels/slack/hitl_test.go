package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestChooseChoiceRender(t *testing.T) {
	shortN := func(n int) []string {
		s := make([]string, n)
		for i := range s {
			s[i] = "x"
		}
		return s
	}
	// choiceLabelWidgetMax runes of a 2-byte rune: over the byte limit but at
	// the rune limit, so it must still render as a widget (rune-counted, not bytes).
	atRuneCap := strings.Repeat("é", choiceLabelWidgetMax)
	overRuneCap := strings.Repeat("é", choiceLabelWidgetMax+1)

	for _, tc := range []struct {
		name string
		q    channels.HitlQuestion
		want choiceRender
	}{
		{"no choices", channels.HitlQuestion{}, renderText},
		{"single short", channels.HitlQuestion{Choices: []string{"a"}}, renderWidget},
		{"multi flag does not change mode", channels.HitlQuestion{Choices: []string{"a"}, Multiple: true}, renderWidget},
		{"at option cap", channels.HitlQuestion{Choices: shortN(maxChoiceOptions)}, renderWidget},
		{"over option cap", channels.HitlQuestion{Choices: shortN(maxChoiceOptions + 1)}, renderText},
		{"label at rune cap", channels.HitlQuestion{Choices: []string{atRuneCap}}, renderWidget},
		{"label over rune cap", channels.HitlQuestion{Choices: []string{overRuneCap}}, renderSection},
		{"long label wins over short peers", channels.HitlQuestion{Choices: []string{"a", overRuneCap}}, renderSection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, chooseChoiceRender(tc.q))
		})
	}
}

func TestFormRenderable(t *testing.T) {
	q := func(multiple bool, choices ...string) channels.HitlQuestion {
		return channels.HitlQuestion{Question: "q?", Multiple: multiple, Choices: choices}
	}
	longLabel := strings.Repeat("a", choiceLabelWidgetMax+1)
	manyChoices := make([]string, maxChoiceOptions+1)
	for i := range manyChoices {
		manyChoices[i] = "x"
	}
	manyQuestions := make([]channels.HitlQuestion, maxFormQuestions+1)
	for i := range manyQuestions {
		manyQuestions[i] = q(false, "a", "b")
	}

	for _, tc := range []struct {
		name string
		qs   []channels.HitlQuestion
		want bool
	}{
		{"two widgetable questions", []channels.HitlQuestion{q(false, "a", "b"), q(true, "c", "d")}, true},
		{"single question is not a form", []channels.HitlQuestion{q(false, "a", "b")}, false},
		{"a free-text question blocks the form", []channels.HitlQuestion{q(false, "a", "b"), q(false)}, false},
		{"an over-long label blocks the form", []channels.HitlQuestion{q(false, "a"), q(false, longLabel)}, false},
		{"an over-count question blocks the form", []channels.HitlQuestion{q(false, "a", "b"), q(false, manyChoices...)}, false},
		{"too many questions falls back to text", manyQuestions, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &channels.HitlPrompt{ToolName: channels.AskUserToolName, Questions: tc.qs}
			require.Equal(t, tc.want, formRenderable(p))
		})
	}
}

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
	act := hitlAction{kind: hitlChoice, choice: choiceValue{Choice: 1}}

	decision, resume, display := buildButtonDecision(act, prompt)
	require.Equal(t, channels.DecisionApprove, decision.Type)
	require.Equal(t, [][]string{{"Health check"}}, decision.AskUserAnswers)
	require.Equal(t, "Health check", resume)
	require.Contains(t, display, "Health check")
}

func TestBuildButtonDecision_Submit(t *testing.T) {
	prompt := askUserPrompt(true, "Auth", "Logging", "Caching")
	act := hitlAction{kind: hitlSubmit, choices: []int{0, 2}}

	decision, resume, display := buildButtonDecision(act, prompt)
	require.Equal(t, channels.DecisionApprove, decision.Type)
	require.Equal(t, [][]string{{"Auth", "Caching"}}, decision.AskUserAnswers)
	require.Equal(t, "Auth, Caching", resume)
	require.Contains(t, display, "Auth, Caching")
}

func TestBuildButtonDecision_SubmitForm(t *testing.T) {
	prompt := &channels.HitlPrompt{
		ToolName: channels.AskUserToolName,
		Questions: []channels.HitlQuestion{
			{Question: "Database?", Choices: []string{"PostgreSQL", "MySQL"}},
			{Question: "Features?", Multiple: true, Choices: []string{"Auth", "Logging", "Caching"}},
		},
	}
	act := hitlAction{kind: hitlSubmit, answers: map[int][]int{0: {1}, 1: {0, 2}}}

	decision, resume, display := buildButtonDecision(act, prompt)
	require.Equal(t, channels.DecisionApprove, decision.Type)
	require.Equal(t, [][]string{{"MySQL"}, {"Auth", "Caching"}}, decision.AskUserAnswers)
	require.Equal(t, "MySQL; Auth, Caching", resume)
	require.Contains(t, display, "Database?")
	require.Contains(t, display, "MySQL")
	require.Contains(t, display, "Auth, Caching")
}

func TestBuildButtonDecision_ApproveDeny(t *testing.T) {
	approve, _, _ := buildButtonDecision(hitlAction{kind: hitlApprove}, nil)
	require.Equal(t, channels.DecisionApprove, approve.Type)

	deny, _, _ := buildButtonDecision(hitlAction{kind: hitlDeny}, nil)
	require.Equal(t, channels.DecisionReject, deny.Type)
}

func TestChoiceValueRoundTrip(t *testing.T) {
	v := encodeChoiceValue("1700.0001", 3)
	got, ok := decodeChoiceValue(v)
	require.True(t, ok)
	require.Equal(t, choiceValue{Thread: "1700.0001", Choice: 3}, got)

	_, ok = decodeChoiceValue("not json")
	require.False(t, ok)
}
