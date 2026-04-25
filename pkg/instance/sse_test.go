package instance_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

func TestProxySSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, line := range []string{
			"data: {\"delta\":\"hello\"}\n",
			"\n",
			"data: {\"delta\":\"world\"}\n",
			"\n",
		} {
			_, _ = fmt.Fprint(w, line)
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	resp, err := http.Get(upstream.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	rr := httptest.NewRecorder()
	require.NoError(t, instance.ProxySSE(context.Background(), rr, resp.Body))

	body := rr.Body.String()
	require.Contains(t, body, `data: {"delta":"hello"}`)
	require.Contains(t, body, `data: {"delta":"world"}`)
	require.Equal(t, "text/event-stream", rr.Header().Get("Content-Type"))
}

func TestStreamDeltas(t *testing.T) {
	src := strings.NewReader("event: chunk\ndata: {\"a\":1}\n\ndata: {\"b\":2}\n\n")
	out := make(chan instance.Delta, 4)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	require.NoError(t, instance.StreamDeltas(ctx, src, out))

	var got []instance.Delta
	for d := range out {
		got = append(got, d)
	}
	require.Len(t, got, 2)
	require.Equal(t, "chunk", got[0].Event)
	require.JSONEq(t, `{"a":1}`, string(got[0].Data))
	require.Equal(t, "", got[1].Event)
	require.JSONEq(t, `{"b":2}`, string(got[1].Data))
}

func TestClient_StreamCompletion(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	t.Cleanup(backend.Close)

	c := instance.NewClient()
	body, err := c.StreamCompletion(context.Background(),
		lifecycle.InstanceRef{Name: "i1", BaseURL: backend.URL}, []byte(`{"messages":[]}`))
	require.NoError(t, err)
	defer func() { _ = body.Close() }()
	buf, _ := io.ReadAll(body)
	require.Contains(t, string(buf), "data: ok")
}

func TestClient_CallMCPTool(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/mcp", r.URL.Path)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"messages":[{"role":"user","content":"hi"}]}}`)
	}))
	t.Cleanup(backend.Close)

	c := instance.NewClient()
	resp, err := c.Messages(context.Background(),
		lifecycle.InstanceRef{Name: "i1", BaseURL: backend.URL}, "t1")
	require.NoError(t, err)
	require.Len(t, resp.Messages, 1)
	require.Equal(t, "hi", resp.Messages[0].Content)
}
