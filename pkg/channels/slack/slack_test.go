package slack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

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
		{"<@U12345> hello", "hello"},
		{"<@U12345>hello", "hello"},
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
	secrets := slackadapter.Secrets{
		BotToken:      "dummy-bot-token",
		SigningSecret: "signing-secret",
	}
	a := &slackadapter.Adapter{
		Mode:    slackadapter.ModeEvents,
		Secrets: secrets,
		APIBase: fakeAPIBase,
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
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
	defer resp.Body.Close()
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
	require.Equal(t, "U123", got.UserID)
	require.Equal(t, "hello", got.Text)
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
	defer resp.Body.Close()
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
		require.NoError(t, r.ParseForm())
		mu.Lock()
		updates = append(updates, r.FormValue("text"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"ts":"1234.5678"}`)
	}))
	defer fakeSlack.Close()

	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: "hello"},
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
	defer resp.Body.Close()

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

// --- stubGateway ---

type stubGateway struct {
	mu           sync.Mutex
	resolveCount_ int
	onResolve    func(channels.InboundMessage)
	deltas       []channels.OutboundDelta
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
		require.NoError(t, r.ParseForm())
		text := r.FormValue("text")
		if strings.Contains(r.URL.Path, "chat.update") {
			mu.Lock()
			texts = append(texts, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"ts":"111.222"}`)
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

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U1","text":"hi","channel":"C1","ts":"111.000"}}`)
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

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
