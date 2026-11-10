package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// stubGroups knows @team-bumblebee, whose one member is u1@example.com;
// @team-broken fails its lookup.
type stubGroups struct{}

func (stubGroups) IsGroupMember(_ context.Context, group, email string) (bool, error) {
	switch group {
	case "team-bumblebee":
		return email == "u1@example.com", nil
	case "team-broken":
		return false, errors.New("slack is down")
	}
	return false, channels.ErrGroupNotFound
}

func getMembership(t *testing.T, srv *httptest.Server, path, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestGroupMember(t *testing.T) {
	r := chi.NewRouter()
	(&Handler{
		Auth:           &stubAuth{tokens: map[string]string{"good": managerSA, "other": otherSA}},
		AllowedCallers: []string{managerSA},
		Poster:         &recordingPoster{},
		Groups:         stubGroups{},
	}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	for _, c := range []struct {
		path, bearer string
		status       int
		member       bool
	}{
		{"/groups/team-bumblebee/members/u1@example.com", "good", http.StatusOK, true},
		{"/groups/team-bumblebee/members/u2@example.com", "good", http.StatusOK, false},
		{"/groups/team-nobody/members/u1@example.com", "good", http.StatusNotFound, false},
		{"/groups/team-broken/members/u1@example.com", "good", http.StatusBadGateway, false},
		{"/groups/team-bumblebee/members/not-an-email", "good", http.StatusBadRequest, false},
		{"/groups/team-bumblebee/members/u1@example.com", "other", http.StatusForbidden, false},
		{"/groups/team-bumblebee/members/u1@example.com", "", http.StatusUnauthorized, false},
	} {
		resp := getMembership(t, srv, c.path, c.bearer)
		require.Equal(t, c.status, resp.StatusCode, c.path+" as "+c.bearer)
		if c.status == http.StatusOK {
			var m Membership
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
			require.Equal(t, c.member, m.Member, c.path)
		}
	}
}

func TestGroupMember_UnmountedWithoutGroups(t *testing.T) {
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, &recordingPoster{})
	resp := getMembership(t, srv, "/groups/team-bumblebee/members/u1@example.com", "good")
	require.NotEqual(t, http.StatusOK, resp.StatusCode)
}
