package slack_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// The fixture of a tool approval as muster asks for it (tool approval slice 2)
// and kagent relays it: a url_elicitation_request from server muster, its
// message the plain-words ask, its URL muster's approval page and its
// elicitation_id muster's random id.
const (
	approvalMessage       = "Alert Review wants to acknowledge an incident in PagerDuty."
	approvalURL           = "https://muster.example/approvals/0f3a9c"
	approvalElicitationID = "el-7d1e"
	gatewayReturnURL      = "https://gateway.example" + slackadapter.ElicitationReturnPath
)

// approvalTurn pauses on muster's approval elicitation, then the resumed turn
// answers.
func approvalTurn() [][]channels.OutboundDelta {
	return [][]channels.OutboundDelta{
		{{
			Kind: channels.DeltaPrompt, TaskID: "task-1", Content: approvalMessage,
			Prompt: &channels.HitlPrompt{SignIn: &channels.HitlSignIn{
				ID: "elicit-1", Server: "muster", Message: approvalMessage,
				URL: approvalURL, ElicitationID: approvalElicitationID,
			}, RequestID: "elicit-1"},
		}},
		{{Content: "acknowledged P123"}, {Done: true}},
	}
}

// pauseOnApproval drives a DM turn that pauses on the approval, with the
// generic return configured, and returns the adapter and its server.
func pauseOnApproval(t *testing.T, ts string) (*slackadapter.Adapter, *fakeSlackAPI, *dispatches, *httptest.Server) {
	t.Helper()
	sent := &dispatches{}
	gw := &stubGateway{sendQueue: approvalTurn()}
	gw.onDispatch = sent.record
	fake := newFakeSlackAPI()
	adapter, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
		a.MusterURL = "https://muster.example"
		a.ElicitationReturnURL = gatewayReturnURL
	})
	gw.openConversation(t, "D1", ts, "U1", "test-agent")
	sendEvent(t, srv, dmEvent("U1", "ack the incident", ts))
	require.Eventually(t, func() bool {
		_, ok := buttonValue(fake, "elicitation_done")
		return ok
	}, flowWait, 20*time.Millisecond, "the prompt is posted")
	return adapter, fake, sent, srv
}

// ephemeralButtonLabel returns the label of the ephemeral button actionID.
func ephemeralButtonLabel(fake *fakeSlackAPI, actionID string) string {
	for _, call := range fake.pathCalls("chat.postEphemeral") {
		blocks, _ := call.params["blocks"].([]any)
		for _, block := range blocks {
			blockMap, _ := block.(map[string]any)
			elements, _ := blockMap["elements"].([]any)
			for _, element := range elements {
				button, _ := element.(map[string]any)
				if button["action_id"] == actionID {
					text, _ := button["text"].(map[string]any)
					label, _ := text["text"].(string)
					return label
				}
			}
		}
	}
	return ""
}

// A task paused on any URL elicitation (here muster's tool approval) gets the
// one generic prompt, ephemeral to the person whose turn asked: muster's
// message as the heading, attributed to the server, the whole link with its
// host, a neutral Open button labelled with the host, Done and Not now. The
// link is muster's with the gateway's return added. Opening the link resumes
// nothing; Done resumes the paused task with accept, once.
func TestURLElicitation_GenericPromptResumesOnDone(t *testing.T) {
	_, fake, sent, srv := pauseOnApproval(t, "150.000")
	responseSrv, capturedBody := newResponseURLCapture(t)

	wantLink := approvalURL + "?redirect=" + url.QueryEscape(gatewayReturnURL)
	buttonURL, stateID, ok := connectButton(fake)
	require.True(t, ok)
	require.Equal(t, wantLink, buttonURL, "muster's link with the return, nothing else")
	require.Equal(t, "Open ↗ muster.example", ephemeralButtonLabel(fake, "connector_connect"))
	require.Equal(t, "Done", ephemeralButtonLabel(fake, "elicitation_done"))
	require.Equal(t, "Not now", ephemeralButtonLabel(fake, "sign_in_decline"))
	doneValue, _ := buttonValue(fake, "elicitation_done")
	require.Equal(t, stateID, doneValue)

	blob := ephemeralJSON(fake)
	require.Contains(t, blob, `"user":"U1"`)
	require.Contains(t, blob, "*"+approvalMessage+"*\\nAsked by *muster*.\\nOpens *muster.example*\\n`"+wantLink+"`\\nOpen it, then choose *Done* here: the agent continues.",
		"muster's message is the heading, then who asked and the whole link")
	require.NotContains(t, blob, "sign in", "no sign-in wording")
	require.Contains(t, fake.threadText(), "Waiting for <@U1> (asked by *muster*).")

	sendClick(t, srv, "connector_connect", stateID, responseSrv.URL, true)
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, sent.resumes(), "opening the link resumes nothing")

	for range 2 {
		sendClick(t, srv, "elicitation_done", stateID, responseSrv.URL, true)
	}
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "Done resumes the paused task")
	resume := sent.resumes()[0]
	require.Equal(t, channels.DecisionApprove, resume.Decision.Type, "Done accepts the elicitation; muster decides the outcome")
	require.Equal(t, "elicit-1", resume.Decision.RequestID)
	require.Equal(t, "U1", resume.Subject)
	require.Eventually(t, func() bool { return strings.Contains(capturedBody(), "Done. The agent continues.") },
		flowWait, 20*time.Millisecond, "the prompt is replaced")
	time.Sleep(200 * time.Millisecond)
	require.Len(t, sent.resumes(), 1, "a second Done resumes nothing more")
	require.Eventually(t, func() bool { return strings.Contains(fake.threadText(), "<@U1> is done.") },
		flowWait, 20*time.Millisecond, "the thread note says the person is done, not what was decided")
}

// Not now resumes the paused task with decline.
func TestURLElicitation_NotNowDeclines(t *testing.T) {
	_, fake, sent, srv := pauseOnApproval(t, "151.000")
	value, _ := buttonValue(fake, "sign_in_decline")
	sendClick(t, srv, "sign_in_decline", value, "", true)
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond)
	require.Equal(t, channels.DecisionReject, sent.resumes()[0].Decision.Type)
}

// The generic return: muster sends the browser to the gateway's return with
// the elicitationId; the gateway resumes the matching paused task as Done
// would, once, and its page says nothing of what was decided.
func TestURLElicitation_ReturnResumesTheMatchingPrompt(t *testing.T) {
	adapter, _, sent, _ := pauseOnApproval(t, "152.000")
	ret := httptest.NewServer(http.HandlerFunc(adapter.HandleElicitationReturn))
	t.Cleanup(ret.Close)

	resp, err := http.Get(ret.URL + "?elicitationId=unknown")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "an unknown id gets the same page")
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, sent.resumes(), "an unknown id resumes nothing")

	for range 2 {
		resp, err = http.Get(ret.URL + "?elicitationId=" + approvalElicitationID)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		require.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
		require.Contains(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'")
		require.Contains(t, string(body), "go back to Slack")
		require.NotContains(t, strings.ToLower(string(body)), "approved")
	}
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "the return resumes the paused task")
	require.Equal(t, channels.DecisionApprove, sent.resumes()[0].Decision.Type)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, sent.resumes(), 1, "a second return resumes nothing more")

	req, err := http.NewRequest(http.MethodPost, ret.URL+"?elicitationId="+approvalElicitationID, nil)
	require.NoError(t, err)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// Without a configured return, or for a link off muster's origin, the link is
// opened exactly as the server gave it.
func TestURLElicitation_LinkUnchangedWithoutTheReturn(t *testing.T) {
	sent := &dispatches{}
	gw := &stubGateway{sendQueue: approvalTurn()}
	gw.onDispatch = sent.record
	fake, srv := connectorAdapter(t, gw)
	gw.openConversation(t, "D1", "153.000", "U1", "test-agent")
	sendEvent(t, srv, dmEvent("U1", "ack the incident", "153.000"))
	require.Eventually(t, func() bool {
		_, ok := buttonValue(fake, "elicitation_done")
		return ok
	}, flowWait, 20*time.Millisecond)
	buttonURL, _, ok := connectButton(fake)
	require.True(t, ok)
	require.Equal(t, approvalURL, buttonURL)
}
