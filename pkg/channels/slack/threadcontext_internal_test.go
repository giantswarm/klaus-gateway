package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// upper names a user by upper-casing the ID, so a test can tell a resolved
// name from a raw one at a glance.
func upper(id string) string { return strings.ToUpper("name-" + id) }

// fullRead is a read that reached the end of the thread, the ordinary case;
// the tests that care about a read the page bound cut short build their
// threadRead by hand.
func fullRead(msgs []threadMessage) threadRead {
	return threadRead{Messages: msgs, Total: len(msgs), Complete: true}
}

func TestRenderThreadContext_LabelsAndOrdersTheThread(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{
		{TS: "1700000000.000100", User: "u1", Text: "first"},
		{TS: "1700000060.000100", User: "u2", Text: "second"},
	}), "1700000120.000100", "UBOT", "Jose", upper)

	require.Equal(t, strings.Join([]string{
		"[thread context shared by Jose: 2 earlier messages in this thread, oldest first]",
		"2023-11-14 22:13 NAME-U1: first",
		"2023-11-14 22:14 NAME-U2: second",
	}, "\n"), got)
}

func TestRenderThreadContext_OneMessageReadsAsOne(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{{TS: "1700000000.000100", User: "u1", Text: "only"}}),
		"1700000120.000100", "UBOT", "Jose", upper)
	require.Contains(t, got, "1 earlier message in this thread")
}

// The opener and anything after it are the conversation, not its context.
func TestRenderThreadContext_SkipsTheOpenerAndWhatFollows(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{
		{TS: "100.000", User: "u1", Text: "before"},
		{TS: "200.000", User: "u1", Text: "the opener"},
		{TS: "300.000", User: "u1", Text: "after"},
	}), "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "before")
	require.NotContains(t, got, "the opener")
	require.NotContains(t, got, "after")
}

// The gateway's own posts and the channel events without words are not
// transcript; a thread of nothing else renders as no context at all.
func TestRenderThreadContext_SkipsOwnPostsAndContentlessSubtypes(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{
		{TS: "100.000", User: "UBOT", Text: "consent prompt"},
		{TS: "101.000", User: "u1", SubType: "channel_join", Text: "has joined the channel"},
		{TS: "102.000", User: "u1", SubType: "message_changed", Text: "edited"},
	}), "200.000", "UBOT", "Jose", upper)

	require.Empty(t, got)
}

// An alert's words live in its attachment and its blocks, not in its text.
func TestRenderThreadContext_FlattensAttachmentsAndBlocks(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{{
		TS:       "100.000",
		BotID:    "B1",
		Username: "PagerDuty",
		SubType:  "bot_message",
		Attachments: []threadAttachment{{
			Title: "TRIGGERED #4412",
			Text:  "pods are crashlooping",
			Fields: []struct {
				Title string `json:"title"`
				Value string `json:"value"`
			}{{Title: "severity", Value: "critical"}},
		}},
		Blocks: []threadBlock{
			{Type: "header", Text: &threadBlockText{Text: "Incident"}},
			{Type: bkSection, Text: &threadBlockText{Text: "runbook: restart it"}, Fields: []threadBlockText{{Text: "cluster: graveler"}}},
			{Type: bkContext, Elements: []threadBlockElem{{Type: "mrkdwn", Text: "muted footer"}}},
			{Type: "rich_text", Elements: []threadBlockElem{{Type: "rich_text_section", Elements: []threadBlockElem{{Type: "text", Text: "nested run"}}}}},
			{Type: "divider"},
		},
		Files: []struct {
			Name string `json:"name"`
		}{{Name: "report.pdf"}},
	}}), "200.000", "UBOT", "Jose", upper)

	for _, want := range []string{"PagerDuty: TRIGGERED #4412", "pods are crashlooping", "severity: critical",
		"Incident", "runbook: restart it", "cluster: graveler", "muted footer", "nested run", "[file: report.pdf]"} {
		require.Contains(t, got, want)
	}
}

// A human message carries its own words in both text and a rich_text block;
// the transcript says them once.
func TestRenderThreadContext_DoesNotRepeatBlockTextAlreadyInTheMessage(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{{
		TS: "100.000", User: "u1", Text: "the pod restarts",
		Blocks: []threadBlock{{Type: "rich_text", Elements: []threadBlockElem{
			{Type: "rich_text_section", Elements: []threadBlockElem{{Type: "text", Text: "the pod restarts"}}},
		}}},
	}}), "200.000", "UBOT", "Jose", upper)

	require.Equal(t, 1, strings.Count(got, "the pod restarts"))
}

func TestRenderThreadContext_ReplacesMentionsWithNames(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{
		{TS: "100.000", User: "u1", Text: "<@U9> looked at it, see <https://example.com|the graph>"},
	}), "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "NAME-U9 looked at it")
	require.Contains(t, got, "<https://example.com|the graph>", "links stay as Slack wrote them")
}

// A bot's post is named by the identity it posted under; a bot with neither
// falls back to its profile name.
func TestRenderThreadContext_NamesBotAuthors(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{
		{TS: "100.000", BotID: "B1", Username: "PagerDuty", Text: "one"},
		{TS: "101.000", BotID: "B2", Text: "two", BotProfile: struct {
			Name string `json:"name"`
		}{Name: "Grafana"}},
	}), "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "PagerDuty: one")
	require.Contains(t, got, "Grafana: two")
}

func TestRenderThreadContext_MultilineTextIsIndented(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{{TS: "100.000", User: "u1", Text: "line one\nline two"}}),
		"200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "NAME-U1: line one\n  line two")
}

// There is no message cap: a thread of many short messages is handed over
// whole, because it fits the only cap there is.
func TestRenderThreadContext_ManyShortMessagesAllFit(t *testing.T) {
	var msgs []threadMessage
	for i := range 70 {
		msgs = append(msgs, threadMessage{TS: fmt.Sprintf("%d.000", 100+i), User: "u1", Text: fmt.Sprintf("m%d", i)})
	}
	got := renderThreadContext(fullRead(msgs), "900.000", "UBOT", "Jose", upper)

	lines := strings.Split(got, "\n")
	require.Len(t, lines, 71, "the label and every message")
	require.Equal(t, "[thread context shared by Jose: 70 earlier messages in this thread, oldest first]", lines[0])
	require.Contains(t, got, "m0")
	require.Contains(t, got, "m69")
}

// Past the character cap the oldest messages go and the root stays: the root
// is the alert the thread is about.
func TestRenderThreadContext_CharCapTrimsFromTheOldest(t *testing.T) {
	long := strings.Repeat("x", 4000)
	var msgs []threadMessage
	for i := range 5 {
		msgs = append(msgs, threadMessage{TS: fmt.Sprintf("%d.000", 100+i), User: "u1", Text: fmt.Sprintf("m%d ", i) + long})
	}
	got := renderThreadContext(fullRead(msgs), "900.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "5 earlier messages in this thread, the most recent 12,000 characters shown")
	require.Contains(t, got, "m0 ", "the root survives the cap")
	require.Contains(t, got, "m4 ", "the newest survives")
	require.NotContains(t, got, "m2 ")
}

// A single root longer than the whole budget is cut, never dropped.
func TestRenderThreadContext_OversizedRootIsCut(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{{TS: "100.000", User: "u1", Text: strings.Repeat("y", 20000)}}),
		"900.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "NAME-U1: yyy")
	require.Less(t, len(got), threadContextMaxChars+200)
	require.True(t, strings.HasSuffix(got, "…"))
}

func TestRenderThreadContext_EmptyThreadRendersNothing(t *testing.T) {
	require.Empty(t, renderThreadContext(threadRead{Complete: true}, "200.000", "UBOT", "Jose", upper))
}

// A message with no words at all (an image post with no caption is already
// covered by its file name; this is a message with neither) leaves no line.
func TestRenderThreadContext_MessageWithoutWordsIsSkipped(t *testing.T) {
	got := renderThreadContext(fullRead([]threadMessage{
		{TS: "100.000", User: "u1"},
		{TS: "101.000", User: "u1", Text: "something"},
	}), "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "1 earlier message")
	require.Contains(t, got, "something")
}

func TestThreadContextBlock_OfferedOnlyWhereThereIsAThread(t *testing.T) {
	_, ok := threadContextBlock(askAgentRequest{})
	require.False(t, ok, "the slash command roots its own thread, so there is nothing to include")

	block, ok := threadContextBlock(askAgentRequest{Thread: "100.000"})
	require.True(t, ok)
	require.Equal(t, "Include the earlier messages in this thread", checkboxLabel(block),
		"no count: counting would mean reading the thread before the modal opens")
	require.Equal(t, true, block[bkOptional], "a cleared box must still submit")
	require.Len(t, block[bkElement].(map[string]any)[bkInitialOptions], 1, "checked by default")
}

// checkboxLabel digs the single option's text out of a context checkbox block.
func checkboxLabel(block map[string]any) string {
	element := block[bkElement].(map[string]any)
	option := element[bkOptions].([]any)[0].(map[string]any)
	return option[bkText].(map[string]any)[bkText].(string)
}

// A read the page bound stopped never claims its lines are the newest ones:
// they are neither the oldest nor the newest, and an agent told otherwise
// would take stale messages for the state of play. It says how far it got out
// of how long the thread is.
func TestRenderThreadContext_PartialReadSaysSo(t *testing.T) {
	msgs := []threadMessage{{TS: "100.000", User: "u1", Text: "somewhere in the middle"}}
	got := renderThreadContext(threadRead{Messages: msgs, Total: 1342}, "900.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "1 of 1,342 earlier messages read (the read stopped early)")
	require.NotContains(t, got, "in this thread,")
}

// A thread whose root reported no reply count still says the read stopped
// early, without inventing a total.
func TestRenderThreadContext_PartialReadWithoutATotal(t *testing.T) {
	msgs := []threadMessage{{TS: "100.000", User: "u1", Text: "somewhere in the middle"}}
	got := renderThreadContext(threadRead{Messages: msgs}, "900.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "1 earlier messages read (the read stopped early)")
	require.NotContains(t, got, " of ")
}

func TestThousands_GroupsDigits(t *testing.T) {
	require.Equal(t, "7", thousands(7))
	require.Equal(t, "999", thousands(999))
	require.Equal(t, "1,342", thousands(1342))
	require.Equal(t, "12,000", thousands(12000))
	require.Equal(t, "1,234,567", thousands(1234567))
}

// The character cap counts characters, not bytes: a transcript of accented
// text is not cut a third of the way early.
func TestRenderThreadContext_CharCapCountsRunes(t *testing.T) {
	// 5,000 three-byte runes: 15,000 bytes, well over the cap in bytes and
	// well under it in characters.
	got := renderThreadContext(fullRead([]threadMessage{{TS: "100.000", User: "u1", Text: strings.Repeat("→", 5000)}}),
		"900.000", "UBOT", "Jose", upper)

	require.Equal(t, 5000, strings.Count(got, "→"), "nothing is cut below the character cap")
}

// Past the read's budget an author is named by their ID, with no further
// lookup: one rate-limited users.info must not hold the first reply.
func TestDisplayName_PastTheBudgetIsTheUserID(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Marta"}}}`)
	}))
	defer srv.Close()
	a := &Adapter{APIBase: srv.URL, Secrets: Secrets{BotToken: "t"}, Logger: slog.New(slog.DiscardHandler)} //nolint:gosec // dummy test creds
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.Equal(t, "U9", a.displayName(ctx, "U9"))
	require.Zero(t, calls.Load(), "and nothing is asked of Slack")
}

// A name this process already holds costs no call, so the budget running out
// does not throw it away.
func TestDisplayName_CachedNameSurvivesTheBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"user":{"profile":{"display_name":"Marta"}}}`)
	}))
	defer srv.Close()
	a := &Adapter{APIBase: srv.URL, Secrets: Secrets{BotToken: "t"}, Logger: slog.New(slog.DiscardHandler)} //nolint:gosec // dummy test creds

	require.Equal(t, "Marta", a.displayName(t.Context(), "U9"))

	done, cancel := context.WithCancel(t.Context())
	cancel()
	require.Equal(t, "Marta", a.displayName(done, "U9"), "the cached name is used past the budget")
	require.Equal(t, int32(1), calls.Load(), "and asks Slack nothing more")
}

// A PagerDuty alert as Slack delivers it: a section whose fields are text
// objects, an actions block whose buttons carry their labels as text objects,
// and a context block with an image element beside an mrkdwn one. The buttons
// are not prose; everything else is.
func TestThreadMessage_DecodesPagerDutyBlocks(t *testing.T) {
	raw := `{"ts":"1.000","bot_id":"B1","username":"PagerDuty EU","text":"",
	 "blocks":[
	  {"type":"section","text":{"type":"mrkdwn","text":"*alba - FluxCustomerHelmReleaseFailed*: HelmRelease stuck in Failed state."},
	   "fields":[{"type":"mrkdwn","text":"*Urgency:* High"},{"type":"mrkdwn","text":"*Service:* honeybadger-alertmanager"}]},
	  {"type":"actions","elements":[
	    {"type":"button","text":{"type":"plain_text","text":"Reopen","emoji":true},"action_id":"reopen"},
	    {"type":"button","text":{"type":"plain_text","text":"Start Post-Incident Review"},"action_id":"pir"}]},
	  {"type":"context","elements":[
	    {"type":"image","image_url":"https://example.com/i.png","alt_text":"icon"},
	    {"type":"mrkdwn","text":"Triggered via Grafana"}]},
	  {"type":"divider"}
	 ]}`
	var m threadMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	got := strings.Join(messageExtras(m), "\n")
	require.Contains(t, got, "FluxCustomerHelmReleaseFailed")
	require.Contains(t, got, "*Urgency:* High")
	require.Contains(t, got, "Triggered via Grafana")
	require.NotContains(t, got, "Reopen", "button labels are not prose")
}
