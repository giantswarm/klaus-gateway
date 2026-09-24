package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
	// Task is the A2A task the prompt was rendered for; see hitlValue.Task.
	Task string `json:"id,omitempty"`
}

func encodeChoiceValue(threadID, taskID string, c int) string {
	b, _ := json.Marshal(choiceValue{Thread: threadID, Choice: c, Task: taskID})
	return string(b)
}

func decodeChoiceValue(s string) (choiceValue, bool) {
	var v choiceValue
	if err := json.Unmarshal([]byte(s), &v); err != nil || v.Thread == "" {
		return choiceValue{}, false
	}
	return v, true
}

// hitlValue is encoded into the approve/deny/chat/submit button values. Thread
// routes the click; Task pins the prompt to the A2A task it was rendered for,
// so a click on a superseded prompt message (its task already resumed, the
// thread paused again on a different prompt) is refused instead of answering
// the newer prompt with selections the user never saw.
type hitlValue struct {
	Thread string `json:"t"`
	Task   string `json:"id,omitempty"`
}

func encodeHitlValue(threadID, taskID string) string {
	b, _ := json.Marshal(hitlValue{Thread: threadID, Task: taskID})
	return string(b)
}

// decodeHitlValue parses a button value into its thread and task. Prompt
// messages posted by older gateway versions carry the raw threadID; those
// decode with an empty Task, which skips the staleness check.
func decodeHitlValue(s string) hitlValue {
	var v hitlValue
	if err := json.Unmarshal([]byte(s), &v); err == nil && v.Thread != "" {
		return v
	}
	return hitlValue{Thread: s}
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

// formRenderable reports whether a multi-question ask_user prompt can render as
// a single interactive form: every question must be a radio/checkbox widget
// (1..maxChoiceOptions choices, each label within choiceLabelWidgetMax runes),
// and the question count must stay within the per-message block budget. A prompt
// with any free-text, over-long, or over-count question renders as text instead.
func formRenderable(p *channels.HitlPrompt) bool {
	if len(p.Questions) < 2 || len(p.Questions) > maxFormQuestions {
		return false
	}
	for _, q := range p.Questions {
		if chooseChoiceRender(q) != renderWidget {
			return false
		}
	}
	return true
}

// postHitlPrompt renders the appropriate Slack prompt for a paused
// input-required task: an interactive choice widget for a single-question
// ask_user, a form for a multi-question ask_user whose questions all fit a
// widget, Approve/Deny for a generic tool approval, and a free-text fallback for
// everything else. The user can always answer by replying in-thread.
func (a *Adapter) postHitlPrompt(ctx context.Context, client *slackAPIClient, slackChannel, threadID string, pd *channels.OutboundDelta) error {
	p := pd.Prompt

	// The pending task is already stored when this runs, so a prompt that never
	// reaches the thread strands the paused task invisibly: the user has no cue
	// that typing a reply would resume it. A failed Block Kit post therefore
	// falls back to the plain-text rendering (the free-text reply path resolves
	// the task either way).
	if p.IsAskUser() {
		// The question's message is recorded on the pending task, so a typed
		// answer rewrites it the way a click does.
		if len(p.Questions) == 1 {
			q := p.Questions[0]
			var ts string
			var err error
			interactive := true
			switch chooseChoiceRender(q) {
			case renderWidget:
				ts, err = client.postChoiceWidgetPrompt(ctx, slackChannel, threadID, pd.TaskID, q.Question, q.Choices, q.Multiple)
			case renderSection:
				ts, err = client.postChoiceSectionPrompt(ctx, slackChannel, threadID, pd.TaskID, q.Question, q.Choices, q.Multiple)
			default:
				interactive = false
			}
			if interactive {
				if err == nil {
					a.notePromptTS(threadID, pd.TaskID, ts)
					return nil
				}
				a.Logger.Warn("slack: choice prompt failed, falling back to text", "thread", threadID, "error", err)
			}
		} else if formRenderable(p) {
			ts, err := client.postChoiceFormPrompt(ctx, slackChannel, threadID, pd.TaskID, p.Questions)
			if err == nil {
				a.notePromptTS(threadID, pd.TaskID, ts)
				return nil
			}
			a.Logger.Warn("slack: form prompt failed, falling back to text", "thread", threadID, "error", err)
		}
		ts, err := client.postMessage(ctx, slackChannel, renderAskUserText(p), threadID)
		if err == nil {
			a.notePromptTS(threadID, pd.TaskID, ts)
		}
		return err
	}

	// Generic tool approval → the approval card, recorded on the pending task
	// like a question, so a typed approve or deny rewrites it as a click does.
	card := approvalCard(p, pd.Content)
	initiator := a.accessPolicy().Initiator(ctx, slackChannel, threadID)
	ts, err := client.postApprovalPrompt(ctx, slackChannel, threadID, pd.TaskID, card, initiator)
	if err == nil {
		a.notePromptTS(threadID, pd.TaskID, ts)
		return nil
	}
	a.Logger.Warn("slack: approval prompt failed, falling back to text", "thread", threadID, "error", err)
	_, err = client.postMessage(ctx, slackChannel, card+"\n\nReply *approve* or *deny* in this thread.", threadID)
	return err
}

// adkDefaultApprovalHint starts the hint the ADK runtime puts on every approval
// it asks for ("Please approve or reject the tool call call_tool() by
// responding with a FunctionResponse…", adk-go tool.WithConfirmation). It is
// written for the model, not the person, so the card leaves it out.
const adkDefaultApprovalHint = "Please approve or reject the tool call "

// approvalCard renders the section of a tool approval prompt: the ask and the
// calls it covers, named as the reply's task list names them, then the text the
// status carried of its own, without the runtime's default hint. text is the
// prompt delta's Content, used only for a prompt without structure (p nil),
// whose Content is the status's own text. A decision rewrites the prompt with
// the same section, rebuilt from the pending task.
func approvalCard(p *channels.HitlPrompt, text string) string {
	if p != nil {
		text = p.StatusText
	}
	card := "*" + approvalRequiredTitle + "*"
	if titles := approvalToolTitles(p); titles != "" {
		card += " · " + titles
	}
	if body := approvalHint(text); body != "" {
		card += "\n" + escapeMrkdwn(body)
	}
	return truncateRunes(card, slackSectionTextMax)
}

// approvalToolTitles names the calls an approval covers with their step
// titles, muster's call_tool unwrapped to the tool it runs.
func approvalToolTitles(p *channels.HitlPrompt) string {
	if p == nil {
		return ""
	}
	tools := p.Tools
	if len(tools) == 0 && p.ToolName != "" {
		tools = []channels.HitlTool{{Name: p.ToolName, Args: p.Args}}
	}
	titles := make([]string, 0, len(tools))
	for _, t := range tools {
		name := t.Name
		if inner, _, ok := unwrapCallTool(&channels.ToolActivity{Name: t.Name, Args: t.Args}); ok {
			name = inner
		}
		titles = append(titles, stepTitle(name))
	}
	return strings.Join(titles, ", ")
}

// approvalHint is the status's text without the lines that are the runtime's
// default hint, one per call it asks for; the agent's own words stay.
func approvalHint(text string) string {
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, adkDefaultApprovalHint) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// approvalDecisionLine is the context line that replaces an approval card's
// buttons after an Approve or Deny click, or "" for any other click.
func approvalDecisionLine(kind, user string, at time.Time) string {
	format := approvalApprovedBy
	switch kind {
	case hitlApprove:
	case hitlDeny:
		format = approvalDeniedBy
	default:
		return ""
	}
	return fmt.Sprintf(format, user, slackTime(at))
}

// slackTime renders t as a Slack date token: each reader sees the time in
// their own time zone, and the fallback (notifications, old clients) in UTC.
func slackTime(t time.Time) string {
	return fmt.Sprintf("<!date^%d^{time}|%s>", t.Unix(), t.UTC().Format("15:04 UTC"))
}

// approvalCardBlocks is an approval card after a click: the section it was
// posted with and one context line where the buttons were.
func approvalCardBlocks(card, line string) []any {
	return []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: card}},
		contextBlock(line),
	}
}

// questionAnsweredBlocks is a question prompt once it is answered, for the
// message the prompt was posted in: the questions stay, the controls go, and
// one context line says what was answered, by whom and when. A single
// question puts its answer in that line; a form puts each answer under its
// question. It returns the line, which is also the notification text. Question
// and answer text is agent- or user-authored, so it is escaped.
func questionAnsweredBlocks(p *channels.HitlPrompt, answers [][]string, user string, at time.Time) (string, []any) {
	var questions []channels.HitlQuestion
	if p != nil {
		questions = p.Questions
	}
	answer := func(i int) string {
		if i < len(answers) {
			return strings.Join(answers[i], ", ")
		}
		return ""
	}
	// The line is one context element, which Slack caps like a section; an
	// answer can be a pasted log or several long choices, and a line over the
	// cap would get the whole rewrite refused, leaving the controls live.
	var blocks []any
	var line string
	if len(questions) > 1 {
		for i, q := range questions {
			a := answer(i)
			if strings.TrimSpace(a) == "" {
				a = formNoAnswer
			}
			text := truncateRunes("*"+escapeMrkdwn(q.Question)+"*\n"+escapeMrkdwn(a), slackSectionTextMax)
			blocks = append(blocks, map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: text}})
		}
		line = fmt.Sprintf(formAnsweredFormat, user, slackTime(at))
	} else {
		if len(questions) == 1 {
			blocks = append(blocks, questionSection(questions[0].Question))
		}
		rest := utf8.RuneCountInString(fmt.Sprintf(questionAnsweredFormat, "", user, slackTime(at)))
		line = fmt.Sprintf(questionAnsweredFormat, truncateRunes(escapeMrkdwn(answer(0)), slackSectionTextMax-rest), user, slackTime(at))
	}
	return line, append(blocks, contextBlock(line))
}

// markPromptDecided rewrites a prompt that a typed reply decided the way a
// click rewrites it, so its controls do not stay live: a question names its
// answer, an approval card names who approved or denied it. A reply that is
// not an approve word denies (decisionFromText), so the card says so. A card
// posted for a status without a structured prompt has no decision to name:
// the reply goes to the agent as text, so the card only says who answered.
// Best-effort: the decision runs either way.
func (a *Adapter) markPromptDecided(ctx context.Context, slackChannel string, task *pendingTask, decision *channels.HitlDecision, slackUser string) {
	if task.PromptTS == "" || (task.Prompt != nil && decision == nil) {
		return
	}
	now := time.Now()
	var line string
	var blocks []any
	if task.Prompt == nil {
		line = fmt.Sprintf(formAnsweredFormat, slackUser, slackTime(now))
		blocks = approvalCardBlocks(approvalCard(nil, task.PromptText), line)
	} else if task.Prompt.IsAskUser() {
		line, blocks = questionAnsweredBlocks(task.Prompt, decision.AskUserAnswers, slackUser, now)
	} else {
		kind := hitlDeny
		if decision.Type == channels.DecisionApprove {
			kind = hitlApprove
		}
		line = approvalDecisionLine(kind, slackUser, now)
		blocks = approvalCardBlocks(approvalCard(task.Prompt, task.PromptText), line)
	}
	if err := a.apiClient().chatUpdate(ctx, slackChannel, task.PromptTS, line, blocks); err != nil {
		a.Logger.Warn("slack: rewrite decided prompt failed", "channel", slackChannel, "ts", task.PromptTS, "error", err)
	}
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
		b.WriteString("\n\nReply in this thread, one line per question.")
	} else {
		b.WriteString("\n\nReply in this thread with your answer.")
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
