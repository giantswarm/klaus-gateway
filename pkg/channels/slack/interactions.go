package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

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
		// Value carries the button payload: a raw threadID for approve/deny, or
		// JSON for choice ({t,c}) and access ({t,u}) buttons.
		Value string `json:"value"`
	} `json:"actions"`
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
	if payload.Type != "block_actions" || len(payload.Actions) == 0 {
		return
	}

	action := payload.Actions[0]
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
	// approve/deny buttons carry it raw.
	threadID := action.Value
	if act.kind == hitlChoice {
		cv, ok := decodeChoiceValue(action.Value)
		if !ok {
			return
		}
		threadID = cv.Thread
		act.choice = cv
	}

	if err := a.handleDecision(ctx, payload.Channel.ID, threadID, payload.Container.MessageTS, payload.User.ID, act); err != nil {
		if !errors.Is(err, context.Canceled) {
			a.Logger.Error("slack interactions: decision error", "thread", threadID, "error", err)
		}
	}
}

// hitlAction is a decoded Block Kit button click.
type hitlAction struct {
	kind   string // hitlApprove, hitlDeny, or hitlChoice
	choice choiceValue
}

// classifyAction maps a Block Kit action_id to a hitlAction.
func classifyAction(actionID string) (hitlAction, bool) {
	switch {
	case actionID == hitlApprove:
		return hitlAction{kind: hitlApprove}, true
	case actionID == hitlDeny:
		return hitlAction{kind: hitlDeny}, true
	case actionID == accessAllow:
		return hitlAction{kind: accessAllow}, true
	case actionID == accessDeny:
		return hitlAction{kind: accessDeny}, true
	case strings.HasPrefix(actionID, hitlChoice):
		return hitlAction{kind: hitlChoice}, true
	}
	return hitlAction{}, false
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

	req := a.takePendingAccess(threadID, newcomerID)

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

	// Nothing parked (e.g. the newcomer's message expired or was already handled);
	// the grant still stands, so their next message routes without a prompt.
	if req == nil {
		return
	}
	if err := a.dispatch(ctx, req.msg, req.slackChannel); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Error("slack: replay after access grant failed", "thread", threadID, "user", newcomerID, "error", err)
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
	defer a.releaseThread(threadID)

	// Resolve the clicker's human token before taking the pending task: a resume
	// that cannot run (unlinked clicker, token error) must leave the task and the
	// buttons intact, and an approved tool call must execute under the approver's
	// identity, never the gateway service account (klaus-gateway#116).
	token, ok := a.humanToken(ctx, slackChannel, threadID, slackUser, true)
	if !ok {
		return nil
	}

	// takePendingTask clears the entry atomically — if the user also typed a
	// reply the first one wins and the other starts a fresh task.
	task := a.takePendingTask(threadID)
	if task == nil {
		// Nothing pending (already answered). Still tidy up the buttons.
		_ = client.chatUpdateBlocks(ctx, slackChannel, messageTS, "_Already answered._")
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

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		a.storePendingTask(threadID, task)
		return err
	}

	// Button resume: no user message to react to, so use text progress. The turn
	// context feeds the stream so /stop cancels it; the paused turn's usage
	// carries over so /usage reports the whole turn.
	return a.streamResponse(turnCtx, client, deltas, msg, slackChannel, threadID, "", "_continuing…_", task.Usage)
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
