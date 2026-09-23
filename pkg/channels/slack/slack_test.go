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

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
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

// channelMode configures the adapter to serve channels and redirect DMs. Used
// by tests that drive the adapter through a channel app_mention. The harness
// otherwise defaults to DM-only (see newEventsAdapter), since most tests use a
// 1:1 DM as a single-permitted-user surface.
func channelMode(a *slackadapter.Adapter) {
	a.DMMode = slackadapter.DMModeRedirect
	a.ChannelMode = slackadapter.ChannelModeAll
}

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
		// Default to serving DMs only: most tests drive the adapter through a
		// 1:1 DM. Channel-driven tests opt into channelMode.
		DMMode:      slackadapter.DMModeServe,
		ChannelMode: slackadapter.ChannelModeNone,
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
		onDispatch: func(msg channels.InboundMessage) {
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
	// Registered before the adapter, so it closes after the adapter has stopped
	// (cleanups run last-registered-first): a winding-down turn never posts
	// into a closed server.
	t.Cleanup(fakeSlack.Close)

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
	}, flowWait, 50*time.Millisecond, "expected dispatch to fire")

	mu.Lock()
	got := capturedMessages[0]
	mu.Unlock()
	require.Equal(t, "slack", got.Channel)
	require.Equal(t, "C456", got.ChannelID)
	require.Equal(t, "U123", got.Subject, "Subject carries the raw Slack user ID for access control")
	require.Equal(t, helloText, got.Text)
	require.Equal(t, "test-agent", got.AgentRef, "AgentRef must be set to DefaultAgent")
}

func TestEventsHandler_RedeliveredEventDropped(t *testing.T) {
	var dispatched atomic.Int32
	gw := &stubGateway{
		onDispatch: func(channels.InboundMessage) { dispatched.Add(1) },
	}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	t.Cleanup(fakeSlack.Close) // closed after the adapter has stopped; cleanups run LIFO

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
	t.Cleanup(fakeSlack.Close) // closed after the adapter has stopped; cleanups run LIFO

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
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's mention is dispatched")

	// U999 tries to instruct in the same thread: gated, not dispatched.
	send(`{"type":"event_callback","event":{"type":"app_mention","user":"U999","text":"<@BOT> me too","channel":"C1","ts":"333.444","thread_ts":"111.222"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.dispatchCount(), "a newcomer must not reach the agent until approved")
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

	// Give the goroutine time to run (it must not dispatch).
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.dispatchCount())
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

	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "hello world")
	}, flowWait, 20*time.Millisecond, "the answer is streamed")
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

func (f *fakeOBO) Unlink(slackUserID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlinked = append(f.unlinked, slackUserID)
	return nil
}

// dispatchAndCaptureOBO posts an app_mention from slackUser and returns the
// InboundMessage seen by the gateway, with the adapter's OBO source set to obo.
func dispatchAndCaptureOBO(t *testing.T, obo slackadapter.OBOTokenSource, slackUser string) channels.InboundMessage {
	t.Helper()
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onDispatch: func(msg channels.InboundMessage) {
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
	t.Cleanup(fakeSlack.Close) // closed after the adapter has stopped; cleanups run LIFO

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
	}, flowWait, 50*time.Millisecond, "expected dispatch to fire")

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
	fake := newFakeSlackAPI()

	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
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
		return signInPrompted(fake)
	}, flowWait, 50*time.Millisecond, "unlinked user must be prompted to sign in with a real message")
	// In a channel the prompt is ephemeral to its user and carries the link;
	// the public thread notice anchors it (a thread-scoped ephemeral in a
	// thread that shows no message is never surfaced by Slack) and carries
	// neither the link nor a mention (klaus-gateway#185).
	prompt := fake.pathCalls("chat.postEphemeral")[0]
	require.Equal(t, "111.222", prompt.params["thread_ts"])
	require.Equal(t, "U123", prompt.params["user"])
	notice := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "111.222", notice.params["thread_ts"])
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "U123")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "http")
	require.Zero(t, gw.dispatchCount(), "unlinked turn must not reach the agent (no M2M fallback)")
}

// An unlinked user's message is parked while they sign in and replayed through
// dispatch once linked, so the question is answered without being re-typed.
func TestDispatch_OBO_ParksUnlinkedMessageAndReplaysAfterLink(t *testing.T) {
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onDispatch: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}
	fake := newFakeSlackAPI()
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
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
		return signInPrompted(fake)
	}, flowWait, 50*time.Millisecond, "unlinked user must be prompted to sign in")
	require.Zero(t, gw.dispatchCount(), "the message must be parked, not dispatched, before linking")

	// The user completes sign-in; the callback hook replays the parked message.
	obo.completeLink()
	a.OnUserLinked(context.Background(), "U123", "u123@example.com")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(captured) == 1
	}, flowWait, 50*time.Millisecond, "the parked message must replay after linking")
	mu.Lock()
	got := captured[0]
	mu.Unlock()
	require.Equal(t, "human-token", got.BearerToken, "the replayed turn carries the human muster token")
	require.Contains(t, got.Text, "what is failing?")

	// A channel prompt is ephemeral, so the completed link is confirmed with a
	// fresh ephemeral to the same user. The email is not echoed in-thread; it is
	// confirmed on the private browser success page. The agent is never named
	// here: the replay's own output is the handoff signal.
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "Signed in")
	}, flowWait, 50*time.Millisecond, "the link completion is confirmed to the user")
	var confirm recordedCall
	for _, call := range fake.pathCalls("chat.postEphemeral") {
		if text, _ := call.params["text"].(string); strings.Contains(text, "Signed in") {
			confirm = call
		}
	}
	text, _ := confirm.params["text"].(string)
	require.Equal(t, "U123", confirm.params["user"], "the confirmation reaches the linked user only")
	require.NotContains(t, text, "@", "the confirmation carries no email")
	require.NotContains(t, text, "test-agent", "the confirmation must not name the agent")
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
func (o *multiUserOBO) Unlink(string) error   { return nil }

// A newcomer who signs in mid-thread has their parked message replayed to the
// access-consent step, not dispatched to the agent: linking authenticates them,
// but the initiator must still approve before they can instruct the agent.
func TestDispatch_OBO_NewcomerReplaysToAccessPromptNotAgent(t *testing.T) {
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onDispatch: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}
	fake := newFakeSlackAPI()
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
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
	}, flowWait, 50*time.Millisecond, "the initiator's turn is dispatched")

	// A newcomer (unlinked) tries to instruct in the same thread: parked + prompted
	// to sign in, not dispatched.
	send(`{"type":"event_callback","event":{"type":"app_mention","user":"U2","text":"<@BOT> me too","channel":"C1","ts":"333.444","thread_ts":"111.222"}}`)
	require.Eventually(t, func() bool {
		return signInPrompted(fake)
	}, flowWait, 50*time.Millisecond, "the newcomer is prompted to sign in")
	mu.Lock()
	require.Equal(t, 1, len(captured), "an unlinked newcomer must not reach the agent")
	mu.Unlock()

	// The newcomer signs in; the replay lands at the access-consent prompt
	// (ephemeral to the initiator), still not dispatched to the agent.
	obo.link("U2", "tok2")
	a.OnUserLinked(context.Background(), "U2", "u2@example.com")
	fake.waitForPath(t, "chat.postEphemeral", 1)
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
	t.Cleanup(fakeSlack.Close) // closed after the adapter has stopped; cleanups run LIFO

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
	}, flowWait, 50*time.Millisecond, "a transient token failure must surface an error message")
	require.Zero(t, gw.dispatchCount(), "a transient token failure must not reach the agent as the SA")
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
	t.Cleanup(fakeSlack.Close) // closed after the adapter has stopped; cleanups run LIFO

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
	}, flowWait, 50*time.Millisecond, "/logout must unlink the Slack user")

	require.Zero(t, gw.dispatchCount(), "/logout must be consumed, not dispatched to the agent")
}

func TestLogin_PostsSignInPrompt(t *testing.T) {
	fake := newFakeSlackAPI()

	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
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

	require.Eventually(t, func() bool {
		return signInPrompted(fake)
	}, flowWait, 50*time.Millisecond, "/login must post a sign-in prompt")
	require.Zero(t, gw.dispatchCount(), "/login must be consumed, not dispatched to the agent")
}

// --- stubGateway ---

type stubGateway struct {
	mu             sync.Mutex
	dispatchCount_ int
	resumeCount_   int
	onDispatch     func(channels.InboundMessage)
	// dispatchErr, when set, is returned by every SendCompletion call.
	dispatchErr error
	deltas      []channels.OutboundDelta
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
	// sendCauses records, per SendCompletion whose context ended before the
	// deltas were consumed, the context cause it ended with (a shutdown names
	// channels.ErrShutdown; a /stop is a plain cancellation).
	sendCauses []error
	// delivered counts the deltas the adapter has actually taken off the
	// stream. A turn is dispatched before its first delta is read, so a test
	// that needs the content to be in the writer (a shutdown flush, say)
	// waits on this rather than on the dispatch.
	delivered int
	// resumes, when set, backs the restart-recovery capability (InFlightTurns,
	// InFlightTurn, ResumeTurn); nil reports no turns left running.
	resumes *stubResumes
	// records backs the thread-record capability. Two adapters sharing one
	// recorder simulate a restart with a surviving routing store.
	records *slackadapter.MemoryRecorder
	// recordsErr, when set, fails every thread-record read and write: a
	// routing store that is down.
	recordsErr error
}

// rec is the stub's thread recorder, created on first use.
func (s *stubGateway) rec() *slackadapter.MemoryRecorder {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = slackadapter.NewMemoryRecorder()
	}
	return s.records
}

func (s *stubGateway) ThreadRecord(ctx context.Context, ch, cid, tid string) (store.Entry, bool, error) {
	if s.recordsErr != nil {
		return store.Entry{}, false, s.recordsErr
	}
	return s.rec().ThreadRecord(ctx, ch, cid, tid)
}

func (s *stubGateway) UpdateThreadRecord(ctx context.Context, ch, cid, tid string, mutate func(e *store.Entry, found bool) bool) error {
	if s.recordsErr != nil {
		return s.recordsErr
	}
	return s.rec().UpdateThreadRecord(ctx, ch, cid, tid, mutate)
}

func (s *stubGateway) ThreadState(ctx context.Context, ch, cid, tid string) (channels.ThreadState, error) {
	if s.recordsErr != nil {
		return channels.ThreadState{}, s.recordsErr
	}
	return s.rec().ThreadState(ctx, ch, cid, tid)
}

// stubResumes is the stubGateway's record of turns a previous process left
// running: the turns InFlightTurns lists (and InFlightTurn finds by thread),
// the deltas ResumeTurn streams for each task, and what was resumed.
type stubResumes struct {
	turns        []channels.InFlightTurn
	deltas       map[string][]channels.OutboundDelta // task id -> deltas
	resumeErr    error
	durable      bool
	resumed      []channels.InboundMessage
	resumedTasks []string
}

func (s *stubGateway) ResumesTurns() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumes != nil && s.resumes.durable
}

func (s *stubGateway) InFlightTurns(_ context.Context, channel string) ([]channels.InFlightTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumes == nil {
		return nil, nil
	}
	var out []channels.InFlightTurn
	for _, t := range s.resumes.turns {
		if t.Msg.Channel == channel {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *stubGateway) InFlightTurn(_ context.Context, msg channels.InboundMessage) (channels.InFlightTurn, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumes == nil {
		return channels.InFlightTurn{}, false, nil
	}
	for _, t := range s.resumes.turns {
		if t.Msg.Channel == msg.Channel && t.Msg.ChannelID == msg.ChannelID && t.Msg.ThreadID == msg.ThreadID {
			return t, true, nil
		}
	}
	return channels.InFlightTurn{}, false, nil
}

func (s *stubGateway) ResumeTurn(ctx context.Context, msg channels.InboundMessage, taskID string) (<-chan channels.OutboundDelta, error) {
	s.mu.Lock()
	if s.resumes == nil {
		s.mu.Unlock()
		return nil, errors.New("stub: no resumes configured")
	}
	if err := s.resumes.resumeErr; err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.resumes.resumed = append(s.resumes.resumed, msg)
	s.resumes.resumedTasks = append(s.resumes.resumedTasks, taskID)
	deltas := s.resumes.deltas[taskID]
	// The record is consumed: a later lookup finds no turn left running.
	kept := s.resumes.turns[:0]
	for _, t := range s.resumes.turns {
		if t.TaskID != taskID {
			kept = append(kept, t)
		}
	}
	s.resumes.turns = kept
	s.mu.Unlock()
	ch := make(chan channels.OutboundDelta)
	go func() {
		defer close(ch)
		for _, d := range deltas {
			select {
			case ch <- d:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// deliveredDeltas returns how many deltas the adapter has taken off the
// streams of this gateway.
func (s *stubGateway) deliveredDeltas() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered
}

// sendCauseList returns the recorded context causes of the ended sends.
func (s *stubGateway) sendCauseList() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.sendCauses...)
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

func (s *stubGateway) dispatchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dispatchCount_
}

func (s *stubGateway) SendCompletion(ctx context.Context, msg channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	s.mu.Lock()
	s.dispatchCount_++
	cb := s.onDispatch
	dispatchErr := s.dispatchErr
	s.mu.Unlock()
	if cb != nil {
		cb(msg)
	}
	if dispatchErr != nil {
		return nil, dispatchErr
	}
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
	recordCause := func() {
		s.mu.Lock()
		s.sendCauses = append(s.sendCauses, context.Cause(ctx))
		s.mu.Unlock()
	}
	go func() {
		defer close(ch)
		for i, d := range deltas {
			if i > 0 && s.interDeltaDelay > 0 {
				select {
				case <-time.After(s.interDeltaDelay):
				case <-ctx.Done():
					recordCause()
					return
				}
			}
			select {
			case ch <- d:
				s.mu.Lock()
				s.delivered++
				s.mu.Unlock()
			case <-ctx.Done():
				recordCause()
				return
			}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				recordCause()
			}
		}
	}()
	return ch, nil
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
		return strings.Contains(fake.streamedText(), "foobar")
	}, flowWait, 50*time.Millisecond, "expected foobar in the streamed answer")
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
	// failIf, when set, is consulted per call with the parsed params; a
	// non-empty return fails that call with the given slack error code. For
	// conditional failures failWith cannot express (e.g. reject only branded
	// posts).
	failIf func(path string, params map[string]any) string
	// respondFn, when set for a path, builds that call's response from its
	// params. It serves what one canned body cannot: a paged
	// conversations.replies, or a users.info answering per user.
	respondFn map[string]func(params map[string]any) string
	// delayIf, when set, holds a call's answer back for as long as it returns,
	// so a test can watch a caller's budget run out: one user's users.info,
	// say, while every other lookup answers at once. The wait ends early when
	// the caller gives up.
	delayIf     func(path string, params map[string]any) time.Duration
	seq         int
	botUserID   string // returned as user_id from auth.test
	botUsername string // returned as user from auth.test
	botTeamID   string // returned as team_id from auth.test
	// streaming holds the ts of the messages chat.startStream opened and
	// chat.stopStream has not closed; an append or stop against any other ts is
	// refused the way Slack refuses it.
	streaming map[string]bool
	// stoppedByUser answers every append and stop with stopped_by_user, as
	// Slack does once the user has pressed the stop button.
	stoppedByUser bool
}

func newFakeSlackAPI() *fakeSlackAPI {
	return &fakeSlackAPI{
		failWith:    map[string]string{},
		respondWith: map[string]string{},
		respondFn:   map[string]func(map[string]any) string{},
		streaming:   map[string]bool{},
		botUserID:   "UBOT",
		botUsername: "swarmgeist",
		botTeamID:   "TWORKSPACE",
	}
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
		if code == "" && f.failIf != nil {
			code = f.failIf(path, params)
		}
		canned := f.respondWith[path]
		if fn := f.respondFn[path]; fn != nil && code == "" {
			canned = fn(params)
		}
		f.seq++
		ts := fmt.Sprintf("1700000000.%06d", f.seq)
		botID := f.botUserID
		botName := f.botUsername
		botTeam := f.botTeamID
		// Model the streaming state: a stream is open between start and stop,
		// and only an open one accepts an append or a stop.
		if code == "" {
			target, _ := params["ts"].(string)
			switch path {
			case pathStartStream:
				f.streaming[ts] = true
			case pathAppendStream, pathStopStream:
				switch {
				case f.stoppedByUser:
					code = "stopped_by_user"
				case !f.streaming[target]:
					code = "message_not_in_streaming_state"
				case path == pathStopStream:
					delete(f.streaming, target)
				}
			}
		}
		var delay time.Duration
		if f.delayIf != nil {
			delay = f.delayIf(path, params)
		}
		f.mu.Unlock()

		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if path == "auth.test" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"user_id":%q,"user":%q,"team_id":%q}`, botID, botName, botTeam)
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

// setDelayIf holds back only the calls fn picks, by path and parsed params.
func (f *fakeSlackAPI) setDelayIf(fn func(path string, params map[string]any) time.Duration) {
	f.mu.Lock()
	f.delayIf = fn
	f.mu.Unlock()
}

// setResponder makes the fake build path's response from each call's params.
func (f *fakeSlackAPI) setResponder(path string, fn func(params map[string]any) string) {
	f.mu.Lock()
	f.respondFn[path] = fn
	f.mu.Unlock()
}

// The streaming methods a turn's answer is rendered with.
const (
	pathStartStream  = "chat.startStream"
	pathAppendStream = "chat.appendStream"
	pathStopStream   = "chat.stopStream"
)

// chunkText concatenates the text of a streaming call's markdown_text chunks,
// which is where the agent's prose lands.
func chunkText(params map[string]any) string {
	var b strings.Builder
	for _, c := range chunksOf(params) {
		if c["type"] == "markdown_text" {
			s, _ := c["text"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

// chunksOf returns a streaming call's chunks as decoded objects.
func chunksOf(params map[string]any) []map[string]any {
	raw, _ := params["chunks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// streamedText concatenates the prose of every streaming call, which is where
// the agent's answer lands.
func (f *fakeSlackAPI) streamedText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, c := range f.calls {
		switch c.path {
		case pathStartStream, pathAppendStream, pathStopStream:
			b.WriteString(chunkText(c.params))
		}
	}
	return b.String()
}

// streamedSteps returns every task_update chunk of the streaming calls, in
// call order.
func (f *fakeSlackAPI) streamedSteps() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		switch c.path {
		case pathStartStream, pathAppendStream, pathStopStream:
			for _, chunk := range chunksOf(c.params) {
				if chunk["type"] == "task_update" {
					out = append(out, chunk)
				}
			}
		}
	}
	return out
}

// threadText is everything the adapter sent, in call order: the fallback text
// and blocks of its posts plus the text of its streamed answer, so assertions
// can find content wherever the renderer put it — and tell what landed first.
func (f *fakeSlackAPI) threadText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, c := range f.calls {
		// The streamed text is written without a separator: one answer arrives
		// in as many pieces as the stream sent, and an assertion looks for the
		// answer, not for the pieces.
		b.WriteString(chunkText(c.params))
		if s, ok := c.params["text"].(string); ok {
			b.WriteString(s)
			b.WriteString("\n")
		}
		if blocks, ok := c.params["blocks"]; ok {
			raw, _ := json.Marshal(blocks)
			b.Write(raw)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// streamMethods returns the streaming calls' methods, in order.
func (f *fakeSlackAPI) streamMethods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		switch c.path {
		case pathStartStream, pathAppendStream, pathStopStream:
			out = append(out, c.path)
		}
	}
	return out
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
	}, flowWait, 20*time.Millisecond, "expected >=%d call(s) to %s", n, path)
}

// signInPromptPrefix is the opening of the sign-in prompt, asserted on whichever
// surface carries it.
const signInPromptPrefix = "Sign in so I can act as you"

// signInPrompted reports whether the sign-in prompt reached its user: an
// ephemeral in a channel (klaus-gateway#185), a real threaded message in a DM.
func signInPrompted(fake *fakeSlackAPI) bool {
	return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), signInPromptPrefix) ||
		strings.Contains(allText(fake.pathCalls("chat.postMessage")), signInPromptPrefix)
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

// allBlockText concatenates the fallback text and the raw blocks JSON of the
// given calls, so assertions can find content wherever a renderer put it
// (fallback text, markdown block, or context element).
func allBlockText(calls []recordedCall) string {
	var b strings.Builder
	for _, c := range calls {
		if s, ok := c.params["text"].(string); ok {
			b.WriteString(s)
			b.WriteString("\n")
		}
		if blocks, ok := c.params["blocks"]; ok {
			raw, _ := json.Marshal(blocks)
			b.Write(raw)
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

// dmThreadFileEvent builds a DM thread reply carrying one file attachment.
// Slack sets subtype file_share on every human message with an upload, so the
// fixture carries it too.
func dmThreadFileEvent(user, text, ts, threadTS, filename string) string {
	return fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","subtype":"file_share","channel_type":"im","user":%q,"text":%q,"channel":"D1","ts":%q,"thread_ts":%q,"files":[{"name":%q,"mimetype":"image/png","url_private":"https://files.slack.com/f.png","size":10}]}}`, user, text, ts, threadTS, filename)
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

// A turn that fails before any answer text gets the failed reaction AND the
// retry note in the thread: the emoji alone does not tell the user what to do,
// and on a conversation the gateway opened itself it sits on the bot's own
// root message where nobody looks for it.
func TestProgress_FailedReactionOnError(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Err: errors.New("boom")}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hi", "333.000"))

	fake.waitForPath(t, "reactions.add", 2)
	require.Contains(t, fake.reactionNames("reactions.add"), "x", "failed reaction added on error delta")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "the turn failed")
	}, flowWait, 20*time.Millisecond, "the retry note is posted in the thread")
	for _, c := range fake.pathCalls("chat.postMessage") {
		if strings.Contains(fmt.Sprint(c.params["text"]), "the turn failed") {
			require.Equal(t, "333.000", c.params["thread_ts"], "the note lands in the turn's thread")
		}
	}
}

// A failure the gateway can name gets the note of its class instead of the
// generic retry invitation, whether the stream failed or the send was refused
// before it started; the generic note is left for what no class names.
func TestProgress_FailureNoteNamesTheClass(t *testing.T) {
	const toolSet = `failed to extract tools from the tool set "mcp_tool_set": failed to list MCP tools: failed to init MCP session: calling "initialize": read: connection reset by peer`
	for _, tc := range []struct {
		name      string
		gw        *stubGateway
		want      string
		wantNotIn string
	}{
		{"tools", &stubGateway{deltas: []channels.OutboundDelta{{Err: errors.New(toolSet)}}}, "I couldn't connect to my tools", "the turn failed"},
		{"model", &stubGateway{deltas: []channels.OutboundDelta{{Err: errors.New(`anthropic API error: 529 {"type":"overloaded_error"}`)}}}, "The model behind this agent returned an error", "the turn failed"},
		{"policy", &stubGateway{deltas: []channels.OutboundDelta{{Err: errors.New("OpenAI chat completion request failed: 403 authorization failed")}}}, "A platform policy refused this request", "the turn failed"},
		{"platform, before the stream", &stubGateway{dispatchErr: errors.New("rpc error: code = Unavailable desc = connection refused")}, "I couldn't reach the agent platform", "the turn failed"},
		{"unknown", &stubGateway{deltas: []channels.OutboundDelta{{Err: errors.New("boom")}}}, "the turn failed; please try again", "⚠️"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeSlackAPI()
			_, srv := newEventsAdapter(t, tc.gw, fake.server(t).URL)

			sendEvent(t, srv, dmEvent("U1", "hi", "335.000"))

			require.Eventually(t, func() bool {
				return strings.Contains(allText(fake.pathCalls("chat.postMessage")), tc.want)
			}, flowWait, 20*time.Millisecond, "the class's note is posted in the thread")
			require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), tc.wantNotIn)
		})
	}
}

// Once answer text has streamed, a failure keeps today's behaviour in
// reactions mode: the failed emoji marks the incomplete reply and no generic
// note is added under it.
func TestProgress_FailureAfterContentPostsNoNote(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "partial answer"}, {Err: errors.New("boom")}},
		// Well past the writer's batch interval, so the content is flushed
		// before the error arrives even on a slow runner under the race detector.
		interDeltaDelay: time.Second,
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hi", "444.000"))

	fake.waitForPath(t, "reactions.add", 2)
	require.Equal(t, []string{"eyes", "x"}, fake.reactionNames("reactions.add"))
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "partial answer")
	}, flowWait, 20*time.Millisecond, "the streamed content reached the thread")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "the turn failed", "no generic note under streamed content")
}

func TestProgress_TextFallbackOnMissingScope(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("reactions.add", "missing_scope")
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "answer"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "first", "444.000"))
	// Missing scope -> text mode: a placeholder (chat.postMessage), then the
	// answer in a stream of its own, which retires the placeholder.
	fake.waitForPath(t, pathStopStream, 1)
	require.Contains(t, fake.streamedText(), "answer")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "_thinking", "text placeholder posted")
	require.NotEmpty(t, fake.pathCalls("chat.delete"), "the placeholder is retired by the streamed answer")

	// Second turn must not retry reactions.add (the downgrade is cached).
	sendEvent(t, srv, dmEvent("U1", "second", "445.000"))
	fake.waitForPath(t, pathStopStream, 2)
	require.Len(t, fake.pathCalls("reactions.add"), 1, "reactions.add attempted once, then downgraded to text")
}

func TestProgress_TextModeConfigured(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "hello"}, {Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "hi", "555.000"))
	fake.waitForPath(t, pathStopStream, 1) // placeholder (postMessage), then the streamed answer
	require.Contains(t, fake.streamedText(), "hello")
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
	}, flowWait, 50*time.Millisecond, "the failure note posts as a new message")
	require.Contains(t, fake.streamedText(), "partial answer", "the streamed content reached the thread")
	require.NotContains(t, allText(fake.pathCalls("chat.update")), "the turn failed",
		"the note must not overwrite streamed content")
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
	}, flowWait, 50*time.Millisecond, "the failure note posts as a new message")
	require.Contains(t, fake.streamedText(), "partial answer", "buffered content is flushed before the error")
	require.NotContains(t, allText(fake.pathCalls("chat.update")), "the turn failed",
		"the note must not overwrite streamed content")
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
	}, flowWait, 20*time.Millisecond, "expected a busy notice for the concurrent button click")
	require.Equal(t, 1, gw.dispatchCount(), "resume rejected before reaching the agent")

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
	// A distinct ts: a real second message is never a redelivery of the first.
	sendEvent(t, srv, dmThreadEvent("U1", "second", "667.000", "666.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "still finishing")
	}, flowWait, 20*time.Millisecond, "expected a busy notice for the second turn")

	require.Equal(t, 1, gw.dispatchCount(), "second turn is rejected before reaching the agent")
	close(hold)
}

// A turn that dies before its stream starts (the agent lookup or the send fails) must
// post the failure note: streamResponse only covers errors after the stream is
// running, so a kagent outage was previously complete silence.
func TestDispatch_PreStreamFailurePostsNote(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{dispatchErr: errors.New("kagent unreachable")}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hi", "100.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "the turn failed")
	}, flowWait, 20*time.Millisecond, "a pre-stream dispatch failure must post the failure note")
}

// A slash command the gateway does not own ("/invite", a typo) must not fall
// through to the agent (a full turn spent explaining Slack commands); it gets
// a short notice instead. A prompt merely starting with a path still
// dispatches.
func TestHandleInbound_UnknownSlashCommandIntercepted(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "/invite <@U2>", "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "not one of my commands")
	}, flowWait, 20*time.Millisecond, "an unknown command replies with a notice")
	require.Zero(t, gw.dispatchCount(), "an unknown slash command must not reach the agent")

	sendEvent(t, srv, mention("U1", "/etc/hosts on node X is broken", "101.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 20*time.Millisecond, "a path-shaped prompt still dispatches")
}

// A plain (non-mention) reply in a thread the bot has no trace of stays fully
// silent: no dispatch, no hint (a served channel's unrelated threads must not
// be pinged).
func TestHandleInbound_UnrelatedThreadReplyStaysSilent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"channel","user":"U1","text":"lunch anyone?","channel":"C1","ts":"200.000","thread_ts":"100.000"}}`)
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, gw.dispatchCount())
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "no hint for a thread with no bot trace")
	require.Empty(t, fake.pathCalls("chat.postMessage"))
}

// A thread_broadcast reply ("also send to #channel") into an active bot
// thread is a deliberate user message and must reach the agent like any other
// reply; other subtypes stay rejected.
func TestHandleInbound_ThreadBroadcastReplyDispatches(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "answer-text"}, {Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "start", "400.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "answer-text")
	}, flowWait, 50*time.Millisecond, "the mention's turn completes")

	broadcast := `{"type":"event_callback","event":{"type":"message","subtype":"thread_broadcast","user":"U1","text":"and then?","channel":"C1","ts":"401.000","thread_ts":"400.000"}}`
	waitThreadIdle(t, a, "400.000")
	sendEvent(t, srv, broadcast)
	require.Eventually(t, func() bool { return gw.dispatchCount() >= 2 },
		flowWait, 50*time.Millisecond,
		"a broadcast thread reply must reach the agent like any other reply")

	// Other subtypes must stay rejected: an edit in the same active thread
	// never starts a turn.
	waitThreadIdle(t, a, "400.000")
	before := gw.dispatchCount()
	edited := `{"type":"event_callback","event":{"type":"message","subtype":"message_changed","user":"U1","text":"edited","channel":"C1","ts":"402.000","thread_ts":"400.000"}}`
	sendEvent(t, srv, edited)
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, before, gw.dispatchCount(), "a message_changed subtype must not start a turn")
}

// A file_share reply (an upload without a bot mention) into an active bot
// thread is a deliberate user message and must reach the agent like any other
// reply. Slack sets subtype file_share on every human message carrying a file,
// and such a reply has no app_mention twin to route it.
func TestHandleInbound_FileShareReplyDispatches(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "answer-text"}, {Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "start", "500.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "answer-text")
	}, flowWait, 50*time.Millisecond, "the mention's turn completes")

	// The file has no url_private, so dispatch drops it by name (posting the
	// dropped-attachments notice) without touching the network; the caption
	// still runs the turn. Routing, not downloading, is under test here.
	reply := `{"type":"event_callback","event":{"type":"message","subtype":"file_share","user":"U1","text":"look at this","channel":"C1","ts":"501.000","thread_ts":"500.000","files":[{"name":"shot.png","mimetype":"image/png","size":10}]}}`
	waitThreadIdle(t, a, "500.000")
	sendEvent(t, srv, reply)
	require.Eventually(t, func() bool { return gw.dispatchCount() >= 2 },
		flowWait, 50*time.Millisecond,
		"a file_share thread reply must reach the agent like any other reply")

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "shot.png")
	}, flowWait, 50*time.Millisecond,
		"the attachment metadata travelled into dispatch (named in the dropped-attachments notice)")
}

// /stop before any streamed content in text-progress mode must resolve the
// "thinking" placeholder instead of leaving it dangling above "Stopped.".
func TestStop_TextModePlaceholderResolved(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	gw := &stubGateway{hold: hold}
	defer close(hold)
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "long task", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "_thinking")
	}, flowWait, 20*time.Millisecond, "text placeholder posted")

	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "101.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.update")), "(stopped)")
	}, flowWait, 20*time.Millisecond, "the placeholder is replaced on stop")
}

// A turn pausing on an approval prompt before any streamed content in
// text-progress mode must resolve the placeholder, which otherwise sits as
// "thinking" above the prompt.
func TestPrompt_TextModePlaceholderResolved(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{
		{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "delete_pod"}},
	}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "do it", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.update")), "(waiting for your input")
	}, flowWait, 20*time.Millisecond, "the placeholder is replaced when the turn pauses")
}

// A retried delivery whose original never reached the handler (pod restart,
// ingress failure) is the only delivery of that user message: it must be
// processed, not dropped, as long as its event_id is unseen.
func TestEventsHandler_RetryWithUnseenEventIDProcessed(t *testing.T) {
	var dispatched atomic.Int32
	gw := &stubGateway{
		onDispatch: func(channels.InboundMessage) { dispatched.Add(1) },
	}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	t.Cleanup(fakeSlack.Close) // closed after the adapter has stopped; cleanups run LIFO

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
		flowWait, 10*time.Millisecond,
		"a retry whose original delivery was lost must be processed")
}

// A second delivery of an already-seen event_id must be dropped so a duplicate
// delivery never starts a duplicate turn.
func TestEventsHandler_DuplicateEventIDDropped(t *testing.T) {
	var dispatched atomic.Int32
	gw := &stubGateway{
		onDispatch: func(channels.InboundMessage) { dispatched.Add(1) },
	}

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	t.Cleanup(fakeSlack.Close) // closed after the adapter has stopped; cleanups run LIFO

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
		flowWait, 10*time.Millisecond)
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

func TestToolActivity_RendersSteps(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: toolActivityDeltas()}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"list pods","channel":"D1","ts":"111.000"}}`)

	require.Eventually(t, func() bool {
		steps := fake.streamedSteps()
		return len(steps) == 2 && steps[0]["title"] == "List pods" &&
			steps[0]["status"] == "in_progress" && steps[1]["status"] == "complete" &&
			strings.Contains(fake.threadText(), "Found 3 pods.")
	}, flowWait, 20*time.Millisecond, "the tool step and the answer should render")
}

func TestResume_PostsStartingFreshWhenSessionGone(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return false, true }}
	// The thread is already bound to an agent: this reply continues a
	// conversation, it does not open one.
	require.NoError(t, gw.rec().UpdateThreadRecord(context.Background(), "slack", "D1", "100.000", func(e *store.Entry, _ bool) bool {
		e.AgentRef = "test-agent"
		return true
	}))
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	// A reply into a thread this process never started (thread_ts != ts).
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi again","channel":"D1","ts":"201.000","thread_ts":"100.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "starting fresh")
	}, flowWait, 20*time.Millisecond, "a gone session should trigger the starting-fresh notice")
	require.Equal(t, 1, gw.resumeCount())
}

func TestResume_SilentWhenSessionPresent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{onSessionResumable: func(channels.InboundMessage) (bool, bool) { return true, true }}
	require.NoError(t, gw.rec().UpdateThreadRecord(context.Background(), "slack", "D1", "100.000", func(e *store.Entry, _ bool) bool {
		e.AgentRef = "test-agent"
		return true
	}))
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
		return strings.Contains(fake.streamedText(), "3 pods running.")
	}, flowWait, 20*time.Millisecond, "the turn must complete before /usage is sent")

	// /usage typed as a new top-level DM message: its own ts is the threadID.
	sendEvent(t, srv, dmEvent("U1", "/usage", "200.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Last turn — in 10 · out 5 · total 15")
	}, flowWait, 20*time.Millisecond, "a top-level DM /usage must report the channel's usage")
}

// /usage mentioned in a channel thread no turn ever ran in replies with
// guidance to run it inside the agent's thread, not "not available yet".
func TestUsage_ChannelFreshThreadGetsGuidance(t *testing.T) {
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@UBOT> /usage","channel":"C1","ts":"300.000"}}`)

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "as a reply inside the agent's thread")
	}, flowWait, 20*time.Millisecond)
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
		return strings.Contains(fake.streamedText(), "done.")
	}, flowWait, 20*time.Millisecond, "the turn must complete before /usage is sent")

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","user":"U1","text":"/usage","channel":"C1","ts":"101.000","thread_ts":"100.000"}}`)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Last turn — in 7 · out 3 · total 10")
	}, flowWait, 20*time.Millisecond)
}

// Slack refusing the reply's rendering does not fail a turn the agent
// completed: the thread gets the failed reaction and a note that the reply is
// incomplete, not the generic failure note, and the dispatch succeeds — so the
// completed task is not cancelled server-side (klaus-gateway#242).
func TestTurn_RenderFailureAfterCompletionIsNotAFailedTurn(t *testing.T) {
	fake := newFakeSlackAPI()
	// Every rendering of the reply is refused; other posts (the note) land.
	fake.setFail(pathStartStream, "msg_too_long")
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Kind: channels.DeltaText, Content: "first half of the answer"},
			{Kind: channels.DeltaText, Content: ", second half of the answer"},
			{Done: true},
		},
		interDeltaDelay: 400 * time.Millisecond,
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "how many nodes?", "555.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Slack refused the rest of the reply: msg_too_long")
	}, flowWait, 20*time.Millisecond, "the thread is told the reply is incomplete")

	require.Equal(t, []string{"eyes", "x"}, fake.reactionNames("reactions.add"), "the failed reaction marks the incomplete reply")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "turn failed", "a completed turn is not reported as failed")
}

// errTaskGone is the controller's answer for a task it no longer has, as the
// a2a package surfaces it.
var errTaskGone = a2apkg.ErrTaskNotFound
