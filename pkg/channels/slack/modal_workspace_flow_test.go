package slack_test

import (
	"encoding/json"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/muster/mustertest"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// The agent picker's Workspace select: offered beside the agent when the
// person has workspaces, preset to the one they last started the agent with,
// following a change of agent, and carried by the submission into the
// thread's first turn, so the thread asks nothing.

var updateGolden = flag.Bool("update", false, "rewrite the golden files under testdata")

// workspaceBlock returns the picker view's Workspace input block, nil when it
// has none.
func workspaceBlock(view map[string]any) map[string]any {
	for _, b := range view["blocks"].([]any) {
		block := b.(map[string]any)
		if id, _ := block["block_id"].(string); strings.HasPrefix(id, "ask_agent_workspace.") {
			return block
		}
	}
	return nil
}

// optionTexts lists a select element's option labels, each group's label in
// brackets before its options.
func optionTexts(element map[string]any) []string {
	var texts []string
	add := func(options any) {
		for _, o := range options.([]any) {
			texts = append(texts, o.(map[string]any)["text"].(map[string]any)["text"].(string))
		}
	}
	if groups, ok := element["option_groups"].([]any); ok {
		for _, g := range groups {
			group := g.(map[string]any)
			texts = append(texts, "["+group["label"].(map[string]any)["text"].(string)+"]")
			add(group["options"])
		}
		return texts
	}
	add(element["options"])
	return texts
}

func initialValue(block map[string]any) string {
	return block["element"].(map[string]any)["initial_option"].(map[string]any)["value"].(string)
}

func hintText(block map[string]any) string {
	hint, _ := block["hint"].(map[string]any)
	text, _ := hint["text"].(string)
	return text
}

// submitPicker posts the view_submission of the picker opened as view: the
// agent agentRef, the question, and — when the view has a Workspace select —
// the option whose value is workspace ("" leaves the optional select empty).
func submitPicker(t *testing.T, srv *httptest.Server, view map[string]any, user, agentRef, workspace, question string) {
	t.Helper()
	values := map[string]any{
		"ask_agent_agent":    map[string]any{"agent": map[string]any{"type": "static_select", "selected_option": map[string]any{"value": agentRef}}},
		"ask_agent_question": map[string]any{"question": map[string]any{"type": "plain_text_input", "value": question}},
	}
	if block := workspaceBlock(view); block != nil {
		state := map[string]any{"type": "static_select", "selected_option": nil}
		if workspace != "" {
			state["selected_option"] = map[string]any{"value": workspace}
		}
		values[block["block_id"].(string)] = map[string]any{"workspace": state}
	}
	postInteraction(t, srv, map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": user},
		"view": map[string]any{
			"id":               "V1",
			"callback_id":      "ask_agent",
			"private_metadata": view["private_metadata"],
			"state":            map[string]any{"values": values},
		},
	})
}

// changePickerAgent posts the block_actions Slack sends when the person picks
// agentRef in the open picker view: the view as Slack holds it, and the pick.
func changePickerAgent(t *testing.T, srv *httptest.Server, view map[string]any, user, agentRef string) {
	t.Helper()
	open := map[string]any{"id": "V1", "hash": "h1"}
	for k, v := range view {
		open[k] = v
	}
	postInteraction(t, srv, map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": user},
		"view":    open,
		"actions": []any{map[string]any{"action_id": "agent", "block_id": "ask_agent_agent", "type": "static_select", "selected_option": map[string]any{"value": agentRef}}},
	})
}

// updatedView waits for the n-th views.update and returns its view.
func updatedView(t *testing.T, fake *fakeSlackAPI, n int) map[string]any {
	t.Helper()
	fake.waitForPath(t, "views.update", n)
	call := fake.pathCalls("views.update")[n-1]
	require.Equal(t, "V1", call.params["view_id"])
	require.Equal(t, "h1", call.params["hash"], "the redraw is guarded by the view's hash")
	return call.params["view"].(map[string]any)
}

// recordLastWorkspace writes user's last choice for agentRef, as a start from
// the picker does.
func recordLastWorkspace(t *testing.T, rec *slackadapter.MemoryRecorder, user, agentRef string, choice *store.WorkspaceChoice) {
	t.Helper()
	require.NoError(t, rec.UpdateThreadRecord(t.Context(), "slack-workspace-pref", user, agentRef, func(e *store.Entry, _ bool) bool {
		e.Workspace = choice
		return true
	}))
}

// The slash command opens the picker with the Workspace select between Agent
// and Prompt: No workspace first, then U001's workspaces grouped by
// Organization with their sync age, the one being deleted left out, preset to
// No workspace with no earlier choice. The agent select dispatches its
// changes.
func TestPickerWorkspace_SlashCommandOffersTheWorkspaces(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, _ := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	view := openedView(t, fake)

	blocks := view["blocks"].([]any)
	require.Len(t, blocks, 4)
	agent := blocks[1].(map[string]any)
	require.Equal(t, true, agent["dispatch_action"], "a change of agent reaches the gateway")
	require.Equal(t, "Type to search or pick an agent", agent["element"].(map[string]any)["placeholder"].(map[string]any)["text"])
	require.Equal(t, "Swarmgeist is the default agent.", hintText(agent))

	ws := blocks[2].(map[string]any)
	require.Equal(t, "ask_agent_workspace.0", ws["block_id"])
	require.Equal(t, true, ws["optional"])
	require.Equal(t, "Workspace", ws["label"].(map[string]any)["text"])
	element := ws["element"].(map[string]any)
	require.Equal(t, "static_select", element["type"], "Slack filters a static select as the person types")
	require.Equal(t, "Type to search or pick a workspace", element["placeholder"].(map[string]any)["text"])
	require.Equal(t, []string{
		"[Without a workspace]", "No workspace",
		"[acme]", "klaus-dev · never synced",
		"[giantswarm]", "platform · synced 3 h ago", "secret · synced 10 min ago",
	}, optionTexts(element))
	require.Equal(t, "*none*", initialValue(ws), "no earlier choice: No workspace")
	require.Empty(t, hintText(ws))
	require.Equal(t, "Prompt", blocks[3].(map[string]any)["label"].(map[string]any)["text"])
}

// One Organization needs no groups: No workspace and the workspaces are one
// list.
func TestPickerWorkspace_OneOrganizationIsOneList(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	wm.setWorkspaces("tok1", map[string]any{"name": "platform", "organization": "giantswarm"})
	gw, _ := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	element := workspaceBlock(openedView(t, fake))["element"].(map[string]any)
	require.Nil(t, element["option_groups"])
	require.Equal(t, []string{"No workspace", "platform · never synced"}, optionTexts(element))
}

// A submission with a workspace starts the thread with it: authorized as the
// submitter, the grant on the first turn, no thread prompt. The choice becomes
// the preset the next time the person opens the picker for that agent.
func TestPickerWorkspace_SubmissionStartsInTheWorkspaceAndPresetsTheNext(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	submitPicker(t, srv, openedView(t, fake), "U001", "kagent/sre-agent", "platform", "why are pods crashlooping?")
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)

	got := captured()[0]
	require.Equal(t, "kagent/sre-agent", got.AgentRef)
	require.Equal(t, &store.WorkspaceChoice{Name: "platform"}, got.Workspace)
	require.Equal(t, "grant-of-platform-for-tok1", got.WorkspaceGrant.Token(), "the submitter's grant")
	require.Empty(t, workspacePrompts(fake), "the modal chose: the thread asks nothing")
	require.Equal(t, []string{"Bearer tok1"}, authorizeBearers(wm))

	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	fake.waitForPath(t, "views.open", 2)
	next := fake.pathCalls("views.open")[1].params["view"].(map[string]any)
	require.Equal(t, "kagent/swarmgeist", blocks(next)[1]["element"].(map[string]any)["initial_option"].(map[string]any)["value"])
	require.Equal(t, "*none*", initialValue(workspaceBlock(next)), "the default agent was never started with one")
}

func blocks(view map[string]any) []map[string]any {
	var out []map[string]any
	for _, b := range view["blocks"].([]any) {
		out = append(out, b.(map[string]any))
	}
	return out
}

// The preset is the workspace last used with the agent the picker opens on,
// named in the hint; a workspace no longer offered presets No workspace.
func TestPickerWorkspace_PresetIsTheLastWorkspaceStillOffered(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	rec := slackadapter.NewMemoryRecorder()
	gw, _ := captureDispatch(rec)
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

	recordLastWorkspace(t, rec, "U001", "kagent/swarmgeist", &store.WorkspaceChoice{Name: "klaus-dev"})
	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	ws := workspaceBlock(openedView(t, fake))
	require.Equal(t, "klaus-dev", initialValue(ws))
	require.Equal(t, "Last used with Swarmgeist.", hintText(ws))

	recordLastWorkspace(t, rec, "U001", "kagent/swarmgeist", &store.WorkspaceChoice{Name: "gone"})
	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	fake.waitForPath(t, "views.open", 2)
	ws = workspaceBlock(fake.pathCalls("views.open")[1].params["view"].(map[string]any))
	require.Equal(t, "*none*", initialValue(ws), "a workspace no longer offered is no preset")
	require.Empty(t, hintText(ws))
}

// Picking another agent redraws the picker with that agent's preset under a
// new block revision, so Slack takes it; the other blocks go back unchanged.
func TestPickerWorkspace_PresetFollowsAChangeOfAgent(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	rec := slackadapter.NewMemoryRecorder()
	gw, _ := captureDispatch(rec)
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))
	recordLastWorkspace(t, rec, "U001", "kagent/sre-agent", &store.WorkspaceChoice{Name: "platform"})

	sendSlashCommand(t, srv, "C1", "U001", "check the nodes", api.URL+"/response_url")
	view := openedView(t, fake)
	require.Equal(t, "*none*", initialValue(workspaceBlock(view)))

	changePickerAgent(t, srv, view, "U001", "kagent/sre-agent")
	redrawn := updatedView(t, fake, 1)
	ws := workspaceBlock(redrawn)
	require.Equal(t, "ask_agent_workspace.1", ws["block_id"])
	require.Equal(t, "platform", initialValue(ws))
	require.Equal(t, "Last used with SRE Agent.", hintText(ws))
	require.Equal(t, "ask_agent", redrawn["callback_id"])
	require.Equal(t, view["private_metadata"], redrawn["private_metadata"])
	require.Equal(t, blocks(view)[3], blocks(redrawn)[3], "the Prompt goes back as it was, so Slack keeps what was typed")

	changePickerAgent(t, srv, redrawn, "U001", "kagent/swarmgeist")
	ws = workspaceBlock(updatedView(t, fake, 2))
	require.Equal(t, "ask_agent_workspace.2", ws["block_id"])
	require.Equal(t, "*none*", initialValue(ws))
	require.Empty(t, hintText(ws))
}

// No workspace, picked or left empty, starts the Session without a grant,
// asks the workspace-manager nothing more and the thread nothing at all.
func TestPickerWorkspace_NoWorkspaceStartsWithoutAGrant(t *testing.T) {
	for name, value := range map[string]string{"picked": "*none*", "cleared": ""} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeSlackAPI()
			api := fake.server(t)
			wm := newFakeWorkspaceManager(t)
			gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
			_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

			sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
			submitPicker(t, srv, openedView(t, fake), "U001", "kagent/sre-agent", value, "hello")
			require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
			require.Equal(t, &store.WorkspaceChoice{None: true}, captured()[0].Workspace)
			require.True(t, captured()[0].WorkspaceGrant.IsZero())
			require.Zero(t, wm.calls("authorize_workspace"))
			require.Equal(t, 1, wm.calls("list_workspaces"), "only the picker listed them")
			require.Empty(t, workspacePrompts(fake))
		})
	}
}

// A refused workspace starts nothing: no root, no turn. The submitter is told
// privately why, with the prompt handed back.
func TestPickerWorkspace_RefusalStartsNothingAndHandsThePromptBack(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	submitPicker(t, srv, openedView(t, fake), "U001", "kagent/sre-agent", "secret", "rotate the vault keys\nall of them")
	require.Eventually(t, func() bool { return len(fake.pathCalls("response_url")) == 1 }, flowWait, 20*time.Millisecond)
	notice := responseURLTexts(fake)
	require.Contains(t, notice, "Cannot start in workspace *secret*: you cannot read 1 repository of workspace secret: giantswarm/vault.")
	require.Contains(t, notice, "Your prompt:\n>rotate the vault keys\n>all of them")
	require.Empty(t, captured())
	require.Empty(t, fake.pathCalls("chat.postMessage"), "nothing is posted")
}

// The message shortcut opens the same picker with the Workspace select, and
// its submission starts the existing thread in the workspace.
func TestPickerWorkspace_ShortcutStartsTheThreadInTheWorkspace(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U001", "103.000", "100.000", api.URL+"/response_url")
	view := openedView(t, fake)
	require.NotNil(t, workspaceBlock(view))
	submitPicker(t, srv, view, "U001", "kagent/sre-agent", "klaus-dev", "what happened here?")
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
	got := captured()[0]
	require.Equal(t, "100.000", got.ThreadID)
	require.Equal(t, &store.WorkspaceChoice{Name: "klaus-dev"}, got.Workspace)
	require.Equal(t, "grant-of-klaus-dev-for-tok1", got.WorkspaceGrant.Token())
	require.Empty(t, workspacePrompts(fake))
}

// A listing that misses its budget opens the picker without the Workspace
// select, in time for Slack's trigger, and the thread then asks once.
func TestPickerWorkspace_LateListingFallsBackToTheThreadPrompt(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	list := wm.CallTools[workspaceToolsPrefix+"list_workspaces"]
	var calls atomic.Int32
	wm.CallTools[workspaceToolsPrefix+"list_workspaces"] = func(call mustertest.Call) mustertest.Answer {
		if calls.Add(1) == 1 {
			time.Sleep(2 * time.Second)
		}
		return list(call)
	}
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm), withSelection(pickerRoster(), pickerCards()))

	start := time.Now()
	sendSlashCommand(t, srv, "C1", "U001", "", api.URL+"/response_url")
	view := openedView(t, fake)
	require.Less(t, time.Since(start), 2500*time.Millisecond, "the picker opens within Slack's trigger window")
	require.Nil(t, workspaceBlock(view))
	require.Nil(t, blocks(view)[1]["dispatch_action"])

	submitPicker(t, srv, view, "U001", "kagent/sre-agent", "", "hello")
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 1 }, flowWait, 20*time.Millisecond, "the thread asks instead")
	require.Empty(t, captured())
}

// A person without workspaces, and an installation without them, get the
// picker exactly as it is without the feature (the golden view).
func TestPickerWorkspace_WithoutWorkspacesThePickerIsUnchanged(t *testing.T) {
	golden := filepath.Join("testdata", "picker-without-workspaces.json")
	for name, opts := range map[string][]func(*slackadapter.Adapter){
		"workspaces off": {channelMode},
		"none listed":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeSlackAPI()
			api := fake.server(t)
			gw, _ := captureDispatch(slackadapter.NewMemoryRecorder())
			if opts == nil {
				wm := newFakeWorkspaceManager(t)
				wm.setWorkspaces("tok1")
				opts = []func(*slackadapter.Adapter){withWorkspaces(wm)}
			}
			_, srv := newEventsAdapter(t, gw, api.URL, append(opts, withSelection(pickerRoster(), pickerCards()))...)

			sendSlashCommand(t, srv, "C1", "U001", "why are pods crashlooping?", "https://hooks.example.com/r")
			got, err := json.MarshalIndent(openedView(t, fake), "", "  ")
			require.NoError(t, err)
			if *updateGolden {
				require.NoError(t, os.WriteFile(golden, append(got, '\n'), 0o600))
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(got))
		})
	}
}
