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

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: an RFC 8693 identifier, not a credential
	tokenTypeIDToken       = "urn:ietf:params:oauth:token-type:id_token"       //nolint:gosec // G101: an RFC 8693 identifier, not a credential
	tokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"   //nolint:gosec // G101: an RFC 8693 identifier, not a credential
	// exchangeResponseLimit bounds the token endpoint's answer.
	exchangeResponseLimit = 64 << 10
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

// Exchange returns the exchanged ID token for a subject token of issuer,
// through issuer's connector (Dex's connector_id) and with its subject token
// type.
func (e *Exchanger) Exchange(ctx context.Context, subjectToken string, issuer TrustedIssuer) (string, error) {
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
		return "", fmt.Errorf("token exchange request: %w", err)
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
	resp, err := client.Do(req) //nolint:gosec // G704: the token endpoint is operator configuration (a chart value)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, exchangeResponseLimit))
	if err != nil {
		return "", fmt.Errorf("token exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var oauthErr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return "", fmt.Errorf("%w: %s: %s", ErrExchangeRefused, oauthErr.Error, oauthErr.ErrorDescription)
		}
		return "", fmt.Errorf("token exchange: status %d: %s", resp.StatusCode, oauthErr.Error)
	}
	var issued struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		return "", fmt.Errorf("token exchange response: %w", err)
	}
	if issued.AccessToken == "" || issued.IssuedTokenType != tokenTypeIDToken {
		return "", fmt.Errorf("token exchange: no ID token issued (issued_token_type %q)", issued.IssuedTokenType)
	}
	return issued.AccessToken, nil
}

// TrustedIssuer is an identity provider whose tokens the channel accepts.
type TrustedIssuer struct {
	// Name names the issuer's security scheme in the Agent Card.
	Name string
	// Issuer is the token's iss, exactly.
	Issuer string
	// Keys are the issuer's signing keys.
	Keys oidc.KeySet
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
	OpenIDConnectURL string // AccessTokens marks an issuer whose tokens are access tokens: the
	// exchange names them as such. Otherwise they are ID tokens.
	AccessTokens bool
}

func (i TrustedIssuer) subjectTokenType() string {
	if i.AccessTokens {
		return tokenTypeAccessToken
	}
	return tokenTypeIDToken
}

// signingAlgorithms are the token signature algorithms accepted.
var signingAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// JWTVerifier verifies an A2A client's token against the trusted issuer its
// iss names, then checks the client that requested it.
type JWTVerifier struct {
	issuers map[string]issuerVerifier
	// exchangeClientID is the gateway's own exchange client. A token it
	// requested is a platform token, never an A2A client's token.
	exchangeClientID string
}

type issuerVerifier struct {
	issuer   TrustedIssuer
	verifier *oidc.IDTokenVerifier
}

// NewJWTVerifier returns a verifier of the tokens of issuers, refusing those
// exchangeClientID requested.
func NewJWTVerifier(issuers []TrustedIssuer, exchangeClientID string) *JWTVerifier {
	algorithms := make([]string, 0, len(signingAlgorithms))
	for _, alg := range signingAlgorithms {
		algorithms = append(algorithms, string(alg))
	}
	v := &JWTVerifier{issuers: make(map[string]issuerVerifier, len(issuers)), exchangeClientID: exchangeClientID}
	for _, issuer := range issuers {
		v.issuers[issuer.Issuer] = issuerVerifier{
			issuer: issuer,
			verifier: oidc.NewVerifier(issuer.Issuer, issuer.Keys, &oidc.Config{
				ClientID:             issuer.Audience,
				SupportedSigningAlgs: algorithms,
			}),
		}
	}
	return v
}

// Verify selects the trusted issuer by the token's unverified iss, checks the
// token's signature, issuer, audience and expiry with that issuer alone, then
// the client that requested it. It returns the issuer.
func (v *JWTVerifier) Verify(ctx context.Context, rawToken string) (TrustedIssuer, error) {
	parsed, err := jwt.ParseSigned(rawToken, signingAlgorithms)
	if err != nil {
		return TrustedIssuer{}, fmt.Errorf("malformed token: %w", err)
	}
	var unverified jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&unverified); err != nil {
		return TrustedIssuer{}, fmt.Errorf("malformed token claims: %w", err)
	}
	trusted, ok := v.issuers[unverified.Issuer]
	if !ok {
		return TrustedIssuer{}, fmt.Errorf("issuer %q is not trusted", unverified.Issuer)
	}
	token, err := trusted.verifier.Verify(ctx, rawToken)
	if err != nil {
		return TrustedIssuer{}, err
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
		ClientID        string `json:"client_id"`
	}
	if err := token.Claims(&claims); err != nil {
		return TrustedIssuer{}, fmt.Errorf("token claims: %w", err)
	}
	if err := v.checkClient(trusted.issuer, requestingClient(claims.AuthorizedParty, claims.ClientID, token.Audience)); err != nil {
		return TrustedIssuer{}, err
	}
	return trusted.issuer, nil
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

// requestingClient is the client a token was issued to: its azp (an ID
// token, OIDC Core section 2), else its client_id (an access token, RFC 9068
// section 2.2), else its only audience. It is empty when none names a single
// client.
func requestingClient(azp, clientID string, audience []string) string {
	if azp != "" {
		return azp
	}
	if clientID != "" {
		return clientID
	}
	if len(audience) == 1 {
		return audience[0]
	}
	return ""
}
