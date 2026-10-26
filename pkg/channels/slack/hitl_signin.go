package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// signInDecline is the "Not now" button of a sign-in prompt; its value is the
// hitlValue of the paused task.
const signInDecline = "sign_in_decline"

// signInDeclineText is the message a "Not now" click resumes the paused task
// with; decisionFromText reads it as a decline.
const signInDeclineText = "Not now"

// postMCPSignInPrompt renders a task paused on an MCP server's sign-in link. The
// link is minted for slackUser, the person whose turn asked for it, so the
// Connect prompt is theirs alone; the thread gets a note naming whom the turn
// waits on, which also anchors the ephemeral there (klaus-gateway#156). The
// completed sign-in resumes the task with accept, "Not now" with decline, and
// a typed reply decides as decisionFromText reads it.
func (a *Adapter) postMCPSignInPrompt(ctx context.Context, client *slackAPIClient, slackChannel, threadID, slackUser string, pd *channels.OutboundDelta) error {
	server := pd.Prompt.SignIn.Server
	if server == "" {
		server = "the requested tools"
	}
	if ts, err := client.postMessage(ctx, slackChannel, fmt.Sprintf(signInWaitingNote, slackUser, escapeMrkdwn(server)), threadID); err != nil {
		a.Logger.Warn("slack: post sign-in waiting note failed", "thread", threadID, "error", err)
	} else {
		a.notePromptTS(threadID, pd.TaskID, ts)
	}

	promptURL, connectValue, resumes := pd.Prompt.SignIn.URL, server, false
	if base := a.PublicBaseURL; base != "" {
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: slackUser, server: server, channel: slackChannel, threadTS: threadID, signInTask: pd.TaskID})
		if decorated, err := decorateConnectorLoginURL(promptURL, base, stateID); err != nil {
			a.Logger.Warn("slack: sign-in URL decoration failed, posting plain link", "server", server, "error", err)
		} else {
			promptURL, connectValue, resumes = decorated, stateID, true
		}
	}
	text := fmt.Sprintf(signInPromptManualText, escapeMrkdwn(server))
	if resumes {
		text = fmt.Sprintf(signInPromptText, escapeMrkdwn(server))
	}
	blocks := connectPromptBlocks(text, server, promptURL, connectValue, connectValue, false, client.authorizeAll)
	actions, _ := blocks[len(blocks)-1].(map[string]any)
	actions[bkElements] = append(actions[bkElements].([]any), map[string]any{
		bkType:     bkButton,
		bkText:     map[string]any{bkType: bkPlainText, bkText: "Not now"},
		bkActionID: signInDecline,
		bkValue:    encodeHitlValue(threadID, pd.TaskID),
	})
	return client.postConnectEphemeral(ctx, slackChannel, threadID, slackUser, text, blocks)
}

// signInDecided is the line a sign-in prompt's thread note is rewritten to
// once the paused task resumes.
func signInDecided(prompt *channels.HitlPrompt, decision *channels.HitlDecision, slackUser string) string {
	server := escapeMrkdwn(prompt.SignIn.Server)
	if decision.Type == channels.DecisionApprove {
		return fmt.Sprintf(signInAcceptedLine, slackUser, server)
	}
	return fmt.Sprintf(signInDeclinedLine, slackUser, server)
}

// signInDecision reads a typed reply to a sign-in prompt: a refusal declines,
// anything else says the person signed in. A reply that comes before the
// sign-in did resumes the task all the same; the server then asks again.
func signInDecision(text string) *channels.HitlDecision {
	if isDenyWord(text) || strings.EqualFold(strings.TrimSpace(text), signInDeclineText) {
		return &channels.HitlDecision{Type: channels.DecisionReject}
	}
	return &channels.HitlDecision{Type: channels.DecisionApprove}
}

// pendingSignIn returns the thread's paused task when it is still the sign-in
// taskID named, nil otherwise: the task resumed meanwhile, or the thread
// paused again on another prompt.
func (a *Adapter) pendingSignIn(threadID, taskID string) *pendingTask {
	task := a.peekPendingTask(threadID)
	if task == nil || task.TaskID != taskID || !task.Prompt.IsSignIn() {
		return nil
	}
	return task
}

// handleSignInDecline resumes a task paused on a sign-in with decline, for
// the "Not now" click of its prompt.
func (a *Adapter) handleSignInDecline(ctx context.Context, slackUser, value, responseURL string) {
	v := decodeHitlValue(value)
	task := a.pendingSignIn(v.Thread, v.Task)
	if task == nil {
		if err := respondURL(ctx, responseURL, v.Thread, promptAnsweredNotice); err != nil {
			a.Logger.Warn("slack: update sign-in prompt (answered) failed", "thread", v.Thread, "error", err)
		}
		return
	}
	if err := respondURL(ctx, responseURL, v.Thread, fmt.Sprintf(signInDismissedNotice, escapeMrkdwn(task.Prompt.SignIn.Server))); err != nil {
		a.Logger.Warn("slack: update sign-in prompt (declined) failed", "thread", v.Thread, "error", err)
	}
	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: task.ChannelID,
		ThreadID:  v.Thread,
		Text:      signInDeclineText,
		Subject:   slackUser,
	}
	a.background(func(ctx context.Context) {
		if err := a.replayDispatch(ctx, msg, task.Channel); err != nil && !errors.Is(err, context.Canceled) {
			a.Logger.Log(ctx, failureLogLevel(err), "slack: sign-in decline resume failed", "user", slackUser, "thread", v.Thread, "failure_class", channels.ClassifyFailure(err), "error", err)
			a.postReplayFailureNote(ctx, task.Channel, v.Thread, err)
		}
	})
}
