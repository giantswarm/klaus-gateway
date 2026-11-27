package a2aserver_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
)

const agentCardPath = "/a2a/" + tenant + "/.well-known/agent-card.json"

// getAgentCard reads path from srv with token (none when empty).
func getAgentCard(t *testing.T, srv *httptest.Server, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

func decodeCard(t *testing.T, body []byte) *a2a.AgentCard {
	t.Helper()
	var card a2a.AgentCard
	require.NoError(t, json.Unmarshal(body, &card))
	return &card
}

func TestAgentCardKeepsTheAgentsExtensions(t *testing.T) {
	agents := &fakeAgents{}
	srv, _ := serve(t, agents)

	status, body := getAgentCard(t, srv, agentCardPath, clientToken)
	require.Equal(t, http.StatusOK, status, string(body))
	var raw struct {
		Capabilities struct {
			Extensions json.RawMessage `json:"extensions"`
		} `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	want, err := json.Marshal(agentExtensions())
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(raw.Capabilities.Extensions), "the agent's extensions reach the client unchanged")

	card := decodeCard(t, body)
	require.Equal(t, "export-agent", card.Name)
	require.Equal(t, "1.2.3", card.Version)
	require.Equal(t, []string{"text/plain", "application/json"}, card.DefaultOutputModes)
	require.Equal(t, "export", card.Skills[0].ID)
	require.Equal(t, agentRef, agents.refs[len(agents.refs)-1], "the card is read for the agent the URL names")
	require.Equal(t, clientToken, agents.tokens[len(agents.tokens)-1], "the card is read as the caller")
}

func TestAgentCardNamesNothingBehindTheGateway(t *testing.T) {
	srv, card := serve(t, &fakeAgents{})

	status, body := getAgentCard(t, srv, agentCardPath, clientToken)
	require.Equal(t, http.StatusOK, status, string(body))
	// The extensions keep their own URIs (kagent.dev); nothing else may name
	// the controller, its namespace or its URLs.
	for _, leak := range []string{"kagent-controller", "agents-acme-x7k2q", "8083", "grpc://", "signatures", "iconUrl", "provider"} {
		require.NotContains(t, string(body), leak)
	}
	agentCard := decodeCard(t, body)
	require.Equal(t, []*a2a.AgentInterface{{
		URL: srv.URL + "/a2a/" + tenant, ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version,
	}}, agentCard.SupportedInterfaces, "the agent's only interface is its endpoint on the gateway")
	require.Equal(t, card.SecuritySchemes, agentCard.SecuritySchemes, "the gateway's sign-in schemes, not the controller's")
	require.Equal(t, card.SecurityRequirements, agentCard.SecurityRequirements)
}

func TestAgentCardOfAnAgentTheCallerMayNotUseIsNotFound(t *testing.T) {
	srv, _ := serve(t, &fakeAgents{})
	status, unknown := getAgentCard(t, srv, "/a2a/acme/no-such-agent/.well-known/agent-card.json", clientToken)
	require.Equal(t, http.StatusNotFound, status)
	for _, path := range []string{
		"/a2a/shared/twin/.well-known/agent-card.json",
		"/a2a/acme/broken/.well-known/agent-card.json",
		"/a2a/kagent/default-agent/.well-known/agent-card.json",
	} {
		status, body := getAgentCard(t, srv, path, clientToken)
		require.Equal(t, http.StatusNotFound, status, path)
		require.Equal(t, unknown, body, "an agent the caller may not use reads like one that does not exist")
	}

	refused := &fakeAgents{cardErr: pkga2a.ErrAgentNotPermitted}
	srv, _ = serve(t, refused)
	status, body := getAgentCard(t, srv, agentCardPath, clientToken)
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, unknown, body, "the controller's refusal reads like an unknown agent")
}

func TestAgentCardNeedsAToken(t *testing.T) {
	agents := &fakeAgents{}
	srv, _ := serve(t, agents)
	status, _ := getAgentCard(t, srv, agentCardPath, "")
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = getAgentCard(t, srv, agentCardPath, "another-token")
	require.Equal(t, http.StatusUnauthorized, status)
	require.Zero(t, agents.cardReads, "no card is read for an unauthenticated caller")
}

// postAgent sends one JSON-RPC SendMessage to the endpoint of agentTenant.
func postAgent(t *testing.T, srv *httptest.Server, agentTenant string, params map[string]any) jsonrpcResponse {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": params})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/a2a/"+agentTenant, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var body jsonrpcResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func TestAgentEndpointRefusesAnotherTenant(t *testing.T) {
	agents := &fakeAgents{}
	srv, _ := serve(t, agents)
	message := map[string]any{"messageId": "m-1", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}}

	body := postAgent(t, srv, tenant, map[string]any{"tenant": "acme/other-agent", "message": message})
	require.NotNil(t, body.Error)
	require.Equal(t, -32602, body.Error.Code, "a tenant naming another agent is invalid params")
	require.Empty(t, agents.started, "nothing starts for a mismatched tenant")

	body = postAgent(t, srv, tenant, map[string]any{"tenant": tenant, "message": message})
	require.Nil(t, body.Error, "a tenant naming the same agent is accepted")
}

func TestAgentEndpointStartsTheAgentsSession(t *testing.T) {
	agents := &fakeAgents{}
	srv, _ := serve(t, agents)
	status, body := getAgentCard(t, srv, agentCardPath, clientToken)
	require.Equal(t, http.StatusOK, status)

	c, err := a2aclient.NewFromCard(t.Context(), decodeCard(t, body), a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearerTransport{token: clientToken}}))
	require.NoError(t, err)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	_, err = c.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg})
	require.NoError(t, err)
	require.Equal(t, []string{msg.ID}, agents.started, "the message starts the agent's Session through agent-manager")
	require.Equal(t, []string{newSessionID}, agents.streamed)
	require.Contains(t, agents.refs, agentRef)
	require.NotContains(t, agents.refs, "kagent/default-agent")
}

func TestAgentCardIsReusedForAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agents := &fakeAgents{}
		card := a2aserver.NewCard(a2aserver.CardConfig{URL: "https://agents.example.com/a2a", Version: "test"})
		paths := a2aserver.NewAgentPaths(&a2aserver.Handler{Agents: agents, Card: card, Organizations: organizations{}}, "/a2a/")
		read := func() int {
			req := httptest.NewRequestWithContext(pkga2a.WithForwardedToken(t.Context(), clientToken), http.MethodGet, agentCardPath, nil)
			recorder := httptest.NewRecorder()
			paths.ServeHTTP(recorder, req)
			return recorder.Code
		}

		require.Equal(t, http.StatusOK, read())
		time.Sleep(59 * time.Second)
		require.Equal(t, http.StatusOK, read())
		require.Equal(t, 1, agents.cardReads, "a card read within a minute is reused")

		time.Sleep(2 * time.Second)
		require.Equal(t, http.StatusOK, read())
		require.Equal(t, 2, agents.cardReads, "after a minute the card is read again")
	})
}

// agentgatewayInterfaceURL ports agentgateway's rewrite of an Agent Card's
// interface URLs for a route without a URL rewrite filter
// (crates/agentgateway/src/a2a/mod.rs, rewrite_agent_card,
// build_agent_path and public_interface_url): the public card URL without
// its /.well-known/agent-card.json suffix, followed by what remains of the
// interface's path after the backend request path without that suffix.
func agentgatewayInterfaceURL(publicCardURL, backendCardPath, interfaceURL string) string {
	base := strings.TrimSuffix(publicCardURL, "/.well-known/agent-card.json")
	backendAgentPath := strings.TrimSuffix(backendCardPath, "/.well-known/agent-card.json")
	path := interfaceURL
	if i := strings.Index(interfaceURL, "://"); i >= 0 {
		if j := strings.Index(interfaceURL[i+3:], "/"); j >= 0 {
			path = interfaceURL[i+3+j:]
		} else {
			path = ""
		}
	}
	relative := path
	if rest, ok := strings.CutPrefix(path, backendAgentPath); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		relative = rest
	}
	return base + relative
}

func TestAgentCardURLSurvivesAgentgatewaysRewrite(t *testing.T) {
	const publicURL = "https://agents.example.com"
	card := a2aserver.NewCard(a2aserver.CardConfig{URL: publicURL + "/a2a", Version: "test"})
	paths := a2aserver.NewAgentPaths(&a2aserver.Handler{Agents: &fakeAgents{}, Card: card, Organizations: organizations{}}, "/a2a/")
	req := httptest.NewRequestWithContext(pkga2a.WithForwardedToken(t.Context(), clientToken), http.MethodGet, agentCardPath, nil)
	recorder := httptest.NewRecorder()
	paths.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)

	emitted := decodeCard(t, recorder.Body.Bytes()).SupportedInterfaces[0].URL
	require.Equal(t, publicURL+"/a2a/"+tenant, emitted)
	require.Equal(t, emitted, agentgatewayInterfaceURL(publicURL+agentCardPath, agentCardPath, emitted),
		"agentgateway's card rewrite leaves the gateway's agent URL as it is")
}
