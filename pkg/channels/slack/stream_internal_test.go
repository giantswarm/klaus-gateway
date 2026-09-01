package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
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
	require.Equal(t, int32(4), calls.Load(), "consecutive 429s keep pacing up to the attempt budget before failing")
}

// A burst that clears within the attempt budget succeeds: two consecutive
// 429s pace the call instead of killing it (the old behaviour failed on the
// second).
func TestSend_RecoversAfterConsecutiveRateLimits(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
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
	require.Equal(t, int32(3), calls.Load())
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

// One transient mid-stream Slack failure must not abort a healthy turn: the
// ticker retries the flush and the content is still delivered.
func TestRun_TransientFlushFailureDoesNotAbortTurn(t *testing.T) {
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
	ch := make(chan channels.OutboundDelta)
	done := make(chan error, 1)
	go func() { done <- w.run(t.Context(), ch) }()

	ch <- channels.OutboundDelta{Kind: channels.DeltaText, Content: "hello"}
	require.Eventually(t, func() bool { return updates.Load() >= 2 },
		10*time.Second, 20*time.Millisecond, "the failed flush must be retried on a later tick")
	close(ch)

	require.NoError(t, <-done, "a single flush failure must not fail the turn")
	require.Equal(t, "hello", lastText.Load())
}

// A persistent Slack failure still aborts the turn instead of holding the
// thread slot until the turn deadline.
func TestRun_PersistentFlushFailureAbortsTurn(t *testing.T) {
	var updates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		updates.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":false,"error":"fatal_error"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "1.1", "1.0", detailsOff, slog.Default())
	ch := make(chan channels.OutboundDelta)
	done := make(chan error, 1)
	go func() { done <- w.run(t.Context(), ch) }()

	ch <- channels.OutboundDelta{Kind: channels.DeltaText, Content: "hello"}
	require.ErrorContains(t, <-done, "fatal_error")
	require.GreaterOrEqual(t, updates.Load(), int32(maxFlushFailures))
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

// A multi-chunk reply whose head lands but whose tail fails leaves flushedLen at
// 0, yet the head already replaced the placeholder with agent text. wroteContent
// must report true so the failure note posts as a new message instead of
// overwriting the delivered head.
func TestFlush_PartialMultiChunkStillCountsAsContent(t *testing.T) {
	var updates, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "chat.update"): // head, in place
			updates.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.1"}`)
		case strings.HasSuffix(r.URL.Path, "chat.postMessage"): // tail overflow
			posts.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":false,"error":"fatal_error"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "1.1", "1.0", detailsOff, slog.Default())
	// One line over the block max hard-splits into a head + tail chunk.
	w.buf.WriteString(strings.Repeat("a", slackMarkdownBlockMax+500))

	require.Error(t, w.flush(t.Context()), "the tail post fails")
	require.Equal(t, int32(1), updates.Load(), "the head replaced the placeholder in place")
	require.Equal(t, int32(1), posts.Load(), "the tail was attempted")
	require.Equal(t, 0, w.flushedLen, "flushedLen stays 0 because not every chunk landed")
	require.True(t, w.wroteContent(), "the delivered head must count as written content")
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
// again on the same writer. run() defers drainThreadPosts, so draining must be
// idempotent and re-init the queue for the resumed segment; otherwise the
// second drain re-closes a closed channel and panics the whole process.
func TestDrainThreadPosts_IdempotentAcrossRunCycles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "1.1", "1.0", detailsOn, slog.Default())

	// First segment queued tool activity and drained (as run()'s defer does).
	w.enqueueThreadPost(t.Context(), threadPost{kind: postToolEntry, md: "tool one"})
	require.NotPanics(t, w.drainThreadPosts)

	// Resumed segment over the same writer: a fresh poster starts and drains
	// without re-closing the first segment's queue. Narration shares the queue and
	// carries its per-turn count across cycles.
	require.NotPanics(t, func() {
		w.enqueueThreadPost(t.Context(), threadPost{kind: postToolEntry, md: "tool two"})
		w.renderNarration(t.Context(), "and now the second step")
		w.drainThreadPosts()
	})
	require.Equal(t, 1, w.narrationsRendered)

	// Draining with nothing queued stays a no-op.
	require.NotPanics(t, w.drainThreadPosts)
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
	require.NoError(t, client.postApprovalPrompt(t.Context(), "C1", "T1", "task-1", "run <!channel> now?"))
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

func TestPostChoiceWidgetPrompt_EscapesQuestion(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	require.NoError(t, client.postChoiceWidgetPrompt(t.Context(), "C1", "T1", "task-1", "notify <!here>?", []string{"yes", "no"}, false))
	raw, _ := body.Load().(string)
	var payload struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	require.Equal(t, "notify &lt;!here&gt;?", payload.Text)
}

// choicePayload decodes a choice prompt far enough to inspect the widget
// element, its options, and the Submit button.
type choicePayload struct {
	Blocks []struct {
		Type     string                `json:"type"`
		BlockID  string                `json:"block_id"`
		Text     struct{ Text string } `json:"text"`
		Elements []struct {
			Type     string `json:"type"`
			ActionID string `json:"action_id"`
			Options  []struct {
				Value string                `json:"value"`
				Text  struct{ Text string } `json:"text"`
			} `json:"options"`
		} `json:"elements"`
		Accessory struct {
			Type     string `json:"type"`
			ActionID string `json:"action_id"`
		} `json:"accessory"`
	} `json:"blocks"`
}

func decodeChoicePayload(t *testing.T, raw string) choicePayload {
	t.Helper()
	var p choicePayload
	require.NoError(t, json.Unmarshal([]byte(raw), &p))
	return p
}

func TestPostChoiceWidgetPrompt_SingleUsesRadioMultiUsesCheckbox(t *testing.T) {
	for _, tc := range []struct {
		name     string
		multiple bool
		want     string
	}{
		{"single", false, bkRadioButtons},
		{"multi", true, bkCheckboxes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				body.Store(string(raw))
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
			}))
			defer srv.Close()

			client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
			choices := []string{"alpha", "beta", "gamma"}
			require.NoError(t, client.postChoiceWidgetPrompt(t.Context(), "C1", "T1", "task-1", "pick", choices, tc.multiple))

			p := decodeChoicePayload(t, body.Load().(string))
			var group, submit bool
			for _, b := range p.Blocks {
				if b.BlockID == hitlGroupBlock {
					require.Len(t, b.Elements, 1)
					el := b.Elements[0]
					require.Equal(t, tc.want, el.Type)
					require.Equal(t, hitlGroup, el.ActionID)
					require.Len(t, el.Options, len(choices))
					for i, opt := range el.Options {
						require.Equal(t, strconv.Itoa(i), opt.Value)
						require.LessOrEqual(t, len([]rune(opt.Text.Text)), choiceLabelWidgetMax)
					}
					group = true
				}
				for _, el := range b.Elements {
					if el.ActionID == hitlSubmit {
						submit = true
					}
				}
			}
			require.True(t, group, "radio/checkbox group block present")
			require.True(t, submit, "Submit button present")
		})
	}
}

// A multi-question form posts one widget block per question, block_id-tagged
// with the question index (radio for single-select, checkbox for multi), with
// option values as choice indices, plus a single Submit.
func TestPostChoiceFormPrompt_PerQuestionBlocks(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	questions := []channels.HitlQuestion{
		{Question: "Database?", Choices: []string{"PostgreSQL", "MySQL"}},
		{Question: "Features?", Multiple: true, Choices: []string{"Auth", "Logging", "Caching"}},
	}
	require.NoError(t, client.postChoiceFormPrompt(t.Context(), "C1", "T1", "task-1", questions))

	p := decodeChoicePayload(t, body.Load().(string))
	want := map[string]struct {
		element    string
		numOptions int
	}{
		hitlQGroupPrefix + "_0": {bkRadioButtons, 2},
		hitlQGroupPrefix + "_1": {bkCheckboxes, 3},
	}
	seen := map[string]bool{}
	var submit bool
	for _, b := range p.Blocks {
		if w, ok := want[b.BlockID]; ok {
			require.Len(t, b.Elements, 1)
			el := b.Elements[0]
			require.Equal(t, w.element, el.Type)
			require.Equal(t, hitlGroup, el.ActionID)
			require.Len(t, el.Options, w.numOptions)
			for i, opt := range el.Options {
				require.Equal(t, strconv.Itoa(i), opt.Value)
			}
			seen[b.BlockID] = true
		}
		for _, el := range b.Elements {
			if el.ActionID == hitlSubmit {
				submit = true
			}
		}
	}
	require.Len(t, seen, 2, "one widget block per question")
	require.True(t, submit, "single Submit button present")
}

func TestPostChoiceWidgetPrompt_TruncatesOversizedQuestion(t *testing.T) {
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
	require.NoError(t, client.postChoiceWidgetPrompt(t.Context(), "C1", "T1", "task-1", oversized, []string{"yes"}, false))

	raw, _ := body.Load().(string)
	var payload sectionPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	section := payload.Blocks[0].Text.Text
	require.LessOrEqual(t, len([]rune(section)), slackSectionTextMax)
	require.Contains(t, section, "…", "truncation marker expected")
}

// A choice longer than the widget option-text limit must render as a section
// per choice with the full label intact (no truncation), which is the whole
// point of the section fallback.
func TestPostChoiceSectionPrompt_DoesNotTruncateLongChoice(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	long := strings.Repeat("a", choiceLabelWidgetMax+50) // > 75 runes, < 3000
	require.NoError(t, client.postChoiceSectionPrompt(t.Context(), "C1", "T1", "task-1", "pick", []string{long, "short"}, false))

	p := decodeChoicePayload(t, body.Load().(string))
	var found bool
	for _, b := range p.Blocks {
		if b.Type == bkSection && b.Text.Text == long {
			require.Equal(t, bkButton, b.Accessory.Type)
			require.True(t, strings.HasPrefix(b.Accessory.ActionID, hitlChoice))
			found = true
		}
	}
	require.True(t, found, "long choice rendered untruncated in its own section")
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
	require.NoError(t, client.postApprovalPrompt(t.Context(), "C1", "T1", "task-1", oversized))

	raw, _ := body.Load().(string)
	var payload sectionPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	section := payload.Blocks[0].Text.Text
	require.LessOrEqual(t, len([]rune(section)), slackSectionTextMax)
	require.True(t, strings.HasSuffix(section, "…"), "truncation marker expected")
	require.True(t, utf8.ValidString(section))
}

func TestRespondURL_ErrorsOnNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := respondURL(t.Context(), srv.URL, "", "updated")
	require.Error(t, err)
	require.Contains(t, err.Error(), "http status 500")
}

func TestRespondURL_SucceedsOn2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, respondURL(t.Context(), srv.URL, "", "updated"))
}

// A replacement of a thread-scoped ephemeral must carry the source thread_ts,
// or Slack renders it at channel top level as well as in the thread.
func TestRespondURL_CarriesThreadTS(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		got = v
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	body := func() map[string]any { mu.Lock(); defer mu.Unlock(); return got }

	require.NoError(t, respondURL(t.Context(), srv.URL, "100.000", "✅ allowed"))
	require.Equal(t, "100.000", body()["thread_ts"])
	require.Equal(t, true, body()["replace_original"])
	require.Equal(t, "ephemeral", body()["response_type"])

	// Without a thread the field stays absent (a top-level ephemeral).
	require.NoError(t, respondURL(t.Context(), srv.URL, "", "✅ allowed"))
	_, hasThread := body()["thread_ts"]
	require.False(t, hasThread)
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

// postJSON applies the client's display identity to the request without
// mutating the caller's body map.
func TestPostJSON_IdentityDoesNotMutateCallerBody(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL, username: "SRE agent", iconURL: "https://example.test/icon.png"}
	requestBody := map[string]any{paramChannel: "C1", paramText: "hello"}
	_, err := client.postJSON(t.Context(), "chat.postMessage", requestBody)
	require.NoError(t, err)

	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(body.Load().(string)), &sent))
	require.Equal(t, "SRE agent", sent[paramUsername], "the request carries the identity")
	require.Equal(t, "https://example.test/icon.png", sent[paramIconURL])

	require.Equal(t, map[string]any{paramChannel: "C1", paramText: "hello"}, requestBody,
		"the caller's body map must stay unmodified")
}

func TestParseAuthChallenge(t *testing.T) {
	server, loginURL := parseAuthChallenge("Authentication Required\n\nServer: gazelle-mcp-pro\n\n" +
		"Please sign in:\n\nhttps://pro.example.com/authorize?state=abc\n\nThen retry.")
	require.Equal(t, "gazelle-mcp-pro", server)
	require.Equal(t, "https://pro.example.com/authorize?state=abc", loginURL)
}

func TestParseAuthChallenge_TrimsTrailingPunctuation(t *testing.T) {
	_, loginURL := parseAuthChallenge("Sign in here: (https://x.example/auth?s=1).")
	require.Equal(t, "https://x.example/auth?s=1", loginURL)
}

func TestParseAuthChallenge_NoURL(t *testing.T) {
	server, loginURL := parseAuthChallenge("Server: pro\nno link present")
	require.Equal(t, "pro", server)
	require.Empty(t, loginURL)
}

func TestParseAuthChallenge_NoServerLine(t *testing.T) {
	server, loginURL := parseAuthChallenge("please visit https://x.example/auth")
	require.Empty(t, server, "no Server: line yields empty; the caller falls back to the call arguments")
	require.Equal(t, "https://x.example/auth", loginURL)
}

func TestParseAuthChallenge_RejectsNonHTTPS(t *testing.T) {
	// An http:// link is rejected: the Connect button must not open a non-https
	// URL scraped from agent/tool-controlled text.
	_, loginURL := parseAuthChallenge("Server: pro\nSign in: http://pro.example.com/authorize")
	require.Empty(t, loginURL)

	// A URL matching the scheme regex but with no host is rejected.
	_, loginURL = parseAuthChallenge("Server: pro\nSign in: https://?state=abc")
	require.Empty(t, loginURL)

	// A malformed (bad percent-escape) https URL is rejected.
	_, loginURL = parseAuthChallenge("Server: pro\nSign in: https://%zz")
	require.Empty(t, loginURL)
}

func TestParseAuthChallengePayload_DirectOutput(t *testing.T) {
	server, loginURL := parseAuthChallengePayload(map[string]any{
		"output": "Server: pro\nhttps://x.example/auth",
	}, 0)
	require.Equal(t, "pro", server)
	require.Equal(t, "https://x.example/auth", loginURL)
}

func TestParseAuthChallengePayload_MCPContentList(t *testing.T) {
	server, loginURL := parseAuthChallengePayload(map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "Server: pro\nhttps://x.example/auth"}},
		"isError": false,
	}, 0)
	require.Equal(t, "pro", server)
	require.Equal(t, "https://x.example/auth", loginURL)
}

func TestParseAuthChallengePayload_NoURL(t *testing.T) {
	server, loginURL := parseAuthChallengePayload(map[string]any{
		"output": "Server 'pro' is already authenticated.",
	}, 0)
	require.Empty(t, server)
	require.Empty(t, loginURL)
}

func TestParseAuthChallenge_JSONEscapedAmpersand(t *testing.T) {
	// muster's challenge text embeds a JSON-encoded blob, so Go's HTML-safe
	// encoding turns each & into a literal escape sequence; the button must
	// still open the real URL.
	challenge := "Server: pro\nSign in: https://x.example/auth?a=1" + jsonEscapedAmp + "b=2"
	server, loginURL := parseAuthChallenge(challenge)
	require.Equal(t, "pro", server)
	require.Equal(t, "https://x.example/auth?a=1&b=2", loginURL)
}

func TestParseAuthChallenge_TerminatesAtJSONEscapedWhitespace(t *testing.T) {
	// muster's core_auth_login result reaches the gateway as an undecoded JSON
	// string, so the newline separating the URL from the trailing instructions
	// is the literal escape `\n`, which \S+ does not stop at. Without cutting
	// there, the parsed URL carries `\n\nAfter...`; decorating it with the
	// completion redirect then re-encodes the garbage into the state query and
	// muster rejects the mangled state as "session expired".
	challenge := `Authentication is required for gazelle-mcp-pro.\n\n` +
		`https://muster.gazelle.awsprod.gigantic.io/oauth/proxy/start?state=abc123\n\nAfter you've signed in, let me know.`
	_, loginURL := parseAuthChallenge(challenge)
	require.Equal(t, "https://muster.gazelle.awsprod.gigantic.io/oauth/proxy/start?state=abc123", loginURL)
}

func TestParseAuthChallenge_TerminatesBeforeJSONQuote(t *testing.T) {
	// A URL that ends at the closing quote of its JSON string value.
	_, loginURL := parseAuthChallenge(`{"login":"https://x.example/auth?s=1"}`)
	require.Equal(t, "https://x.example/auth?s=1", loginURL)
}

func TestParseAuthChallenge_JSONEscapedAmpersandBeforeTerminator(t *testing.T) {
	// The & inside the query survives while the trailing escaped newline is cut.
	challenge := `https://x.example/auth?a=1` + jsonEscapedAmp + `b=2\nthen retry`
	_, loginURL := parseAuthChallenge(challenge)
	require.Equal(t, "https://x.example/auth?a=1&b=2", loginURL)
}

func TestScrubLoginURLs_ReencodedVariants(t *testing.T) {
	// The agent re-encodes the recorded URL's query freely: JSON-escaped
	// ampersands, percent-encoded base64 padding. Prefix matching must catch
	// every spelling.
	const loginURL = "https://pro.example/authorize?a=1&state=abc="
	w := &batchedWriter{loginURLs: []string{loginURL}}
	escaped := strings.ReplaceAll(loginURL, "&", jsonEscapedAmp)
	percent := "https://pro.example/authorize?a=1&state=abc%3D"
	out := w.scrubLoginURLs("raw: " + escaped + "\npercent: <" + percent + "|sign in>\nlink: [sign in](" + loginURL + ")")
	require.NotContains(t, out, "pro.example")
	require.Empty(t, out, "every line carried the link, so nothing is left")
}

func TestNoteCallToolTarget_RecordsServerArgument(t *testing.T) {
	w := &batchedWriter{}
	w.noteCallToolTarget(&channels.ToolActivity{
		Kind:   channels.ToolCall,
		Name:   musterCallToolMetaTool,
		CallID: "c1",
		Args: map[string]any{
			"name":      musterAuthLoginTool,
			"arguments": map[string]any{"server": "gazelle-mcp-pro"},
		},
	})
	require.Equal(t, callToolTarget{name: musterAuthLoginTool, server: "gazelle-mcp-pro"}, w.callToolInner["c1"])
}

func TestScrubLoginURLs(t *testing.T) {
	const loginURL = "https://pro.example/authorize?state=abc&code_challenge=xyz"
	w := &batchedWriter{loginURLs: []string{loginURL}}

	// Markdown link, Slack mrkdwn link, and a bare occurrence are all removed with
	// their lines; unrelated links survive. Nothing is left in their place.
	in := "Please [sign in](" + loginURL + ") first.\n" +
		"Or <" + loginURL + "|click here>.\n" +
		"Raw: " + loginURL + "\n" +
		"Docs: https://example.com/docs"
	out := w.scrubLoginURLs(in)
	require.NotContains(t, out, loginURL)
	require.Equal(t, "Docs: https://example.com/docs", out)
	require.NotContains(t, out, "[sign in]", "no dangling markdown link label")

	// No recorded URLs: text passes through untouched.
	require.Equal(t, in, (&batchedWriter{}).scrubLoginURLs(in))
}

func TestScrubLoginURLs_DropsLinkLineAndLeadIn(t *testing.T) {
	const loginURL = "https://pro.example/authorize?state=abc"
	w := &batchedWriter{loginURLs: []string{loginURL}}

	// The link sits on its own line after a lead-in ending in ":". Both go; the
	// Connect button prompt is the only sign-in affordance the user needs.
	in := "To list your issues, I need to authenticate first. Please visit the following link to sign in:\n" +
		":point_right: " + loginURL
	require.Empty(t, w.scrubLoginURLs(in))
}

func TestScrubLoginURLs_KeepsSurroundingContent(t *testing.T) {
	const loginURL = "https://pro.example/authorize?state=abc"
	w := &batchedWriter{loginURLs: []string{loginURL}}

	// A real intro line that does not end in ":" survives; only the link line and
	// the "here is the link:" lead-in are removed. The actionable outro ("tell me
	// once you're signed in") is preserved for the no-auto-resume case.
	in := "Here is what I found.\n" +
		"Sign in here:\n" +
		loginURL + "\n" +
		"Once you've signed in, let me know."
	out := w.scrubLoginURLs(in)
	require.Equal(t, "Here is what I found.\nOnce you've signed in, let me know.", out)
	require.NotContains(t, out, "Sign in here:")
}

func TestConnectorReplyRetractable(t *testing.T) {
	// No connector prompt this turn: nothing to retract.
	require.False(t, (&batchedWriter{}).connectorReplyRetractable())

	// A prompt with a working auto-resume callback: the narration is redundant.
	require.True(t, (&batchedWriter{loginURLs: []string{"https://x/authorize"}}).connectorReplyRetractable())

	// A prompt without a callback (no-op button): keep the narration so the user
	// knows to sign in and say so.
	manual := &batchedWriter{loginURLs: []string{"https://x/authorize"}, connectorManualSignIn: true}
	require.False(t, manual.connectorReplyRetractable())
}

func TestRetractRendered_DeletesHeadAndTails(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chat.delete") {
			var body struct {
				TS string `json:"ts"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			deleted = append(deleted, body.TS)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "head-ts", "T1", detailsOff, nil)
	w.tailTS = []string{"tail-1", "tail-2"}
	w.wroteAny = true

	w.retractRendered(t.Context())

	require.ElementsMatch(t, []string{"head-ts", "tail-1", "tail-2"}, deleted)
	require.Empty(t, w.ts)
	require.Empty(t, w.tailTS)
	require.False(t, w.wroteContent())
}

func TestParseAuthChallengePayload_DepthBounded(t *testing.T) {
	nested := any("Server: pro\nhttps://x.example/auth")
	for range maxChallengePayloadDepth + 2 {
		nested = map[string]any{"inner": nested}
	}
	_, loginURL := parseAuthChallengePayload(nested, 0)
	require.Empty(t, loginURL)
}

// A transient Slack failure on the final flush must not abort the turn: a
// short reply that never hits a ticker flush has no later tick to re-send it,
// so the terminal flush retries in place.
func TestRun_TransientFinalFlushFailureDoesNotAbortTurn(t *testing.T) {
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
	ch := make(chan channels.OutboundDelta, 2)
	ch <- channels.OutboundDelta{Kind: channels.DeltaText, Content: "hello"}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch), "one transient final-flush failure must not fail the turn")
	require.Equal(t, int32(2), updates.Load(), "the failed final flush is retried in place")
	require.Equal(t, "hello", lastText.Load())
}

func TestUnwrapCallTool(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		tool     *channels.ToolActivity
		wantName string
		wantOK   bool
	}{
		{
			desc: "call_tool with inner name and arguments",
			tool: &channels.ToolActivity{Name: musterCallToolMetaTool, Args: map[string]any{
				"name":      "x_kubernetes_get",
				"arguments": map[string]any{"namespace": "flux-giantswarm"},
			}},
			wantName: "x_kubernetes_get",
			wantOK:   true,
		},
		{
			desc: "direct tool is not unwrapped",
			tool: &channels.ToolActivity{Name: "x_kubernetes_get", Args: map[string]any{"namespace": "flux"}},
		},
		{
			desc: "call_tool missing arguments falls back",
			tool: &channels.ToolActivity{Name: musterCallToolMetaTool, Args: map[string]any{"name": "x_kubernetes_get"}},
		},
		{
			desc: "call_tool with empty inner name falls back",
			tool: &channels.ToolActivity{Name: musterCallToolMetaTool, Args: map[string]any{
				"name":      "",
				"arguments": map[string]any{"namespace": "flux"},
			}},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			name, args, ok := unwrapCallTool(tc.tool)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.Equal(t, tc.wantName, name)
				require.NotNil(t, args)
			}
		})
	}
}

func TestSpaceStructuralJSON(t *testing.T) {
	require.Equal(t, `{"a": "b", "c": "d"}`, spaceStructuralJSON([]byte(`{"a":"b","c":"d"}`)))
	// ':' and ',' inside a string value stay untouched.
	require.Equal(t, `{"k": "a,b:c"}`, spaceStructuralJSON([]byte(`{"k":"a,b:c"}`)))
	// An escaped quote does not end the string, so its trailing ',' is left alone.
	require.Equal(t, `{"k": "a\"b,c"}`, spaceStructuralJSON([]byte(`{"k":"a\"b,c"}`)))
}

// capturedMessage is one thread message's final content: the text of each of
// its blocks (context elements flattened), after all in-place updates.
type capturedMessage []string

// statusCall is one assistant.threads.setStatus invocation the fake thread
// received.
type statusCall struct {
	channelID string
	threadTS  string
	status    string
}

// fakeThread models a Slack thread: chat.postMessage appends a message with a
// fresh ts, chat.update replaces the content at its ts (upserting an unknown
// ts, e.g. the text-mode placeholder posted before the writer existed), so
// assertions run against the thread a user would actually see.
// assistant.threads.setStatus calls are recorded separately (failStatus makes
// them fail), and history keeps every message revision so tests can assert
// content that never survives to the final state, like the live ticker line.
type fakeThread struct {
	mu       sync.Mutex
	order    []string
	messages map[string]capturedMessage
	nextTS   int
	posts    int
	history  []capturedMessage
	// statusCalls records assistant.threads.setStatus invocations in order.
	statusCalls []statusCall
	// failStatus, when set, makes assistant.threads.setStatus respond with
	// this Slack error instead of ok.
	failStatus string
}

func (f *fakeThread) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TS        string            `json:"ts"`
			Blocks    []json.RawMessage `json:"blocks"`
			ChannelID string            `json:"channel_id"`
			ThreadTS  string            `json:"thread_ts"`
			Status    string            `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		texts := blockTexts(body.Blocks)
		ts := body.TS
		statusErr := ""
		f.mu.Lock()
		if f.messages == nil {
			f.messages = map[string]capturedMessage{}
		}
		switch path.Base(r.URL.Path) {
		case "chat.postMessage":
			f.posts++
			f.nextTS++
			ts = "msg-" + strconv.Itoa(f.nextTS)
			f.order = append(f.order, ts)
			f.messages[ts] = texts
			f.history = append(f.history, texts)
		case "chat.update":
			if _, ok := f.messages[ts]; !ok {
				f.order = append(f.order, ts)
			}
			f.messages[ts] = texts
			f.history = append(f.history, texts)
		case "assistant.threads.setStatus":
			f.statusCalls = append(f.statusCalls, statusCall{channelID: body.ChannelID, threadTS: body.ThreadTS, status: body.Status})
			statusErr = f.failStatus
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if statusErr != "" {
			_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, statusErr)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":%q}`, ts)
	}
}

// statuses returns the status texts of every recorded setStatus call, in order.
func (f *fakeThread) statuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.statusCalls))
	for _, c := range f.statusCalls {
		out = append(out, c.status)
	}
	return out
}

// sawText reports whether any message revision (post or update) ever carried a
// block whose text contains sub.
func (f *fakeThread) sawText(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, revision := range f.history {
		for _, text := range revision {
			if strings.Contains(text, sub) {
				return true
			}
		}
	}
	return false
}

// finalMessages returns each thread message's final content, in post order.
func (f *fakeThread) finalMessages() []capturedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedMessage, 0, len(f.order))
	for _, ts := range f.order {
		out = append(out, f.messages[ts])
	}
	return out
}

// postCount returns how many chat.postMessage calls the thread received.
func (f *fakeThread) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts
}

// blockTexts flattens a message's blocks into their text strings: markdown
// blocks carry the text directly, context blocks carry mrkdwn elements, and
// section blocks a text object.
func blockTexts(blocks []json.RawMessage) capturedMessage {
	var out capturedMessage
	for _, raw := range blocks {
		var b struct {
			Type     string          `json:"type"`
			Text     json.RawMessage `json:"text"`
			Elements []struct {
				Text string `json:"text"`
			} `json:"elements"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			continue
		}
		switch b.Type {
		case bkMarkdown:
			var s string
			_ = json.Unmarshal(b.Text, &s)
			out = append(out, s)
		case bkContext:
			for _, e := range b.Elements {
				out = append(out, e.Text)
			}
		case bkSection:
			var obj struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b.Text, &obj)
			out = append(out, obj.Text)
		}
	}
	return out
}

// capturePosts drives run() over deltas (plus a terminal Done) against a fake
// Slack thread and returns its final messages in order, together with the
// writer. headTS empty is reactions mode, where the main reply is posted lazily
// on the first flush; a non-empty headTS is text mode, where it is updated in
// place.
func capturePosts(t *testing.T, details detailsLevel, headTS string, deltas ...channels.OutboundDelta) ([]capturedMessage, *batchedWriter) {
	t.Helper()
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", headTS, "1.0", details, slog.Default())
	ch := make(chan channels.OutboundDelta, len(deltas)+1)
	for _, d := range deltas {
		ch <- d
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)
	require.NoError(t, w.run(t.Context(), ch))

	return ft.finalMessages(), w
}

// captureToolPostBlocks returns every rendered tool entry across the thread's
// activity messages, in stream order, in text mode.
func captureToolPostBlocks(t *testing.T, details detailsLevel, deltas ...channels.OutboundDelta) []string {
	t.Helper()
	msgs, _ := capturePosts(t, details, "1.1", deltas...)
	var entries []string
	for _, m := range msgs {
		entries = append(entries, m...)
	}
	return entries
}

// runSurfaceWriter drives run() over deltas against ft on the given channel
// (a "D…" channel is the assistant-pane surface, "C…" a channel), with an
// adapter attached so the assistant-status downgrade latch has a home. The
// deltas are passed verbatim — append a Done (or Err/Prompt) delta yourself —
// and run()'s error is returned for the failure-path tests.
func runSurfaceWriter(t *testing.T, ft *fakeThread, channel string, deltas ...channels.OutboundDelta) ([]capturedMessage, *batchedWriter, error) {
	t.Helper()
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, channel, "", "1.0", detailsOn, slog.Default())
	w.adapter = &Adapter{}
	ch := make(chan channels.OutboundDelta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	err := w.run(t.Context(), ch)
	return ft.finalMessages(), w, err
}

func doneDelta() channels.OutboundDelta { return channels.OutboundDelta{Done: true} }

func narrationDelta(text string) channels.OutboundDelta {
	return channels.OutboundDelta{Kind: channels.DeltaNarration, Content: text}
}

func toolCallDelta(name string) channels.OutboundDelta {
	return channels.OutboundDelta{
		Kind: channels.DeltaToolActivity,
		Tool: &channels.ToolActivity{Kind: channels.ToolCall, Name: name},
	}
}

// The agent's narration reads in order with the status ticker it introduces,
// and the answer still lands last (klaus-gateway#197). The ticker is
// per-segment: narration closes the live ticker into its receipt at its
// position — counting only that segment's steps — and a short narration folds
// into the NEXT segment's status message, so each narrate-then-call group is
// one compact message and the thread reads in stream order.
func TestRenderNarration_PerSegmentReceiptsInStreamOrder(t *testing.T) {
	msgs, w := capturePosts(t, detailsOn, "",
		narrationDelta("Let me pull the HelmRelease from both clusters."),
		toolCallDelta("x_kubernetes_get"),
		toolCallDelta("x_kubernetes_get"),
		narrationDelta("Both share the same chart version."),
		toolCallDelta("x_kubernetes_get"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "here is the diff"},
	)

	require.Len(t, msgs, 3)
	require.Equal(t, capturedMessage{
		"Let me pull the HelmRelease from both clusters.",
		"🛠️ 2 steps · x_kubernetes_get ×2",
	}, msgs[0], "the first segment's receipt counts only the steps before the next narration")
	require.Equal(t, capturedMessage{
		"Both share the same chart version.",
		"🛠️ 1 step · x_kubernetes_get",
	}, msgs[1], "mid-turn narration closes the segment and folds into the next one")
	require.Equal(t, capturedMessage{"here is the diff"}, msgs[2], "the answer is the turn's last message")
	require.True(t, w.wroteContent())
}

// A full-weight (non-foldable) narration between tool batches collapses the
// live ticker into its receipt AT ITS POSITION, and the next tool call opens a
// fresh ticker message below the narration — the thread reads like the stream:
// receipt → narration → receipt → answer.
func TestToolStatus_OwnMessageNarrationSplitsSegments(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		toolCallDelta("x_kubernetes_get"),
		toolCallDelta("x_kubernetes_list"),
		narrationDelta("one\ntwo\nthree"), // multi-line keeps its own message
		toolCallDelta("skills"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "the answer"},
	)

	require.Len(t, msgs, 4)
	require.Equal(t, capturedMessage{"🛠️ 2 steps · x_kubernetes_get · x_kubernetes_list"}, msgs[0],
		"the first segment's receipt stays above the narration that closed it")
	require.Equal(t, capturedMessage{"one\ntwo\nthree"}, msgs[1])
	require.Equal(t, capturedMessage{"🛠️ 1 step · skills"}, msgs[2],
		"the next segment opens a fresh status message below the narration")
	require.Equal(t, capturedMessage{"the answer"}, msgs[3])
}

// Each segment's receipt lists tools in ITS OWN first-use order and counts,
// independent of earlier segments.
func TestToolStatus_SegmentReceiptsUseSegmentLocalOrderAndCounts(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		toolCallDelta("alpha"),
		toolCallDelta("beta"),
		narrationDelta("one\ntwo\nthree"),
		toolCallDelta("beta"),
		toolCallDelta("beta"),
		toolCallDelta("alpha"),
	)

	require.Len(t, msgs, 3)
	require.Equal(t, capturedMessage{"🛠️ 2 steps · alpha · beta"}, msgs[0])
	require.Equal(t, capturedMessage{"one\ntwo\nthree"}, msgs[1])
	require.Equal(t, capturedMessage{"🛠️ 3 steps · beta ×2 · alpha"}, msgs[2],
		"the second segment counts and orders its own calls only")
}

// A turn ending mid-segment (tool calls after the last narration, then done)
// still collapses the open segment into its receipt before the answer.
func TestToolStatus_TurnEndingMidSegmentCollapsesLastSegment(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		toolCallDelta("alpha"),
		narrationDelta("one\ntwo\nthree"),
		toolCallDelta("beta"),
	)

	require.Len(t, msgs, 3)
	require.Equal(t, capturedMessage{"🛠️ 1 step · alpha"}, msgs[0])
	require.Equal(t, capturedMessage{"one\ntwo\nthree"}, msgs[1])
	require.Equal(t, capturedMessage{"🛠️ 1 step · beta"}, msgs[2],
		"the open segment collapses at turn end")
}

// Consecutive foldable narrations with no tool calls between them share one
// status message instead of each opening a segment: an empty segment has no
// receipt to collapse.
func TestToolStatus_ConsecutiveFoldedNarrationsShareOneMessage(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		toolCallDelta("alpha"),
		narrationDelta("First thought."),
		narrationDelta("Second thought."),
		toolCallDelta("beta"),
	)

	require.Len(t, msgs, 2)
	require.Equal(t, capturedMessage{"🛠️ 1 step · alpha"}, msgs[0])
	require.Equal(t, capturedMessage{
		"First thought.",
		"Second thought.",
		"🛠️ 1 step · beta",
	}, msgs[1], "a stepless narration does not close the fresh segment")
}

// The typical turn — one short narration, a few tool calls, the answer —
// renders as exactly one status message plus the answer: the narration block
// kept above the collapsed receipt, with no full-weight narration message.
func TestNarrationFold_SingleStatusMessagePlusAnswer(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		narrationDelta("Sure! I'll call the skills tool right away."),
		toolCallDelta("skills"),
		toolCallDelta("skills"),
		toolCallDelta("skills"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "the answer"},
	)

	require.Len(t, msgs, 2, "one status message plus the answer")
	require.Equal(t, capturedMessage{
		"Sure! I'll call the skills tool right away.",
		"🛠️ 3 steps · skills ×3",
	}, msgs[0])
	require.Equal(t, capturedMessage{"the answer"}, msgs[1])
}

// Folding is only for narration that is safely short: long prose keeps the
// own-message rendering (a markdown block, not a muted context block).
func TestNarrationFold_LongNarrationKeepsOwnMessage(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("I will inspect the cluster state next. ", 10)) // > foldedNarrationMaxChars
	msgs, _ := capturePosts(t, detailsOn, "",
		narrationDelta(long),
		toolCallDelta("skills"),
	)

	require.Len(t, msgs, 2)
	require.Equal(t, capturedMessage{long}, msgs[0])
	require.Equal(t, capturedMessage{"🛠️ 1 step · skills"}, msgs[1])
}

// A narration over the fold's line budget keeps its own message even when it
// is short by character count.
func TestNarrationFold_MultilineNarrationKeepsOwnMessage(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		narrationDelta("one\ntwo\nthree"),
		toolCallDelta("skills"),
	)

	require.Len(t, msgs, 2)
	require.Equal(t, capturedMessage{"one\ntwo\nthree"}, msgs[0])
	require.Equal(t, capturedMessage{"🛠️ 1 step · skills"}, msgs[1])
}

// A fallback (own-message) narration posted before any tool call closes the
// ticker-less status message, so the ticker opens a fresh one below it and the
// thread keeps reading in stream order.
func TestNarrationFold_FallbackBeforeTickerKeepsStreamOrder(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		narrationDelta("Short intro."),
		narrationDelta("one\ntwo\nthree"),
		toolCallDelta("skills"),
	)

	require.Len(t, msgs, 3)
	require.Equal(t, capturedMessage{"Short intro."}, msgs[0], "the folded narration stays in the first status message")
	require.Equal(t, capturedMessage{"one\ntwo\nthree"}, msgs[1])
	require.Equal(t, capturedMessage{"🛠️ 1 step · skills"}, msgs[2],
		"the ticker opens below the fallback narration, in stream order")
}

// Folded narration enters an mrkdwn context block, so mrkdwn control sequences
// must arrive escaped: a quoted <!channel> cannot notify from the status
// message any more than from a tool name. (Own-message narration renders as a
// Block Kit markdown block, which does not parse these sequences.)
func TestNarrationFold_EscapesMrkdwn(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		narrationDelta("Pinging <!channel> & <@U1>"),
		toolCallDelta("skills"),
	)

	require.Len(t, msgs, 1)
	require.Equal(t, capturedMessage{
		"Pinging &lt;!channel&gt; &amp; &lt;@U1&gt;",
		"🛠️ 1 step · skills",
	}, msgs[0])
}

// Post-login-challenge narration is retractable, and retraction deletes whole
// messages by ts — it cannot delete one block inside the shared status message
// — so it must keep the own-message rendering however short it is.
func TestNarrationFold_RetractableKeepsOwnMessageAndRetracts(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TS string `json:"ts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if path.Base(r.URL.Path) == "chat.delete" {
			mu.Lock()
			deleted = append(deleted, body.TS)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"narr-1"}`)
	}))
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{botToken: "t", baseURL: srv.URL}, "C1", "", "1.0", detailsOn, slog.Default())
	w.loginURLs = []string{"https://auth.example/authorize?x=1"} // challenge seen
	w.renderNarration(t.Context(), "Sign in, then tell me once you are done.")
	w.drainThreadPosts()

	require.Equal(t, []string{"narr-1"}, w.narrationTS,
		"short post-challenge narration keeps its own message so it stays retractable")
	w.retractRendered(t.Context())
	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, deleted, "narr-1")
}

// Narration never folds at detailsFull: there is no status ticker there, and
// narration separates the aggregated tool-activity segments instead.
func TestNarrationFold_DetailsFullKeepsOwnMessage(t *testing.T) {
	msgs, _ := capturePosts(t, detailsFull, "",
		narrationDelta("Short intro."),
		toolCallDelta("skills"),
	)

	require.Len(t, msgs, 2)
	require.Equal(t, capturedMessage{"Short intro."}, msgs[0])
	require.Contains(t, msgs[1][0], "skills")
}

func TestFoldableNarration(t *testing.T) {
	require.True(t, foldableNarration("one line"))
	require.True(t, foldableNarration("one\ntwo"))
	require.False(t, foldableNarration("one\ntwo\nthree"), "over the line budget")
	require.False(t, foldableNarration(strings.Repeat("x", foldedNarrationMaxChars+1)), "over the char budget")
	require.True(t, foldableNarration(strings.Repeat("ä", foldedNarrationMaxChars)), "the budget counts runes, not bytes")
}

// Narration is the agent talking, not tool transparency, so /details off mutes
// the tool post and keeps the prose.
func TestRenderNarration_ShownWithDetailsOff(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOff, "",
		narrationDelta("Let me look that up."),
		toolCallDelta("x_kubernetes_get"),
	)

	require.Len(t, msgs, 1)
	require.Equal(t, capturedMessage{"Let me look that up."}, msgs[0])
}

// Narration must stay out of the main reply buffer: the reply is posted lazily on
// the first flush, and seeding it early would put the answer above the narration
// and tool posts it followed. It also must not count as delivered content, or a
// turn that only narrated would leave its "thinking" placeholder in place.
func TestRenderNarration_DoesNotTouchMainReply(t *testing.T) {
	msgs, w := capturePosts(t, detailsOn, "", narrationDelta("Let me look that up."))

	require.Len(t, msgs, 1)
	require.Equal(t, capturedMessage{"Let me look that up."}, msgs[0],
		"a folded narration still renders when no tool call ever follows")
	require.Empty(t, w.ts, "no main reply was posted")
	require.False(t, w.wroteContent())
}

// Dropping the agent's prose without saying so is the bug this rendering fixes,
// so the per-turn cap ends in a visible note.
func TestRenderNarration_CapsWithOneNote(t *testing.T) {
	deltas := make([]channels.OutboundDelta, 0, maxNarrationMessages+5)
	for i := range maxNarrationMessages + 5 {
		deltas = append(deltas, narrationDelta(fmt.Sprintf("step %d", i)))
	}
	deltas = append(deltas, toolCallDelta("x_kubernetes_get"))
	msgs, _ := capturePosts(t, detailsOn, "", deltas...)

	require.Len(t, msgs, 3, "the folded narration, its note, and the tool receipt")
	require.Len(t, msgs[0], maxNarrationMessages, "short narration folds and shares the per-turn budget")
	require.Equal(t, capturedMessage{narrationLimitNote}, msgs[1], "the note stays a visible message of its own")
	require.Contains(t, msgs[2][0], "x_kubernetes_get", "tool activity keeps its own budget")
}

// Slack rejects a markdown block over slackMarkdownBlockMax outright, so an
// outsized narration must be split rather than dropped.
func TestRenderNarration_SplitsOversizedNarration(t *testing.T) {
	long := strings.Repeat("plan step. ", slackMarkdownBlockMax/5) // ~2.4x the block cap
	msgs, _ := capturePosts(t, detailsOn, "", narrationDelta(long))

	require.Len(t, msgs, 3)
	var joined strings.Builder
	for _, m := range msgs {
		require.Len(t, m, 1)
		require.LessOrEqual(t, len(m[0]), slackMarkdownBlockMax)
		joined.WriteString(m[0])
	}
	require.Equal(t, len(strings.TrimSpace(long)), len(strings.TrimSpace(joined.String())), "no prose is dropped")
}

// Chunks share the per-turn budget, so one enormous narration cannot flood the
// thread and still ends in the visible note.
func TestRenderNarration_SplitChunksShareTheBudget(t *testing.T) {
	long := strings.Repeat("plan step. ", slackMarkdownBlockMax) // far past the cap
	msgs, _ := capturePosts(t, detailsOn, "", narrationDelta(long))

	require.Len(t, msgs, maxNarrationMessages+1)
	require.Equal(t, capturedMessage{narrationLimitNote}, msgs[maxNarrationMessages])
}

// A single-use login link must not be duplicated next to the Connect button, in
// narration any more than in the main reply. Narration that is nothing but the
// link posts nothing and keeps its budget.
func TestRenderNarration_ScrubsLoginURL(t *testing.T) {
	const loginURL = "https://auth.example/authorize?client_id=x"
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Blocks []struct {
				Text string `json:"text"`
			} `json:"blocks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Blocks) > 0 {
			posted = append(posted, body.Blocks[0].Text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1"}`)
	}))
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{botToken: "t", baseURL: srv.URL}, "C1", "", "1.0", detailsOn, slog.Default())
	w.loginURLs = []string{loginURL}
	w.renderNarration(t.Context(), loginURL)
	w.renderNarration(t.Context(), "Sign in here:\n"+loginURL+"\nThen tell me once you are done.")
	w.drainThreadPosts()

	require.Equal(t, 1, w.narrationsRendered, "a link-only narration keeps its budget")
	require.Len(t, posted, 1)
	require.NotContains(t, posted[0], "auth.example")
	require.NotContains(t, posted[0], "Sign in here:")
	require.Contains(t, posted[0], "Then tell me once you are done.")
}

// The retract exists for sign-in prose the Connect button contradicts. Narration
// from before the challenge explains tool posts that stay, so only what follows
// the challenge is marked for retraction.
func TestRenderNarration_OnlyPostChallengeNarrationIsRetractable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1"}`)
	}))
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{botToken: "t", baseURL: srv.URL}, "C1", "", "1.0", detailsOn, slog.Default())
	w.renderNarration(t.Context(), "Let me pull the HelmRelease from both clusters.")
	w.loginURLs = append(w.loginURLs, "https://auth.example/authorize?x=1") // challenge seen
	w.renderNarration(t.Context(), "You need to sign in before I can continue.")
	w.drainThreadPosts()

	require.Len(t, w.narrationTS, 1, "only the sign-in narration is retractable")
}

// Narration that followed the challenge is the same sign-in prose the retract
// exists for, so it goes with the reply when the button takes the turn over.
func TestRetractRendered_DeletesNarrationPosts(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chat.delete") {
			var body struct {
				TS string `json:"ts"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			deleted = append(deleted, body.TS)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "head-ts", "T1", detailsOff, nil)
	w.tailTS = []string{"tail-1"}
	w.narrationTS = []string{"narr-1", "narr-2"}

	w.retractRendered(t.Context())

	require.ElementsMatch(t, []string{"head-ts", "tail-1", "narr-1", "narr-2"}, deleted)
	require.Empty(t, w.narrationTS)
}

// The queued in-thread posts all precede the reply, so the terminal flush drains
// the poster before posting it — otherwise a slow Slack API leaves the answer
// above the narration that led to it.
func TestFinalFlush_DrainsThreadPostsBeforeReply(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Blocks []struct {
				Text string `json:"text"`
			} `json:"blocks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Blocks) > 0 && strings.HasPrefix(body.Blocks[0].Text, "slow") {
			time.Sleep(100 * time.Millisecond)
		}
		mu.Lock()
		if len(body.Blocks) > 0 {
			posted = append(posted, body.Blocks[0].Text)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1"}`)
	}))
	t.Cleanup(srv.Close)

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "", "1.0", detailsOn, slog.Default())
	ch := make(chan channels.OutboundDelta, 3)
	// Three lines keep the narration out of the fold, so it posts as its own
	// (slow) message ahead of the answer.
	ch <- narrationDelta("slow narration\nsecond line\nthird line")
	ch <- channels.OutboundDelta{Kind: channels.DeltaText, Content: "the answer"}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)
	require.NoError(t, w.run(t.Context(), ch))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"slow narration\nsecond line\nthird line", "the answer"}, posted)
}

func TestRenderToolActivity_UnwrapsCallTool(t *testing.T) {
	posts := captureToolPostBlocks(t, detailsFull, channels.OutboundDelta{
		Kind: channels.DeltaToolActivity,
		Tool: &channels.ToolActivity{
			Kind:   channels.ToolCall,
			Name:   musterCallToolMetaTool,
			CallID: "c1",
			Args: map[string]any{
				"name":      "x_kubernetes_get",
				"arguments": map[string]any{"namespace": "flux-giantswarm", "resourceType": "helmreleases"},
			},
		},
	})
	require.Len(t, posts, 1)
	require.Contains(t, posts[0], "🔧 *`x_kubernetes_get`* (via muster)")
	require.Contains(t, posts[0], `{"namespace": "flux-giantswarm", "resourceType": "helmreleases"}`)
	require.NotContains(t, posts[0], "call_tool")
}

func TestRenderToolActivity_DirectToolUnchanged(t *testing.T) {
	posts := captureToolPostBlocks(t, detailsFull, channels.OutboundDelta{
		Kind: channels.DeltaToolActivity,
		Tool: &channels.ToolActivity{Kind: channels.ToolCall, Name: "list_pods"},
	})
	require.Len(t, posts, 1)
	require.Contains(t, posts[0], "🔧 *`list_pods`*")
	require.NotContains(t, posts[0], "via muster")
}

// The default level is a status experience, not an audit log: the whole turn
// collapses into one receipt line with no payloads, and the call_tool wrapper
// is unwrapped so the receipt names the real tools.
func TestToolStatus_CollapsesToReceiptWithoutPayloads(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolCall, Name: musterCallToolMetaTool, CallID: "c1",
			Args: map[string]any{"name": "x_kubernetes_get", "arguments": map[string]any{"namespace": "flux"}},
		}},
		toolCallDelta("x_kubernetes_get"),
		toolCallDelta("ask_user"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "done"},
	)

	require.Len(t, msgs, 2, "one receipt line plus the answer")
	require.Equal(t, capturedMessage{"🛠️ 3 steps · x_kubernetes_get ×2 · ask_user"}, msgs[0])
	require.Equal(t, capturedMessage{"done"}, msgs[1])
}

// Tool results carry no extra signal at the default level; only calls count as
// steps, so a call+result pair is one step, not two.
func TestToolStatus_ResultsDoNotCountAsSteps(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		toolCallDelta("list_pods"),
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolResult, Name: "list_pods", Response: map[string]any{"output": "ok"},
		}},
	)

	require.Len(t, msgs, 1)
	require.Equal(t, capturedMessage{"🛠️ 1 step · list_pods"}, msgs[0])
}

// Receipt and ticker text is agent-controlled: mrkdwn control sequences must
// arrive escaped so a quoted <!channel> in a tool name cannot notify.
func TestToolStatus_EscapesMrkdwn(t *testing.T) {
	msgs, _ := capturePosts(t, detailsOn, "",
		toolCallDelta("notify <!channel>"),
	)

	require.Len(t, msgs, 1)
	require.NotContains(t, msgs[0][0], "<!channel>")
	require.Contains(t, msgs[0][0], "&lt;!channel&gt;")
}

func TestRenderToolTickerAndReceipt(t *testing.T) {
	require.Equal(t, "⏳ skills…", renderToolTicker(1, "skills"))
	require.Equal(t, "⏳ ask_user… · step 4", renderToolTicker(4, "ask_user"))

	require.Equal(t, "🛠️ 1 step · skills", renderToolReceipt(1, []string{"skills"}, map[string]int{"skills": 1}))
	require.Equal(t, "🛠️ 3 steps · skills ×2 · ask_user",
		renderToolReceipt(3, []string{"skills", "ask_user"}, map[string]int{"skills": 2, "ask_user": 1}))

	// Past the name cap the receipt summarises instead of growing unbounded.
	order := make([]string, receiptNameMax+3)
	counts := map[string]int{}
	for i := range order {
		order[i] = fmt.Sprintf("tool_%d", i)
		counts[order[i]] = 1
	}
	got := renderToolReceipt(len(order), order, counts)
	require.Contains(t, got, "+3 more")
	require.NotContains(t, got, order[receiptNameMax])
}

func TestRenderToolActivity_UnwrapsCallToolResult(t *testing.T) {
	posts := captureToolPostBlocks(t, detailsFull,
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolCall, Name: musterCallToolMetaTool, CallID: "c1",
			Args: map[string]any{"name": "x_kubernetes_get", "arguments": map[string]any{"namespace": "flux"}},
		}},
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolResult, Name: musterCallToolMetaTool, CallID: "c1",
			Response: map[string]any{"output": "ok"},
		}},
	)
	require.Len(t, posts, 2)
	require.Contains(t, posts[0], "🔧 *`x_kubernetes_get`* (via muster)")
	require.Contains(t, posts[1], "↳ *`x_kubernetes_get`* result (via muster)")
	require.Contains(t, posts[1], "`ok`", "the output wrap is unwrapped to the bare payload")
	require.NotContains(t, posts[1], `{"output"`)
}

// mcpEnvelope builds an MCP tool-result envelope carrying one text item, the
// shape MCP servers (and muster's call_tool) return results in.
func mcpEnvelope(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// serialize marshals an envelope the way muster's call_tool re-wraps the inner
// tool's result into the outer envelope's text.
func serialize(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestToolResultPreview(t *testing.T) {
	clusters := `{"clusters":["alpha","beta"]}`

	t.Run("non-envelope payloads keep the raw JSON rendering", func(t *testing.T) {
		preview, isErr := toolResultPreview(map[string]any{"items": "3 pods"}, 100)
		require.Equal(t, `{"items": "3 pods"}`, preview)
		require.False(t, isErr)
	})

	t.Run("output wrap with extra keys is not a text carrier", func(t *testing.T) {
		preview, _ := toolResultPreview(map[string]any{"output": "x", "status": "ok"}, 100)
		require.Equal(t, `{"output": "x", "status": "ok"}`, preview)
	})

	t.Run("output wrap unwraps to the bare text", func(t *testing.T) {
		preview, isErr := toolResultPreview(map[string]any{"output": "all fine"}, 100)
		require.Equal(t, "all fine", preview)
		require.False(t, isErr)
	})

	t.Run("envelope text renders without the content boilerplate", func(t *testing.T) {
		preview, isErr := toolResultPreview(mcpEnvelope("Server: pro\nStatus: ok", false), 100)
		require.Equal(t, "Server: pro Status: ok", preview, "newlines collapse deliberately")
		require.False(t, isErr)
	})

	t.Run("JSON text decodes to compact JSON with real quotes", func(t *testing.T) {
		preview, _ := toolResultPreview(mcpEnvelope("{\n  \"filters\": {\n    \"query\": \"list clusters\"\n  }\n}", false), 100)
		require.Equal(t, `{"filters": {"query": "list clusters"}}`, preview)
	})

	t.Run("muster double wrap unwraps to the innermost payload", func(t *testing.T) {
		resp := mcpEnvelope(serialize(t, mcpEnvelope(clusters, false)), false)
		preview, isErr := toolResultPreview(resp, 100)
		require.Equal(t, `{"clusters": ["alpha", "beta"]}`, preview)
		require.False(t, isErr)
	})

	t.Run("triple wrap unwraps too", func(t *testing.T) {
		resp := mcpEnvelope(serialize(t, mcpEnvelope(serialize(t, mcpEnvelope(clusters, false)), false)), false)
		preview, _ := toolResultPreview(resp, 100)
		require.Equal(t, `{"clusters": ["alpha", "beta"]}`, preview)
	})

	t.Run("inner isError surfaces through the wrap", func(t *testing.T) {
		resp := mcpEnvelope(serialize(t, mcpEnvelope("tool exploded", true)), false)
		preview, isErr := toolResultPreview(resp, 100)
		require.Equal(t, "tool exploded", preview)
		require.True(t, isErr)
	})

	t.Run("outer isError surfaces on a plain envelope", func(t *testing.T) {
		_, isErr := toolResultPreview(mcpEnvelope("denied", true), 100)
		require.True(t, isErr)
	})

	t.Run("unwrapping stops at the depth cap", func(t *testing.T) {
		text := clusters
		for i := 0; i < maxMCPResultUnwrapDepth+2; i++ {
			text = serialize(t, mcpEnvelope(text, false))
		}
		var resp map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &resp))
		preview, _ := toolResultPreview(resp, 2000)
		require.Contains(t, preview, "content", "past the cap the remaining wrap renders as-is")
	})

	t.Run("truncation spends the budget on the unwrapped payload", func(t *testing.T) {
		long := strings.Repeat("cluster alpha ", 50)
		preview, _ := toolResultPreview(mcpEnvelope(long, false), 20)
		require.LessOrEqual(t, len([]rune(preview)), 20)
		require.True(t, strings.HasPrefix(preview, "cluster alpha"), "budget goes to payload, not envelope: %q", preview)
		require.Contains(t, preview, "…")
	})

	t.Run("non-text content items render as type placeholders", func(t *testing.T) {
		resp := map[string]any{"content": []any{
			map[string]any{"type": "image", "data": "AAAA"},
			map[string]any{"type": "text", "text": "a caption"},
		}}
		preview, _ := toolResultPreview(resp, 100)
		require.Equal(t, "[image] a caption", preview)
	})

	t.Run("empty content falls back to the raw payload", func(t *testing.T) {
		preview, isErr := toolResultPreview(map[string]any{"content": []any{}, "isError": true}, 100)
		require.Equal(t, `{"content": [], "isError": true}`, preview)
		require.True(t, isErr)
	})

	t.Run("malformed content items keep the raw rendering", func(t *testing.T) {
		preview, _ := toolResultPreview(map[string]any{"content": []any{"not a map"}}, 100)
		require.Equal(t, `{"content": ["not a map"]}`, preview)
	})
}

// A direct MCP tool result (the filter_tools case) renders the payload the
// envelope carries, not the envelope itself.
func TestRenderToolActivity_UnwrapsMCPResultEnvelope(t *testing.T) {
	posts := captureToolPostBlocks(t, detailsFull,
		toolCallDelta("filter_tools"),
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolResult, Name: "filter_tools",
			Response: mcpEnvelope("{\n  \"filters\": {\n    \"query\": \"list clusters\"\n  }\n}", false),
		}},
	)
	require.Len(t, posts, 2)
	require.Contains(t, posts[1], "↳ *`filter_tools`* result")
	require.Contains(t, posts[1], `{"filters": {"query": "list clusters"}}`)
	require.NotContains(t, posts[1], "content", "no envelope boilerplate in the preview")
	require.NotContains(t, posts[1], `\n`, "no literal escape sequences in the preview")
}

// A muster call_tool result is an envelope whose text is the serialized inner
// result: the entry names the inner tool and previews the innermost payload.
func TestRenderToolActivity_UnwrapsMusterDoubleWrappedResult(t *testing.T) {
	inner := serialize(t, mcpEnvelope(`{"clusters":["alpha","beta"]}`, false))
	posts := captureToolPostBlocks(t, detailsFull,
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolCall, Name: musterCallToolMetaTool, CallID: "c1",
			Args: map[string]any{"name": "x_kubernetes_capi_list_clusters", "arguments": map[string]any{"management_cluster": "gazelle"}},
		}},
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolResult, Name: musterCallToolMetaTool, CallID: "c1",
			Response: mcpEnvelope(inner, false),
		}},
	)
	require.Len(t, posts, 2)
	require.Contains(t, posts[1], "↳ *`x_kubernetes_capi_list_clusters`* result (via muster)")
	require.Contains(t, posts[1], `{"clusters": ["alpha", "beta"]}`)
	require.NotContains(t, posts[1], `\"`, "no double-escaped quotes in the preview")
}

// A result the tool flagged as an error is marked visibly.
func TestRenderToolActivity_FlagsErrorResults(t *testing.T) {
	posts := captureToolPostBlocks(t, detailsFull,
		toolCallDelta("kube_get"),
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolResult, Name: "kube_get",
			Response: mcpEnvelope("forbidden: access denied", true),
		}},
	)
	require.Len(t, posts, 2)
	require.Contains(t, posts[1], "↳ ⚠️ *`kube_get`* result")
	require.Contains(t, posts[1], "forbidden: access denied")
}

// Unwrapped result text is MCP-server-controlled and no longer neutralised by
// JSON marshaling: the mrkdwn escaping must hold on the plain-text path.
func TestRenderToolActivity_UnwrappedResultEscapesHostileText(t *testing.T) {
	posts := captureToolPostBlocks(t, detailsFull,
		toolCallDelta("kube_get"),
		channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Kind: channels.ToolResult, Name: "kube_get",
			Response: mcpEnvelope("ping <!channel> then `break`\nout <@U1>", false),
		}},
	)
	require.Len(t, posts, 2)
	require.NotContains(t, posts[1], "<!channel>")
	require.NotContains(t, posts[1], "<@U1>")
	require.Contains(t, posts[1], "&lt;!channel&gt;")
	require.NotContains(t, posts[1], "`break`", "backticks must not terminate the code span")
	require.NotContains(t, posts[1], "\nout", "newlines must not carry content out of the span")
}

// Tool names and payload previews are agent- and MCP-server-controlled text
// entering an mrkdwn context block: mrkdwn control sequences must arrive
// escaped so a quoted <!channel> cannot notify, and backticks cannot break out
// of the code span.
func TestRenderToolActivity_EscapesMrkdwnAndCodeSpans(t *testing.T) {
	entries := captureToolPostBlocks(t, detailsFull, channels.OutboundDelta{
		Kind: channels.DeltaToolActivity,
		Tool: &channels.ToolActivity{
			Kind: channels.ToolCall,
			Name: "no`tify <!channel>",
			Args: map[string]any{"msg": "<@U1>"},
		},
	})
	require.Len(t, entries, 1)
	require.NotContains(t, entries[0], "<!channel>")
	require.NotContains(t, entries[0], "<@U1>")
	require.Contains(t, entries[0], "&lt;!channel&gt;")
	require.Contains(t, entries[0], "no'tify", "backticks in the name must not terminate the code span")
}

// In the assistant pane the live ticker renders as the native status line
// under the composer (assistant.threads.setStatus), never as message content:
// the segment's only in-thread artifact is the collapsed receipt, and the turn
// end clears the status explicitly so nothing strands "working…".
func TestPaneToolStatus_NativeTickerLeavesOnlyReceipt(t *testing.T) {
	ft := &fakeThread{}
	msgs, _, err := runSurfaceWriter(t, ft, "D1",
		toolCallDelta("alpha"),
		toolCallDelta("beta"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "the answer"},
		doneDelta(),
	)
	require.NoError(t, err)

	require.Len(t, msgs, 2)
	require.Equal(t, capturedMessage{"🛠️ 2 steps · alpha · beta"}, msgs[0],
		"the receipt is the only ticker artifact left in the thread")
	require.Equal(t, capturedMessage{"the answer"}, msgs[1])
	require.False(t, ft.sawText("⏳"), "no message revision ever carried the live ticker line")

	statuses := ft.statuses()
	require.NotEmpty(t, statuses)
	require.Equal(t, "", statuses[len(statuses)-1], "turn end clears the native status")
	live := statuses[:len(statuses)-1]
	require.NotEmpty(t, live)
	require.Equal(t, "⏳ beta… · step 2", live[len(live)-1])
	for _, s := range live {
		require.True(t, strings.HasPrefix(s, "⏳ "), "live status %q renders the ticker line", s)
	}
	for _, c := range ft.statusCalls {
		require.Equal(t, "D1", c.channelID)
		require.Equal(t, "1.0", c.threadTS)
	}
}

// A channel thread has no assistant pane: the ticker stays a message and
// assistant.threads.setStatus is never attempted.
func TestChannelToolStatus_NeverCallsSetStatus(t *testing.T) {
	ft := &fakeThread{}
	msgs, _, err := runSurfaceWriter(t, ft, "C1",
		toolCallDelta("alpha"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "the answer"},
		doneDelta(),
	)
	require.NoError(t, err)

	require.Empty(t, ft.statuses())
	require.Len(t, msgs, 2)
	require.Equal(t, capturedMessage{"🛠️ 1 step · alpha"}, msgs[0])
	require.True(t, ft.sawText("⏳"), "the live line renders as a message ticker in channels")
}

// missing_scope means the install cannot set the native status at all (no
// assistant:write, or not an Agent-type app — every plain-DM deployment lands
// here): one rejection latches the process-wide downgrade, the message ticker
// takes over, and no further setStatus calls are made — including the trailing
// clear, which has nothing to clear.
func TestPaneToolStatus_MissingScopeLatchesMessageTicker(t *testing.T) {
	ft := &fakeThread{failStatus: "missing_scope"}
	msgs, w, err := runSurfaceWriter(t, ft, "D1",
		toolCallDelta("alpha"),
		narrationDelta("one\ntwo\nthree"), // own-message narration forces a second render cycle
		toolCallDelta("beta"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "done"},
		doneDelta(),
	)
	require.NoError(t, err)

	require.True(t, w.adapter.assistantStatusUnsupported.Load())
	require.Len(t, ft.statuses(), 1, "the downgrade latches on the first rejection")
	require.True(t, ft.sawText("⏳"), "the message ticker took over")
	require.Len(t, msgs, 4)
	require.Equal(t, capturedMessage{"🛠️ 1 step · alpha"}, msgs[0])
	require.Equal(t, capturedMessage{"one\ntwo\nthree"}, msgs[1])
	require.Equal(t, capturedMessage{"🛠️ 1 step · beta"}, msgs[2])
	require.Equal(t, capturedMessage{"done"}, msgs[3])
}

// A transient setStatus failure falls back to the message ticker for that
// render without writing off the whole process: the latch stays unset so a
// later segment tries the native line again.
func TestPaneToolStatus_TransientFailureDoesNotLatch(t *testing.T) {
	ft := &fakeThread{failStatus: "fatal_error"}
	msgs, w, err := runSurfaceWriter(t, ft, "D1",
		toolCallDelta("alpha"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "done"},
		doneDelta(),
	)
	require.NoError(t, err)

	require.False(t, w.adapter.assistantStatusUnsupported.Load())
	require.True(t, ft.sawText("⏳"), "the render fell back to the message ticker")
	require.Equal(t, capturedMessage{"🛠️ 1 step · alpha"}, msgs[0])
	for _, s := range ft.statuses() {
		require.NotEqual(t, "", s, "no clear is sent when the native line never landed")
	}
}

// A turn pausing on an approval prompt clears the native status: the app posts
// nothing further into the thread until the user decides, so without the
// explicit clear "working…" would sit under the composer while the agent waits.
func TestPaneToolStatus_ClearsOnPromptPause(t *testing.T) {
	ft := &fakeThread{}
	_, w, err := runSurfaceWriter(t, ft, "D1",
		toolCallDelta("alpha"),
		channels.OutboundDelta{Kind: channels.DeltaPrompt, TaskID: "task-1"},
	)
	require.NoError(t, err)
	require.NotNil(t, w.promptDelta)

	statuses := ft.statuses()
	require.NotEmpty(t, statuses)
	require.Equal(t, "", statuses[len(statuses)-1], "the prompt pause clears the native status")
}

// A turn ending on a stream error clears the native status so the failure does
// not strand "working…" under the composer.
func TestPaneToolStatus_ClearsOnStreamError(t *testing.T) {
	errStream := errors.New("stream failed")
	ft := &fakeThread{}
	_, _, err := runSurfaceWriter(t, ft, "D1",
		toolCallDelta("alpha"),
		channels.OutboundDelta{Err: errStream},
	)
	require.ErrorIs(t, err, errStream)

	statuses := ft.statuses()
	require.NotEmpty(t, statuses)
	require.Equal(t, "", statuses[len(statuses)-1], "the failed turn clears the native status")
}

// setAssistantStatus maps the unsupported-class rejections to
// errAssistantStatusUnsupported (the latch signal) and surfaces everything
// else as-is.
func TestSetAssistantStatus_ErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		slackErr string
		latches  bool
	}{
		{"missing_scope", true},
		{"not_allowed_token_type", true},
		{"method_not_supported_for_channel_type", true},
		{"fatal_error", false},
		{"invalid_thread_ts", false},
	} {
		t.Run(tc.slackErr, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, tc.slackErr)
			}))
			t.Cleanup(srv.Close)
			c := &slackAPIClient{botToken: "t", baseURL: srv.URL}
			err := c.setAssistantStatus(t.Context(), "D1", "1.0", "⏳ x…")
			require.Error(t, err)
			require.Equal(t, tc.latches, errors.Is(err, errAssistantStatusUnsupported))
		})
	}
}
