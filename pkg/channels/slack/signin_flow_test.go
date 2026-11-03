package slack_test

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// signInTurn pauses on an MCP server's sign-in link, then the resumed turn
// answers.
func signInTurn() [][]channels.OutboundDelta {
	return [][]channels.OutboundDelta{
		{{
			Kind: channels.DeltaPrompt, TaskID: "task-1", Content: "Sign in to github",
			Prompt: &channels.HitlPrompt{SignIn: &channels.HitlSignIn{
				ID: "sign-in-1", Server: "muster", Message: "Sign in to github to continue.",
				URL: "https://gateway.example/oauth/proxy/start?s=1",
			}, RequestID: "sign-in-1"},
		}},
		{{Content: "you are jane"}, {Done: true}},
	}
}

// dispatches records the turns the gateway was sent.
type dispatches struct {
	mu   sync.Mutex
	msgs []channels.InboundMessage
}

func (d *dispatches) record(msg channels.InboundMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.msgs = append(d.msgs, msg)
}

// resumes returns the turns that resumed task-1.
func (d *dispatches) resumes() []channels.InboundMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []channels.InboundMessage
	for _, msg := range d.msgs {
		if msg.TaskID == "task-1" {
			out = append(out, msg)
		}
	}
	return out
}

// buttonValue returns the value of the ephemeral button actionID.
func buttonValue(fake *fakeSlackAPI, actionID string) (string, bool) {
	for _, call := range fake.pathCalls("chat.postEphemeral") {
		blocks, _ := call.params["blocks"].([]any)
		for _, block := range blocks {
			blockMap, _ := block.(map[string]any)
			elements, _ := blockMap["elements"].([]any)
			for _, element := range elements {
				button, _ := element.(map[string]any)
				if button["action_id"] == actionID {
					value, _ := button["value"].(string)
					return value, true
				}
			}
		}
	}
	return "", false
}

func pauseOnSignIn(t *testing.T, ts string) (*fakeSlackAPI, *dispatches, func(path string) *http.Response, func(actionID, value, responseURL string)) {
	t.Helper()
	sent := &dispatches{}
	gw := &stubGateway{sendQueue: signInTurn()}
	gw.onDispatch = sent.record
	fake, srv := decoratedConnectorAdapter(t, gw)
	gw.openConversation(t, "D1", ts, "U1", "test-agent")
	sendEvent(t, srv, dmEvent("U1", "who am i", ts))
	require.Eventually(t, func() bool {
		_, ok := buttonValue(fake, "sign_in_decline")
		return ok
	}, flowWait, 20*time.Millisecond, "the sign-in prompt is posted")
	get := func(path string) *http.Response {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		return resp
	}
	click := func(actionID, value, responseURL string) {
		sendConnectorInteractionURL(t, srv, actionID, value, responseURL)
	}
	return fake, sent, get, click
}

// A task paused on a sign-in link posts the Connect prompt to the person whose
// turn asked, with the post-login redirect, and a thread note naming them.
// The landing resumes the paused task with accept, once.
func TestMCPSignIn_LandingResumesThePausedTask(t *testing.T) {
	fake, sent, get, _ := pauseOnSignIn(t, "140.000")

	buttonURL, stateID, ok := connectButton(fake)
	require.True(t, ok)
	parsed, err := url.Parse(buttonURL)
	require.NoError(t, err)
	require.Equal(t, "1", parsed.Query().Get("s"), "the link's own parameters survive")
	require.Equal(t, "https://gw.example/connectors/complete?s="+stateID, parsed.Query().Get("redirect"))
	blob := ephemeralJSON(fake)
	require.Contains(t, blob, `"user":"U1"`)
	require.Contains(t, blob, `*muster* asks you to sign in so the agent can continue.\n\u003eSign in to github to continue.\nThe agent picks up here once you are done.`, "the server's message, attributed to it")
	require.True(t, strings.Contains(fake.threadText(), "Waiting for <@U1> to sign in (asked by *muster*)."), "the thread says whom the turn waits on")
	require.Empty(t, sent.resumes(), "nothing resumes before the sign-in")

	for range 2 {
		resp := get("/connectors/complete?s=" + url.QueryEscape(stateID))
		page, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(page), "The sign-in is complete")
	}
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "the landing resumes the paused task")
	resume := sent.resumes()[0]
	require.Equal(t, channels.DecisionApprove, resume.Decision.Type, "a completed sign-in accepts")
	require.Equal(t, "sign-in-1", resume.Decision.RequestID, "the answer names the request it answers")
	require.Equal(t, "U1", resume.Subject)
	require.Equal(t, "I've signed in, continue", resume.Text)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, sent.resumes(), 1, "a reload resumes nothing more")
	require.Eventually(t, func() bool { return strings.Contains(fake.threadText(), "<@U1> signed in.") },
		flowWait, 20*time.Millisecond, "the thread note records the sign-in")
}

// "Not now" resumes the paused task with decline, and a landing that comes
// after it resumes nothing.
func TestMCPSignIn_NotNowDeclines(t *testing.T) {
	fake, sent, get, click := pauseOnSignIn(t, "141.000")
	responseSrv, capturedBody := newResponseURLCapture(t)
	_, stateID, _ := connectButton(fake)
	value, _ := buttonValue(fake, "sign_in_decline")

	click("sign_in_decline", value, responseSrv.URL)
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "Not now resumes the paused task")
	require.Equal(t, channels.DecisionReject, sent.resumes()[0].Decision.Type)
	require.Eventually(t, func() bool { return strings.Contains(capturedBody(), "Not signed in. The agent continues without it.") },
		flowWait, 20*time.Millisecond, "the prompt says it was declined")

	resp := get("/connectors/complete?s=" + url.QueryEscape(stateID))
	_ = resp.Body.Close()
	time.Sleep(200 * time.Millisecond)
	require.Len(t, sent.resumes(), 1, "a landing for a task no longer paused on the sign-in resumes nothing")
}

// The landing can outrun Slack's delivery of the Connect click: the paused
// task still resumes, and the late click rewrites the prompt.
func TestMCPSignIn_LandingBeforeClick(t *testing.T) {
	fake, sent, get, click := pauseOnSignIn(t, "142.000")
	responseSrv, capturedBody := newResponseURLCapture(t)
	_, stateID, ok := connectButton(fake)
	require.True(t, ok)

	resp := get("/connectors/complete?s=" + url.QueryEscape(stateID))
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "the landing resumes the paused task before the click arrives")

	click("connector_connect", stateID, responseSrv.URL)
	require.Eventually(t, func() bool { return strings.Contains(capturedBody(), "Signed in.") },
		flowWait, 20*time.Millisecond, "the late click owns the prompt rewrite")
}

// A sign-in answered elsewhere first (Backstage, on the same task) leaves the
// Slack landing with nothing to resume: the thread is told, no failure note is
// posted, and the stale prompt is dropped so a later reply starts a new turn.
func TestMCPSignIn_AnsweredElsewhereIsANoOp(t *testing.T) {
	sent := &dispatches{}
	gw := &stubGateway{sendQueue: signInTurn()}
	gw.onDispatch = sent.record
	fake, srv := decoratedConnectorAdapter(t, gw)
	gw.openConversation(t, "D1", "143.000", "U1", "test-agent")
	sendEvent(t, srv, dmEvent("U1", "who am i", "143.000"))
	require.Eventually(t, func() bool {
		_, ok := buttonValue(fake, "sign_in_decline")
		return ok
	}, flowWait, 20*time.Millisecond, "the sign-in prompt is posted")
	_, stateID, ok := connectButton(fake)
	require.True(t, ok)

	gw.mu.Lock()
	gw.dispatchErr = fmt.Errorf("%w: task task-1 is TASK_STATE_WORKING", channels.ErrNoPendingPrompt)
	gw.mu.Unlock()
	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Eventually(t, func() bool {
		return strings.Contains(fake.threadText(), "This was already answered elsewhere, so this answer was not sent to the agent.")
	}, flowWait, 20*time.Millisecond, "the thread is told the answer went nowhere")
	require.Len(t, sent.resumes(), 1)
	require.NotContains(t, fake.threadText(), "could not be picked up again", "an answer that lost the race is not a failure")

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendEvent(t, srv, dmThreadEvent("U1", "and now?", "143.001", "143.000"))
	require.Eventually(t, func() bool {
		sent.mu.Lock()
		defer sent.mu.Unlock()
		return len(sent.msgs) == 3
	}, flowWait, 20*time.Millisecond)
	sent.mu.Lock()
	last := sent.msgs[2]
	sent.mu.Unlock()
	require.Empty(t, last.TaskID, "the stale prompt is dropped: a reply is a new turn")
	require.Nil(t, last.Decision)
}
