package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSend_RetriesOnceOnRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	ts, err := client.send(t.Context(), "chat.update", "application/json", `{}`)
	require.NoError(t, err)
	require.Equal(t, "1.2", ts)
	require.Equal(t, int32(2), calls.Load())
}

func TestSend_FailsOnPersistentRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	_, err := client.send(t.Context(), "chat.update", "application/json", `{}`)
	require.ErrorContains(t, err, "rate limited")
	require.Equal(t, int32(2), calls.Load())
}

func TestSend_RateLimitWaitRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	_, err := client.send(ctx, "chat.update", "application/json", `{}`)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFlush_FailedUpdateIsResentOnNextFlush(t *testing.T) {
	var updates atomic.Int32
	var lastText atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if updates.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"ok":false,"error":"fatal_error"}`)
			return
		}
		lastText.Store(body.Text)
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "1.1", "1.0", slog.Default())
	w.buf.WriteString("hello")

	require.Error(t, w.flush(t.Context()))
	require.False(t, w.wroteContent(), "failed flush must not mark content as written")

	require.NoError(t, w.flush(t.Context()))
	require.Equal(t, "hello", lastText.Load(), "pending delta must be resent after a failed flush")
	require.True(t, w.wroteContent())
}

func TestLookupUserEmail_RetriesOnceOnRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"user":{"profile":{"email":"user@example.com"}}}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	email, err := client.lookupUserEmail(t.Context(), "U1")
	require.NoError(t, err)
	require.Equal(t, "user@example.com", email)
	require.Equal(t, int32(2), calls.Load())
}

// Agent-rendered text entering an mrkdwn section block must be escaped so
// quoted content cannot trigger notifications (<!channel>, <@U...>).
func TestPostApprovalPrompt_EscapesMrkdwn(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	require.NoError(t, client.postApprovalPrompt(t.Context(), "C1", "T1", "run <!channel> now?"))
	raw, _ := body.Load().(string)
	var payload struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	require.Equal(t, "run &lt;!channel&gt; now?", payload.Text)
}

func TestPostChoicePrompt_EscapesQuestion(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	require.NoError(t, client.postChoicePrompt(t.Context(), "C1", "T1", "notify <!here>?", []string{"yes", "no"}))
	raw, _ := body.Load().(string)
	var payload struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	require.Equal(t, "notify &lt;!here&gt;?", payload.Text)
}
