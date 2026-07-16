package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

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

func TestSend_SurfacesHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, "<html>gateway error</html>")
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	_, err := client.send(t.Context(), "chat.update", "application/json", `{}`)
	require.ErrorContains(t, err, "http status 500")
}

func TestSend_FailsFastOnHugeRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	_, err := client.send(t.Context(), "chat.update", "application/json", `{}`)
	require.ErrorContains(t, err, "rate limited")
	require.Equal(t, int32(1), calls.Load(), "a wait beyond the cap must not be slept through")
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
	w := newBatchedWriterWithClient(client, "C1", "1.1", "1.0", detailsOff, slog.Default())
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

// An auto-approved read-only prompt resumes the turn in place by calling run()
// again on the same writer. run() defers drainToolPosts, so draining must be
// idempotent and re-init the queue for the resumed segment; otherwise the
// second drain re-closes a closed channel and panics the whole process.
func TestDrainToolPosts_IdempotentAcrossRunCycles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "1.1", "1.0", detailsOn, slog.Default())

	// First segment queued tool activity and drained (as run()'s defer does).
	w.enqueueToolPost(t.Context(), "tool one")
	require.NotPanics(t, w.drainToolPosts)

	// Resumed segment over the same writer: a fresh poster starts and drains
	// without re-closing the first segment's queue.
	require.NotPanics(t, func() {
		w.enqueueToolPost(t.Context(), "tool two")
		w.drainToolPosts()
	})

	// Draining with nothing queued stays a no-op.
	require.NotPanics(t, w.drainToolPosts)
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

// The top-level text of a markdown-block message is the notification fallback
// and is mrkdwn-parsed by Slack, so it must be escaped even though the markdown
// block itself carries the raw text.
func TestPostMarkdown_EscapesFallbackText(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	_, err := client.postMarkdown(t.Context(), "C1", "ping <!channel> now", "1.0")
	require.NoError(t, err)
	payload := decodeBlocksPayload(t, body)
	require.Equal(t, "ping &lt;!channel&gt; now", payload.Text)
	require.Equal(t, "ping <!channel> now", payload.Blocks[0].Text, "markdown block keeps the raw text")
}

func TestChatUpdateMarkdown_EscapesFallbackText(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	require.NoError(t, client.chatUpdateMarkdown(t.Context(), "C1", "1.2", "cc <@U123>"))
	payload := decodeBlocksPayload(t, body)
	require.Equal(t, "cc &lt;@U123&gt;", payload.Text)
	require.Equal(t, "cc <@U123>", payload.Blocks[0].Text, "markdown block keeps the raw text")
}

type blocksPayload struct {
	Text   string `json:"text"`
	Blocks []struct {
		Text string `json:"text"`
	} `json:"blocks"`
}

func decodeBlocksPayload(t *testing.T, body atomic.Value) blocksPayload {
	t.Helper()
	raw, _ := body.Load().(string)
	var payload blocksPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	require.NotEmpty(t, payload.Blocks)
	return payload
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

// sectionPayload decodes a prompt message far enough to read the section
// block's mrkdwn text object.
type sectionPayload struct {
	Blocks []struct {
		Type string `json:"type"`
		Text struct {
			Text string `json:"text"`
		} `json:"text"`
	} `json:"blocks"`
}

// An oversized prompt must be truncated to Slack's 3000-char section limit;
// otherwise the whole message is rejected with invalid_blocks and the paused
// task is stranded with no visible prompt.
func TestPostApprovalPrompt_TruncatesOversizedSection(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	oversized := strings.Repeat("日", 5000)
	require.NoError(t, client.postApprovalPrompt(t.Context(), "C1", "T1", oversized))

	raw, _ := body.Load().(string)
	var payload sectionPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	section := payload.Blocks[0].Text.Text
	require.LessOrEqual(t, len([]rune(section)), slackSectionTextMax)
	require.True(t, strings.HasSuffix(section, "…"), "truncation marker expected")
	require.True(t, utf8.ValidString(section))
}

func TestPostChoicePrompt_TruncatesOversizedSection(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	oversized := strings.Repeat("q", 5000)
	require.NoError(t, client.postChoicePrompt(t.Context(), "C1", "T1", oversized, []string{"yes"}))

	raw, _ := body.Load().(string)
	var payload sectionPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	section := payload.Blocks[0].Text.Text
	require.LessOrEqual(t, len([]rune(section)), slackSectionTextMax)
	require.Contains(t, section, "…", "truncation marker expected")
}

func TestRespondURL_ErrorsOnNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := respondURL(t.Context(), srv.URL, "updated")
	require.Error(t, err)
	require.Contains(t, err.Error(), "http status 500")
}

func TestRespondURL_SucceedsOn2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, respondURL(t.Context(), srv.URL, "updated"))
}

func TestThreadInitiator_ReturnsFirstHumanAuthor(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotQuery = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"user":"U042","ts":"100.000"},{"user":"U999","ts":"200.000"}]}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "tok", baseURL: srv.URL}
	author, err := client.threadInitiator(t.Context(), "C1", "100.000")
	require.NoError(t, err)
	require.Equal(t, "U042", author)
	require.Contains(t, gotQuery, "channel=C1")
	require.Contains(t, gotQuery, "ts=100.000")
	require.Contains(t, gotQuery, "limit=50")
}

func TestThreadInitiator_SkipsBotPrefixToFirstHuman(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"bot_id":"B001","user":"UBOT","ts":"100.000"},{"user":"U042","ts":"200.000"}]}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "tok", baseURL: srv.URL}
	author, err := client.threadInitiator(t.Context(), "C1", "100.000")
	require.NoError(t, err)
	require.Equal(t, "U042", author, "the first human after a bot-authored root is the initiator")
}

func TestThreadInitiator_AllBotReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"bot_id":"B001","user":"UBOT","ts":"100.000"}]}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "tok", baseURL: srv.URL}
	author, err := client.threadInitiator(t.Context(), "C1", "100.000")
	require.NoError(t, err)
	require.Empty(t, author, "an all-bot thread prefix has no human initiator")
}

func TestThreadInitiator_EmptyThreadReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"messages":[]}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "tok", baseURL: srv.URL}
	author, err := client.threadInitiator(t.Context(), "C1", "100.000")
	require.NoError(t, err)
	require.Empty(t, author)
}
