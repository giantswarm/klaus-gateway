package satoken

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stubReviews answers every TokenReview with a fixed status and records the
// spec it was asked to review.
type stubReviews struct {
	status authv1.TokenReviewStatus
	err    error
	spec   authv1.TokenReviewSpec
}

func (s *stubReviews) Create(_ context.Context, review *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
	s.spec = review.Spec
	if s.err != nil {
		return nil, s.err
	}
	return &authv1.TokenReview{Spec: review.Spec, Status: s.status}, nil
}

func TestAuthenticate_NamesTheServiceAccount(t *testing.T) {
	reviews := &stubReviews{status: authv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"klaus-gateway"},
		User:          authv1.UserInfo{Username: "system:serviceaccount:repo:giantswarm-repo-manager", UID: "u1", Groups: []string{"system:serviceaccounts"}},
	}}
	auth := &Authenticator{Reviews: reviews, Audiences: []string{"klaus-gateway"}}

	id, err := auth.Authenticate(context.Background(), "sa-token")
	require.NoError(t, err)
	require.Equal(t, "system:serviceaccount:repo:giantswarm-repo-manager", id.Username)
	require.Equal(t, "u1", id.UID)
	require.Equal(t, []string{"system:serviceaccounts"}, id.Groups)
	require.Equal(t, "sa-token", reviews.spec.Token, "the presented token is what is reviewed")
	require.Equal(t, []string{"klaus-gateway"}, reviews.spec.Audiences, "the gateway's audience is what the API server checks")
}

func TestAuthenticate_Refusals(t *testing.T) {
	cases := map[string]authv1.TokenReviewStatus{
		"not authenticated":         {Authenticated: false},
		"error from the API server": {Authenticated: false, Error: "token has expired"},
		"other audience":            {Authenticated: true, Audiences: []string{"kagent"}},
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			auth := &Authenticator{Reviews: &stubReviews{status: status}, Audiences: []string{"klaus-gateway"}}
			_, err := auth.Authenticate(context.Background(), "sa-token")
			require.ErrorIs(t, err, ErrUnauthenticated)
		})
	}
}

func TestAuthenticate_EmptyTokenNeedsNoReview(t *testing.T) {
	reviews := &stubReviews{err: errors.New("must not be called")}
	_, err := (&Authenticator{Reviews: reviews}).Authenticate(context.Background(), "")
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestAuthenticate_APIServerOutageIsNotARefusal(t *testing.T) {
	reviews := &stubReviews{err: errors.New("connection refused")}
	_, err := (&Authenticator{Reviews: reviews}).Authenticate(context.Background(), "sa-token")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnauthenticated)
}
