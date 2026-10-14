package slack

import (
	"context"
	"fmt"
	"time"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// promptSessionConnect answers a Session start muster refused with its
// sign-in challenge for a server: the person the start ran as is signed in,
// but that server waits for their one-time consent.
//
// msg is the message as admitted (the raw Slack user as Subject), the one a
// replay admits again. When the start ran as the sender, the Connect prompt
// for the server goes to them alone with the message held behind it: with a
// public base URL the link lands back on the gateway and the landing runs the
// held message as the thread's first turn, without one the prompt says to
// send the message again once connected. The hold is the process's own, like
// every connector completion: after a restart the landing says the link is
// gone and the person sends the message again.
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
	promptURL, connectValue, tail := challenge.LoginURL, server, sessionConnectResendTail
	if base := a.PublicBaseURL; base != "" {
		held := heldMessageOf(msg, slackChannel, time.Now())
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: slackUser, server: server, channel: slackChannel, threadTS: msg.ThreadID, held: &held})
		if decorated, err := decorateConnectorLoginURL(challenge.LoginURL, base, stateID); err != nil {
			a.Logger.Warn("slack: session connect login URL decoration failed, posting plain link", "server", server, "error", err)
		} else {
			promptURL, connectValue, tail = decorated, stateID, sessionConnectResumeTail
		}
	}
	text := fmt.Sprintf(sessionConnectNote, escapeMrkdwn(server)) + tail
	if err := client.postConnectPrompt(ctx, slackChannel, msg.ThreadID, slackUser, text, server, promptURL, connectValue, false); err != nil {
		a.Logger.Warn("slack: post session connect prompt failed", "thread", msg.ThreadID, "server", server, "error", err)
	}
}
