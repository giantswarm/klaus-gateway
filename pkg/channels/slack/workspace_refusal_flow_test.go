package slack_test

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// lockedLog is an info-level log the adapter writes to while the test reads.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func withInfoLog(l *lockedLog) func(*slackadapter.Adapter) {
	return func(a *slackadapter.Adapter) {
		a.Logger = slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
}

// refusalFlow is a channel thread of U001 waiting on its workspace prompt,
// with the agent-manager's Session starts recorded by kagent.
type refusalFlow struct {
	fake   *fakeSlackAPI
	wm     *fakeWorkspaceManager
	kagent *workspaceKagent
	facade *channels.Facade
	log    *lockedLog
	a      *slackadapter.Adapter
	srv    *httptest.Server
}

func newRefusalFlow(t *testing.T, opts ...func(*slackadapter.Adapter)) *refusalFlow {
	t.Helper()
	f := &refusalFlow{fake: newFakeSlackAPI(), wm: newFakeWorkspaceManager(t), kagent: newWorkspaceKagent(), log: &lockedLog{}}
	api := f.fake.server(t)
	routes := memory.New()
	t.Cleanup(func() { _ = routes.Close() })
	f.facade = &channels.Facade{Agent: f.kagent, Routes: routes, ThreadTTL: channels.DefaultThreadTTL}
	opts = append([]func(*slackadapter.Adapter){withWorkspaces(f.wm), withInfoLog(f.log)}, opts...)
	a, srv := newEventsAdapter(t, f.facade, api.URL, opts...)
	f.a, f.srv = a, srv

	sendEvent(t, srv, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(f.fake)) == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "100.000")
	return f
}

// ephemerals are the private messages to user.
func (f *refusalFlow) ephemerals(user string) []recordedCall {
	var out []recordedCall
	for _, c := range f.fake.pathCalls("chat.postEphemeral") {
		if c.params["user"] == user {
			out = append(out, c)
		}
	}
	return out
}

func (f *refusalFlow) heldMessage(t *testing.T) string {
	t.Helper()
	row, ok, err := f.facade.ThreadRecord(t.Context(), "slack", "C1", "100.000")
	require.NoError(t, err)
	require.True(t, ok)
	return string(row.Held)
}

// A refusal names the workspace and every repository the initiator cannot
// read, to them alone, with No workspace offered again. The agent-manager is
// asked for no Session, nothing is bound, the held message stays, and the
// next pick starts the Session with it.
func TestWorkspaceRefusal_NamesEveryRepositoryPrivatelyAndStartsNoSession(t *testing.T) {
	f := newRefusalFlow(t)
	sendWorkspaceClick(t, f.srv, "U001", "100.000", "secret")
	require.Eventually(t, func() bool { return len(f.ephemerals("U001")) == 1 }, flowWait, 20*time.Millisecond)

	answer := f.ephemerals("U001")[0]
	require.Equal(t, "100.000", answer.params["thread_ts"], "in the thread")
	require.Contains(t, allText([]recordedCall{answer}), "You cannot start in workspace *secret*: you cannot read `giantswarm/vault`, `giantswarm/keys`. Nothing was started; pick another workspace or *No workspace*.")
	require.Contains(t, allBlockText([]recordedCall{answer}), `"workspace_none"`, "the choice is offered again")
	require.NotContains(t, allBlockText(f.fake.pathCalls("chat.postMessage")), "giantswarm/vault", "the channel learns no repository")

	created, grants, _ := f.kagent.state()
	require.Zero(t, created, "no start_session")
	require.Empty(t, grants)
	row, ok, err := f.facade.ThreadRecord(t.Context(), "slack", "C1", "100.000")
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, row.AgentInstanceID, "no Session bound")
	require.Nil(t, row.Workspace, "no workspace recorded")
	require.Contains(t, f.heldMessage(t), "what failed on prod?", "the held message survives")

	logs := f.log.String()
	require.Contains(t, logs, "record=workspace_refused")
	require.NotContains(t, logs, "giantswarm/vault", "no repository name above debug")

	sendWorkspaceClick(t, f.srv, "U001", "100.000", "platform")
	require.Eventually(t, func() bool { _, _, turns := f.kagent.state(); return len(turns) == 1 }, flowWait, 20*time.Millisecond)
	created, grants, turns := f.kagent.state()
	require.Equal(t, 1, created)
	require.Equal(t, []string{"grant-of-platform-for-tok1"}, grants)
	require.Contains(t, turns[0], "what failed on prod?")
}

// A provider the initiator has not connected shows the connect prompt with
// the workspace-manager's link and starts nothing. Once connected, the next
// click starts the Session with the held message.
func TestWorkspaceRefusal_ConnectPromptThenTheNextClickStarts(t *testing.T) {
	f := newRefusalFlow(t)
	srv := f.srv
	sendWorkspaceClick(t, srv, "U001", "100.000", "gitlab-ws")
	require.Eventually(t, func() bool { return len(f.ephemerals("U001")) == 1 }, flowWait, 20*time.Millisecond)

	prompt := allBlockText(f.ephemerals("U001"))
	require.Contains(t, prompt, "Connect *gitlab* first, so the agent can work in workspace *gitlab-ws* as you.")
	require.Contains(t, prompt, `"https://wm.example/connect/gitlab"`, "the workspace-manager's connect link")
	require.Contains(t, prompt, `"connector_connect"`, "the existing connect prompt")
	created, _, _ := f.kagent.state()
	require.Zero(t, created, "no start_session")
	require.Contains(t, f.heldMessage(t), "what failed on prod?", "the held message survives")
	require.Contains(t, f.log.String(), "record=workspace_connect_required")

	f.wm.connect("tok1")
	sendWorkspaceClick(t, srv, "U001", "100.000", "gitlab-ws")
	require.Eventually(t, func() bool { _, _, turns := f.kagent.state(); return len(turns) == 1 }, flowWait, 20*time.Millisecond)
	created, grants, turns := f.kagent.state()
	require.Equal(t, 1, created)
	require.Equal(t, []string{"grant-of-gitlab-ws-for-tok1"}, grants)
	require.Contains(t, turns[0], "what failed on prod?", "the held message is the first turn")
}

// Where muster's Connect prompts offer Authorize all, a provider's Connect
// prompt still offers the provider's sign-in alone: the modal grants muster
// servers, not a provider account.
func TestWorkspaceRefusal_ProviderConnectPromptOffersNoAuthorizeAll(t *testing.T) {
	f := newRefusalFlow(t, withGrants(&recordingTools{}), func(a *slackadapter.Adapter) {
		a.ConnectorPrompts = true
		a.PublicBaseURL = "https://gw.example"
	})
	sendWorkspaceClick(t, f.srv, "U001", "100.000", "gitlab-ws")
	require.Eventually(t, func() bool { return len(f.ephemerals("U001")) == 1 }, flowWait, 20*time.Millisecond)

	blocks, _ := f.ephemerals("U001")[0].params["blocks"].([]any)
	require.Len(t, blocks, 2, "the note over one actions block")
	actions, _ := blocks[1].(map[string]any)
	elements, _ := actions["elements"].([]any)
	require.Len(t, elements, 1, "the provider's Connect button alone")
	button, _ := elements[0].(map[string]any)
	require.Equal(t, "connector_connect", button["action_id"])
	require.Equal(t, "https://wm.example/connect/gitlab", button["url"])
	require.Equal(t, "gitlab", button["value"])
	label, _ := button["text"].(map[string]any)
	require.Equal(t, "Connect gitlab", label["text"])
	require.NotContains(t, allBlockText(f.ephemerals("U001")), "authorize_all_open")
}

// Once connected, the initiator's next mention retries the workspace that
// waited on the provider: the held message starts the Session, the mention
// follows it. Another person's message retries nothing.
func TestWorkspaceRefusal_ConnectedThenTheNextMentionStarts(t *testing.T) {
	f := newRefusalFlow(t)
	srv := f.srv
	sendWorkspaceClick(t, srv, "U001", "100.000", "gitlab-ws")
	require.Eventually(t, func() bool { return len(f.ephemerals("U001")) == 1 }, flowWait, 20*time.Millisecond)
	require.Eventually(t, func() bool { return strings.Contains(f.heldMessage(t), `"retry":"gitlab-ws"`) }, flowWait, 20*time.Millisecond)

	f.wm.connect("tok1")
	sendEvent(t, srv, mention("U001", "<@UBOT> connected now", "101.000", "100.000"))
	require.Eventually(t, func() bool { _, _, turns := f.kagent.state(); return len(turns) == 2 }, flowWait, 20*time.Millisecond)
	created, grants, turns := f.kagent.state()
	require.Equal(t, 1, created)
	require.Equal(t, []string{"grant-of-gitlab-ws-for-tok1"}, grants)
	require.Contains(t, turns[0], "what failed on prod?")
	require.Contains(t, turns[1], "connected now")
	require.Contains(t, allText(f.fake.pathCalls("chat.update")), "Working in workspace *gitlab-ws*, chosen by <@U001>.")
}

// A workspace that has not synced yet is answered as not synced, privately,
// with the choice offered again, and starts nothing.
func TestWorkspaceRefusal_NotSyncedStartsNothing(t *testing.T) {
	f := newRefusalFlow(t)
	sendWorkspaceClick(t, f.srv, "U001", "100.000", "fresh")
	require.Eventually(t, func() bool { return len(f.ephemerals("U001")) == 1 }, flowWait, 20*time.Millisecond)

	answer := f.ephemerals("U001")[0]
	require.Contains(t, allText([]recordedCall{answer}), "Cannot start in workspace *fresh* yet: workspace fresh has not synced yet. Nothing was started; pick another workspace or *No workspace*.")
	require.Contains(t, allBlockText([]recordedCall{answer}), `"workspace_none"`)
	created, _, _ := f.kagent.state()
	require.Zero(t, created, "no start_session")
	require.Contains(t, f.heldMessage(t), "what failed on prod?")
	require.NotContains(t, f.heldMessage(t), `"retry"`, "a mention retries nothing")
	require.Contains(t, f.log.String(), "record=workspace_unavailable")
}
