package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// The "Ask an agent here" message shortcut opens the same picker the slash
// command opens, but inside the thread of the message it was invoked on: an
// alert thread or a running discussion is handed to a chosen agent without
// anyone leaving it. These flows drive the adapter through the signed
// interactions endpoint and observe the Slack Web API calls and what reaches
// the gateway.

// sendAskAgentShortcut posts a signed message_action payload, as Slack sends
// it when a user picks the shortcut on a message. threadTS is empty for a
// top-level message.
func sendAskAgentShortcut(t *testing.T, srv *httptest.Server, channel, user, ts, threadTS, responseURL string) {
	t.Helper()
	inner := map[string]any{
		"type":         "message_action",
		"callback_id":  "ask_agent_here",
		"trigger_id":   "123.456.abcdef",
		"response_url": responseURL,
		"user":         map[string]any{"id": user},
		"channel":      map[string]any{"id": channel},
		"message":      map[string]any{"ts": ts, "thread_ts": threadTS},
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

// Invoked on a reply inside a thread nobody has bound, the shortcut carries
// that thread through the modal: the submission echoes the question as a reply
// in it, under the agent's identity, and runs the first turn there.
func TestAskAgentShortcut_StartsConversationInTheMessageThread(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "200.000", "100.000", api.URL+"/response_url")

	view := openedView(t, fake)
	var pm map[string]string
	require.NoError(t, json.Unmarshal([]byte(view["private_metadata"].(string)), &pm))
	require.Equal(t, "C1", pm["c"])
	require.Equal(t, "U1", pm["u"])
	require.Equal(t, "100.000", pm["t"], "the conversation starts in the invoked message's thread")
	require.Equal(t, "Start conversation", view["submit"].(map[string]any)["text"], "the shortcut's thread exists already")
	lead := view["blocks"].([]any)[0].(map[string]any)["elements"].([]any)[0].(map[string]any)["text"].(string)
	require.True(t, strings.HasPrefix(lead, "Continues this thread in <#C1> under the agent's name."), lead)
	question := view["blocks"].([]any)[2].(map[string]any)["element"].(map[string]any)
	_, prefilled := question["initial_value"]
	require.False(t, prefilled, "the shortcut has no question to prefill")

	sendAskAgentSubmission(t, srv, "U1", view["private_metadata"].(string), "kagent/sre-agent", "why are pods crashlooping?")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the submission dispatches the first turn")

	echo := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "C1", echo.params["channel"])
	require.Equal(t, "100.000", echo.params["thread_ts"], "the echo is a reply in the target thread")
	require.Equal(t, "SRE Agent", echo.params["username"], "posted under the agent's identity")
	requireQuestionMessage(t, echo.params, "why are pods crashlooping?", "U1")

	msgs := dispatched()
	require.Len(t, msgs, 1)
	require.Equal(t, "100.000", msgs[0].ThreadID, "the turn runs in the existing thread")
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef)
	require.Equal(t, "why are pods crashlooping?", msgs[0].Text)
	require.Equal(t, "U1", msgs[0].Subject)
	require.True(t, msgs[0].Opener, "the question opens the conversation")

	entry, ok, err := gw.rec().ThreadRecord(context.Background(), "slack", "C1", "100.000")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "kagent/sre-agent", entry.AgentRef, "the thread is bound to the chosen agent")
	require.Equal(t, "U1", entry.Initiator, "the submitter owns the thread")
}

// Invoked on a top-level message, the shortcut opens that message's own
// thread: the echo is its first reply.
func TestAskAgentShortcut_RootMessageOpensItsOwnThread(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "500.000", "", api.URL+"/response_url")

	pmRaw := openedView(t, fake)["private_metadata"].(string)
	var pm map[string]string
	require.NoError(t, json.Unmarshal([]byte(pmRaw), &pm))
	require.Equal(t, "500.000", pm["t"], "a root message's own ts is the thread")

	sendAskAgentSubmission(t, srv, "U1", pmRaw, "kagent/sre-agent", "what happened here?")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the submission dispatches the first turn")

	require.Equal(t, "500.000", fake.pathCalls("chat.postMessage")[0].params["thread_ts"])
	require.Equal(t, "500.000", dispatched()[0].ThreadID)
}

// A thread that already talks to an agent is refused before the picker opens:
// a second conversation in it would fork the one it has.
func TestAskAgentShortcut_ThreadWithAnAgentIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	require.NoError(t, gw.rec().UpdateThreadRecord(context.Background(), "slack", "C1", "100.000", func(e *store.Entry, _ bool) bool {
		e.AgentRef = "kagent/sre-agent"
		return true
	}))
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "200.000", "100.000", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "already talks to *SRE Agent*")
	require.Empty(t, fake.pathCalls("views.open"), "no picker in a thread that has its agent")
	require.Equal(t, 0, gw.dispatchCount())
}

// Without a caller identity the controller refuses the roster listing; the
// invoker is told to sign in rather than that the picker is broken.
func TestAskAgentShortcut_UnlinkedCallerIsAskedToSignIn(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	roster := &fakeRoster{err: pkga2a.ErrNoIdentity}
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, channelMode, withSelection(roster, pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "200.000", "100.000", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "`/login`")
	require.Empty(t, fake.pathCalls("views.open"))
}

// A thread can already have an owner and no agent — someone typed /usage or
// /stop there before any conversation. The shortcut is refused for anyone
// else before the picker opens: a conversation opened here would run under
// the owner's delegated identity, a decision the access prompt exists to take.
// Nothing is echoed and nothing is bound.
func TestAskAgentShortcut_ThreadOwnedBySomeoneElseIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	require.NoError(t, gw.rec().UpdateThreadRecord(context.Background(), "slack", "C1", "100.000", func(e *store.Entry, _ bool) bool {
		e.Initiator = "UA"
		return true
	}))
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "UB", "200.000", "100.000", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "belongs to <@UA>")
	require.Empty(t, fake.pathCalls("views.open"), "no picker in another person's thread")
	require.Equal(t, 0, gw.dispatchCount())
	entry, _, _ := gw.rec().ThreadRecord(context.Background(), "slack", "C1", "100.000")
	require.Equal(t, "UA", entry.Initiator)
	require.Empty(t, entry.AgentRef, "nothing is bound")
	require.Empty(t, entry.Granted, "nobody is granted behind the owner's back")
}

// The owner appeared between the picker opening and the submit (they typed a
// command in the thread meanwhile): the submission is refused the same way,
// before anything is echoed or bound.
func TestAskAgentShortcut_OwnedBetweenOpenAndSubmitIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "UB", "200.000", "100.000", api.URL+"/response_url")
	pmRaw := openedView(t, fake)["private_metadata"].(string)
	require.NoError(t, gw.rec().UpdateThreadRecord(context.Background(), "slack", "C1", "100.000", func(e *store.Entry, _ bool) bool {
		e.Initiator = "UA"
		return true
	}))

	sendAskAgentSubmission(t, srv, "UB", pmRaw, "kagent/sre-agent", "what is going on?")
	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "belongs to <@UA>")
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, fake.pathCalls("chat.postMessage"), "no echo is posted")
	require.Equal(t, 0, gw.dispatchCount(), "nothing runs")
	entry, _, _ := gw.rec().ThreadRecord(context.Background(), "slack", "C1", "100.000")
	require.Equal(t, "UA", entry.Initiator)
	require.Empty(t, entry.AgentRef, "nothing is bound")
}

// The thread was free when the picker opened and someone bound it before the
// submit: the submission is refused, nothing is echoed and nothing runs, so
// one thread never ends up with two conversations.
func TestAskAgentShortcut_BoundBetweenOpenAndSubmitIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "200.000", "100.000", api.URL+"/response_url")
	pmRaw := openedView(t, fake)["private_metadata"].(string)
	require.NoError(t, gw.rec().UpdateThreadRecord(context.Background(), "slack", "C1", "100.000", func(e *store.Entry, _ bool) bool {
		e.AgentRef = "kagent/sre-agent"
		return true
	}))

	sendAskAgentSubmission(t, srv, "U1", pmRaw, "kagent/sre-agent", "why are pods crashlooping?")
	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "already talks to *SRE Agent*")
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, fake.pathCalls("chat.postMessage"), "no echo is posted")
	require.Equal(t, 0, gw.dispatchCount(), "nothing runs")
}

// sendRosterSelect clicks a roster row's Select button: a block_actions
// payload from the roster message, which is a reply in threadTS.
func sendRosterSelect(t *testing.T, srv *httptest.Server, channel, user, threadTS, ref, responseURL string) {
	t.Helper()
	inner := map[string]any{
		"type":         "block_actions",
		"trigger_id":   "123.456.abcdef",
		"response_url": responseURL,
		"user":         map[string]any{"id": user},
		"channel":      map[string]any{"id": channel},
		"container":    map[string]any{"message_ts": "150.000"},
		"message":      map[string]any{"ts": "150.000", "thread_ts": threadTS},
		"actions":      []any{map[string]any{"action_id": "agent_select", "value": ref}},
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

// A bare /agent lists the agents as rows, the default first, each with a
// Select button; a click opens the picker for the roster's thread with that
// agent preselected.
func TestRoster_SelectOpensThePickerPreselected(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "<@UBOT> /agent", "100.000", ""))
	var blocks []any
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postMessage") {
			if b, ok := c.params["blocks"].([]any); ok && strings.Contains(allBlockText([]recordedCall{c}), "agent_select") {
				blocks = b
				require.Equal(t, "100.000", c.params["thread_ts"], "the roster is a reply in the command's thread")
				return true
			}
		}
		return false
	}, flowWait, 20*time.Millisecond, "the roster is posted as rows")

	var rows []string
	for _, b := range blocks {
		m := b.(map[string]any)
		if acc, ok := m["accessory"].(map[string]any); ok {
			require.Equal(t, "Select", acc["text"].(map[string]any)["text"])
			rows = append(rows, acc["value"].(string))
		}
	}
	require.Equal(t, []string{"kagent/swarmgeist", "kagent/grill-master", "kagent/sre-agent"}, rows,
		"the default first, then A–Z by display name")

	sendRosterSelect(t, srv, "C1", "U1", "100.000", "kagent/sre-agent", api.URL+"/response_url")
	view := openedView(t, fake)
	var pm map[string]string
	require.NoError(t, json.Unmarshal([]byte(view["private_metadata"].(string)), &pm))
	require.Equal(t, "100.000", pm["t"], "the conversation opens in the roster's thread")
	agentSelect := view["blocks"].([]any)[1].(map[string]any)["element"].(map[string]any)
	require.Equal(t, "kagent/sre-agent", agentSelect["initial_option"].(map[string]any)["value"], "the clicked agent is preselected")
}

// hasSelectRows reports whether a recorded post carries roster rows with
// Select buttons.
func hasSelectRows(c recordedCall) bool {
	return strings.Contains(allBlockText([]recordedCall{c}), `"agent_select"`)
}

// A failed selection posts its notice with the roster rows under it, in one
// message, so the person picks a real agent with one click.
func TestRoster_UnknownAgentNoticeCarriesTheRows(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", `<@UBOT> /agent "No Such Agent" hello`, "100.000", ""))
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postMessage") {
			text, _ := c.params["text"].(string)
			if strings.HasPrefix(text, "No agent named `No Such Agent` is available.") && hasSelectRows(c) {
				return true
			}
		}
		return false
	}, flowWait, 20*time.Millisecond, "the notice and the rows are one message")
}

// A bare /agent inside a thread that already has its conversation lists the
// agents without Select buttons: the picker would refuse every click there.
func TestRoster_BoundThreadListsWithoutButtons(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "<@UBOT> /agent sre-agent start here", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)

	sendEvent(t, srv, mention("U1", "<@UBOT> /agent", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postMessage") {
			if strings.Contains(allBlockText([]recordedCall{c}), "This thread already has its agent") {
				require.False(t, hasSelectRows(c), "no Select button in a bound thread")
				return true
			}
		}
		return false
	}, flowWait, 20*time.Millisecond, "the bound thread gets the rows without buttons")
}

// A Select on a thread that already has its conversation is refused as an
// ephemeral in the thread, never through the click's response_url: from a
// button on a normal message that URL replaces the roster for everyone.
func TestRoster_SelectRefusalLeavesTheRoster(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "<@UBOT> /agent sre-agent start here", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)

	sendRosterSelect(t, srv, "C1", "U1", "100.000", "kagent/sre-agent", api.URL+"/response_url")
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postEphemeral") {
			text, _ := c.params["text"].(string)
			if strings.Contains(text, "already talks to") && c.params["thread_ts"] == "100.000" && c.params["user"] == "U1" {
				return true
			}
		}
		return false
	}, flowWait, 20*time.Millisecond, "the refusal is an ephemeral in the thread")
	require.Empty(t, fake.pathCalls("response_url"), "the click's response_url is never used")
	require.Empty(t, fake.pathCalls("chat.update"), "the roster is not rewritten")
}
