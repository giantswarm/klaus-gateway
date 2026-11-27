package a2aserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	return a2aserver.VerifiedToken{Expiry: expiry}, nil
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

func TestShortLivedPlatformTokenIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		auth := &a2aserver.Authenticator{
			Verifier: clockVerifier{tokens: map[string]time.Time{
				"short":     time.Now().Add(5 * time.Minute),
				"long":      time.Now().Add(30 * time.Minute),
				"no-expiry": {},
			}},
			MinTokenLifetime: 25 * time.Minute,
		}
		for _, token := range []string{"short", "no-expiry"} {
			status, forwarded := forward(t, auth, token)
			require.Equal(t, http.StatusBadGateway, status, token)
			require.Empty(t, forwarded, "no request runs with a token that may expire mid-turn")
		}

		status, forwarded := forward(t, auth, "long")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "long", forwarded)

		time.Sleep(6 * time.Minute)
		status, _ = forward(t, auth, "long")
		require.Equal(t, http.StatusBadGateway, status, "24 minutes left is less than a turn")
	})
}

func TestPlatformTokenIsForwardedUnchanged(t *testing.T) {
	const raw = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"
	auth := &a2aserver.Authenticator{Verifier: clockVerifier{tokens: map[string]time.Time{raw: time.Now().Add(time.Hour)}}, MinTokenLifetime: time.Minute}
	status, forwarded := forward(t, auth, raw)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, raw, forwarded, "the gateway acts with the platform token byte for byte")
}
