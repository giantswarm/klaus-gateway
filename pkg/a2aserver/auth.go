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
	"time"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// VerifiedToken is a platform token the verifier accepted.
type VerifiedToken struct {
	// Expiry is the token's exp.
	Expiry time.Time
}

// TokenVerifier verifies the platform token a request carries (signature,
// issuer, audience, expiry and requesting client).
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (VerifiedToken, error)
}

// Authenticator admits a request whose bearer token the Verifier accepts and
// runs it with that token, unchanged, as the caller's forwarded token.
type Authenticator struct {
	Verifier TokenVerifier
	// MaxBodyBytes, when positive, caps the request body. The body is read
	// only after the token is verified.
	MaxBodyBytes int64
	// MinTokenLifetime is the least lifetime the token must have left when a
	// request runs with it: the agent runtime keeps it for the whole turn and
	// cannot renew it. Zero checks only that it has not expired.
	MinTokenLifetime time.Duration
	// Realm is the realm of the WWW-Authenticate challenge.
	Realm  string
	Logger *slog.Logger
}

// ErrTokenTooShortLived is a platform token with less than
// Authenticator.MinTokenLifetime left: the identity provider issues tokens
// shorter than a turn may run.
var ErrTokenTooShortLived = errors.New("platform token is too short-lived for a turn")

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
		if err := a.checkLifetime(verified, time.Now()); err != nil {
			a.logger().ErrorContext(r.Context(), "a2a server: the identity provider issues tokens shorter than the minimum token lifetime", "error", err)
			http.Error(w, "the identity provider issues tokens too short-lived for a turn", http.StatusBadGateway)
			return
		}
		forward := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(pkga2a.WithForwardedToken(r.Context(), raw)))
		}))
		if a.MaxBodyBytes > 0 {
			forward = limitBody(a.MaxBodyBytes, forward)
		}
		forward.ServeHTTP(w, r)
	})
}

func (a *Authenticator) checkLifetime(verified VerifiedToken, now time.Time) error {
	switch {
	case verified.Expiry.IsZero():
		return fmt.Errorf("the platform token states no expiry")
	case verified.Expiry.Sub(now) < max(a.MinTokenLifetime, 0) || !verified.Expiry.After(now):
		return fmt.Errorf("%w: %s left, %s needed", ErrTokenTooShortLived, verified.Expiry.Sub(now).Round(time.Second), a.MinTokenLifetime)
	}
	return nil
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
