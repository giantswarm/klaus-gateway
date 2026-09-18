package reviews

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/satoken"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

const (
	managerSA = "system:serviceaccount:repo-manager:giantswarm-repo-manager"
	otherSA   = "system:serviceaccount:default:someone-else"
)

// stubAuth maps tokens to ServiceAccounts; anything else is unauthenticated.
type stubAuth struct {
	tokens map[string]string
	down   bool
}

func (s *stubAuth) Authenticate(_ context.Context, token string) (satoken.Identity, error) {
	if s.down {
		return satoken.Identity{}, errors.New("api server unreachable")
	}
	if name, ok := s.tokens[token]; ok {
		return satoken.Identity{Username: name}, nil
	}
	return satoken.Identity{}, satoken.ErrUnauthenticated
}

// postedResult is one recorded PostTeamReviewResult.
type postedResult struct {
	id     string
	result channels.TeamReviewResult
}

// recordingPoster records what it is asked to post; the one review it knows
// is "rv1", a result for any other id is not found.
type recordingPoster struct {
	reviews []channels.TeamReview
	notices []channels.TeamNotice
	results []postedResult
	err     error
}

func (p *recordingPoster) PostTeamReview(_ context.Context, review channels.TeamReview) (channels.PostReceipt, error) {
	p.reviews = append(p.reviews, review)
	receipt := channels.PostReceipt{ID: "rv1", Channel: review.Channel, TS: "1.000"}
	if review.NoticeChannel != "" {
		receipt.NoticeTS = "0.500"
	}
	return receipt, p.err
}

func (p *recordingPoster) PostTeamNotice(_ context.Context, notice channels.TeamNotice) (channels.PostReceipt, error) {
	p.notices = append(p.notices, notice)
	return channels.PostReceipt{Channel: notice.Channel, TS: "2.000"}, p.err
}

func (p *recordingPoster) PostTeamReviewResult(_ context.Context, id string, result channels.TeamReviewResult) (channels.PostReceipt, error) {
	if p.err != nil {
		return channels.PostReceipt{}, p.err
	}
	if id != "rv1" {
		return channels.PostReceipt{}, channels.ErrReviewNotFound
	}
	p.results = append(p.results, postedResult{id: id, result: result})
	return channels.PostReceipt{ID: id, Channel: "C0123ABCDE", TS: "3.000"}, nil
}

func newServer(t *testing.T, auth Authenticator, poster channels.TeamReviewPoster) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	(&Handler{Auth: auth, AllowedCallers: []string{managerSA}, Poster: poster}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, path, bearer string, body any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(payload))
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func validReview() map[string]any {
	return map[string]any{
		"team":    "team-bumblebee",
		"channel": "C0123ABCDE",
		"text":    "*Archive* `giantswarm/old-thing` (owned by team-bumblebee).",
		"link":    "https://github.com/giantswarm/github/pull/4711",
		"approve": map[string]any{"tool": "x_giantswarm-repo-manager_approve_change", "arguments": map[string]any{"pr": 4711}},
	}
}

func TestPostReview_UnauthenticatedIsRefused(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, poster)

	for name, bearer := range map[string]string{"no token": "", "unknown token": "forged"} {
		t.Run(name, func(t *testing.T) {
			resp := post(t, srv, "/reviews", bearer, validReview())
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			require.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")
		})
	}
	require.Empty(t, poster.reviews, "nothing is posted for an unauthenticated caller")
}

func TestPostReview_UnknownServiceAccountIsRefused(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"other": otherSA}}, poster)

	resp := post(t, srv, "/reviews", "other", validReview())
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, poster.reviews)
}

func TestPostReview_APIServerDownIsNotARefusal(t *testing.T) {
	srv := newServer(t, &stubAuth{down: true}, &recordingPoster{})
	resp := post(t, srv, "/reviews", "good", validReview())
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestPostReview_AllowedCallerPostsTheAsk(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, poster)

	resp := post(t, srv, "/reviews", "good", validReview())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var receipt channels.PostReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	require.Equal(t, channels.PostReceipt{ID: "rv1", Channel: "C0123ABCDE", TS: "1.000"}, receipt)

	require.Len(t, poster.reviews, 1)
	got := poster.reviews[0]
	require.Equal(t, "team-bumblebee", got.Team)
	require.Equal(t, "x_giantswarm-repo-manager_approve_change", got.Approve.Tool)
	require.Equal(t, map[string]any{"pr": float64(4711)}, got.Approve.Arguments, "arguments reach the poster verbatim")
}

func TestPostReview_InvalidBodies(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, poster)

	cases := map[string]func(m map[string]any){
		"channel name instead of ID": func(m map[string]any) { m["channel"] = "#team-bumblebee" },
		"no tool":                    func(m map[string]any) { m["approve"] = map[string]any{} },
		"no text":                    func(m map[string]any) { m["text"] = "" },
		"link not http":              func(m map[string]any) { m["link"] = "javascript:alert(1)" },
		"unknown field":              func(m map[string]any) { m["initiator"] = "U1" },
		"deny arguments without a tool": func(m map[string]any) {
			m["deny"] = map[string]any{"arguments": map[string]any{"action": "a1"}}
		},
		"pull request not http":  func(m map[string]any) { m["pullRequests"] = []string{"ftp://example.com/x"} },
		"empty pull request":     func(m map[string]any) { m["pullRequests"] = []string{""} },
		"too many pull requests": func(m map[string]any) { m["pullRequests"] = make([]string, channels.TeamPullRequestsMax+1) },
		"notice channel name":    func(m map[string]any) { m["noticeChannel"] = "#account-engineers" },
		"notice channel is the channel": func(m map[string]any) {
			m["noticeChannel"] = m["channel"]
		},
		"blank actor": func(m map[string]any) { m["actor"] = "  " },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := validReview()
			mutate(body)
			resp := post(t, srv, "/reviews", "good", body)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
	require.Empty(t, poster.reviews)
}

// An action's review: the actor, the pull requests, a deny tool and a second
// channel reach the poster as given, and the receipt names the notice.
func TestPostReview_ActionFieldsReachThePoster(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, poster)

	body := validReview()
	body["actor"] = "alex@example.com"
	body["pullRequests"] = []string{"https://github.com/giantswarm/a-configs/pull/12", "https://github.com/giantswarm/b-configs/pull/7"}
	body["deny"] = map[string]any{"tool": "x_giantswarm-platform-manager_deny_action", "arguments": map[string]any{"action": "a1"}}
	body["noticeChannel"] = "C0456FGHIJ"
	resp := post(t, srv, "/reviews", "good", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var receipt channels.PostReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	require.Equal(t, "0.500", receipt.NoticeTS, "the receipt locates the notice too")

	require.Len(t, poster.reviews, 1)
	got := poster.reviews[0]
	require.Equal(t, "alex@example.com", got.Actor)
	require.Equal(t, []string{"https://github.com/giantswarm/a-configs/pull/12", "https://github.com/giantswarm/b-configs/pull/7"}, got.PullRequests)
	require.Equal(t, "x_giantswarm-platform-manager_deny_action", got.Deny.Tool)
	require.Equal(t, map[string]any{"action": "a1"}, got.Deny.Arguments)
	require.Equal(t, "C0456FGHIJ", got.NoticeChannel)
}

func TestPostResult_AllowedCallerPostsIntoTheThread(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, poster)

	resp := post(t, srv, "/reviews/rv1/results", "good", map[string]any{
		"text": "✅ Merged and rolled out; every probe green.", "link": "https://github.com/giantswarm/a-configs/actions/runs/99",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var receipt channels.PostReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	require.Equal(t, channels.PostReceipt{ID: "rv1", Channel: "C0123ABCDE", TS: "3.000"}, receipt)
	require.Len(t, poster.results, 1)
	require.Equal(t, "rv1", poster.results[0].id)
	require.Equal(t, "https://github.com/giantswarm/a-configs/actions/runs/99", poster.results[0].result.Link)
}

func TestPostResult_Refusals(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA, "other": otherSA}}, poster)
	body := map[string]any{"text": "merged"}

	require.Equal(t, http.StatusNotFound, post(t, srv, "/reviews/gone/results", "good", body).StatusCode, "a review the gateway does not hold")
	require.Equal(t, http.StatusBadRequest, post(t, srv, "/reviews/rv1/results", "good", map[string]any{"text": ""}).StatusCode)
	require.Equal(t, http.StatusBadRequest, post(t, srv, "/reviews/rv1/results", "good", map[string]any{"text": "x", "review": "rv1"}).StatusCode, "unknown field")
	require.Equal(t, http.StatusUnauthorized, post(t, srv, "/reviews/rv1/results", "", body).StatusCode)
	require.Equal(t, http.StatusForbidden, post(t, srv, "/reviews/rv1/results", "other", body).StatusCode)
	require.Empty(t, poster.results)
}

func TestPostNotice_AllowedCallerPostsIt(t *testing.T) {
	poster := &recordingPoster{}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, poster)

	resp := post(t, srv, "/notices", "good", map[string]any{
		"team": "team-honeybadger", "channel": "C0123ABCDE", "text": "`giantswarm/old-thing` is now archived.",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Len(t, poster.notices, 1)
	require.Equal(t, "team-honeybadger", poster.notices[0].Team)

	resp = post(t, srv, "/notices", "", map[string]any{"team": "t", "channel": "C0123ABCDE", "text": "x"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Len(t, poster.notices, 1)
}

func TestPost_PosterFailureIsABadGateway(t *testing.T) {
	poster := &recordingPoster{err: errors.New("slack: channel_not_found")}
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, poster)

	resp := post(t, srv, "/reviews", "good", validReview())
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}
