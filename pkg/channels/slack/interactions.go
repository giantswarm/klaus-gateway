package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// slackOption is one selected option in a radio_buttons/checkboxes state entry.
type slackOption struct {
	Value string `json:"value"`
}

// blockActionState is the state of one action inside a block, as reported under
// state.values on a block_actions payload. selected_option is set for a
// radio_buttons group; selected_options for a checkboxes group.
type blockActionState struct {
	SelectedOption  *slackOption  `json:"selected_option"`
	SelectedOptions []slackOption `json:"selected_options"`
}

// interactionPayload is the subset of the Slack interactions payload we need.
type interactionPayload struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Message struct {
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	// ResponseURL updates the source message. Ephemeral messages (the
	// access-consent prompt) have no addressable ts, so they are updated this way.
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		// Value carries the button payload: a raw threadID for approve/deny/submit,
		// or JSON for choice ({t,c}) and access ({t,u}) buttons.
		Value string `json:"value"`
	} `json:"actions"`
	// State carries the current selection of stateful widgets (radio/checkbox),
	// keyed state.values[block_id][action_id]. Read on a Submit click.
	State struct {
		Values map[string]map[string]blockActionState `json:"values"`
	} `json:"state"`
}

// questionIndexFromBlockID extracts the question index from a multi-question
// form widget's block_id (hitlQGroupPrefix + "_<qi>"). It returns false for any
// block that is not a form group, so the single-question widget/section blocks
// are ignored.
func questionIndexFromBlockID(blockID string) (int, bool) {
	rest, ok := strings.CutPrefix(blockID, hitlQGroupPrefix+"_")
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(rest)
	if err != nil || i < 0 {
		return 0, false
	}
	return i, true
}

// choiceSelections reads a Submit's widget state in a single pass. flat is the
// de-duplicated choice indices selected across every radio/checkbox block, used
// for the single-question decision and the nothing-selected check; the section
// multi-select layout spreads one checkbox per block, so every block counts.
// byQuestion groups the indices per question for a multi-question form, reading
// only blocks whose block_id encodes a question index (see
// questionIndexFromBlockID). Option values are choice indices; a value that is
// not an index is ignored. Indices are de-duplicated and ordered throughout.
func choiceSelections(state struct {
	Values map[string]map[string]blockActionState `json:"values"`
}) (flat []int, byQuestion map[int][]int) {
	flatSet := map[int]struct{}{}
	perQuestion := map[int]map[int]struct{}{}
	record := func(qi int, isForm bool, v string) {
		ci, err := strconv.Atoi(v)
		if err != nil {
			return
		}
		flatSet[ci] = struct{}{}
		if isForm {
			if perQuestion[qi] == nil {
				perQuestion[qi] = map[int]struct{}{}
			}
			perQuestion[qi][ci] = struct{}{}
		}
	}
	for blockID, actions := range state.Values {
		qi, isForm := questionIndexFromBlockID(blockID)
		for _, st := range actions {
			if st.SelectedOption != nil {
				record(qi, isForm, st.SelectedOption.Value)
			}
			for _, opt := range st.SelectedOptions {
				record(qi, isForm, opt.Value)
			}
		}
	}
	byQuestion = make(map[int][]int, len(perQuestion))
	for qi, set := range perQuestion {
		byQuestion[qi] = slices.Sorted(maps.Keys(set))
	}
	return slices.Sorted(maps.Keys(flatSet)), byQuestion
}

// interactionsHandler serves POST /channels/slack/interactions.
type interactionsHandler struct {
	signingSecret string
	adapter       *Adapter
	ctx           context.Context //nolint:containedctx
}

func (h *interactionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	if err := VerifySignature(h.signingSecret, r.Header, body); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Slack encodes the payload as a form field named "payload".
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	raw := form.Get("payload")
	if raw == "" {
		http.Error(w, "missing payload", http.StatusBadRequest)
		return
	}

	var payload interactionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		http.Error(w, "invalid payload JSON", http.StatusBadRequest)
		return
	}

	// Acknowledge immediately so Slack doesn't show a timeout spinner.
	w.WriteHeader(http.StatusOK)
	go h.adapter.routeInteraction(h.ctx, payload)
}

// routeInteraction dispatches a parsed Slack block_actions payload to the
// pending HITL task. Shared by the HTTP interactions endpoint (Events API mode)
// and the Socket Mode interactive-envelope handler (dev mode), so buttons work
// identically in both deployments.
func (a *Adapter) routeInteraction(ctx context.Context, payload interactionPayload) {
	if payload.Type != payloadTypeBlockActions || len(payload.Actions) == 0 {
		return
	}

	action := payload.Actions[0]

	// The sign-in URL button opens its link in the browser; the click payload
	// carries the response_url that lets NotifyLinked replace the ephemeral
	// prompt once the account link completes.
	if action.ActionID == oboSignIn {
		a.storeSignInPrompt(payload.User.ID, payload.ResponseURL)
		return
	}

	switch action.ActionID {
	case connectorDismiss:
		a.handleConnectorDismiss(ctx, payload.User.ID, action.Value, payload.ResponseURL)
		return
	case connectorConnect:
		// URL button: the browser opens the consent flow itself, so there is
		// nothing for the gateway to do on the click.
		return
	}

	act, ok := classifyAction(action.ActionID)
	if !ok {
		return
	}

	// Access-consent buttons resolve to a grant decision, not an A2A task. They
	// carry an encoded thread+newcomer (one initiator may have several pending).
	if act.kind == accessAllow || act.kind == accessDeny {
		av, ok := decodeAccessValue(action.Value)
		if !ok {
			return
		}
		a.handleAccessDecision(ctx, av.Thread, av.User, payload.User.ID, payload.ResponseURL, act.kind == accessAllow)
		return
	}

	// The threadID is in the button value. Choice buttons encode it as JSON;
	// approve/deny/submit buttons carry it raw.
	threadID := action.Value
	switch act.kind {
	case hitlChoice:
		cv, ok := decodeChoiceValue(action.Value)
		if !ok {
			return
		}
		threadID = cv.Thread
		act.choice = cv
	case hitlSubmit:
		// choices is the flat selection across every widget (used for the
		// single-question decision and the nothing-selected check); answers keeps
		// selections grouped per question for a multi-question form.
		act.choices, act.answers = choiceSelections(payload.State)
		if len(act.choices) == 0 {
			// Submit with nothing selected: nudge, leaving the task (and widget)
			// pending so the user can pick and submit again.
			if err := a.apiClient().postEphemeralText(ctx, payload.Channel.ID, payload.User.ID, threadID, choiceSelectNudge); err != nil {
				a.Logger.Warn("slack: post choice-select nudge failed", "thread", threadID, "error", err)
			}
			return
		}
	}

	if err := a.handleDecision(ctx, payload.Channel.ID, threadID, payload.Container.MessageTS, payload.User.ID, act); err != nil {
		if !errors.Is(err, context.Canceled) {
			a.Logger.Error("slack interactions: decision error", "thread", threadID, "error", err)
		}
	}
}

// hitlAction is a decoded Block Kit button click.
type hitlAction struct {
	kind    string // hitlApprove, hitlDeny, hitlChat, hitlChoice, or hitlSubmit
	choice  choiceValue
	choices []int         // selected choice indices, for a single-question hitlSubmit
	answers map[int][]int // selected choice indices per question, for a multi-question form hitlSubmit
}

// classifyAction maps a Block Kit action_id to a hitlAction.
func classifyAction(actionID string) (hitlAction, bool) {
	switch {
	case actionID == hitlApprove:
		return hitlAction{kind: hitlApprove}, true
	case actionID == hitlDeny:
		return hitlAction{kind: hitlDeny}, true
	case actionID == hitlChat:
		return hitlAction{kind: hitlChat}, true
	case actionID == hitlSubmit:
		return hitlAction{kind: hitlSubmit}, true
	case actionID == accessAllow:
		return hitlAction{kind: accessAllow}, true
	case actionID == accessDeny:
		return hitlAction{kind: accessDeny}, true
	case strings.HasPrefix(actionID, hitlChoice):
		return hitlAction{kind: hitlChoice}, true
	}
	return hitlAction{}, false
}

// validConnectorName bounds a backend name arriving in a button value:
// interaction payloads are attacker-shaped input, so an empty or oversized
// value is dropped rather than persisted.
func validConnectorName(server string) bool {
	return server != "" && len(server) <= maxConnectorNameLen
}

// handleConnectorDismiss acknowledges a "Not now" click and replaces the
// ephemeral prompt. The prompt cooldown already suppresses re-prompts for the
// backend, so no state is cleared here.
func (a *Adapter) handleConnectorDismiss(ctx context.Context, slackUser, server, responseURL string) {
	if !validConnectorName(server) {
		return
	}
	text := "_Okay, I won't ask again for a while._"
	if err := respondURL(ctx, responseURL, text); err != nil {
		a.Logger.Warn("slack: update connector prompt (dismissed) failed", "user", slackUser, "server", server, "error", err)
	}
}

// handleAccessDecision resolves the initiator's Yes/No on a newcomer's request
// to instruct the agent. On Yes the newcomer is granted (additively) and their
// held message is replayed through dispatch; on No it is discarded. The
// ephemeral prompt is updated in place via the interaction response_url.
//
// Only the thread initiator may decide. The prompt is ephemeral to the
// initiator, but the grant authorises another user to act under the initiator's
// identity, so the clicker's identity is checked here too rather than trusting
// ephemeral delivery alone (fail closed on any mismatch).
func (a *Adapter) handleAccessDecision(ctx context.Context, threadID, newcomerID, clickerID, responseURL string, allow bool) {
	if initiator := a.accessPolicy().Initiator(threadID); initiator == "" || initiator != clickerID {
		a.Logger.Warn("slack: access decision from non-initiator ignored",
			"thread", threadID, "clicker", clickerID, "newcomer", newcomerID)
		return
	}

	parked := a.takePendingAccess(threadID, newcomerID)

	if !allow {
		if err := respondURL(ctx, responseURL, "🚫 _Declined._"); err != nil {
			a.Logger.Warn("slack: update access prompt (declined) failed", "thread", threadID, "error", err)
		}
		return
	}

	a.accessPolicy().Grant(threadID, newcomerID)
	if err := respondURL(ctx, responseURL, fmt.Sprintf("✅ _<@%s> allowed._", newcomerID)); err != nil {
		a.Logger.Warn("slack: update access prompt (allowed) failed", "thread", threadID, "error", err)
	}

	// Nothing parked (e.g. the newcomer's messages expired or were already
	// handled); the grant still stands, so their next message routes without a
	// prompt.
	//
	// replayDispatch (not dispatch) so a thread slot held by an in-flight turn
	// defers the replay until the slot frees instead of dropping a message with
	// a busy notice right after the newcomer was told they are allowed. It is
	// blocking, which keeps a multi-message queue in order; this runs on the
	// interaction's own goroutine, so waiting here stalls nothing else.
	for _, req := range parked {
		if err := a.replayDispatch(ctx, req.msg, req.slackChannel); err != nil && !errors.Is(err, context.Canceled) {
			a.Logger.Error("slack: replay after access grant failed",
				"thread", threadID, "user", newcomerID, "error", err)
		}
	}
}

// handleDecision processes a button click: updates the prompt message to show
// the decision, then resumes (or cancels) the pending A2A task with a
// structured HITL decision.
func (a *Adapter) handleDecision(ctx context.Context, slackChannel, threadID, messageTS, slackUser string, act hitlAction) error {
	client := a.apiClient()

	// The approval buttons are posted in-thread and visible to everyone, but the
	// tool call runs under the initiator's identity, so only a permitted user (the
	// initiator or a granted collaborator) may approve or cancel it. An onlooker
	// click is refused ephemerally and the pending task is left intact.
	if !a.accessPolicy().Allowed(threadID, slackUser) {
		if err := client.postEphemeralText(ctx, slackChannel, slackUser, threadID, accessDecisionRefusal); err != nil {
			a.Logger.Warn("slack: post decision refusal failed", "thread", threadID, "user", slackUser, "error", err)
		}
		return nil
	}

	// Serialize the resume with any concurrent turn on this thread (typed reply
	// or another click). Acquire before taking the pending task so a rejected
	// click leaves the task and the button intact for a retry.
	if !a.acquireThread(threadID) {
		if _, err := client.postMessage(ctx, slackChannel, busyNotice, threadID); err != nil {
			a.Logger.Warn("slack: post busy notice failed", "thread", threadID, "error", err)
		}
		return nil
	}
	released := false
	release := func() {
		if !released {
			released = true
			a.releaseThread(threadID)
		}
	}
	defer release()

	// Peek without consuming: the completeness gate and the human-token gate both
	// run before the task is taken, so an incomplete form or a failed mint leaves
	// the pending task (and its buttons) intact. The thread lock is held, so the
	// task cannot be consumed between this peek and takePendingTask below.
	pending := a.peekPendingTask(threadID)
	if pending == nil {
		// Nothing pending (already answered). Still tidy up the buttons.
		_ = client.chatUpdateBlocks(ctx, slackChannel, messageTS, "_Already answered._")
		return nil
	}

	// A multi-question form must be fully answered before it resumes: an
	// unanswered question would reach the agent as an empty answer slot. Nudge and
	// leave the form pending, without minting a token or taking the task.
	if act.kind == hitlSubmit && pending.Prompt != nil && len(pending.Prompt.Questions) > 1 {
		if unansweredQuestions(pending.Prompt, act.answers) {
			if err := client.postEphemeralText(ctx, slackChannel, slackUser, threadID, formIncompleteNudge); err != nil {
				a.Logger.Warn("slack: post form-incomplete nudge failed", "thread", threadID, "error", err)
			}
			return nil
		}
	}

	// A button-click resume is a turn like any other: it must carry the clicking
	// user's human token, never the gateway's machine identity. Resolve it BEFORE
	// consuming the task or rewriting the message, so a mint failure leaves the
	// task and buttons untouched and the click stays retryable.
	token, ok, signIn := a.humanToken(ctx, slackChannel, threadID, slackUser)
	if signIn {
		// A button resume has no message to replay, so just prompt; the clicker
		// signs in and clicks again.
		a.postSignIn(ctx, slackChannel, threadID, slackUser)
	}
	if !ok {
		return nil
	}

	// takePendingTask clears the entry atomically — if the user also typed a
	// reply the first one wins and the other starts a fresh task.
	task := a.takePendingTask(threadID)
	if task == nil {
		// A concurrent reply consumed it between the peek and here.
		_ = client.chatUpdateBlocks(ctx, slackChannel, messageTS, "_Already answered._")
		return nil
	}

	// Chat: keep the approval pending and swap the buttons for a reply hint. The
	// next in-thread reply resolves the task through the normal free-text path
	// (decisionFromText), which turns a follow-up question into a reject carrying
	// it as the reason, so the agent answers and asks to confirm again — while a
	// plain "approve"/"deny" reply still decides directly.
	if act.kind == hitlChat {
		a.storePendingTask(threadID, task)
		// Release the slot before the Slack round-trip: the user is invited to
		// type their question right away, and a reply arriving while the slot is
		// still held would bounce off the busy notice instead of resuming the task.
		release()
		if err := client.chatUpdateBlocks(ctx, slackChannel, messageTS, chatModePrompt); err != nil {
			a.Logger.Warn("slack: update prompt for chat mode failed", "error", err)
		}
		return nil
	}

	decision, resumeText, decisionText := buildButtonDecision(act, task.Prompt)

	// Replace the Block Kit buttons with the decision text.
	if err := client.chatUpdateBlocks(ctx, slackChannel, messageTS, decisionText); err != nil {
		a.Logger.Warn("slack: update approval message failed", "error", err)
	}

	msg := channels.InboundMessage{
		Channel:     ChannelName,
		ChannelID:   task.ChannelID,
		ThreadID:    threadID,
		Subject:     slackUser,
		AgentRef:    task.AgentRef,
		TaskID:      task.TaskID,
		Text:        resumeText,
		Decision:    decision,
		BearerToken: token,
	}

	// Resolve email for the button-clicking user.
	a.resolveSubjectEmail(ctx, &msg)

	// A failure before the stream is running re-stores the taken task: the
	// buttons already show the decision, but a typed reply can still resume it.
	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		a.storePendingTask(threadID, task)
		return err
	}

	turnCtx, done := a.registerTurn(ctx, threadID)
	defer done()

	a.logTurnDispatch(msg, slackUser, true)

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		a.storePendingTask(threadID, task)
		return err
	}

	// Button resume: no user message to react to, so use text progress. The turn
	// context feeds the stream so /stop cancels it; the paused turn's usage
	// carries over so /usage reports the whole turn; the reply is branded as the
	// agent (the answer is the agent's).
	return a.streamResponse(turnCtx, a.agentClient(ctx, task.AgentRef), deltas, msg, slackUser, slackChannel, threadID, "", "_continuing…_", task.Usage)
}

// buildButtonDecision turns a Block Kit click into a structured HITL decision,
// the human-readable resume label, and the text the prompt message is updated
// to after the click.
func buildButtonDecision(act hitlAction, prompt *channels.HitlPrompt) (*channels.HitlDecision, string, string) {
	switch act.kind {
	case hitlChoice:
		label := choiceLabel(prompt, act.choice)
		decision := &channels.HitlDecision{
			Type:           channels.DecisionApprove,
			AskUserAnswers: [][]string{{label}},
		}
		// The label is agent-authored; it re-enters Slack via chat.update text.
		return decision, label, "👉 _" + escapeMrkdwn(label) + "_"
	case hitlSubmit:
		if prompt != nil && len(prompt.Questions) > 1 {
			answers := answersByQuestion(prompt, act.answers)
			decision := &channels.HitlDecision{
				Type:           channels.DecisionApprove,
				AskUserAnswers: answers,
			}
			resume, display := formResumeText(prompt, answers)
			return decision, resume, display
		}
		labels := choiceLabels(prompt, act.choices)
		decision := &channels.HitlDecision{
			Type:           channels.DecisionApprove,
			AskUserAnswers: [][]string{labels},
		}
		joined := strings.Join(labels, ", ")
		return decision, joined, "👉 _" + escapeMrkdwn(joined) + "_"
	case hitlDeny:
		return &channels.HitlDecision{Type: channels.DecisionReject}, "denied", "❌ _Denied._"
	default: // hitlApprove
		return &channels.HitlDecision{Type: channels.DecisionApprove}, labelApproved, "✅ _Approved._"
	}
}

// choiceLabel resolves a choice button's option label from the stored prompt's
// single question, falling back to a generic label when it is unavailable.
func choiceLabel(prompt *channels.HitlPrompt, cv choiceValue) string {
	if prompt != nil && len(prompt.Questions) > 0 {
		q := prompt.Questions[0]
		if cv.Choice >= 0 && cv.Choice < len(q.Choices) {
			return q.Choices[cv.Choice]
		}
	}
	return "selected option"
}

// choiceLabels resolves the option labels for a set of selected choice indices
// against the stored prompt's single question, dropping out-of-range indices.
// Falls back to a generic label when no index resolves.
func choiceLabels(prompt *channels.HitlPrompt, indices []int) []string {
	var choices []string
	if prompt != nil && len(prompt.Questions) > 0 {
		choices = prompt.Questions[0].Choices
	}
	labels := make([]string, 0, len(indices))
	for _, i := range indices {
		if i >= 0 && i < len(choices) {
			labels = append(labels, choices[i])
		}
	}
	if len(labels) == 0 {
		return []string{"selected option"}
	}
	return labels
}

// unansweredQuestions reports whether any question of a multi-question form has
// no resolved selection in selected.
func unansweredQuestions(prompt *channels.HitlPrompt, selected map[int][]int) bool {
	for qi := range prompt.Questions {
		if len(selected[qi]) == 0 {
			return true
		}
	}
	return false
}

// answersByQuestion resolves each question's selected choice indices into option
// labels, one slot per question in order. Out-of-range indices are dropped; a
// question with no resolvable selection yields an empty slot.
func answersByQuestion(prompt *channels.HitlPrompt, selected map[int][]int) [][]string {
	answers := make([][]string, len(prompt.Questions))
	for qi, q := range prompt.Questions {
		labels := make([]string, 0, len(selected[qi]))
		for _, ci := range selected[qi] {
			if ci >= 0 && ci < len(q.Choices) {
				labels = append(labels, q.Choices[ci])
			}
		}
		answers[qi] = labels
	}
	return answers
}

// formResumeText builds the human-readable resume label (msg.Text) and the
// display text that replaces the form after Submit. Both list each question's
// answer; question and choice text are agent-authored, so they are escaped.
func formResumeText(prompt *channels.HitlPrompt, answers [][]string) (resume, display string) {
	var resumeB, displayB strings.Builder
	for qi, q := range prompt.Questions {
		joined := strings.Join(answers[qi], ", ")
		if qi > 0 {
			resumeB.WriteString("; ")
		}
		resumeB.WriteString(joined)
		fmt.Fprintf(&displayB, "👉 *%s* _%s_\n", escapeMrkdwn(q.Question), escapeMrkdwn(joined))
	}
	return resumeB.String(), strings.TrimRight(displayB.String(), "\n")
}
