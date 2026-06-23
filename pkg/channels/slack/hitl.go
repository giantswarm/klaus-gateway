package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// choiceValue is encoded into an ask_user choice button's value so the
// interaction handler can map a click back to a question + option.
type choiceValue struct {
	Thread   string `json:"t"`
	Question int    `json:"q"`
	Choice   int    `json:"c"`
}

func encodeChoiceValue(threadID string, q, c int) string {
	b, _ := json.Marshal(choiceValue{Thread: threadID, Question: q, Choice: c})
	return string(b)
}

func decodeChoiceValue(s string) (choiceValue, bool) {
	var v choiceValue
	if err := json.Unmarshal([]byte(s), &v); err != nil || v.Thread == "" {
		return choiceValue{}, false
	}
	return v, true
}

// postHitlPrompt renders the appropriate Slack prompt for a paused
// input-required task: per-choice buttons for a simple single-select ask_user
// question, Approve/Deny for a generic tool approval, and a free-text fallback
// for everything else. The user can always answer by replying in-thread.
func (a *Adapter) postHitlPrompt(ctx context.Context, client *slackAPIClient, slackChannel, threadID string, pd *channels.OutboundDelta) error {
	p := pd.Prompt

	// Simple single-select ask_user → one button per choice.
	if p.IsAskUser() && len(p.Questions) == 1 {
		q := p.Questions[0]
		if !q.Multiple && len(q.Choices) > 0 && len(q.Choices) <= maxChoiceButtons {
			return client.postChoicePrompt(ctx, slackChannel, threadID, q.Question, q.Choices)
		}
	}

	// ask_user that doesn't fit buttons → render questions as text, answer free-text.
	if p.IsAskUser() {
		_, err := client.postMessage(ctx, slackChannel, renderAskUserText(p), threadID)
		return err
	}

	// Generic tool approval → Approve/Deny.
	text := pd.Content
	if text == "" {
		text = "_Waiting for approval…_"
	}
	return client.postApprovalPrompt(ctx, slackChannel, threadID, text, pd.TaskID)
}

// renderAskUserText renders all questions and their choices as mrkdwn, with an
// instruction to reply in-thread.
func renderAskUserText(p *channels.HitlPrompt) string {
	var b strings.Builder
	for i, q := range p.Questions {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("*")
		b.WriteString(q.Question)
		b.WriteString("*")
		for j, c := range q.Choices {
			fmt.Fprintf(&b, "\n  %d. %s", j+1, c)
		}
	}
	if len(p.Questions) > 1 {
		b.WriteString("\n\n_Reply in this thread, one line per question._")
	} else {
		b.WriteString("\n\n_Reply in this thread with your answer._")
	}
	return b.String()
}

// decisionFromText maps a free-text in-thread reply to a structured HITL
// decision, given the prompt the thread is paused on. Returns nil when there is
// no structured prompt (the reply is sent as plain text — the legacy path).
func decisionFromText(p *channels.HitlPrompt, text string) *channels.HitlDecision {
	if p == nil {
		return nil
	}
	if p.IsAskUser() {
		return &channels.HitlDecision{
			Type:           channels.DecisionApprove,
			AskUserAnswers: answersFromText(p.Questions, text),
		}
	}
	// Generic approval: interpret approve/deny keywords; anything ambiguous is
	// treated as a rejection carrying the text as the reason (never silently
	// approve a side-effecting tool).
	if isApproveWord(text) {
		return &channels.HitlDecision{Type: channels.DecisionApprove}
	}
	if isDenyWord(text) {
		return &channels.HitlDecision{Type: channels.DecisionReject}
	}
	return &channels.HitlDecision{Type: channels.DecisionReject, RejectionReason: strings.TrimSpace(text)}
}

// answersFromText builds positional ask_user answers from a free-text reply.
// One question → the whole reply (split on comma for multi-select). Multiple
// questions → one line per question, in order.
func answersFromText(questions []channels.HitlQuestion, text string) [][]string {
	text = strings.TrimSpace(text)
	if len(questions) <= 1 {
		multiple := len(questions) == 1 && questions[0].Multiple
		return [][]string{splitAnswer(text, multiple)}
	}
	lines := strings.Split(text, "\n")
	answers := make([][]string, len(questions))
	for i := range questions {
		line := ""
		if i < len(lines) {
			line = strings.TrimSpace(lines[i])
		}
		answers[i] = splitAnswer(line, questions[i].Multiple)
	}
	return answers
}

// splitAnswer turns one answer string into the answer list. Multi-select splits
// on commas; single-select keeps the whole string as one answer.
func splitAnswer(s string, multiple bool) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	if !multiple {
		return []string{s}
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

var approveWords = map[string]bool{
	"approve": true, "approved": true, "yes": true, "y": true, "ok": true,
	"okay": true, "go": true, "proceed": true, "confirm": true, "do it": true,
}

var denyWords = map[string]bool{
	"deny": true, "denied": true, "no": true, "n": true, "reject": true,
	"cancel": true, "abort": true, "stop": true,
}

func isApproveWord(text string) bool {
	return approveWords[strings.ToLower(strings.TrimSpace(text))]
}

func isDenyWord(text string) bool {
	return denyWords[strings.ToLower(strings.TrimSpace(text))]
}
