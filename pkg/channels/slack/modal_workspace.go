package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// The agent picker's Workspace select: where the person decides, with the
// agent, which workspace the conversation's Session works in. Both selects are
// static_selects, which Slack filters as the person types, so each is one
// search-and-dropdown field without a round trip per keystroke. The field
// shows when workspaces are configured and the person has at least one; its
// preset is their last choice with the chosen agent (a workspace still
// offered, or No workspace), else the agent's default workspace, else No
// workspace, and it follows a change of agent. A modal opened without it (the
// listing was late or failed) leaves the choice to the thread's workspace
// prompt, as a mention does.

// modalWorkspaceListBudget bounds the listing of the person's workspaces
// before views.open; it runs beside the roster read, and what is left of
// pickerOpenBudget after it is views.open's.
const modalWorkspaceListBudget = 1500 * time.Millisecond

// workspacePrefChannel is the routing-store channel of the last-used records:
// one row per person (channel id) and agent (thread id), whose Workspace is
// the choice of the person's last start of that agent from the picker. No
// channel adapter lists it, and it ages out like a thread.
const workspacePrefChannel = "slack-workspace-pref"

// modalWorkspaces is what the picker's Workspace select offers a person: their
// workspaces and the agents they last started with a choice.
type modalWorkspaces struct {
	offered []workspaces.Workspace
	now     time.Time
	// lastFor is the person's last choice when they started an agent from
	// the picker, nil for none.
	lastFor func(agentRef string) *store.WorkspaceChoice
	// defaults are the agents' default workspaces, by ref.
	defaults map[string]string
}

// presetSource says where a Workspace select's preset comes from, for its
// hint.
type presetSource int

const (
	presetNone presetSource = iota
	presetLast
	presetDefault
)

// preset is the option value the Workspace select opens on for agentRef and
// where it comes from (workspacePreset).
func (ws *modalWorkspaces) preset(agentRef string) (string, presetSource) {
	var last *store.WorkspaceChoice
	if ws.lastFor != nil {
		last = ws.lastFor(agentRef)
	}
	offered := func(name string) bool {
		return slices.ContainsFunc(ws.offered, func(w workspaces.Workspace) bool { return w.Name == name })
	}
	return workspacePreset(last, ws.defaults[agentRef], offered)
}

// workspacePreset is the option value a Workspace select opens on: the last
// choice (No workspace, or a workspace offered still), else the agent's
// default workspace def when offered, else No workspace.
func workspacePreset(last *store.WorkspaceChoice, def string, offered func(string) bool) (string, presetSource) {
	switch {
	case last != nil && last.None:
		return askAgentWorkspaceNoneValue, presetLast
	case last.Chosen() && offered(last.Name):
		return last.Name, presetLast
	case def != "" && offered(def):
		return def, presetDefault
	}
	return askAgentWorkspaceNoneValue, presetNone
}

// workspaceHint is the hint under a Workspace select whose preset comes from
// source, for the agent agentLabel; "" for none.
func workspaceHint(source presetSource, agentLabel string) string {
	if agentLabel == "" {
		return ""
	}
	switch source {
	case presetLast:
		return truncateRunes(fmt.Sprintf(askAgentWorkspaceLastHint, agentLabel), modalHintMax)
	case presetDefault:
		return truncateRunes(fmt.Sprintf(askAgentWorkspaceDefaultHint, agentLabel), modalHintMax)
	}
	return ""
}

// pickerWorkspaces lists user's workspaces for the picker within
// modalWorkspaceListBudget. ok is false when the field is not offered:
// workspaces are off, the person has none, or the listing failed or was late.
func (a *Adapter) pickerWorkspaces(ctx context.Context, user string) (*modalWorkspaces, bool) {
	if a.Workspaces == nil || !a.Workspaces.Enabled() || a.OBO == nil {
		return nil, false
	}
	lctx, cancel := context.WithTimeout(ctx, modalWorkspaceListBudget)
	defer cancel()
	token, err := a.OBO.TokenFor(lctx, user)
	if err != nil {
		return nil, false
	}
	list, err := a.Workspaces.List(lctx, token)
	if err != nil {
		a.Logger.Info("slack: picker opens without the workspace select, the thread asks instead", "user", user, "error", err)
		return nil, false
	}
	offered := offeredWorkspaces(list)
	// No workspace takes one of the options a flat select holds.
	offered = offered[:min(len(offered), workspaceOptionsMax-1)]
	if len(offered) == 0 {
		return nil, false
	}
	return &modalWorkspaces{
		offered: offered,
		now:     time.Now(),
		lastFor: func(agentRef string) *store.WorkspaceChoice { return a.lastWorkspace(ctx, user, agentRef) },
	}, true
}

// lastWorkspace is the choice user made the last time they started agentRef
// from the picker, nil when there is none.
func (a *Adapter) lastWorkspace(ctx context.Context, user, agentRef string) *store.WorkspaceChoice {
	if a.gw == nil || user == "" || agentRef == "" {
		return nil
	}
	row, ok, err := a.gw.ThreadRecord(ctx, workspacePrefChannel, user, agentRef)
	if err != nil || !ok {
		return nil
	}
	return row.Workspace
}

// recordLastWorkspace remembers choice as user's last for agentRef: a name or
// None, never a grant.
func (a *Adapter) recordLastWorkspace(ctx context.Context, user, agentRef string, choice *store.WorkspaceChoice) {
	if a.gw == nil || choice == nil {
		return
	}
	err := a.gw.UpdateThreadRecord(ctx, workspacePrefChannel, user, agentRef, func(e *store.Entry, _ bool) bool {
		e.Workspace = &store.WorkspaceChoice{Name: choice.Name, None: choice.None}
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: record last workspace failed", "user", user, "agent", agentRef, "error", err)
	}
}

// workspaceInput is the picker's Workspace select at revision rev, preset to
// preset from source; agentLabel names the agent in its hint.
func workspaceInput(ws *modalWorkspaces, rev int, preset string, source presetSource, agentLabel string) map[string]any {
	none := map[string]any{bkText: plainTextObj(workspaceNoneLabel), bkValue: askAgentWorkspaceNoneValue}
	initial := none
	option := func(w workspaces.Workspace) map[string]any {
		opt := map[string]any{bkText: plainTextObj(workspaceOptionText(w, ws.now)), bkValue: w.Name}
		if w.Name == preset {
			initial = opt
		}
		return opt
	}
	element := map[string]any{
		bkType:        bkStaticSelect,
		bkActionID:    askAgentWorkspaceActionID,
		bkPlaceholder: plainTextObj(askAgentWorkspacePlaceholder),
	}
	if orgs := organizations(ws.offered); len(orgs) > 1 {
		groups := []any{map[string]any{bkLabel: plainTextObj(askAgentWorkspaceNoneGroup), bkOptions: []any{none}}}
		for _, org := range orgs {
			var options []any
			for _, w := range ws.offered {
				if w.Organization == org {
					options = append(options, option(w))
				}
			}
			label := org
			if label == "" {
				label = workspaceNoOrganization
			}
			groups = append(groups, map[string]any{bkLabel: plainTextObj(truncateRunes(label, slackOptionTextMax)), bkOptions: options})
		}
		element[bkOptionGroups] = groups
	} else {
		options := []any{none}
		for _, w := range ws.offered {
			options = append(options, option(w))
		}
		element[bkOptions] = options
	}
	element[bkInitialOption] = initial
	input := map[string]any{
		bkType:     bkInput,
		bkBlockID:  askAgentWorkspaceBlockID + "." + strconv.Itoa(rev),
		bkOptional: true,
		bkLabel:    plainTextObj(askAgentWorkspaceLabel),
		bkElement:  element,
	}
	if hint := workspaceHint(source, agentLabel); hint != "" {
		input[bkHint] = plainTextObj(hint)
	}
	return input
}

// organizations are the Organizations of offered in their order, which
// offeredWorkspaces sorts by Organization.
func organizations(offered []workspaces.Workspace) []string {
	var orgs []string
	for _, w := range offered {
		if len(orgs) == 0 || orgs[len(orgs)-1] != w.Organization {
			orgs = append(orgs, w.Organization)
		}
	}
	return orgs
}

// workspaceBlockRev is the revision of a Workspace input's block_id; ok is
// false for any other block.
func workspaceBlockRev(blockID string) (int, bool) {
	rest, ok := strings.CutPrefix(blockID, askAgentWorkspaceBlockID+".")
	if !ok {
		return 0, false
	}
	rev, err := strconv.Atoi(rest)
	return rev, err == nil
}

// submittedWorkspace is the choice a submitted picker carries: nil when it had
// no Workspace select, None for No workspace (or a cleared optional field).
func submittedWorkspace(values map[string]map[string]blockActionState) *store.WorkspaceChoice {
	for blockID, actions := range values {
		if _, ok := workspaceBlockRev(blockID); !ok {
			continue
		}
		v := actions[askAgentWorkspaceActionID].selectedValue()
		if v == "" || v == askAgentWorkspaceNoneValue {
			return &store.WorkspaceChoice{None: true}
		}
		return &store.WorkspaceChoice{Name: v}
	}
	return nil
}

// handleAskAgentAgentChange redraws an open picker whose agent changed, so its
// Workspace select opens on the new agent's preset, its default workspace read
// from the person's roster. Everything else is sent
// back as Slack holds it, and the inputs whose block_id stays keep what the
// person typed. The view's hash makes a stale redraw fail instead of
// overwriting a newer view.
func (a *Adapter) handleAskAgentAgentChange(ctx context.Context, payload interactionPayload, agentRef string) {
	view := payload.View
	blocks := slices.Clone(view.Blocks)
	idx, rev := -1, 0
	for i, b := range blocks {
		id, _ := b[bkBlockID].(string)
		if r, ok := workspaceBlockRev(id); ok {
			idx, rev = i, r
			break
		}
	}
	if idx < 0 || agentRef == "" {
		return
	}
	// The preset is matched against the options the select already holds: a
	// workspace no longer on them is no preset, and No workspace stands in.
	callerCtx := a.withCallerToken(ctx, payload.User.ID)
	last := a.lastWorkspace(ctx, payload.User.ID, agentRef)
	def := a.defaultWorkspaceOf(callerCtx, agentRef)
	block, source, ok := redrawWorkspaceBlock(blocks[idx], rev+1, last, def)
	if !ok {
		return
	}
	if hint := workspaceHint(source, a.agentNameFor(callerCtx, agentRef)); hint != "" {
		block[bkHint] = plainTextObj(hint)
	} else {
		delete(block, bkHint)
	}
	blocks[idx] = block
	out := make([]any, len(blocks))
	for i, b := range blocks {
		out[i] = b
	}
	updated := map[string]any{
		bkType:            bkModal,
		bkCallbackID:      view.CallbackID,
		bkPrivateMetadata: view.PrivateMetadata,
		bkTitle:           view.Title,
		bkSubmit:          view.Submit,
		bkClose:           view.Close,
		bkBlocks:          out,
	}
	if err := a.apiClient().viewsUpdate(ctx, view.ID, view.Hash, updated); err != nil {
		a.Logger.Info("slack: picker redraw for a new agent failed", "user", payload.User.ID, "error", err)
	}
}

// redrawWorkspaceBlock is the Workspace input block at revision rev, opening
// on workspacePreset's option for the last choice and the default workspace
// def among the options it holds; source says which.
func redrawWorkspaceBlock(block map[string]any, rev int, last *store.WorkspaceChoice, def string) (_ map[string]any, source presetSource, ok bool) {
	// A deep copy through JSON: the block is Slack's, decoded from the payload.
	raw, err := json.Marshal(block)
	if err != nil {
		return nil, presetNone, false
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, presetNone, false
	}
	element, ok := out[bkElement].(map[string]any)
	if !ok {
		return nil, presetNone, false
	}
	var options []map[string]any
	collect := func(list any) {
		items, _ := list.([]any)
		for _, o := range items {
			if opt, ok := o.(map[string]any); ok {
				options = append(options, opt)
			}
		}
	}
	collect(element[bkOptions])
	groups, _ := element[bkOptionGroups].([]any)
	for _, g := range groups {
		if group, ok := g.(map[string]any); ok {
			collect(group[bkOptions])
		}
	}
	byValue := make(map[string]map[string]any, len(options))
	for _, opt := range options {
		if v, ok := opt[bkValue].(string); ok {
			byValue[v] = opt
		}
	}
	preset, source := workspacePreset(last, def, func(name string) bool { return byValue[name] != nil })
	initial := byValue[preset]
	if initial == nil {
		return nil, presetNone, false
	}
	element[bkInitialOption] = initial
	out[bkBlockID] = askAgentWorkspaceBlockID + "." + strconv.Itoa(rev)
	return out, source, true
}
