package slack_test

import (
	"bytes"
	"context"
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
	"github.com/giantswarm/klaus-gateway/pkg/muster"
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

// sendConnectorInteractionURL posts a signed block_actions interaction (user
// U1, channel D1) on an ephemeral prompt, with the button's value. A
// non-empty responseURL is attached as the interaction's response_url.
func sendConnectorInteractionURL(t *testing.T, srv *httptest.Server, actionID, value, responseURL string) {
	t.Helper()
	sendClick(t, srv, actionID, value, responseURL, true)
}

// sendClick posts a signed block_actions interaction (user U1, channel D1)
// with the button's value, on an ephemeral prompt or on a message.
func sendClick(t *testing.T, srv *httptest.Server, actionID, value, responseURL string, ephemeral bool) {
	t.Helper()
	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": "U1"},
		"channel":   map[string]any{"id": "D1"},
		"container": map[string]any{"type": "message", "is_ephemeral": ephemeral},
		"actions":   []any{map[string]any{"action_id": actionID, "value": value}},
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

// A Connect click whose value is no completion state (a conversation's or a
// provider's prompt, or one minted before a restart) stays a no-op.
func TestConnectorConnect_LegacyValueNoOp(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	_, srv := connectorAdapter(t, gw)
	responseSrv, capturedBody := newResponseURLCapture(t)

	sendConnectorInteractionURL(t, srv, "connector_connect", "mcp-pro", responseSrv.URL)

	time.Sleep(100 * time.Millisecond)
	require.Empty(t, capturedBody(), "a legacy Connect click must not touch the response_url")
}

// fakeAccepter records the sign-ins the adapter accepts and answers each with
// result.
type fakeAccepter struct {
	mu       sync.Mutex
	result   muster.Result
	accepted []*muster.SignIn
	bearers  []string
}

func (f *fakeAccepter) Accept(_ context.Context, bearer string, signIn *muster.SignIn) (muster.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted = append(f.accepted, signIn)
	f.bearers = append(f.bearers, bearer)
	return f.result, nil
}

func (f *fakeAccepter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accepted)
}
