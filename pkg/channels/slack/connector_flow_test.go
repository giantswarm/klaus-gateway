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
// connector prompts on.
func connectorAdapter(t *testing.T, gw *stubGateway) (*fakeSlackAPI, *httptest.Server) {
	t.Helper()
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
	})
	return fake, srv
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

// decoratedConnectorAdapter is connectorAdapter with a public base URL, so
// Connect buttons carry the post-login redirect and completion state. Kept
// separate: q.Encode() reorders query params, so tests asserting the raw
// login URL stay on the undecorated adapter.
func decoratedConnectorAdapter(t *testing.T, gw *stubGateway) (*fakeSlackAPI, *httptest.Server) {
	t.Helper()
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
		a.PublicBaseURL = "https://gw.example"
	})
	return fake, srv
}

// connectButton walks the recorded ephemeral posts for the Connect button and
// returns its url and value.
func connectButton(fake *fakeSlackAPI) (buttonURL, buttonValue string, ok bool) {
	for _, call := range fake.pathCalls("chat.postEphemeral") {
		blocks, _ := call.params["blocks"].([]any)
		for _, block := range blocks {
			blockMap, _ := block.(map[string]any)
			elements, _ := blockMap["elements"].([]any)
			for _, element := range elements {
				button, _ := element.(map[string]any)
				if button["action_id"] == "connector_connect" {
					buttonURL, _ = button["url"].(string)
					buttonValue, _ = button["value"].(string)
					return buttonURL, buttonValue, true
				}
			}
		}
	}
	return "", "", false
}

// newResponseURLCapture returns a server standing in for a Slack interaction
// response_url, and an accessor for everything POSTed to it.
func newResponseURLCapture(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		body += string(raw)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return body
	}
}

// An unknown or expired state renders the graceful expired page and dispatches
// nothing.
func TestConnectorComplete_UnknownState(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	_, srv := decoratedConnectorAdapter(t, gw)

	resp, err := http.Get(srv.URL + "/connectors/complete?s=bogus")
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, string(page), "Link expired")

	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.dispatchCount(), "nothing is dispatched for an unknown state")
}

// A Connect click whose value is a plain server name (a button posted without
// a post-login redirect) stays a no-op.
func TestConnectorConnect_LegacyValueNoOp(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	_, srv := connectorAdapter(t, gw)
	responseSrv, capturedBody := newResponseURLCapture(t)

	sendConnectorInteractionURL(t, srv, "connector_connect", "mcp-pro", responseSrv.URL)

	time.Sleep(100 * time.Millisecond)
	require.Empty(t, capturedBody(), "a legacy Connect click must not touch the response_url")
}
