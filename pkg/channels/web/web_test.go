package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/channels/web"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

type stubVerifier struct {
	sub string
	err error
}

func (s stubVerifier) Verify(context.Context, string) (string, error) { return s.sub, s.err }

func serveAdapter(t *testing.T, a *web.Adapter, gw channels.Gateway) *httptest.Server {
	t.Helper()
	require.NoError(t, a.Start(t.Context(), gw))
	r := chi.NewRouter()
	a.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

func postWithBearer(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

type stubGateway struct {
	resolveRef     channels.InstanceRef
	resolveErr     error
	deltas         []channels.OutboundDelta
	sendErr        error
	history        []channels.Message
	historyErr     error
	resolveInbound channels.InboundMessage
	sendInbound    channels.InboundMessage
}

func (s *stubGateway) Resolve(_ context.Context, in channels.InboundMessage) (channels.InstanceRef, error) {
	s.resolveInbound = in
	if s.resolveErr != nil {
		return channels.InstanceRef{}, s.resolveErr
	}
	if s.resolveRef.Name == "" {
		s.resolveRef.Name = "i1"
	}
	return s.resolveRef, nil
}

func (s *stubGateway) SendCompletion(_ context.Context, _ channels.InstanceRef, msg channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	s.sendInbound = msg
	if s.sendErr != nil {
		return nil, s.sendErr
	}
	ch := make(chan channels.OutboundDelta, len(s.deltas))
	go func() {
		for _, d := range s.deltas {
			ch <- d
		}
		close(ch)
	}()
	return ch, nil
}

func (s *stubGateway) FetchHistory(context.Context, channels.InstanceRef) ([]channels.Message, error) {
	if s.historyErr != nil {
		return nil, s.historyErr
	}
	return s.history, nil
}

func newServer(t *testing.T, gw channels.Gateway) *httptest.Server {
	t.Helper()
	a := &web.Adapter{}
	require.NoError(t, a.Start(context.Background(), gw))
	r := chi.NewRouter()
	a.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

func newServerWithDefaultAgent(t *testing.T, gw channels.Gateway, defaultAgent string) *httptest.Server {
	t.Helper()
	a := &web.Adapter{DefaultAgent: defaultAgent}
	require.NoError(t, a.Start(t.Context(), gw))
	r := chi.NewRouter()
	a.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

func TestPostMessages_StreamsSSE(t *testing.T) {
	gw := &stubGateway{
		resolveRef: channels.InstanceRef{Name: "test-instance"},
		deltas: []channels.OutboundDelta{
			{Content: "hel"},
			{Content: "lo"},
			{Done: true},
		},
	}
	ts := newServer(t, gw)

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi"}`
	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "test-instance", resp.Header.Get("X-Klaus-Instance"))

	buf, _ := io.ReadAll(resp.Body)
	raw := string(buf)
	require.Contains(t, raw, `"content":"hel"`)
	require.Contains(t, raw, `"content":"lo"`)
	require.Contains(t, raw, "event: done")

	require.Equal(t, "web", gw.resolveInbound.Channel)
	require.Equal(t, "c1", gw.resolveInbound.ChannelID)
	require.Equal(t, "u1", gw.resolveInbound.UserID)
	require.Equal(t, "t1", gw.resolveInbound.ThreadID)
	require.Equal(t, "hi", gw.resolveInbound.Text)
}

func TestPostMessages_MissingFields(t *testing.T) {
	ts := newServer(t, &stubGateway{})
	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(`{"userId":"u1"}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPostMessages_ResolveRouteNotFound(t *testing.T) {
	gw := &stubGateway{resolveErr: routing.ErrRouteNotFound}
	ts := newServer(t, gw)
	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi"}`
	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestGetMessages_ReturnsHistory(t *testing.T) {
	gw := &stubGateway{history: []channels.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}}
	ts := newServer(t, gw)

	resp, err := http.Get(ts.URL + "/web/messages?channelId=c1&userId=u1&threadId=t1")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		Messages []channels.Message `json:"messages"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got.Messages, 2)
	require.Equal(t, "hello", got.Messages[1].Content)
}

func TestGetMessages_MissingParams(t *testing.T) {
	ts := newServer(t, &stubGateway{})
	resp, err := http.Get(ts.URL + "/web/messages")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHealthz_AfterStart(t *testing.T) {
	ts := newServer(t, &stubGateway{})
	resp, err := http.Get(ts.URL + "/web/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHealthz_BeforeStart(t *testing.T) {
	a := &web.Adapter{}
	r := chi.NewRouter()
	a.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/web/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestPostMessages_DefaultAgentSet(t *testing.T) {
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Done: true}},
	}
	ts := newServerWithDefaultAgent(t, gw, "my-agent")

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi"}`
	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "my-agent", gw.sendInbound.AgentRef)
}

func TestPostMessages_PerRequestAgentRefOverridesDefault(t *testing.T) {
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Done: true}},
	}
	ts := newServerWithDefaultAgent(t, gw, "default-agent")

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi","agentRef":"override-agent"}`
	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "override-agent", gw.sendInbound.AgentRef)
}

func TestPostMessages_NilVerifierSkipsAndForwardsToken(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Done: true}}}
	ts := serveAdapter(t, &web.Adapter{}, gw)

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi"}`
	resp := postWithBearer(t, ts.URL+"/web/messages", "raw-token", body)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "raw-token", gw.sendInbound.BearerToken)
}

func TestPostMessages_VerifierRejectsWith401(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Done: true}}}
	ts := serveAdapter(t, &web.Adapter{Verifier: stubVerifier{err: errors.New("bad token")}}, gw)

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi"}`
	resp := postWithBearer(t, ts.URL+"/web/messages", "bad", body)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPostMessages_VerifiedSubjectOverridesBody(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Done: true}}}
	ts := serveAdapter(t, &web.Adapter{Verifier: stubVerifier{sub: "verified-sub"}}, gw)

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi","subject":"client-claimed"}`
	resp := postWithBearer(t, ts.URL+"/web/messages", "good", body)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "verified-sub", gw.sendInbound.Subject)
}

func TestPostMessages_NoAgentRefFallsBackToOpenAIPath(t *testing.T) {
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Done: true}},
	}
	ts := newServer(t, gw)

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"hi"}`
	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "", gw.sendInbound.AgentRef)
}
