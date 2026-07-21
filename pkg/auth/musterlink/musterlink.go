// Package musterlink performs muster account-linking so the gateway can act on
// behalf of a human (OBO) instead of as a pure machine identity.
//
// muster is the OAuth 2.1 authorization server (the same one `muster auth
// login` uses; it federates to Dex/Entra internally). The gateway is a muster
// OAuth client. A Slack user authenticates with muster once via a browser
// OAuth code flow (PKCE). The gateway stores the resulting muster refresh
// token (encrypted at rest) keyed by Slack user ID, then silently obtains a
// fresh short-lived human token for each message via TokenFor and forwards it
// onto the A2A request as the human subject.
//
// The forwarded token is the upstream dex id_token, which muster returns in the
// token response alongside its own opaque access token. dex is the sole SSO
// authority and muster never signs tokens, so the id_token (a dex-issued OIDC
// JWT) is what a downstream validates the human identity against. The opaque
// access token is used only to read the linked identity's email from the
// userinfo endpoint.
//
// Discovery uses RFC 8414 OAuth Authorization Server Metadata
// (/.well-known/oauth-authorization-server).
//
// The HTTP surface is three handlers mounted on the gateway router:
//
//	GET /auth/slack/link?u=<signed state>   -> redirect to muster authorize (PKCE)
//	GET /auth/slack/callback                -> exchange code, verify, store link
//	GET /auth/slack/client.json             -> serve the CIMD document
//
// Client registration uses CIMD (Client ID Metadata Documents, the IETF
// client-id-metadata-document draft): the gateway's OAuth client_id is an HTTPS
// URL pointing at the doc it serves from /auth/slack/client.json. muster fetches
// and validates that doc (client_id must equal its own URL; redirect_uri must be
// listed) -- no Dynamic Client Registration, no registration token, no
// server-side allowlist.
//
// State is an HMAC-signed Slack user ID with a short expiry: it is both the
// CSRF token and the binding of the link to the requesting Slack user.
package musterlink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// HTTP paths for the linking handlers, mounted on the gateway's public router.
const (
	LinkPath     = "/auth/slack/link"
	CallbackPath = "/auth/slack/callback"
	// CIMDPath serves the Client ID Metadata Document. The gateway's OAuth
	// client_id is the absolute URL of this path (PublicBaseURL + CIMDPath);
	// muster fetches it during /authorize to validate the client.
	CIMDPath = "/auth/slack/client.json"
)

// defaultStateTTL bounds the whole interactive link flow (button click ->
// muster consent -> callback). Short enough to limit replay, long enough for a
// human to complete an OAuth consent screen.
const defaultStateTTL = 15 * time.Minute

// defaultHTTPTimeout bounds every outbound muster call (discovery, userinfo,
// and the oauth2 token endpoint) when no HTTPClient is supplied. Without it a
// blackholed endpoint blocks the caller forever; TokenFor holds the per-user
// refresh lock across the token call, so an unbounded hang there wedges the
// user until process restart.
const defaultHTTPTimeout = 30 * time.Second

// grantTypeRefreshToken is the OAuth grant type advertised in the CIMD document.
const grantTypeRefreshToken = "refresh_token"

// tokenRefreshSkew refreshes a cached token this long before it actually
// expires, so a token handed to a downstream A2A call is not about to expire
// mid-request.
const tokenRefreshSkew = 60 * time.Second

// ErrNotLinked is returned by TokenFor when no muster link exists for the Slack
// user. Callers treat it as a signal to prompt the user to sign in.
var ErrNotLinked = errors.New("musterlink: slack user is not linked to a muster identity")

// pageHTML is the branded shell served for the interactive (browser-facing) link
// outcomes: the success page and the handful of user-actionable errors. It is the
// gateway's adaptation of the platform gateway-api error page
// (giantswarm/shared-configs), reworked from Envoy %RESPONSE_*% substitutions
// into an html/template, with light/dark support and the Giant Swarm logo.
//
//go:embed page.html
var pageHTML string

// pageTmpl is parsed once at startup; a parse failure is a defect in the
// embedded asset and should fail fast rather than per request.
var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// page is the data rendered into pageHTML. Detail is optional: its box is omitted
// when empty.
type page struct {
	Title   string // browser tab title and bold subtitle line
	Heading string // large accent heading (e.g. "Signed in" or "Sign-in failed")
	Message string // explanatory paragraph
	Detail  string // optional monospace detail (technical hint); omitted when empty
}

// renderPage writes p as an HTML document with the given status. It renders into
// a buffer first so a template failure cannot emit a half-written body, falling
// back to a plain-text status line if rendering fails.
func (l *Linker) renderPage(w http.ResponseWriter, status int, p page) {
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, p); err != nil {
		l.logger.Error("musterlink: render page", "err", err)
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// Config configures a Linker.
type Config struct {
	// BaseURL is the muster authorization server base URL. RFC 8414 discovery
	// (/.well-known/oauth-authorization-server) is performed against it.
	BaseURL string
	// ClientID / ClientSecret identify the gateway's muster OAuth client.
	// ClientSecret may be empty for a public (PKCE-only) client.
	ClientID     string
	ClientSecret string
	// RedirectURL is the public, absolute callback URL registered with muster,
	// e.g. https://gateway.example.com/auth/slack/callback.
	RedirectURL string
	// PublicBaseURL is the gateway's public, externally reachable base URL
	// (e.g. https://gateway.example.com). LinkURL prepends it so the Slack
	// "Sign in" button points at an absolute URL. When empty, LinkURL returns a
	// relative path.
	PublicBaseURL string
	// Scopes requested from muster. When empty, defaults to
	// openid, profile, email, offline_access (offline_access is required to
	// receive a refresh token).
	Scopes []string
	// StateKey is the HMAC key used to sign link state. Must be non-empty.
	StateKey []byte
	// StateTTL bounds the link flow lifetime. Zero uses defaultStateTTL.
	StateTTL time.Duration
	// Store persists the resulting links. Required.
	Store Store
	// SlackEmail, when set, returns the Slack-workspace-verified email for a
	// Slack user. At callback the muster identity email must equal it, else the
	// link is rejected (anti-spoof). When nil the check is skipped.
	SlackEmail func(ctx context.Context, slackUserID string) (string, error)
	// OnLinked, when set, is invoked after a successful link so a caller can act
	// on it (e.g. replay a message parked while the user signed in, or replace
	// the sign-in prompt). It must not block: the callback HTTP response waits
	// on it. The context is detached from the request, so it stays valid after
	// the handler returns.
	OnLinked func(ctx context.Context, slackUserID, email string)
	// HTTPClient is used for discovery, userinfo, and oauth2 token-endpoint
	// calls (code exchange and refresh). Nil uses a client with a 30s timeout;
	// a supplied client should carry its own timeout, since TokenFor holds a
	// per-user lock across the refresh call.
	HTTPClient *http.Client
	// Logger defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// authServerMetadata is the subset of RFC 8414 metadata the gateway needs.
type authServerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// Linker drives the muster account-linking flow and mints human tokens.
type Linker struct {
	oauth         *oauth2.Config
	baseURL       string
	userinfoURL   string
	publicBaseURL string
	httpClient    *http.Client
	store         Store
	stateKey      []byte
	stateTTL      time.Duration
	slackEmail    func(ctx context.Context, slackUserID string) (string, error)
	onLinked      func(ctx context.Context, slackUserID, email string)
	logger        *slog.Logger
	now           func() time.Time

	// discoverMu guards lazy RFC 8414 discovery. Endpoints are resolved on
	// first use rather than at construction so the gateway can boot (and serve
	// non-OBO traffic plus the CIMD document) even when muster is briefly
	// unreachable. A failed attempt is not cached, so the next request retries.
	discoverMu sync.Mutex
	discovered bool

	mu      sync.Mutex
	pending map[string]pendingAuth // state -> PKCE verifier

	// refreshMu guards refreshLocks; each per-user lock serializes that user's
	// token refreshes so concurrent messages (e.g. Slack event retries) don't
	// both spend the rotating refresh token and invalidate the link.
	refreshMu    sync.Mutex
	refreshLocks map[string]*sync.Mutex
}

// pendingAuth holds the PKCE code verifier between the authorize redirect and
// the callback. It lives in memory only; a restart mid-flow forces the user to
// retry the link (the signed state itself is stateless and survives).
//
// ponytail: in-memory pending map -> a multi-replica gateway would lose the
// verifier if authorize and callback hit different replicas. Move to the Store
// (or a shared cache) if the gateway is scaled out.
type pendingAuth struct {
	verifier string
	expires  time.Time
}

// New builds a Linker. muster's OAuth endpoints are discovered lazily (RFC 8414)
// on first use, so construction performs no network I/O.
//
// ClientID and RedirectURL are optional: when empty they are derived from
// PublicBaseURL as the self-hosted CIMD document URL (the recommended default)
// and the callback URL respectively. PublicBaseURL is therefore required unless
// both are supplied explicitly.
func New(cfg Config) (*Linker, error) {
	if cfg.Store == nil {
		return nil, errors.New("musterlink: Store is required")
	}
	if len(cfg.StateKey) == 0 {
		return nil, errors.New("musterlink: StateKey is required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("musterlink: BaseURL is required")
	}
	publicBaseURL := strings.TrimRight(cfg.PublicBaseURL, "/")
	redirectURL := cfg.RedirectURL
	if redirectURL == "" {
		if publicBaseURL == "" {
			return nil, errors.New("musterlink: RedirectURL or PublicBaseURL is required")
		}
		redirectURL = publicBaseURL + CallbackPath
	}
	// CIMD: the OAuth client_id is the absolute URL of the document the gateway
	// serves at CIMDPath; muster fetches it and validates client_id == URL. An
	// explicit ClientID overrides this (e.g. a pre-registered client).
	clientID := cfg.ClientID
	if clientID == "" {
		if publicBaseURL == "" {
			return nil, errors.New("musterlink: ClientID or PublicBaseURL is required")
		}
		clientID = publicBaseURL + CIMDPath
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "offline_access"}
	}
	ttl := cfg.StateTTL
	if ttl <= 0 {
		ttl = defaultStateTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Linker{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  redirectURL,
			Scopes:       scopes,
		},
		baseURL:       cfg.BaseURL,
		publicBaseURL: publicBaseURL,
		httpClient:    httpClient,
		store:         cfg.Store,
		stateKey:      cfg.StateKey,
		stateTTL:      ttl,
		slackEmail:    cfg.SlackEmail,
		onLinked:      cfg.OnLinked,
		logger:        logger,
		now:           time.Now,
		pending:       map[string]pendingAuth{},
		refreshLocks:  map[string]*sync.Mutex{},
	}, nil
}

// oauthContext routes the oauth2 package's token-endpoint calls through the
// Linker's HTTP client (and its timeout) instead of http.DefaultClient.
func (l *Linker) oauthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, l.httpClient)
}

// ensureEndpoints lazily resolves and caches muster's OAuth endpoints via RFC
// 8414. It is safe for concurrent use; a failed attempt is not cached so the
// next caller retries.
func (l *Linker) ensureEndpoints(ctx context.Context) error {
	l.discoverMu.Lock()
	defer l.discoverMu.Unlock()
	if l.discovered {
		return nil
	}
	md, err := discover(ctx, l.httpClient, l.baseURL)
	if err != nil {
		return err
	}
	l.oauth.Endpoint = oauth2.Endpoint{
		AuthURL:   md.AuthorizationEndpoint,
		TokenURL:  md.TokenEndpoint,
		AuthStyle: oauth2.AuthStyleInParams,
	}
	l.userinfoURL = md.UserinfoEndpoint
	l.discovered = true
	return nil
}

// discover fetches RFC 8414 authorization-server metadata from baseURL.
func discover(ctx context.Context, client *http.Client, baseURL string) (authServerMetadata, error) {
	u := strings.TrimRight(baseURL, "/") + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return authServerMetadata{}, fmt.Errorf("musterlink: build discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return authServerMetadata{}, fmt.Errorf("musterlink: oauth discovery: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return authServerMetadata{}, fmt.Errorf("musterlink: oauth discovery: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return authServerMetadata{}, fmt.Errorf("musterlink: read discovery: %w", err)
	}
	var md authServerMetadata
	if err := json.Unmarshal(body, &md); err != nil {
		return authServerMetadata{}, fmt.Errorf("musterlink: parse discovery: %w", err)
	}
	if md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" {
		return authServerMetadata{}, errors.New("musterlink: discovery missing authorization_endpoint or token_endpoint")
	}
	return md, nil
}

// RegisterRoutes mounts the link, callback and CIMD-document handlers on mux.
func (l *Linker) RegisterRoutes(mux interface {
	Get(pattern string, h http.HandlerFunc)
}) {
	mux.Get(LinkPath, l.HandleLink)
	mux.Get(CallbackPath, l.HandleCallback)
	mux.Get(CIMDPath, l.HandleClientMetadata)
}

// clientMetadata is the Client ID Metadata Document the gateway serves at
// CIMDPath. It mirrors the IETF client-id-metadata-document draft (the subset
// muster validates). client_id is the absolute URL of this document, so a
// fetcher gets back a doc whose client_id equals the URL it fetched.
type clientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

// HandleClientMetadata serves the gateway's CIMD document. The served client_id
// equals this document's own URL (the gateway's configured OAuth client_id) and
// redirect_uris contains the callback URL -- the two invariants muster enforces
// when it fetches the doc during /authorize.
func (l *Linker) HandleClientMetadata(w http.ResponseWriter, _ *http.Request) {
	doc := clientMetadata{
		ClientID:                l.oauth.ClientID,
		ClientName:              "klaus-gateway",
		ClientURI:               "https://github.com/giantswarm/klaus-gateway",
		RedirectURIs:            []string{l.oauth.RedirectURL},
		GrantTypes:              []string{"authorization_code", grantTypeRefreshToken},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   strings.Join(l.oauth.Scopes, " "),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		l.logger.Error("musterlink: encode client metadata", "err", err)
	}
}

// SignState returns an HMAC-signed, expiring state token binding the link flow
// to slackUserID. The Slack UX embeds it in the "Sign in" button URL as the `u`
// query parameter.
func (l *Linker) SignState(slackUserID string) string {
	exp := l.now().Add(l.stateTTL).Unix()
	nonce := oauth2.GenerateVerifier() // reuse as a random, URL-safe nonce
	payload := fmt.Sprintf("%s|%d|%s", slackUserID, exp, nonce)
	mac := l.mac(payload)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(mac)
}

// LinkURL returns the URL a user follows to start linking. When PublicBaseURL
// was configured the URL is absolute (suitable for a Slack "Sign in" button);
// otherwise it is relative to the gateway root.
func (l *Linker) LinkURL(slackUserID string) string {
	return l.publicBaseURL + LinkPath + "?u=" + l.SignState(slackUserID)
}

// Unlink removes any stored link for the Slack user (e.g. /klaus logout).
func (l *Linker) Unlink(slackUserID string) { l.store.Delete(slackUserID) }

// HandleLink verifies the signed state, generates a PKCE verifier, and
// redirects the browser to the muster authorization endpoint.
func (l *Linker) HandleLink(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("u")
	if _, err := l.verifyState(state); err != nil {
		l.logger.Warn("musterlink: rejected link request", "err", err)
		l.renderPage(w, http.StatusBadRequest, page{
			Heading: "Link expired",
			Title:   "Sign-in link invalid",
			Message: "This sign-in link is invalid or has expired. Return to Slack and start the sign-in again.",
		})
		return
	}
	if err := l.ensureEndpoints(r.Context()); err != nil {
		l.logger.Error("musterlink: discovery failed", "err", err)
		l.renderPage(w, http.StatusBadGateway, page{
			Heading: "Unavailable",
			Title:   "Sign-in is temporarily unavailable",
			Message: "The sign-in service could not be reached. Please try again in a moment.",
		})
		return
	}
	verifier := oauth2.GenerateVerifier()
	l.putPending(state, verifier)
	url := l.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	// G710: the redirect target is muster's discovered authorize endpoint built
	// by oauth2, not user-controlled input.
	http.Redirect(w, r, url, http.StatusFound) //nolint:gosec
}

// HandleCallback completes the flow: it verifies state, exchanges the code
// (with the PKCE verifier), looks up the muster identity, enforces the
// Slack/muster email match, and stores the link.
func (l *Linker) HandleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		l.renderPage(w, http.StatusBadRequest, page{
			Heading: "Sign-in cancelled",
			Title:   "Sign-in was not completed",
			Message: "The sign-in was cancelled or denied. Return to Slack and try again.",
			Detail:  e,
		})
		return
	}
	state := q.Get("state")
	slackUser, err := l.verifyState(state)
	if err != nil {
		l.logger.Warn("musterlink: rejected callback state", "err", err)
		l.renderPage(w, http.StatusBadRequest, page{
			Heading: "Link expired",
			Title:   "Sign-in link invalid",
			Message: "This sign-in link is invalid or has expired. Return to Slack and start the sign-in again.",
		})
		return
	}
	verifier, ok := l.takePending(state)
	if !ok {
		l.renderPage(w, http.StatusBadRequest, page{
			Heading: "Session expired",
			Title:   "Sign-in expired",
			Message: "Your sign-in session expired before it could complete. Return to Slack and try again.",
		})
		return
	}

	ctx := r.Context()
	link, err := l.Exchange(ctx, q.Get("code"), verifier)
	if err != nil {
		l.logger.Error("musterlink: code exchange failed", "err", err)
		l.renderPage(w, http.StatusBadGateway, page{
			Heading: "Sign-in failed",
			Title:   "Sign-in could not be completed",
			Message: "Something went wrong while completing your sign-in. Please try again in a moment.",
		})
		return
	}

	if l.slackEmail != nil {
		want, err := l.slackEmail(ctx, slackUser)
		if err != nil {
			l.logger.Error("musterlink: slack email lookup failed", "err", err)
			l.renderPage(w, http.StatusBadGateway, page{
				Heading: "Sign-in failed",
				Title:   "Sign-in could not be completed",
				Message: "Something went wrong while completing your sign-in. Please try again in a moment.",
			})
			return
		}
		if want == "" || !strings.EqualFold(want, link.Email) {
			l.logger.Warn("musterlink: email mismatch", "slackUser", slackUser, "slackEmail", want, "identityEmail", link.Email)
			l.renderPage(w, http.StatusForbidden, page{
				Heading: "Email mismatch",
				Title:   "Account email does not match",
				Message: fmt.Sprintf("The account you signed in with reports %s, but your Slack profile email is %s. "+
					"Sign in with the account whose email matches your Slack email. "+
					"For GitHub-backed sign-in, the released email is your GitHub primary email; update it if it does not match.",
					link.Email, want),
			})
			return
		}
	}

	link.LinkedAt = l.now()
	l.store.Put(slackUser, link)
	l.logger.Info("musterlink: linked slack user to muster identity", "slackUser", slackUser, "email", link.Email)

	if l.onLinked != nil {
		l.onLinked(context.WithoutCancel(ctx), slackUser, link.Email)
	}

	l.renderPage(w, http.StatusOK, page{
		Heading: "Success",
		Title:   "Signed in to Giant Swarm",
		Message: "You can close this tab and return to Slack.",
	})
}

// Exchange trades an authorization code (with its PKCE verifier) for muster
// tokens and resolves the muster identity (sub, email) via the userinfo
// endpoint. The returned Link has no LinkedAt set; the caller stamps and stores
// it after any email-match check.
func (l *Linker) Exchange(ctx context.Context, code, codeVerifier string) (*Link, error) {
	if err := l.ensureEndpoints(ctx); err != nil {
		return nil, err
	}
	tok, err := l.oauth.Exchange(l.oauthContext(ctx), code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return nil, fmt.Errorf("musterlink: code exchange: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("musterlink: token response carried no refresh token (offline_access not granted?)")
	}
	// The dex id_token is the credential forwarded downstream; a link without
	// one can never serve a turn, so fail the sign-in here instead of storing a
	// link that errors on every message.
	idToken, _ := tok.Extra("id_token").(string)
	if idToken == "" {
		return nil, errors.New("musterlink: token response carried no id_token (openid scope missing or upstream IdP misconfigured?)")
	}
	sub, email, err := l.userinfo(ctx, tok.AccessToken)
	if err != nil {
		return nil, err
	}
	return &Link{
		Sub:          sub,
		Email:        email,
		RefreshToken: tok.RefreshToken,
		IDToken:      idToken,
		Expiry:       idTokenExpiry(idToken, tok.Expiry),
	}, nil
}

// userinfo fetches the muster identity claims using the access token.
func (l *Linker) userinfo(ctx context.Context, accessToken string) (sub, email string, err error) {
	if l.userinfoURL == "" {
		return "", "", errors.New("musterlink: muster metadata has no userinfo endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.userinfoURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("musterlink: build userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("musterlink: userinfo: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("musterlink: userinfo: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", "", fmt.Errorf("musterlink: read userinfo: %w", err)
	}
	var ui struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &ui); err != nil {
		return "", "", fmt.Errorf("musterlink: parse userinfo: %w", err)
	}
	if ui.Email == "" {
		return "", "", errors.New("musterlink: userinfo response had no email")
	}
	return ui.Sub, ui.Email, nil
}

// TokenFor returns a fresh short-lived human token (the dex id_token) for the
// Slack user, to be forwarded as the subject on A2A requests. It reuses a
// still-valid cached id_token when one is stored, and only when none is valid
// does it refresh: it spends the stored (rotating) muster refresh token, caches
// the new id_token with its expiry, rotates the stored refresh token, and
// persists both. It returns ErrNotLinked when no link exists and drops the link
// on a hard refresh failure (invalid/expired refresh token) so the next attempt
// prompts a clean re-link.
//
// Refreshes are serialized per user: a Slack turn can drive several TokenFor
// calls (event retries, concurrent messages), and muster invalidates the old
// refresh token on rotation, so two simultaneous refreshes would race and burn
// the link. The cached id_token means those extra calls return without
// refreshing at all.
func (l *Linker) TokenFor(ctx context.Context, slackUserID string) (string, error) {
	// Fast path: a still-valid cached id_token avoids a refresh (and the
	// per-user lock) entirely.
	if link, ok := l.store.Get(slackUserID); ok {
		if tok := validCachedToken(link, l.now()); tok != "" {
			return tok, nil
		}
	}

	unlock := l.lockUser(slackUserID)
	defer unlock()

	link, ok := l.store.Get(slackUserID)
	if !ok {
		return "", ErrNotLinked
	}
	// Re-check under the lock: a concurrent caller may have just refreshed.
	if tok := validCachedToken(link, l.now()); tok != "" {
		return tok, nil
	}
	if err := l.ensureEndpoints(ctx); err != nil {
		return "", fmt.Errorf("musterlink: discover endpoints: %w", err)
	}
	src := l.oauth.TokenSource(l.oauthContext(ctx), &oauth2.Token{RefreshToken: link.RefreshToken})
	tok, err := src.Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			// muster rejected the refresh token: the link is dead. Drop it and
			// report as unlinked so the caller prompts sign-in on this same turn
			// rather than a turn later. A non-invalid_grant token-endpoint error
			// (transient 5xx, network) keeps the link and stays retryable.
			l.store.Delete(slackUserID)
			return "", ErrNotLinked
		}
		return "", fmt.Errorf("musterlink: refresh token for slack user: %w", err)
	}
	// The refresh succeeded, so muster has already rotated the refresh token
	// server-side; persist it even when the response is unusable, or the stored
	// token is spent and the next refresh burns the link with invalid_grant.
	updated := *link
	if tok.RefreshToken != "" {
		updated.RefreshToken = tok.RefreshToken
	}
	idToken, _ := tok.Extra("id_token").(string)
	if idToken == "" {
		updated.IDToken = ""
		updated.Expiry = time.Time{}
		l.store.Put(slackUserID, &updated)
		return "", errors.New("musterlink: refresh response carried no id_token")
	}
	updated.IDToken = idToken
	updated.Expiry = idTokenExpiry(idToken, tok.Expiry)
	l.store.Put(slackUserID, &updated)
	return idToken, nil
}

// LinkedIdentity returns the muster identity (subject and email) stored for a
// linked Slack user. ok is false when the user has no link. Read-only: safe
// without the per-user refresh lock.
func (l *Linker) LinkedIdentity(slackUserID string) (sub, email string, ok bool) {
	link, ok := l.store.Get(slackUserID)
	if !ok {
		return "", "", false
	}
	return link.Sub, link.Email, true
}

// validCachedToken returns the link's cached id_token when it is present and
// not within tokenRefreshSkew of expiry, else "". A zero Expiry is treated as
// unknown and forces a refresh.
func validCachedToken(link *Link, now time.Time) string {
	if link.IDToken == "" || link.Expiry.IsZero() {
		return ""
	}
	if now.Add(tokenRefreshSkew).Before(link.Expiry) {
		return link.IDToken
	}
	return ""
}

// idTokenExpiry reads the exp claim from a JWT id_token without verifying its
// signature. It falls back to the supplied token expiry when the id_token is
// not a decodable JWT or carries no exp. The fallback is best-effort: it
// assumes the dex id_token lives at least as long as the muster access token,
// so an undecodable id_token is refreshed no later than the access token would
// be.
func idTokenExpiry(idToken string, fallback time.Time) time.Time {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return fallback
	}
	return time.Unix(claims.Exp, 0)
}

// lockUser returns the per-user refresh lock, held; the caller defers the
// returned unlock.
func (l *Linker) lockUser(slackUserID string) func() {
	l.refreshMu.Lock()
	mu, ok := l.refreshLocks[slackUserID]
	if !ok {
		mu = &sync.Mutex{}
		l.refreshLocks[slackUserID] = mu
	}
	l.refreshMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (l *Linker) mac(payload string) []byte {
	h := hmac.New(sha256.New, l.stateKey)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// verifyState validates the HMAC and expiry and returns the bound Slack user ID.
func (l *Linker) verifyState(state string) (string, error) {
	sep := strings.LastIndexByte(state, '.')
	if sep <= 0 {
		return "", errors.New("malformed state")
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(state[:sep])
	if err != nil {
		return "", errors.New("malformed state payload")
	}
	gotMAC, err := enc.DecodeString(state[sep+1:])
	if err != nil {
		return "", errors.New("malformed state mac")
	}
	if subtle.ConstantTimeCompare(gotMAC, l.mac(string(payload))) != 1 {
		return "", errors.New("state signature mismatch")
	}
	parts := strings.SplitN(string(payload), "|", 3)
	if len(parts) != 3 {
		return "", errors.New("malformed state fields")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", errors.New("malformed state expiry")
	}
	if l.now().Unix() > exp {
		return "", errors.New("state expired")
	}
	return parts[0], nil
}

func (l *Linker) putPending(state, verifier string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, p := range l.pending { // opportunistic prune
		if now.After(p.expires) {
			delete(l.pending, k)
		}
	}
	l.pending[state] = pendingAuth{verifier: verifier, expires: now.Add(l.stateTTL)}
}

func (l *Linker) takePending(state string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.pending[state]
	if !ok {
		return "", false
	}
	delete(l.pending, state)
	if l.now().After(p.expires) {
		return "", false
	}
	return p.verifier, true
}
