package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// upper names a user by upper-casing the ID, so a test can tell a resolved
// name from a raw one at a glance.
func upper(id string) string { return strings.ToUpper("name-" + id) }

func TestRenderThreadContext_LabelsAndOrdersTheThread(t *testing.T) {
	got := renderThreadContext([]threadMessage{
		{TS: "1700000000.000100", User: "u1", Text: "first"},
		{TS: "1700000060.000100", User: "u2", Text: "second"},
	}, "1700000120.000100", "UBOT", "Jose", upper)

	require.Equal(t, strings.Join([]string{
		"[thread context shared by Jose: 2 earlier messages in this thread, oldest first]",
		"2023-11-14 22:13 NAME-U1: first",
		"2023-11-14 22:14 NAME-U2: second",
	}, "\n"), got)
}

func TestRenderThreadContext_OneMessageReadsAsOne(t *testing.T) {
	got := renderThreadContext([]threadMessage{{TS: "1700000000.000100", User: "u1", Text: "only"}},
		"1700000120.000100", "UBOT", "Jose", upper)
	require.Contains(t, got, "1 earlier message in this thread")
}

// The opener and anything after it are the conversation, not its context.
func TestRenderThreadContext_SkipsTheOpenerAndWhatFollows(t *testing.T) {
	got := renderThreadContext([]threadMessage{
		{TS: "100.000", User: "u1", Text: "before"},
		{TS: "200.000", User: "u1", Text: "the opener"},
		{TS: "300.000", User: "u1", Text: "after"},
	}, "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "before")
	require.NotContains(t, got, "the opener")
	require.NotContains(t, got, "after")
}

// The gateway's own posts and the channel events without words are not
// transcript; a thread of nothing else renders as no context at all.
func TestRenderThreadContext_SkipsOwnPostsAndContentlessSubtypes(t *testing.T) {
	got := renderThreadContext([]threadMessage{
		{TS: "100.000", User: "UBOT", Text: "consent prompt"},
		{TS: "101.000", User: "u1", SubType: "channel_join", Text: "has joined the channel"},
		{TS: "102.000", User: "u1", SubType: "message_changed", Text: "edited"},
	}, "200.000", "UBOT", "Jose", upper)

	require.Empty(t, got)
}

// An alert's words live in its attachment and its blocks, not in its text.
func TestRenderThreadContext_FlattensAttachmentsAndBlocks(t *testing.T) {
	got := renderThreadContext([]threadMessage{{
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
	}}, "200.000", "UBOT", "Jose", upper)

	for _, want := range []string{"PagerDuty: TRIGGERED #4412", "pods are crashlooping", "severity: critical",
		"Incident", "runbook: restart it", "cluster: graveler", "muted footer", "nested run", "[file: report.pdf]"} {
		require.Contains(t, got, want)
	}
}

// A human message carries its own words in both text and a rich_text block;
// the transcript says them once.
func TestRenderThreadContext_DoesNotRepeatBlockTextAlreadyInTheMessage(t *testing.T) {
	got := renderThreadContext([]threadMessage{{
		TS: "100.000", User: "u1", Text: "the pod restarts",
		Blocks: []threadBlock{{Type: "rich_text", Elements: []threadBlockElem{
			{Type: "rich_text_section", Elements: []threadBlockElem{{Type: "text", Text: "the pod restarts"}}},
		}}},
	}}, "200.000", "UBOT", "Jose", upper)

	require.Equal(t, 1, strings.Count(got, "the pod restarts"))
}

func TestRenderThreadContext_ReplacesMentionsWithNames(t *testing.T) {
	got := renderThreadContext([]threadMessage{
		{TS: "100.000", User: "u1", Text: "<@U9> looked at it, see <https://example.com|the graph>"},
	}, "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "NAME-U9 looked at it")
	require.Contains(t, got, "<https://example.com|the graph>", "links stay as Slack wrote them")
}

// A bot's post is named by the identity it posted under; a bot with neither
// falls back to its profile name.
func TestRenderThreadContext_NamesBotAuthors(t *testing.T) {
	got := renderThreadContext([]threadMessage{
		{TS: "100.000", BotID: "B1", Username: "PagerDuty", Text: "one"},
		{TS: "101.000", BotID: "B2", Text: "two", BotProfile: struct {
			Name string `json:"name"`
		}{Name: "Grafana"}},
	}, "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "PagerDuty: one")
	require.Contains(t, got, "Grafana: two")
}

func TestRenderThreadContext_MultilineTextIsIndented(t *testing.T) {
	got := renderThreadContext([]threadMessage{{TS: "100.000", User: "u1", Text: "line one\nline two"}},
		"200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "NAME-U1: line one\n  line two")
}

// Past the message cap the oldest go and the root stays: the root is the alert
// the thread is about.
func TestRenderThreadContext_MessageCapKeepsTheRootAndTheNewest(t *testing.T) {
	var msgs []threadMessage
	for i := range 70 {
		msgs = append(msgs, threadMessage{TS: fmt.Sprintf("%d.000", 100+i), User: "u1", Text: fmt.Sprintf("m%d", i)})
	}
	got := renderThreadContext(msgs, "900.000", "UBOT", "Jose", upper)

	lines := strings.Split(got, "\n")
	require.Len(t, lines, threadContextMaxMessages+1)
	require.Contains(t, lines[0], "70 earlier messages in this thread, the last 40 shown")
	require.Contains(t, lines[1], "m0", "the root survives the cap")
	require.Contains(t, got, "m69")
	require.NotContains(t, got, "m1 ")
}

// The character cap trims from the oldest too, and the root is the last line
// standing.
func TestRenderThreadContext_CharCapTrimsFromTheOldest(t *testing.T) {
	long := strings.Repeat("x", 4000)
	var msgs []threadMessage
	for i := range 5 {
		msgs = append(msgs, threadMessage{TS: fmt.Sprintf("%d.000", 100+i), User: "u1", Text: fmt.Sprintf("m%d ", i) + long})
	}
	got := renderThreadContext(msgs, "900.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "5 earlier messages in this thread, the last 2 shown")
	require.Contains(t, got, "m0 ", "the root survives the cap")
	require.Contains(t, got, "m4 ", "the newest survives")
	require.NotContains(t, got, "m2 ")
}

// A single root longer than the whole budget is cut, never dropped.
func TestRenderThreadContext_OversizedRootIsCut(t *testing.T) {
	got := renderThreadContext([]threadMessage{{TS: "100.000", User: "u1", Text: strings.Repeat("y", 20000)}},
		"900.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "NAME-U1: yyy")
	require.Less(t, len(got), threadContextMaxChars+200)
	require.True(t, strings.HasSuffix(got, "…"))
}

func TestRenderThreadContext_EmptyThreadRendersNothing(t *testing.T) {
	require.Empty(t, renderThreadContext(nil, "200.000", "UBOT", "Jose", upper))
}

// A message with no words at all (an image post with no caption is already
// covered by its file name; this is a message with neither) leaves no line.
func TestRenderThreadContext_MessageWithoutWordsIsSkipped(t *testing.T) {
	got := renderThreadContext([]threadMessage{
		{TS: "100.000", User: "u1"},
		{TS: "101.000", User: "u1", Text: "something"},
	}, "200.000", "UBOT", "Jose", upper)

	require.Contains(t, got, "1 earlier message")
	require.Contains(t, got, "something")
}

func TestThreadContextBlock_CountsWhatItKnows(t *testing.T) {
	_, ok := threadContextBlock(askAgentRequest{})
	require.False(t, ok, "no thread, no checkbox")

	block, ok := threadContextBlock(askAgentRequest{Thread: "100.000", ThreadSize: 7})
	require.True(t, ok)
	require.Equal(t, "Include the 7 earlier messages in this thread", checkboxLabel(block))

	block, _ = threadContextBlock(askAgentRequest{Thread: "100.000", ThreadSize: 1})
	require.Equal(t, askAgentContextOptionOne, checkboxLabel(block))

	block, _ = threadContextBlock(askAgentRequest{Thread: "100.000"})
	require.Equal(t, askAgentContextOptionNoCount, checkboxLabel(block),
		"a count that could not be read leaves the checkbox unnumbered, not unoffered")
}

// checkboxLabel digs the single option's text out of a context checkbox block.
func checkboxLabel(block map[string]any) string {
	element := block[bkElement].(map[string]any)
	option := element[bkOptions].([]any)[0].(map[string]any)
	return option[bkText].(map[string]any)[bkText].(string)
}
