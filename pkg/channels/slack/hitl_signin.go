package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// signInDecline is the "Not now" button of a sign-in prompt; its value is the
// hitlValue of the paused task.
const signInDecline = "sign_in_decline"

// signInDeclineText is the message a "Not now" click resumes the paused task
// with; decisionFromText reads it as a decline.
const signInDeclineText = "Not now"

// postMCPSignInPrompt renders a task paused on an MCP server's URL
// elicitation: a sign-in, a tool approval or any later one, all alike. The
// link is minted for slackUser, the person whose turn asked for it, so the
// prompt is theirs alone; the thread gets a note naming whom the turn waits
// on, which also anchors the ephemeral there (klaus-gateway#156). The
// server's own message is the heading, attributed to the server, and the
// whole link is shown with its host before the person opens it. The link
// button (Open, labelled with the host) opens it and decides nothing; Done,
// or muster's return with the elicitation's id, resumes the task with
// accept, "Not now" with decline, and a typed reply decides as
// decisionFromText reads it. A link that cannot be shown as written is not
// offered: the prompt says so, and only "Not now" or a reply answers it.
func (a *Adapter) postMCPSignInPrompt(ctx context.Context, client *slackAPIClient, slackChannel, threadID, slackUser string, pd *channels.OutboundDelta) error {
	signIn := pd.Prompt.SignIn
	server := signIn.Server
	if server == "" {
		server = "An MCP server"
	}
	if ts, err := client.postMessage(ctx, slackChannel, fmt.Sprintf(signInWaitingNote, slackUser, escapeMrkdwn(server)), threadID); err != nil {
		a.Logger.Warn("slack: post sign-in waiting note failed", "thread", threadID, "error", err)
	} else {
		a.notePromptTS(threadID, pd.TaskID, ts)
	}

	decline := map[string]any{
		bkType:     bkButton,
		bkText:     map[string]any{bkType: bkPlainText, bkText: "Not now"},
		bkActionID: signInDecline,
		bkValue:    encodeHitlValue(threadID, pd.TaskID),
	}
	lead := elicitationHeading(server, signIn.Message)
	if err := muster.ValidateSignInURL(signIn.URL); err != nil {
		a.Logger.Warn("slack: elicitation link not offered", "server", server, "thread", threadID, "error", err)
		text := lead + "\n" + signInLinkUnusableNotice
		blocks := []any{
			map[string]any{bkType: bkSection, bkText: verbatimMrkdwn(text)},
			map[string]any{bkType: bkActions, bkElements: []any{decline}},
		}
		return client.postConnectEphemeral(ctx, slackChannel, threadID, slackUser, text, blocks)
	}

	link := a.elicitationLink(signIn.URL, signIn.ElicitationID)
	stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: slackUser, channel: slackChannel, threadTS: threadID, signInTask: pd.TaskID, elicitationID: signIn.ElicitationID})
	text := lead + "\n" + fmt.Sprintf(signInAskedBy, escapeMrkdwnCapped(server, signInServerNameMaxRunes)) + "\n" + signInLinkText(link) + "\n" + signInPromptTail
	blocks := connectPromptBlocks(text, elicitationOpenButtonLabel(link), link, stateID, false, client.authorizeAll)
	actions, _ := blocks[len(blocks)-1].(map[string]any)
	elements := actions[bkElements].([]any)
	// Open, Done, Not now, then Authorize all when offered.
	actions[bkElements] = append([]any{elements[0], elicitationDoneButton(stateID), decline}, elements[1:]...)
	return client.postConnectEphemeral(ctx, slackChannel, threadID, slackUser, text, blocks)
}

// elicitationHeading is a URL elicitation prompt's heading: the server's own
// message in bold, on one line and capped, or, when it sent none, a line
// naming the server.
func elicitationHeading(server, message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return fmt.Sprintf(signInPromptLead, escapeMrkdwnCapped(server, signInServerNameMaxRunes))
	}
	return "*" + escapeMrkdwnCapped(message, signInMessageMaxRunes) + "*"
}

// signInDecided is the line a sign-in prompt's thread note is rewritten to
// once the paused task resumes.
func signInDecided(decision *channels.HitlDecision, slackUser string) string {
	if decision.Type == channels.DecisionApprove {
		return fmt.Sprintf(signInAcceptedLine, slackUser)
	}
	return fmt.Sprintf(signInDeclinedLine, slackUser)
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
	if err := respondURL(ctx, responseURL, v.Thread, signInDismissedNotice); err != nil {
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
