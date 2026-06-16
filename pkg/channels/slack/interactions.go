package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

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
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"` // encodes threadID
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

	if payload.Type != "block_actions" || len(payload.Actions) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	action := payload.Actions[0]
	if action.ActionID != hitlApprove && action.ActionID != hitlDeny {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Acknowledge immediately so Slack doesn't show a timeout spinner.
	w.WriteHeader(http.StatusOK)

	threadID := action.Value
	slackChannel := payload.Channel.ID
	messageTS := payload.Container.MessageTS
	slackUser := payload.User.ID
	approved := action.ActionID == hitlApprove

	go func() {
		if err := h.adapter.handleApproval(h.ctx, slackChannel, threadID, messageTS, slackUser, approved); err != nil {
			if !errors.Is(err, context.Canceled) {
				h.adapter.Logger.Error("slack interactions: approval error", "thread", threadID, "error", err)
			}
		}
	}()
}

// handleApproval processes a button click: updates the prompt message to show
// the decision, then resumes (or cancels) the pending A2A task.
func (a *Adapter) handleApproval(ctx context.Context, slackChannel, threadID, messageTS, slackUser string, approved bool) error {
	client := a.apiClient()

	decisionText := "❌ _Denied._"
	resumeText := "denied"
	if approved {
		decisionText = "✅ _Approved._"
		resumeText = "approved"
	}

	// Replace the Block Kit buttons with the decision text.
	if err := client.chatUpdateBlocks(ctx, slackChannel, messageTS, decisionText); err != nil {
		a.Logger.Warn("slack: update approval message failed", "error", err)
	}

	// takePendingTask clears the entry atomically — if the user also typed a
	// reply the first one wins and the other starts a fresh task.
	task := a.takePendingTask(threadID)
	if task == nil {
		return nil
	}

	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: task.ChannelID,
		ThreadID:  threadID,
		Subject:   slackUser,
		AgentRef:  task.AgentRef,
		TaskID:    task.TaskID,
		Text:      resumeText,
	}

	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		return err
	}

	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()
	t := &turn{cancel: turnCancel}
	a.turnsMu.Lock()
	if a.turns == nil {
		a.turns = make(map[string]*turn)
	}
	a.turns[threadID] = t
	a.turnsMu.Unlock()
	defer func() {
		a.turnsMu.Lock()
		if a.turns[threadID] == t {
			delete(a.turns, threadID)
		}
		a.turnsMu.Unlock()
	}()

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		return err
	}

	ts, err := client.postMessage(ctx, slackChannel, "_continuing…_", threadID)
	if err != nil {
		return err
	}

	w := newBatchedWriterWithClient(client, slackChannel, ts)
	if err := w.run(ctx, deltas); err != nil {
		return err
	}

	// Chain: if the resumed task pauses again for approval, register it.
	if w.promptDelta != nil {
		pd := w.promptDelta
		a.storePendingTask(threadID, &pendingTask{
			TaskID:    pd.TaskID,
			AgentRef:  task.AgentRef,
			Channel:   slackChannel,
			ChannelID: task.ChannelID,
		})
		return client.postApprovalPrompt(ctx, slackChannel, threadID, pd.Content, pd.TaskID)
	}
	return nil
}
