package a2aserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// TokenVerifier verifies an A2A client's bearer token (signature, issuer,
// audience, expiry and requesting client) and returns its trusted issuer.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (TrustedIssuer, error)
}

// TokenExchanger exchanges a verified A2A client token of issuer for the
// token the gateway acts with (RFC 8693).
type TokenExchanger interface {
	Exchange(ctx context.Context, subjectToken string, issuer TrustedIssuer) (string, error)
}

// Authenticator admits a request whose bearer token the Verifier accepts and
// runs it with the exchanged token as the caller's forwarded token.
type Authenticator struct {
	Verifier  TokenVerifier
	Exchanger TokenExchanger
	// Realm is the realm of the WWW-Authenticate challenge.
	Realm  string
	Logger *slog.Logger
}

// ErrExchangeRefused is an exchange the identity provider refused.
var ErrExchangeRefused = errors.New("token exchange refused")

// Middleware authenticates every request it wraps.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			a.challenge(w, "", "")
			return
		}
		issuer, err := a.Verifier.Verify(r.Context(), raw)
		if err != nil {
			a.logger().InfoContext(r.Context(), "a2a server: bearer token refused", "error", err)
			a.challenge(w, "invalid_token", "the access token is invalid or expired")
			return
		}
		exchanged, err := a.Exchanger.Exchange(r.Context(), raw, issuer)
		if err != nil {
			a.logger().WarnContext(r.Context(), "a2a server: token exchange failed", "issuer", issuer.Name, "error", err)
			if errors.Is(err, ErrExchangeRefused) {
				a.challenge(w, "invalid_token", "the access token cannot be used for this resource")
				return
			}
			http.Error(w, "the identity provider is unavailable", http.StatusBadGateway)
			return
		}
		next.ServeHTTP(w, r.WithContext(pkga2a.WithForwardedToken(r.Context(), exchanged)))
	})
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

// LimitBody refuses a request whose body is larger than limit bytes with 413
// before next reads any of it. The body is read into memory first, so next
// never sees a truncated one.
func LimitBody(limit int64, next http.Handler) http.Handler {
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
