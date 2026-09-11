package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/channels/web"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

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

// A turn paused on a prompt ends the stream with a prompt event carrying the
// task to resume and what is asked; the decision comes back on the task.
func TestPostMessages_PromptEventAndDecision(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{
		Kind: channels.DeltaPrompt, Content: "Delete the pod?", TaskID: "task-7",
		Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete", Hint: "Delete the pod?", Tools: []channels.HitlTool{{ID: "approval-1", Name: "kubectl_delete", Args: map[string]any{"pod": "web-1"}}}},
	}}}
	ts := newServer(t, gw)

	body := `{"channelId":"c1","userId":"u1","threadId":"t1","text":"delete web-1"}`
	resp, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	buf, _ := io.ReadAll(resp.Body)
	raw := string(buf)
	require.Contains(t, raw, "event: prompt\n")
	require.Contains(t, raw, `"taskId":"task-7"`)
	require.Contains(t, raw, `"toolName":"kubectl_delete"`)
	require.Contains(t, raw, `"id":"approval-1"`)
	require.NotContains(t, raw, "event: done", "a paused turn is not done")

	gw.deltas = []channels.OutboundDelta{{Content: "deleted"}, {Done: true}}
	body = `{"channelId":"c1","userId":"u1","threadId":"t1","text":"approve","taskId":"task-7","decision":{"type":"approve"}}`
	resp2, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	require.Equal(t, "task-7", gw.sendInbound.TaskID, "the decision resumes the paused task")
	require.NotNil(t, gw.sendInbound.Decision)
	require.Equal(t, channels.DecisionApprove, gw.sendInbound.Decision.Type)

	// An ask_user answer travels positionally; a decision needs its task.
	body = `{"channelId":"c1","userId":"u1","threadId":"t1","taskId":"task-8","decision":{"type":"approve","askUserAnswers":[["Health check"]]}}`
	resp3, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp3.Body.Close() }()
	require.Equal(t, http.StatusOK, resp3.StatusCode, "a decision without text is a complete message")
	require.Equal(t, [][]string{{"Health check"}}, gw.sendInbound.Decision.AskUserAnswers)

	body = `{"channelId":"c1","userId":"u1","threadId":"t1","decision":{"type":"approve"}}`
	resp4, err := http.Post(ts.URL+"/web/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp4.Body.Close() }()
	require.Equal(t, http.StatusBadRequest, resp4.StatusCode)
}

type listingGateway struct {
	stubGateway
	agents []pkga2a.AgentInfo
	gotCtx context.Context
}

func (g *listingGateway) ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	g.gotCtx = ctx
	return g.agents, nil
}

func TestGetAgents(t *testing.T) {
	gw := &listingGateway{agents: []pkga2a.AgentInfo{{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent", IconURL: "https://icons/sre.png", Description: "Investigates"}}}
	ts := newServer(t, gw)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/web/agents", nil)
	req.Header.Set("Authorization", "Bearer user-jwt")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got struct {
		Agents []map[string]any `json:"agents"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got.Agents, 1)
	require.Equal(t, "SRE Agent", got.Agents[0]["displayName"])
	require.Equal(t, "https://icons/sre.png", got.Agents[0]["iconUrl"])
	require.Equal(t, "user-jwt", pkga2a.ForwardedTokenFromContext(gw.gotCtx), "the roster is read as the caller")

	// A gateway without discovery says so.
	plain := newServer(t, &stubGateway{})
	resp2, err := http.Get(plain.URL + "/web/agents")
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp2.StatusCode)
}
