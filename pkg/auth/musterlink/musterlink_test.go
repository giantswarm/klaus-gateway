package musterlink

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// musterStub is a minimal muster-like OAuth 2.1 authorization server: RFC 8414
// discovery, a token endpoint that issues opaque access tokens and rotating
// refresh tokens, and a userinfo endpoint that returns the identity claims.
type musterStub struct {
	server   *httptest.Server
	clientID string

	mu             sync.Mutex
	email          string
	sub            string
	failRefresh    bool // reject refresh with 400 invalid_grant (dead token)
	failRefresh5xx bool // reject refresh with a transient 503 (no OAuth error code)
	omitIDToken    bool // omit id_token from the token response (upstream had none)
	counter        int
	validAccess    map[string]bool
}

func newMusterStub(t *testing.T, clientID, email, sub string) *musterStub {
	t.Helper()
	s := &musterStub{clientID: clientID, email: email, sub: sub, validAccess: map[string]bool{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 s.server.URL,
			"authorization_endpoint": s.server.URL + "/authorize",
			"token_endpoint":         s.server.URL + "/token",
			"userinfo_endpoint":      s.server.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Form.Get("grant_type") == "refresh_token" && s.failRefresh {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" && s.failRefresh5xx {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.counter++
		at := fmt.Sprintf("access-%d", s.counter)
		rt := fmt.Sprintf("refresh-%d", s.counter)
		s.validAccess[at] = true
		resp := map[string]any{
			"access_token":  at,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": rt,
		}
		if !s.omitIDToken {
			resp["id_token"] = makeIDToken(s.sub, time.Now().Add(time.Hour))
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		at := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 {
			at = h[7:]
		}
		s.mu.Lock()
		ok := s.validAccess[at]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"sub": s.sub, "email": s.email})
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

// makeIDToken builds a JWT-shaped dex id_token (header.payload.signature)
// carrying sub and exp, mimicking the token muster forwards. The gateway
// forwards it without verifying the signature, so a placeholder signature
// segment suffices.
func makeIDToken(sub string, exp time.Time) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test"})
	payload := enc(map[string]any{"sub": sub, "exp": exp.Unix(), "iss": "https://dex.test"})
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

// jwtSub decodes the sub claim from a JWT without verifying it, asserting the
// token is a well-formed 3-part JWT (what the kagent trusted-proxy edge needs).
func jwtSub(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "forwarded token must be a 3-part JWT, got %q", token)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims struct {
		Sub string `json:"sub"`
	}
	require.NoError(t, json.Unmarshal(payload, &claims))
	return claims.Sub
}

func newTestLinker(t *testing.T, stub *musterStub, store Store, slackEmail func(context.Context, string) (string, error)) *Linker {
	t.Helper()
	l, err := New(Config{
		BaseURL:      stub.server.URL,
		ClientID:     stub.clientID,
		ClientSecret: "secret",
		RedirectURL:  "https://gw.example.com" + CallbackPath,
		StateKey:     []byte("hmac-state-key"),
		Store:        store,
		SlackEmail:   slackEmail,
	})
	require.NoError(t, err)
	return l
}

func TestClientMetadataDocument(t *testing.T) {
	stub := newMusterStub(t, "ignored", "a@example.com", "muster-sub")
	// The gateway's CIMD client_id is the absolute URL of the served document.
	const publicBase = "https://gw.example.com"
	clientID := publicBase + CIMDPath
	l, err := New(Config{
		BaseURL:       stub.server.URL,
		ClientID:      clientID,
		RedirectURL:   publicBase + CallbackPath,
		PublicBaseURL: publicBase,
		StateKey:      []byte("hmac-state-key"),
		Store:         NewMemStore(),
	})
	require.NoError(t, err)

	// Serve the document the way muster fetches it: at the CIMD URL.
	srv := httptest.NewServer(http.HandlerFunc(l.HandleClientMetadata))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var doc clientMetadata
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))

	// Invariant 1: client_id equals the gateway's CIMD document URL (what muster
	// fetched). Invariant 2: redirect_uris contains the callback. These are the
	// two checks muster enforces against the fetched document.
	require.Equal(t, clientID, doc.ClientID)
	require.Contains(t, doc.RedirectURIs, publicBase+CallbackPath)
	require.Equal(t, "none", doc.TokenEndpointAuthMethod)
	require.Contains(t, doc.GrantTypes, "refresh_token")
	require.Equal(t, "openid profile email offline_access", doc.Scope)
}

func TestNewDerivesClientIDAndRedirectFromPublicBaseURL(t *testing.T) {
	stub := newMusterStub(t, "ignored", "a@example.com", "muster-sub")
	const publicBase = "https://gw.example.com/" // trailing slash must be trimmed
	l, err := New(Config{
		BaseURL:       stub.server.URL,
		PublicBaseURL: publicBase,
		StateKey:      []byte("hmac-state-key"),
		Store:         NewMemStore(),
	})
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(l.HandleClientMetadata))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var doc clientMetadata
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	require.Equal(t, "https://gw.example.com"+CIMDPath, doc.ClientID)
	require.Contains(t, doc.RedirectURIs, "https://gw.example.com"+CallbackPath)
}

func TestSignVerifyState(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	l := newTestLinker(t, stub, NewMemStore(), nil)

	state := l.SignState("U123")
	user, err := l.verifyState(state)
	require.NoError(t, err)
	require.Equal(t, "U123", user)

	// Tampered MAC is rejected.
	_, err = l.verifyState(state[:len(state)-2] + "xx")
	require.Error(t, err)

	// Expired state is rejected.
	l.now = func() time.Time { return time.Now().Add(2 * l.stateTTL) }
	_, err = l.verifyState(state)
	require.Error(t, err)
}

func TestTokenForRefreshAndRotate(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"})
	l := newTestLinker(t, stub, store, nil)

	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "muster-sub", jwtSub(t, tok), "forwards the dex id_token as subject")

	// The stored refresh token was rotated to the value muster returned.
	got, ok := store.Get("U1")
	require.True(t, ok)
	require.Equal(t, "refresh-1", got.RefreshToken)
}

func TestTokenForForwardsDexIDTokenNotAccessToken(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"})
	l := newTestLinker(t, stub, store, nil)

	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	// The kagent trusted-proxy edge base64-decodes the JWT payload and reads sub;
	// the opaque muster access token has neither and is rejected 401. Forward the
	// dex id_token, never the "access-N" opaque token.
	require.Equal(t, "muster-sub", jwtSub(t, tok))
	require.NotContains(t, tok, "access-", "must not forward the opaque access token")

	// The cache holds the id_token, not the access token.
	got, ok := store.Get("U1")
	require.True(t, ok)
	require.Equal(t, tok, got.IDToken)
}

func TestTokenForMissingIDTokenErrors(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	stub.omitIDToken = true
	store := NewMemStore()
	store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"})
	l := newTestLinker(t, stub, store, nil)

	// Without an id_token there is nothing forwardable: TokenFor must error rather
	// than fall back to the opaque access token.
	_, err := l.TokenFor(context.Background(), "U1")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotLinked)

	// The refresh itself succeeded, so muster rotated the refresh token; the
	// rotated value must be persisted or the next attempt spends a dead token
	// and burns the link.
	link, ok := store.Get("U1")
	require.True(t, ok, "link must survive a missing id_token")
	require.Equal(t, "refresh-1", link.RefreshToken)
	require.Empty(t, link.IDToken)

	// Once the upstream recovers, the same link refreshes successfully.
	stub.omitIDToken = false
	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "muster-sub", jwtSub(t, tok))
}

func TestTokenForReusesCachedToken(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"})
	l := newTestLinker(t, stub, store, nil)

	tok1, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "muster-sub", jwtSub(t, tok1))

	// A second call within the token's lifetime reuses the cached id_token and
	// must not spend the (rotating) refresh token again -- doing so would race
	// muster's rotation and burn the link.
	tok2, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, tok1, tok2)

	stub.mu.Lock()
	calls := stub.counter
	stub.mu.Unlock()
	require.Equal(t, 1, calls, "second TokenFor must reuse the cached token, not refresh")
}

func TestTokenForRefreshesExpiredCachedToken(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	// A cached token already past expiry must be discarded and refreshed.
	store.Put("U1", &Link{RefreshToken: "refresh-0", IDToken: "stale", Expiry: time.Now().Add(-time.Minute)})
	l := newTestLinker(t, stub, store, nil)

	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "muster-sub", jwtSub(t, tok))
	require.NotEqual(t, "stale", tok)
}

func TestTokenForNotLinked(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	l := newTestLinker(t, stub, NewMemStore(), nil)
	_, err := l.TokenFor(context.Background(), "nobody")
	require.ErrorIs(t, err, ErrNotLinked)
}

func TestTokenForInvalidGrantDropsLink(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	stub.failRefresh = true
	store := NewMemStore()
	store.Put("U1", &Link{RefreshToken: "dead"})
	l := newTestLinker(t, stub, store, nil)

	// invalid_grant is terminal: the link is dropped and the error surfaces as
	// ErrNotLinked so the caller prompts sign-in on this same turn.
	_, err := l.TokenFor(context.Background(), "U1")
	require.ErrorIs(t, err, ErrNotLinked)
	_, ok := store.Get("U1")
	require.False(t, ok, "a hard refresh failure must drop the stale link")
}

func TestTokenForTransientRefreshErrorKeepsLink(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	stub.failRefresh5xx = true
	store := NewMemStore()
	store.Put("U1", &Link{RefreshToken: "refresh-0"})
	l := newTestLinker(t, stub, store, nil)

	// A transient token-endpoint failure (5xx, no invalid_grant) is retryable:
	// the link must survive and the error must not masquerade as ErrNotLinked.
	_, err := l.TokenFor(context.Background(), "U1")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotLinked)
	got, ok := store.Get("U1")
	require.True(t, ok, "a transient refresh failure must retain the link")
	require.Equal(t, "refresh-0", got.RefreshToken)
}

func TestCallbackStoresLinkOnEmailMatch(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "alice@example.com", "muster-sub")
	store := NewMemStore()
	l := newTestLinker(t, stub, store, func(context.Context, string) (string, error) {
		return "alice@example.com", nil
	})

	driveCallback(t, l, "U1", http.StatusOK)
	got, ok := store.Get("U1")
	require.True(t, ok)
	require.Equal(t, "alice@example.com", got.Email)
	require.Equal(t, "muster-sub", got.Sub)
	require.NotEmpty(t, got.RefreshToken)
	require.False(t, got.LinkedAt.IsZero())
	require.Equal(t, "muster-sub", jwtSub(t, got.IDToken), "the code exchange seeds the cached dex id_token")
}

func TestCallbackRejectsEmailMismatch(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "alice@example.com", "muster-sub")
	store := NewMemStore()
	l := newTestLinker(t, stub, store, func(context.Context, string) (string, error) {
		return "bob@example.com", nil // Slack says bob, muster says alice
	})

	driveCallback(t, l, "U1", http.StatusForbidden)
	_, ok := store.Get("U1")
	require.False(t, ok, "a spoofed email must not create a link")
}

// driveCallback runs HandleLink (to seed PKCE pending state) then HandleCallback
// for slackUser, asserting the callback's HTTP status.
func driveCallback(t *testing.T, l *Linker, slackUser string, wantStatus int) {
	t.Helper()
	state := l.SignState(slackUser)

	lrec := httptest.NewRecorder()
	l.HandleLink(lrec, httptest.NewRequest(http.MethodGet, LinkPath+"?u="+url.QueryEscape(state), nil))
	require.Equal(t, http.StatusFound, lrec.Code)

	crec := httptest.NewRecorder()
	q := url.Values{"state": {state}, "code": {"auth-code"}}
	l.HandleCallback(crec, httptest.NewRequest(http.MethodGet, CallbackPath+"?"+q.Encode(), nil))
	require.Equal(t, wantStatus, crec.Code, "callback body: %s", crec.Body.String())
}
