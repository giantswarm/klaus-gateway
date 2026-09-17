package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "200.000", "100.000", api.URL+"/response_url")

	view := openedView(t, fake)
	var pm map[string]string
	require.NoError(t, json.Unmarshal([]byte(view["private_metadata"].(string)), &pm))
	require.Equal(t, "C1", pm["c"])
	require.Equal(t, "U1", pm["u"])
	require.Equal(t, "100.000", pm["t"], "the conversation starts in the invoked message's thread")
	question := view["blocks"].([]any)[1].(map[string]any)["element"].(map[string]any)
	_, prefilled := question["initial_value"]
	require.False(t, prefilled, "the shortcut has no question to prefill")

	sendAskAgentSubmission(t, srv, "U1", view["private_metadata"].(string), "kagent/sre-agent", "why are pods crashlooping?")
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the submission dispatches the first turn")

	echo := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "C1", echo.params["channel"])
	require.Equal(t, "100.000", echo.params["thread_ts"], "the echo is a reply in the target thread")
	require.Equal(t, "SRE Agent", echo.params["username"], "posted under the agent's identity")
	require.Contains(t, echo.params["text"].(string), "<@U1> asked *SRE Agent*")
	require.Contains(t, echo.params["text"].(string), "> why are pods crashlooping?")

	msgs := resolved()
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
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendAskAgentShortcut(t, srv, "C1", "U1", "500.000", "", api.URL+"/response_url")

	pmRaw := openedView(t, fake)["private_metadata"].(string)
	var pm map[string]string
	require.NoError(t, json.Unmarshal([]byte(pmRaw), &pm))
	require.Equal(t, "500.000", pm["t"], "a root message's own ts is the thread")

	sendAskAgentSubmission(t, srv, "U1", pmRaw, "kagent/sre-agent", "what happened here?")
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the submission dispatches the first turn")

	require.Equal(t, "500.000", fake.pathCalls("chat.postMessage")[0].params["thread_ts"])
	require.Equal(t, "500.000", resolved()[0].ThreadID)
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
	require.Equal(t, 0, gw.resolveCount())
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
	require.Equal(t, 0, gw.resolveCount())
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
	require.Equal(t, 0, gw.resolveCount(), "nothing runs")
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
	require.Equal(t, 0, gw.resolveCount(), "nothing runs")
}
