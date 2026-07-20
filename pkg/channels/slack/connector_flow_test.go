package slack_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// authChallengeOutput is the free text a core_auth_login result carries: a
// human-readable challenge with the backend name and the login URL.
const authChallengeOutput = "Authentication Required\n\n" +
	"Server: gazelle-mcp-pro\nStatus: needs sign-in\n\n" +
	"Please sign in to connect to this server:\n\n" +
	"https://pro.example.com/authorize?state=abc\n\n" +
	"After signing in, run this tool again to complete the connection."

// authLoginResult is a stream delta modelling a core_auth_login tool result.
func authLoginResult(output string) channels.OutboundDelta {
	return channels.OutboundDelta{
		Kind: channels.DeltaToolActivity,
		Tool: &channels.ToolActivity{
			Name:     "core_auth_login",
			Kind:     channels.ToolResult,
			Response: map[string]any{"output": output},
		},
	}
}

// callToolLoginTurn models how the auth challenge actually reaches the stream
// on a live install: the agent invokes muster's call_tool meta-tool with the
// inner tool in the arguments, and the result (named call_tool, no arguments)
// nests the challenge text in an MCP-style content list.
func callToolLoginTurn(output string) []channels.OutboundDelta {
	return []channels.OutboundDelta{
		{
			Kind: channels.DeltaToolActivity,
			Tool: &channels.ToolActivity{
				Name:   "call_tool",
				Kind:   channels.ToolCall,
				CallID: "call-1",
				Args: map[string]any{
					"name":      "core_auth_login",
					"arguments": map[string]any{"server": "gazelle-mcp-pro"},
				},
			},
		},
		{
			Kind: channels.DeltaToolActivity,
			Tool: &channels.ToolActivity{
				Name:   "call_tool",
				Kind:   channels.ToolResult,
				CallID: "call-1",
				Response: map[string]any{
					"content": []any{map[string]any{"type": "text", "text": output}},
					"isError": false,
				},
			},
		},
	}
}

// ephemeralJSON returns the JSON of every chat.postEphemeral call, for
// asserting on Block Kit contents.
func ephemeralJSON(fake *fakeSlackAPI) string {
	var b strings.Builder
	for _, c := range fake.pathCalls("chat.postEphemeral") {
		raw, _ := json.Marshal(c.params)
		b.Write(raw)
		b.WriteString("\n")
	}
	return b.String()
}

// connectorAdapter builds an events adapter for a linked user (U1) with the
// reactive connector prompt enabled.
func connectorAdapter(t *testing.T, gw *stubGateway) (*fakeSlackAPI, *httptest.Server) {
	t.Helper()
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
	})
	return fake, srv
}

// A core_auth_login result in the turn's stream yields one ephemeral Connect
// prompt carrying the parsed backend name and login URL.
func TestConnectorPrompt_PostedFromToolResult(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		authLoginResult(authChallengeOutput),
		{Content: "ok"}, {Done: true},
	}}
	fake, srv := connectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "use the pro tools", "100.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(ephemeralJSON(fake), "connector_connect")
	}, 2*time.Second, 20*time.Millisecond, "connect prompt is posted")

	blob := ephemeralJSON(fake)
	require.Contains(t, blob, "gazelle-mcp-pro")
	require.Contains(t, blob, "connector_dismiss")
	require.Contains(t, blob, "https://pro.example.com/authorize?state=abc")
}

// The challenge arriving through muster's call_tool meta-tool (the shape live
// agents produce: result named call_tool, inner tool only in the call
// arguments, text nested in an MCP content list) also yields the prompt.
func TestConnectorPrompt_PostedFromCallToolResult(t *testing.T) {
	gw := &stubGateway{deltas: append(
		callToolLoginTurn(authChallengeOutput),
		channels.OutboundDelta{Content: "ok"}, channels.OutboundDelta{Done: true},
	)}
	fake, srv := connectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "use the pro tools", "101.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(ephemeralJSON(fake), "connector_connect")
	}, 2*time.Second, 20*time.Millisecond, "connect prompt is posted")

	blob := ephemeralJSON(fake)
	require.Contains(t, blob, "gazelle-mcp-pro")
	require.Contains(t, blob, "https://pro.example.com/authorize?state=abc")
}

// A call_tool result whose recorded inner tool is not core_auth_login yields
// no prompt, even when its payload happens to carry a URL.
func TestConnectorPrompt_CallToolOtherToolNoPrompt(t *testing.T) {
	turn := callToolLoginTurn("see https://pro.example.com/docs for details")
	turn[0].Tool.Args["name"] = "list_tools"
	gw := &stubGateway{deltas: append(turn,
		channels.OutboundDelta{Content: "ok"}, channels.OutboundDelta{Done: true},
	)}
	fake, srv := connectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "hi", "105.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.NotContains(t, ephemeralJSON(fake), "connector_connect")
}

// The cooldown bounds re-prompts: two turns that both report the same backend
// yield a single Connect prompt.
func TestConnectorPrompt_Cooldown(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		authLoginResult(authChallengeOutput),
		{Content: "ok"}, {Done: true},
	}}
	fake, srv := connectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "one", "102.000"))
	require.Eventually(t, func() bool {
		return strings.Count(ephemeralJSON(fake), "connector_connect") == 1
	}, 2*time.Second, 20*time.Millisecond, "first turn posts the prompt")

	sendEvent(t, srv, dmThreadEvent("U1", "two", "103.000", "102.000"))
	require.Eventually(t, func() bool {
		return gw.resolveCount() == 2
	}, 2*time.Second, 20*time.Millisecond, "second turn ran")
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, strings.Count(ephemeralJSON(fake), "connector_connect"),
		"second turn within the cooldown must not re-prompt")
}

// A result without a login URL yields no prompt.
func TestConnectorPrompt_NoURLNoPrompt(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		authLoginResult("Authentication Required\n\nServer: pro\n(no link yet)"),
		{Content: "ok"}, {Done: true},
	}}
	fake, srv := connectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "hi", "104.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.NotContains(t, ephemeralJSON(fake), "connector_connect")
}

// The ephemeral Connect prompt must be addressed to the RAW Slack user ID, not
// the resolved workspace email. dispatch rewrites msg.Subject to the email
// before the stream runs; chat.postEphemeral's user param requires a "U…" Slack
// ID, so an email would fail with user_not_found and (via clearConnectorPrompted)
// re-fail every turn. Resolve an email for the user, drive a challenge, and
// assert the prompt still targets "U1".
func TestConnectorPrompt_AddressedToRawSlackUserID(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		authLoginResult(authChallengeOutput),
		{Content: "ok"}, {Done: true},
	}}
	fake, srv := connectorAdapter(t, gw)
	// Make users.info resolve an email so dispatch rewrites msg.Subject to it
	// before the connector prompt posts. Before the fix this email leaked into
	// the chat.postEphemeral user param.
	fake.setResponse("users.info", `{"ok":true,"user":{"profile":{"email":"u1@example.com"}}}`)

	sendEvent(t, srv, dmEvent("U1", "use the pro tools", "120.000"))

	var connect *recordedCall
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postEphemeral") {
			raw, _ := json.Marshal(c.params)
			if strings.Contains(string(raw), "connector_connect") {
				cc := c
				connect = &cc
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "connect prompt is posted")

	require.Equal(t, "U1", connect.params["user"], "prompt must target the raw Slack user ID")
	require.NotEqual(t, "u1@example.com", connect.params["user"], "the resolved email must not be used as the ephemeral user")
}

// A challenge whose login link is not https is rejected: the Connect button
// opens agent/tool-controlled text as a browser URL, so a non-https link yields
// no prompt rather than a button that would navigate over http.
func TestConnectorPrompt_NonHTTPSNoPrompt(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		authLoginResult("Authentication Required\n\nServer: pro\n\nSign in: http://pro.example.com/authorize"),
		{Content: "ok"}, {Done: true},
	}}
	fake, srv := connectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "hi", "106.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.NotContains(t, ephemeralJSON(fake), "connector_connect")
}

// A "Not now" click replaces the ephemeral prompt with an acknowledgement.
func TestConnectorDismissInteraction(t *testing.T) {
	var captured struct {
		mu   sync.Mutex
		body string
	}
	responseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured.mu.Lock()
		captured.body += string(raw)
		captured.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseSrv.Close)

	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	_, srv := connectorAdapter(t, gw)

	sendConnectorInteractionURL(t, srv, "connector_dismiss", "gazelle-mcp-pro", responseSrv.URL)

	require.Eventually(t, func() bool {
		captured.mu.Lock()
		defer captured.mu.Unlock()
		return strings.Contains(captured.body, "won't ask again")
	}, 2*time.Second, 20*time.Millisecond, "the prompt is replaced with an acknowledgement")
}

// An oversized or empty backend name in the button value is dropped (no
// response_url update).
func TestConnectorDismissInteraction_InvalidServer(t *testing.T) {
	var captured struct {
		mu   sync.Mutex
		body string
	}
	responseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured.mu.Lock()
		captured.body += string(raw)
		captured.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseSrv.Close)

	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	_, srv := connectorAdapter(t, gw)

	sendConnectorInteractionURL(t, srv, "connector_dismiss", strings.Repeat("x", 200), responseSrv.URL)
	sendConnectorInteractionURL(t, srv, "connector_dismiss", "", responseSrv.URL)

	time.Sleep(100 * time.Millisecond)
	captured.mu.Lock()
	defer captured.mu.Unlock()
	require.Empty(t, captured.body)
}

// A linked user's /login confirms their signed-in identity, with no connector
// listing or prompt.
func TestLoginCommand_LinkedConfirmation(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	fake, srv := connectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "/login", "110.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Signed in")
	}, 2*time.Second, 20*time.Millisecond, "signed-in confirmation is posted")
	require.NotContains(t, ephemeralJSON(fake), "connector_connect")
}

// An unlinked user's /login posts the sign-in prompt.
func TestLoginCommand_UnlinkedGetsSignIn(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "someone-else", linkURL: "https://gw.example/link"}
		a.ConnectorPrompts = true
	})

	sendEvent(t, srv, dmEvent("U1", "/login", "111.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(ephemeralJSON(fake), "obo_sign_in")
	}, 2*time.Second, 20*time.Millisecond)
}

// sendConnectorInteractionURL posts a signed block_actions interaction (user
// U1, channel D1) whose button value carries a backend name. A non-empty
// responseURL is attached as the interaction's response_url.
func sendConnectorInteractionURL(t *testing.T, srv *httptest.Server, actionID, server, responseURL string) {
	t.Helper()
	inner := map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U1"},
		"channel": map[string]any{"id": "D1"},
		"actions": []any{map[string]any{"action_id": actionID, "value": server}},
	}
	if responseURL != "" {
		inner["response_url"] = responseURL
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
}
