// Package reviews is the inbound endpoint through which a manager posts a
// team review or a notice into a team's channel:
//
//	POST /reviews  -- an ask with an Approve button and the tool call it makes
//	POST /notices  -- a message without a decision
//
// The caller is a service identity: a Kubernetes ServiceAccount whose
// projected token the API server verifies (TokenReview), allow-listed by
// name. No personal token, no shared secret.
package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/giantswarm/klaus-gateway/pkg/auth/satoken"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// Authenticator names the ServiceAccount behind a bearer token.
// *satoken.Authenticator satisfies it.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (satoken.Identity, error)
}

// Handler serves the endpoint.
type Handler struct {
	Logger *slog.Logger
	Auth   Authenticator
	// AllowedCallers lists the ServiceAccounts
	// (system:serviceaccount:<namespace>:<name>) that may post. Empty admits
	// nobody.
	AllowedCallers []string
	Poster         channels.TeamReviewPoster
}

const maxBodyBytes = 1 << 20

// Mount attaches POST /reviews and POST /notices to r.
func (h *Handler) Mount(r chi.Router) {
	if h.Logger == nil {
		h.Logger = slog.Default()
	}
	r.Post("/reviews", h.postReview)
	r.Post("/notices", h.postNotice)
}

func (h *Handler) postReview(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	var review channels.TeamReview
	if !decodeBody(w, r, &review) {
		return
	}
	if err := review.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	receipt, err := h.Poster.PostTeamReview(r.Context(), review)
	if err != nil {
		h.Logger.Error("team review: post failed", "caller", caller.Username, "team", review.Team, "channel", review.Channel, "error", err)
		http.Error(w, "posting the review failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("team review posted", "record", "team_review_posted",
		"caller", caller.Username, "team", review.Team, "channel", receipt.Channel, "ts", receipt.TS, "review", receipt.ID, "tool", review.Approve.Tool)
	writeReceipt(w, receipt)
}

func (h *Handler) postNotice(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	var notice channels.TeamNotice
	if !decodeBody(w, r, &notice) {
		return
	}
	if err := notice.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	receipt, err := h.Poster.PostTeamNotice(r.Context(), notice)
	if err != nil {
		h.Logger.Error("team notice: post failed", "caller", caller.Username, "team", notice.Team, "channel", notice.Channel, "error", err)
		http.Error(w, "posting the notice failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("team notice posted", "record", "team_notice_posted",
		"caller", caller.Username, "team", notice.Team, "channel", receipt.Channel, "ts", receipt.TS)
	writeReceipt(w, receipt)
}

// authenticate resolves the caller from the bearer token and checks the
// allow-list. It writes the refusal itself: 401 when the API server does not
// vouch for the token, 403 for a ServiceAccount that is not allowed, 503 when
// the API server could not be asked.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (satoken.Identity, bool) {
	token, ok := bearerToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="klaus-gateway"`)
		http.Error(w, "a ServiceAccount bearer token is required", http.StatusUnauthorized)
		return satoken.Identity{}, false
	}
	caller, err := h.Auth.Authenticate(r.Context(), token)
	switch {
	case errors.Is(err, satoken.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="klaus-gateway", error="invalid_token"`)
		http.Error(w, "the token was not accepted", http.StatusUnauthorized)
		return satoken.Identity{}, false
	case err != nil:
		h.Logger.Warn("team review: token review unavailable", "error", err)
		http.Error(w, "the token could not be verified", http.StatusServiceUnavailable)
		return satoken.Identity{}, false
	}
	if !slices.Contains(h.AllowedCallers, caller.Username) {
		h.Logger.Warn("team review: caller not allowed", "caller", caller.Username)
		http.Error(w, "this ServiceAccount may not post team reviews", http.StatusForbidden)
		return satoken.Identity{}, false
	}
	return caller, true
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeReceipt(w http.ResponseWriter, receipt channels.PostReceipt) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(receipt)
}
