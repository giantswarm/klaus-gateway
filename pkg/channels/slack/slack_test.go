package slack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, slackadapter.StripMention(tc.in))
		})
	}
}

// --- Events API handler ---

func newEventsAdapter(t *testing.T, gw channels.Gateway, fakeAPIBase string) (*slackadapter.Adapter, *httptest.Server) {
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

	_, srv := newEventsAdapter(t, gw, fakeSlack.URL)

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

func TestEventsHandler_LockedMode_NonOwnerDropped(t *testing.T) {
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
		// Default mode is locked; owner is first sender U001.
		AllowedUsers: []string{"U001"},
	}
	require.NoError(t, a.Start(ctx, gw))
	r := chi.NewRouter()
	a.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(cancel)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	// U999 is not U001 — should be silently dropped.
	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U999","text":"<@BOT> hi","channel":"C1","ts":"111.222"}}`
	body := []byte(payload)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	time.Sleep(150 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "non-owner must not trigger resolve")
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
	var mu sync.Mutex
	var updates []string

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		require.NoError(t, r.ParseForm())
		mu.Lock()
		updates = append(updates, r.FormValue("text"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: helloText},
			{Content: " world"},
			{Done: true},
		},
	}

	_, srv := newEventsAdapter(t, gw, fakeSlack.URL)

	payload := `{
		"type":"event_callback",
		"event":{
			"type":"app_mention",
			"user":"U123",
			"text":"<@BOT> go",
			"channel":"C1",
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

	// Wait for the batchedWriter to complete and flush.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(updates) >= 2 // postMessage + at least one chatUpdate
	}, 2*time.Second, 50*time.Millisecond, "expected chat.update calls")

	mu.Lock()
	lastUpdate := updates[len(updates)-1]
	mu.Unlock()
	require.Contains(t, lastUpdate, "hello world")
}

// --- OBO injection ---

// fakeOBO is a test OBOTokenSource: it returns token for the configured Slack
// user and musterlink.ErrNotLinked for anyone else.
type fakeOBO struct {
	linkedUser string
	token      string

	mu       sync.Mutex
	unlinked []string
	linkURL  string
}

func (f *fakeOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	if slackUserID == f.linkedUser {
		return f.token, nil
	}
	return "", musterlink.ErrNotLinked
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

func TestDispatch_OBO_UnlinkedUserFallsBackToM2M(t *testing.T) {
	got := dispatchAndCaptureOBO(t, &fakeOBO{linkedUser: "U999", token: "x"}, "U123")
	require.Empty(t, got.BearerToken, "unlinked user's turn must leave BearerToken empty (M2M fallback)")
}

func TestDispatch_OBO_DisabledLeavesBearerTokenEmpty(t *testing.T) {
	got := dispatchAndCaptureOBO(t, nil, "U123")
	require.Empty(t, got.BearerToken, "with OBO disabled the turn must run as M2M")
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

func TestKlausLogout_Unlinks(t *testing.T) {
	fakeSlack, _ := captureEphemeral(t)
	defer fakeSlack.Close()

	obo := &fakeOBO{linkedUser: "U123", token: "human-token"}
	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL)
	a.OBO = obo

	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> /klaus logout","channel":"C1","ts":"111.222"}}`
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
	}, 2*time.Second, 50*time.Millisecond, "/klaus logout must unlink the Slack user")

	require.Zero(t, gw.resolveCount(), "/klaus logout must be consumed, not dispatched to the agent")
}

func TestKlausLogin_PostsSignInPrompt(t *testing.T) {
	fakeSlack, ephemeral := captureEphemeral(t)
	defer fakeSlack.Close()

	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fakeSlack.URL)
	a.OBO = &fakeOBO{linkedUser: "U999", linkURL: "https://gw.example.com/auth/slack/link?u=xyz"}

	payload := `{"type":"event_callback","event":{"type":"app_mention","user":"U123","text":"<@BOT> /klaus login","channel":"C1","ts":"111.222"}}`
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
		2*time.Second, 50*time.Millisecond, "/klaus login must post a sign-in prompt")
	require.Zero(t, gw.resolveCount(), "/klaus login must be consumed, not dispatched to the agent")
}

// --- stubGateway ---

type stubGateway struct {
	mu            sync.Mutex
	resolveCount_ int
	onResolve     func(channels.InboundMessage)
	deltas        []channels.OutboundDelta
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
	s.mu.Unlock()
	if cb != nil {
		cb(msg)
	}
	return channels.InstanceRef{Name: "test-instance"}, nil
}

func (s *stubGateway) SendCompletion(_ context.Context, _ channels.InstanceRef, _ channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	s.mu.Lock()
	deltas := s.deltas
	s.mu.Unlock()
	if deltas == nil {
		deltas = []channels.OutboundDelta{{Done: true}}
	}
	ch := make(chan channels.OutboundDelta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func (s *stubGateway) FetchHistory(_ context.Context, _ channels.InstanceRef) ([]channels.Message, error) {
	return nil, nil
}

// Ensure stubGateway satisfies channels.Gateway at compile time.
var _ channels.Gateway = (*stubGateway)(nil)

// Ensure batchedWriter output is correctly structured.
func TestBatchedWriter_CombinesDeltas(t *testing.T) {
	var mu sync.Mutex
	var texts []string

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		require.NoError(t, r.ParseForm())
		text := r.FormValue("text")
		if strings.Contains(r.URL.Path, "chat.update") {
			mu.Lock()
			texts = append(texts, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"ts":"111.222"}`)
	}))
	defer fakeSlack.Close()

	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: "foo"},
			{Content: "bar"},
			{Done: true},
		},
	}
	_, srv := newEventsAdapter(t, gw, fakeSlack.URL)

	// Use a DM (channel_type "im"): top-level channel messages are intentionally
	// dropped now (no #random chatter), so a DM is the right way to exercise the
	// batched writer end to end.
	body := []byte(`{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"U1","text":"hi","channel":"D1","ts":"111.000"}}`)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, t := range texts {
			if strings.Contains(t, "foobar") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond, "expected foobar in a chat.update call")
}
