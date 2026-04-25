package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/api"
	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

type stubManager struct {
	refs map[string]lifecycle.InstanceRef
}

func (s *stubManager) Get(_ context.Context, name string) (lifecycle.InstanceRef, error) {
	ref, ok := s.refs[name]
	if !ok {
		return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
	}
	return ref, nil
}

type stubStreamer struct {
	sse           string
	sseErr        error
	messagesResp  instance.MessagesResponse
	messagesErr   error
	gotBody       []byte
	gotThreadID   string
	gotInstanceID string
}

func (s *stubStreamer) StreamCompletion(_ context.Context, ref lifecycle.InstanceRef, body []byte) (io.ReadCloser, error) {
	s.gotBody = append([]byte(nil), body...)
	s.gotInstanceID = ref.Name
	if s.sseErr != nil {
		return nil, s.sseErr
	}
	return io.NopCloser(strings.NewReader(s.sse)), nil
}

func (s *stubStreamer) Messages(_ context.Context, _ lifecycle.InstanceRef, threadID string) (instance.MessagesResponse, error) {
	s.gotThreadID = threadID
	if s.messagesErr != nil {
		return instance.MessagesResponse{}, s.messagesErr
	}
	return s.messagesResp, nil
}

func newRouter(h *api.Handler) http.Handler {
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func TestCompletions_SSEPassThrough(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	streamer := &stubStreamer{sse: sse}
	mgr := &stubManager{refs: map[string]lifecycle.InstanceRef{
		"i1": {Name: "i1", BaseURL: "http://i1"},
	}}
	h := &api.Handler{Manager: mgr, Streamer: streamer}

	ts := httptest.NewServer(newRouter(h))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/i1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, sse, string(body), "stream must be byte-identical to upstream")

	// The forwarded body reaches the instance unchanged.
	require.Contains(t, string(streamer.gotBody), `"ping"`)
	require.Equal(t, "i1", streamer.gotInstanceID)
}

func TestCompletions_NotFound(t *testing.T) {
	streamer := &stubStreamer{}
	mgr := &stubManager{refs: map[string]lifecycle.InstanceRef{}}
	h := &api.Handler{Manager: mgr, Streamer: streamer}

	ts := httptest.NewServer(newRouter(h))
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/missing/chat/completions", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestCompletions_UpstreamError(t *testing.T) {
	streamer := &stubStreamer{sseErr: errors.New("upstream blew up")}
	mgr := &stubManager{refs: map[string]lifecycle.InstanceRef{
		"i1": {Name: "i1", BaseURL: "http://i1"},
	}}
	h := &api.Handler{Manager: mgr, Streamer: streamer}

	ts := httptest.NewServer(newRouter(h))
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/i1/chat/completions", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestCompletions_UpstreamTimeout(t *testing.T) {
	streamer := &stubStreamer{sseErr: fmt.Errorf("dial: %w", context.DeadlineExceeded)}
	mgr := &stubManager{refs: map[string]lifecycle.InstanceRef{
		"i1": {Name: "i1", BaseURL: "http://i1"},
	}}
	h := &api.Handler{Manager: mgr, Streamer: streamer}

	ts := httptest.NewServer(newRouter(h))
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/i1/chat/completions", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
}

func TestMessages_ReturnsHistory(t *testing.T) {
	streamer := &stubStreamer{
		messagesResp: instance.MessagesResponse{
			Messages: []instance.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
		},
	}
	mgr := &stubManager{refs: map[string]lifecycle.InstanceRef{
		"i1": {Name: "i1", BaseURL: "http://i1"},
	}}
	h := &api.Handler{Manager: mgr, Streamer: streamer}

	ts := httptest.NewServer(newRouter(h))
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/i1/chat/messages?thread_id=t1", "application/json", strings.NewReader(""))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got instance.MessagesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got.Messages, 2)
	require.Equal(t, "t1", streamer.gotThreadID)
}

func TestMessages_ReadsThreadIDFromBody(t *testing.T) {
	streamer := &stubStreamer{messagesResp: instance.MessagesResponse{}}
	mgr := &stubManager{refs: map[string]lifecycle.InstanceRef{"i1": {Name: "i1"}}}
	h := &api.Handler{Manager: mgr, Streamer: streamer}

	ts := httptest.NewServer(newRouter(h))
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/i1/chat/messages", "application/json", strings.NewReader(`{"thread_id":"from-body"}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "from-body", streamer.gotThreadID)
}
