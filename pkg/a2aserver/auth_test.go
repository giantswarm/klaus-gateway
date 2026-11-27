package a2aserver_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
)

// clockVerifier accepts every token in tokens, each expiring at its time.
type clockVerifier struct{ tokens map[string]time.Time }

func (v clockVerifier) Verify(_ context.Context, raw string) (a2aserver.VerifiedToken, error) {
	expiry, ok := v.tokens[raw]
	if !ok {
		return a2aserver.VerifiedToken{}, errors.New("bad token")
	}
	return a2aserver.VerifiedToken{Issuer: a2aserver.TrustedIssuer{Name: "dex"}, Expiry: expiry}, nil
}

// lifetimeExchanger issues numbered tokens living lifetime.
type lifetimeExchanger struct {
	calls    atomic.Int32
	lifetime time.Duration
}

func (e *lifetimeExchanger) Exchange(context.Context, string, a2aserver.TrustedIssuer) (a2aserver.ExchangedToken, error) {
	n := e.calls.Add(1)
	return a2aserver.ExchangedToken{Token: fmt.Sprintf("exchanged-%d", n), Expiry: time.Now().Add(e.lifetime)}, nil
}

// forward sends one request with token through auth and returns the status
// and the token the request ran with.
func forward(t *testing.T, auth *a2aserver.Authenticator, token string) (int, string) {
	t.Helper()
	var forwarded string
	handler := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		forwarded = pkga2a.ForwardedTokenFromContext(r.Context())
	}))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/a2a", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, forwarded
}

func TestShortLivedExchangedTokenIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		verifier := clockVerifier{tokens: map[string]time.Time{clientToken: time.Now().Add(time.Hour)}}
		short := &a2aserver.Authenticator{Verifier: verifier, Exchanger: &lifetimeExchanger{lifetime: 5 * time.Minute}, MinTokenLifetime: 25 * time.Minute}
		status, forwarded := forward(t, short, clientToken)
		require.Equal(t, http.StatusBadGateway, status)
		require.Empty(t, forwarded, "no request runs with a token that would expire mid-turn")

		long := &a2aserver.Authenticator{Verifier: verifier, Exchanger: &lifetimeExchanger{lifetime: 30 * time.Minute}, MinTokenLifetime: 25 * time.Minute}
		status, forwarded = forward(t, long, clientToken)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "exchanged-1", forwarded)
	})
}

// unsignedJWT is a JWT-shaped token carrying claims, for code that reads
// claims without a signature check.
func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"RS256"}`)) + "." + encode(payload) + ".c2lnbmF0dXJl"
}

func TestExchangedTokenExpiryIsTheEarlierDeadline(t *testing.T) {
	for name, test := range map[string]struct {
		expiresIn int64
		exp       time.Duration
		want      time.Duration
	}{
		"exp earlier":        {expiresIn: 1800, exp: 10 * time.Minute, want: 10 * time.Minute},
		"expires_in earlier": {expiresIn: 600, exp: time.Hour, want: 10 * time.Minute},
		"exp only":           {exp: 20 * time.Minute, want: 20 * time.Minute},
		"neither":            {},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			claims := map[string]any{"sub": "app"}
			if test.exp > 0 {
				claims["exp"] = now.Add(test.exp).Unix()
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				//nolint:gosec // G101: an RFC 8693 identifier and a test token, not a credential
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": unsignedJWT(t, claims), "issued_token_type": "urn:ietf:params:oauth:token-type:id_token",
					"token_type": "bearer", "expires_in": test.expiresIn,
				})
			}))
			t.Cleanup(server.Close)
			exchanger := &a2aserver.Exchanger{TokenURL: server.URL, HTTPClient: server.Client()}
			issued, err := exchanger.Exchange(t.Context(), clientToken, a2aserver.TrustedIssuer{})
			require.NoError(t, err)
			if test.want == 0 {
				require.True(t, issued.Expiry.IsZero())
				status, _ := forward(t, &a2aserver.Authenticator{Verifier: staticVerifier{accept: clientToken}, Exchanger: exchanger, MinTokenLifetime: time.Minute}, clientToken)
				require.Equal(t, http.StatusBadGateway, status, "a token without a known expiry cannot be checked against a turn")
				return
			}
			require.WithinDuration(t, now.Add(test.want), issued.Expiry, 2*time.Second)
		})
	}
}
