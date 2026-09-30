package slack

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestToolLog_CapsEntriesAndCountsDropped(t *testing.T) {
	a := &Adapter{}
	turn := a.beginToolLogTurn("T1")
	require.Equal(t, 1, turn)
	for i := range maxToolLogEntries + 20 {
		a.appendToolLog("T1", toolLogEntry{turn: turn, name: fmt.Sprintf("tool-%d", i), called: true})
	}

	entries, dropped := a.toolLogSnapshot("T1")
	require.Len(t, entries, maxToolLogEntries, "the cap bounds retained entries")
	require.Equal(t, 20, dropped, "evicted entries are counted")
	require.Equal(t, "tool-20", entries[0].name, "the oldest entries are the ones evicted")
	require.Equal(t, fmt.Sprintf("tool-%d", maxToolLogEntries+19), entries[len(entries)-1].name)

	// Other threads are unaffected.
	other, _ := a.toolLogSnapshot("T2")
	require.Empty(t, other)
}

func TestToolLog_TurnOrdinalsAdvance(t *testing.T) {
	a := &Adapter{}
	t1 := a.beginToolLogTurn("T1")
	a.appendToolLog("T1", toolLogEntry{turn: t1, name: "first"})
	t2 := a.beginToolLogTurn("T1")
	a.appendToolLog("T1", toolLogEntry{turn: t2, name: "second"})
	require.Equal(t, 2, t2)

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 2)
	require.Equal(t, 1, entries[0].turn)
	require.Equal(t, 2, entries[1].turn)
}

func TestToolLog_TTLEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &Adapter{}
		a.appendToolLog("T1", toolLogEntry{turn: a.beginToolLogTurn("T1"), name: "old"})

		time.Sleep(threadStateTTL + time.Minute)

		entries, _ := a.toolLogSnapshot("T1")
		require.Empty(t, entries, "an idle thread's log expires")

		// Touching another thread sweeps the expired sibling out of the map.
		a.appendToolLog("T2", toolLogEntry{turn: a.beginToolLogTurn("T2"), name: "fresh"})
		a.toolLogMu.Lock()
		defer a.toolLogMu.Unlock()
		require.NotContains(t, a.toolLogs, "T1", "insert sweeps expired siblings")
		require.Contains(t, a.toolLogs, "T2")
	})
}

// A result closes the running call it belongs to: by call id, or, when the
// stream gave none, the oldest running call of the same tool without one. A
// result no running call of the turn matches stands on its own.
func TestToolLog_ResultsCloseTheirCalls(t *testing.T) {
	a := &Adapter{}
	turn := a.beginToolLogTurn("T1")
	a.appendToolLog("T1", toolLogEntry{turn: turn, callID: "c1", name: "get", called: true})
	a.appendToolLog("T1", toolLogEntry{turn: turn, name: "list", called: true})
	a.appendToolLog("T1", toolLogEntry{turn: turn, name: "list", called: true})

	a.completeToolLog("T1", toolLogEntry{turn: turn, name: "list", state: toolDone, result: "first"})
	a.completeToolLog("T1", toolLogEntry{turn: turn, callID: "c1", name: "get", state: toolFailed, result: "denied"})
	a.completeToolLog("T1", toolLogEntry{turn: turn, callID: "c9", name: "orphan", state: toolDone, result: "late"})

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 4)
	require.Equal(t, toolFailed, entries[0].state)
	require.Equal(t, "denied", entries[0].result)
	require.Equal(t, "first", entries[1].result, "first in, first out for calls without an id")
	require.Equal(t, toolRunning, entries[2].state)
	require.False(t, entries[3].called, "a result without its call is kept on its own")
	require.Equal(t, "late", entries[3].result)

	// Without an id a call_tool result names no inner tool: it closes the
	// oldest call muster ran.
	a.appendToolLog("T1", toolLogEntry{turn: turn, name: "x_kubernetes_get", viaMuster: true, called: true})
	a.completeToolLog("T1", toolLogEntry{turn: turn, name: musterCallToolMetaTool, state: toolDone, result: "pods"})
	entries, _ = a.toolLogSnapshot("T1")
	require.Len(t, entries, 5)
	require.Equal(t, "x_kubernetes_get", entries[4].name)
	require.Equal(t, "pods", entries[4].result)

	// A result never closes a call of another turn.
	next := a.beginToolLogTurn("T1")
	a.completeToolLog("T1", toolLogEntry{turn: next, name: "list", state: toolDone})
	entries, _ = a.toolLogSnapshot("T1")
	require.Equal(t, toolRunning, entries[2].state)
	require.Len(t, entries, 6)
}

// The writer records a call and pairs its result with it: the arguments as
// indented JSON, the result preview and the state.
func TestRenderToolActivity_RecordsCallAndResult(t *testing.T) {
	a, _ := newInspectTestAdapter(t)
	w := newBatchedWriterWithClient(a.apiClient(), "C1", "T1", testLogger())
	w.adapter = a

	w.renderToolActivity(&channels.ToolActivity{
		Kind: channels.ToolCall, Name: "kube_get", CallID: "c1",
		Args: map[string]any{"resource": "pods"},
	})
	w.renderToolActivity(&channels.ToolActivity{
		Kind: channels.ToolResult, Name: "kube_get", CallID: "c1",
		Response: map[string]any{"output": "3 pods"},
	})

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, "kube_get", e.name)
	require.True(t, e.called)
	require.Equal(t, "{\n  \"resource\": \"pods\"\n}", e.args, "the arguments are indented JSON")
	require.Equal(t, toolDone, e.state)
	require.Equal(t, "3 pods", e.result, "the output wrap is unwrapped to the bare payload")
}

// A result the runtime answers a call with to ask for approval is not the
// tool's output: it marks the call as stopped at the approval.
func TestRenderToolActivity_ApprovalRequestMarksTheCall(t *testing.T) {
	a, _ := newInspectTestAdapter(t)
	w := newBatchedWriterWithClient(a.apiClient(), "C1", "T1", testLogger())
	w.adapter = a

	w.renderToolActivity(&channels.ToolActivity{Kind: channels.ToolCall, Name: "restart", CallID: "c1"})
	w.renderToolActivity(&channels.ToolActivity{
		Kind: channels.ToolResult, Name: "restart", CallID: "c1", AwaitsApproval: true,
		Response: map[string]any{"error": "requires confirmation, please approve or reject"},
	})

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 1)
	require.Equal(t, toolAwaitingApproval, entries[0].state)
	require.Empty(t, entries[0].result)
}

// The turn an approval resumes streams the approved call's result, not the
// call. The writer records the call from the approval, so the resumed turn's
// log names both, and names the result by the tool call_tool really ran.
func TestRenderToolActivity_ApprovedCallIsLoggedInTheResumedTurn(t *testing.T) {
	a, _ := newInspectTestAdapter(t)
	w := newBatchedWriterWithClient(a.apiClient(), "C1", "T1", testLogger())
	w.adapter = a
	args := map[string]any{"name": "x_kubernetes_rollout_restart", "arguments": map[string]any{"name": "loki-backend"}}
	w.approvedCalls = []channels.HitlTool{{ID: "a1", CallID: "c1", Name: musterCallToolMetaTool, Args: args}}

	ch := make(chan channels.OutboundDelta, 3)
	ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
		Kind: channels.ToolResult, Name: musterCallToolMetaTool, CallID: "c1", Response: map[string]any{"output": "restarted"},
	}}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)
	require.NoError(t, w.run(t.Context(), ch))

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 1, "the approved call with its result")
	e := entries[0]
	require.Equal(t, "x_kubernetes_rollout_restart", e.name)
	require.True(t, e.viaMuster)
	require.True(t, e.called)
	require.Contains(t, e.args, "loki-backend")
	require.Equal(t, toolDone, e.state)
	require.Equal(t, "restarted", e.result)
}

// Tool names, args, and results are agent- and MCP-controlled: the rendered
// section must be escaped so hostile content cannot notify, and cannot break
// out of its code span or code block.
func TestCallSectionText_EscapesHostileContent(t *testing.T) {
	md := callSectionText(toolLogEntry{
		name:   "evil`<!channel>`\ntool",
		called: true,
		args:   indentJSON(map[string]any{"cmd": "a&b <script> ```x```"}, toolArgsMax),
		state:  toolDone,
		result: "ping <@U1> ``` then",
	})
	require.NotContains(t, md, "<!channel>", "angle brackets must be escaped")
	require.Contains(t, md, "&lt;!channel&gt;")
	require.NotContains(t, md, "<script>")
	require.NotContains(t, md, "\\u003c", "the payload carries the real characters, not JSON escapes")
	require.Contains(t, md, "&lt;script&gt;")
	require.Contains(t, md, "a&amp;b")
	require.NotContains(t, md, "<@U1>")
	require.Equal(t, 4, strings.Count(md, "```"), "a payload cannot open or close a code block")
	require.Contains(t, md, "`evil'&lt;!channel&gt;' tool`", "the raw name stays one code span on one line")
}

// Each state reads differently, and a call is named by its plain title beside
// the raw tool name.
func TestCallSectionText_States(t *testing.T) {
	done := callSectionText(toolLogEntry{name: "x_kubernetes_list", viaMuster: true, called: true, args: "{}", state: toolDone, result: "ok"})
	require.True(t, strings.HasPrefix(done, "✅ *Kubernetes list*  ·  `x_kubernetes_list`  via muster"), done)
	require.Contains(t, done, "*Arguments*\n```{}```")
	require.Contains(t, done, "*Result*\n```ok```")

	failed := callSectionText(toolLogEntry{name: "get", called: true, state: toolFailed, result: "forbidden"})
	require.True(t, strings.HasPrefix(failed, "❌"))
	require.Contains(t, failed, "_No arguments._")
	require.Contains(t, failed, "*Error*\n```forbidden```")

	require.Contains(t, callSectionText(toolLogEntry{name: "get", called: true, state: toolDone}), "*Result*: _no output._")
	require.True(t, strings.HasPrefix(callSectionText(toolLogEntry{name: "r", called: true, state: toolAwaitingApproval}), "⏸"))
	require.Contains(t, callSectionText(toolLogEntry{name: "r", called: true}), "⏳")
	require.Contains(t, callSectionText(toolLogEntry{name: "r", state: toolDone, result: "x"}), "_The call itself was not recorded._")
}

// However much the escaping grows a payload, one call's section stays under
// Slack's section limit.
func TestCallSectionText_FitsASection(t *testing.T) {
	e := toolLogEntry{
		name:   strings.Repeat("&", 300),
		called: true,
		args:   strings.Repeat("<", toolArgsMax),
		state:  toolFailed,
		result: strings.Repeat("&", toolResultMax),
	}
	md := callSectionText(e)
	require.LessOrEqual(t, len([]rune(md)), slackSectionTextMax)
	require.NotContains(t, md, "&am…", "a cut never splits an entity")
}

func TestCutEscaped(t *testing.T) {
	// A naive cut at 10 keeps "abcdef&am" and splits the entity.
	require.Equal(t, "abcdef…", cutEscaped("abcdef&amp;xyz", 10))
	// A whole entity right before the cut is kept.
	require.Equal(t, "ab&lt;…", cutEscaped("ab&lt;cdefgh", 7))
	// Under the cap nothing changes.
	require.Equal(t, "a&amp;b", cutEscaped("a&amp;b", 10))
}

func TestInspectionBlocks_TurnHeadersAndDropNote(t *testing.T) {
	entries := []toolLogEntry{
		{turn: 3, name: "a", called: true, state: toolDone},
		{turn: 3, name: "b", called: true, state: toolDone},
		{turn: 4, name: "c", called: true},
	}
	blocks := inspectionBlocks(entries, 7, nil, false)
	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	s := string(raw)
	require.Contains(t, s, "Turn 3 · 2 calls")
	require.Contains(t, s, "Turn 4 · 1 call")
	require.Contains(t, s, "7 earlier calls are not shown")
	// context line + (divider + header) per turn + one section per call
	require.Len(t, blocks, 1+2*2+3)
}

// A log of many short turns would outgrow a modal's 100 blocks with its turn
// headers: the oldest calls make room, and the context line counts them.
func TestInspectionBlocks_FitAModal(t *testing.T) {
	var entries []toolLogEntry
	for i := range maxToolLogEntries {
		entries = append(entries, toolLogEntry{turn: i + 1, name: "t", called: true})
	}
	blocks := inspectionBlocks(entries, 0, nil, true)
	require.LessOrEqual(t, len(blocks), maxInspectBlocks)
	raw, err := json.Marshal(blocks[0])
	require.NoError(t, err)
	shown := (len(blocks) - 1) / 3
	require.Contains(t, string(raw), fmt.Sprintf("%d earlier calls are not shown", maxToolLogEntries-shown))
}

// In the modal a call is its short line with a "Show details" button, whose
// value names the entry; open ones show their details and a "Hide" button. The
// fallback messages show every call's details and no button.
func TestInspectionBlocks_ShortLinesAndDetails(t *testing.T) {
	entries := []toolLogEntry{
		{id: 1, turn: 1, name: "x_kubernetes_list", viaMuster: true, called: true, args: "{}", state: toolDone, result: "ok"},
		{id: 2, turn: 1, name: "filter_tools", called: true, state: toolDone, result: "tools"},
	}
	blocks := inspectionBlocks(entries, 0, []int{2}, true)
	require.Len(t, blocks, 1+2+2)
	closed, _ := json.Marshal(blocks[3])
	require.NotContains(t, string(closed), "Arguments", "a closed call is its short line")
	require.Contains(t, string(closed), "Kubernetes list")
	require.Contains(t, string(closed), `"action_id":"inspect_toggle"`)
	require.Contains(t, string(closed), `"value":"1"`)
	require.Contains(t, string(closed), "Show details")
	open, _ := json.Marshal(blocks[4])
	require.Contains(t, string(open), "tools", "an open call shows its result")
	require.Contains(t, string(open), "Hide")

	flat, _ := json.Marshal(inspectionBlocks(entries, 0, nil, false))
	require.Contains(t, string(flat), "Arguments")
	require.NotContains(t, string(flat), "inspect_toggle", "a message has no toggle")
}

// "Show details" redraws the modal it was clicked in with that call open, and
// "Hide" closes it again; the view's hash guards the redraw.
func TestInspectToggle_OpensAndClosesDetails(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	a.appendToolLog("100.000", toolLogEntry{turn: a.beginToolLogTurn("100.000"), name: "kube_get", called: true, args: "{\n  \"kind\": \"pods\"\n}", state: toolDone, result: "3 pods"})

	a.routeInteraction(t.Context(), togglePayload(`{"t":"100.000"}`, "1"))
	updates := srv.updateBodies()
	require.Len(t, updates, 1)
	require.Equal(t, "V1", updates[0]["view_id"])
	require.Equal(t, "hash-1", updates[0]["hash"])
	view, _ := json.Marshal(updates[0]["view"])
	require.Contains(t, string(view), "3 pods", "the call opens with its details")
	require.Contains(t, string(view), "Hide")
	require.Contains(t, string(view), `\"e\":[1]`, "the modal remembers the open call")

	a.routeInteraction(t.Context(), togglePayload(`{"t":"100.000","e":[1]}`, "1"))
	updates = srv.updateBodies()
	require.Len(t, updates, 2)
	view, _ = json.Marshal(updates[1]["view"])
	require.NotContains(t, string(view), "3 pods", "Hide closes it again")
	require.NotContains(t, string(view), `\"e\":`)
	require.Empty(t, srv.ephemeralBodies())
}

// A click that is not on the inspection modal, or carries a broken state or
// value, redraws nothing.
func TestInspectToggle_IgnoresForeignClicks(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	a.appendToolLog("100.000", toolLogEntry{turn: a.beginToolLogTurn("100.000"), name: "kube_get", called: true})

	other := togglePayload(`{"t":"100.000"}`, "1")
	other.View.CallbackID = askAgentCallbackID
	a.routeInteraction(t.Context(), other)
	a.routeInteraction(t.Context(), togglePayload(`not json`, "1"))
	a.routeInteraction(t.Context(), togglePayload(`{"t":""}`, "1"))
	a.routeInteraction(t.Context(), togglePayload(`{"t":"100.000"}`, "one"))
	noView := togglePayload(`{"t":"100.000"}`, "1")
	noView.View.ID = ""
	a.routeInteraction(t.Context(), noView)

	require.Empty(t, srv.updateBodies())
}

// The shortcut opens a modal for the invoker; nothing is posted in the thread.
func TestRouteInteraction_MessageActionOpensTheModal(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	a.appendToolLog("100.000", toolLogEntry{turn: a.beginToolLogTurn("100.000"), name: "kube_get", called: true, args: "{}", state: toolDone, result: "ok"})

	a.routeInteraction(t.Context(), inspectPayload("100.000", ""))

	views := srv.viewBodies()
	require.Len(t, views, 1)
	require.Equal(t, "trigger-1", views[0]["trigger_id"])
	view, _ := json.Marshal(views[0]["view"])
	require.Contains(t, string(view), `"type":"modal"`)
	require.Contains(t, string(view), inspectModalTitle)
	require.Contains(t, string(view), "kube_get")
	require.Contains(t, string(view), "Turn 1 · 1 call")
	require.Contains(t, string(view), "Visible only to you")
	require.Contains(t, string(view), "Show details")
	require.NotContains(t, string(view), "Arguments", "every call opens as its short line")
	require.Contains(t, string(view), inspectViewCallbackID)
	require.Empty(t, srv.ephemeralBodies(), "nothing goes to the thread")
	require.Equal(t, int32(0), srv.posts.Load())
}

// A shortcut invoked on a top-level message (no thread_ts) resolves the thread
// by the message's own ts — the same key a turn on that message records under.
func TestRouteInteraction_MessageActionTopLevelMessage(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	a.appendToolLog("200.000", toolLogEntry{turn: a.beginToolLogTurn("200.000"), name: "top_level_tool", called: true})

	a.routeInteraction(t.Context(), inspectPayload("", "200.000"))

	views := srv.viewBodies()
	require.Len(t, views, 1)
	view, _ := json.Marshal(views[0]["view"])
	require.Contains(t, string(view), "top_level_tool")
}

// An empty or evicted log answers with honest guidance in the modal. A thread
// this process has other traces of gets the "no longer kept" wording; an
// unknown thread the generic guidance.
func TestRouteInteraction_MessageActionEmptyLog(t *testing.T) {
	a, srv := newInspectTestAdapter(t)

	a.routeInteraction(t.Context(), inspectPayload("300.000", ""))
	a.recordTurnUsage("400.000", "C1", channels.TurnUsage{TotalTokens: 1})
	a.routeInteraction(t.Context(), inspectPayload("400.000", ""))

	views := srv.viewBodies()
	require.Len(t, views, 2)
	first, _ := json.Marshal(views[0]["view"])
	require.Contains(t, string(first), "No tool activity is kept")
	second, _ := json.Marshal(views[1]["view"])
	require.Contains(t, string(second), "no longer kept")
	require.Empty(t, srv.ephemeralBodies())
	require.Equal(t, int32(0), srv.posts.Load(), "nothing is ever posted to the thread itself")
}

// A modal Slack refuses (the trigger_id lives 3 seconds) or cannot open (no
// trigger_id) is replaced by the same content as ephemeral messages in the
// thread, split to stay under a message's block budget.
func TestPostInspection_FallsBackToEphemeralMessages(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	srv.refuseViews = true
	turn := a.beginToolLogTurn("100.000")
	for i := range maxActivityBlocks + 10 {
		a.appendToolLog("100.000", toolLogEntry{turn: turn, name: fmt.Sprintf("tool-%d", i), called: true})
	}

	a.postInspection(t.Context(), "C1", "100.000", "U9", "trigger-1")

	require.Len(t, srv.viewBodies(), 1, "the modal was tried first")
	posts := srv.ephemeralBodies()
	require.Len(t, posts, 2, "the content overflows one message's block budget")
	for _, body := range posts {
		require.Equal(t, "U9", body["user"], "the reply goes to the invoker only")
		require.Equal(t, "100.000", body["thread_ts"], "the reply lands in the thread")
		blocks, ok := body["blocks"].([]any)
		require.True(t, ok)
		require.LessOrEqual(t, len(blocks), maxActivityBlocks)
	}

	// No trigger_id at all: no modal is tried; an empty log gets the notice.
	a.postInspection(t.Context(), "C1", "500.000", "U9", "")
	require.Len(t, srv.viewBodies(), 1)
	posts = srv.ephemeralBodies()
	require.Len(t, posts, 3)
	require.Contains(t, posts[2]["text"], "No tool activity is kept")
}

// Unknown callback_ids and payloads missing routing fields are dropped:
// interaction payloads are attacker-shaped input.
func TestRouteInteraction_MessageActionRejectsMalformed(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	a.appendToolLog("100.000", toolLogEntry{turn: a.beginToolLogTurn("100.000"), name: "entry", called: true})

	wrongCallback := inspectPayload("100.000", "")
	wrongCallback.CallbackID = "some_other_shortcut"
	a.routeInteraction(t.Context(), wrongCallback)

	noUser := inspectPayload("100.000", "")
	noUser.User.ID = ""
	a.routeInteraction(t.Context(), noUser)

	noChannel := inspectPayload("100.000", "")
	noChannel.Channel.ID = ""
	a.routeInteraction(t.Context(), noChannel)

	noMessage := inspectPayload("", "")
	a.routeInteraction(t.Context(), noMessage)

	require.Empty(t, srv.viewBodies())
	require.Empty(t, srv.ephemeralBodies())
	require.Equal(t, int32(0), srv.posts.Load())
}

// inspectFakeSlack records chat.postEphemeral and views.open bodies and
// counts any other Web API post, so tests can assert nothing lands in the
// thread itself. refuseViews makes views.open answer expired_trigger_id.
type inspectFakeSlack struct {
	mu          sync.Mutex
	ephemerals  []map[string]any
	views       []map[string]any
	updates     []map[string]any
	refuseViews bool
	posts       atomic.Int32
}

func (f *inspectFakeSlack) updateBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.updates))
	copy(out, f.updates)
	return out
}

func (f *inspectFakeSlack) ephemeralBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.ephemerals))
	copy(out, f.ephemerals)
	return out
}

func (f *inspectFakeSlack) viewBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.views))
	copy(out, f.views)
	return out
}

func (f *inspectFakeSlack) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postEphemeral", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.ephemerals = append(f.ephemerals, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/views.open", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.views = append(f.views, body)
		refuse := f.refuseViews
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "expired_trigger_id"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/views.update", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.updates = append(f.updates, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		f.posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.2"})
	})
	return mux
}

func newInspectTestAdapter(t *testing.T) (*Adapter, *inspectFakeSlack) {
	t.Helper()
	srv := &inspectFakeSlack{}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	a := &Adapter{
		APIBase: ts.URL,
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		Logger:  testLogger(),
	}
	a.gw = newMemoryRecorder()
	return a, srv
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// inspectPayload is a minimal "Inspect agent steps" message_action payload:
// user U9 invoking the shortcut in channel C1 on a message with the given
// thread_ts / ts.
func inspectPayload(threadTS, ts string) interactionPayload {
	var p interactionPayload
	p.Type = payloadTypeMessageAction
	p.CallbackID = inspectShortcutCallbackID
	p.TriggerID = "trigger-1"
	p.User.ID = "U9"
	p.Channel.ID = "C1"
	p.Message.ThreadTS = threadTS
	p.Message.TS = ts
	return p
}

// togglePayload is a "Show details" / "Hide" click in inspection modal V1 of
// user U9, with the modal's private_metadata and the button's value.
func togglePayload(state, value string) interactionPayload {
	var p interactionPayload
	p.Type = payloadTypeBlockActions
	p.User.ID = "U9"
	p.View.ID = "V1"
	p.View.Hash = "hash-1"
	p.View.CallbackID = inspectViewCallbackID
	p.View.PrivateMetadata = state
	p.Actions = append(p.Actions, struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	}{ActionID: inspectToggleAction, Value: value})
	return p
}
