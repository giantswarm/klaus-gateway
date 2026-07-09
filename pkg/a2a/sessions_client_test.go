package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionsClient_Exists(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/api/sessions/present":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := &SessionsClient{
		BaseURL:     srv.URL + "/", // trailing slash must be trimmed
		TokenSource: staticToken("tok-123"),
	}

	exists, err := c.Exists(t.Context(), "present")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "/api/sessions/present", gotPath)
	require.Equal(t, "Bearer tok-123", gotAuth, "the caller's token must be forwarded")

	exists, err = c.Exists(t.Context(), "gone")
	require.NoError(t, err)
	require.False(t, exists)
}

func TestSessionsClient_UnexpectedStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := &SessionsClient{BaseURL: srv.URL}
	_, err := c.Exists(t.Context(), "x")
	require.Error(t, err)
}

type staticToken string

func (s staticToken) Token(_ context.Context) (string, error) { return string(s), nil }
