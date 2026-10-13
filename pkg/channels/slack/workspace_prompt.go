package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// A thread whose Session is about to start without a workspace choice asks
// its initiator once which workspace the Session works in, or none. Nothing is
// read from the message: the offer is the initiator's workspaces, the choice is
// their click. The messages that arrive meanwhile are held with the prompt in
// the thread's row (heldState.Workspace), so a restart between the prompt and
// the click loses none of them.

// workspacePrompt is a thread's open workspace prompt: whose choice it is, the
// message it was posted in, and the messages held until the choice, the first
// of them the one that started the thread. StoredAt runs pendingTTL, across a
// restart too.
//
// choosing is set from the initiator's click until the first message has run:
// the prompt stays open meanwhile, so what arrives before the Session is bound
// waits behind it instead of being asked again.
type workspacePrompt struct {
	Initiator    string        `json:"initiator"`
	SlackChannel string        `json:"slack_channel"`
	PromptTS     string        `json:"prompt_ts,omitempty"`
	Messages     []heldMessage `json:"messages"`
	StoredAt     time.Time     `json:"stored_at"`

	choosing bool
}

func (p *workspacePrompt) expired(now time.Time) bool {
	return !p.choosing && now.Sub(p.StoredAt) > pendingTTL
}

// workspaceListTimeout bounds the listing of the initiator's workspaces, which
// sits before the thread's first turn.
const workspaceListTimeout = 10 * time.Second

// workspaceAuthorizeTimeout bounds authorize_workspace on a choice.
const workspaceAuthorizeTimeout = 15 * time.Second

// workspaceOptionsMax is Slack's bound on the options of one static select.
const workspaceOptionsMax = 100

// holdForWorkspace holds msg behind the thread's workspace prompt and reports
// whether it did. A prompt that is open holds every message of the thread. A
// message of the initiator whose thread has no Session and no choice yet, and
// that makes no choice itself, opens one when the initiator has workspaces to
// pick from; without any, or when they cannot be listed, the Session starts
// without one and nobody is asked. The caller holds the thread's slot.
func (a *Adapter) holdForWorkspace(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser, initiator string) bool {
	if !a.Workspaces.Enabled() || msg.Workspace != nil || msg.TaskID != "" {
		return false
	}
	now := time.Now()
	if held, dropped := a.holdBehindWorkspacePrompt(msg, slackChannel, now); held {
		if dropped {
			a.notifyParkedDrop(ctx, slackChannel, msg.ThreadID, slackUser)
		}
		a.persistHeld(slackChannel, msg.ThreadID)
		return true
	}
	if slackUser != initiator {
		return false
	}
	row, ok, err := a.gw.ThreadRecord(ctx, ChannelName, slackChannel, msg.ThreadID)
	if err != nil {
		a.Logger.Warn("slack: read thread record failed, workspace not asked", "thread", msg.ThreadID, "error", err)
		return false
	}
	if ok && (row.AgentInstanceID != "" || row.Workspace != nil) {
		return false
	}

	lctx, cancel := context.WithTimeout(ctx, workspaceListTimeout)
	list, err := a.Workspaces.List(lctx, msg.BearerToken)
	cancel()
	if err != nil {
		a.Logger.Warn("slack: workspaces not listed, the Session starts without one", "thread", msg.ThreadID, "user", slackUser, "error", err)
		if _, perr := a.apiClient().postNote(ctx, slackChannel, workspacesUnavailableNote, msg.ThreadID); perr != nil {
			a.Logger.Warn("slack: post workspaces-unavailable note failed", "thread", msg.ThreadID, "error", perr)
		}
		return false
	}
	offered := offeredWorkspaces(list)
	if len(offered) == 0 {
		return false
	}

	p := &workspacePrompt{
		Initiator:    initiator,
		SlackChannel: slackChannel,
		Messages:     []heldMessage{heldMessageOf(msg, slackChannel, now)},
		StoredAt:     now,
	}
	a.storeWorkspacePrompt(msg.ThreadID, p)
	text, blocks := workspacePromptBlocks(msg.ThreadID, initiator, offered, now)
	ts, err := a.apiClient().postBlocks(ctx, slackChannel, msg.ThreadID, text, blocks)
	if err != nil {
		// A prompt nobody sees would hold the message for a day: the Session
		// starts without a workspace instead.
		a.Logger.Warn("slack: post workspace prompt failed, the Session starts without one", "thread", msg.ThreadID, "error", err)
		_, _ = a.takeWorkspacePrompt(msg.ThreadID, p)
		return false
	}
	a.withThread(msg.ThreadID, func(st *threadState) {
		if st.workspace == p {
			p.PromptTS = ts
		}
	})
	a.persistHeld(slackChannel, msg.ThreadID)
	a.Logger.Info("slack: workspace asked", "thread", msg.ThreadID, "user", initiator, "offered", len(offered))
	return true
}

// holdBehindWorkspacePrompt appends msg to the thread's open prompt. held is
// false when none is open; an expired one is dropped on the way. Past
// maxParkedPerThread the oldest held messages but the first go (dropped).
func (a *Adapter) holdBehindWorkspacePrompt(msg channels.InboundMessage, slackChannel string, now time.Time) (held, dropped bool) {
	a.withThread(msg.ThreadID, func(st *threadState) {
		p := st.workspace
		if p == nil {
			return
		}
		if p.expired(now) {
			st.workspace = nil
			return
		}
		held = true
		p.Messages = append(p.Messages, heldMessageOf(msg, slackChannel, now))
		if len(p.Messages) > maxParkedPerThread {
			p.Messages = append(p.Messages[:1], lastN(p.Messages[1:], maxParkedPerThread-1)...)
			dropped = true
		}
	})
	return held, dropped
}

// storeWorkspacePrompt opens p on threadID and drops the prompts of other
// threads that expired, so abandoned ones do not stay for the process's
// lifetime.
func (a *Adapter) storeWorkspacePrompt(threadID string, p *workspacePrompt) {
	var swept []store.Key
	a.threadsMu.Lock()
	if a.threads == nil {
		a.threads = make(map[string]*threadState)
	}
	st := a.threads[threadID]
	if st == nil {
		st = &threadState{}
		a.threads[threadID] = st
	}
	st.workspace = p
	for thread, other := range a.threads {
		if other.workspace != nil && other.workspace.expired(p.StoredAt) {
			swept = append(swept, store.Key{ChannelID: other.workspace.SlackChannel, ThreadID: thread})
			other.workspace = nil
			if other.empty() {
				delete(a.threads, thread)
			}
		}
	}
	a.threadsMu.Unlock()
	for _, k := range swept {
		a.persistHeld(k.ChannelID, k.ThreadID)
	}
}

// openWorkspacePrompt is the thread's open prompt, nil when none is open or
// it expired.
func (a *Adapter) openWorkspacePrompt(threadID string) *workspacePrompt {
	var p *workspacePrompt
	a.withThread(threadID, func(st *threadState) {
		if st.workspace != nil && !st.workspace.expired(time.Now()) {
			p = st.workspace
		}
	})
	return p
}

// takeWorkspacePrompt closes p on threadID and returns the messages it still
// holds; ok is false when p was not open.
func (a *Adapter) takeWorkspacePrompt(threadID string, p *workspacePrompt) (held []heldMessage, ok bool) {
	a.withThread(threadID, func(st *threadState) {
		if st.workspace == p {
			st.workspace, held, ok = nil, p.Messages, true
		}
	})
	if ok {
		a.persistHeld(p.SlackChannel, threadID)
	}
	return held, ok
}

// claimWorkspacePrompt starts the initiator's choice on p and hands over the
// message that started the thread; ok is false when p is not open or another
// click is choosing already: of two clicks racing on one prompt, one claims
// it.
func (a *Adapter) claimWorkspacePrompt(threadID string, p *workspacePrompt) (first heldMessage, ok bool) {
	a.withThread(threadID, func(st *threadState) {
		if st.workspace != p || p.choosing || len(p.Messages) == 0 {
			return
		}
		p.choosing, ok = true, true
		first, p.Messages = p.Messages[0], slices.Clone(p.Messages[1:])
	})
	if ok {
		a.persistHeld(p.SlackChannel, threadID)
	}
	return first, ok
}

// unclaimWorkspacePrompt gives first back to p after a choice that could not
// start the Session, so the initiator can pick again.
func (a *Adapter) unclaimWorkspacePrompt(threadID string, p *workspacePrompt, first heldMessage) {
	a.withThread(threadID, func(st *threadState) {
		p.choosing = false
		p.Messages = append([]heldMessage{first}, p.Messages...)
	})
	a.persistHeld(p.SlackChannel, threadID)
}

// offeredWorkspaces are the workspaces a Session can be started on, by
// Organization and name, at most workspaceOptionsMax.
func offeredWorkspaces(list []workspaces.Workspace) []workspaces.Workspace {
	offered := slices.DeleteFunc(slices.Clone(list), func(w workspaces.Workspace) bool { return w.Deleting })
	slices.SortFunc(offered, func(x, y workspaces.Workspace) int {
		return cmp.Or(cmp.Compare(x.Organization, y.Organization), cmp.Compare(x.Name, y.Name))
	})
	return offered[:min(len(offered), workspaceOptionsMax)]
}

// workspaceValue is a workspace option's value: the thread and the workspace.
type workspaceValue struct {
	Thread    string `json:"t"`
	Workspace string `json:"w"`
}

func encodeWorkspaceValue(threadID, workspace string) string {
	b, _ := json.Marshal(workspaceValue{Thread: threadID, Workspace: workspace})
	return string(b)
}

// decodeWorkspaceValue reads an option's value; it is attacker-shaped input,
// so anything but both fields is refused.
func decodeWorkspaceValue(s string) (workspaceValue, bool) {
	var v workspaceValue
	if err := json.Unmarshal([]byte(s), &v); err != nil || v.Thread == "" || v.Workspace == "" {
		return workspaceValue{}, false
	}
	return v, true
}

// workspacePromptBlocks is the prompt: a select of the offered workspaces
// grouped by Organization, each with the age of its last sync, and the No
// workspace button.
func workspacePromptBlocks(threadID, initiator string, offered []workspaces.Workspace, now time.Time) (string, []any) {
	text := fmt.Sprintf(workspacePromptText, initiator)
	var groups []any
	var options []any
	org := ""
	flush := func() {
		if len(options) > 0 {
			label := org
			if label == "" {
				label = workspaceNoOrganization
			}
			groups = append(groups, map[string]any{
				bkLabel:   map[string]any{bkType: bkPlainText, bkText: truncateRunes(label, slackOptionTextMax)},
				bkOptions: options,
			})
		}
		options = nil
	}
	for _, w := range offered {
		if w.Organization != org || len(options) == 0 {
			flush()
			org = w.Organization
		}
		options = append(options, map[string]any{
			bkText:  map[string]any{bkType: bkPlainText, bkText: workspaceOptionText(w, now)},
			bkValue: encodeWorkspaceValue(threadID, w.Name),
		})
	}
	flush()
	return text, []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: text}},
		map[string]any{
			bkType:    bkActions,
			bkBlockID: workspacePromptBlockID,
			bkElements: []any{
				map[string]any{
					bkType:         bkStaticSelect,
					bkActionID:     workspaceSelectAction,
					bkPlaceholder:  map[string]any{bkType: bkPlainText, bkText: "Pick a workspace"},
					bkOptionGroups: groups,
				},
				map[string]any{
					bkType:     bkButton,
					bkText:     map[string]any{bkType: bkPlainText, bkText: workspaceNoneLabel},
					bkActionID: workspaceNoneAction,
					bkValue:    threadID,
				},
			},
		},
	}
}

// workspaceOptionText is an option's label: the workspace and how long ago its
// volume last caught up with its repositories.
func workspaceOptionText(w workspaces.Workspace, now time.Time) string {
	age := " · " + syncAge(w.SyncedAt, now)
	return truncateRunes(w.Name, slackOptionTextMax-len([]rune(age))) + age
}

// syncAge says how long ago at was, for a person choosing a workspace.
func syncAge(at, now time.Time) string {
	if at.IsZero() {
		return "never synced"
	}
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return "synced just now"
	case d < time.Hour:
		return fmt.Sprintf("synced %d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("synced %d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("synced %d days ago", int(d/(24*time.Hour)))
}

// handleWorkspaceChoice is a click on a workspace prompt: a workspace from the
// select, or none (workspace ""). Only the initiator's click counts. A
// workspace is authorized as them first; a refusal leaves the prompt open and
// starts nothing. The choice closes the prompt, which then names it, and the
// held messages go to the agent in order, the first carrying the choice and
// the grant its Session starts with.
func (a *Adapter) handleWorkspaceChoice(ctx context.Context, payload interactionPayload, threadID, workspace string) {
	slackChannel, clicker := payload.Channel.ID, payload.User.ID
	client := a.apiClient()
	p := a.openWorkspacePrompt(threadID)
	if p == nil {
		a.Logger.Info("slack: workspace choice for no open prompt", "thread", threadID, "user", clicker)
		if err := client.postEphemeralText(ctx, slackChannel, clicker, threadID, workspacePromptExpiredNotice); err != nil {
			a.Logger.Warn("slack: post workspace-expired notice failed", "thread", threadID, "error", err)
		}
		if ts := payload.Container.MessageTS; ts != "" {
			if err := client.chatUpdate(ctx, slackChannel, ts, workspacePromptClosedText, []any{contextBlock(workspacePromptClosedText)}); err != nil {
				a.Logger.Warn("slack: rewrite closed workspace prompt failed", "thread", threadID, "error", err)
			}
		}
		return
	}
	if clicker != p.Initiator {
		a.Logger.Info("slack: workspace choice from another user ignored", "thread", threadID, "user", clicker, "initiator", p.Initiator)
		if err := client.postEphemeralText(ctx, slackChannel, clicker, threadID, fmt.Sprintf(workspaceChoiceRefusal, p.Initiator)); err != nil {
			a.Logger.Warn("slack: post workspace-choice refusal failed", "thread", threadID, "error", err)
		}
		return
	}
	first, ok := a.claimWorkspacePrompt(threadID, p)
	if !ok {
		return
	}

	choice := &store.WorkspaceChoice{None: true}
	var grant workspaces.Grant
	if workspace != "" {
		var refusal string
		grant, refusal = a.authorizeWorkspace(ctx, clicker, workspace)
		if grant.IsZero() {
			a.unclaimWorkspacePrompt(threadID, p, first)
			if _, err := client.postNote(ctx, p.SlackChannel, refusal, threadID); err != nil {
				a.Logger.Warn("slack: post workspace refusal failed", "thread", threadID, "error", err)
			}
			return
		}
		choice = &store.WorkspaceChoice{Name: workspace}
	}
	a.Logger.Info("slack: workspace chosen", "thread", threadID, "user", clicker, "workspace", choice.String())

	notice := fmt.Sprintf(workspaceNoneNotice, clicker)
	if choice.Chosen() {
		notice = fmt.Sprintf(workspaceChosenNotice, escapeMrkdwn(workspace), clicker)
	}
	if p.PromptTS != "" {
		if err := client.chatUpdate(ctx, p.SlackChannel, p.PromptTS, notice, []any{contextBlock(notice)}); err != nil {
			a.Logger.Warn("slack: rewrite workspace prompt failed", "thread", threadID, "error", err)
		}
	}

	// replayDispatch waits for the thread's slot and runs the turn to its end;
	// this is the click's own goroutine. The first message starts the Session
	// in the choice while the prompt still holds what arrives meanwhile; the
	// rest then find the Session bound, in the order they came.
	msg := first.Msg
	msg.Workspace, msg.WorkspaceGrant = choice, grant
	a.replayHeld(ctx, threadID, msg, first.SlackChannel)
	rest, _ := a.takeWorkspacePrompt(threadID, p)
	for _, m := range rest {
		a.replayHeld(ctx, threadID, m.Msg, m.SlackChannel)
	}
}

// replayHeld runs a message the workspace prompt held, saying so in the thread
// when it fails.
func (a *Adapter) replayHeld(ctx context.Context, threadID string, msg channels.InboundMessage, slackChannel string) {
	if err := a.replayDispatch(ctx, msg, slackChannel); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Error("slack: replay after workspace choice failed", "thread", threadID, "error", err)
		a.postReplayFailureNote(ctx, slackChannel, threadID, err)
	}
}

// authorizeWorkspace asks the workspace-manager, as slackUser, for a grant on
// workspace. A zero grant comes with the thread note that says why there is
// none.
func (a *Adapter) authorizeWorkspace(ctx context.Context, slackUser, workspace string) (workspaces.Grant, string) {
	name := escapeMrkdwn(workspace)
	if a.OBO == nil {
		return workspaces.Grant{}, fmt.Sprintf(workspaceCheckFailedNote, name)
	}
	token, err := a.OBO.TokenFor(ctx, slackUser)
	if err != nil {
		a.Logger.Warn("slack: token for workspace authorization unavailable", "user", slackUser, "error", err)
		return workspaces.Grant{}, tokenErrorNotice
	}
	actx, cancel := context.WithTimeout(ctx, workspaceAuthorizeTimeout)
	defer cancel()
	auth, err := a.Workspaces.Authorize(actx, token, workspace)
	if err != nil {
		a.Logger.Warn("slack: workspace authorization failed", "user", slackUser, "workspace", workspace, "error", err)
		return workspaces.Grant{}, fmt.Sprintf(workspaceCheckFailedNote, name)
	}
	if auth.Outcome == workspaces.Granted {
		return auth.Grant, ""
	}
	a.Logger.Info("slack: workspace not granted", "user", slackUser, "workspace", workspace, "outcome", auth.Outcome.String())
	reason := strings.TrimSuffix(strings.TrimSpace(auth.Reason), ".")
	if reason == "" {
		reason = auth.Outcome.String()
	}
	return workspaces.Grant{}, fmt.Sprintf(workspaceRefusedNote, name, escapeMrkdwn(reason))
}
