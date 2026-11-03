package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/muster/mustertest"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

const workspaceToolsPrefix = "x_workspace-manager_"

// workspaceLinked are the people the workspace flow tests sign in: U001
// starts the threads, U002 is someone else in the channel.
var workspaceLinked = map[string]string{"U001": "tok1", "U002": "tok2"}

// fakeWorkspaceManager is the workspace-manager behind a fake muster:
// list_workspaces answers each caller's workspaces by bearer token, and
// authorize_workspace grants platform and klaus-dev, refuses secret (two
// repositories the caller cannot read), answers fresh as not synced yet, and
// asks to connect GitLab for gitlab-ws until the caller is marked connected.
// A caller marked blind reads none of a workspace's repositories, and one
// marked unconnected connects GitLab first, whatever the workspace. Its
// connect answer's signIn names muster's core_auth_login for gitlab-manager,
// which the fake muster answers with gitlabSignInLink, until unpinned: then
// muster holds no grant for GitLab.
type fakeWorkspaceManager struct {
	*mustertest.Server
	mu          sync.Mutex
	lists       map[string][]any
	connected   map[string]bool
	blind       map[string]bool
	unconnected map[string]bool
	unpinned    bool
	challenged  map[string]bool
}

// workspaceManagerSignInLink is the link of muster's sign-in challenge for
// the workspace-manager itself.
const workspaceManagerSignInLink = "https://muster.example/oauth/proxy/start?state=wm"

// challenge makes muster answer the person behind token with its sign-in
// challenge for the workspace-manager: they have not granted its App.
func (wm *fakeWorkspaceManager) challenge(token string, on bool) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.challenged[token] = on
}

// challengeFor is muster's answer to a call of the person behind call's
// token while challenged, nil otherwise.
func (wm *fakeWorkspaceManager) challengeFor(call mustertest.Call) *mustertest.Answer {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	if !wm.challenged[strings.TrimPrefix(call.Bearer, "Bearer ")] {
		return nil
	}
	return &mustertest.Answer{IsError: true, Text: "auth_required: server 'workspace-manager' requires authentication\n\n" + workspaceManagerSignInLink,
		Structured: map[string]any{"status": "auth_required", "server": "workspace-manager", "authUrl": workspaceManagerSignInLink}}
}

// gitlabSignInLink is the link muster's core_auth_login answers for
// gitlab-manager.
const gitlabSignInLink = "https://muster.example/oauth/proxy/start?state=gitlab"

// unpin pins the workspace-manager to no provider's App: muster holds no
// grant for GitLab, so its signIn names no muster server.
func (wm *fakeWorkspaceManager) unpin() {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.unpinned = true
}

// blindTo marks the person behind token unable to read any repository.
func (wm *fakeWorkspaceManager) blindTo(token string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.blind[token] = true
}

// disconnect marks the person behind token not connected to GitLab, for every
// workspace.
func (wm *fakeWorkspaceManager) disconnect(token string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.unconnected[token] = true
}

// connect marks the person behind token connected to GitLab.
func (wm *fakeWorkspaceManager) connect(token string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.connected[token] = true
	delete(wm.unconnected, token)
}

func newFakeWorkspaceManager(t *testing.T) *fakeWorkspaceManager {
	t.Helper()
	wm := &fakeWorkspaceManager{lists: map[string][]any{}, connected: map[string]bool{}, blind: map[string]bool{}, unconnected: map[string]bool{}, challenged: map[string]bool{}}
	synced := func(ago time.Duration) map[string]any {
		return map[string]any{"time": time.Now().Add(-ago).UTC().Format(time.RFC3339), "result": "Succeeded"}
	}
	wm.setWorkspaces("tok1",
		map[string]any{"name": "platform", "organization": "giantswarm", "lastSync": synced(3*time.Hour + time.Minute)},
		map[string]any{"name": "secret", "organization": "giantswarm", "lastSync": synced(10 * time.Minute)},
		map[string]any{"name": "klaus-dev", "organization": "acme"},
		map[string]any{"name": "gone", "organization": "acme", "deleting": true},
	)
	wm.Server = mustertest.New(nil)
	wm.CallTools = map[string]func(mustertest.Call) mustertest.Answer{
		workspaceToolsPrefix + "list_workspaces": func(call mustertest.Call) mustertest.Answer {
			if c := wm.challengeFor(call); c != nil {
				return *c
			}
			wm.mu.Lock()
			defer wm.mu.Unlock()
			list := wm.lists[strings.TrimPrefix(call.Bearer, "Bearer ")]
			if list == nil {
				list = []any{}
			}
			return mustertest.Answer{Structured: map[string]any{"workspaces": list}}
		},
		workspaceToolsPrefix + "authorize_workspace": func(call mustertest.Call) mustertest.Answer {
			if c := wm.challengeFor(call); c != nil {
				return *c
			}
			name, _ := call.Args["workspace"].(string)
			token := strings.TrimPrefix(call.Bearer, "Bearer ")
			wm.mu.Lock()
			blind, unconnected := wm.blind[token], wm.unconnected[token]
			wm.mu.Unlock()
			if blind {
				name = "secret"
			} else if unconnected {
				name = "gitlab-ws"
			}
			switch name {
			case "secret":
				reason := "you cannot read 2 repositories of workspace secret"
				return mustertest.Answer{Text: reason, Structured: map[string]any{"workspace": name, "outcome": "refused", "reason": reason,
					"unreadable": []any{"giantswarm/vault", "giantswarm/keys"}}}
			case "fresh":
				reason := "workspace fresh has not synced yet"
				return mustertest.Answer{Text: reason, Structured: map[string]any{"workspace": name, "outcome": "refused", "reason": reason}}
			case "gitlab-ws":
				wm.mu.Lock()
				connected := wm.connected[token]
				wm.mu.Unlock()
				if !connected {
					signIn := "grant the App of provider gitlab once in muster: core_auth_login server=gitlab-manager"
					if wm.unpinned {
						signIn = "muster holds no grant for provider gitlab: this installation pins the workspace-manager to no provider's App"
					}
					return mustertest.Answer{Structured: map[string]any{"workspace": name, "outcome": "connect_required", "reason": "grant gitlab in muster first",
						"connect": []any{map[string]any{"provider": "gitlab", "signIn": signIn}}}}
				}
			}
			return mustertest.Answer{Structured: map[string]any{"workspace": name, "outcome": "granted",
				"grant":     "grant-of-" + name + "-for-" + token,
				"expiresAt": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)}}
		}, "core_auth_login": func(call mustertest.Call) mustertest.Answer {
			if call.Args["server"] != "gitlab-manager" {
				return mustertest.Answer{IsError: true, Text: "server not found"}
			}
			return mustertest.Answer{Text: "Authentication Required\n\nServer: gitlab-manager\n\n" + gitlabSignInLink,
				Structured: map[string]any{"authUrl": gitlabSignInLink}}
		},
	}
	t.Cleanup(wm.Close)
	return wm
}

func (wm *fakeWorkspaceManager) setWorkspaces(token string, list ...any) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wm.lists[token] = list
}

// calls counts the calls of one of the workspace-manager's tools.
func (wm *fakeWorkspaceManager) calls(tool string) int {
	n := 0
	for _, c := range wm.Calls() {
		if c.Tool == workspaceToolsPrefix+tool {
			n++
		}
	}
	return n
}

// withWorkspaces turns workspaces on against wm, with workspaceLinked signed
// in.
func withWorkspaces(wm *fakeWorkspaceManager) func(*slackadapter.Adapter) {
	return func(a *slackadapter.Adapter) {
		channelMode(a)
		a.OBO = &multiUserOBO{linked: workspaceLinked}
		a.Workspaces = workspaces.New(muster.NewClient(wm.URL), workspaceToolsPrefix)
	}
}

// workspacePrompts are the workspace prompts posted.
func workspacePrompts(fake *fakeSlackAPI) []recordedCall {
	var prompts []recordedCall
	for _, c := range fake.pathCalls("chat.postMessage") {
		if strings.Contains(allBlockText([]recordedCall{c}), "workspace_select") {
			prompts = append(prompts, c)
		}
	}
	return prompts
}

// sendWorkspaceClick posts a signed click on a workspace prompt: a pick from
// its select, or its No workspace button for workspace "".
func sendWorkspaceClick(t *testing.T, srv *httptest.Server, clicker, threadID, workspace string) {
	t.Helper()
	action := map[string]any{"action_id": "workspace_none", "value": threadID}
	if workspace != "" {
		value, err := json.Marshal(map[string]string{"t": threadID, "w": workspace})
		require.NoError(t, err)
		action = map[string]any{"action_id": "workspace_select", "type": "static_select",
			"selected_option": map[string]any{"value": string(value)}}
	}
	data, err := json.Marshal(map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": clicker},
		"channel":      map[string]any{"id": "C1"},
		"container":    map[string]any{"message_ts": "1700000000.000001"},
		"message":      map[string]any{"thread_ts": threadID},
		"response_url": srv.URL + "/response",
		"actions":      []any{action},
	})
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
}

// workspaceKagent is a kagent controller behind agent-manager that records
// each Session start's grant and each turn's text, and completes every turn.
type workspaceKagent struct {
	*ownedKagent
	grants []string
	turns  []string
}

func newWorkspaceKagent() *workspaceKagent {
	return &workspaceKagent{ownedKagent: &ownedKagent{owners: map[string]string{}, shares: map[string]string{}}}
}

func (k *workspaceKagent) StartSession(ctx context.Context, agentRef, requestID, name string, grant workspaces.Grant) (pkga2a.Session, error) {
	k.mu.Lock()
	k.grants = append(k.grants, grant.Token())
	k.mu.Unlock()
	return k.ownedKagent.StartSession(ctx, agentRef, requestID, name, grant)
}

func (k *workspaceKagent) Stream(_ context.Context, sessionID string, msg *a2apkg.Message) iter.Seq2[a2apkg.Event, error] {
	var text strings.Builder
	for _, p := range msg.Parts {
		text.WriteString(p.Text())
	}
	k.mu.Lock()
	k.streamed++
	k.turns = append(k.turns, text.String())
	task := a2apkg.TaskInfo{TaskID: a2apkg.TaskID(fmt.Sprintf("task-%d", k.streamed)), ContextID: sessionID}
	k.mu.Unlock()
	return func(yield func(a2apkg.Event, error) bool) {
		for _, ev := range []a2apkg.Event{
			&a2apkg.Task{ID: task.TaskID, ContextID: task.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
			a2apkg.NewArtifactEvent(task, a2apkg.NewTextPart("done")),
			a2apkg.NewStatusUpdateEvent(task, a2apkg.TaskStateCompleted, nil),
		} {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (k *workspaceKagent) state() (created int, grants, turns []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.created, append([]string(nil), k.grants...), append([]string(nil), k.turns...)
}

// A new thread asks its initiator once. Picking a workspace starts one Session
// with that workspace's grant, and the held message is its first turn; the
// message written while the prompt was open follows it. The next three turns
// ask nothing and run on the same Session.
func TestWorkspace_NewThreadAsksOnceAndStartsOneSessionWithTheGrant(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	routes := memory.New()
	t.Cleanup(func() { _ = routes.Close() })
	kagent := newWorkspaceKagent()
	facade := &channels.Facade{Agent: kagent, Routes: routes, ThreadTTL: channels.DefaultThreadTTL}
	a, srv := newEventsAdapter(t, facade, api.URL, withWorkspaces(wm))

	openConversationIn(t, facade, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 1 }, flowWait, 20*time.Millisecond, "the thread asks for its workspace")
	sendEvent(t, srv, mention("U001", "<@UBOT> and on staging?", "101.000", "100.000"))
	require.Eventually(t, func() bool { return wm.calls("list_workspaces") == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "100.000")

	prompt := allBlockText(workspacePrompts(fake))
	require.Contains(t, prompt, "platform · synced 3 h ago")
	require.Contains(t, prompt, "klaus-dev · never synced")
	require.Contains(t, prompt, `"giantswarm"`, "grouped by Organization")
	require.Contains(t, prompt, "No workspace")
	require.NotContains(t, prompt, "gone", "a workspace being deleted is not offered")
	created, _, _ := kagent.state()
	require.Zero(t, created, "nothing starts before the choice")

	sendWorkspaceClick(t, srv, "U001", "100.000", "platform")
	require.Eventually(t, func() bool { _, _, turns := kagent.state(); return len(turns) == 2 }, flowWait, 20*time.Millisecond)
	created, grants, turns := kagent.state()
	require.Equal(t, 1, created, "one Session")
	require.Equal(t, []string{"grant-of-platform-for-tok1"}, grants, "started with the initiator's grant for the chosen workspace")
	require.Contains(t, turns[0], "what failed on prod?", "the held message is the first turn")
	require.Contains(t, turns[1], "and on staging?")
	require.Contains(t, allText(fake.pathCalls("chat.update")), "Working in workspace *platform*, chosen by <@U001>.")

	row, ok, err := facade.ThreadRecord(t.Context(), "slack", "C1", "100.000")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, &store.WorkspaceChoice{Name: "platform"}, row.Workspace)
	require.Empty(t, row.Held, "nothing is held any more")

	for i, text := range []string{"and dev?", "and the logs?", "thanks"} {
		waitThreadIdle(t, a, "100.000")
		sendEvent(t, srv, mention("U001", "<@UBOT> "+text, fmt.Sprintf("10%d.500", 2+i), "100.000"))
		require.Eventually(t, func() bool { _, _, turns := kagent.state(); return len(turns) == 3+i }, flowWait, 20*time.Millisecond)
	}
	created, _, _ = kagent.state()
	require.Equal(t, 1, created, "the next turns reuse the Session")
	require.Len(t, workspacePrompts(fake), 1, "and ask nothing")
	require.Equal(t, 1, wm.calls("list_workspaces"))
	require.Equal(t, 1, wm.calls("authorize_workspace"))
}

// A sender with no workspaces is never asked: the Session starts at once,
// without one.
func TestWorkspace_SenderWithoutWorkspacesIsNotAsked(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	wm.setWorkspaces("tok1")
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm))

	gw.openConversation(t, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> hello", "100.000", ""))
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
	require.Nil(t, captured()[0].Workspace, "no choice, nobody asked")
	require.True(t, captured()[0].WorkspaceGrant.IsZero())
	require.Empty(t, workspacePrompts(fake))
	require.Equal(t, 1, wm.calls("list_workspaces"))
}

// No workspace starts the Session without a grant, and asks the
// workspace-manager nothing more.
func TestWorkspace_NoWorkspaceStartsWithoutAGrant(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm))

	gw.openConversation(t, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> hello", "100.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 1 }, flowWait, 20*time.Millisecond)
	sendWorkspaceClick(t, srv, "U001", "100.000", "")
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
	got := captured()[0]
	require.Equal(t, &store.WorkspaceChoice{None: true}, got.Workspace)
	require.True(t, got.WorkspaceGrant.IsZero())
	require.Zero(t, wm.calls("authorize_workspace"))
	require.Contains(t, allText(fake.pathCalls("chat.update")), "Working without a workspace, chosen by <@U001>.")
}

// Nothing in the message chooses or narrows the workspace: a message that
// names one gets the same offer, waits for the click like any other, and runs
// in the workspace clicked, not the one it names.
func TestWorkspace_MessageTextChangesNeitherOfferNorChoice(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm))

	gw.openConversation(t, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> hello", "100.000", ""))
	gw.openConversation(t, "C1", "200.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> work in workspace klaus-dev, not platform", "200.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 2 }, flowWait, 20*time.Millisecond)
	prompts := workspacePrompts(fake)
	require.Equal(t, offer(t, prompts[0]), offer(t, prompts[1]), "the same offer whatever the message says")
	require.Empty(t, captured(), "the named workspace starts nothing")

	sendWorkspaceClick(t, srv, "U001", "200.000", "platform")
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
	got := captured()[0]
	require.Equal(t, "work in workspace klaus-dev, not platform", strings.TrimSpace(strings.TrimPrefix(got.Text, "<@UBOT>")))
	require.Equal(t, &store.WorkspaceChoice{Name: "platform"}, got.Workspace, "the click chooses")
}

// offer is a prompt's actions block, the thread's own value taken out.
func offer(t *testing.T, prompt recordedCall) string {
	t.Helper()
	blocks, err := json.Marshal(prompt.params["blocks"])
	require.NoError(t, err)
	thread, _ := prompt.params["thread_ts"].(string)
	return strings.ReplaceAll(string(blocks), thread, "")
}

// Only the initiator chooses; a refusal starts nothing and leaves the prompt
// open for another pick.
func TestWorkspace_OnlyTheInitiatorChoosesAndARefusalKeepsThePrompt(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm))

	gw.openConversation(t, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> hello", "100.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 1 }, flowWait, 20*time.Millisecond)

	sendWorkspaceClick(t, srv, "U002", "100.000", "platform")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "Only <@U001>, who started this thread, chooses its workspace.")
	}, flowWait, 20*time.Millisecond)

	sendWorkspaceClick(t, srv, "U001", "100.000", "secret")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "You cannot start in workspace *secret*: you cannot read `giantswarm/vault`, `giantswarm/keys`.")
	}, flowWait, 20*time.Millisecond)
	require.Empty(t, captured(), "a refusal starts nothing")

	sendWorkspaceClick(t, srv, "U001", "100.000", "platform")
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
	require.Equal(t, &store.WorkspaceChoice{Name: "platform"}, captured()[0].Workspace)
	require.Equal(t, []string{"Bearer tok1", "Bearer tok1"}, authorizeBearers(wm), "authorized as the initiator only")
}

func authorizeBearers(wm *fakeWorkspaceManager) []string {
	var bearers []string
	for _, c := range wm.Calls() {
		if c.Tool == workspaceToolsPrefix+"authorize_workspace" {
			bearers = append(bearers, c.Bearer)
		}
	}
	return bearers
}

// A restart between the prompt and the click keeps the held message: the
// click on the new process starts the Session in the chosen workspace with
// the message as its first turn.
func TestWorkspace_RestartBetweenPromptAndClickCompletesTheStart(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	shared := slackadapter.NewMemoryRecorder()

	a1, srv1 := newEventsAdapter(t, &stubGateway{records: shared}, api.URL, withWorkspaces(wm))
	openConversationIn(t, shared, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv1, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 1 }, flowWait, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		row, _, err := shared.ThreadRecord(t.Context(), "slack", "C1", "100.000")
		return err == nil && strings.Contains(string(row.Held), "what failed on prod?") && strings.Contains(string(row.Held), "prompt_ts")
	}, flowWait, 20*time.Millisecond, "the held message and its prompt reach the thread's row")
	require.NoError(t, a1.Stop(context.Background()))

	gw2, captured := captureDispatch(shared)
	a2, srv2 := newEventsAdapter(t, gw2, api.URL, withWorkspaces(wm))
	a2.RecoverTurns()
	a2.WaitHeldRestored()

	sendWorkspaceClick(t, srv2, "U001", "100.000", "platform")
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond, "the held message starts the Session after the restart")
	got := captured()[0]
	require.Contains(t, got.Text, "what failed on prod?")
	require.Equal(t, &store.WorkspaceChoice{Name: "platform"}, got.Workspace)
	require.False(t, got.WorkspaceGrant.IsZero(), "with the grant")
	require.Eventually(t, func() bool { return !heldIn(t, shared, "C1", "100.000") }, flowWait, 20*time.Millisecond, "the row holds nothing any more")
	require.Len(t, workspacePrompts(fake), 1, "nobody is asked twice")
}

// A click on a prompt that is no longer open says so and starts nothing.
func TestWorkspace_ClickOnAClosedPromptStartsNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	gw, captured := captureDispatch(slackadapter.NewMemoryRecorder())
	_, srv := newEventsAdapter(t, gw, api.URL, withWorkspaces(wm))

	sendWorkspaceClick(t, srv, "U001", "100.000", "platform")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "This workspace choice has expired.")
	}, flowWait, 20*time.Millisecond)
	require.Empty(t, captured())
	require.Zero(t, wm.calls("authorize_workspace"))
}

// A person who has not granted the workspace-manager's App gets muster's
// sign-in challenge for the listing: the thread answers with the Connect
// prompt for it, holding the message, never "could not be listed". Once
// connected, the landing replays the message, which lists again and asks for
// the workspace.
func TestWorkspace_SignInChallengeOnTheListingGetsTheConnectPrompt(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	wm := newFakeWorkspaceManager(t)
	wm.challenge("tok1", true)
	routes := memory.New()
	t.Cleanup(func() { _ = routes.Close() })
	kagent := newWorkspaceKagent()
	facade := &channels.Facade{Agent: kagent, Routes: routes, ThreadTTL: channels.DefaultThreadTTL}
	log := &lockedLog{}
	_, srv := newEventsAdapter(t, facade, api.URL, withWorkspaces(wm), withInfoLog(log), func(a *slackadapter.Adapter) {
		a.PublicBaseURL = "https://gw.example"
	})

	openConversationIn(t, facade, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "<@UBOT> what failed on prod?", "100.000", ""))
	var buttonURL, stateID string
	var ok bool
	require.Eventually(t, func() bool { buttonURL, stateID, ok = connectButton(fake); return ok }, flowWait, 20*time.Millisecond,
		"the Connect prompt for the workspace-manager")
	parsed, err := url.Parse(buttonURL)
	require.NoError(t, err)
	require.Equal(t, "wm", parsed.Query().Get("state"), "muster's own sign-in link, decorated")
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "*workspace-manager*")
	cards := 0
	for _, call := range append(fake.pathCalls("chat.postEphemeral"), fake.pathCalls("chat.postMessage")...) {
		if _, _, _, ok := connectButtonIn([]recordedCall{call}); !ok {
			continue
		}
		cards++
		require.Equal(t, 1, strings.Count(slackRendered(call), "The conversation can't start yet"),
			"%s: the Connect prompt says its lead sentence once", call.path)
	}
	require.Equal(t, 2, cards, "the ephemeral and the DM card")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "could not be listed")
	require.Empty(t, workspacePrompts(fake))
	created, _, _ := kagent.state()
	require.Zero(t, created, "nothing starts before the grant")
	require.Contains(t, log.String(), "record=session_connect_required")
	require.Contains(t, log.String(), "server=workspace-manager")

	wm.challenge("tok1", false)
	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID) + "&server=workspace-manager")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool { return len(workspacePrompts(fake)) == 1 }, flowWait, 20*time.Millisecond,
		"the replayed message lists the workspaces and asks")
	created, _, _ = kagent.state()
	require.Zero(t, created)
}
