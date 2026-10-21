package musterlink

import (
	"context"
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
			want:       []string{"<h1>Sign-in failed</h1>", "The sign-in was denied or failed", "<code>access_denied</code>", "<title>Sign-in to Giant Swarm failed</title>"},
		},
		{
			name:       "provider unavailable",
			handler:    l.HandleCallback,
			target:     CallbackPath + "?error=temporarily_unavailable",
			wantStatus: http.StatusBadGateway,
			want:       []string{"The sign-in was denied or failed", "<code>temporarily_unavailable</code>", "Try again in a moment"},
		},
		{
			name:       "provider error that is not a known code",
			handler:    l.HandleCallback,
			target:     CallbackPath + "?" + url.Values{"error": {"your_account_is_locked_call_555"}}.Encode(),
			wantStatus: http.StatusBadRequest,
			want:       []string{"The sign-in was denied or failed"},
			notWant:    []string{"your_account_is_locked"},
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

// callbackPage runs a sign-in for slackUser through the link and the callback
// and returns the callback's page.
func callbackPage(t *testing.T, l *Linker, slackUser string) *httptest.ResponseRecorder {
	t.Helper()
	state := l.SignState(slackUser)
	l.HandleLink(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, LinkPath+"?u="+url.QueryEscape(state), nil))
	rec := httptest.NewRecorder()
	q := url.Values{"state": {state}, "code": {"auth-code"}}
	l.HandleCallback(rec, httptest.NewRequest(http.MethodGet, CallbackPath+"?"+q.Encode(), nil))
	return rec
}

func TestEmailPages(t *testing.T) {
	tests := []struct {
		name       string
		slackEmail string
		want       []string
		notWant    []string
	}{
		{
			name:       "mismatch",
			slackEmail: "bob@example.com",
			want:       []string{"Your account email does not match Slack", "alice@example.com", "bob@example.com"},
		},
		{
			name:       "no Slack email",
			slackEmail: "",
			want:       []string{"Your Slack profile has no email address", "Add an email address to your Slack profile"},
			notWant:    []string{"email is ."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := newMusterStub(t, "klaus-gateway", "alice@example.com", "muster-sub")
			l := newTestLinker(t, stub, NewMemStore(), func(context.Context, string) (string, error) {
				return tc.slackEmail, nil
			})

			rec := callbackPage(t, l, "U1")

			require.Equal(t, http.StatusForbidden, rec.Code)
			for _, want := range tc.want {
				require.Contains(t, rec.Body.String(), want)
			}
			for _, notWant := range tc.notWant {
				require.NotContains(t, rec.Body.String(), notWant)
			}
		})
	}
}
