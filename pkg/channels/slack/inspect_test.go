package slack

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		a.appendToolLog("T1", turn, fmt.Sprintf("entry-%d", i))
	}

	entries, dropped := a.toolLogSnapshot("T1")
	require.Len(t, entries, maxToolLogEntries, "the cap bounds retained entries")
	require.Equal(t, 20, dropped, "evicted entries are counted")
	require.Equal(t, "entry-20", entries[0].md, "the oldest entries are the ones evicted")
	require.Equal(t, fmt.Sprintf("entry-%d", maxToolLogEntries+19), entries[len(entries)-1].md)

	// Other threads are unaffected.
	other, _ := a.toolLogSnapshot("T2")
	require.Empty(t, other)
}

func TestToolLog_TurnOrdinalsAdvance(t *testing.T) {
	a := &Adapter{}
	t1 := a.beginToolLogTurn("T1")
	a.appendToolLog("T1", t1, "first-turn call")
	t2 := a.beginToolLogTurn("T1")
	a.appendToolLog("T1", t2, "second-turn call")
	require.Equal(t, 2, t2)

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 2)
	require.Equal(t, 1, entries[0].turn)
	require.Equal(t, 2, entries[1].turn)
}

func TestToolLog_TTLEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &Adapter{}
		a.appendToolLog("T1", a.beginToolLogTurn("T1"), "old entry")

		time.Sleep(threadStateTTL + time.Minute)

		entries, _ := a.toolLogSnapshot("T1")
		require.Empty(t, entries, "an idle thread's log expires")

		// Touching another thread sweeps the expired sibling out of the map.
		a.appendToolLog("T2", a.beginToolLogTurn("T2"), "fresh entry")
		a.toolLogMu.Lock()
		defer a.toolLogMu.Unlock()
		require.NotContains(t, a.toolLogs, "T1", "insert sweeps expired siblings")
		require.Contains(t, a.toolLogs, "T2")
	})
}

// The tool log records at detailsOn and detailsFull — the retroactive view must
// exist for the default level — but not at detailsOff, which stays private.
func TestRenderToolActivity_RecordsAtOnAndFullNotOff(t *testing.T) {
	for _, tc := range []struct {
		level detailsLevel
		want  int
	}{
		{detailsOn, 2},
		{detailsFull, 2},
		{detailsOff, 0},
	} {
		t.Run(tc.level.String(), func(t *testing.T) {
			a, _ := newInspectTestAdapter(t)
			w := newBatchedWriterWithClient(a.apiClient(), "C1", "", "T1", tc.level, testLogger())
			w.adapter = a

			w.renderToolActivity(t.Context(), &channels.ToolActivity{
				Kind: channels.ToolCall, Name: "kube_get", CallID: "c1",
				Args: map[string]any{"resource": "pods"},
			})
			w.renderToolActivity(t.Context(), &channels.ToolActivity{
				Kind: channels.ToolResult, Name: "kube_get", CallID: "c1",
				Response: map[string]any{"items": "3 pods"},
			})
			w.drainThreadPosts()

			entries, _ := a.toolLogSnapshot("T1")
			require.Len(t, entries, tc.want)
			if tc.want == 0 {
				return
			}
			require.Contains(t, entries[0].md, "🔧")
			require.Contains(t, entries[0].md, "kube_get")
			require.Contains(t, entries[0].md, "pods", "args summary is retained")
			require.Contains(t, entries[1].md, "↳")
			require.Contains(t, entries[1].md, "result")
			require.Contains(t, entries[1].md, "3 pods", "result preview is retained")
		})
	}
}

// Tool names, args, and results are agent- and MCP-controlled: the recorded
// entry must be escaped so hostile content cannot break out of the code spans
// or inject mrkdwn when the inspection renders it.
func TestRenderToolActivity_RecordedEntryEscapesHostileContent(t *testing.T) {
	a, _ := newInspectTestAdapter(t)
	w := newBatchedWriterWithClient(a.apiClient(), "C1", "", "T1", detailsOn, testLogger())
	w.adapter = a

	w.renderToolActivity(t.Context(), &channels.ToolActivity{
		Kind: channels.ToolCall, Name: "evil`<!channel>`\ntool",
		Args: map[string]any{"cmd": "a&b <script>"},
	})
	w.drainThreadPosts()

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 1)
	md := entries[0].md
	require.NotContains(t, md, "<!channel>", "angle brackets must be escaped")
	require.Contains(t, md, "&lt;!channel&gt;")
	require.NotContains(t, md, "`\n", "newlines and backticks must not break the code span")
	// Args pass through Go's HTML-safe JSON marshaling, which neutralises the
	// mrkdwn-sensitive bytes as literal \u escapes.
	require.NotContains(t, md, "<script>")
	require.Contains(t, md, "\\u003cscript\\u003e")
	require.Contains(t, md, "a\\u0026b")
}

// A call_tool invocation is unwrapped to the inner muster tool in the log,
// matching the detailsFull rendering.
func TestRenderToolActivity_RecordsUnwrappedCallTool(t *testing.T) {
	a, _ := newInspectTestAdapter(t)
	w := newBatchedWriterWithClient(a.apiClient(), "C1", "", "T1", detailsOn, testLogger())
	w.adapter = a

	w.renderToolActivity(t.Context(), &channels.ToolActivity{
		Kind: channels.ToolCall, Name: musterCallToolMetaTool, CallID: "c1",
		Args: map[string]any{"name": "x_prometheus_query", "arguments": map[string]any{"query": "up"}},
	})
	w.renderToolActivity(t.Context(), &channels.ToolActivity{
		Kind: channels.ToolResult, Name: musterCallToolMetaTool, CallID: "c1",
		Response: map[string]any{"output": "1"},
	})
	w.drainThreadPosts()

	entries, _ := a.toolLogSnapshot("T1")
	require.Len(t, entries, 2)
	require.Contains(t, entries[0].md, "x_prometheus_query")
	require.Contains(t, entries[0].md, "(via muster)")
	require.Contains(t, entries[0].md, "up", "inner args are the summary")
	require.Contains(t, entries[1].md, "x_prometheus_query")
	require.Contains(t, entries[1].md, "(via muster)")
}

func TestRouteInteraction_MessageActionRendersEphemeralLog(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	turn := a.beginToolLogTurn("100.000")
	a.appendToolLog("100.000", turn, "🔧 *`kube_get`*\n`{\"resource\": \"pods\"}`")

	a.routeInteraction(t.Context(), inspectPayload("100.000", ""))

	posts := srv.ephemeralBodies()
	require.Len(t, posts, 1)
	body := posts[0]
	require.Equal(t, "C1", body["channel"])
	require.Equal(t, "U9", body["user"], "the reply goes to the invoker only")
	require.Equal(t, "100.000", body["thread_ts"], "the reply lands in the thread")
	blocks, _ := json.Marshal(body["blocks"])
	require.Contains(t, string(blocks), "kube_get")
	require.Contains(t, string(blocks), "turn 1")
	require.Contains(t, string(blocks), "only you can see this")
}

// A shortcut invoked on a top-level message (no thread_ts) resolves the thread
// by the message's own ts — the same key a turn on that message records under.
func TestRouteInteraction_MessageActionTopLevelMessage(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	a.appendToolLog("200.000", a.beginToolLogTurn("200.000"), "🔧 *`tool`*")

	a.routeInteraction(t.Context(), inspectPayload("", "200.000"))

	posts := srv.ephemeralBodies()
	require.Len(t, posts, 1)
	require.Equal(t, "200.000", posts[0]["thread_ts"])
}

// An empty or evicted log answers with honest guidance instead of silence, and
// stays ephemeral. A thread this process has other traces of gets the
// "no longer retained" wording; an unknown thread the generic guidance.
func TestRouteInteraction_MessageActionEmptyLog(t *testing.T) {
	a, srv := newInspectTestAdapter(t)

	a.routeInteraction(t.Context(), inspectPayload("300.000", ""))
	posts := srv.ephemeralBodies()
	require.Len(t, posts, 1)
	require.Contains(t, posts[0]["text"], "don't have retained tool activity")
	require.Contains(t, posts[0]["text"], "/details full")

	// The same thread with a details setting is known to have been served.
	a.setDetailsLevel("400.000", detailsFull)
	a.routeInteraction(t.Context(), inspectPayload("400.000", ""))
	posts = srv.ephemeralBodies()
	require.Len(t, posts, 2)
	require.Contains(t, posts[1]["text"], "no longer retained")
	require.Equal(t, int32(0), srv.posts.Load(), "nothing is ever posted to the thread itself")
}

// Unknown callback_ids and payloads missing routing fields are dropped:
// interaction payloads are attacker-shaped input.
func TestRouteInteraction_MessageActionRejectsMalformed(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	a.appendToolLog("100.000", a.beginToolLogTurn("100.000"), "entry")

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

	require.Empty(t, srv.ephemeralBodies())
	require.Equal(t, int32(0), srv.posts.Load())
}

// A log larger than one message's block budget splits across several ephemeral
// posts instead of being rejected by Slack's block limit.
func TestPostInspection_SplitsAcrossMessages(t *testing.T) {
	a, srv := newInspectTestAdapter(t)
	turn := a.beginToolLogTurn("100.000")
	for i := range maxActivityBlocks + 10 {
		a.appendToolLog("100.000", turn, fmt.Sprintf("🔧 *`tool-%d`*", i))
	}

	a.postInspection(t.Context(), "C1", "100.000", "U9")

	posts := srv.ephemeralBodies()
	require.Len(t, posts, 2, "header + marker + entries overflow one block budget")
	for _, body := range posts {
		blocks, ok := body["blocks"].([]any)
		require.True(t, ok)
		require.LessOrEqual(t, len(blocks), maxActivityBlocks)
	}
}

func TestInspectionBlocks_TurnMarkersAndDropNote(t *testing.T) {
	entries := []toolLogEntry{
		{turn: 3, md: "🔧 *`a`*"},
		{turn: 3, md: "↳ *`a`* result"},
		{turn: 4, md: "🔧 *`b`*"},
	}
	blocks := inspectionBlocks(entries, 7)
	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	s := string(raw)
	require.Contains(t, s, "turn 3")
	require.Contains(t, s, "turn 4")
	require.Contains(t, s, "7 earlier ones were dropped")
	// header + 2 turn markers + 3 entries
	require.Len(t, blocks, 6)
}

// inspectFakeSlack records chat.postEphemeral bodies (blocks included) and
// counts any other Web API post, so tests can assert nothing lands in the
// thread itself.
type inspectFakeSlack struct {
	mu         sync.Mutex
	ephemerals []map[string]any
	posts      atomic.Int32
}

func (f *inspectFakeSlack) ephemeralBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.ephemerals))
	copy(out, f.ephemerals)
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
	p.User.ID = "U9"
	p.Channel.ID = "C1"
	p.Message.ThreadTS = threadTS
	p.Message.TS = ts
	return p
}
