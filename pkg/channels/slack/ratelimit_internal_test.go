package slack

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recordingRateLimits is a RateLimitRecorder that keeps what it was given, as
// "<method> <outcome>" pairs.
type recordingRateLimits struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingRateLimits) RecordSlackRateLimit(method, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, method+" "+outcome)
}

func (r *recordingRateLimits) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// A 429 the client waits out and retries is counted, even though the call
// succeeds and leaves no other trace: that count is what says a workspace is
// being throttled before anything fails.
func TestCall_CountsA429ItRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// Zero seconds keeps the wait instant; the branch under test is
			// the one a real Retry-After takes.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer srv.Close()

	rec := &recordingRateLimits{}
	c := &slackAPIClient{botToken: "t", baseURL: srv.URL, rateLimits: rec}

	body, err := c.call(t.Context(), methodChatAppendStream, "application/x-www-form-urlencoded", "")
	require.NoError(t, err)
	require.Contains(t, string(body), `"ok":true`)
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, []string{methodChatAppendStream + " " + rateLimitRetried}, rec.recorded())
}

// A call Slack rate-limits on every attempt is counted once per wait and once
// more for giving up, which is the count that pairs with the error the caller
// sees.
func TestCall_CountsTheAttemptsAndTheGivingUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rec := &recordingRateLimits{}
	c := &slackAPIClient{botToken: "t", baseURL: srv.URL, rateLimits: rec, logger: slog.Default()}

	_, err := c.call(t.Context(), methodChatAppendStream, "application/x-www-form-urlencoded", "")
	require.ErrorContains(t, err, "rate limited")
	require.Equal(t, int32(4), calls.Load(), "the attempt budget is spent")
	require.Equal(t, []string{
		methodChatAppendStream + " " + rateLimitRetried,
		methodChatAppendStream + " " + rateLimitRetried,
		methodChatAppendStream + " " + rateLimitRetried,
		methodChatAppendStream + " " + rateLimitExhausted,
	}, rec.recorded())
}

// A Retry-After over the cap is given up on at once: counted exhausted, never
// waited out, never retried.
func TestCall_CountsAWaitOverTheCapAsGivenUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rec := &recordingRateLimits{}
	c := &slackAPIClient{botToken: "t", baseURL: srv.URL, rateLimits: rec}

	start := time.Now()
	_, err := c.call(t.Context(), methodChatStartStream, "application/x-www-form-urlencoded", "")
	require.ErrorContains(t, err, "rate limited")
	require.Less(t, time.Since(start), time.Second, "a wait over the cap is not waited out")
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, []string{methodChatStartStream + " " + rateLimitExhausted}, rec.recorded())
}
