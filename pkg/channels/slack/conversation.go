package slack

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A conversation is a thread a service holds with one person on behalf of an
// agent the gateway does not run, such as a person's guide on their own
// machine (POST /conversations, pkg/reviews). The opening message is a direct
// message from the app; the service posts every later message into its thread
// (POST /conversations/{id}/messages). A reply of the person's in the thread
// calls the conversation's reply tool through muster as the person, with
// "message" added: the reply as an A2A Message whose contextId is the
// conversation. The tool decides whom the person may reach; a refusal (no
// guide running, another person's guide) is a note in the thread saying the
// reply was not delivered and why. Nothing is queued anywhere else.
//
// The record lives in the review store (store.Review with Conversation set)
// under the decision's naming, "<channel>-<ts>" of the opening message, so the
// one lookup a reply under a bot message costs finds a decision or a
// conversation alike. Its TTL slides with every message.

// conversationKeep is how long a conversation outlives its latest message.
const conversationKeep = 30 * 24 * time.Hour

// What a conversation's messages and notes say.
const (
	conversationFromLine         = "%s · reply in this thread"
	conversationNotYours         = "This thread is <@%s>'s conversation with %s; only they can write to it."
	conversationNotDelivered     = "Not delivered to %s: %s"
	conversationFailed           = "Not delivered to %s: it could not be reached right now. Send it again in a moment."
	conversationConnectNotice    = "Not delivered to %s yet. *muster* asks you to sign in once:"
	conversationConnectTail      = "Open it with *Sign in*: your reply goes to %s once you're signed in."
	conversationSignInUnfinished = "Not delivered to %s: the sign-in did not finish in time. Send it again once you're signed in."
	conversationDelivered        = "incoming_envelope"
)

// OpenConversation sends the opening message to the person by direct message
// and records the thread's binding. It implements channels.ConversationPoster.
func (a *Adapter) OpenConversation(ctx context.Context, conversation channels.Conversation) (channels.PostReceipt, error) {
	if a.Tools == nil || a.OBO == nil {
		return channels.PostReceipt{}, errors.New("slack: conversations need a tool caller and account linking")
	}
	user, err := a.apiClient().lookupUserByEmail(ctx, conversation.Person)
	if err != nil {
		return channels.PostReceipt{}, fmt.Errorf("slack: find %s: %w", conversation.Person, err)
	}
	resp, err := a.apiClient().postJSONResponse(ctx, methodChatPostMessage, map[string]any{
		paramChannel: user,
		paramText:    fallbackText(conversation.Text),
		paramBlocks: append(markdownBlocks(conversation.Text),
			contextBlock(truncateRunes(fmt.Sprintf(conversationFromLine, escapeMrkdwn(conversation.From)), slackSectionTextMax))),
	})
	if err != nil {
		return channels.PostReceipt{}, err
	}
	// A direct message is addressed by user ID; its thread lives in the D…
	// conversation Slack put it in.
	channel := resp.Channel
	if channel == "" {
		channel = user
	}
	rv := store.Review{
		ID:      decisionID(channel, resp.Ts),
		Channel: channel, TS: resp.Ts,
		Text: conversation.Text,
		Tool: conversation.Reply.Tool, Arguments: conversation.Reply.Arguments,
		Conversation: &store.Conversation{Person: conversation.Person, SlackUser: user, From: conversation.From},
		PostedAt:     time.Now(),
		TTL:          conversationKeep,
	}
	if err := a.reviews().PutReview(ctx, rv); err != nil {
		return channels.PostReceipt{}, fmt.Errorf("slack: record conversation: %w", err)
	}
	return channels.PostReceipt{ID: rv.ID, Channel: rv.Channel, TS: rv.TS}, nil
}

// PostConversationMessage posts the service's message into the conversation's
// thread and keeps the conversation for another conversationKeep. It
// implements channels.ConversationPoster; a conversation the gateway holds no
// record of is channels.ErrConversationNotFound.
func (a *Adapter) PostConversationMessage(ctx context.Context, id string, message channels.ConversationMessage) (channels.PostReceipt, error) {
	rv, found, err := a.reviews().GetReview(ctx, id)
	switch {
	case err != nil:
		return channels.PostReceipt{}, fmt.Errorf("slack: conversation lookup: %w", err)
	case !found || rv.Conversation == nil:
		return channels.PostReceipt{}, channels.ErrConversationNotFound
	}
	ts, err := a.apiClient().postJSON(ctx, methodChatPostMessage, map[string]any{
		paramChannel:  rv.Channel,
		paramThreadTS: rv.TS,
		paramText:     fallbackText(message.Text),
		paramBlocks:   markdownBlocks(message.Text),
	})
	if err != nil {
		return channels.PostReceipt{}, err
	}
	a.keepConversation(ctx, id)
	return channels.PostReceipt{ID: id, Channel: rv.Channel, TS: ts}, nil
}

// keepConversation slides the conversation's TTL to conversationKeep from now.
func (a *Adapter) keepConversation(ctx context.Context, id string) {
	now := time.Now()
	if _, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		r.TTL = now.Sub(r.PostedAt) + conversationKeep
		return true
	}); err != nil {
		a.Logger.Warn("slack: conversation keep failed", "record", "conversation_store_failed", "conversation", id, "error", err)
	}
}

// relayConversationReply delivers a reply in the conversation's thread to the
// conversation's agent as the person who wrote it. Only the person's own
// replies go; anyone else in the thread is told it is not theirs.
func (a *Adapter) relayConversationReply(ctx context.Context, rv store.Review, inner slackInnerEvent) {
	conv := rv.Conversation
	from := "*" + escapeMrkdwn(conv.From) + "*"
	if inner.User != conv.SlackUser {
		notice := fmt.Sprintf(conversationNotYours, conv.SlackUser, from)
		if err := a.apiClient().postEphemeralText(ctx, rv.Channel, inner.User, rv.TS, notice); err != nil {
			a.Logger.Warn("slack: conversation notice failed", "conversation", rv.ID, "user", inner.User, "error", err)
		}
		return
	}
	text := strings.TrimSpace(slackTextUnescaper.Replace(inner.Text))
	if text == "" || a.Tools == nil {
		return
	}
	token, ok, signIn := a.humanToken(ctx, rv.Channel, rv.TS, inner.User)
	if signIn {
		a.postSignIn(ctx, rv.Channel, rv.TS, inner.User, false, signInForReply)
	}
	if !ok {
		return
	}
	args := maps.Clone(rv.Arguments)
	if args == nil {
		args = map[string]any{}
	}
	args["message"] = conversationMessage(rv.ID, inner.Channel, inner.TS, truncateRunes(text, channels.ConversationTextMax))

	res, err := a.Tools.CallTool(ctx, token, rv.Tool, args)
	a.settleConversationReply(ctx, rv, inner.User, conversationReply{channel: inner.Channel, ts: inner.TS}, res, err, false)
}

// conversationReply is a reply of the person's in a conversation's thread:
// the Slack message it was.
type conversationReply struct {
	channel, ts string
}

// settleConversationReply tells the person what became of their reply: a
// note when it was not delivered, a Connect prompt when muster asks for a
// sign-in first, the delivered reaction otherwise. accepted is true when res
// answers the retry that accepted such a sign-in: muster asking again then
// means the sign-in did not complete in time, and is not prompted again.
func (a *Adapter) settleConversationReply(ctx context.Context, rv store.Review, user string, reply conversationReply, res muster.Result, err error, accepted bool) {
	from := "*" + escapeMrkdwn(rv.Conversation.From) + "*"
	note := ""
	switch {
	case err != nil:
		a.Logger.Warn("slack: conversation reply tool call failed", "conversation", rv.ID, "tool", rv.Tool, "slack_user", user, "error", err)
		note = fmt.Sprintf(conversationFailed, from)
	case res.SignIn != nil && accepted:
		a.Logger.Warn("slack: conversation reply still asks for a sign-in after the accepted one", "conversation", rv.ID, "tool", rv.Tool, "slack_user", user)
		note = fmt.Sprintf(conversationSignInUnfinished, from)
	case res.SignIn != nil:
		a.Logger.Info("slack: conversation reply needs the person's sign-in", "record", "conversation_connect",
			"conversation", rv.ID, "tool", rv.Tool, "slack_user", user)
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: user, channel: rv.Channel, threadTS: rv.TS, signIn: res.SignIn, conversation: rv.ID, reply: reply})
		text := signInPromptText(fmt.Sprintf(conversationConnectNotice, from), res.SignIn.Message, res.SignIn.URL, fmt.Sprintf(conversationConnectTail, from))
		if err := a.apiClient().postConnectPrompt(ctx, rv.Channel, rv.TS, user, text, res.SignIn.URL, stateID); err != nil {
			a.Logger.Warn("slack: conversation connect prompt failed", "conversation", rv.ID, "error", err)
		}
		return
	case res.IsError:
		a.Logger.Info("slack: conversation reply refused by the tool", "record", "conversation_refused",
			"conversation", rv.ID, "tool", rv.Tool, "slack_user", user, "reason", res.Text)
		reason := "the recipient refused it"
		if res.Text != "" {
			reason = truncateRunes(escapeMrkdwn(res.Text), teamReviewReasonMax)
		}
		note = fmt.Sprintf(conversationNotDelivered, from, reason)
	}
	if note != "" {
		if _, err := a.apiClient().postNote(ctx, rv.Channel, note, rv.TS); err != nil {
			a.Logger.Warn("slack: conversation note failed", "conversation", rv.ID, "error", err)
		}
		return
	}
	a.Logger.Info("slack: conversation reply delivered", "record", "conversation_delivered",
		"conversation", rv.ID, "tool", rv.Tool, "slack_user", user, "subject", a.linkedSubject(user), "resumed", accepted)
	a.keepConversation(ctx, rv.ID)
	if err := a.apiClient().reactionsAdd(ctx, reply.channel, reply.ts, conversationDelivered); err != nil {
		a.Logger.Debug("slack: conversation delivered reaction failed", "conversation", rv.ID, "error", err)
	}
}

// resumeConversationReply accepts the sign-in a conversation reply was asked
// for, as its writer: muster waits for the sign-in, then delivers the reply.
func (a *Adapter) resumeConversationReply(ctx context.Context, entry connectorCompletion) {
	rv, found, err := a.reviews().GetReview(ctx, entry.conversation)
	if err != nil || !found || rv.Conversation == nil {
		a.Logger.Warn("slack: conversation lookup after sign-in failed", "conversation", entry.conversation, "found", found, "error", err)
		return
	}
	if a.SignIns == nil || a.OBO == nil {
		return
	}
	token, err := a.OBO.TokenFor(ctx, entry.slackUser)
	if err != nil {
		a.Logger.Warn("slack: token to accept a sign-in unavailable", "user", entry.slackUser, "error", err)
		return
	}
	res, err := a.SignIns.Accept(ctx, token, entry.signIn)
	a.settleConversationReply(ctx, rv, entry.slackUser, entry.reply, res, err, true)
}

// a2aPartText is an A2A text part's kind and the key of its text.
const a2aPartText = "text"

// conversationMessage is a reply as an A2A Message: its id names the Slack
// message, so a redelivered event is the same message; its contextId the
// conversation, which the agent answers through
// POST /conversations/{contextId}/messages.
func conversationMessage(id, channel, ts, text string) map[string]any {
	return map[string]any{
		"messageId": "slack-" + channel + "-" + ts,
		"role":      "user",
		"contextId": id,
		"parts":     []any{map[string]any{"kind": a2aPartText, a2aPartText: text}},
		"metadata":  map[string]any{"source": "slack", "conversation": id},
	}
}
