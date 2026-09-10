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

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// The native slash command is a second way to open a conversation, next to
// the "@bot /agent <name> <question>" mention. Slack posts the command to
// /channels/slack/commands (or a slash_commands Socket Mode envelope); the
// gateway answers with a modal — an agent picker over the live roster and a
// question box — and, on submit, posts the conversation's root message itself
// and dispatches the question as the thread's first turn. The root is a bot
// message, so the conversation's agent and initiator cannot be re-derived
// from human text after a restart the way a prefixed mention's can; they are
// stamped on the root as Slack message metadata instead (see
// conversationMetadata), which threadAgent and threadInitiator read first.
//
// The gateway never depends on the command's name: Slack routes the payload by
// the URL, so each app manifest may call it what it likes. Slack does not
// offer developer slash commands inside threads or in the agent pane, so the
// command only ever opens a channel conversation.

// slashCommandPayload is the subset of Slack's slash command payload the
// gateway uses. The HTTP form and the Socket Mode JSON payload carry the same
// field names.
type slashCommandPayload struct {
	Command     string `json:"command"`
	Text        string `json:"text"`
	UserID      string `json:"user_id"`
	ChannelID   string `json:"channel_id"`
	TriggerID   string `json:"trigger_id"`
	ResponseURL string `json:"response_url"`
}

func slashCommandFromForm(form url.Values) slashCommandPayload {
	return slashCommandPayload{
		Command:     form.Get("command"),
		Text:        form.Get("text"),
		UserID:      form.Get("user_id"),
		ChannelID:   form.Get("channel_id"),
		TriggerID:   form.Get("trigger_id"),
		ResponseURL: form.Get("response_url"),
	}
}

// commandsHandler serves POST /channels/slack/commands in Events API mode.
// Like the events and interactions handlers it acks immediately — Slack
// requires a 200 within 3 seconds — and does the work in the background; every
// user-visible outcome goes through the payload's response_url.
type commandsHandler struct {
	signingSecret string
	adapter       *Adapter
}

func (h *commandsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := VerifySignature(h.signingSecret, r.Header, body); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	payload := slashCommandFromForm(form)
	if payload.TriggerID == "" || payload.UserID == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	h.adapter.background(func(ctx context.Context) { h.adapter.handleSlashCommand(ctx, payload) })
}

// askAgentPrivateMetadata travels inside the modal (private_metadata, invisible
// to the user) so the submission knows where the command was typed and how to
// answer the user privately. Keys are short: the field is capped at 3000 chars.
type askAgentPrivateMetadata struct {
	Channel     string `json:"c"`
	User        string `json:"u"`
	ResponseURL string `json:"r"`
}

// conversationMetadata is the event_payload of the message metadata stamped on
// a conversation root the gateway posts. It is the durable record of the
// conversation's agent and initiator: a bot-authored root has no /agent prefix
// to re-derive the binding from, and the earliest human in the thread is
// whoever replied first, not who opened it. Slack keeps the metadata with the
// message and returns it from conversations.replies with
// include_all_metadata=true. The event_type must be declared under
// metadata_events in the app manifest or Slack drops it.
type conversationMetadata struct {
	AgentRef   string `json:"agent_ref"`
	Initiator  string `json:"initiator_user_id"`
	EntryPoint string `json:"entry_point,omitempty"`
}

// handleSlashCommand opens the agent picker modal for a slash command, or
// tells the invoking user privately why it cannot. The roster comes from the
// 30s cache (rosterAgentsBestEffort) because the trigger_id expires 3 seconds
// after Slack issued it.
func (a *Adapter) handleSlashCommand(ctx context.Context, p slashCommandPayload) {
	notify := func(text string) {
		if err := a.apiClient().respondToURL(ctx, p.ResponseURL, text); err != nil {
			a.Logger.Warn("slack: slash command notice failed", "user", p.UserID, "error", err)
		}
	}
	if isDMChannelID(p.ChannelID) {
		notify(slashCommandDMNotice)
		return
	}
	if !a.channelServed(p.ChannelID) {
		notify(channelNotServed)
		return
	}
	if a.Roster == nil {
		notify(agentSelectionUnavailable)
		return
	}
	if _, ok := a.AgentCards.(agentCardChecker); !ok {
		notify(agentSelectionUnavailable)
		return
	}
	agents, err := a.rosterAgentsBestEffort(ctx)
	if err != nil {
		a.Logger.Warn("slack: slash command roster unavailable", "error", err)
		notify(agentRosterUnavailable)
		return
	}
	if len(agents) == 0 {
		notify(agentRosterEmpty)
		return
	}
	view, err := a.askAgentModal(agents, p)
	if err != nil {
		a.Logger.Warn("slack: build agent picker modal failed", "error", err)
		notify(slashCommandOpenFailedNotice)
		return
	}
	if err := a.apiClient().viewsOpen(ctx, p.TriggerID, view); err != nil {
		a.Logger.Warn("slack: views.open failed", "user", p.UserID, "channel", p.ChannelID, "error", err)
		notify(slashCommandOpenFailedNotice)
	}
}

// askAgentModal renders the picker: a static_select over the roster (display
// name as label, agent ref as value, the default agent preselected) and a
// multiline question box prefilled with whatever followed the command. A
// static_select holds at most modalMaxAgents options; a larger roster is cut,
// with a warning, rather than refused.
func (a *Adapter) askAgentModal(agents []pkga2a.AgentInfo, p slashCommandPayload) (map[string]any, error) {
	pm, err := json.Marshal(askAgentPrivateMetadata{Channel: p.ChannelID, User: p.UserID, ResponseURL: p.ResponseURL})
	if err != nil {
		return nil, err
	}
	options := make([]any, 0, len(agents))
	var initial map[string]any
	seen := make(map[string]bool, len(agents))
	for _, ag := range agents {
		ref := a.agentInfoRef(ag)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if len(options) == modalMaxAgents {
			a.Logger.Warn("slack: roster exceeds the modal option cap, list cut", "cap", modalMaxAgents, "roster", len(agents))
			break
		}
		label := sanitizeDisplayName(ag.DisplayName)
		if label == "" {
			label = ag.Name
		}
		opt := map[string]any{bkText: plainTextObj(truncateRunes(label, modalOptionLabelMax)), bkValue: ref}
		if ref == a.DefaultAgent {
			initial = opt
		}
		options = append(options, opt)
	}
	agentSelect := map[string]any{
		bkType:        bkStaticSelect,
		bkActionID:    askAgentAgentActionID,
		bkPlaceholder: plainTextObj(askAgentAgentPlaceholder),
		bkOptions:     options,
	}
	if initial != nil {
		agentSelect[bkInitialOption] = initial
	}
	question := map[string]any{
		bkType:        bkPlainTextInput,
		bkActionID:    askAgentQuestionActionID,
		bkMultiline:   true,
		bkMaxLength:   modalQuestionMax,
		bkPlaceholder: plainTextObj(askAgentQuestionPlaceholder),
	}
	if text := truncateRunes(strings.TrimSpace(p.Text), modalQuestionMax); text != "" {
		question[bkInitialValue] = text
	}
	return map[string]any{
		bkType:            bkModal,
		bkCallbackID:      askAgentCallbackID,
		bkPrivateMetadata: string(pm),
		bkTitle:           plainTextObj(askAgentModalTitle),
		bkSubmit:          plainTextObj(askAgentSubmitLabel),
		bkClose:           plainTextObj(askAgentCloseLabel),
		bkBlocks: []any{
			map[string]any{bkType: bkInput, bkBlockID: askAgentAgentBlockID, bkLabel: plainTextObj(askAgentAgentLabel), bkElement: agentSelect},
			map[string]any{bkType: bkInput, bkBlockID: askAgentQuestionBlockID, bkLabel: plainTextObj(askAgentQuestionLabel), bkElement: question},
		},
	}, nil
}

// handleAskAgentSubmission opens the conversation a submitted picker
// describes: validate the agent (loud failure, never a substitute), post the
// root under the agent's identity with the conversation metadata, make the
// submitter the initiator, bind the thread, and dispatch the question as the
// first turn through the same path a mention takes. Slack has already closed
// the modal (the interactions handler acked), so failures reach the user
// through the slash command's response_url.
func (a *Adapter) handleAskAgentSubmission(ctx context.Context, payload interactionPayload) {
	var pm askAgentPrivateMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &pm); err != nil || pm.Channel == "" || pm.User == "" {
		a.Logger.Warn("slack: ask-agent submission without usable private metadata", "user", payload.User.ID)
		return
	}
	user := payload.User.ID
	if user == "" {
		user = pm.User
	}
	notify := func(text string) {
		if err := a.apiClient().respondToURL(ctx, pm.ResponseURL, text); err != nil {
			a.Logger.Warn("slack: ask-agent notice failed", "user", user, "error", err)
		}
	}
	ref := payload.View.State.Values[askAgentAgentBlockID][askAgentAgentActionID].selectedValue()
	question := strings.TrimSpace(payload.View.State.Values[askAgentQuestionBlockID][askAgentQuestionActionID].Value)
	if ref == "" || question == "" {
		notify(askAgentIncompleteNotice)
		return
	}
	if !a.channelServed(pm.Channel) {
		notify(channelNotServed)
		return
	}
	checker, ok := a.AgentCards.(agentCardChecker)
	if !ok {
		notify(agentSelectionUnavailable)
		return
	}
	vctx, cancel := context.WithTimeout(ctx, agentValidateTimeout)
	defer cancel()
	if _, _, err := checker.CardInfo(vctx, ref); err != nil {
		a.Logger.Info("slack: ask-agent selection failed validation", "agent", ref, "user", user, "error", err)
		notify(a.agentUnavailableReply(ctx, ref))
		return
	}

	name := a.agentNameFor(ctx, ref)
	rootText := fmt.Sprintf(askAgentRootText, user, escapeMrkdwn(name), quoteMrkdwn(escapeMrkdwn(question)))
	meta := conversationMetadata{AgentRef: ref, Initiator: user, EntryPoint: entryPointSlashCommand}
	client := a.agentClientNamed(ctx, ref, name)
	rootTS, err := client.postMessageWithMetadata(ctx, pm.Channel, rootText, meta)
	if err != nil && isNotInChannelErr(err) {
		// A public channel the bot was never invited to: join (channels:join)
		// and retry once. A private channel refuses the join, and the user is
		// asked to invite the bot instead.
		if jerr := a.apiClient().conversationsJoin(ctx, pm.Channel); jerr != nil {
			a.Logger.Info("slack: ask-agent join failed", "channel", pm.Channel, "error", jerr)
			notify(askAgentInviteNotice)
			return
		}
		rootTS, err = client.postMessageWithMetadata(ctx, pm.Channel, rootText, meta)
	}
	if err != nil {
		a.Logger.Warn("slack: ask-agent root post failed", "channel", pm.Channel, "error", err)
		notify(askAgentPostFailedNotice)
		return
	}

	// The root is the bot's, so the thread state a mention would derive from
	// the root message is recorded here: the submitter owns the thread, the
	// thread is bound to the chosen agent, and the launch intro is pre-claimed
	// (the root already names the agent).
	a.accessPolicy().SetInitiator(rootTS, user)
	a.bindThreadAgent(rootTS, ref)
	a.claimLaunchAnnounce(rootTS, ref, true)

	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: pm.Channel,
		ThreadID:  rootTS,
		MessageID: rootTS,
		Text:      question,
		Subject:   user,
		AgentRef:  ref,
	}
	if err := a.dispatchFrom(ctx, msg, pm.Channel, agentSourceCommand); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Error("slack: ask-agent dispatch error", "thread", rootTS, "error", err)
	}
}

// selectedValue is the chosen option of a static_select state entry, or "".
func (s blockActionState) selectedValue() string {
	if s.SelectedOption == nil {
		return ""
	}
	return s.SelectedOption.Value
}

func isNotInChannelErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not_in_channel")
}

func plainTextObj(s string) map[string]any {
	return map[string]any{bkType: bkPlainText, bkText: s}
}

// quoteMrkdwn renders s as a Slack block quote, one "> " prefix per line.
func quoteMrkdwn(s string) string {
	return "> " + strings.ReplaceAll(s, "\n", "\n> ")
}
