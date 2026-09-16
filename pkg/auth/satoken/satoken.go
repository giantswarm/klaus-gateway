// Package satoken authenticates a caller by its Kubernetes ServiceAccount
// token. The caller presents the projected token it already has as a bearer;
// the API server verifies it for the gateway's audience through the
// TokenReview API and names the ServiceAccount
// (system:serviceaccount:<namespace>:<name>). Nothing is shared and nothing
// is personal: the token is the caller's own, the API server is the verifier.
package satoken

import (
	"context"
	"errors"
	"fmt"
	"slices"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrUnauthenticated is returned when the API server does not vouch for the
// token: missing, expired, revoked, or minted for another audience.
var ErrUnauthenticated = errors.New("satoken: token not authenticated")

// Identity is the ServiceAccount the API server vouched for.
type Identity struct {
	// Username is system:serviceaccount:<namespace>:<name>.
	Username string
	UID      string
	Groups   []string
}

// TokenReviewer creates TokenReviews. The clientset's
// AuthenticationV1().TokenReviews() satisfies it.
type TokenReviewer interface {
	Create(ctx context.Context, review *authv1.TokenReview, opts metav1.CreateOptions) (*authv1.TokenReview, error)
}

// Authenticator verifies bearer tokens through TokenReview.
type Authenticator struct {
	Reviews TokenReviewer
	// Audiences the token must be minted for. The API server intersects them
	// with the token's; an empty intersection is a refusal. Empty accepts the
	// API server's default audience.
	Audiences []string
}

// Authenticate returns the ServiceAccount behind token, or ErrUnauthenticated.
// A failure to reach the API server is its own error, so a caller can tell a
// refusal from an outage.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrUnauthenticated
	}
	review, err := a.Reviews.Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: a.Audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, fmt.Errorf("satoken: token review: %w", err)
	}
	if review.Status.Error != "" {
		return Identity{}, fmt.Errorf("%w: %s", ErrUnauthenticated, review.Status.Error)
	}
	if !review.Status.Authenticated {
		return Identity{}, ErrUnauthenticated
	}
	if len(a.Audiences) > 0 && !slices.ContainsFunc(review.Status.Audiences, func(aud string) bool {
		return slices.Contains(a.Audiences, aud)
	}) {
		return Identity{}, fmt.Errorf("%w: token audience %v is not %v", ErrUnauthenticated, review.Status.Audiences, a.Audiences)
	}
	return Identity{
		Username: review.Status.User.Username,
		UID:      review.Status.User.UID,
		Groups:   review.Status.User.Groups,
	}, nil
}
