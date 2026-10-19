package slack

import (
	"context"
	"fmt"
	"strings"
	"time"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// sessionConnectSurfaces is where a Session start's Connect prompt put its
// button so the person finds it, per (user, thread): in a channel thread the
// waiting line everyone sees and the card in the person's DM, in a DM
// conversation the latest card, a message of the conversation. A re-issued
// prompt refreshes the line and the DM card in place, and the settled
// consent rewrites them. Kept for pendingTTL, the lifetime of a parked
// message: the link's own expiry only ends the landing.
type sessionConnectSurfaces struct {
	server   string
	channel  string // the thread's channel
	threadTS string
	lineTS   string // the waiting line in the thread; "" when none was posted
	dmed     bool   // the line says the DM holds the card
	// cardChannel and cardTS address the card: the DM card of a channel
	// prompt, or the latest message of a DM conversation; "" when none was
	// posted.
	cardChannel string
	cardTS      string
	// states are the completion states the prompts minted, each holding the
	// message of its turn: dropped once the consent is settled, so a stale
	// button runs no message twice.
	states []string
}

// lastState is the completion state of the latest prompt, "" when none was
// minted: the cards' Authorize all carries it, so a grant from the modal it
// opens runs the held message as the prompt's own link would.
func (s sessionConnectSurfaces) lastState() string {
	if len(s.states) == 0 {
		return ""
	}
	return s.states[len(s.states)-1]
}

func sessionConnectKey(slackUser, threadID string) string { return slackUser + "\x00" + threadID }

// sessionConnectFor returns the live surfaces of (slackUser, threadID).
func (a *Adapter) sessionConnectFor(slackUser, threadID string) (sessionConnectSurfaces, bool) {
	a.sessionConnectMu.Lock()
	defer a.sessionConnectMu.Unlock()
	entry, ok := a.sessionConnects[sessionConnectKey(slackUser, threadID)]
	if !ok || !time.Now().Before(entry.expires) {
		return sessionConnectSurfaces{}, false
	}
	return entry.value, true
}

// recordSessionConnect keeps the surfaces of (slackUser, threadID).
func (a *Adapter) recordSessionConnect(slackUser, threadID string, s sessionConnectSurfaces) {
	now := time.Now()
	a.sessionConnectMu.Lock()
	defer a.sessionConnectMu.Unlock()
	if a.sessionConnects == nil {
		a.sessionConnects = make(map[string]ttlEntry[sessionConnectSurfaces])
	}
	sweepExpired(a.sessionConnects, now)
	a.sessionConnects[sessionConnectKey(slackUser, threadID)] = ttlEntry[sessionConnectSurfaces]{value: s, expires: now.Add(pendingTTL)}
}

// takeSessionConnect returns and forgets the surfaces of (slackUser,
// threadID), so the settled consent rewrites them once and a later prompt in
// the thread starts afresh.
func (a *Adapter) takeSessionConnect(slackUser, threadID string) (sessionConnectSurfaces, bool) {
	a.sessionConnectMu.Lock()
	defer a.sessionConnectMu.Unlock()
	entry, ok := a.sessionConnects[sessionConnectKey(slackUser, threadID)]
	delete(a.sessionConnects, sessionConnectKey(slackUser, threadID))
	if !ok || !time.Now().Before(entry.expires) {
		return sessionConnectSurfaces{}, false
	}
	return entry.value, true
}

// takeOtherSessionConnects returns and forgets the live surfaces of every
// thread but threadID where slackUser waits for server.
func (a *Adapter) takeOtherSessionConnects(slackUser, server, threadID string) []sessionConnectSurfaces {
	now := time.Now()
	prefix := sessionConnectKey(slackUser, "")
	a.sessionConnectMu.Lock()
	defer a.sessionConnectMu.Unlock()
	var taken []sessionConnectSurfaces
	for key, entry := range a.sessionConnects {
		if !strings.HasPrefix(key, prefix) || entry.value.server != server || entry.value.threadTS == threadID || !now.Before(entry.expires) {
			continue
		}
		taken = append(taken, entry.value)
		delete(a.sessionConnects, key)
	}
	return taken
}

// promptSessionConnect answers a Session start muster refused with its
// sign-in challenge for a server: the person the start ran as is signed in,
// but that server waits for their one-time consent.
//
// msg is the message as admitted (the raw Slack user as Subject), the one a
// replay admits again. When the start ran as the sender, the Connect prompt
// for the server goes to them with the message held behind it: with a
// public base URL the link lands back on the gateway and the landing runs the
// held message as the thread's first turn, without one the prompt says to
// send the message again once connected. The hold is the process's own, like
// every connector completion: after a restart the landing says the link is
// gone and the person sends the message again.
//
// Where the prompt goes follows the sign-in prompt (klaus-gateway#406). In a
// DM conversation the card is a message of the conversation: one reader, and
// only real messages render in the assistant pane; a later message gets a
// card of its own under it, and the earlier card loses its button. In a
// channel thread the link is the person's alone, so the card is ephemeral; an
// ephemeral reaches only a Slack client that is open when it goes out and
// shows nothing from the channel, so the same card also goes to the person's
// DM, and a line everyone in the thread sees says whom it waits for and where
// the button is. A re-issued prompt refreshes the DM card in place; the line
// stays. Only the ephemeral's Connect button carries the completion state in
// its value: a click on it records the response_url that replaces it, while
// the cards, real messages, are rewritten in place by settleSessionConnect.
// Every surface's Authorize all carries the state, so the modal it opens
// grants the server on the prompt's own completion.
//
// When the start ran as the thread's owner (asOwner: a collaborator's turn
// that starts the owner's Session), the challenge and its link are the
// owner's, so the sender gets no link: a private note that names the owner,
// whose own next message gets the prompt.
func (a *Adapter) promptSessionConnect(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser, owner string, asOwner bool, challenge *pkga2a.ConnectRequiredError) {
	server := challenge.Server
	a.Logger.Info("slack: session start waits for the person's consent to a server",
		"record", "session_connect_required",
		"server", server,
		"agent", msg.AgentRef,
		"slack_user", slackUser,
		"owner", owner,
		"channel_id", msg.ChannelID,
		"thread_id", msg.ThreadID,
	)
	client := a.apiClient()
	if asOwner {
		text := fmt.Sprintf(sessionConnectOwnerNote, owner, escapeMrkdwn(server))
		if err := client.postEphemeralText(ctx, slackChannel, slackUser, msg.ThreadID, text); err != nil {
			a.Logger.Warn("slack: post session connect note failed", "thread", msg.ThreadID, "server", server, "error", err)
		}
		return
	}
	surfaces, _ := a.sessionConnectFor(slackUser, msg.ThreadID)
	surfaces.server, surfaces.channel, surfaces.threadTS = server, slackChannel, msg.ThreadID
	promptURL, connectValue, tail := challenge.LoginURL, server, sessionConnectResendTail
	if base := a.PublicBaseURL; base != "" {
		held := heldMessageOf(msg, slackChannel, time.Now())
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: slackUser, server: server, channel: slackChannel, threadTS: msg.ThreadID, held: &held})
		if decorated, err := decorateConnectorLoginURL(challenge.LoginURL, base, stateID); err != nil {
			a.Logger.Warn("slack: session connect login URL decoration failed, posting plain link", "server", server, "error", err)
		} else {
			promptURL, connectValue, tail = decorated, stateID, sessionConnectResumeTail
			surfaces.states = append(surfaces.states, stateID)
		}
	}
	text := fmt.Sprintf(sessionConnectNote, escapeMrkdwn(server)) + tail

	if isDMChannelID(slackChannel) {
		ts, err := client.postConnectPromptMessage(ctx, slackChannel, msg.ThreadID, text, server, promptURL, server, surfaces.lastState())
		if err != nil {
			a.Logger.Warn("slack: post session connect prompt failed", "thread", msg.ThreadID, "server", server, "error", err)
			a.recordSessionConnect(slackUser, msg.ThreadID, surfaces)
			return
		}
		// The earlier card loses its button only once the new one is there.
		if surfaces.cardTS != "" {
			superseded := fmt.Sprintf(sessionConnectSupersededNote, escapeMrkdwn(server))
			if err := client.chatUpdate(ctx, surfaces.cardChannel, surfaces.cardTS, superseded, []any{sectionBlock(superseded)}); err != nil {
				a.Logger.Warn("slack: retire earlier session connect card failed", "thread", msg.ThreadID, "error", err)
			}
		}
		surfaces.cardChannel, surfaces.cardTS = slackChannel, ts
		a.recordSessionConnect(slackUser, msg.ThreadID, surfaces)
		return
	}

	origin := a.signInOrigin(ctx, client, slackChannel, msg.ThreadID)
	dmed := a.sendSessionConnectDM(ctx, client, &surfaces, slackUser, origin, text, promptURL)
	a.ensureSessionConnectLine(ctx, client, &surfaces, slackUser, dmed)
	a.recordSessionConnect(slackUser, msg.ThreadID, surfaces)
	if err := client.postConnectPrompt(ctx, slackChannel, msg.ThreadID, slackUser, text, server, promptURL, connectValue, false); err != nil {
		a.Logger.Warn("slack: post session connect prompt failed", "thread", msg.ThreadID, "server", server, "error", err)
	}
}

// sendSessionConnectDM puts the Connect card into slackUser's DM, led by
// origin, and reports whether it is there. A card already there for this
// (user, thread) is refreshed to the new button in place, so the DM holds one
// live card however often the prompt is re-issued; one that cannot be
// refreshed is replaced. A failure costs only the DM: the ephemeral still
// goes out, and the line does not point at a DM that is not there.
func (a *Adapter) sendSessionConnectDM(ctx context.Context, client *slackAPIClient, s *sessionConnectSurfaces, slackUser, origin, text, loginURL string) bool {
	if s.cardTS != "" {
		err := client.updateConnectPrompt(ctx, s.cardChannel, s.cardTS, origin, text, s.server, loginURL, s.server, s.lastState())
		if err == nil {
			return true
		}
		a.Logger.Warn("slack: refresh session connect DM failed, posting a new one", "user", slackUser, "thread", s.threadTS, "error", err)
	}
	channel, ts, err := client.postConnectPromptDM(ctx, slackUser, origin, text, s.server, loginURL, s.server, s.lastState())
	if err != nil {
		a.Logger.Warn("slack: post session connect DM failed", "user", slackUser, "thread", s.threadTS, "error", err)
		s.cardChannel, s.cardTS = "", ""
		return false
	}
	s.cardChannel, s.cardTS = channel, ts
	return true
}

// ensureSessionConnectLine posts the thread's waiting line, or rewrites the
// one there when whether the DM holds the card changed. The line carries no
// link: that is the person's alone.
func (a *Adapter) ensureSessionConnectLine(ctx context.Context, client *slackAPIClient, s *sessionConnectSurfaces, slackUser string, dmed bool) {
	if s.lineTS != "" && s.dmed == dmed {
		return
	}
	text := fmt.Sprintf(sessionConnectWaitingFormat, slackUser, escapeMrkdwn(s.server))
	if dmed {
		text += sessionConnectWaitingDMHint
	}
	if s.lineTS == "" {
		ts, err := client.postContextMessage(ctx, s.channel, s.threadTS, text)
		if err != nil {
			a.Logger.Warn("slack: post session connect line failed", "thread", s.threadTS, "error", err)
			return
		}
		s.lineTS, s.dmed = ts, dmed
		return
	}
	if err := client.chatUpdate(ctx, s.channel, s.lineTS, text, []any{contextBlock(text)}); err != nil {
		a.Logger.Warn("slack: rewrite session connect line failed", "thread", s.threadTS, "error", err)
		return
	}
	s.dmed = dmed
}

// settleSessionConnect rewrites the surfaces of the prompt (slackUser,
// threadID) got, once the consent is given: the waiting line to who
// connected, the card to the confirmation. Called by the landing of the
// prompt's own link, which then runs the held message (delivered), and by
// the person's next turn in the thread that starts its Session, however they
// connected (another muster client, a gateway without a public base URL),
// when the held message stays unsent and the
// card must not say otherwise. The prompts' completion states go with the
// surfaces, so a stale button runs no held message after the fact. The
// ephemeral is replaced through its click's response_url by the landing,
// when the click reached this process.
func (a *Adapter) settleSessionConnect(ctx context.Context, slackUser, threadID string, delivered bool) {
	s, ok := a.takeSessionConnect(slackUser, threadID)
	if !ok {
		return
	}
	a.dropConnectorCompletions(s.states)
	a.rewriteSessionConnect(ctx, slackUser, s, delivered)
}

// settleOtherSessionConnects settles, once a landing settled its own thread,
// every other thread where slackUser waits for server: the consent is the
// person's per server, not per thread. Each thread's held message, the one
// of its latest prompt, runs as the person in that thread, as the landing
// runs its own; a thread whose held message has expired gets the confirmation
// alone.
func (a *Adapter) settleOtherSessionConnects(ctx context.Context, slackUser, server, threadID string) {
	for _, s := range a.takeOtherSessionConnects(slackUser, server, threadID) {
		held := a.takeHeldMessage(s.states)
		a.rewriteSessionConnect(ctx, slackUser, s, held != nil)
		if held == nil {
			continue
		}
		entry := connectorCompletion{slackUser: slackUser, server: server, channel: s.channel, threadTS: s.threadTS, held: held}
		a.background(func(ctx context.Context) { a.resumeAfterConnectorSignIn(ctx, entry) })
	}
}

// rewriteSessionConnect rewrites the line and the card of s to who connected.
func (a *Adapter) rewriteSessionConnect(ctx context.Context, slackUser string, s sessionConnectSurfaces, delivered bool) {
	client := a.apiClient()
	if s.lineTS != "" {
		text := fmt.Sprintf(sessionConnectConnectedFormat, slackUser, escapeMrkdwn(s.server))
		if err := client.chatUpdate(ctx, s.channel, s.lineTS, text, []any{contextBlock(text)}); err != nil {
			a.Logger.Warn("slack: rewrite session connect line after sign-in failed", "thread", s.threadTS, "error", err)
		}
	}
	if s.cardTS != "" {
		text := fmt.Sprintf(sessionConnectCardConnected, escapeMrkdwn(s.server))
		if delivered {
			text = fmt.Sprintf(sessionConnectCardDone, escapeMrkdwn(s.server))
		}
		if err := client.chatUpdate(ctx, s.cardChannel, s.cardTS, text, []any{sectionBlock(text)}); err != nil {
			a.Logger.Warn("slack: rewrite session connect card after sign-in failed", "thread", s.threadTS, "error", err)
		}
	}
}

// postConnectPromptMessage posts a Connect prompt as a message of the
// conversation, in threadID when set, and returns its ts.
func (c *slackAPIClient) postConnectPromptMessage(ctx context.Context, channel, threadID, text, server, loginURL, connectValue, authorizeOrigin string) (string, error) {
	return c.postBlocks(ctx, channel, threadID, text, connectPromptBlocks(text, server, loginURL, connectValue, authorizeOrigin, false, c.authorizeAll))
}

// postConnectPromptDM posts a Connect prompt's card to user's DM, led by
// origin, where it waits for a Slack client that was closed when the
// ephemeral went out. The DM is addressed by user ID, so the D… conversation
// Slack put it in comes back with its ts for the rewrites.
func (c *slackAPIClient) postConnectPromptDM(ctx context.Context, user, origin, text, server, loginURL, connectValue, authorizeOrigin string) (channel, ts string, err error) {
	blocks := append([]any{contextBlock(origin)}, connectPromptBlocks(text, server, loginURL, connectValue, authorizeOrigin, false, c.authorizeAll)...)
	resp, err := c.postJSONResponse(ctx, methodChatPostMessage, map[string]any{paramChannel: user, paramText: text, paramBlocks: blocks})
	if err != nil {
		return "", "", err
	}
	if resp.Channel == "" {
		resp.Channel = user
	}
	return resp.Channel, resp.Ts, nil
}

// updateConnectPrompt rewrites the Connect card at (channel, ts) to a fresh
// button, led by origin when there is one.
func (c *slackAPIClient) updateConnectPrompt(ctx context.Context, channel, ts, origin, text, server, loginURL, connectValue, authorizeOrigin string) error {
	blocks := connectPromptBlocks(text, server, loginURL, connectValue, authorizeOrigin, false, c.authorizeAll)
	if origin != "" {
		blocks = append([]any{contextBlock(origin)}, blocks...)
	}
	return c.chatUpdate(ctx, channel, ts, text, blocks)
}
