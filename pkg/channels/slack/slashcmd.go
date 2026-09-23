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
// message with nothing to re-derive from, but nothing needs re-deriving: the
// submission writes the thread's agent and initiator into its thread record in
// the routing store, which is where every later turn reads them.
//
// The gateway never depends on the command's name: Slack routes the payload by
// the URL, so each app manifest may call it what it likes. Slack does not
// offer developer slash commands inside threads or in the agent pane, and
// sends no thread with one, so the command only ever opens a channel
// conversation on a fresh root. The "Ask an agent here" message shortcut
// (inspect.go routes it) opens the same picker where a thread does exist: its
// message_action payload carries the message, so the conversation starts
// inside that message's thread.

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
// requires a 200 within 3 seconds — and does the work in the background.
// Success opens the picker modal with the payload's trigger_id; every failure
// notice goes through its response_url.
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
// to the user) so the submission knows where the picker was opened and how to
// answer the user privately. Keys are short: the field is capped at 3000 chars.
type askAgentPrivateMetadata struct {
	Channel     string `json:"c"`
	User        string `json:"u"`
	ResponseURL string `json:"r"`
	// Thread is the thread the conversation starts in, set by the message
	// shortcut. Empty for the slash command, whose submission posts a new root
	// and opens the thread under it.
	Thread string `json:"t,omitempty"`
}

// askAgentRequest is one request to open the picker, whatever asked for it:
// where the conversation goes, who asked, how to answer them privately, the
// trigger the modal opens on, and — for the message shortcut — the thread it
// starts in. Prefill is the question box's initial value (the text after the
// slash command; the shortcut has none).
type askAgentRequest struct {
	Channel     string
	User        string
	ResponseURL string
	TriggerID   string
	Thread      string
	Prefill     string
}

// handleSlashCommand opens the agent picker modal for a slash command, or
// tells the invoking user privately why it cannot. Slack sends no thread with
// a command, so its conversation always starts on a fresh root.
func (a *Adapter) handleSlashCommand(ctx context.Context, p slashCommandPayload) {
	if !a.started.Load() {
		return
	}
	req := askAgentRequest{Channel: p.ChannelID, User: p.UserID, ResponseURL: p.ResponseURL, TriggerID: p.TriggerID, Prefill: p.Text}
	notify := a.askAgentNotifier(ctx, req, "slash command")
	if isDMChannelID(p.ChannelID) {
		notify(slashCommandDMNotice)
		return
	}
	if !a.channelServed(p.ChannelID) {
		notify(channelNotServed)
		return
	}
	if !a.agentSelectionReady() {
		notify(agentSelectionUnavailable)
		return
	}
	a.openAgentPicker(ctx, req, notify)
}

// handleAskAgentShortcut opens the agent picker for the "Ask an agent here"
// message shortcut. Unlike the slash command it carries a thread — the one the
// invoked message sits in, or the one that message roots — so the conversation
// starts inside an alert's thread or a discussion instead of a new root
// message. A thread that already talks to an agent is refused: the picker
// opens conversations, and the thread's record is what every later turn reads.
func (a *Adapter) handleAskAgentShortcut(ctx context.Context, payload interactionPayload, threadID string) {
	if !a.started.Load() {
		return
	}
	req := askAgentRequest{
		Channel:     payload.Channel.ID,
		User:        payload.User.ID,
		ResponseURL: payload.ResponseURL,
		TriggerID:   payload.TriggerID,
		Thread:      threadID,
	}
	notify := a.askAgentNotifier(ctx, req, "ask-agent shortcut")
	if isDMChannelID(req.Channel) {
		if a.dmMode() != DMModeServe {
			notify(dmRedirect)
			return
		}
	} else if !a.channelServed(req.Channel) {
		notify(channelNotServed)
		return
	}
	if !a.agentSelectionReady() {
		notify(agentSelectionUnavailable)
		return
	}
	// The thread-bound check runs inside openAgentPicker, under the trigger
	// budget and as the caller.
	a.openAgentPicker(ctx, req, notify)
}

// askAgentNotifier answers the picker's invoker privately through the
// interaction's response_url; surface names the entry point in the log.
func (a *Adapter) askAgentNotifier(ctx context.Context, req askAgentRequest, surface string) func(string) {
	return func(text string) {
		if err := a.apiClient().respondToURL(ctx, req.ResponseURL, text); err != nil {
			a.Logger.Warn("slack: "+surface+" notice failed", "user", req.User, "error", err)
		}
	}
}

// agentSelectionReady reports whether this gateway can offer a picker at all:
// a roster to list and a card client to validate the pick against.
func (a *Adapter) agentSelectionReady() bool {
	if a.Roster == nil {
		return false
	}
	_, ok := a.AgentCards.(agentCardChecker)
	return ok
}

// openAgentPicker lists the roster as the caller and opens the picker modal —
// the half both entry points share. The roster comes from the 30s cache
// (rosterAgentsBestEffort) because the trigger_id expires 3 seconds after
// Slack issued it. Every failure is reported through notify.
func (a *Adapter) openAgentPicker(ctx context.Context, req askAgentRequest, notify func(string)) {
	// Everything before views.open shares one budget: Slack invalidates the
	// trigger_id 3 seconds after issuing it, and on a cold roster cache the
	// token mint and the controller list both go to the network. Past the
	// budget the user is told to retry (the next attempt finds the cache warm
	// or the roster read still in flight), not that the picker is broken.
	pctx, cancel := context.WithTimeout(ctx, pickerOpenBudget)
	defer cancel()
	// The roster is listed as the caller: the kagent controller serves
	// AgentTemplates to a human identity, and without one only a warm cache
	// answers. An unlinked caller on a cold cache is told to sign in.
	pctx = a.withCallerToken(pctx, req.User)
	// A shortcut targets an existing thread: one that already talks to an
	// agent is refused, because a second conversation would fork it; one that
	// already belongs to someone else is refused too, because a conversation
	// opened here would run under the owner's delegated identity without the
	// consent the access prompt exists to ask for. The reads spend the same
	// budget as everything else before views.open, and the agent's name
	// resolves as the caller — the way the submit-time re-check names it, so
	// both refusals agree.
	if req.Thread != "" {
		if ref, bound := a.threadAgentBinding(pctx, req.Channel, req.Thread); bound {
			notify(fmt.Sprintf(askAgentThreadBoundNotice, escapeMrkdwn(a.agentNameFor(pctx, ref))))
			return
		}
		if owner := a.accessPolicy().Initiator(pctx, req.Channel, req.Thread); owner != "" && owner != req.User {
			notify(fmt.Sprintf(askAgentThreadOwnedNotice, owner))
			return
		}
	}
	agents, err := a.rosterAgentsBestEffort(pctx)
	if err != nil {
		a.Logger.Warn("slack: agent picker roster unavailable", "user", req.User, "error", err)
		switch {
		case errors.Is(err, pkga2a.ErrNoIdentity):
			notify(slashCommandSignInNotice)
		case pctx.Err() != nil:
			notify(slashCommandSlowNotice)
		default:
			notify(agentRosterUnavailable)
		}
		return
	}
	if len(agents) == 0 {
		notify(agentRosterEmpty)
		return
	}
	view, err := a.askAgentModal(agents, req)
	if err != nil {
		a.Logger.Warn("slack: build agent picker modal failed", "error", err)
		notify(slashCommandOpenFailedNotice)
		return
	}
	if err := a.apiClient().viewsOpen(pctx, req.TriggerID, view); err != nil {
		a.Logger.Warn("slack: views.open failed", "user", req.User, "channel", req.Channel, "error", err)
		if pctx.Err() != nil || strings.Contains(err.Error(), "expired_trigger_id") {
			notify(slashCommandSlowNotice)
		} else {
			notify(slashCommandOpenFailedNotice)
		}
	}
}

// askAgentModal renders the picker: a line saying where the conversation
// lands, a static_select over the roster (display name as label, agent ref as
// value, the default agent preselected and named in a hint) and a multiline
// prompt box prefilled with the request's text, if any. A
// static_select holds at most modalMaxAgents options; a larger roster is cut,
// with a warning, rather than refused.
func (a *Adapter) askAgentModal(agents []pkga2a.AgentInfo, req askAgentRequest) (map[string]any, error) {
	pm, err := json.Marshal(askAgentPrivateMetadata{Channel: req.Channel, User: req.User, ResponseURL: req.ResponseURL, Thread: req.Thread})
	if err != nil {
		return nil, err
	}
	options := make([]any, 0, len(agents))
	var initial map[string]any
	var defaultLabel string
	defaultIdx := -1
	seen := make(map[string]bool, len(agents))
	for _, ag := range agents {
		ref := a.agentInfoRef(ag)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		label := sanitizeDisplayName(ag.DisplayName)
		if label == "" {
			label = ag.Name
		}
		opt := map[string]any{bkText: plainTextObj(truncateRunes(label, modalOptionLabelMax)), bkValue: ref}
		if ref == a.DefaultAgent {
			initial, defaultIdx, defaultLabel = opt, len(options), label
		}
		options = append(options, opt)
	}
	if len(options) > modalMaxAgents {
		// The cut must never drop the preselected default: move it to the
		// front so it stays on the list whatever the roster order.
		if defaultIdx >= modalMaxAgents {
			def := options[defaultIdx]
			options = append([]any{def}, append(options[:defaultIdx:defaultIdx], options[defaultIdx+1:]...)...)
		}
		a.Logger.Warn("slack: roster exceeds the modal option cap, list cut", "cap", modalMaxAgents, "roster", len(options))
		options = options[:modalMaxAgents]
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
	if text := truncateRunes(strings.TrimSpace(req.Prefill), modalQuestionMax); text != "" {
		question[bkInitialValue] = text
	}
	agentInput := map[string]any{bkType: bkInput, bkBlockID: askAgentAgentBlockID, bkLabel: plainTextObj(askAgentAgentLabel), bkElement: agentSelect}
	if initial != nil {
		agentInput[bkHint] = plainTextObj(truncateRunes(fmt.Sprintf(askAgentDefaultHint, defaultLabel), modalHintMax))
	}
	blocks := []any{
		contextBlock(askAgentLead(req)),
		agentInput,
		map[string]any{bkType: bkInput, bkBlockID: askAgentQuestionBlockID, bkLabel: plainTextObj(askAgentQuestionLabel), bkElement: question},
	}
	if block, ok := threadContextBlock(req); ok {
		blocks = append(blocks, block)
	}
	submit := askAgentSubmitLabel
	if req.Thread != "" {
		submit = askAgentShortcutSubmitLabel
	}
	return map[string]any{
		bkType:            bkModal,
		bkCallbackID:      askAgentCallbackID,
		bkPrivateMetadata: string(pm),
		bkTitle:           plainTextObj(askAgentModalTitle),
		bkSubmit:          plainTextObj(submit),
		bkClose:           plainTextObj(askAgentCloseLabel),
		bkBlocks:          blocks,
	}, nil
}

// askAgentLead says where the picker's conversation lands: a new thread in the
// channel for the slash command, the invoked thread for the shortcut.
func askAgentLead(req askAgentRequest) string {
	switch {
	case isDMChannelID(req.Channel):
		return askAgentLeadDM
	case req.Thread != "":
		return fmt.Sprintf(askAgentLeadThread, req.Channel)
	default:
		return fmt.Sprintf(askAgentLeadNewThread, req.Channel)
	}
}

// threadContextBlock is the checkbox that decides whether the agent is given
// the messages the target thread already holds. It is offered whenever the
// picker was opened on a message in a channel — there is then always at least
// that thread's root to include — and is checked by default: the person
// starting the session is a member of that thread and does it in the open, but
// a thread whose earlier part is noise or private banter is theirs to leave
// out with one click. A DM is not offered it: the shortcut works there when
// DMs are served, and the only messages before the opener are the person's own
// and the bot's, which the agent either wrote or is about to. It names no
// count: counting would mean reading the thread before views.open, and Slack
// invalidates the trigger after three seconds. The input is optional, or Slack
// would refuse a submission with the box cleared.
func threadContextBlock(req askAgentRequest) (map[string]any, bool) {
	if req.Thread == "" || isDMChannelID(req.Channel) {
		return nil, false
	}
	option := map[string]any{bkText: plainTextObj(askAgentContextOption), bkValue: askAgentContextValue}
	return map[string]any{
		bkType:     bkInput,
		bkBlockID:  askAgentContextBlockID,
		bkOptional: true,
		bkLabel:    plainTextObj(askAgentContextLabel),
		bkElement: map[string]any{
			bkType:           bkCheckboxes,
			bkActionID:       askAgentContextActionID,
			bkOptions:        []any{option},
			bkInitialOptions: []any{option},
		},
	}, true
}

// handleAskAgentSubmission opens the conversation a submitted picker
// describes: validate the agent (loud failure, never a substitute), post the
// echo under the agent's identity with the conversation metadata, make the
// submitter the initiator, bind the thread, and dispatch the question as the
// first turn through the same path a mention takes. The slash command's echo
// is a new root and its ts is the thread; the shortcut's is a reply in the
// thread the picker was opened on. Slack has already closed the modal (the
// interactions handler acked), so failures reach the user through the
// response_url the picker travelled with.
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
	// The context checkbox: absent from the view when there was no thread to
	// read, and cleared by a person who wants the agent to see their question
	// alone.
	includeContext := len(payload.View.State.Values[askAgentContextBlockID][askAgentContextActionID].SelectedOptions) > 0
	// Both inputs are required in the modal, so Slack refuses an empty
	// submission itself; this only guards a malformed payload.
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
	// Validation and branding read the controller as the submitter, like the
	// roster did when the picker opened.
	ctx = a.withCallerToken(ctx, user)
	// The shortcut's thread was free when the picker opened; someone may have
	// started a conversation in it since. The record decides, as everywhere.
	if pm.Thread != "" {
		if bound, ok := a.threadAgentBinding(ctx, pm.Channel, pm.Thread); ok {
			notify(fmt.Sprintf(askAgentThreadBoundNotice, escapeMrkdwn(a.agentNameFor(ctx, bound))))
			return
		}
	}
	vctx, cancel := context.WithTimeout(ctx, agentValidateTimeout)
	defer cancel()
	if _, _, err := checker.CardInfo(vctx, ref); err != nil {
		a.Logger.Info("slack: ask-agent selection failed validation", "agent", ref, "user", user, "error", err)
		if errors.Is(err, pkga2a.ErrNoIdentity) {
			// The picker opened on a cached roster, but this replica — another
			// one, or this one after a restart — holds a cold cache, so the card
			// read needs the caller's identity. Not an unknown agent: a sign-in.
			notify(slashCommandSignInNotice)
			return
		}
		// The picker never offers an unavailable template, so this only fires
		// when the agent became unavailable between the listing and the submit.
		if text, ok := a.agentNotRunnableReply(ctx, ref, err); ok {
			notify(text)
			return
		}
		notify(a.agentUnavailableReply(ctx, ref))
		return
	}

	// The shortcut's thread exists already, so it is claimed before anything
	// is posted: SetInitiator makes the submitter its owner, or returns the
	// owner it already has — a /usage or /stop typed there wrote one, without
	// an agent. Another person's thread is refused here, with nothing echoed
	// and nothing bound; their reply in the thread takes the normal path, where
	// the owner is asked to allow them. Granting them instead would let the
	// owner's delegated identity act on their word without that consent.
	if pm.Thread != "" {
		if owner := a.accessPolicy().SetInitiator(ctx, pm.Channel, pm.Thread, user); owner != user {
			notify(fmt.Sprintf(askAgentThreadOwnedNotice, owner))
			return
		}
	}

	name := a.agentNameFor(ctx, ref)
	client := a.agentClientNamed(ctx, ref, name)
	echoTS, err := client.postQuestion(ctx, pm.Channel, question, user, pm.Thread)
	if err != nil && isNotInChannelErr(err) {
		// A public channel the bot was never invited to: join (channels:join)
		// and retry once. A private channel refuses the join, and the user is
		// asked to invite the bot instead.
		if jerr := a.apiClient().conversationsJoin(ctx, pm.Channel); jerr != nil {
			a.Logger.Info("slack: ask-agent join failed", "channel", pm.Channel, "error", jerr)
			notify(askAgentInviteNotice)
			return
		}
		echoTS, err = client.postQuestion(ctx, pm.Channel, question, user, pm.Thread)
	}
	if err != nil {
		a.Logger.Warn("slack: ask-agent echo post failed", "channel", pm.Channel, "error", err)
		notify(askAgentPostFailedNotice)
		return
	}

	// The shortcut starts the conversation in the thread it was invoked on;
	// the slash command's own echo is the root, so it is the thread.
	threadTS, source := pm.Thread, agentSourceShortcut
	if threadTS == "" {
		threadTS, source = echoTS, agentSourceCommand
	}

	// The opening message is the bot's, so the thread state a mention would
	// carry on its own is written to the thread record here: the submitter
	// owns the thread (the slash command's root is brand new; the shortcut's
	// thread was claimed above), and the thread is bound to the chosen agent.
	a.accessPolicy().SetInitiator(ctx, pm.Channel, threadTS, user)
	a.bindThreadAgent(ctx, pm.Channel, threadTS, ref)

	// The thread the picker was opened on is read now, with the echo as the
	// opener: it was just posted, so only the messages that were already there
	// — everyone else's — land in the transcript. A thread the submission
	// rooted itself (the slash command) has nothing earlier to read, and a DM
	// is never read at all — the same rule the typed entry points follow
	// (attachThreadContext), and the reason no checkbox was offered there.
	var threadContext string
	if pm.Thread != "" && includeContext && !isDMChannelID(pm.Channel) {
		threadContext = a.threadContext(ctx, pm.Channel, threadTS, echoTS, user)
	}

	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: pm.Channel,
		ThreadID:  threadTS,
		MessageID: echoTS,
		Text:      question,
		Context:   threadContext,
		Subject:   user,
		AgentRef:  ref,
		// The question opens the conversation: it names the agent session and
		// the session title keys on it.
		Opener: true,
	}
	if err := a.dispatchFrom(ctx, msg, pm.Channel, source); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Error("slack: ask-agent dispatch error", "thread", threadTS, "error", err)
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
