package musterlink

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLinkPages covers the pages a browser lands on during the Slack sign-in.
func TestLinkPages(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "alice@example.com", "muster-sub")
	l := newTestLinker(t, stub, NewMemStore(), nil)

	tests := []struct {
		name       string
		handler    http.HandlerFunc
		target     string
		wantStatus int
		want       []string
		notWant    []string
	}{
		{
			name:       "invalid link",
			handler:    l.HandleLink,
			target:     LinkPath + "?u=forged",
			wantStatus: http.StatusBadRequest,
			want:       []string{"<h1>Sign-in failed</h1>", "The sign-in link is invalid or has expired", "Return to Slack"},
		},
		{
			name:       "invalid callback state",
			handler:    l.HandleCallback,
			target:     CallbackPath + "?" + url.Values{"state": {"forged"}, "code": {"c"}}.Encode(),
			wantStatus: http.StatusBadRequest,
			want:       []string{"<h1>Sign-in failed</h1>", "The sign-in link is invalid or has expired"},
		},
		{
			name:       "provider refuses",
			handler:    l.HandleCallback,
			target:     CallbackPath + "?error=access_denied",
			wantStatus: http.StatusBadRequest,
			want:       []string{"<h1>Sign-in failed</h1>", "The sign-in was denied or failed", "<code>access_denied</code>"},
		},
		{
			name:       "provider error that is not a known code",
			handler:    l.HandleCallback,
			target:     CallbackPath + "?" + url.Values{"error": {"your_account_is_locked_call_555"}}.Encode(),
			wantStatus: http.StatusBadRequest,
			want:       []string{"The sign-in was denied or failed"},
			notWant:    []string{"555"},
		},
		{
			name:       "success landing",
			handler:    l.HandleCallback,
			target:     CallbackPath + "?done=stale",
			wantStatus: http.StatusOK,
			want:       []string{"<title>Signed in to Giant Swarm</title>", "<h1>Signed in</h1>", "Giant Swarm is now connected", "return to Slack"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()

			tc.handler(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))

			require.Equal(t, tc.wantStatus, rec.Code)
			require.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'none'")
			for _, want := range tc.want {
				require.Contains(t, rec.Body.String(), want)
			}
			for _, notWant := range tc.notWant {
				require.NotContains(t, rec.Body.String(), notWant)
			}
		})
	}
}
