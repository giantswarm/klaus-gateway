package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// An agent's default workspace: a plain mention starts in it as the person,
// without the prompt; the picker presets it after the person's last choice.

// defaultRoster lists sre-agent with the default workspace def ("" for none)
// beside the picker's other agents.
func defaultRoster(def string) *fakeRoster {
	roster := pickerRoster()
	for i := range roster.agents {
		if roster.agents[i].Name == "sre-agent" {
			roster.agents[i].DefaultWorkspace = def
		}
	}
	return roster
}

// A plain mention of an agent with a default starts its Session in it with
// the initiator's grant, lists and asks nothing, and the thread names the
// workspace and the slash command that picks another. The message sent while
// the start runs follows on the same Session.
func TestDefaultWorkspace_MentionStartsInTheDefaultWithoutAPrompt(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	routes := memory.New()
	t.Cleanup(func() { _ = routes.Close() })
	kagent := newWorkspaceKagent()
	facade := &channels.Facade{Agent: kagent, Routes: routes, ThreadTTL: channels.DefaultThreadTTL}
	a, srv := newEventsAdapter(t, facade, api.URL, withWorkspaces(wm), withSelection(defaultRoster("platform"), pickerCards()))

	openConversationIn(t, facade, "C1", "100.000", "U001", "kagent/sre-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	sendEvent(t, srv, mention("U001", "<@UBOT> and on staging?", "101.000", "100.000"))
	require.Eventually(t, func() bool { _, _, turns := kagent.state(); return len(turns) == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "100.000")

	created, grants, turns := kagent.state()
	require.Equal(t, 1, created, "one Session")
	require.Equal(t, []string{"grant-of-platform-for-tok1"}, grants, "in the default, with the initiator's grant")
	require.Contains(t, turns[0], "what failed on prod?")
	require.Contains(t, turns[1], "and on staging?")
	require.Empty(t, workspacePrompts(fake), "nobody is asked")
	require.Zero(t, wm.calls("list_workspaces"), "nothing is listed")
	require.Equal(t, []string{"Bearer tok1"}, authorizeBearers(wm), "authorized as the initiator, once")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")),
		"Working in workspace *platform*, this agent's default. To work in another, start with `/swarmgeist` and pick it.")

	row, ok, err := facade.ThreadRecord(t.Context(), "slack", "C1", "100.000")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, &store.WorkspaceChoice{Name: "platform"}, row.Workspace)
	require.Empty(t, row.Held)
}

// A plain mention of an agent without a default asks, as before.
func TestDefaultWorkspace_AgentWithoutADefaultAsks(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(defaultRoster(""), pickerCards()))

	gw.openConversation(t, "C1", "100.000", "U001", "kagent/sre-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> hello", "100.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 1 }, flowWait, 20*time.Millisecond)
	require.Empty(t, captured())
	require.Zero(t, wm.calls("authorize_workspace"))
}

// A person who cannot read the default's repositories gets no Session: the
// answer, to them alone, names the repositories and the slash command that
// starts in another workspace or none, never that the agent is closed to
// them. Nothing stays held.
func TestDefaultWorkspace_RefusalNamesTheRepositoriesAndTheWayOut(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	wm.blindTo("tok1")
	rec := slackadapter.NewMemoryRecorder()
	gw, captured := captureDispatch(rec)
	a, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(defaultRoster("platform"), pickerCards()))

	gw.openConversation(t, "C1", "100.000", "U001", "kagent/sre-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "100.000")

	answer := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, answer, "You cannot start in workspace *platform*, this agent's default: you cannot read `giantswarm/vault`, `giantswarm/keys`.")
	require.Contains(t, answer, "Nothing was started; pick another workspace or *No workspace* in `/swarmgeist`.")
	require.Contains(t, answer, "what failed on prod?", "the message is handed back")
	require.Equal(t, "U001", fake.pathCalls("chat.postEphemeral")[0].params["user"])
	require.Empty(t, captured(), "no Session")
	require.Empty(t, workspacePrompts(fake))
	require.False(t, heldIn(t, rec, "C1", "100.000"), "nothing is held")

	sendEvent(t, srv, mention("U001", "<@UBOT> again?", "101.000", "100.000"))
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == 2 }, flowWait, 20*time.Millisecond,
		"the next mention tries the default again")
	require.Empty(t, captured())
}

// A default on a provider the person has not connected gets the Connect
// prompt and keeps the message; once connected, their next mention starts
// the Session in the default with both messages.
func TestDefaultWorkspace_ConnectThenMentionStartsInTheDefault(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	routes := memory.New()
	t.Cleanup(func() { _ = routes.Close() })
	kagent := newWorkspaceKagent()
	facade := &channels.Facade{Agent: kagent, Routes: routes, ThreadTTL: channels.DefaultThreadTTL}
	a, srv := newEventsAdapter(t, facade, api.URL, withWorkspaces(wm), withSelection(defaultRoster("gitlab-ws"), pickerCards()))

	openConversationIn(t, facade, "C1", "100.000", "U001", "kagent/sre-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "Connect *gitlab* first, so the agent can work in workspace *gitlab-ws* as you.")
	}, flowWait, 20*time.Millisecond)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "Your message is kept: once connected, mention me and it runs.")
	waitThreadIdle(t, a, "100.000")
	created, _, _ := kagent.state()
	require.Zero(t, created)

	wm.connect("tok1")
	sendEvent(t, srv, mention("U001", "<@UBOT> connected", "101.000", "100.000"))
	require.Eventually(t, func() bool { _, _, turns := kagent.state(); return len(turns) == 2 }, flowWait, 20*time.Millisecond)
	created, grants, turns := kagent.state()
	require.Equal(t, 1, created)
	require.Equal(t, []string{"grant-of-gitlab-ws-for-tok1"}, grants)
	require.Contains(t, turns[0], "what failed on prod?")
	require.Contains(t, turns[1], "connected")
	require.Empty(t, workspacePrompts(fake))
}

// The picker presets the person's last choice with the agent, else the
// agent's default, else No workspace; picking another agent follows.
func TestDefaultWorkspace_PickerPresetsLastChoiceThenDefault(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	rec := slackadapter.NewMemoryRecorder()
	gw, _ := captureDispatch(rec)
	roster := defaultRoster("platform")
	roster.agents[0].DefaultWorkspace = "klaus-dev" // swarmgeist, the agent the picker opens on
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(roster, pickerCards()))

	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	view := openedView(t, fake)
	ws := workspaceBlock(view)
	require.Equal(t, "klaus-dev", initialValue(ws), "no last choice: the default")
	require.Equal(t, "The default of Swarmgeist.", hintText(ws))

	changePickerAgent(t, srv, view, "U001", "kagent/sre-agent")
	ws = workspaceBlock(updatedView(t, fake, 1))
	require.Equal(t, "platform", initialValue(ws), "the new agent's default")
	require.Equal(t, "The default of SRE Agent.", hintText(ws))

	recordLastWorkspace(t, rec, "U001", "kagent/swarmgeist", &store.WorkspaceChoice{None: true})
	recordLastWorkspace(t, rec, "U001", "kagent/sre-agent", &store.WorkspaceChoice{Name: "secret"})
	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	fake.waitForPath(t, "views.open", 2)
	view = fake.pathCalls("views.open")[1].params["view"].(map[string]any)
	ws = workspaceBlock(view)
	require.Equal(t, "*none*", initialValue(ws), "the last choice, No workspace, comes first")
	require.Equal(t, "Last used with Swarmgeist.", hintText(ws))

	changePickerAgent(t, srv, view, "U001", "kagent/sre-agent")
	ws = workspaceBlock(updatedView(t, fake, 2))
	require.Equal(t, "secret", initialValue(ws), "the last choice before the default")
	require.Equal(t, "Last used with SRE Agent.", hintText(ws))
}

// No workspace in the picker, on an agent with a default, starts without
// one: no grant, nothing authorized, and agent-manager is told noWorkspace.
func TestDefaultWorkspace_PickerNoWorkspaceStartsWithoutOne(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(defaultRoster("platform"), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	submitPicker(t, srv, openedView(t, fake), "U001", "kagent/sre-agent", "*none*", "hello")
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
	require.Equal(t, &store.WorkspaceChoice{None: true}, captured()[0].Workspace)
	require.True(t, captured()[0].WorkspaceGrant.IsZero())
	require.Zero(t, wm.calls("authorize_workspace"), "the default is not taken")
	require.Empty(t, workspacePrompts(fake))
}
