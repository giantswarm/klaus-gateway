package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// choiceValue is encoded into a section-accessory choice button's value so the
// interaction handler can map a click back to the selected option. Only
// single-question ask_user prompts render interactively, so no question index
// is needed. The radio/checkbox widget path carries indices in state.values
// instead and does not use this.
type choiceValue struct {
	Thread string `json:"t"`
	Choice int    `json:"c"`
}

func encodeChoiceValue(threadID string, c int) string {
	b, _ := json.Marshal(choiceValue{Thread: threadID, Choice: c})
	return string(b)
}

func decodeChoiceValue(s string) (choiceValue, bool) {
	var v choiceValue
	if err := json.Unmarshal([]byte(s), &v); err != nil || v.Thread == "" {
		return choiceValue{}, false
	}
	return v, true
}

// accessValue is encoded into an access-consent button's value so the
// interaction handler can map a click back to the thread and the newcomer the
// initiator is deciding on.
type accessValue struct {
	Thread string `json:"t"`
	User   string `json:"u"`
}

func encodeAccessValue(threadID, userID string) string {
	b, _ := json.Marshal(accessValue{Thread: threadID, User: userID})
	return string(b)
}

func decodeAccessValue(s string) (accessValue, bool) {
	var v accessValue
	if err := json.Unmarshal([]byte(s), &v); err != nil || v.Thread == "" || v.User == "" {
		return accessValue{}, false
	}
	return v, true
}

// choiceRender selects how a single ask_user question's choices are presented.
type choiceRender int

const (
	renderText    choiceRender = iota // numbered list, reply free-text in-thread
	renderWidget                      // radio_buttons (single) / checkboxes (multi) + Submit
	renderSection                     // one section per choice + accessory, for long labels
)

// chooseChoiceRender picks the render mode for a single ask_user question. A
// widget carries choice labels of at most choiceLabelWidgetMax runes; a longer
// label forces the section layout so nothing truncates. More than
// maxChoiceOptions choices (or none) falls back to text.
func chooseChoiceRender(q channels.HitlQuestion) choiceRender {
	if len(q.Choices) == 0 || len(q.Choices) > maxChoiceOptions {
		return renderText
	}
	for _, c := range q.Choices {
		if len([]rune(c)) > choiceLabelWidgetMax {
			return renderSection
		}
	}
	return renderWidget
}

// postHitlPrompt renders the appropriate Slack prompt for a paused
// input-required task: an interactive choice widget for a single-question
// ask_user, Approve/Deny for a generic tool approval, and a free-text fallback
// for everything else. The user can always answer by replying in-thread.
func (a *Adapter) postHitlPrompt(ctx context.Context, client *slackAPIClient, slackChannel, threadID string, pd *channels.OutboundDelta) error {
	p := pd.Prompt

	// The pending task is already stored when this runs, so a prompt that never
	// reaches the thread strands the paused task invisibly: the user has no cue
	// that typing a reply would resume it. A failed Block Kit post therefore
	// falls back to the plain-text rendering (the free-text reply path resolves
	// the task either way).
	if p.IsAskUser() {
		// Only a single-question prompt renders interactively; multiple questions
		// need one answer line each, which the text path handles.
		if len(p.Questions) == 1 {
			q := p.Questions[0]
			var err error
			interactive := true
			switch chooseChoiceRender(q) {
			case renderWidget:
				err = client.postChoiceWidgetPrompt(ctx, slackChannel, threadID, q.Question, q.Choices, q.Multiple)
			case renderSection:
				err = client.postChoiceSectionPrompt(ctx, slackChannel, threadID, q.Question, q.Choices, q.Multiple)
			default:
				interactive = false
			}
			if interactive {
				if err == nil {
					return nil
				}
				a.Logger.Warn("slack: choice prompt failed, falling back to text", "thread", threadID, "error", err)
			}
		}
		_, err := client.postMessage(ctx, slackChannel, renderAskUserText(p), threadID)
		return err
	}

	// Generic tool approval → Approve/Deny.
	text := pd.Content
	if text == "" {
		text = "_Waiting for approval…_"
	}
	err := client.postApprovalPrompt(ctx, slackChannel, threadID, text)
	if err == nil {
		return nil
	}
	a.Logger.Warn("slack: approval prompt failed, falling back to text", "thread", threadID, "error", err)
	_, err = client.postMessage(ctx, slackChannel, text+"\n\n_Reply *approve* or *deny* in this thread._", threadID)
	return err
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
		b.WriteString(escapeMrkdwn(q.Question))
		b.WriteString("*")
		for j, c := range q.Choices {
			fmt.Fprintf(&b, "\n  %d. %s", j+1, escapeMrkdwn(c))
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
	for part := range strings.SplitSeq(s, ",") {
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
	"approve": true, labelApproved: true, wordYes: true, "y": true, "ok": true,
	"okay": true, "go": true, "proceed": true, "confirm": true, "do it": true,
}

var denyWords = map[string]bool{
	"deny": true, "denied": true, "no": true, "n": true, "reject": true,
	"cancel": true, "abort": true, cmdStop: true, "/" + cmdStop: true,
}

func isApproveWord(text string) bool {
	return approveWords[strings.ToLower(strings.TrimSpace(text))]
}

func isDenyWord(text string) bool {
	return denyWords[strings.ToLower(strings.TrimSpace(text))]
}
