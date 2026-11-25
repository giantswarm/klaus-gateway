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
	return elicitationTurn("muster", approvalURL)
}

// elicitationTurn pauses on a URL elicitation of server's with link, under
// muster's approval message and elicitationId, then the resumed turn answers.
func elicitationTurn(server, link string) [][]channels.OutboundDelta {
	return [][]channels.OutboundDelta{
		{{
			Kind: channels.DeltaPrompt, TaskID: "task-1", Content: approvalMessage,
			Prompt: &channels.HitlPrompt{SignIn: &channels.HitlSignIn{
				ID: "elicit-1", Server: server, Message: approvalMessage,
				URL: link, ElicitationID: approvalElicitationID,
			}, RequestID: "elicit-1"},
		}},
		{{Content: "acknowledged P123"}, {Done: true}},
	}
}

// pauseOnApproval drives a DM turn that pauses on the approval, with the
// generic return configured, and returns the adapter and its server.
func pauseOnApproval(t *testing.T, ts string) (*slackadapter.Adapter, *fakeSlackAPI, *dispatches, *httptest.Server) {
	t.Helper()
	return pauseOn(t, ts, approvalTurn())
}

// pauseOn drives a DM turn that pauses on turns' elicitation, with the
// generic return configured.
func pauseOn(t *testing.T, ts string, turns [][]channels.OutboundDelta) (*slackadapter.Adapter, *fakeSlackAPI, *dispatches, *httptest.Server) {
	t.Helper()
	sent := &dispatches{}
	gw := &stubGateway{sendQueue: turns}
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

	buttonURL, stateID, ok := connectButton(fake)
	require.True(t, ok)
	back, err := url.Parse(mustersReturn(t, buttonURL, nil))
	require.NoError(t, err)
	key := back.Query().Get("prompt")
	require.Len(t, key, 22, "the return carries the prompt's own 128-bit key")
	wantLink := approvalURL + "?redirect=" + url.QueryEscape(gatewayReturnURL+"?prompt="+key)
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

// The generic return: muster sends the browser to the gateway's return, the
// prompt's key kept and the elicitationId appended; the gateway resumes the
// matching paused task as Done would, once, and its page says nothing of
// what was decided.
func TestURLElicitation_ReturnResumesTheMatchingPrompt(t *testing.T) {
	adapter, fake, sent, _ := pauseOnApproval(t, "152.000")
	ret := httptest.NewServer(http.HandlerFunc(adapter.HandleElicitationReturn))
	t.Cleanup(ret.Close)
	link, _, ok := connectButton(fake)
	require.True(t, ok)
	back, err := url.Parse(mustersReturn(t, link, url.Values{"elicitationId": {approvalElicitationID}}))
	require.NoError(t, err)

	resp, err := http.Get(ret.URL + "?elicitationId=unknown&prompt=unknown")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "an unknown id gets the same page")
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, sent.resumes(), "an unknown id resumes nothing")

	for range 2 {
		resp, err = http.Get(ret.URL + "?" + back.RawQuery)
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

	req, err := http.NewRequest(http.MethodPost, ret.URL+"?"+back.RawQuery, nil)
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

// mustersReturn is where muster sends the browser after the person acted at
// link: link's redirect with the query muster appends, extra (elicitationId
// after an approval, server after a sign-in). "" when link names no redirect.
func mustersReturn(t *testing.T, link string, extra url.Values) string {
	t.Helper()
	u, err := url.Parse(link)
	require.NoError(t, err)
	redirect := u.Query().Get("redirect")
	if redirect == "" {
		return ""
	}
	target, err := url.Parse(redirect)
	require.NoError(t, err)
	q := target.Query()
	for k, v := range extra {
		q[k] = v
	}
	target.RawQuery = q.Encode()
	return target.String()
}

// getReturn requests the gateway's return at target (a URL whose path and
// query are the return's) and checks the page is the uniform one.
func getReturn(t *testing.T, adapter *slackadapter.Adapter, target string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	adapter.HandleElicitationReturn(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "go back to Slack")
}

// muster's return is bound to the prompt the gateway issued: its link's
// redirect carries a value of the gateway's own, which a bare elicitationId,
// known to the server that sent it and to whoever it tells, does not. The
// page is the same either way.
func TestURLElicitation_ReturnNeedsThePromptsOwnValue(t *testing.T) {
	adapter, fake, sent, _ := pauseOnApproval(t, "154.000")
	link, _, ok := connectButton(fake)
	require.True(t, ok)

	getReturn(t, adapter, gatewayReturnURL+"?elicitationId="+approvalElicitationID)
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, sent.resumes(), "a bare elicitationId resumes nothing")

	back := mustersReturn(t, link, url.Values{"elicitationId": {approvalElicitationID}})
	require.NotEmpty(t, back)
	u, err := url.Parse(back)
	require.NoError(t, err)
	require.Equal(t, gatewayReturnURL, u.Scheme+"://"+u.Host+u.Path, "the redirect is the configured return, which muster's allowlist admits as a prefix")

	getReturn(t, adapter, mustersReturn(t, link, url.Values{"elicitationId": {"another-elicitation"}}))
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, sent.resumes(), "the prompt's value with another elicitation's id resumes nothing")

	getReturn(t, adapter, back)
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "muster's return resumes the prompt")
	require.Equal(t, "U1", sent.resumes()[0].Subject, "as the person the prompt was posted for")
}

// A link off muster's origin gets no return, so no return resumes its
// prompt, even with the elicitationId its server chose.
func TestURLElicitation_ReturnNeverResumesAnotherServersPrompt(t *testing.T) {
	const thirdParty = "https://mcp.third-party.example/consent"
	adapter, fake, sent, _ := pauseOn(t, "155.000", elicitationTurn("third-party", thirdParty))
	link, _, ok := connectButton(fake)
	require.True(t, ok)
	require.Equal(t, thirdParty, link, "the link is opened as its server gave it")

	getReturn(t, adapter, gatewayReturnURL+"?elicitationId="+approvalElicitationID)
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, sent.resumes())
}

// A sign-in on muster's origin returns the same way: muster's sign-in sends
// the browser to the redirect with server=<name> and no elicitationId, and
// the prompt's own value resumes it.
func TestURLElicitation_SignInReturnResumes(t *testing.T) {
	const signInLink = "https://muster.example/oauth/proxy/start?state=abc"
	adapter, fake, sent, _ := pauseOn(t, "156.000", elicitationTurn("muster", signInLink))
	link, _, ok := connectButton(fake)
	require.True(t, ok)
	back := mustersReturn(t, link, url.Values{"server": {"github"}})
	require.NotEmpty(t, back, "a sign-in link on muster's origin gets the return")

	getReturn(t, adapter, back)
	require.Eventually(t, func() bool { return len(sent.resumes()) == 1 }, flowWait, 20*time.Millisecond, "the sign-in's return resumes the prompt")
	require.Equal(t, channels.DecisionApprove, sent.resumes()[0].Decision.Type)
}
