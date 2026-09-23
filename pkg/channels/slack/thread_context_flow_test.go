package slack_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// A conversation that opens inside a thread other people wrote hands the agent
// what the thread already said. These flows drive the adapter through the
// signed endpoints and read the transcript off the message that reaches the
// gateway.

// replyMsg is one message of a thread as the fake serves it from
// conversations.replies.
type replyMsg struct {
	TS          string           `json:"ts"`
	User        string           `json:"user,omitempty"`
	BotID       string           `json:"bot_id,omitempty"`
	Username    string           `json:"username,omitempty"`
	SubType     string           `json:"subtype,omitempty"`
	Text        string           `json:"text,omitempty"`
	ReplyCount  int              `json:"reply_count,omitempty"`
	Attachments []map[string]any `json:"attachments,omitempty"`
	Blocks      []map[string]any `json:"blocks,omitempty"`
	Files       []map[string]any `json:"files,omitempty"`
}

// withThread makes the fake serve msgs (oldest first) as the thread's
// conversations.replies view: pages of pageSize followed by next_cursor, with
// the thread's reply_count on the root of the first page, the way Slack
// reports it.
func (f *fakeSlackAPI) withThread(msgs []replyMsg, pageSize int) {
	f.setResponder("conversations.replies", func(params map[string]any) string {
		if len(msgs) == 0 {
			return `{"ok":true,"messages":[]}`
		}
		start := 0
		if c, ok := params["cursor"].(string); ok && c != "" {
			start, _ = strconv.Atoi(c)
		}
		end := min(start+pageSize, len(msgs))
		next := ""
		if end < len(msgs) {
			next = strconv.Itoa(end)
		}
		page := append([]replyMsg(nil), msgs[start:end]...)
		if start == 0 && len(page) > 0 {
			// Slack reports the thread's reply count on its root, which is how
			// a read that stops early knows how much it did not reach.
			page[0].ReplyCount = len(msgs) - 1
		}
		return repliesJSON(page, next)
	})
}

func repliesJSON(msgs []replyMsg, nextCursor string) string {
	body := map[string]any{"ok": true, "messages": msgs}
	if nextCursor != "" {
		body["response_metadata"] = map[string]any{"next_cursor": nextCursor}
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// withUserNames makes users.info answer with a display name per user; a user
// not listed answers with an empty profile, and a user in fails is refused.
func (f *fakeSlackAPI) withUserNames(names map[string]string, fails map[string]bool) {
	f.setResponder("users.info", func(params map[string]any) string {
		user, _ := params["user"].(string)
		if fails[user] {
			return `{"ok":false,"error":"user_not_found"}`
		}
		raw, _ := json.Marshal(map[string]any{
			"ok":   true,
			"user": map[string]any{"profile": map[string]any{"display_name": names[user]}},
		})
		return string(raw)
	})
}

// alertThread is the shape the feature exists for: a bot alert whose words live
// in its attachment, two colleagues discussing under it, and one message the
// gateway itself posted, which is never part of the transcript.
func alertThread() []replyMsg {
	return []replyMsg{
		{TS: "100.000", BotID: "B1", Username: "PagerDuty", SubType: "bot_message", Attachments: []map[string]any{{
			"title":  "TRIGGERED #4412 KubePodCrashLooping",
			"text":   "klaus-gateway is crashlooping on graveler",
			"fields": []map[string]any{{"title": "severity", "value": "critical"}},
		}}},
		{TS: "101.000", User: "U2", Text: "the pod restarts every 40 s"},
		{TS: "102.000", User: "UBOT", Text: "_I couldn't find our earlier conversation._"},
		{TS: "103.000", User: "U3", Text: "logs say\nflag provided but not defined"},
	}
}

// sendAskAgentSubmissionWithContext posts a view_submission of the picker with
// the thread-context checkbox in the given state, the way Slack reports it:
// selected_options carries the option when the box is ticked and is empty when
// the person cleared it.
func sendAskAgentSubmissionWithContext(t *testing.T, srv *httptest.Server, user, privateMetadata, agentRef, question string, include bool) {
	t.Helper()
	selected := []any{}
	if include {
		selected = append(selected, map[string]any{"value": "include"})
	}
	inner := map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": user},
		"view": map[string]any{
			"id":               "V1",
			"callback_id":      "ask_agent",
			"private_metadata": privateMetadata,
			"state": map[string]any{"values": map[string]any{
				"ask_agent_agent":    map[string]any{"agent": map[string]any{"type": "static_select", "selected_option": map[string]any{"value": agentRef}}},
				"ask_agent_question": map[string]any{"question": map[string]any{"type": "plain_text_input", "value": question}},
				"ask_agent_context":  map[string]any{"context": map[string]any{"type": "checkboxes", "selected_options": selected}},
			}},
		},
	}
	data, err := json.Marshal(inner)
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
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// The shortcut on an alert thread, checkbox left ticked: the agent's turn
// carries every message written before the echo, oldest first, with the
// alert's attachment flattened, the authors named, and the gateway's own post
// left out.
func TestThreadContext_ShortcutSharesTheThread(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 200)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta", "U3": "Piotr"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "103.000", "100.000", api.URL+"/response_url")
	pmRaw := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmissionWithContext(t, srv, "U1", pmRaw, "kagent/sre-agent", "which release introduced it?", true)
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	lines := strings.Split(got, "\n")
	require.Equal(t, "[thread context shared by Jose: 3 earlier messages in this thread, oldest first]", lines[0])
	require.Contains(t, got, "PagerDuty: TRIGGERED #4412 KubePodCrashLooping")
	require.Contains(t, got, "klaus-gateway is crashlooping on graveler", "the alert's attachment text is flattened")
	require.Contains(t, got, "severity: critical", "attachment fields are flattened")
	require.Contains(t, got, "Marta: the pod restarts every 40 s")
	require.Contains(t, got, "Piotr: logs say\n  flag provided but not defined", "a multi-line message keeps its breaks, indented")
	require.NotContains(t, got, "I couldn't find our earlier conversation", "the gateway's own posts are not transcript")
	require.Less(t, strings.Index(got, "Marta"), strings.Index(got, "Piotr"), "oldest first")
}

// The same shortcut with the box cleared: nothing of the thread is shared, and
// the submission reads no history at all — only the picker's own count call
// was made, before the person decided.
func TestThreadContext_ShortcutCheckboxOffSharesNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 200)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "103.000", "100.000", api.URL+"/response_url")
	pmRaw := openedView(t, fake)["private_metadata"].(string)
	countCalls := len(fake.pathCalls("conversations.replies"))
	sendAskAgentSubmissionWithContext(t, srv, "U1", pmRaw, "kagent/sre-agent", "why?", false)
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	require.Empty(t, dispatched()[0].Context, "a cleared box shares nothing")
	require.Len(t, fake.pathCalls("conversations.replies"), countCalls, "the thread is not read on submit")
}

// The picker offers the thread's messages with a checkbox, ticked, and opens
// without reading anything: the trigger it opens on dies after three seconds,
// so nothing that can be slow runs before views.open.
func TestThreadContext_PickerOffersTheCheckbox(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 50)
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "103.000", "100.000", api.URL+"/response_url")

	blocks := openedView(t, fake)["blocks"].([]any)
	require.Len(t, blocks, 3, "agent, question, thread context")
	block := blocks[2].(map[string]any)
	require.Equal(t, "ask_agent_context", block["block_id"])
	require.Equal(t, true, block["optional"], "a cleared box must still submit")
	element := block["element"].(map[string]any)
	require.Equal(t, "checkboxes", element["type"])
	option := element["options"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"]
	require.Equal(t, "Include the earlier messages in this thread", option)
	require.Len(t, element["initial_options"], 1, "checked by default")
	require.Empty(t, fake.pathCalls("conversations.replies"), "the picker reads no thread to open")
}

// The slash command roots its own thread, so there is nothing earlier to
// share: no checkbox, no read, no context.
func TestThreadContext_SlashCommandHasNoThreadToShare(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 200)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	require.Equal(t, http.StatusOK, sendSlashCommand(t, srv, "C1", "U1", "why are pods crashlooping?", api.URL+"/response_url"))
	pmRaw := openedView(t, fake)["private_metadata"].(string)
	require.Len(t, openedView(t, fake)["blocks"].([]any), 2, "no context checkbox without a thread")

	sendAskAgentSubmission(t, srv, "U1", pmRaw, "kagent/sre-agent", "why are pods crashlooping?")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	require.Empty(t, dispatched()[0].Context)
	require.Empty(t, fake.pathCalls("conversations.replies"), "a thread the command rooted itself is not read")
}

// `/agent <name> <question>` typed as a reply under an alert opens the
// conversation in that thread, and the agent gets the thread with it. There is
// no modal here, so there is no checkbox: the person chose to ask in front of
// everyone in the thread.
func TestThreadContext_AgentSelectionReplySharesTheThread(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 200)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta", "U3": "Piotr"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "/agent \"SRE Agent\" what happened?", "104.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	msg := dispatched()[0]
	require.True(t, msg.Opener)
	require.Contains(t, msg.Context, "[thread context shared by Jose:")
	require.Contains(t, msg.Context, "Marta: the pod restarts every 40 s")
}

// A bare mention under an alert opens a conversation with the default agent,
// and has the same need: the thread comes with it.
func TestThreadContext_BareMentionReplySharesTheThread(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 200)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta", "U3": "Piotr"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "104.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	require.Contains(t, dispatched()[0].Context, "Marta: the pod restarts every 40 s")
}

// The second message of a conversation is a turn of it already: the thread is
// read once, at the opener, and never again.
func TestThreadContext_LaterRepliesDoNotReadAgain(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 200)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	a, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "104.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)
	reads := len(fake.pathCalls("conversations.replies"))
	require.Positive(t, reads)
	waitThreadIdle(t, a, "100.000")

	sendEvent(t, srv, mention("U1", "and the release?", "105.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 50*time.Millisecond)

	require.Empty(t, dispatched()[1].Context, "a turn inside the conversation carries no transcript")
	require.Len(t, fake.pathCalls("conversations.replies"), reads, "and reads nothing")
}

// There is no message cap: a thread of seventy short messages is handed over
// whole, because it fits the only cap there is.
func TestThreadContext_ManyShortMessagesAllFit(t *testing.T) {
	msgs := []replyMsg{{TS: "100.000", User: "U2", Text: "the alert that started it"}}
	for i := 1; i < 70; i++ {
		msgs = append(msgs, replyMsg{TS: fmt.Sprintf("%d.000", 100+i), User: "U2", Text: fmt.Sprintf("line %d", i)})
	}
	fake := newFakeSlackAPI()
	fake.withThread(msgs, 50)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "900.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	lines := strings.Split(got, "\n")
	require.Equal(t, "[thread context shared by Jose: 70 earlier messages in this thread, oldest first]", lines[0])
	require.Len(t, lines, 71)
	require.Contains(t, got, "the alert that started it")
	require.Contains(t, got, "line 69")
}

// A thread of long messages is cut to the character cap: the oldest go and the
// root stays, because the root is the alert the thread is about.
func TestThreadContext_LongMessagesAreCutFromTheOldest(t *testing.T) {
	long := strings.Repeat("x", 3000)
	msgs := []replyMsg{{TS: "100.000", User: "U2", Text: "the alert that started it " + long}}
	for i := 1; i < 8; i++ {
		msgs = append(msgs, replyMsg{TS: fmt.Sprintf("%d.000", 100+i), User: "U2", Text: fmt.Sprintf("line %d ", i) + long})
	}
	fake := newFakeSlackAPI()
	fake.withThread(msgs, 50)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "900.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	lines := strings.Split(got, "\n")
	require.Equal(t, "[thread context shared by Jose: 8 earlier messages in this thread, the most recent 12,000 characters shown]", lines[0])
	require.Contains(t, got, "the alert that started it", "the root is always kept")
	require.Contains(t, got, "line 7 ", "the newest are the ones kept")
	require.NotContains(t, got, "line 1 ", "the oldest are the ones dropped")
}

// A thread whose replies span several pages is read to the end, not to the
// first page.
func TestThreadContext_PagesThroughTheWholeThread(t *testing.T) {
	msgs := []replyMsg{{TS: "100.000", User: "U2", Text: "root of it all"}}
	for i := 1; i < 12; i++ {
		msgs = append(msgs, replyMsg{TS: fmt.Sprintf("%d.000", 100+i), User: "U2", Text: fmt.Sprintf("line %d", i)})
	}
	fake := newFakeSlackAPI()
	fake.withThread(msgs, 5) // three pages
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "900.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	require.Contains(t, got, "root of it all")
	require.Contains(t, got, "line 11", "the last page is read too")
	require.Contains(t, got, "12 earlier messages")
}

// Slack refusing the read never blocks the turn: it runs without the
// transcript, and the person who opened the conversation is told why.
func TestThreadContext_ReadFailureRunsTheTurnAndNotifies(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("conversations.replies", "missing_scope")
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "104.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	require.Empty(t, dispatched()[0].Context)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "I couldn't read the earlier messages in this thread (`missing_scope`)")
	}, flowWait, 20*time.Millisecond, "the initiator is told the agent only sees their question")
}

// One author whose profile cannot be read is named by their ID; everyone else
// still reads as a name.
func TestThreadContext_UnresolvableAuthorFallsBackToTheID(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 200)
	fake.withUserNames(map[string]string{"U1": "Jose", "U3": "Piotr"}, map[string]bool{"U2": true})
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "104.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	require.Contains(t, got, "U2: the pod restarts every 40 s", "the unreadable author keeps their ID")
	require.Contains(t, got, "Piotr: logs say", "the others still resolve")
}

// The whole transcript costs one budget, the name lookups included: a
// users.info that never answers ends with the budget, and the authors it could
// not name keep their IDs.
func TestThreadContext_HangingNameLookupEndsWithTheBudget(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 50)
	// The two authors of the thread never get a name: only a context that ends
	// releases their lookup. Every other users.info (the bot's own identity,
	// the sender's email) answers at once, as Slack would.
	fake.setDelayIf(func(path string, params map[string]any) time.Duration {
		if path == "users.info" && (params["user"] == "U2" || params["user"] == "U3") {
			return time.Minute
		}
		return 0
	})
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "104.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond,
		"the turn runs once the read's budget ends, not once Slack answers")

	got := dispatched()[0].Context
	require.Contains(t, got, "U2: the pod restarts every 40 s", "an author nobody could name keeps their ID")
	require.Contains(t, got, "U3: logs say")
	require.Positive(t, userLookups(fake, "U2"), "the first author's lookup is what runs the budget out")
	require.Zero(t, userLookups(fake, "U3"), "every author after it is named by their ID without asking Slack")
}

// A thread longer than the read may page through is handed over as what it is:
// part of a longer thread, with how far the read got and how long the thread
// is, never as its newest messages.
func TestThreadContext_ThreadTooLongToReadIsLabelledPartial(t *testing.T) {
	var msgs []replyMsg
	for i := range 1300 {
		msgs = append(msgs, replyMsg{TS: fmt.Sprintf("%d.000", 1000+i), User: "U2", Text: fmt.Sprintf("line %d", i)})
	}
	fake := newFakeSlackAPI()
	fake.withThread(msgs, 50) // 26 pages; the read stops at 10
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "9000.000", "1000.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	lines := strings.Split(got, "\n")
	require.Equal(t, "[thread context shared by Jose: 500 of 1,300 earlier messages read (the read stopped early), "+
		"the most recent 12,000 characters shown]", lines[0])
	require.Contains(t, lines[1], "line 0", "the root is kept whatever the read reached")
	require.Len(t, fake.pathCalls("conversations.replies"), 10, "paging is bounded")
}

// A thread the read CAN reach the end of counts the whole thread, and its
// newest messages are the ones the character cap keeps.
func TestThreadContext_LongButReadableThreadIsCountedInFull(t *testing.T) {
	var msgs []replyMsg
	for i := range 300 {
		msgs = append(msgs, replyMsg{TS: fmt.Sprintf("%d.000", 1000+i), User: "U2", Text: fmt.Sprintf("line %d ", i) + strings.Repeat("y", 60)})
	}
	fake := newFakeSlackAPI()
	fake.withThread(msgs, 50)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "9000.000", "1000.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	require.Contains(t, got, "300 earlier messages in this thread, the most recent 12,000 characters shown")
	require.Contains(t, got, "line 299 ", "the newest are the ones kept")
	require.Contains(t, got, "line 0 ", "the root is always kept")
	require.NotContains(t, got, "line 5 ")
}

// The assistant pane roots every chat at an anchor of Slack's own, so the
// first message of a chat is never its own thread root — and reading that
// thread would only hand the person their own words back. Nothing is read.
func TestThreadContext_AssistantPaneIsNotRead(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 50)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL)

	sendEvent(t, srv, dmThreadEvent("U1", "what can you do?", "300.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	require.True(t, dispatched()[0].Opener)
	require.Empty(t, dispatched()[0].Context)
	require.Empty(t, fake.pathCalls("conversations.replies"), "a DM chat has no thread that predates it")
}

// userLookups counts the users.info calls made for one Slack user.
func userLookups(fake *fakeSlackAPI, user string) int {
	n := 0
	for _, c := range fake.pathCalls("users.info") {
		if c.params["user"] == user {
			n++
		}
	}
	return n
}

// The shortcut works in a DM when DMs are served, and there the thread is the
// chat itself: the only messages before the opener are the person's own and
// the bot's. Nothing is offered and nothing is read — the same rule the typed
// entry points follow.
func TestThreadContext_ShortcutInADMOffersNothingAndReadsNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(alertThread(), 50)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	// DMs served and channels served: the shipped default, and the only shape
	// in which the shortcut reaches a DM at all.
	_, srv := newEventsAdapter(t, gw, api.URL, withSelection(pickerRoster(), pickerCards()),
		func(a *slackadapter.Adapter) { a.ChannelMode = slackadapter.ChannelModeAll })

	sendAskAgentShortcut(t, srv, "D1", "U1", "103.000", "100.000", api.URL+"/response_url")

	view := openedView(t, fake)
	require.Len(t, view["blocks"].([]any), 2, "no context checkbox in a DM")

	sendAskAgentSubmissionWithContext(t, srv, "U1", view["private_metadata"].(string), "kagent/sre-agent", "what happened?", true)
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	require.Empty(t, dispatched()[0].Context)
	require.Empty(t, fake.pathCalls("conversations.replies"), "a DM chat has no thread that predates it")
}

// A page failing in the middle of a read leaves the turn with what was read,
// labelled as partial — and the reason in the log, so a rate limit is not
// mistaken for the read's own page bound.
func TestThreadContext_PageFailureMidReadKeepsWhatWasRead(t *testing.T) {
	var msgs []replyMsg
	for i := range 120 {
		msgs = append(msgs, replyMsg{TS: fmt.Sprintf("%d.000", 1000+i), User: "U2", Text: fmt.Sprintf("line %d", i)})
	}
	fake := newFakeSlackAPI()
	fake.withThread(msgs, 50)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	// Everything past the first page is refused.
	fake.failIf = func(path string, params map[string]any) string {
		if path == "conversations.replies" && params["cursor"] != nil && params["cursor"] != "" {
			return "ratelimited"
		}
		return ""
	}
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "what happened here?", "9000.000", "1000.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	require.Contains(t, got, "50 of 120 earlier messages read (the read stopped early)")
	require.Contains(t, got, "line 0", "what was read is kept")
	require.Empty(t, allText(fake.pathCalls("chat.postEphemeral")), "a partial read is not a failed one: nobody is told")
}

// One field of one message in a shape the decoder did not expect — here the
// root's files as a string and a section's fields as a string — costs that
// field, not the thread: encoding/json fills every other field and reports the
// mismatch at the end, and the read keeps the page.
func TestThreadContext_UnexpectedFieldShapeKeepsTheThread(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setResponder("conversations.replies", func(map[string]any) string {
		return `{"ok":true,"messages":[
		 {"ts":"100.000","bot_id":"B1","username":"PagerDuty","subtype":"bot_message","text":"TRIGGERED #4412","files":"none",
		  "blocks":[{"type":"section","text":{"type":"mrkdwn","text":"pod crashlooping"},"fields":"oops"}],"reply_count":1},
		 {"ts":"101.000","user":"U2","text":"restarts every 40 s"}]}`
	})
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "103.000", "100.000", api.URL+"/response_url")
	pmRaw := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmissionWithContext(t, srv, "U1", pmRaw, "kagent/sre-agent", "which release introduced it?", true)
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	got := dispatched()[0].Context
	require.Equal(t, "[thread context shared by Jose: 2 earlier messages in this thread, oldest first]", strings.Split(got, "\n")[0])
	require.Contains(t, got, "TRIGGERED #4412")
	require.Contains(t, got, "pod crashlooping", "the section text beside the odd field is kept")
	require.Contains(t, got, "Marta: restarts every 40 s")
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "no failure notice: the read succeeded")
}
