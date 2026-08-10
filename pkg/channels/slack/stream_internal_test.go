package slack

import (
	"context"
	"encoding/json"
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
	w.enqueueThreadPost(t.Context(), threadPost{md: "tool one"})
	require.NotPanics(t, w.drainThreadPosts)

	// Resumed segment over the same writer: a fresh poster starts and drains
	// without re-closing the first segment's queue. Narration shares the queue and
	// carries its per-turn count across cycles.
	require.NotPanics(t, func() {
		w.enqueueThreadPost(t.Context(), threadPost{md: "tool two"})
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

// capturedPost is one Slack write recorded by capturePosts: the API method and
// the markdown of its first block.
type capturedPost struct {
	method string
	md     string
}

// capturePosts drives run() over deltas (plus a terminal Done) against a fake
// Slack server and returns each write in order, together with the writer. headTS
// empty is reactions mode, where the main reply is posted lazily on the first
// flush; a non-empty headTS is text mode, where it is updated in place.
func capturePosts(t *testing.T, details detailsLevel, headTS string, deltas ...channels.OutboundDelta) ([]capturedPost, *batchedWriter) {
	t.Helper()
	var mu sync.Mutex
	var got []capturedPost
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Blocks []struct {
				Text string `json:"text"`
			} `json:"blocks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Blocks) > 0 {
			mu.Lock()
			got = append(got, capturedPost{method: path.Base(r.URL.Path), md: body.Blocks[0].Text})
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1"}`)
	}))
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

	mu.Lock()
	defer mu.Unlock()
	return append([]capturedPost(nil), got...), w
}

// captureToolPostBlocks returns just the markdown of each write, in text mode.
func captureToolPostBlocks(t *testing.T, details detailsLevel, deltas ...channels.OutboundDelta) []string {
	t.Helper()
	posts, _ := capturePosts(t, details, "1.1", deltas...)
	md := make([]string, 0, len(posts))
	for _, p := range posts {
		md = append(md, p.md)
	}
	return md
}

func narrationDelta(text string) channels.OutboundDelta {
	return channels.OutboundDelta{Kind: channels.DeltaNarration, Content: text}
}

func toolCallDelta(name string) channels.OutboundDelta {
	return channels.OutboundDelta{
		Kind: channels.DeltaToolActivity,
		Tool: &channels.ToolActivity{Kind: channels.ToolCall, Name: name},
	}
}

// The agent's narration reads in order with the tool posts it introduces, and the
// answer still lands last (klaus-gateway#197).
func TestRenderNarration_PostsOwnMessageBeforeToolPostsAndAnswer(t *testing.T) {
	posts, w := capturePosts(t, detailsOn, "",
		narrationDelta("Let me pull the HelmRelease from both clusters."),
		toolCallDelta("x_kubernetes_get"),
		toolCallDelta("x_kubernetes_get"),
		narrationDelta("Both share the same chart version."),
		toolCallDelta("x_kubernetes_get"),
		channels.OutboundDelta{Kind: channels.DeltaText, Content: "here is the diff"},
	)

	require.Len(t, posts, 6)
	for _, p := range posts {
		require.Equal(t, "chat.postMessage", p.method, "reactions mode posts every message, including the reply")
	}
	require.Equal(t, "Let me pull the HelmRelease from both clusters.", posts[0].md)
	require.Contains(t, posts[1].md, "🔧 *`x_kubernetes_get`*")
	require.Contains(t, posts[2].md, "🔧 *`x_kubernetes_get`*")
	require.Equal(t, "Both share the same chart version.", posts[3].md)
	require.Contains(t, posts[4].md, "🔧 *`x_kubernetes_get`*")
	require.Equal(t, "here is the diff", posts[5].md, "the answer is the turn's last message")
	require.True(t, w.wroteContent())
}

// Narration is the agent talking, not tool transparency, so /details off mutes
// the tool post and keeps the prose.
func TestRenderNarration_ShownWithDetailsOff(t *testing.T) {
	posts, _ := capturePosts(t, detailsOff, "",
		narrationDelta("Let me look that up."),
		toolCallDelta("x_kubernetes_get"),
	)

	require.Len(t, posts, 1)
	require.Equal(t, "Let me look that up.", posts[0].md)
}

// Narration must stay out of the main reply buffer: the reply is posted lazily on
// the first flush, and seeding it early would put the answer above the narration
// and tool posts it followed. It also must not count as delivered content, or a
// turn that only narrated would leave its "thinking" placeholder in place.
func TestRenderNarration_DoesNotTouchMainReply(t *testing.T) {
	posts, w := capturePosts(t, detailsOn, "", narrationDelta("Let me look that up."))

	require.Len(t, posts, 1)
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
	posts, _ := capturePosts(t, detailsOn, "", deltas...)

	require.Len(t, posts, maxNarrationMessages+2, "capped narration, its note, and the tool post")
	require.Equal(t, narrationLimitNote, posts[maxNarrationMessages].md)
	require.Contains(t, posts[maxNarrationMessages+1].md, "🔧 *`x_kubernetes_get`*", "tool activity keeps its own budget")
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

// Narration is the same sign-in prose the retract exists for, so it goes with the
// reply when an auto-resuming Connect button takes the turn over.
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
	ch <- narrationDelta("slow narration")
	ch <- channels.OutboundDelta{Kind: channels.DeltaText, Content: "the answer"}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)
	require.NoError(t, w.run(t.Context(), ch))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"slow narration", "the answer"}, posted)
}

func TestRenderToolActivity_UnwrapsCallTool(t *testing.T) {
	posts := captureToolPostBlocks(t, detailsOn, channels.OutboundDelta{
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
	posts := captureToolPostBlocks(t, detailsOn, channels.OutboundDelta{
		Kind: channels.DeltaToolActivity,
		Tool: &channels.ToolActivity{Kind: channels.ToolCall, Name: "list_pods"},
	})
	require.Len(t, posts, 1)
	require.Contains(t, posts[0], "🔧 *`list_pods`*")
	require.NotContains(t, posts[0], "via muster")
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
	require.Contains(t, posts[1], `{"output": "ok"}`)
}
