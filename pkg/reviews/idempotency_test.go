package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	valkeystore "github.com/giantswarm/klaus-gateway/pkg/routing/store/valkey"
)

// keyedHandler is the endpoint as main wires it for the team reviews, its
// receipts in receipts.
func keyedHandler(receipts store.ReceiptStore, poster channels.TeamReviewPoster) *Handler {
	return &Handler{
		Auth:           &stubAuth{tokens: map[string]string{"good": managerSA}},
		AllowedCallers: []string{managerSA},
		Poster:         poster,
		Receipts:       receipts,
	}
}

func memoryReceipts(t *testing.T) *memory.Store {
	t.Helper()
	s := memory.New()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func valkeyReceipts(t *testing.T, addr string) *valkeystore.Store {
	t.Helper()
	s, err := valkeystore.New(valkeystore.Options{URL: addr, Timeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// Every POST of the three surfaces: the same request sent again with its key
// posts nothing and is answered with the first answer, marked as replayed.
func TestIdempotencyKey_SameRequestIsReplayed(t *testing.T) {
	poster := &recordingPoster{}
	decisions := &recordingDecisions{}
	conversations := &recordingConversations{}
	h := keyedHandler(memoryReceipts(t), poster)
	h.Decisions, h.Conversations = decisions, conversations
	srv := newHandlerServer(t, h)

	for name, tc := range map[string]struct {
		path   string
		body   map[string]any
		status int
		posts  func() int
	}{
		"review":       {"/reviews", validReview(), http.StatusCreated, func() int { return len(poster.reviews) }},
		"result":       {"/reviews/rv1/results", map[string]any{"text": "merged"}, http.StatusCreated, func() int { return len(poster.results) }},
		"notice":       {"/notices", map[string]any{"team": "team-honeybadger", "channel": "C0123ABCDE", "text": "archived"}, http.StatusCreated, func() int { return len(poster.notices) }},
		"decision":     {"/decisions", validDecision(), http.StatusCreated, func() int { return len(decisions.decisions) }},
		"close":        {"/decisions/C0123ABCDE-4.000/close", map[string]any{"outcome": "defaulted"}, http.StatusOK, func() int { return len(decisions.closes) }},
		"conversation": {"/conversations", validConversation(), http.StatusCreated, func() int { return len(conversations.opened) }},
		"message":      {"/conversations/D0123ABCDE-5.000/messages", map[string]any{"text": "noted"}, http.StatusCreated, func() int { return len(conversations.messages) }},
	} {
		t.Run(name, func(t *testing.T) {
			first := postWithKey(t, srv, tc.path, "good", "key-"+name, tc.body)
			second := postWithKey(t, srv, tc.path, "good", "key-"+name, tc.body)

			require.Equal(t, tc.status, first.StatusCode)
			require.Empty(t, first.Header.Get("Idempotency-Replayed"))
			require.Equal(t, tc.status, second.StatusCode)
			require.Equal(t, "true", second.Header.Get("Idempotency-Replayed"))
			require.Equal(t, readBody(t, first), readBody(t, second), "the first answer, byte for byte")
			require.Equal(t, 1, tc.posts(), "Slack saw the request once")
		})
	}
}

// gatedPoster holds the first review it is asked to post until release is
// closed, and counts the posts. Like the Slack call, a post whose context
// ended meanwhile fails.
type gatedPoster struct {
	recordingPoster
	posts   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (p *gatedPoster) PostTeamReview(ctx context.Context, review channels.TeamReview) (channels.PostReceipt, error) {
	if p.posts.Add(1) == 1 {
		close(p.entered)
		<-p.release
	}
	if err := ctx.Err(); err != nil {
		return channels.PostReceipt{}, err
	}
	return channels.PostReceipt{ID: "rv1", Channel: review.Channel, TS: "1.000"}, nil
}

// Two replicas of the gateway share the store, as installations run on
// Valkey. While the first request with a key is posting, the same request on
// the other replica is told it is still running and posts nothing; once the
// first has answered, the request is answered from its receipt.
func TestIdempotencyKey_ConcurrentRequestsPostOnce(t *testing.T) {
	for name, stores := range map[string]func(t *testing.T) (store.ReceiptStore, store.ReceiptStore){
		"memory": func(t *testing.T) (store.ReceiptStore, store.ReceiptStore) {
			s := memoryReceipts(t)
			return s, s
		},
		"valkey": func(t *testing.T) (store.ReceiptStore, store.ReceiptStore) {
			m := miniredis.RunT(t)
			return valkeyReceipts(t, m.Addr()), valkeyReceipts(t, m.Addr())
		},
	} {
		t.Run(name, func(t *testing.T) {
			one, two := stores(t)
			poster := &gatedPoster{entered: make(chan struct{}), release: make(chan struct{})}
			replicaOne := newHandlerServer(t, keyedHandler(one, poster))
			replicaTwo := newHandlerServer(t, keyedHandler(two, poster))
			release := sync.OnceFunc(func() { close(poster.release) })
			defer release() // a failed assertion must not leave the first request holding the server open
			body := validReview()

			type result struct {
				resp *http.Response
				err  error
			}
			first := make(chan result, 1)
			req := newRequest(t, replicaOne, "/reviews", "good", "k1", body)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				first <- result{resp, err}
			}()
			select {
			case <-poster.entered:
			case res := <-first:
				require.NoError(t, res.err)
				t.Fatalf("the first request was answered %d before it posted", res.resp.StatusCode)
			}

			running := postWithKey(t, replicaTwo, "/reviews", "good", "k1", body)
			require.Equal(t, http.StatusServiceUnavailable, running.StatusCode)
			require.Equal(t, "1", running.Header.Get("Retry-After"))

			release()
			answered := <-first
			require.NoError(t, answered.err)
			t.Cleanup(func() { _ = answered.resp.Body.Close() })
			require.Equal(t, http.StatusCreated, answered.resp.StatusCode)

			replayed := postWithKey(t, replicaTwo, "/reviews", "good", "k1", body)
			require.Equal(t, http.StatusCreated, replayed.StatusCode)
			require.Equal(t, "true", replayed.Header.Get("Idempotency-Replayed"))
			require.Equal(t, readBody(t, answered.resp), readBody(t, replayed))
			require.EqualValues(t, 1, poster.posts.Load(), "Slack saw the request once")
		})
	}
}

// A request with a key keeps running when its caller hangs up, so the
// caller's retry finds its answer instead of posting a second time.
func TestIdempotencyKey_ACallerThatHangsUpFindsItsAnswer(t *testing.T) {
	poster := &gatedPoster{entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(poster.release) })
	defer release()
	// started hands over each request's own context, finished says it was
	// answered: the caller that hung up sees neither.
	started := make(chan context.Context, 2)
	finished := make(chan struct{}, 2)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			started <- req.Context()
			defer func() { finished <- struct{}{} }()
			next.ServeHTTP(w, req)
		})
	})
	keyedHandler(memoryReceipts(t), poster).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	callerCtx, hangUp := context.WithCancel(context.Background())
	req := newRequest(t, srv, "/reviews", "good", "k1", validReview()).WithContext(callerCtx)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	gatewayCtx := <-started
	select {
	case <-poster.entered:
	case <-finished:
		t.Fatal("the request was answered before it posted")
	}
	hangUp()
	<-gatewayCtx.Done() // the gateway saw the caller go
	release()
	<-finished

	again := postWithKey(t, srv, "/reviews", "good", "k1", validReview())
	require.Equal(t, http.StatusCreated, again.StatusCode)
	require.Equal(t, "true", again.Header.Get("Idempotency-Replayed"))
	require.EqualValues(t, 1, poster.posts.Load(), "Slack saw the request once")
}

// A key is its caller's own: another ServiceAccount sending the same request
// with the same key runs it, and is not answered from the first one's
// receipt.
func TestIdempotencyKey_IsTheCallersOwn(t *testing.T) {
	poster := &recordingPoster{}
	h := keyedHandler(memoryReceipts(t), poster)
	h.Auth = &stubAuth{tokens: map[string]string{"good": managerSA, "other": otherSA}}
	h.AllowedCallers = []string{managerSA, otherSA}
	srv := newHandlerServer(t, h)

	require.Equal(t, http.StatusCreated, postWithKey(t, srv, "/reviews", "good", "k1", validReview()).StatusCode)
	other := postWithKey(t, srv, "/reviews", "other", "k1", validReview())
	require.Equal(t, http.StatusCreated, other.StatusCode)
	require.Empty(t, other.Header.Get("Idempotency-Replayed"))
	require.Len(t, poster.reviews, 2)
}

// failingReceipts is a receipt store that does not answer: Valkey down.
type failingReceipts struct{}

func (failingReceipts) UpdateReceipt(context.Context, string, func(*store.Receipt, bool) bool) error {
	return errors.New("valkey: get receipt: dial tcp: connection refused")
}

// A key the store cannot check does not run the request unprotected: the
// caller is told to send it again, and nothing is posted.
func TestIdempotencyKey_AnUncheckedKeyPostsNothing(t *testing.T) {
	poster := &recordingPoster{}
	srv := newHandlerServer(t, keyedHandler(failingReceipts{}, poster))

	require.Equal(t, http.StatusServiceUnavailable, postWithKey(t, srv, "/reviews", "good", "k1", validReview()).StatusCode)
	require.Empty(t, poster.reviews)
}

// A key belongs to the first request sent with it: the key on another
// request is a conflict, and a key past its bound is refused; neither posts.
func TestIdempotencyKey_Refusals(t *testing.T) {
	poster := &recordingPoster{}
	srv := newHandlerServer(t, keyedHandler(memoryReceipts(t), poster))
	require.Equal(t, http.StatusCreated, postWithKey(t, srv, "/reviews", "good", "k1", validReview()).StatusCode)

	otherText := validReview()
	otherText["text"] = "*Archive* `giantswarm/other-thing`."
	for name, tc := range map[string]struct {
		path, key string
		body      map[string]any
		status    int
	}{
		"another body":  {"/reviews", "k1", otherText, http.StatusConflict},
		"another route": {"/notices", "k1", map[string]any{"team": "team-bumblebee", "channel": "C0123ABCDE", "text": "x"}, http.StatusConflict},
		"key too long":  {"/reviews", strings.Repeat("k", idempotencyKeyMax+1), validReview(), http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			resp := postWithKey(t, srv, tc.path, "good", tc.key, tc.body)
			require.Equal(t, tc.status, resp.StatusCode)
			if tc.status == http.StatusConflict {
				require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
				var refusal struct{ Error string }
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&refusal))
				require.NotEmpty(t, refusal.Error)
			}
		})
	}
	require.Len(t, poster.reviews, 1, "only the first request posted")
	require.Empty(t, poster.notices)
}

// An answer that is not a success is not kept: the request did not go
// through, and sent again with its key it runs again.
func TestIdempotencyKey_AFailureIsNotKept(t *testing.T) {
	poster := &recordingPoster{err: errors.New("slack: ratelimited")}
	srv := newHandlerServer(t, keyedHandler(memoryReceipts(t), poster))

	require.Equal(t, http.StatusBadGateway, postWithKey(t, srv, "/reviews", "good", "k1", validReview()).StatusCode)
	poster.err = nil
	again := postWithKey(t, srv, "/reviews", "good", "k1", validReview())
	require.Equal(t, http.StatusCreated, again.StatusCode)
	require.Empty(t, again.Header.Get("Idempotency-Replayed"), "the request ran again")
	require.Len(t, poster.reviews, 2)
}

// A gateway that stopped between taking a key and answering leaves a claim
// without an answer. Past claimAbandoned it no longer holds the key: the
// request sent again runs.
func TestIdempotencyKey_AnAbandonedClaimRunsAgain(t *testing.T) {
	receipts := memoryReceipts(t)
	poster := &recordingPoster{}
	srv := newHandlerServer(t, keyedHandler(receipts, poster))
	body := validReview()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	require.NoError(t, receipts.UpdateReceipt(context.Background(), receiptID(managerSA, "k1"), func(r *store.Receipt, _ bool) bool {
		*r = store.Receipt{Hash: requestHash(http.MethodPost, "/reviews", payload), ClaimedAt: time.Now().Add(-claimAbandoned - time.Minute), TTL: receiptTTL}
		return true
	}))

	resp := postWithKey(t, srv, "/reviews", "good", "k1", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Len(t, poster.reviews, 1)
}
