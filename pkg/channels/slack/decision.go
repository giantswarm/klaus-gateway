package slack

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A decision is the question a service puts to a person or a team through
// POST /decisions (pkg/reviews): the question as the header, the status quo,
// one section per option with a Choose button (the recommended one marked and
// primary), an "Answer in my own words" button that opens a modal, and the due
// time, the default and who asks under them. A person's decision is a direct
// message from the app, a team's a message in the team's channel.
//
// It is answered by a Choose click, the modal, or a plain reply in the
// message's thread. Each calls the decision's answer tool through muster as
// the person who answered, with "choice" and "text" added; the tool decides
// whether that person may answer (the addressee, a member of the team). The
// rest is the team review's machinery: the record lives in the review store
// (store.Review with Decision set) for the due time plus seven days, a click
// claims it by compare-and-set so two answers cannot both go through, a
// refusal or a failure is a status line under the buttons, a sign-in
// challenge a Connect prompt whose landing submits the answer again, and the
// answer rewrites the message in place. The asker closes it through
// POST /decisions/{id}/close on every path — answered here or elsewhere,
// defaulted at its due time, withdrawn — which rewrites the message to its
// outcome and refuses later clicks.
//
// The decision's id is its message: "<channel>-<ts>". A click, a modal
// submission and a thread reply all carry the message, so none of them needs
// an index.

// Decision action IDs and the modal's callback, blocks and actions.
const (
	decisionChoose   = "decision_choose"    // an option's Choose button; the value is the option, 1-based
	decisionOwnWords = "decision_own_words" // opens the answer modal

	decisionAnswerCallbackID     = "decision_answer"
	decisionAnswerTextBlockID    = "decision_answer_text"
	decisionAnswerTextActionID   = "text"
	decisionAnswerChoiceBlockID  = "decision_answer_choice"
	decisionAnswerChoiceActionID = "choice"
)

// decisionKeep is how long a decision's record outlives its due time: the
// asker closes it at the due time, and a late click is still told how it
// closed.
const decisionKeep = 7 * 24 * time.Hour

// What a decision's message and modal say.
const (
	decisionChooseLabel       = "Choose"
	decisionOwnWordsLabel     = "Answer in my own words"
	decisionRecommended       = "_Recommended_"
	decisionModalTitle        = "Answer"
	decisionModalSubmit       = "Answer"
	decisionModalClose        = "Cancel"
	decisionModalTextLabel    = "Your answer"
	decisionModalChoiceLabel  = "With an option"
	decisionModalChoiceHint   = "Pick one if your answer builds on it"
	decisionExpiredNotice     = "_This decision has expired._"
	decisionUnrecordedNotice  = "_This decision could not be recorded and cannot be answered here. Ask for it to be sent again._"
	decisionUnavailableNotice = "_The decision could not be looked up right now. Try again in a moment._"

	decisionAnsweredByNotice = "This decision was already answered by <@%s>."
	decisionAnsweredNotice   = "This decision was already answered."
	decisionDefaultedNotice  = "This decision was not answered in time; the default was applied."
	decisionWithdrawnNotice  = "This decision was withdrawn."
	decisionPendingNotice    = "<@%s>'s answer is being submitted right now."

	decisionStatusConnecting      = "<@%s> is connecting *%s* to answer as themselves."
	decisionStatusRefused         = "<@%s>'s answer was not accepted: %s"
	decisionStatusFailed          = "<@%s>'s answer could not be submitted; try again in a moment."
	decisionStatusStillChallenged = "<@%s> connected *%s*, but the answer still asks them to sign in; try again in a moment."

	decisionConnectNotice       = "To answer as yourself, connect *%s* once. Your answer is submitted as soon as you're back."
	decisionConnectManualNotice = "To answer as yourself, connect *%s* once, then answer again."
)

// decisionAnswer is what a person answered: an option (1-based, 0 for none)
// and their own words ("" for none).
type decisionAnswer struct {
	choice int
	text   string
}

// arguments are the answer tool's arguments: the decision's own, plus
// "choice" and "text" when the answer has them.
func (ans decisionAnswer) arguments(rv store.Review) map[string]any {
	args := maps.Clone(rv.Arguments)
	if args == nil {
		args = map[string]any{}
	}
	if ans.choice > 0 {
		args["choice"] = ans.choice
	}
	if ans.text != "" {
		args["text"] = ans.text
	}
	return args
}

// decisionID names the decision whose message is ts in channel.
func decisionID(channel, ts string) string {
	return channel + "-" + ts
}

// decisionChannel is the channel a decision id names.
func decisionChannel(id string) string {
	channel, _, _ := strings.Cut(id, "-")
	return channel
}

// PostDecision sends the decision to its addressee — a person by direct
// message, found by their email, or a team in its channel — and records it so
// its answers can be resolved. It implements channels.DecisionPoster.
func (a *Adapter) PostDecision(ctx context.Context, decision channels.Decision) (channels.PostReceipt, error) {
	if a.Tools == nil || a.OBO == nil {
		return channels.PostReceipt{}, errors.New("slack: decisions need a tool caller and account linking")
	}
	target := decision.Channel
	if decision.Person != "" {
		user, err := a.apiClient().lookupUserByEmail(ctx, decision.Person)
		if err != nil {
			return channels.PostReceipt{}, fmt.Errorf("slack: find %s: %w", decision.Person, err)
		}
		target = user
	}
	now := time.Now()
	rv := store.Review{
		Team: decision.Team,
		Text: decision.Question,
		Tool: decision.Answer.Tool, Arguments: decision.Answer.Arguments,
		Decision: &store.Decision{
			Note:      decision.Note,
			StatusQuo: decision.StatusQuo,
			Options:   decision.Options,
			Recommend: decision.Recommend,
			Due:       decision.Due,
			Default:   decision.Default,
			AskedBy:   decision.AskedBy,
		},
		PostedAt: now,
		TTL:      decision.Due.Sub(now) + decisionKeep,
	}
	resp, err := a.apiClient().postJSONResponse(ctx, methodChatPostMessage, map[string]any{
		paramChannel: target,
		paramText:    decisionFallback(rv),
		paramBlocks:  decisionBlocks(rv),
	})
	if err != nil {
		return channels.PostReceipt{}, err
	}
	// A direct message is addressed by user ID; its edits need the D…
	// conversation Slack put it in.
	rv.Channel, rv.TS = resp.Channel, resp.Ts
	if rv.Channel == "" {
		rv.Channel = target
	}
	rv.ID = decisionID(rv.Channel, rv.TS)
	if err := a.reviews().PutReview(ctx, rv); err != nil {
		a.Logger.Error("slack: decision could not be recorded", "decision", rv.ID, "error", err)
		if uerr := a.apiClient().chatUpdateBlocks(ctx, rv.Channel, rv.TS, decisionUnrecordedNotice); uerr != nil {
			a.Logger.Warn("slack: decision unrecorded rewrite failed", "decision", rv.ID, "error", uerr)
		}
		return channels.PostReceipt{}, fmt.Errorf("slack: record decision: %w", err)
	}
	return channels.PostReceipt{ID: rv.ID, Channel: rv.Channel, TS: rv.TS}, nil
}

// CloseDecision rewrites the decision's message to its outcome and refuses
// every later answer. A decision answered here keeps the answer and who gave
// it; a close repeated is a rewrite to the same outcome. It implements
// channels.DecisionPoster; a decision the gateway holds no record of is
// channels.ErrDecisionNotFound.
func (a *Adapter) CloseDecision(ctx context.Context, id string, closing channels.DecisionClose) (channels.PostReceipt, error) {
	now := time.Now()
	var closed store.Review
	isDecision := false
	found, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		isDecision = r.Decision != nil
		if !isDecision {
			return false
		}
		d := *r.Decision
		if d.Outcome != closing.Outcome {
			d.Outcome, d.ClosedAt = closing.Outcome, now
		}
		d.CloseText = closing.Text
		r.Decision, r.Done, r.Status = &d, true, ""
		closed = *r
		return true
	})
	switch {
	case err != nil:
		return channels.PostReceipt{}, fmt.Errorf("slack: close decision: %w", err)
	case !found || !isDecision:
		return channels.PostReceipt{}, channels.ErrDecisionNotFound
	}
	if err := a.apiClient().chatUpdate(ctx, closed.Channel, closed.TS, decisionFallback(closed), decisionClosedBlocks(closed)); err != nil {
		return channels.PostReceipt{}, err
	}
	return channels.PostReceipt{ID: id, Channel: closed.Channel, TS: closed.TS}, nil
}

// decisionFallback is the notification text of a decision's message.
func decisionFallback(rv store.Review) string {
	return truncateRunes(rv.Text, slackSectionTextMax)
}

// decisionBlocks renders an open decision: the question, the status quo, an
// option per section with its Choose button, the own-words button, the due
// time with the default and who asks, and the latest status line.
func decisionBlocks(rv store.Review) []any {
	d := rv.Decision
	blocks := decisionHead(rv)
	for i, o := range d.Options {
		n := i + 1
		text := "*" + escapeMrkdwn(o.Label) + "*"
		if o.Consequence != "" {
			text += "\n" + o.Consequence
		}
		button := map[string]any{
			bkType:     bkButton,
			bkText:     plainTextObj(decisionChooseLabel),
			bkActionID: decisionChoose,
			bkValue:    strconv.Itoa(n),
		}
		if n == d.Recommend {
			text = decisionRecommended + "\n" + text
			button[bkStyle] = bkPrimary
		}
		blocks = append(blocks, map[string]any{
			bkType:      bkSection,
			bkText:      map[string]any{bkType: bkMrkdwn, bkText: truncateRunes(text, slackSectionTextMax)},
			bkAccessory: button,
		})
	}
	blocks = append(blocks,
		map[string]any{bkType: bkActions, bkElements: []any{map[string]any{
			bkType:     bkButton,
			bkText:     plainTextObj(decisionOwnWordsLabel),
			bkActionID: decisionOwnWords,
		}}},
		contextBlock(truncateRunes(fmt.Sprintf("Due %s · if unanswered: %s", slackDate(d.Due), escapeMrkdwn(d.Default)), slackSectionTextMax)),
		contextBlock(decisionAskedLine(rv)),
	)
	if rv.Status != "" {
		blocks = append(blocks, contextBlock(rv.Status))
	}
	return blocks
}

// decisionClosedBlocks renders a closed decision: the question and the status
// quo stay, the options and buttons go, one line says how it closed.
func decisionClosedBlocks(rv store.Review) []any {
	return append(decisionHead(rv), contextBlock(decisionOutcomeLine(rv)), contextBlock(decisionAskedLine(rv)))
}

func decisionHead(rv store.Review) []any {
	return []any{
		map[string]any{bkType: bkHeader, bkText: plainTextObj(truncateRunes(rv.Text, channels.DecisionQuestionMax))},
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: truncateRunes(rv.Decision.StatusQuo, slackSectionTextMax)}},
	}
}

// decisionAskedLine names who asks, for which team, and the note.
func decisionAskedLine(rv store.Review) string {
	d := rv.Decision
	line := "Asked by " + escapeMrkdwn(d.AskedBy)
	if rv.Team != "" {
		line = "For " + escapeMrkdwn(rv.Team) + " · " + line
	}
	if d.Note != "" {
		line += " · note #" + escapeMrkdwn(d.Note)
	}
	return truncateRunes(line, slackSectionTextMax)
}

// decisionOutcomeLine says how the decision closed: who answered what and
// when, that its default was applied, or that it was withdrawn.
func decisionOutcomeLine(rv store.Review) string {
	d := rv.Decision
	var line string
	switch d.Outcome {
	case store.DecisionDefaulted:
		line = fmt.Sprintf("Not answered by %s; the default was applied: %s", slackDate(d.Due), escapeMrkdwn(d.Default))
	case store.DecisionWithdrawn:
		line = "Withdrawn · " + slackDate(d.ClosedAt)
		if d.CloseText != "" {
			line += ": " + escapeMrkdwn(d.CloseText)
		}
	default:
		line = "Answered · " + slackDate(d.ClosedAt)
		if rv.DecidedBy != "" {
			line = fmt.Sprintf("Answered by <@%s> · %s", rv.DecidedBy, slackDate(d.ClosedAt))
		}
		if said := decisionAnswerText(d); said != "" {
			line += ": " + said
		}
	}
	return truncateRunes(line, slackSectionTextMax)
}

// decisionAnswerText is the answer as shown: the option's label and the
// person's words, or the text the close carried when the answer was given
// elsewhere.
func decisionAnswerText(d *store.Decision) string {
	var parts []string
	if d.Choice > 0 && d.Choice <= len(d.Options) {
		parts = append(parts, "*"+escapeMrkdwn(d.Options[d.Choice-1].Label)+"*")
	}
	if d.Answer != "" {
		parts = append(parts, escapeMrkdwn(d.Answer))
	}
	if len(parts) == 0 && d.CloseText != "" {
		parts = append(parts, escapeMrkdwn(d.CloseText))
	}
	return strings.Join(parts, " — ")
}

// slackDate renders t in each reader's own time zone, with a UTC fallback
// for clients that cannot.
func slackDate(t time.Time) string {
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format("2006-01-02 15:04 UTC"))
}

// lookupDecision resolves a click on the decision message ts in channel. A
// decision gone rewrites the dead buttons; a store that does not answer tells
// the clicker to try again and leaves the message alone.
func (a *Adapter) lookupDecision(ctx context.Context, channel, ts, clicker string) (store.Review, bool) {
	id := decisionID(channel, ts)
	rv, found, err := a.reviews().GetReview(ctx, id)
	if err != nil {
		a.Logger.Warn("slack: decision lookup failed", "record", "decision_store_failed", "decision", id, "slack_user", clicker, "error", err)
		a.tellClickerIn(ctx, channel, id, clicker, decisionUnavailableNotice)
		return store.Review{}, false
	}
	if !found || rv.Decision == nil {
		if err := a.apiClient().chatUpdateBlocks(ctx, channel, ts, decisionExpiredNotice); err != nil {
			a.Logger.Warn("slack: decision expired rewrite failed", "decision", id, "error", err)
		}
		return store.Review{}, false
	}
	if a.Tools == nil {
		a.Logger.Error("slack: decision click without a tool caller", "decision", id)
		return store.Review{}, false
	}
	return rv, true
}

// handleDecisionChoose answers the decision with the option clicked.
func (a *Adapter) handleDecisionChoose(ctx context.Context, channel, ts, clicker, value string) {
	rv, ok := a.lookupDecision(ctx, channel, ts, clicker)
	if !ok {
		return
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > len(rv.Decision.Options) {
		return
	}
	a.answerDecision(ctx, rv, clicker, decisionAnswer{choice: n}, signInForClick, false)
}

// handleDecisionOwnWordsClick opens the answer modal. The clicker must be
// linked and the decision open; the claim is taken on submission, so a modal
// left open holds nothing.
func (a *Adapter) handleDecisionOwnWordsClick(ctx context.Context, channel, ts, clicker, triggerID string) {
	rv, ok := a.lookupDecision(ctx, channel, ts, clicker)
	if !ok || a.tellIfDecided(ctx, rv, clicker) {
		return
	}
	_, ok, signIn := a.humanToken(ctx, rv.Channel, "", clicker)
	if signIn {
		a.postSignIn(ctx, rv.Channel, "", clicker, false, signInForClick)
	}
	if !ok {
		return
	}
	if err := a.apiClient().viewsOpen(ctx, triggerID, decisionAnswerView(rv)); err != nil {
		a.Logger.Warn("slack: decision answer modal failed", "decision", rv.ID, "user", clicker, "error", err)
		a.tellClicker(ctx, rv, clicker, decisionUnavailableNotice)
	}
}

// decisionAnswerView is the answer modal: the question, a required box for
// the answer and, when the decision has options, an optional select of them.
func decisionAnswerView(rv store.Review) map[string]any {
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: "*" + escapeMrkdwn(rv.Text) + "*"}},
		map[string]any{
			bkType:    bkInput,
			bkBlockID: decisionAnswerTextBlockID,
			bkLabel:   plainTextObj(decisionModalTextLabel),
			bkElement: map[string]any{
				bkType:      bkPlainTextInput,
				bkActionID:  decisionAnswerTextActionID,
				bkMultiline: true,
				bkMaxLength: channels.DecisionAnswerMax,
			},
		},
	}
	if options := rv.Decision.Options; len(options) > 0 {
		items := make([]any, 0, len(options))
		for i, o := range options {
			items = append(items, map[string]any{bkText: plainTextObj(o.Label), bkValue: strconv.Itoa(i + 1)})
		}
		blocks = append(blocks, map[string]any{
			bkType:     bkInput,
			bkBlockID:  decisionAnswerChoiceBlockID,
			bkOptional: true,
			bkLabel:    plainTextObj(decisionModalChoiceLabel),
			bkElement: map[string]any{
				bkType:        bkStaticSelect,
				bkActionID:    decisionAnswerChoiceActionID,
				bkPlaceholder: plainTextObj(decisionModalChoiceHint),
				bkOptions:     items,
			},
		})
	}
	return map[string]any{
		bkType:            bkModal,
		bkCallbackID:      decisionAnswerCallbackID,
		bkPrivateMetadata: rv.ID,
		bkTitle:           plainTextObj(decisionModalTitle),
		bkSubmit:          plainTextObj(decisionModalSubmit),
		bkClose:           plainTextObj(decisionModalClose),
		bkBlocks:          blocks,
	}
}

// handleDecisionAnswerSubmission answers the decision with what the person
// wrote into the modal, and the option they picked with it.
func (a *Adapter) handleDecisionAnswerSubmission(ctx context.Context, payload interactionPayload) {
	id := payload.View.PrivateMetadata
	values := payload.View.State.Values
	text := strings.TrimSpace(values[decisionAnswerTextBlockID][decisionAnswerTextActionID].Value)
	if id == "" || text == "" {
		// Slack requires the box before it submits; a submission without it
		// did not come from the modal.
		return
	}
	ans := decisionAnswer{text: truncateRunes(text, channels.DecisionAnswerMax)}
	if opt := values[decisionAnswerChoiceBlockID][decisionAnswerChoiceActionID].SelectedOption; opt != nil {
		ans.choice, _ = strconv.Atoi(opt.Value)
	}
	clicker := payload.User.ID
	rv, ok := a.decisionFor(ctx, id, clicker)
	if !ok {
		return
	}
	if ans.choice < 0 || ans.choice > len(rv.Decision.Options) {
		ans.choice = 0
	}
	a.answerDecision(ctx, rv, clicker, ans, signInForClick, false)
}

// answerDecisionReply takes a reply in a decision's thread as the replier's
// answer in their own words, relays a reply in a conversation's thread to its
// agent, and reports whether it did either. Only a reply under a
// message of the bot's own is looked up, so every other message costs no
// store read here; the app_mention twin of a reply that mentions the bot is
// consumed with it.
func (a *Adapter) answerDecisionReply(ctx context.Context, inner slackInnerEvent) bool {
	if (inner.Type != evtMessage && inner.Type != evtAppMention) || inner.ThreadTS == "" || inner.ThreadTS == inner.TS ||
		inner.BotID != "" || inner.User == "" || !routableSubtype(inner.SubType) ||
		inner.ParentUserID == "" || inner.ParentUserID != a.botID(ctx) {
		return false
	}
	id := decisionID(inner.Channel, inner.ThreadTS)
	rv, found, err := a.reviews().GetReview(ctx, id)
	if err != nil {
		a.Logger.Warn("slack: decision lookup failed", "record", "decision_store_failed", "decision", id, "slack_user", inner.User, "error", err)
		return false
	}
	if !found || (rv.Decision == nil && rv.Conversation == nil) {
		return false
	}
	if a.seenMessage(inner.Channel, inner.TS) {
		return true
	}
	if rv.Conversation != nil {
		a.relayConversationReply(ctx, rv, inner)
		return true
	}
	text := strings.TrimSpace(slackTextUnescaper.Replace(inner.Text))
	if text == "" || a.Tools == nil {
		return true
	}
	a.answerDecision(ctx, rv, inner.User, decisionAnswer{text: truncateRunes(text, channels.DecisionAnswerMax)}, signInForReply, false)
	return true
}

// slackTextUnescaper turns a message's text as Slack sends it back into what
// the person typed: Slack escapes these three.
var slackTextUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

// decisionFor reads decision id for an answer that did not come from its
// message; what the person is told goes to the decision's channel.
func (a *Adapter) decisionFor(ctx context.Context, id, user string) (store.Review, bool) {
	rv, found, err := a.reviews().GetReview(ctx, id)
	switch {
	case err != nil:
		a.Logger.Warn("slack: decision lookup failed", "record", "decision_store_failed", "decision", id, "slack_user", user, "error", err)
		a.tellClickerIn(ctx, decisionChannel(id), id, user, decisionUnavailableNotice)
		return store.Review{}, false
	case !found || rv.Decision == nil:
		a.tellClickerIn(ctx, decisionChannel(id), id, user, decisionExpiredNotice)
		return store.Review{}, false
	case a.Tools == nil:
		a.Logger.Error("slack: decision answer without a tool caller", "decision", id)
		return store.Review{}, false
	}
	return rv, true
}

// answerDecision submits the answer as the person who gave it: they must be
// linked (an unlinked one is asked to sign in, the decision stays open), the
// decision must be open (a later answer is told how it closed), and the
// answer tool is called as them. A sign-in challenge is a Connect prompt
// whose landing submits the answer again (resumed=true); a refusal or a
// failure is a status line under the buttons, the decision open; a success
// rewrites the message to the answer.
func (a *Adapter) answerDecision(ctx context.Context, rv store.Review, user string, ans decisionAnswer, trigger signInTrigger, resumed bool) {
	token, ok, signIn := a.humanToken(ctx, rv.Channel, "", user)
	if signIn {
		a.postSignIn(ctx, rv.Channel, "", user, false, trigger)
	}
	if !ok || a.tellIfDecided(ctx, rv, user) {
		return
	}
	current, found, claimed, err := a.claimTeamReview(ctx, rv.ID, user, false)
	switch {
	case err != nil:
		a.Logger.Warn("slack: decision claim failed", "record", "decision_store_failed", "decision", rv.ID, "slack_user", user, "error", err)
		a.tellClicker(ctx, rv, user, decisionUnavailableNotice)
		return
	case !found:
		a.tellClicker(ctx, rv, user, decisionExpiredNotice)
		return
	case !claimed:
		a.tellClicker(ctx, rv, user, decidedNotice(current))
		return
	}

	res, err := a.Tools.CallTool(ctx, token, rv.Tool, ans.arguments(rv))
	if err != nil {
		a.releaseTeamReview(ctx, rv.ID)
		a.Logger.Warn("slack: decision answer tool call failed", "decision", rv.ID, "tool", rv.Tool, "user", user, "error", err)
		a.showDecisionStatus(ctx, rv, fmt.Sprintf(decisionStatusFailed, user))
		return
	}
	if server, loginURL, challenged := authChallengeOf(res); challenged {
		a.releaseTeamReview(ctx, rv.ID)
		if resumed {
			a.Logger.Warn("slack: decision answer still challenged after the connector sign-in", "decision", rv.ID, "tool", rv.Tool, "user", user, "server", server)
			a.showDecisionStatus(ctx, rv, fmt.Sprintf(decisionStatusStillChallenged, user, escapeMrkdwn(server)))
			return
		}
		a.promptDecisionConnect(ctx, rv, user, ans, server, loginURL)
		return
	}
	if res.IsError {
		a.releaseTeamReview(ctx, rv.ID)
		a.Logger.Info("slack: decision answer refused by the tool", "record", "decision_refused",
			"decision", rv.ID, "tool", rv.Tool, "slack_user", user, "reason", res.Text)
		reason := "the asker refused it"
		if res.Text != "" {
			reason = truncateRunes(escapeMrkdwn(res.Text), teamReviewReasonMax)
		}
		a.showDecisionStatus(ctx, rv, fmt.Sprintf(decisionStatusRefused, user, reason))
		return
	}

	answered, ok := a.finishDecision(ctx, rv.ID, user, ans)
	if !ok {
		return
	}
	a.Logger.Info("slack: decision answered", "record", "decision_answered",
		"decision", rv.ID, "tool", rv.Tool, "slack_user", user, "subject", a.linkedSubject(user), "choice", ans.choice, "resumed", resumed)
	if err := a.apiClient().chatUpdate(ctx, answered.Channel, answered.TS, decisionFallback(answered), decisionClosedBlocks(answered)); err != nil {
		a.Logger.Warn("slack: decision answer rewrite failed", "decision", rv.ID, "error", err)
	}
}

// promptDecisionConnect answers a sign-in challenge on an answer: the
// decision stays open, its status line says who is connecting, and the
// person gets a Connect button whose landing submits the answer again.
func (a *Adapter) promptDecisionConnect(ctx context.Context, rv store.Review, user string, ans decisionAnswer, server, loginURL string) {
	a.Logger.Info("slack: decision answer needs the person to connect the backend", "record", "decision_connect",
		"decision", rv.ID, "tool", rv.Tool, "slack_user", user, "server", server)
	a.showDecisionStatus(ctx, rv, fmt.Sprintf(decisionStatusConnecting, user, escapeMrkdwn(server)))

	promptURL, connectValue := loginURL, server
	text := fmt.Sprintf(decisionConnectManualNotice, escapeMrkdwn(server))
	if base := a.PublicBaseURL; base != "" {
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: user, server: server, channel: rv.Channel, review: rv.ID, answer: &ans})
		if decorated, err := decorateConnectorLoginURL(loginURL, base, stateID); err != nil {
			a.Logger.Warn("slack: decision login URL decoration failed, posting plain link", "decision", rv.ID, "server", server, "error", err)
		} else {
			promptURL, connectValue = decorated, stateID
			text = fmt.Sprintf(decisionConnectNotice, escapeMrkdwn(server))
		}
	}
	if err := a.apiClient().postConnectPrompt(ctx, rv.Channel, "", user, text, server, promptURL, connectValue, false); err != nil {
		a.Logger.Warn("slack: decision connect prompt failed", "decision", rv.ID, "user", user, "error", err)
	}
}

// resumeDecisionAnswer is the landing's continuation of a Connect prompt
// posted for an answer: the person signed in to the backend, so their answer
// is submitted again.
func (a *Adapter) resumeDecisionAnswer(ctx context.Context, entry connectorCompletion) {
	rv, ok := a.decisionFor(ctx, entry.review, entry.slackUser)
	if !ok {
		return
	}
	a.answerDecision(ctx, rv, entry.slackUser, *entry.answer, signInForClick, true)
}

// showDecisionStatus records status as the decision's status line and
// rewrites the message with it, buttons kept. A decision closed meanwhile,
// or a store that cannot say, leaves the message alone.
func (a *Adapter) showDecisionStatus(ctx context.Context, rv store.Review, status string) {
	if !a.setTeamReviewStatus(ctx, rv.ID, status) {
		return
	}
	rv.Status = status
	if err := a.apiClient().chatUpdate(ctx, rv.Channel, rv.TS, decisionFallback(rv), decisionBlocks(rv)); err != nil {
		a.Logger.Warn("slack: decision status rewrite failed", "decision", rv.ID, "error", err)
	}
}

// finishDecision records the answer and returns the record as closed by it.
// A decision the asker closed meanwhile keeps that outcome (ok is false: the
// message already says so).
func (a *Adapter) finishDecision(ctx context.Context, id, user string, ans decisionAnswer) (answered store.Review, ok bool) {
	now := time.Now()
	_, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		ok = false
		if r.Decision == nil || (r.Done && r.Decision.Outcome != "") {
			return false
		}
		d := *r.Decision
		d.Outcome, d.Choice, d.Answer, d.ClosedAt = store.DecisionAnswered, ans.choice, ans.text, now
		r.Decision, r.Done, r.DecidedBy, r.Status = &d, true, user, ""
		answered, ok = *r, true
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: decision finish failed", "record", "decision_store_failed", "decision", id, "error", err)
		return store.Review{}, false
	}
	return answered, ok
}

// decisionNotice tells a person answering a decision that is closed how it
// closed, or whose answer is in flight.
func decisionNotice(rv store.Review) string {
	switch {
	case rv.Decision.Outcome == store.DecisionDefaulted:
		return decisionDefaultedNotice
	case rv.Decision.Outcome == store.DecisionWithdrawn:
		return decisionWithdrawnNotice
	case !rv.Done:
		return fmt.Sprintf(decisionPendingNotice, rv.DecidedBy)
	case rv.DecidedBy != "":
		return fmt.Sprintf(decisionAnsweredByNotice, rv.DecidedBy)
	}
	return decisionAnsweredNotice
}
