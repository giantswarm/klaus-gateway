package slack_test

import (
	"fmt"
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

func pauseOnSignIn(t *testing.T, ts string) (*fakeSlackAPI, *dispatches, func(actionID, value, responseURL string)) {
	t.Helper()
	sent := &dispatches{}
	gw := &stubGateway{sendQueue: signInTurn()}
	gw.onDispatch = sent.record
	fake, srv := connectorAdapter(t, gw)
	gw.openConversation(t, "D1", ts, "U1", "test-agent")
	sendEvent(t, srv, dmEvent("U1", "who am i", ts))
	require.Eventually(t, func() bool {
		_, ok := buttonValue(fake, "sign_in_decline")
		return ok
	}, flowWait, 20*time.Millisecond, "the sign-in prompt is posted")
	click := func(actionID, value, responseURL string) {
		sendConnectorInteractionURL(t, srv, actionID, value, responseURL)
	}
	return fake, sent, click
}

// A task paused on a sign-in link posts the Connect prompt to the person whose
// turn asked: the server's message attributed to it, the whole link as given
// with its domain in bold, and a thread note naming them. The link button
// opens the link unchanged; its first click is the consent, which resumes the
// paused task with accept, once.
func TestMCPSignIn_ConsentResumesThePausedTask(t *testing.T) {
	fake, sent, click := pauseOnSignIn(t, "140.000")
	responseSrv, capturedBody := newResponseURLCapture(t)

	buttonURL, stateID, ok := connectButton(fake)
	require.True(t, ok)
	require.Equal(t, "https://gateway.example/oauth/proxy/start?s=1", buttonURL, "the server's link, unchanged")
	blob := ephemeralJSON(fake)
	require.Contains(t, blob, `"user":"U1"`)
	require.Contains(t, blob, "*muster* asks you to sign in so the agent can continue.\\n\\u003eSign in to github to continue.\\nOpens *gateway.example*\\n`https://gateway.example/oauth/proxy/start?s=1`", "the server's message, attributed to it, and the whole link")
	require.True(t, strings.Contains(fake.threadText(), "Waiting for <@U1> to sign in (asked by *muster*)."), "the thread says whom the turn waits on")
	require.Empty(t, sent.resumes(), "nothing resumes before the consent")

	for range 2 {
		click("connector_connect", stateID, responseSrv.URL)
	}
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "the consent resumes the paused task")
	resume := sent.resumes()[0]
	require.Equal(t, channels.DecisionApprove, resume.Decision.Type, "the consent accepts")
	require.Equal(t, "sign-in-1", resume.Decision.RequestID, "the answer names the request it answers")
	require.Equal(t, "U1", resume.Subject)
	require.Equal(t, "I've signed in, continue", resume.Text)
	require.Eventually(t, func() bool { return strings.Contains(capturedBody(), "Opened the sign-in link.") },
		flowWait, 20*time.Millisecond, "the prompt says the agent continues once signed in")
	time.Sleep(200 * time.Millisecond)
	require.Len(t, sent.resumes(), 1, "a second click resumes nothing more")
}

// "Not now" resumes the paused task with decline, and a consent that comes
// after it resumes nothing.
func TestMCPSignIn_NotNowDeclines(t *testing.T) {
	fake, sent, click := pauseOnSignIn(t, "141.000")
	responseSrv, capturedBody := newResponseURLCapture(t)
	_, stateID, _ := connectButton(fake)
	value, _ := buttonValue(fake, "sign_in_decline")

	click("sign_in_decline", value, responseSrv.URL)
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "Not now resumes the paused task")
	require.Equal(t, channels.DecisionReject, sent.resumes()[0].Decision.Type)
	require.Eventually(t, func() bool { return strings.Contains(capturedBody(), "Not signed in. The agent continues without it.") },
		flowWait, 20*time.Millisecond, "the prompt says it was declined")

	click("connector_connect", stateID, "")
	time.Sleep(200 * time.Millisecond)
	require.Len(t, sent.resumes(), 1, "a consent for a task no longer paused on the sign-in resumes nothing")
}

// A link that cannot be shown as written is not offered: the prompt says so
// and keeps "Not now".
func TestMCPSignIn_UnusableLinkIsNotOffered(t *testing.T) {
	sent := &dispatches{}
	turn := signInTurn()
	turn[0][0].Prompt.SignIn.URL = "http://gateway.example/oauth/proxy/start?s=1"
	gw := &stubGateway{sendQueue: turn}
	gw.onDispatch = sent.record
	fake, srv := connectorAdapter(t, gw)
	gw.openConversation(t, "D1", "144.000", "U1", "test-agent")
	sendEvent(t, srv, dmEvent("U1", "who am i", "144.000"))
	require.Eventually(t, func() bool {
		_, ok := buttonValue(fake, "sign_in_decline")
		return ok
	}, flowWait, 20*time.Millisecond, "the prompt is posted")
	_, _, ok := connectButton(fake)
	require.False(t, ok, "no link button")
	require.Contains(t, ephemeralJSON(fake), "Its sign-in link is not one the app opens")
}

// A sign-in answered elsewhere first (Backstage, on the same task) leaves the
// Slack consent with nothing to resume: the thread is told, no failure note is
// posted, and the stale prompt is dropped so a later reply starts a new turn.
func TestMCPSignIn_AnsweredElsewhereIsANoOp(t *testing.T) {
	sent := &dispatches{}
	gw := &stubGateway{sendQueue: signInTurn()}
	gw.onDispatch = sent.record
	fake, srv := connectorAdapter(t, gw)
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
	sendConnectorInteractionURL(t, srv, "connector_connect", stateID, "")

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
