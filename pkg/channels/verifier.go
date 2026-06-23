package channels

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

// TokenVerifier validates an inbound bearer token and returns the verified
// subject. Implementations validate signature, issuer, audience and expiry.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (string, error)
}

// OIDCVerifierConfig configures an OIDCTokenVerifier.
type OIDCVerifierConfig struct {
	// Issuer is the OIDC issuer URL whose discovery document and JWKS validate
	// inbound tokens.
	Issuer string
	// Audience is the expected `aud` claim. Tokens minted for a different
	// audience are rejected.
	Audience string
}

// OIDCTokenVerifier validates bearer tokens against an OIDC issuer's JWKS.
// Discovery and JWKS retrieval happen on the first Verify call and are cached;
// the underlying key set refreshes itself as signing keys rotate.
type OIDCTokenVerifier struct {
	cfg OIDCVerifierConfig

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

// NewOIDCTokenVerifier returns a verifier for cfg. It performs no network I/O;
// discovery is deferred to the first Verify call so issuer unavailability at
// startup does not block the gateway.
func NewOIDCTokenVerifier(cfg OIDCVerifierConfig) *OIDCTokenVerifier {
	return &OIDCTokenVerifier{cfg: cfg}
}

// Verify validates token's signature, issuer, audience and expiry, returning
// the `sub` claim on success.
func (v *OIDCTokenVerifier) Verify(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", errors.New("empty bearer token")
	}
	idVerifier, err := v.idVerifier(ctx)
	if err != nil {
		return "", err
	}
	idToken, err := idVerifier.Verify(ctx, token)
	if err != nil {
		return "", err
	}
	return idToken.Subject, nil
}

// idVerifier resolves the OIDC discovery document on first use and caches the
// resulting verifier. Discovery failures are not cached, so a transient issuer
// outage is retried on the next call. The discovery context is detached from
// cancellation so the cached key set keeps refreshing after the first request
// completes.
func (v *OIDCTokenVerifier) idVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	cached := v.verifier
	v.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	// Discover outside the lock so a slow issuer does not stall concurrent
	// verifies; a rare duplicate discovery under a first-call race is harmless
	// (the result is idempotent and only the first store wins).
	provider, err := oidc.NewProvider(context.WithoutCancel(ctx), v.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %q: %w", v.cfg.Issuer, err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: v.cfg.Audience})
	v.mu.Lock()
	if v.verifier == nil {
		v.verifier = verifier
	}
	cached = v.verifier
	v.mu.Unlock()
	return cached, nil
}
