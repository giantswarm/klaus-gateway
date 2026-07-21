package slack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

const helloText = "hello"

// signBody computes the x-slack-signature header value for body.
func signBody(t *testing.T, signingSecret string, body []byte) (ts, sig string) {
	t.Helper()
	ts = fmt.Sprintf("%d", time.Now().Unix())
	base := "v0:" + ts + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(base))
	sig = "v0=" + hex.EncodeToString(mac.Sum(nil))
	return ts, sig
}

// --- VerifySignature ---

func TestVerifySignature_Valid(t *testing.T) {
	body := []byte(`{"type":"url_verification","challenge":"abc"}`)
	ts, sig := signBody(t, "test-secret", body)
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", ts)
	h.Set("X-Slack-Signature", sig)
	require.NoError(t, slackadapter.VerifySignature("test-secret", h, body))
}

func TestVerifySignature_InvalidSig(t *testing.T) {
	body := []byte(`{}`)
	ts, _ := signBody(t, "test-secret", body)
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", ts)
	h.Set("X-Slack-Signature", "v0=badbad")
	require.Error(t, slackadapter.VerifySignature("test-secret", h, body))
}

func TestVerifySignature_StaleTimestamp(t *testing.T) {
	body := []byte(`{}`)
	stale := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	base := "v0:" + stale + ":" + string(body)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(base))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", stale)
	h.Set("X-Slack-Signature", sig)
	require.Error(t, slackadapter.VerifySignature("test-secret", h, body))
}

func TestVerifySignature_MissingHeaders(t *testing.T) {
	require.Error(t, slackadapter.VerifySignature("s", http.Header{}, []byte("x")))
}

// --- StripMention ---

func TestStripMention(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"<@U12345> " + helloText, helloText},
		{"<@U12345>" + helloText, helloText},
		{"<@BOT> hi there", "hi there"},
		{"no mention here", "no mention here"},
		{"", ""},
		// Non-mention angle-bracket tokens are message content, not mention noise.
		{"<@BOT> <https://grafana.example/alert/123> explain this", "<https://grafana.example/alert/123> explain this"},
		{"<https://example.com|link> hi", "<https://example.com|link> hi"},
		{"<#C123|general> hello", "<#C123|general> hello"},
		{"<@U1><@U2> hi", "hi"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, slackadapter.StripMention(tc.in))
		})
	}
}

// --- Events API handler ---

// channelMode configures the adapter to serve channels and redirect DMs (the
// production default). Used by tests that drive the adapter through a channel
// app_mention. The harness otherwise defaults to DM-only (see newEventsAdapter),
// since most tests use a 1:1 DM as a single-permitted-user surface.
func channelMode(a *slackadapter.Adapter) { a.DMOnly = false }

func newEventsAdapter(t *testing.T, gw channels.Gateway, fakeAPIBase string, opts ...func(*slackadapter.Adapter)) (*slackadapter.Adapter, *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	secrets := slackadapter.Secrets{ //nolint:gosec // G101 dummy values used only in tests
		BotToken:      "dummy-bot-token",
		SigningSecret: "signing-secret",
	}
	a := &slackadapter.Adapter{
		Mode:         slackadapter.ModeEvents,
		Secrets:      secrets,
		APIBase:      fakeAPIBase,
		DefaultAgent: "test-agent",
		// Default to serving DMs: most tests drive the adapter through a 1:1 DM.
		// Channel-driven tests opt into channelMode.
		DMOnly: true,
	}
	for _, opt := range opts {
		opt(a)
	}
	require.NoError(t, a.Start(ctx, gw))
	r := chi.NewRouter()
	a.Mount(r)
	ts := httptest.NewServer(r)
	// Cancel the adapter context first so dispatch goroutines exit, then close the HTTP server.
	t.Cleanup(cancel)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, ts
}

func TestEventsHandler_URLVerification(t *testing.T) {
	_, srv := newEventsAdapter(t, &stubGateway{}, "")

	body := []byte(`{"type":"url_verification","challenge":"test-challenge-xyz"}`)
	stamp, sig := signBody(t, "signing-secret", body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "test-challenge-xyz", got["challenge"])
}

func TestEventsHandler_InvalidSignature(t *testing.T) {
	_, srv := newEventsAdapter(t, &stubGateway{}, "")

	body := []byte(`{"type":"url_verification","challenge":"x"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("X-Slack-Signature", "v0=badsig")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestEventsHandler_AppMentionDispatch(t *testing.T) {
	var mu sync.Mutex
	var capturedMessages []channels.InboundMessage

	gw := &stubGateway{
		onResolve: func(msg channels.InboundMessage) {
			mu.Lock()
			capturedMessages = append(capturedMessages, msg)
			mu.Unlock()
		},
	}

	// Fake Slack API server: returns ok=true for postMessage and chatUpdate.
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	_, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)

	payload := `{
		"type":"event_callback",
		"event":{
			"type":"app_mention",
			"user":"U123",
			"text":"<@BOT> hello",
			"channel":"C456",
			"ts":"1234.5678"
		}
	}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Wait for the async goroutine to fire.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(capturedMessages) > 0
	}, 2*time.Second, 50*time.Millisecond, "expected dispatch to fire")

	mu.Lock()
	got := capturedMessages[0]
	mu.Unlock()
	require.Equal(t, "slack", got.Channel)
	require.Equal(t, "C456", got.ChannelID)
	require.Empty(t, got.UserID, "thread-scoped session shares one contextID")
	require.Equal(t, "U123", got.Subject, "Subject carries the raw Slack user ID for access control")
	require.Equal(t, helloText, got.Text)
	require.Equal(t, "test-agent", got.AgentRef, "AgentRef must be set to DefaultAgent")
}

func TestEventsHandler_RedeliveredEventDropped(t *testing.T) {
	var dispatched atomic.Int32
	gw := &stubGateway{
		onResolve: func(channels.InboundMessage) { dispatched.Add(1) },
	}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	_, srv := newEventsAdapter(t, gw, fakeSlack.URL)

	body := []byte(`{
		"type":"event_callback",
		"event":{"type":"app_mention","user":"U123","text":"<@BOT> hello","channel":"C456","ts":"1234.5678"}
	}`)
	stamp, sig := signBody(t, "signing-secret", body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	req.Header.Set("X-Slack-Retry-Num", "1")
	req.Header.Set("X-Slack-Retry-Reason", "http_timeout")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("X-Slack-No-Retry"))

	time.Sleep(200 * time.Millisecond)
	require.Zero(t, dispatched.Load(), "redelivered event must not start a duplicate turn")
}

// A newcomer who posts into a thread that already has an initiator is gated:
// their message does not reach the agent until the initiator approves them. The
// initiator's own launching mention dispatches normally.
func TestEventsHandler_NewcomerGatedAfterInitiator(t *testing.T) {
	gw := &stubGateway{}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := &slackadapter.Adapter{
		Mode:         slackadapter.ModeEvents,
		Secrets:      slackadapter.Secrets{BotToken: "dummy-bot-token", SigningSecret: "signing-secret"}, //nolint:gosec
		APIBase:      fakeSlack.URL,
		DefaultAgent: "test-agent",
	}
	require.NoError(t, a.Start(ctx, gw))
	r := chi.NewRouter()
	a.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(cancel)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	send := func(payload string) {
		body := []byte(payload)
		stamp, sig := signBody(t, "signing-secret", body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Slack-Request-Timestamp", stamp)
		req.Header.Set("X-Slack-Signature", sig)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	// U001 launches the thread and becomes its initiator.
	send(`{"type":"event_callback","event":{"type":"app_mention","user":"U001","text":"<@BOT> hi","channel":"C1","ts":"111.222"}}`)
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the initiator's mention is dispatched")

	// U999 tries to instruct in the same thread: gated, not dispatched.
	send(`{"type":"event_callback","event":{"type":"app_mention","user":"U999","text":"<@BOT> me too","channel":"C1","ts":"333.444","thread_ts":"111.222"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount(), "a newcomer must not trigger resolve until approved")
}

func TestEventsHandler_BotMessageIgnored(t *testing.T) {
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, "")

	payload := `{
		"type":"event_callback",
		"event":{
			"type":"message",
			"bot_id":"B001",
			"user":"U123",
			"text":"bot says hi",
			"channel":"C456",
			"ts":"111.222"
		}
	}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Give the goroutine time to run (it should not call Resolve).
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount())
}

// --- Batched writer via fake Slack API ---

func TestBatchedWriter_FlushesContent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: helloText},
			{Content: " world"},
			{Done: true},
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> go","channel":"C1","ts":"111.222"}}`)

	// Default (auto) mode posts the answer as a Block Kit markdown message. The
	// channel launch announcement also posts here, so wait for the answer text
	// rather than the first postMessage call.
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "hello world")
	}, 2*time.Second, 20*time.Millisecond, "streamed answer is posted")
}

// --- OBO injection ---

// fakeOBO is a test OBOTokenSource: it returns token for the configured Slack
// user and musterlink.ErrNotLinked for anyone else. When tokenErr is set it is
// returned for the linked user instead (a transient token-mint failure).
type fakeOBO struct {
	linkedUser string
	token      string
	tokenErr   error

	mu           sync.Mutex
	unlinked     []string
	linkURL      string
	notYetLinked bool // when true, linkedUser is treated as unlinked until completeLink
}

func (f *fakeOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	f.mu.Lock()
	notYet := f.notYetLinked
	f.mu.Unlock()
	if slackUserID == f.linkedUser && !notYet {
		if f.tokenErr != nil {
			return "", f.tokenErr
		}
		return f.token, nil
	}
	return "", musterlink.ErrNotLinked
}

// completeLink flips the user from unlinked to linked, simulating a finished
// sign-in flow.
func (f *fakeOBO) completeLink() {
	f.mu.Lock()
	f.notYetLinked = false
	f.mu.Unlock()
}

func (f *fakeOBO) LinkURL(slackUserID string) string {
	if f.linkURL != "" {
		return f.linkURL
	}
	return "https://gw.example.com/auth/slack/link?u=signed-" + slackUserID
}

func (f *fakeOBO) Unlink(slackUserID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlinked = append(f.unlinked, slackUserID)
}

// dispatchAndCaptureOBO posts an app_mention from slackUser and returns the
// InboundMessage seen by the gateway, with the adapter's OBO source set to obo.
func dispatchAndCaptureOBO(t *testing.T, obo slackadapter.OBOTokenSource, slackUser string) channels.InboundMessage {
	t.Helper()
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onResolve: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "users.info") {
			_, _ = fmt.Fprintf(w, `{"ok":true,"user":{"profile":{"email":"u@example.com"}}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := &slackadapter.Adapter{
		Mode:         slackadapter.ModeEvents,
		Secrets:      slackadapter.Secrets{BotToken: "dummy-bot-token", SigningSecret: "signing-secret"}, //nolint:gosec
		APIBase:      fakeSlack.URL,
		DefaultAgent: "test-agent",
		OBO:          obo,
	}
	require.NoError(t, a.Start(ctx, gw))
	r := chi.NewRouter()
	a.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(cancel)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	payload := fmt.Sprintf(`{"type":"event_callback","event":{"type":"app_mention","user":%q,"text":"<@BOT> hi","channel":"C1","ts":"111.222"}}`, slackUser)
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(captured) > 0
	}, 2*time.Second, 50*time.Millisecond, "expected dispatch to fire")

	mu.Lock()
	defer mu.Unlock()
	return captured[0]
}

func TestDispatch_OBO_LinkedUserSetsBearerToken(t *testing.T) {
	got := dispatchAndCaptureOBO(t, &fakeOBO{linkedUser: "U123", token: "human-muster-token"}, "U123")
	require.Equal(t, "human-muster-token", got.BearerToken, "linked user's turn must carry the human muster token")
}

func TestDispatch_OBO_DisabledLeavesBearerTokenEmpty(t *testing.T) {
	got := dispatchAndCaptureOBO(t, nil, "U123")
	require.Empty(t, got.BearerToken, "with OBO disabled the turn must run as M2M")
}

// With linking enabled, an unlinked user's turn is aborted with a sign-in
// prompt and never dispatched to the agent — no silent M2M service-account
// fallback (klaus-gateway#116).
func TestDispatch_OBO_UnlinkedUserPromptsSignInAndDoesNotDispatch(t *testing.T) {
	fakeSlack, ephemeral := captureEphemeral(t)
	defer fakeSlack.Close()

	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)
	a.OBO = &fakeOBO{linkedUser: "U999", token: "x", linkURL: "https://gw.example.com/auth/slack/link?u=xyz"}

	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> hi","channel":"C1","ts":"111.222"}}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Eventually(t, func() bool {
		return len(ephemeral()) >= 1
	}, 2*time.Second, 50*time.Millisecond, "unlinked user must be prompted to sign in")
	require.Zero(t, gw.resolveCount(), "unlinked turn must not reach the agent (no M2M fallback)")
}

// An unlinked user's message is parked while they sign in and replayed through
// dispatch once linked, so the question is answered without being re-typed.
func TestDispatch_OBO_ParksUnlinkedMessageAndReplaysAfterLink(t *testing.T) {
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onResolve: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}
	fakeSlack, ephemeral := captureEphemeral(t)
	defer fakeSlack.Close()
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)
	obo := &fakeOBO{linkedUser: "U123", token: "human-token", notYetLinked: true, linkURL: "https://gw.example.com/link"}
	a.OBO = obo

	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> what is failing?","channel":"C1","ts":"111.222"}}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Eventually(t, func() bool {
		return len(ephemeral()) >= 1
	}, 2*time.Second, 50*time.Millisecond, "unlinked user must be prompted to sign in")
	require.Zero(t, gw.resolveCount(), "the message must be parked, not dispatched, before linking")

	// The user completes sign-in; the callback hook replays the parked message.
	obo.completeLink()
	a.OnUserLinked(context.Background(), "U123", "u123@example.com")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(captured) == 1
	}, 2*time.Second, 50*time.Millisecond, "the parked message must replay after linking")
	mu.Lock()
	got := captured[0]
	mu.Unlock()
	require.Equal(t, "human-token", got.BearerToken, "the replayed turn carries the human muster token")
	require.Contains(t, got.Text, "what is failing?")
}

// multiUserOBO is a test OBOTokenSource with independent per-user link state, so
// a test can link one user while another stays unlinked.
type multiUserOBO struct {
	mu     sync.Mutex
	linked map[string]string // slackUser -> token
}

func (o *multiUserOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if tok, ok := o.linked[slackUserID]; ok {
		return tok, nil
	}
	return "", musterlink.ErrNotLinked
}

func (o *multiUserOBO) link(slackUserID, token string) {
	o.mu.Lock()
	o.linked[slackUserID] = token
	o.mu.Unlock()
}

func (o *multiUserOBO) LinkURL(string) string { return "https://gw.example.com/link" }
func (o *multiUserOBO) Unlink(string)         {}

// A newcomer who signs in mid-thread has their parked message replayed to the
// access-consent step, not dispatched to the agent: linking authenticates them,
// but the initiator must still approve before they can instruct the agent.
func TestDispatch_OBO_NewcomerReplaysToAccessPromptNotAgent(t *testing.T) {
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onResolve: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}
	fakeSlack, ephemeral := captureEphemeral(t)
	defer fakeSlack.Close()
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)
	obo := &multiUserOBO{linked: map[string]string{"U1": "tok1"}}
	a.OBO = obo

	send := func(payload string) {
		body := []byte(payload)
		stamp, sig := signBody(t, "signing-secret", body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Slack-Request-Timestamp", stamp)
		req.Header.Set("X-Slack-Signature", sig)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	// The initiator (linked) starts the thread and is dispatched.
	send(`{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@BOT> hi","channel":"C1","ts":"111.222"}}`)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(captured) == 1
	}, 2*time.Second, 50*time.Millisecond, "the initiator's turn is dispatched")

	// A newcomer (unlinked) tries to instruct in the same thread: parked + prompted
	// to sign in, not dispatched.
	send(`{"type":"event_callback","event":{"type":"app_mention","user":"U2","text":"<@BOT> me too","channel":"C1","ts":"333.444","thread_ts":"111.222"}}`)
	require.Eventually(t, func() bool {
		return len(ephemeral()) >= 1
	}, 2*time.Second, 50*time.Millisecond, "the newcomer is prompted to sign in")
	mu.Lock()
	require.Equal(t, 1, len(captured), "an unlinked newcomer must not reach the agent")
	mu.Unlock()

	// The newcomer signs in; the replay lands at the access-consent prompt, still
	// not dispatched to the agent.
	obo.link("U2", "tok2")
	a.OnUserLinked(context.Background(), "U2", "u2@example.com")
	require.Eventually(t, func() bool {
		return len(ephemeral()) >= 2
	}, 2*time.Second, 50*time.Millisecond, "the replayed newcomer message posts the initiator access prompt")
	mu.Lock()
	require.Equal(t, 1, len(captured), "a linked-but-unapproved newcomer must not reach the agent on replay")
	mu.Unlock()
}

// A linked user hitting a transient token-mint failure gets a clear error
// (ephemeral to them) and the turn is aborted — never run as the service account.
func TestDispatch_OBO_TokenErrorAbortsTurn(t *testing.T) {
	var mu sync.Mutex
	var messages int
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "users.info") {
			_, _ = fmt.Fprintf(w, `{"ok":true,"user":{"profile":{"email":"u@example.com"}}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "chat.postEphemeral") {
			mu.Lock()
			messages++
			mu.Unlock()
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)
	a.OBO = &fakeOBO{linkedUser: "U123", token: "x", tokenErr: errors.New("muster unreachable")}

	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> hi","channel":"C1","ts":"111.222"}}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return messages >= 1
	}, 2*time.Second, 50*time.Millisecond, "a transient token failure must surface an error message")
	require.Zero(t, gw.resolveCount(), "a transient token failure must not reach the agent as the SA")
}

func TestLookupUserEmail_Caches(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "users.info") {
			mu.Lock()
			calls++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true,"user":{"profile":{"email":"u@example.com"}}}`)
	}))
	defer srv.Close()

	a := &slackadapter.Adapter{
		Secrets: slackadapter.Secrets{BotToken: "dummy-bot-token"}, //nolint:gosec // G101 dummy value used only in tests
		APIBase: srv.URL,
	}
	for range 3 {
		got, err := a.LookupUserEmail(context.Background(), "U123")
		require.NoError(t, err)
		require.Equal(t, "u@example.com", got)
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls, "repeated lookups for the same user must hit users.info once")
}

// --- OBO sign-in UX ---

// captureEphemeral spins up a fake Slack API that records chat.postEphemeral
// request bodies and returns ok for everything else (placeholder, chat.update,
// users.info). It returns the server and an accessor for the captured bodies.
func captureEphemeral(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var ephemeral []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chat.postEphemeral") {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			mu.Lock()
			ephemeral = append(ephemeral, m)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "users.info") {
			_, _ = fmt.Fprintf(w, `{"ok":true,"user":{"profile":{"email":"u@example.com"}}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), ephemeral...)
	}
}

func TestLogout_Unlinks(t *testing.T) {
	fakeSlack, _ := captureEphemeral(t)
	defer fakeSlack.Close()

	obo := &fakeOBO{linkedUser: "U123", token: "human-token"}
	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)
	a.OBO = obo

	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> /logout","channel":"C1","ts":"111.222"}}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Eventually(t, func() bool {
		obo.mu.Lock()
		defer obo.mu.Unlock()
		return len(obo.unlinked) == 1 && obo.unlinked[0] == "U123"
	}, 2*time.Second, 50*time.Millisecond, "/logout must unlink the Slack user")

	require.Zero(t, gw.resolveCount(), "/logout must be consumed, not dispatched to the agent")
}

func TestLogin_PostsSignInPrompt(t *testing.T) {
	fakeSlack, ephemeral := captureEphemeral(t)
	defer fakeSlack.Close()

	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)
	a.OBO = &fakeOBO{linkedUser: "U999", linkURL: "https://gw.example.com/auth/slack/link?u=xyz"}

	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> /login","channel":"C1","ts":"111.222"}}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Eventually(t, func() bool { return len(ephemeral()) > 0 },
		2*time.Second, 50*time.Millisecond, "/login must post a sign-in prompt")
	require.Zero(t, gw.resolveCount(), "/login must be consumed, not dispatched to the agent")
}

// --- stubGateway ---

type stubGateway struct {
	mu            sync.Mutex
	resolveCount_ int
	resumeCount_  int
	onResolve     func(channels.InboundMessage)
	// resolveErr, when set, is returned by every Resolve call.
	resolveErr error
	deltas     []channels.OutboundDelta
	// sendQueue, when non-empty, supplies a distinct delta set per SendCompletion
	// call (popped in order), so a test can drive a multi-step turn such as a
	// prompt followed by its auto-approved continuation. Falls back to deltas.
	sendQueue [][]channels.OutboundDelta
	// hold, when non-nil, keeps a turn in flight: SendCompletion streams deltas
	// then blocks until hold is closed, so a test can hold the per-thread slot.
	hold chan struct{}
	// interDeltaDelay, when set, pauses between streamed deltas so the
	// writer's batch ticker can flush mid-turn (e.g. content before an error).
	interDeltaDelay time.Duration
	// onSessionResumable, when set, backs SessionResumable; nil reports the check
	// as unavailable (checked=false).
	onSessionResumable func(channels.InboundMessage) (exists, checked bool)
	// onResetSession, when set, backs ResetSession; nil reports the reset as
	// unavailable (false, nil).
	onResetSession func(channels.InboundMessage) (bool, error)
	// failSends makes the next N SendCompletion calls return an error, so a test
	// can drive the resume-failure paths.
	failSends int
	// failSendsAfter delays failSends past this many leading successful sends, so
	// a test can fail an in-turn resume (e.g. an auto-approved continuation) that
	// follows an initial successful send within the same turn.
	failSendsAfter int
}

func (s *stubGateway) resumeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumeCount_
}

func (s *stubGateway) ResetSession(_ context.Context, msg channels.InboundMessage) (bool, error) {
	s.mu.Lock()
	cb := s.onResetSession
	s.mu.Unlock()
	if cb == nil {
		return false, nil
	}
	return cb(msg)
}

func (s *stubGateway) SessionResumable(_ context.Context, msg channels.InboundMessage) (bool, bool) {
	s.mu.Lock()
	s.resumeCount_++
	cb := s.onSessionResumable
	s.mu.Unlock()
	if cb == nil {
		return false, false
	}
	return cb(msg)
}

func (s *stubGateway) resolveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveCount_
}

func (s *stubGateway) Resolve(_ context.Context, msg channels.InboundMessage) (channels.InstanceRef, error) {
	s.mu.Lock()
	s.resolveCount_++
	cb := s.onResolve
	resolveErr := s.resolveErr
	s.mu.Unlock()
	if cb != nil {
		cb(msg)
	}
	if resolveErr != nil {
		return channels.InstanceRef{}, resolveErr
	}
	return channels.InstanceRef{Name: "test-instance"}, nil
}

func (s *stubGateway) SendCompletion(ctx context.Context, _ channels.InstanceRef, _ channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	s.mu.Lock()
	if s.failSendsAfter > 0 {
		s.failSendsAfter--
	} else if s.failSends > 0 {
		s.failSends--
		s.mu.Unlock()
		return nil, errors.New("stub: send completion failed")
	}
	var deltas []channels.OutboundDelta
	if len(s.sendQueue) > 0 {
		deltas = s.sendQueue[0]
		s.sendQueue = s.sendQueue[1:]
	} else {
		deltas = s.deltas
	}
	hold := s.hold
	s.mu.Unlock()
	if deltas == nil && hold == nil {
		deltas = []channels.OutboundDelta{{Done: true}}
	}
	ch := make(chan channels.OutboundDelta)
	go func() {
		defer close(ch)
		for i, d := range deltas {
			if i > 0 && s.interDeltaDelay > 0 {
				select {
				case <-time.After(s.interDeltaDelay):
				case <-ctx.Done():
					return
				}
			}
			select {
			case ch <- d:
			case <-ctx.Done():
				return
			}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}

func (s *stubGateway) FetchHistory(_ context.Context, _ channels.InstanceRef) ([]channels.Message, error) {
	return nil, nil
}

// Ensure stubGateway satisfies channels.Gateway at compile time.
var _ channels.Gateway = (*stubGateway)(nil)

// Ensure batchedWriter output is correctly structured.
func TestBatchedWriter_CombinesDeltas(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: "foo"},
			{Content: "bar"},
			{Done: true},
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A DM (channel_type "im"): top-level channel messages are intentionally
	// dropped now, so a DM is the right way to exercise the batched writer.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi","channel":"D1","ts":"111.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "foobar")
	}, 2*time.Second, 50*time.Millisecond, "expected foobar in the posted answer")
}

// --- Progress reactions & serialization (black-box via fake Slack Web API) ---

// recordedCall is one Slack Web API request captured by fakeSlackAPI, with its
// form or JSON body merged into params.
type recordedCall struct {
	path   string
	params map[string]any
}

// fakeSlackAPI is a structured fake Slack Web API: it records every request by
// method path with a decoded body and can inject an error code per path.
type fakeSlackAPI struct {
	mu          sync.Mutex
	calls       []recordedCall
	failWith    map[string]string // path (e.g. "reactions.add") -> slack error code
	respondWith map[string]string // path -> canned JSON response body
	seq         int
	botUserID   string // returned as user_id from auth.test
}

func newFakeSlackAPI() *fakeSlackAPI {
	return &fakeSlackAPI{failWith: map[string]string{}, respondWith: map[string]string{}, botUserID: "UBOT"}
}

func (f *fakeSlackAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		path := strings.TrimPrefix(r.URL.Path, "/")
		params := map[string]any{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.NewDecoder(r.Body).Decode(&params)
		} else {
			_ = r.ParseForm()
			for k := range r.Form {
				params[k] = r.Form.Get(k)
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, recordedCall{path: path, params: params})
		code := f.failWith[path]
		canned := f.respondWith[path]
		f.seq++
		ts := fmt.Sprintf("1700000000.%06d", f.seq)
		botID := f.botUserID
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if path == "auth.test" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"user_id":%q}`, botID)
			return
		}
		if code != "" {
			_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
			return
		}
		if canned != "" {
			_, _ = fmt.Fprint(w, canned)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":%q}`, ts)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeSlackAPI) setFail(path, code string) {
	f.mu.Lock()
	f.failWith[path] = code
	f.mu.Unlock()
}

func (f *fakeSlackAPI) setResponse(path, body string) {
	f.mu.Lock()
	f.respondWith[path] = body
	f.mu.Unlock()
}

func (f *fakeSlackAPI) pathCalls(path string) []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedCall
	for _, c := range f.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlackAPI) waitForPath(t *testing.T, path string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(f.pathCalls(path)) >= n
	}, 2*time.Second, 20*time.Millisecond, "expected >=%d call(s) to %s", n, path)
}

// allText concatenates the "text" param of the given calls.
func allText(calls []recordedCall) string {
	var b strings.Builder
	for _, c := range calls {
		if s, ok := c.params["text"].(string); ok {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// reactionNames returns the "name" param of each recorded reactions.* call.
func (f *fakeSlackAPI) reactionNames(path string) []string {
	var out []string
	for _, c := range f.pathCalls(path) {
		if s, ok := c.params["name"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sendEvent(t *testing.T, srv *httptest.Server, eventJSON string) {
	t.Helper()
	body := []byte(eventJSON)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
}

// dmEvent builds a DM message event (sender always permitted) for thread ts.
func dmEvent(user, text, ts string) string {
	return fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"im","user":%q,"text":%q,"channel":"D1","ts":%q}}`, user, text, ts)
}

// dmThreadEvent builds a DM reply into an existing thread.
func dmThreadEvent(user, text, ts, threadTS string) string {
	return fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"im","user":%q,"text":%q,"channel":"D1","ts":%q,"thread_ts":%q}}`, user, text, ts, threadTS)
}

func TestProgress_ReactionsLifecycle(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hi", "222.000"))

	// working reaction added, then swapped to done (working removed, done added).
	fake.waitForPath(t, "reactions.remove", 1)
	fake.waitForPath(t, "reactions.add", 2)

	added := fake.reactionNames("reactions.add")
	require.Contains(t, added, "eyes", "working reaction added")
	require.Contains(t, added, "white_check_mark", "done reaction added")
	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.remove"), "working reaction removed on completion")

	// The triggering message (ts 222.000) is the reaction target, not a reply.
	require.Equal(t, "222.000", fake.pathCalls("reactions.add")[0].params["timestamp"])
}

func TestProgress_ClearReactionOnDone(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ClearReactionOnDone = true

	sendEvent(t, srv, dmEvent("U1", "hi", "222.000"))

	// Working reaction added, then removed on completion with no done reaction.
	fake.waitForPath(t, "reactions.remove", 1)
	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.add"), "only the working reaction is added")
	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.remove"), "working reaction removed on completion")
	require.NotContains(t, fake.reactionNames("reactions.add"), "white_check_mark", "no done reaction added")
}

func TestProgress_FailedReactionOnError(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Err: errors.New("boom")}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hi", "333.000"))

	fake.waitForPath(t, "reactions.add", 2)
	require.Contains(t, fake.reactionNames("reactions.add"), "x", "failed reaction added on error delta")
}

func TestProgress_TextFallbackOnMissingScope(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("reactions.add", "missing_scope")
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "answer"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "first", "444.000"))
	// Missing scope -> text mode: a placeholder (chat.postMessage) then the answer
	// streamed via chat.update.
	fake.waitForPath(t, "chat.update", 1)
	require.Contains(t, allText(fake.pathCalls("chat.update")), "answer")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "_thinking", "text placeholder posted")

	// Second turn must not retry reactions.add (the downgrade is cached).
	sendEvent(t, srv, dmEvent("U1", "second", "444.000"))
	fake.waitForPath(t, "chat.update", 2)
	require.Len(t, fake.pathCalls("reactions.add"), 1, "reactions.add attempted once, then downgraded to text")
}

func TestProgress_TextModeConfigured(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "hello"}, {Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "hi", "555.000"))
	fake.waitForPath(t, "chat.update", 1) // placeholder (postMessage) then answer (update)
	require.Contains(t, allText(fake.pathCalls("chat.update")), "hello")
	require.Empty(t, fake.pathCalls("reactions.add"), "text mode never adds reactions")
}

func TestTextMode_EmptyOutputReplacesPlaceholder(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Done: true}}} // no content
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "hi", "777.000"))

	// Placeholder is posted, then replaced by a terminal note (not left as "thinking").
	fake.waitForPath(t, "chat.update", 1)
	require.Contains(t, allText(fake.pathCalls("chat.update")), "finished without a reply")
	require.Empty(t, fake.pathCalls("reactions.add"), "text mode adds no reactions")
}

func TestReactionsMode_EmptyOutputPostsNote(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Done: true}}} // no content
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)              // default: auto (reactions)

	sendEvent(t, srv, dmEvent("U1", "hi", "660.000"))

	// Reactions mode has no placeholder, so a zero-output turn must still post a
	// note rather than leaving only a done emoji.
	fake.waitForPath(t, "chat.postMessage", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "finished without a reply")
	require.Contains(t, fake.reactionNames("reactions.add"), "white_check_mark")
}

func TestTextMode_FailedTurnReplacesPlaceholder(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Err: errors.New("boom")}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "hi", "778.000"))

	// Placeholder is posted, then replaced by a failure note rather than left
	// dangling as "thinking"; text mode swaps no failed reaction.
	fake.waitForPath(t, "chat.update", 1)
	require.Contains(t, allText(fake.pathCalls("chat.update")), "the turn failed")
	require.Empty(t, fake.pathCalls("reactions.add"), "text mode adds no reactions")
}

// A turn that fails after part of the answer was already streamed must not
// overwrite the streamed content with the failure note; the note posts as a
// new message instead.
func TestTextMode_FailedTurnAfterContentPostsNewNote(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "partial answer"}, {Err: errors.New("boom")}},
		// Longer than the writer's batch interval so the content flushes into
		// the placeholder before the error arrives.
		interDeltaDelay: 600 * time.Millisecond,
	}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "hi", "779.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "the turn failed")
	}, 3*time.Second, 50*time.Millisecond, "the failure note posts as a new message")
	updates := allText(fake.pathCalls("chat.update"))
	require.Contains(t, updates, "partial answer", "the streamed content reached the placeholder")
	require.NotContains(t, updates, "the turn failed", "the note must not overwrite streamed content")
}

// An error arriving in the same batch window as the text (no tick in between)
// must still preserve the content: the writer flushes buffered text before
// surfacing the error, so the note posts as a new message rather than
// overwriting it.
func TestTextMode_FailedTurnFlushesBufferedContentBeforeNote(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "partial answer"}, {Err: errors.New("boom")}},
		// No interDeltaDelay: the error follows the text with no batch tick, so
		// the content is only surfaced by the flush on the error path.
	}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "hi", "780.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "the turn failed")
	}, 3*time.Second, 50*time.Millisecond, "the failure note posts as a new message")
	updates := allText(fake.pathCalls("chat.update"))
	require.Contains(t, updates, "partial answer", "buffered content is flushed before the error")
	require.NotContains(t, updates, "the turn failed", "the note must not overwrite streamed content")
}

// sendInteraction posts a signed block_actions interaction for actionID on
// threadID (channel D1, user U1) to the interactions endpoint.
func sendInteraction(t *testing.T, srv *httptest.Server, actionID, threadID string) {
	t.Helper()
	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": "U1"},
		"channel":   map[string]any{"id": "D1"},
		"container": map[string]any{"message_ts": "prompt.000"},
		"message":   map[string]any{"thread_ts": threadID},
		"actions":   []any{map[string]any{"action_id": actionID, "value": threadID}},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
}

func TestSerializeResumeWhileTurnInFlight(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	gw := &stubGateway{hold: hold}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A typed turn holds the thread.
	sendEvent(t, srv, dmEvent("U1", "first", "999.000"))
	fake.waitForPath(t, "reactions.add", 1)

	// A HITL button click for the same thread must be rejected, not resumed
	// concurrently, and must not consume the pending task or reach the agent.
	sendInteraction(t, srv, "hitl_approve", "999.000")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "still finishing")
	}, 2*time.Second, 20*time.Millisecond, "expected a busy notice for the concurrent button click")
	require.Equal(t, 1, gw.resolveCount(), "resume rejected before reaching the agent")

	close(hold)
}

func TestSerializeTurnsPerThread(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	gw := &stubGateway{hold: hold}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// Turn A holds the thread (blocks in SendCompletion until hold closes).
	sendEvent(t, srv, dmEvent("U1", "first", "666.000"))
	fake.waitForPath(t, "reactions.add", 1) // A acquired the thread and started

	// Turn B on the same thread while A is in flight -> rejected with a notice.
	sendEvent(t, srv, dmEvent("U1", "second", "666.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "still finishing")
	}, 2*time.Second, 20*time.Millisecond, "expected a busy notice for the second turn")

	require.Equal(t, 1, gw.resolveCount(), "second turn is rejected before reaching the agent")
	close(hold)
}

// A retried delivery whose original never reached the handler (pod restart,
// ingress failure) is the only delivery of that user message: it must be
// processed, not dropped, as long as its event_id is unseen.
func TestEventsHandler_RetryWithUnseenEventIDProcessed(t *testing.T) {
	var dispatched atomic.Int32
	gw := &stubGateway{
		onResolve: func(channels.InboundMessage) { dispatched.Add(1) },
	}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	_, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)

	body := []byte(`{
		"type":"event_callback",
		"event_id":"Ev-retry-only",
		"event":{"type":"app_mention","user":"U123","text":"<@BOT> hello","channel":"C456","ts":"1234.5678"}
	}`)
	stamp, sig := signBody(t, "signing-secret", body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	req.Header.Set("X-Slack-Retry-Num", "1")
	req.Header.Set("X-Slack-Retry-Reason", "http_error")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Eventually(t, func() bool { return dispatched.Load() == 1 },
		2*time.Second, 10*time.Millisecond,
		"a retry whose original delivery was lost must be processed")
}

// A second delivery of an already-seen event_id must be dropped so a duplicate
// delivery never starts a duplicate turn.
func TestEventsHandler_DuplicateEventIDDropped(t *testing.T) {
	var dispatched atomic.Int32
	gw := &stubGateway{
		onResolve: func(channels.InboundMessage) { dispatched.Add(1) },
	}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	_, srv := newEventsAdapter(t, gw, fakeSlack.URL, channelMode)

	body := []byte(`{
		"type":"event_callback",
		"event_id":"Ev-dup",
		"event":{"type":"app_mention","user":"U123","text":"<@BOT> hello","channel":"C456","ts":"1234.5678"}
	}`)

	deliver := func(retryNum string) *http.Response {
		stamp, sig := signBody(t, "signing-secret", body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Slack-Request-Timestamp", stamp)
		req.Header.Set("X-Slack-Signature", sig)
		if retryNum != "" {
			req.Header.Set("X-Slack-Retry-Num", retryNum)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	first := deliver("")
	_ = first.Body.Close()
	require.Equal(t, http.StatusOK, first.StatusCode)

	second := deliver("1")
	_ = second.Body.Close()
	require.Equal(t, http.StatusOK, second.StatusCode)

	// Dedup runs in the shared handleInbound pipeline (after the ack), so the
	// duplicate is dropped there rather than pre-ack: both deliveries return 200,
	// but only one turn is dispatched.
	require.Eventually(t, func() bool { return dispatched.Load() >= 1 },
		2*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, int32(1), dispatched.Load(), "duplicate delivery must not start a second turn")
}

// toolActivityDeltas is a turn that invokes a tool and then answers.
func toolActivityDeltas() []channels.OutboundDelta {
	return []channels.OutboundDelta{
		{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Name: "list_pods", Kind: channels.ToolCall,
			Args: map[string]any{"namespace": "kube-system"},
		}},
		{Content: "Found 3 pods."},
		{Done: true},
	}
}

func TestDetails_DefaultOn_RendersToolActivity(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: toolActivityDeltas()}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"list pods","channel":"D1","ts":"111.000"}}`)

	require.Eventually(t, func() bool {
		text := allText(fake.pathCalls("chat.postMessage"))
		return strings.Contains(text, "list_pods") && strings.Contains(text, "Found 3 pods.")
	}, 2*time.Second, 20*time.Millisecond, "default-on details should render the tool call and the answer")
}

func TestDetails_Off_SuppressesToolActivity(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: toolActivityDeltas()}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// Quiet the thread first (same thread_ts as the turn below).
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"/details off","channel":"D1","ts":"100.000","thread_ts":"100.000"}}`)
	fake.waitForPath(t, "chat.postMessage", 1)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"list pods","channel":"D1","ts":"101.000","thread_ts":"100.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Found 3 pods.")
	}, 2*time.Second, 20*time.Millisecond, "the answer should still be posted")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "list_pods",
		"details off must not render tool activity")
}

func TestResume_PostsStartingFreshWhenSessionGone(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return false, true }}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A reply into a thread this process never started (thread_ts != ts).
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi again","channel":"D1","ts":"201.000","thread_ts":"100.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "starting fresh")
	}, 2*time.Second, 20*time.Millisecond, "a gone session should trigger the starting-fresh notice")
	require.Equal(t, 1, gw.resumeCount())
}

func TestResume_SilentWhenSessionPresent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return true, true }}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi again","channel":"D1","ts":"201.000","thread_ts":"100.000"}}`)

	// Wait for the turn to complete (empty-output note), then assert no notice.
	fake.waitForPath(t, "chat.postMessage", 1)
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "starting fresh")
	require.Equal(t, 1, gw.resumeCount())
}

func TestResume_SkippedForRootMessage(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return false, true }}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A fresh root message (no thread_ts) starts a new session: no resume check.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"brand new","channel":"D1","ts":"300.000"}}`)

	fake.waitForPath(t, "chat.postMessage", 1)
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "starting fresh")
	require.Equal(t, 0, gw.resumeCount(), "root messages must not trigger the resume check")
}

// A top-level /usage in a DM keys a brand-new thread (its own ts, no
// thread_ts); the reply must still report the DM's usage instead of "not
// available yet".
func TestUsage_DMTopLevelReportsSession(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		{Content: "3 pods running."},
		{Usage: &channels.TurnUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
		{Done: true},
	}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A completed DM turn records usage under its thread root ("100.000").
	sendEvent(t, srv, dmEvent("U1", "count pods", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "3 pods running.")
	}, 2*time.Second, 20*time.Millisecond, "the turn must complete before /usage is sent")

	// /usage typed as a new top-level DM message: its own ts is the threadID.
	sendEvent(t, srv, dmEvent("U1", "/usage", "200.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Last turn — in 10 · out 5 · total 15")
	}, 2*time.Second, 20*time.Millisecond, "a top-level DM /usage must report the channel's usage")
}

// /usage mentioned in a channel thread no turn ever ran in replies with
// guidance to run it inside the agent's thread, not "not available yet".
func TestUsage_ChannelFreshThreadGetsGuidance(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@UBOT> /usage","channel":"C1","ts":"300.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "as a reply inside the agent's thread")
	}, 2*time.Second, 20*time.Millisecond)
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "not available yet")
}

// /usage as a reply inside the agent's thread keeps working: the thread-keyed
// lookup hits directly.
func TestUsage_InThreadStillWorks(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		{Content: "done."},
		{Usage: &channels.TurnUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}},
		{Done: true},
	}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@UBOT> count pods","channel":"C1","ts":"100.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "done.")
	}, 2*time.Second, 20*time.Millisecond, "the turn must complete before /usage is sent")

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","user":"U1","text":"/usage","channel":"C1","ts":"101.000","thread_ts":"100.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Last turn — in 7 · out 3 · total 10")
	}, 2*time.Second, 20*time.Millisecond)
}
