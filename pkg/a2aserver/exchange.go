package a2aserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	mcpoidc "github.com/giantswarm/mcp-oauth/providers/oidc"
)

const (
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: an RFC 8693 identifier, not a credential
	tokenTypeIDToken       = "urn:ietf:params:oauth:token-type:id_token"       //nolint:gosec // G101: an RFC 8693 identifier, not a credential
	tokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"   //nolint:gosec // G101: an RFC 8693 identifier, not a credential
	// exchangeResponseLimit bounds the token endpoint's answer.
	exchangeResponseLimit = 64 << 10
	exchangeTimeout       = 10 * time.Second
)

// Exchanger exchanges an A2A client's token at the identity provider's token
// endpoint (RFC 8693) as the gateway's own confidential client.
type Exchanger struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	// Audiences are the clients the exchanged token is issued for, each
	// requested as Dex's cross-client scope.
	Audiences  []string
	HTTPClient *http.Client
}

// ExchangedToken is the token an exchange issued and when it expires.
type ExchangedToken struct {
	Token string
	// Expiry is the earlier of the response's expires_in (RFC 8693 section
	// 2.2.1) and the token's exp; zero when the response states neither.
	Expiry time.Time
}

// Exchange returns the exchanged ID token for a subject token of issuer,
// through issuer's connector (Dex's connector_id) and with its subject token
// type.
func (e *Exchanger) Exchange(ctx context.Context, subjectToken string, issuer TrustedIssuer) (ExchangedToken, error) {
	endpoint, err := url.Parse(e.TokenURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return ExchangedToken{}, fmt.Errorf("token exchange endpoint must be an absolute HTTPS URL without userinfo or fragment")
	}
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	scopes := []string{ScopeOpenID, "email", "profile", "groups"}
	for _, audience := range e.Audiences {
		scopes = append(scopes, "audience:server:client_id:"+audience)
	}
	form := url.Values{
		"grant_type":           {grantTypeTokenExchange},
		"subject_token":        {subjectToken},
		"subject_token_type":   {issuer.subjectTokenType()},
		"requested_token_type": {tokenTypeIDToken},
		"scope":                {strings.Join(scopes, " ")},
	}
	if issuer.ConnectorID != "" {
		form.Set("connector_id", issuer.ConnectorID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.TokenURL, strings.NewReader(form.Encode())) //nolint:gosec // G704: the token endpoint is operator configuration (a chart value)
	if err != nil {
		return ExchangedToken{}, fmt.Errorf("token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 section 2.3.1: client credentials are form-encoded before
	// they are put in the Basic header.
	req.SetBasicAuth(url.QueryEscape(e.ClientID), url.QueryEscape(e.ClientSecret))

	client := e.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req) //nolint:gosec // G704: the token endpoint is operator configuration (a chart value)
	if err != nil {
		return ExchangedToken{}, fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, exchangeResponseLimit))
	if err != nil {
		return ExchangedToken{}, fmt.Errorf("token exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var oauthErr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return ExchangedToken{}, fmt.Errorf("%w: %s: %s", ErrExchangeRefused, oauthErr.Error, oauthErr.ErrorDescription)
		}
		return ExchangedToken{}, fmt.Errorf("token exchange: status %d: %s", resp.StatusCode, oauthErr.Error)
	}
	var issued struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
		TokenType       string `json:"token_type"`
		ExpiresIn       int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		return ExchangedToken{}, fmt.Errorf("token exchange response: %w", err)
	}
	if issued.AccessToken == "" || issued.IssuedTokenType != tokenTypeIDToken {
		return ExchangedToken{}, fmt.Errorf("token exchange: no ID token issued (issued_token_type %q)", issued.IssuedTokenType)
	}
	if !strings.EqualFold(issued.TokenType, "Bearer") {
		return ExchangedToken{}, fmt.Errorf("token exchange: unsupported token_type %q", issued.TokenType)
	}
	return ExchangedToken{Token: issued.AccessToken, Expiry: issuedExpiry(time.Now(), issued.ExpiresIn, issued.AccessToken)}, nil
}

// issuedExpiry is the earlier of now + expiresIn and the token's exp, read
// without a signature check: it is only a deadline. Zero when neither is
// known.
func issuedExpiry(now time.Time, expiresIn int64, token string) time.Time {
	var expiry time.Time
	if expiresIn > 0 {
		expiry = now.Add(time.Duration(expiresIn) * time.Second)
	}
	if claims, err := mcpoidc.ParseUnverifiedClaims(token); err == nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			if fromClaim := time.Unix(int64(exp), 0); expiry.IsZero() || fromClaim.Before(expiry) {
				expiry = fromClaim
			}
		}
	}
	return expiry
}

// TrustedIssuer is an identity provider whose tokens the channel accepts.
type TrustedIssuer struct {
	// Name names the issuer's security scheme in the Agent Card.
	Name string
	// Issuer is the token's iss, exactly.
	Issuer string
	// JWKSURL is the issuer's key set.
	JWKSURL string
	// JWKS fetches and caches the key set, refusing private addresses the
	// issuer's entry does not allow (mcp-oauth's SSRF guard).
	JWKS *mcpoidc.JWKSClient
	// Audience is the client a token must be issued for.
	Audience string
	// AllowedClients, when not empty, are the only clients whose tokens are
	// accepted.
	AllowedClients []string
	// ConnectorID names the connector of the exchange's identity provider
	// that verifies this issuer's tokens.
	ConnectorID string
	// TokenURL, when set, makes the issuer's card scheme OAuth 2.0 client
	// credentials at this token endpoint.
	TokenURL string
	// MetadataURL is the RFC 8414 metadata the OAuth 2.0 scheme names, if
	// any.
	MetadataURL string
	// OpenIDConnectURL is the discovery document the OpenID Connect scheme
	// names when TokenURL is empty.
	OpenIDConnectURL string
	// AccessTokens marks an issuer whose tokens are access tokens: the
	// exchange names them as such. Otherwise they are ID tokens.
	AccessTokens bool
}

func (i TrustedIssuer) subjectTokenType() string {
	if i.AccessTokens {
		return tokenTypeAccessToken
	}
	return tokenTypeIDToken
}

// JWTVerifier verifies an A2A client's token against the trusted issuer its
// iss names, then checks the client that requested it.
type JWTVerifier struct {
	issuers map[string]TrustedIssuer
	// exchangeClientID is the gateway's own exchange client. A token it
	// requested is a platform token, never an A2A client's token.
	exchangeClientID string
}

// NewJWTVerifier returns a verifier of the tokens of issuers, refusing those
// exchangeClientID requested.
func NewJWTVerifier(issuers []TrustedIssuer, exchangeClientID string) *JWTVerifier {
	v := &JWTVerifier{issuers: make(map[string]TrustedIssuer, len(issuers)), exchangeClientID: exchangeClientID}
	for _, issuer := range issuers {
		v.issuers[issuer.Issuer] = issuer
	}
	return v
}

// Verify selects the trusted issuer by the token's unverified iss and checks
// the token with that issuer alone (mcp-oauth's ValidateIDToken: signature
// with a pinned asymmetric algorithm, expiry, not-before, issuer and
// audience), then the client that requested it.
func (v *JWTVerifier) Verify(ctx context.Context, rawToken string) (VerifiedToken, error) {
	unverified, err := mcpoidc.ParseUnverifiedClaims(rawToken)
	if err != nil {
		return VerifiedToken{}, fmt.Errorf("malformed token: %w", err)
	}
	iss, _ := unverified["iss"].(string)
	issuer, ok := v.issuers[iss]
	if !ok {
		return VerifiedToken{}, fmt.Errorf("issuer %q is not trusted", iss)
	}
	claims, err := mcpoidc.ValidateIDToken(ctx, rawToken, issuer.JWKS, issuer.JWKSURL, issuer.Issuer, []string{issuer.Audience})
	if err != nil {
		return VerifiedToken{}, err
	}
	if !slices.Contains([]string(claims.Audience), issuer.Audience) {
		return VerifiedToken{}, fmt.Errorf("token audience does not match %q exactly", issuer.Audience)
	}
	client, err := requestingClient(issuer, claims.AuthorizedParty, claims.ClientID, []string(claims.Audience))
	if err != nil {
		return VerifiedToken{}, err
	}
	if err := v.checkClient(issuer, client); err != nil {
		return VerifiedToken{}, err
	}
	verified := VerifiedToken{Issuer: issuer}
	if claims.Expiry != nil {
		verified.Expiry = claims.Expiry.Time()
	}
	return verified, nil
}

func (v *JWTVerifier) checkClient(issuer TrustedIssuer, client string) error {
	if v.exchangeClientID != "" && client == v.exchangeClientID {
		return fmt.Errorf("token requested by the gateway's exchange client %q", client)
	}
	if len(issuer.AllowedClients) > 0 && !slices.Contains(issuer.AllowedClients, client) {
		return fmt.Errorf("token of issuer %q requested by client %q, which is not allowed", issuer.Name, client)
	}
	return nil
}

// requestingClient uses client_id for access tokens and azp for ID tokens.
// Provider-specific access tokens may identify the client with azp alone.
func requestingClient(issuer TrustedIssuer, azp, clientID string, audience []string) (string, error) {
	if issuer.AccessTokens {
		if clientID != "" && azp != "" && clientID != azp {
			return "", fmt.Errorf("access token has conflicting client_id and azp claims")
		}
		if clientID != "" {
			return clientID, nil
		}
		if azp == "" {
			return "", fmt.Errorf("access token does not identify its requesting client")
		}
		return azp, nil
	}
	if azp != "" {
		return azp, nil
	}
	if len(audience) == 1 {
		return audience[0], nil
	}
	return "", fmt.Errorf("ID token with multiple audiences must identify its authorized party")
}
