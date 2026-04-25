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

func (s *stubGateway) SendCompletion(_ context.Context, _ channels.InstanceRef, _ channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
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
