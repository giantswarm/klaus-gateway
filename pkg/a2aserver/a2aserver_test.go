package a2aserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
	"github.com/giantswarm/klaus-gateway/pkg/server"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

const (
	clientToken    = "app-token"
	exchangedToken = "exchanged-token"
	tenant         = "acme/export-agent"
	agentRef       = "agents-acme-x7k2q/export-agent"
	newSessionID   = "session-1"
)

// issuerConnector is the connector the trusted issuer's tokens are exchanged
// through.
const issuerConnector = "dex-self"

type staticVerifier struct{ accept string }

func (v staticVerifier) Verify(_ context.Context, raw string) (a2aserver.TrustedIssuer, error) {
	if raw != v.accept {
		return a2aserver.TrustedIssuer{}, errors.New("bad token")
	}
	return a2aserver.TrustedIssuer{Name: "dex", ConnectorID: issuerConnector}, nil
}

type staticExchanger struct{ err error }

func (e staticExchanger) Exchange(_ context.Context, subject string, issuer a2aserver.TrustedIssuer) (string, error) {
	if e.err != nil {
		return "", e.err
	}
	if subject != clientToken || issuer.ConnectorID != issuerConnector {
		return "", a2aserver.ErrExchangeRefused
	}
	return exchangedToken, nil
}

// fakeAgents records what the channel asks of the platform.
type fakeAgents struct {
	mu       sync.Mutex
	started  []string // request ids
	streamed []string // session ids
	tokens   []string
	refs     []string
	// metadata and extensions are what each turn passes on to the agent
	// runtime: the propagated request metadata and A2A-Extensions values.
	metadata      []map[string]any
	extensions    [][]string
	streamErr     error
	configuration *a2a.SendMessageConfig
}

// propagatedExtensions are the A2A-Extensions values ctx passes on.
func propagatedExtensions(ctx context.Context) []string {
	for name, values := range a2aext.GetRequestHeaders(ctx) {
		if strings.EqualFold(name, a2a.SvcParamExtensions) {
			return values
		}
	}
	return nil
}

func (f *fakeAgents) ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	f.record(ctx)
	return []pkga2a.AgentInfo{
		{Namespace: "agents-acme-x7k2q", Name: "export-agent", DisplayName: "Export agent"},
		{Namespace: "agents-acme-x7k2q", Name: "broken", Unavailable: "no model"},
		{Namespace: "kagent", Name: "default-agent"},
		{Namespace: "agents-shared-a1b2c", Name: "twin"},
	}, nil
}

func (f *fakeAgents) StartSession(ctx context.Context, agentRef, requestID, _ string, _ workspaces.Grant) (pkga2a.Session, error) {
	f.record(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, requestID)
	return pkga2a.Session{ID: newSessionID, State: "READY"}, nil
}

func (f *fakeAgents) StreamWithConfig(ctx context.Context, sessionID string, msg *a2a.Message, configuration *a2a.SendMessageConfig) iter.Seq2[a2a.Event, error] {
	f.record(ctx)
	f.mu.Lock()
	f.configuration = configuration
	f.streamed = append(f.streamed, sessionID)
	f.metadata = append(f.metadata, a2aext.GetMetadata(ctx))
	f.extensions = append(f.extensions, propagatedExtensions(ctx))
	err := f.streamErr
	f.mu.Unlock()
	return func(yield func(a2a.Event, error) bool) {
		if err != nil {
			yield(nil, err)
			return
		}
		info := a2a.TaskInfo{TaskID: "task-1", ContextID: sessionID}
		if !yield(&a2a.Task{ID: info.TaskID, ContextID: sessionID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil), nil)
	}
}

func (f *fakeAgents) SendMessage(ctx context.Context, sessionID string, message *a2a.Message, configuration *a2a.SendMessageConfig) (a2a.SendMessageResult, error) {
	for _, err := range f.StreamWithConfig(ctx, sessionID, message, configuration) {
		if err != nil {
			return nil, err
		}
	}
	return f.GetTaskWithHistory(ctx, "task-1", nil)
}

func (f *fakeAgents) Subscribe(context.Context, a2a.TaskID) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func (f *fakeAgents) GetTaskWithHistory(ctx context.Context, id a2a.TaskID, _ *int) (*a2a.Task, error) {
	f.record(ctx)
	return &a2a.Task{ID: id, ContextID: newSessionID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
}

func (f *fakeAgents) CancelTask(_ context.Context, id a2a.TaskID) (*a2a.Task, error) {
	return &a2a.Task{ID: id, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

func (f *fakeAgents) record(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, pkga2a.ForwardedTokenFromContext(ctx))
	f.refs = append(f.refs, pkga2a.AgentRefFromContext(ctx))
}

// organizations knows the organization acme, and two organizations named
// shared under different parents.
type organizations struct{}

func (organizations) Organization(namespace string) (string, string, bool) {
	switch namespace {
	case "agents-acme-x7k2q":
		return "acme", "agentlab-organizations", true
	case "agents-shared-a1b2c":
		return "shared", "org-a", true
	}
	return "", "", false
}

func (organizations) Shared(name string) bool { return name == "shared" }

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

// testBodyLimit is the request body cap of the tests' channel.
const testBodyLimit = 4 << 10

// sessionKey identifies the tests' application session metadata.
const sessionKey = "com.example/session"

func serve(t *testing.T, agents a2aserver.Agents, exchanger a2aserver.TokenExchanger) (*httptest.Server, *a2a.AgentCard) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(server.New(server.Options{Public: mux}).PublicHandler())
	t.Cleanup(srv.Close)
	card := a2aserver.NewCard(a2aserver.CardConfig{URL: srv.URL + "/a2a", Version: "test", Issuers: []a2aserver.TrustedIssuer{
		{Name: "dex", Issuer: "https://dex.example", Audience: clientAudience, TokenURL: "https://dex.example/token"}, //nolint:gosec // G101: a test URL
		{Name: "outside", Issuer: "https://idp.example.org", Audience: outsideAudience, OpenIDConnectURL: "https://idp.example.org/.well-known/openid-configuration"},
		{Name: "rfc8414", Issuer: "https://as.example.net", Audience: outsideAudience, TokenURL: "https://as.example.net/oauth/token", MetadataURL: "https://as.example.net/.well-known/oauth-authorization-server"}, //nolint:gosec // G101: a test URL
	}})
	auth := &a2aserver.Authenticator{Verifier: staticVerifier{accept: clientToken}, Exchanger: exchanger, Realm: "klaus-gateway"}
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	handler := a2aserver.NewRequestHandler(&a2aserver.Handler{Agents: agents, Card: card, Organizations: organizations{}})
	mux.Handle("/a2a", a2aserver.LimitBody(testBodyLimit, auth.Middleware(a2asrv.NewJSONRPCHandler(handler))))
	return srv, card
}

func client(t *testing.T, card *a2a.AgentCard, token string, extensions ...string) *a2aclient.Client {
	t.Helper()
	c, err := a2aclient.NewFromCard(t.Context(), card,
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearerTransport{token: token}}),
		a2aclient.WithCallInterceptors(a2aext.NewActivator(extensions...)),
	)
	require.NoError(t, err)
	return c
}

func TestUnauthenticatedRequestsAreChallenged(t *testing.T) {
	srv, _ := serve(t, &fakeAgents{}, staticExchanger{})
	for name, test := range map[string]struct {
		header    string
		challenge string
	}{
		"no token":      {challenge: `Bearer realm="klaus-gateway"`},
		"invalid token": {header: "Bearer forged", challenge: `Bearer realm="klaus-gateway", error="invalid_token", error_description="the access token is invalid or expired"`},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/a2a", nil)
			require.NoError(t, err)
			if test.header != "" {
				req.Header.Set("Authorization", test.header)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			require.Equal(t, test.challenge, resp.Header.Get("WWW-Authenticate"))
		})
	}
}

func TestRefusedExchangeIsChallenged(t *testing.T) {
	srv, _ := serve(t, &fakeAgents{}, staticExchanger{err: a2aserver.ErrExchangeRefused})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/a2a", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Contains(t, resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`)
}

func TestPublicCardOffersEachIssuer(t *testing.T) {
	srv, _ := serve(t, &fakeAgents{}, staticExchanger{})
	resp, err := http.Get(srv.URL + a2asrv.WellKnownAgentCardPath)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var card struct {
		SupportedInterfaces []map[string]any          `json:"supportedInterfaces"`
		Capabilities        map[string]any            `json:"capabilities"`
		SecuritySchemes     map[string]map[string]any `json:"securitySchemes"`
		Security            []map[string]any          `json:"securityRequirements"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&card))

	require.Equal(t, map[string]any{
		"oauth2SecurityScheme": map[string]any{
			"description": "A token issued by https://dex.example for the audience klaus-gateway-app.",
			"flows":       map[string]any{"clientCredentials": map[string]any{"tokenUrl": "https://dex.example/token", "scopes": map[string]any{"openid": "OpenID Connect"}}}, //nolint:gosec // G101: a test URL
		},
	}, card.SecuritySchemes["dex"], "an OpenID-only issuer's discovery document is no RFC 8414 metadata")
	require.Equal(t, map[string]any{
		"oauth2SecurityScheme": map[string]any{
			"description":       "A token issued by https://as.example.net for the audience klaus-gateway-outside.",
			"flows":             map[string]any{"clientCredentials": map[string]any{"tokenUrl": "https://as.example.net/oauth/token", "scopes": map[string]any{"openid": "OpenID Connect"}}}, //nolint:gosec // G101: a test URL
			"oauth2MetadataUrl": "https://as.example.net/.well-known/oauth-authorization-server",
		},
	}, card.SecuritySchemes["rfc8414"])
	require.Equal(t, map[string]any{
		"openIdConnectSecurityScheme": map[string]any{
			"description":      "A token issued by https://idp.example.org for the audience klaus-gateway-outside.",
			"openIdConnectUrl": "https://idp.example.org/.well-known/openid-configuration",
		},
	}, card.SecuritySchemes["outside"], "an issuer without a token endpoint names its metadata only, never the exchange's identity provider")
	require.Len(t, card.Security, 3, "any one scheme is enough")
	require.Empty(t, card.Capabilities["extensions"])
	require.Len(t, card.SupportedInterfaces, 1)
	require.Equal(t, "1.0", card.SupportedInterfaces[0]["protocolVersion"])
	require.NotContains(t, card.SupportedInterfaces[0], "tenant", "the public card names no agent")
}

func TestConversationForwardsRawMetadataWithoutActivation(t *testing.T) {
	agents := &fakeAgents{}
	_, card := serve(t, agents, staticExchanger{})
	caller := client(t, card, clientToken)
	metadata := map[string]any{sessionKey: map[string]any{"sid": "ref-1"}, "format": "csv", "com.example/options": map[string]any{"any nested field": true}}
	first := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("export as csv"))
	result, err := caller.SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: tenant, Message: first, Metadata: metadata})
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, newSessionID, task.ContextID)
	next := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("and as json"))
	next.ContextID = task.ContextID
	_, err = caller.SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: tenant, Message: next})
	require.NoError(t, err)
	require.Equal(t, []string{first.ID}, agents.started)
	require.Equal(t, []string{newSessionID, newSessionID}, agents.streamed)
	require.Equal(t, metadata, agents.metadata[0])
	require.Empty(t, agents.metadata[1])
	for index, token := range agents.tokens {
		require.Equal(t, exchangedToken, token)
		if agents.refs[index] != "" {
			require.Equal(t, agentRef, agents.refs[index])
		}
	}
	for _, extensions := range agents.extensions {
		require.Empty(t, extensions)
	}
}

func TestRequestMetadataValuesRemainOpaque(t *testing.T) {
	for name, metadata := range map[string]map[string]any{
		"omitted": nil, "empty": {}, "null value": {sessionKey: nil}, "scalar": {sessionKey: 42.0}, "array": {sessionKey: []any{"ref-1", true}},
	} {
		t.Run(name, func(t *testing.T) {
			agents := &fakeAgents{}
			server, _ := serve(t, agents, staticExchanger{})
			response, body := postJSONRPC(t, server.URL, "", map[string]any{"tenant": tenant, "message": map[string]any{"messageId": "m-1", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}}, "metadata": metadata})
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.Nil(t, body.Error)
			require.Empty(t, response.Header.Get("A2A-Extensions"))
			require.Len(t, agents.streamed, 1)
			if len(metadata) == 0 {
				require.Empty(t, agents.metadata[0])
			} else {
				require.Equal(t, metadata, agents.metadata[0])
			}
			require.Empty(t, agents.extensions[0])
		})
	}
}

func TestMalformedRequestMetadataIsRefusedBeforeDispatch(t *testing.T) {
	for _, key := range []string{"not valid/key", "io.modelcontextprotocol/protocolVersion", "io.modelcontextprotocol/clientInfo", "io.modelcontextprotocol/clientCapabilities", "progressToken", "traceparent"} {
		t.Run(key, func(t *testing.T) {
			agents := &fakeAgents{}
			server, _ := serve(t, agents, staticExchanger{})
			response, body := postJSONRPC(t, server.URL, "", map[string]any{"tenant": tenant, "message": map[string]any{"messageId": "m-1", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}}, "metadata": map[string]any{key: "ref-1"}})
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NotNil(t, body.Error)
			require.Equal(t, -32602, body.Error.Code)
			require.NotContains(t, body.Error.Message, "ref-1")
			require.Empty(t, agents.started)
			require.Empty(t, agents.streamed)
			require.Empty(t, agents.refs)
		})
	}
}

func TestStreamingMetadataNeedsNoExtensionHeader(t *testing.T) {
	agents := &fakeAgents{}
	server, _ := serve(t, agents, staticExchanger{})
	values := map[string]any{sessionKey: "ref-1"}
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "SendStreamingMessage", "params": map[string]any{"tenant": tenant, "message": map[string]any{"messageId": "m-1", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}}}, "metadata": values}})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/a2a", bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+clientToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("A2A-Version", "1.0")
	request.Header.Set("A2A-Extensions", "https://example.com/extensions/other/v1")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	events, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
	require.Empty(t, response.Header.Get("A2A-Extensions"))
	require.Contains(t, string(events), "TASK_STATE_COMPLETED")
	require.Equal(t, values, agents.metadata[0])
	require.Empty(t, agents.extensions[0])
}

type jsonrpcResponse struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func postJSONRPC(t *testing.T, base, extensions string, params map[string]any) (*http.Response, jsonrpcResponse) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": params})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/a2a", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if extensions != "" {
		req.Header.Set("A2A-Extensions", extensions)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var body jsonrpcResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp, body
}

func TestTenantMustNameTheAgent(t *testing.T) {
	_, card := serve(t, &fakeAgents{}, staticExchanger{})
	c := client(t, card, clientToken)
	_, err := c.SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: "export-agent", Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
	require.ErrorIs(t, err, a2a.ErrInvalidParams)
}

func TestOnlyOrganizationAgentsAreAddressable(t *testing.T) {
	agents := &fakeAgents{}
	_, card := serve(t, agents, staticExchanger{})
	c := client(t, card, clientToken)
	for _, refused := range []string{"kagent/default-agent", "shared/twin", "acme/broken", "acme/unknown", "agents-acme-x7k2q/export-agent"} {
		_, err := c.SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: refused, Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
		require.ErrorIs(t, err, a2a.ErrUnauthorized, refused)
	}
	require.Empty(t, agents.started, "no Session starts for an agent not offered")
}

func TestPlatformErrorsNameNothingBehindTheGateway(t *testing.T) {
	agents := &fakeAgents{streamErr: errors.New("kagent controller kagent-controller.kagent.svc: connection refused")}
	_, card := serve(t, agents, staticExchanger{})
	c := client(t, card, clientToken)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.ContextID = newSessionID
	_, err := c.SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: tenant, Message: msg})
	require.ErrorIs(t, err, a2a.ErrInternalError)
	require.NotContains(t, err.Error(), "kagent")
}

func TestExtendedCardListsTheCallersAgents(t *testing.T) {
	agents := &fakeAgents{}
	_, card := serve(t, agents, staticExchanger{})
	c := client(t, card, clientToken)
	extended, err := c.GetExtendedAgentCard(t.Context(), &a2a.GetExtendedAgentCardRequest{})
	require.NoError(t, err)
	require.Len(t, extended.SupportedInterfaces, 1, "unavailable agents, agents outside an organization and of an ambiguous organization are left out")
	require.Equal(t, tenant, extended.SupportedInterfaces[0].Tenant)
	require.Equal(t, card.SupportedInterfaces[0].URL, extended.SupportedInterfaces[0].URL)
}

func TestExchangerSendsAnRFC8693Request(t *testing.T) {
	var form url.Values
	var user, pass string
	dex := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		form = r.PostForm
		user, pass, _ = r.BasicAuth()
		if form.Get("subject_token") != clientToken {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"invalid subject_token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + exchangedToken + `","issued_token_type":"urn:ietf:params:oauth:token-type:id_token","token_type":"bearer"}`))
	}))
	t.Cleanup(dex.Close)
	e := &a2aserver.Exchanger{TokenURL: dex.URL, ClientID: "klaus-gateway-app", ClientSecret: "s3cr3t:/", Audiences: []string{"kagent", "dex-k8s-authenticator"}, HTTPClient: dex.Client()}

	token, err := e.Exchange(t.Context(), clientToken, a2aserver.TrustedIssuer{ConnectorID: "dex-self"})
	require.NoError(t, err)
	require.Equal(t, exchangedToken, token)
	require.Equal(t, "urn:ietf:params:oauth:grant-type:token-exchange", form.Get("grant_type"))
	require.Equal(t, "urn:ietf:params:oauth:token-type:id_token", form.Get("subject_token_type"))
	require.Equal(t, "urn:ietf:params:oauth:token-type:id_token", form.Get("requested_token_type"))
	require.Equal(t, "dex-self", form.Get("connector_id"))
	require.Equal(t, "openid email profile groups audience:server:client_id:kagent audience:server:client_id:dex-k8s-authenticator", form.Get("scope"))
	require.Equal(t, "klaus-gateway-app", user)
	require.Equal(t, url.QueryEscape("s3cr3t:/"), pass)

	_, err = e.Exchange(t.Context(), "forged", a2aserver.TrustedIssuer{ConnectorID: "dex-self"})
	require.ErrorIs(t, err, a2aserver.ErrExchangeRefused)

	_, err = e.Exchange(t.Context(), clientToken, a2aserver.TrustedIssuer{AccessTokens: true})
	require.NoError(t, err)
	require.NotContains(t, form, "connector_id", "an issuer without a connector names none")
	require.Equal(t, "urn:ietf:params:oauth:token-type:access_token", form.Get("subject_token_type"), "an access token is named as one")
	require.Equal(t, "urn:ietf:params:oauth:token-type:id_token", form.Get("requested_token_type"), "the platform still gets an ID token")
}

func TestOversizedBodyIsRefusedBeforeAnything(t *testing.T) {
	agents := &fakeAgents{}
	srv, _ := serve(t, agents, staticExchanger{})
	text := strings.Repeat("x", testBodyLimit)
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{
		"tenant":  tenant,
		"message": map[string]any{"messageId": "m-1", "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}},
	}})
	require.NoError(t, err)

	for name, body := range map[string]io.Reader{
		"declared length": bytes.NewReader(payload),
		"chunked":         io.MultiReader(bytes.NewReader(payload)),
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/a2a", body)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+clientToken)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
		})
	}
	require.Empty(t, agents.refs, "nothing is asked of the platform")

	small := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(strings.Repeat("x", testBodyLimit/2)))
	_, err = client(t, &a2a.AgentCard{SupportedInterfaces: []*a2a.AgentInterface{{URL: srv.URL + "/a2a", ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version}}}, clientToken).
		SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: tenant, Message: small})
	require.NoError(t, err, "a body under the cap is served whole")
}
