package slack

import (
	"context"
	"fmt"
	"time"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// promptSessionConnect answers a Session start muster refused with its
// sign-in challenge for a server: the person is signed in, but that server
// waits for their one-time consent. The Connect prompt for it goes to the
// person alone, with the message held behind it. With a public base URL the
// link lands back on the gateway and the landing runs the held message as the
// thread's first turn; without one the prompt says to send the message again
// once connected. The hold is the process's own, like every connector
// completion: after a restart the landing says the link is gone and the
// person sends the message again.
func (a *Adapter) promptSessionConnect(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string, challenge *pkga2a.ConnectRequiredError) {
	server := challenge.Server
	a.Logger.Info("slack: session start waits for the person's consent to a server",
		"record", "session_connect_required",
		"server", server,
		"agent", msg.AgentRef,
		"slack_user", slackUser,
		"channel_id", msg.ChannelID,
		"thread_id", msg.ThreadID,
	)
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
	if err := a.apiClient().postConnectPrompt(ctx, slackChannel, msg.ThreadID, slackUser, text, server, promptURL, connectValue, false); err != nil {
		a.Logger.Warn("slack: post session connect prompt failed", "thread", msg.ThreadID, "server", server, "error", err)
	}
}
