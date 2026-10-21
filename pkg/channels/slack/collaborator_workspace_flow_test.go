package slack_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// collaboratorFlow is a channel thread of U001 whose Session works in the
// workspace U001 chose (or none for ""), with the first turn run and U002
// waiting on U001's consent.
type collaboratorFlow struct {
	fake    *fakeSlackAPI
	fakeURL string
	wm      *fakeWorkspaceManager
	kagent  *workspaceKagent
	facade  *channels.Facade
	log     *lockedLog
	a       *slackadapter.Adapter
	srv     *httptest.Server
}

func newCollaboratorFlow(t *testing.T, workspace string) *collaboratorFlow {
	t.Helper()
	f := &collaboratorFlow{fake: newFakeSlackAPI(), wm: newFakeWorkspaceManager(t), kagent: newWorkspaceKagent(), log: &lockedLog{}}
	f.fakeURL = f.fake.server(t).URL
	routes := memory.New()
	t.Cleanup(func() { _ = routes.Close() })
	f.facade = &channels.Facade{Agent: f.kagent, Routes: routes, ThreadTTL: channels.DefaultThreadTTL}
	f.a, f.srv = newEventsAdapter(t, f.facade, f.fakeURL, withWorkspaces(f.wm), withInfoLog(f.log))

	openConversationIn(t, f.facade, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, f.srv, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(f.fake)) == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, f.a, "100.000")
	sendWorkspaceClick(t, f.srv, "U001", "100.000", workspace)
	f.waitTurns(t, 1)

	sendEvent(t, f.srv, mention("U002", "<@UBOT> and on staging?", "101.000", "100.000"))
	require.Eventually(t, func() bool {
		return len(f.fake.pathCalls("chat.postEphemeral")) > 0
	}, flowWait, 20*time.Millisecond, "the initiator is asked to let U002 in")
	return f
}

func (f *collaboratorFlow) waitTurns(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { _, _, turns := f.kagent.state(); return len(turns) == n }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, f.a, "100.000")
}

func (f *collaboratorFlow) allow(t *testing.T) {
	t.Helper()
	sendAccessInteraction(t, f.srv, "U001", accessAllowAction, "100.000", "U002", f.fakeURL+"/response")
	require.Eventually(t, func() bool { return len(f.fake.pathCalls("response")) == 1 }, flowWait, 20*time.Millisecond)
}

func (f *collaboratorFlow) ephemerals(user string) []recordedCall {
	var out []recordedCall
	for _, c := range f.fake.pathCalls("chat.postEphemeral") {
		if c.params["user"] == user {
			out = append(out, c)
		}
	}
	return out
}

// waitHeld waits until the thread's row holds text for a collaborator's connect.
func (f *collaboratorFlow) waitHeld(t *testing.T, text string) {
	t.Helper()
	require.Eventually(t, func() bool {
		held := string(f.row(t).Held)
		return strings.Contains(held, "workspace_connect") && strings.Contains(held, text)
	}, flowWait, 20*time.Millisecond, "the thread's row holds %q", text)
}

func (f *collaboratorFlow) row(t *testing.T) store.Entry {
	t.Helper()
	row, ok, err := f.facade.ThreadRecord(t.Context(), "slack", "C1", "100.000")
	require.NoError(t, err)
	require.True(t, ok)
	return row
}

// A collaborator who can read the thread's workspace runs their turns on the
// initiator's Session: their first turn asks the workspace-manager as them,
// the pass is remembered by name, and their next turn asks nothing.
func TestCollaboratorWorkspace_GrantedIsAskedOnceAndRuns(t *testing.T) {
	f := newCollaboratorFlow(t, "platform")
	require.Equal(t, 1, f.wm.calls("authorize_workspace"), "the initiator's choice")

	f.allow(t)
	require.Contains(t, allText(f.fake.pathCalls("response")),
		"<@U002> allowed. This thread works in workspace *platform*: the agent acts on their messages once the workspace-manager confirms they can read its repositories.")
	f.waitTurns(t, 2)
	created, _, turns := f.kagent.state()
	require.Equal(t, 1, created, "the initiator's Session, shared")
	require.Contains(t, turns[1], "and on staging?")
	require.Equal(t, 2, f.wm.calls("authorize_workspace"), "asked as the collaborator")
	require.Equal(t, "Bearer tok2", f.wm.Calls()[len(f.wm.Calls())-1].Bearer)
	row := f.row(t)
	require.Equal(t, []string{"U002"}, row.WorkspaceReaders)
	require.NotContains(t, string(row.Held)+row.Initiator, "grant-of", "no grant is kept")

	sendEvent(t, f.srv, mention("U002", "<@UBOT> and on dev?", "102.000", "100.000"))
	f.waitTurns(t, 3)
	require.Equal(t, 2, f.wm.calls("authorize_workspace"), "remembered: not asked again")

	sendEvent(t, f.srv, mention("U001", "<@UBOT> thanks", "103.000", "100.000"))
	f.waitTurns(t, 4)
	require.Equal(t, 2, f.wm.calls("authorize_workspace"), "the initiator is not asked")
}

// A collaborator who cannot read the thread's workspace is told so privately,
// with the repositories named, and no turn of theirs reaches the agent. They
// are asked again on their next turn.
func TestCollaboratorWorkspace_RefusedPrivatelyAndNoTurnRuns(t *testing.T) {
	f := newCollaboratorFlow(t, "platform")
	f.wm.blindTo("tok2")

	f.allow(t)
	require.Eventually(t, func() bool { return len(f.ephemerals("U002")) == 2 }, flowWait, 20*time.Millisecond)
	refusal := allText(f.ephemerals("U002")[1:])
	require.Contains(t, refusal, "This thread works in workspace *platform*, which holds repositories you cannot read: `giantswarm/vault`, `giantswarm/keys`.")
	require.NotContains(t, allBlockText(f.fake.pathCalls("chat.postMessage")), "giantswarm/vault", "the channel learns no repository")
	require.Contains(t, f.log.String(), "record=workspace_collaborator_refused")
	require.NotContains(t, f.log.String(), "giantswarm/vault", "no repository name above debug")

	sendEvent(t, f.srv, mention("U002", "<@UBOT> please?", "102.000", "100.000"))
	require.Eventually(t, func() bool { return len(f.ephemerals("U002")) == 3 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, f.a, "100.000")
	_, _, turns := f.kagent.state()
	require.Len(t, turns, 1, "only the initiator's turn reached the agent")
	require.Equal(t, 3, f.wm.calls("authorize_workspace"), "a refusal is not remembered")
	require.Empty(t, f.row(t).WorkspaceReaders)
}

// A collaborator who has not connected the workspace's provider gets the
// connect prompt and no turn; their message is held, and once connected their
// next mention runs it first, then itself, without a resend.
func TestCollaboratorWorkspace_ConnectPromptThenTheHeldMessageRuns(t *testing.T) {
	f := newCollaboratorFlow(t, "platform")
	f.wm.disconnect("tok2")

	f.allow(t)
	require.Eventually(t, func() bool { return len(f.ephemerals("U002")) == 2 }, flowWait, 20*time.Millisecond)
	prompt := allBlockText(f.ephemerals("U002")[1:])
	require.Contains(t, prompt, "Connect *gitlab* first, so the agent can work in workspace *platform* as you. Your message is kept for a day: once connected, mention me here and it runs.")
	require.Contains(t, prompt, `"connector_connect"`)
	f.waitHeld(t, "and on staging?")
	_, _, turns := f.kagent.state()
	require.Len(t, turns, 1)

	f.wm.connect("tok2")
	sendEvent(t, f.srv, mention("U002", "<@UBOT> connected now", "102.000", "100.000"))
	f.waitTurns(t, 3)
	_, _, turns = f.kagent.state()
	require.Contains(t, turns[1], "and on staging?", "the held message runs first")
	require.Contains(t, turns[2], "connected now")
	require.Equal(t, []string{"U002"}, f.row(t).WorkspaceReaders)
	require.Contains(t, f.log.String(), "record=workspace_collaborator_held_run")
	require.Eventually(t, func() bool { return !strings.Contains(string(f.row(t).Held), "workspace_connect") }, flowWait, 20*time.Millisecond, "nothing held any more")
}

// A collaborator who never connects has no turn run and loses no message
// silently: each message of theirs is held behind a connect prompt that says
// so, past the cap they are told the oldest went, and an answer that drops
// what is held says so.
func TestCollaboratorWorkspace_NeverConnectedRunsNothingAndLosesNothingSilently(t *testing.T) {
	f := newCollaboratorFlow(t, "platform")
	f.wm.disconnect("tok2")

	f.allow(t)
	require.Eventually(t, func() bool { return len(f.ephemerals("U002")) == 2 }, flowWait, 20*time.Millisecond)
	for i := range 5 {
		sendEvent(t, f.srv, mention("U002", fmt.Sprintf("<@UBOT> still there %d?", i), fmt.Sprintf("10%d.500", 2+i), "100.000"))
		f.waitHeld(t, fmt.Sprintf("still there %d?", i))
		waitThreadIdle(t, f.a, "100.000")
	}
	require.Eventually(t, func() bool { return len(f.ephemerals("U002")) == 8 }, flowWait, 20*time.Millisecond, "a prompt per message and one drop notice")
	require.Contains(t, allText(f.ephemerals("U002")), "Only your last 5 messages are held here; earlier ones were dropped.")
	f.waitHeld(t, "still there 4?")
	require.NotContains(t, string(f.row(t).Held), "and on staging?", "the oldest went past the cap")
	_, _, turns := f.kagent.state()
	require.Len(t, turns, 1, "only the initiator's turn reached the agent")

	f.wm.blindTo("tok2")
	sendEvent(t, f.srv, mention("U002", "<@UBOT> now?", "108.000", "100.000"))
	require.Eventually(t, func() bool { return len(f.ephemerals("U002")) == 9 }, flowWait, 20*time.Millisecond)
	require.Contains(t, allText(f.ephemerals("U002")[8:]), "The messages held while you were asked to connect were not passed on either.")
	require.Eventually(t, func() bool { return !strings.Contains(string(f.row(t).Held), "workspace_connect") }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, f.a, "100.000")
	_, _, turns = f.kagent.state()
	require.Len(t, turns, 1)
}

// A collaborator's held message is kept in the thread's row: after a restart
// their mention once connected still runs it first.
func TestCollaboratorWorkspace_HeldMessageSurvivesARestart(t *testing.T) {
	f := newCollaboratorFlow(t, "platform")
	f.wm.disconnect("tok2")
	f.allow(t)
	f.waitHeld(t, "and on staging?")
	waitThreadIdle(t, f.a, "100.000")
	require.NoError(t, f.a.Stop(context.Background()))

	a2, srv2 := newEventsAdapter(t, f.facade, f.fakeURL, withWorkspaces(f.wm), withInfoLog(f.log))
	a2.RecoverTurns()
	a2.WaitHeldRestored()
	f.wm.connect("tok2")
	sendEvent(t, srv2, mention("U002", "<@UBOT> connected now", "102.000", "100.000"))
	require.Eventually(t, func() bool { _, _, turns := f.kagent.state(); return len(turns) == 3 }, flowWait, 20*time.Millisecond)
	_, _, turns := f.kagent.state()
	require.Contains(t, turns[1], "and on staging?")
	require.Contains(t, turns[2], "connected now")
}

// A collaborator in a thread without a workspace is not asked.
func TestCollaboratorWorkspace_NoWorkspaceIsNotAsked(t *testing.T) {
	f := newCollaboratorFlow(t, "")
	f.wm.blindTo("tok2")

	f.allow(t)
	require.NotContains(t, allText(f.fake.pathCalls("response")), "workspace", "nothing to confirm")
	f.waitTurns(t, 2)
	require.Zero(t, f.wm.calls("authorize_workspace"))
}
