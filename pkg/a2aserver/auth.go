package a2aserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// VerifiedToken is an A2A client's token the verifier accepted.
type VerifiedToken struct {
	Issuer TrustedIssuer
	// Expiry is the token's exp.
	Expiry time.Time
}

// TokenVerifier verifies an A2A client's bearer token (signature, issuer,
// audience, expiry and requesting client).
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (VerifiedToken, error)
}

// TokenExchanger exchanges a verified A2A client token of issuer for the
// token the gateway acts with (RFC 8693).
type TokenExchanger interface {
	Exchange(ctx context.Context, subjectToken string, issuer TrustedIssuer) (ExchangedToken, error)
}

// Authenticator admits a request whose bearer token the Verifier accepts and
// runs it with the exchanged token as the caller's forwarded token.
type Authenticator struct {
	Verifier  TokenVerifier
	Exchanger TokenExchanger
	// MaxBodyBytes, when positive, caps the request body. The body is read
	// only after the token is verified, and the exchange runs only for a
	// body within the cap.
	MaxBodyBytes int64
	// MinTokenLifetime is the least lifetime the exchanged token must have
	// left when a request runs with it: the agent runtime keeps it for the
	// whole turn and cannot renew it. Zero checks only that it has not
	// expired.
	MinTokenLifetime time.Duration
	// Realm is the realm of the WWW-Authenticate challenge.
	Realm  string
	Logger *slog.Logger

	exchanged exchangeCache
}

// ErrExchangeRefused is an exchange the identity provider refused.
var ErrExchangeRefused = errors.New("token exchange refused")

// ErrTokenTooShortLived is an exchanged token with less than
// Authenticator.MinTokenLifetime left: the identity provider issues tokens
// shorter than a turn may run.
var ErrTokenTooShortLived = errors.New("exchanged token is too short-lived for a turn")

// Middleware authenticates every request it wraps.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			a.challenge(w, "", "")
			return
		}
		verified, err := a.Verifier.Verify(r.Context(), raw)
		if err != nil {
			a.logger().InfoContext(r.Context(), "a2a server: bearer token refused", "error", err)
			a.challenge(w, "invalid_token", "the access token is invalid or expired")
			return
		}
		exchange := a.exchange(raw, verified, next)
		if a.MaxBodyBytes > 0 {
			exchange = limitBody(a.MaxBodyBytes, exchange)
		}
		exchange.ServeHTTP(w, r)
	})
}

// exchange runs next with the exchanged token of raw, reusing the one an
// earlier request with raw got while it has MinTokenLifetime left.
func (a *Authenticator) exchange(raw string, verified VerifiedToken, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := sha256.Sum256([]byte(raw))
		token, ok := a.exchanged.get(key, time.Now(), a.MinTokenLifetime)
		if !ok {
			issued, err := a.Exchanger.Exchange(r.Context(), raw, verified.Issuer)
			if err == nil {
				err = a.checkLifetime(issued, time.Now())
			}
			if err != nil {
				a.refuseExchange(w, r, verified.Issuer, err)
				return
			}
			a.exchanged.put(key, issued, verified.Expiry, time.Now())
			token = issued.Token
		}
		next.ServeHTTP(w, r.WithContext(pkga2a.WithForwardedToken(r.Context(), token)))
	})
}

func (a *Authenticator) checkLifetime(issued ExchangedToken, now time.Time) error {
	switch {
	case issued.Expiry.IsZero():
		return fmt.Errorf("token exchange: the issued token states no expiry")
	case issued.Expiry.Sub(now) < max(a.MinTokenLifetime, 0) || !issued.Expiry.After(now):
		return fmt.Errorf("%w: %s left, %s needed", ErrTokenTooShortLived, issued.Expiry.Sub(now).Round(time.Second), a.MinTokenLifetime)
	}
	return nil
}

func (a *Authenticator) refuseExchange(w http.ResponseWriter, r *http.Request, issuer TrustedIssuer, err error) {
	switch {
	case errors.Is(err, ErrExchangeRefused):
		a.logger().WarnContext(r.Context(), "a2a server: token exchange failed", "issuer", issuer.Name, "error", err)
		a.challenge(w, "invalid_token", "the access token cannot be used for this resource")
	case errors.Is(err, ErrTokenTooShortLived):
		a.logger().ErrorContext(r.Context(), "a2a server: the identity provider issues tokens shorter than the minimum token lifetime", "issuer", issuer.Name, "error", err)
		http.Error(w, "the identity provider issues tokens too short-lived for a turn", http.StatusBadGateway)
	default:
		a.logger().WarnContext(r.Context(), "a2a server: token exchange failed", "issuer", issuer.Name, "error", err)
		http.Error(w, "the identity provider is unavailable", http.StatusBadGateway)
	}
}

// challenge answers 401 with an RFC 6750 Bearer challenge.
func (a *Authenticator) challenge(w http.ResponseWriter, code, description string) {
	value := fmt.Sprintf("Bearer realm=%q", a.Realm)
	if code != "" {
		value += fmt.Sprintf(", error=%q, error_description=%q", code, description)
	}
	w.Header().Set("WWW-Authenticate", value)
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func (a *Authenticator) logger() *slog.Logger {
	if a.Logger == nil {
		return slog.Default()
	}
	return a.Logger
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// limitBody refuses a request whose body is larger than limit bytes with 413
// before next reads any of it. The body is read into memory first, so next
// never sees a truncated one.
func limitBody(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}
