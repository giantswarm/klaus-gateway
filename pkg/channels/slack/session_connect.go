package slack

import (
	"context"
	"fmt"
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
// message: the link's own expiry only ends its click's continuation.
type sessionConnectSurfaces struct {
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
	// tool is the muster tool whose call the latest prompt's sign-in was
	// asked for: a sign-in completed for the person on one thread's call of
	// it is the one every other thread waiting on that tool waits for.
	tool string
}

// lastState is the completion state of the latest prompt, "" when none was
// minted: every surface's link button carries it, so the person's click on any
// of them continues the start.
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
// thread but threadID where slackUser waits for a sign-in to tool.
func (a *Adapter) takeOtherSessionConnects(slackUser, tool, threadID string) []sessionConnectSurfaces {
	if tool == "" {
		return nil
	}
	now := time.Now()
	a.sessionConnectMu.Lock()
	defer a.sessionConnectMu.Unlock()
	var taken []sessionConnectSurfaces
	for key, entry := range a.sessionConnects {
		surfaces := entry.value
		if surfaces.threadTS == threadID || surfaces.tool != tool || key != sessionConnectKey(slackUser, surfaces.threadTS) || !now.Before(entry.expires) {
			continue
		}
		taken = append(taken, surfaces)
		delete(a.sessionConnects, key)
	}
	return taken
}

// promptSessionConnect answers a Session start muster asked a sign-in for:
// the person the start ran as is signed in, but a server the start goes
// through waits for their one-time consent.
//
// msg is the message as admitted (the raw Slack user as Subject), the one a
// replay admits again. When the start ran as the sender, the Connect prompt
// goes to them with the message held behind it: their click on its link is
// their consent, which accepts the sign-in (muster waits for it, then starts
// the Session) and runs the held message as the thread's first turn. The hold
// is the process's own, like every connector completion: after a restart the
// click continues nothing and the person sends the message again.
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
// stays. Every surface's link button carries the latest completion state; the
// ephemeral is rewritten through its click's response_url, the cards, real
// messages, in place by settleSessionConnect.
//
// When the start ran as the thread's owner (asOwner: a collaborator's turn
// that starts the owner's Session), the sign-in and its link are the owner's,
// so the sender gets no link: a private note that names the owner, whose own
// next message gets the prompt.
func (a *Adapter) promptSessionConnect(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser, owner string, asOwner bool, challenge *pkga2a.ConnectRequiredError) {
	a.Logger.Info("slack: session start waits for the person's sign-in",
		"record", "session_connect_required",
		"agent", msg.AgentRef,
		"slack_user", slackUser,
		"owner", owner,
		"channel_id", msg.ChannelID,
		"thread_id", msg.ThreadID,
	)
	client := a.apiClient()
	if asOwner {
		text := fmt.Sprintf(sessionConnectOwnerNote, owner)
		if err := client.postEphemeralText(ctx, slackChannel, slackUser, msg.ThreadID, text); err != nil {
			a.Logger.Warn("slack: post session connect note failed", "thread", msg.ThreadID, "error", err)
		}
		return
	}
	signIn := challenge.SignIn
	surfaces, _ := a.sessionConnectFor(slackUser, msg.ThreadID)
	surfaces.channel, surfaces.threadTS = slackChannel, msg.ThreadID
	held := heldMessageOf(msg, slackChannel, time.Now())
	stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: slackUser, channel: slackChannel, threadTS: msg.ThreadID, signIn: signIn, held: &held})
	surfaces.states = append(surfaces.states, stateID)
	surfaces.tool = signIn.Tool()
	text := signInPromptText(sessionConnectNote, signIn.Message, signIn.URL, sessionConnectTail)

	if isDMChannelID(slackChannel) {
		ts, err := client.postConnectPromptMessage(ctx, slackChannel, msg.ThreadID, text, signIn.URL, stateID)
		if err != nil {
			a.Logger.Warn("slack: post session connect prompt failed", "thread", msg.ThreadID, "error", err)
			a.recordSessionConnect(slackUser, msg.ThreadID, surfaces)
			return
		}
		// The earlier card loses its button only once the new one is there.
		if surfaces.cardTS != "" {
			if err := client.chatUpdate(ctx, surfaces.cardChannel, surfaces.cardTS, sessionConnectSupersededNote, []any{sectionBlock(sessionConnectSupersededNote)}); err != nil {
				a.Logger.Warn("slack: retire earlier session connect card failed", "thread", msg.ThreadID, "error", err)
			}
		}
		surfaces.cardChannel, surfaces.cardTS = slackChannel, ts
		a.recordSessionConnect(slackUser, msg.ThreadID, surfaces)
		return
	}

	origin := a.signInOrigin(ctx, client, slackChannel, msg.ThreadID)
	dmed := a.sendSessionConnectDM(ctx, client, &surfaces, slackUser, origin, text, signIn.URL)
	a.ensureSessionConnectLine(ctx, client, &surfaces, slackUser, dmed)
	a.recordSessionConnect(slackUser, msg.ThreadID, surfaces)
	if err := client.postConnectPrompt(ctx, slackChannel, msg.ThreadID, slackUser, text, signIn.URL, stateID); err != nil {
		a.Logger.Warn("slack: post session connect prompt failed", "thread", msg.ThreadID, "error", err)
	}
}

// sendSessionConnectDM puts the Connect card into slackUser's DM, led by
// origin, and reports whether it is there. A card already there for this
// (user, thread) is refreshed to the new button in place, so the DM holds one
// live card however often the prompt is re-issued; one that cannot be
// refreshed is replaced. A failure costs only the DM: the ephemeral still
// goes out, and the line does not point at a DM that is not there.
func (a *Adapter) sendSessionConnectDM(ctx context.Context, client *slackAPIClient, s *sessionConnectSurfaces, slackUser, origin, text, link string) bool {
	if s.cardTS != "" {
		err := client.updateConnectPrompt(ctx, s.cardChannel, s.cardTS, origin, text, link, s.lastState())
		if err == nil {
			return true
		}
		a.Logger.Warn("slack: refresh session connect DM failed, posting a new one", "user", slackUser, "thread", s.threadTS, "error", err)
	}
	channel, ts, err := client.postConnectPromptDM(ctx, slackUser, origin, text, link, s.lastState())
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
	text := fmt.Sprintf(sessionConnectWaitingFormat, slackUser)
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

// sessionConnectOutcome is how a Session start's sign-in prompt settled.
type sessionConnectOutcome int

const (
	// sessionConnectDelivered: the prompt's own click continued, and the
	// held message runs.
	sessionConnectDelivered sessionConnectOutcome = iota
	// sessionConnectSignedIn: the person's next turn found them signed in;
	// the held message stays unsent.
	sessionConnectSignedIn
	// sessionConnectDeclined: the person chose Not now; the held message is
	// dropped.
	sessionConnectDeclined
)

// settleSessionConnect rewrites the surfaces of the prompt (slackUser,
// threadID) got once it settled: the waiting line to who signed in or did
// not, the card to the outcome. Called by the continuation of the prompt's
// own click, which then runs the held message, by its Not now, and by the
// person's next turn in the thread that starts its Session, however they
// signed in (another muster client, another thread's prompt for another
// tool), when the held message stays unsent and the card must not say
// otherwise. The prompts' completion states go with the
// surfaces, so a stale button runs no held message after the fact. The
// ephemeral is replaced through its click's response_url by the continuation.
func (a *Adapter) settleSessionConnect(ctx context.Context, slackUser, threadID string, outcome sessionConnectOutcome) {
	s, ok := a.takeSessionConnect(slackUser, threadID)
	if !ok {
		return
	}
	a.dropConnectorCompletions(s.states)
	a.rewriteSessionConnect(ctx, slackUser, s, outcome)
}

// settleOtherSessionConnects settles, once slackUser signed in through the
// prompt of threadID, every other thread where they wait for a sign-in to the
// same tool: the sign-in is the person's, not the thread's. A thread's held
// message, the one of its latest prompt, runs as the person in that thread;
// a thread whose held message expired gets the confirmation alone, and one
// whose own click already continues it is only rewritten.
func (a *Adapter) settleOtherSessionConnects(ctx context.Context, slackUser, tool, threadID string) {
	for _, s := range a.takeOtherSessionConnects(slackUser, tool, threadID) {
		held, continuing := a.takeHeldStart(s.states)
		outcome := sessionConnectSignedIn
		if held != nil || continuing {
			outcome = sessionConnectDelivered
		}
		a.rewriteSessionConnect(ctx, slackUser, s, outcome)
		if held != nil {
			threadTS := s.threadTS
			a.background(func(ctx context.Context) { a.runHeldMessage(ctx, slackUser, threadTS, held) })
		}
	}
}

// rewriteSessionConnect rewrites the line and the card of s to outcome.
func (a *Adapter) rewriteSessionConnect(ctx context.Context, slackUser string, s sessionConnectSurfaces, outcome sessionConnectOutcome) {
	client := a.apiClient()
	line, card := sessionConnectConnectedFormat, sessionConnectCardConnected
	switch outcome {
	case sessionConnectDelivered:
		card = sessionConnectCardDone
	case sessionConnectDeclined:
		line, card = sessionConnectDeclinedFormat, connectorDeclinedNotice
	}
	if s.lineTS != "" {
		text := fmt.Sprintf(line, slackUser)
		if err := client.chatUpdate(ctx, s.channel, s.lineTS, text, []any{contextBlock(text)}); err != nil {
			a.Logger.Warn("slack: rewrite session connect line after sign-in failed", "thread", s.threadTS, "error", err)
		}
	}
	if s.cardTS != "" {
		text := card
		if err := client.chatUpdate(ctx, s.cardChannel, s.cardTS, text, []any{sectionBlock(text)}); err != nil {
			a.Logger.Warn("slack: rewrite session connect card after sign-in failed", "thread", s.threadTS, "error", err)
		}
	}
}

// postConnectPromptMessage posts a Connect prompt as a message of the
// conversation, in threadID when set, and returns its ts.
func (c *slackAPIClient) postConnectPromptMessage(ctx context.Context, channel, threadID, text, link, connectValue string) (string, error) {
	return c.postBlocks(ctx, channel, threadID, text, connectPromptBlocks(text, signInButtonLabel, link, connectValue, true, c.authorizeAll))
}

// postConnectPromptDM posts a Connect prompt's card to user's DM, led by
// origin, where it waits for a Slack client that was closed when the
// ephemeral went out. The DM is addressed by user ID, so the D… conversation
// Slack put it in comes back with its ts for the rewrites.
func (c *slackAPIClient) postConnectPromptDM(ctx context.Context, user, origin, text, link, connectValue string) (channel, ts string, err error) {
	blocks := append([]any{contextBlock(origin)}, connectPromptBlocks(text, signInButtonLabel, link, connectValue, true, c.authorizeAll)...)
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
func (c *slackAPIClient) updateConnectPrompt(ctx context.Context, channel, ts, origin, text, link, connectValue string) error {
	blocks := connectPromptBlocks(text, signInButtonLabel, link, connectValue, true, c.authorizeAll)
	if origin != "" {
		blocks = append([]any{contextBlock(origin)}, blocks...)
	}
	return c.chatUpdate(ctx, channel, ts, text, blocks)
}
