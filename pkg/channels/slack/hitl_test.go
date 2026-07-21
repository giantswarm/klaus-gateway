package slack

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	act := hitlAction{kind: hitlChoice, choice: choiceValue{Choice: 1}}

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
	v := encodeChoiceValue("1700.0001", 3)
	got, ok := decodeChoiceValue(v)
	require.True(t, ok)
	require.Equal(t, choiceValue{Thread: "1700.0001", Choice: 3}, got)

	_, ok = decodeChoiceValue("not json")
	require.False(t, ok)
}

// A Block Kit approval prompt that Slack rejects falls back to a plain-text
// prompt: the pending task is already stored, so a thread with no visible
// prompt strands it (nothing tells the user a reply would resume it).
func TestPostHitlPrompt_FallsBackToTextOnBlockKitFailure(t *testing.T) {
	var mu sync.Mutex
	var plainTexts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"blocks"`) {
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_blocks"}`))
			return
		}
		// postMessage sends form-encoded params (Block Kit posts send JSON).
		values, _ := url.ParseQuery(string(body))
		mu.Lock()
		plainTexts = append(plainTexts, values.Get("text"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(srv.Close)

	a := &Adapter{
		APIBase: srv.URL,
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		Logger:  slog.New(slog.DiscardHandler),
	}

	// Generic tool approval: Approve/Deny buttons fail, plain text lands.
	err := a.postHitlPrompt(t.Context(), a.apiClient(), "C1", "T1", &channels.OutboundDelta{
		Content: "Run kubectl delete?",
		Prompt:  &channels.HitlPrompt{},
	})
	require.NoError(t, err)

	// Single-select ask_user: choice buttons fail, text rendering lands.
	err = a.postHitlPrompt(t.Context(), a.apiClient(), "C1", "T1", &channels.OutboundDelta{
		Prompt: askUserPrompt(false, "yes", "no"),
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, plainTexts, 2)
	require.Contains(t, plainTexts[0], "Run kubectl delete?")
	require.Contains(t, plainTexts[0], "approve")
	require.Contains(t, plainTexts[1], "Reply in this thread")
}
