// Package reviews is the inbound endpoint through which a manager posts a
// team review or a notice into a team's channel, and an action's result into
// its review's thread, and through which a service puts a decision to a person
// or a team:
//
//	POST /reviews               -- an ask with Approve (and Deny) buttons and the tool calls they make
//	POST /notices               -- a message without a decision
//	POST /reviews/{id}/results  -- the outcome of the action, as a follow-up in the review's thread
//	POST /decisions             -- a question with its options, answered by a click or in the person's own words
//	POST /decisions/{id}/close  -- the decision's outcome: answered, defaulted or withdrawn
//	POST /conversations         -- a thread with one person, whose replies go to the caller's reply tool
//	POST /conversations/{id}/messages -- a message into the conversation's thread
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
	"time"

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
	// Decisions serves POST /decisions; nil leaves those routes unmounted.
	Decisions channels.DecisionPoster
	// Conversations serves POST /conversations; nil leaves those routes
	// unmounted.
	Conversations channels.ConversationPoster
}

const maxBodyBytes = 1 << 20

// Mount attaches POST /reviews, POST /notices and POST /reviews/{id}/results
// to r, POST /decisions and POST /decisions/{id}/close when the handler has a
// DecisionPoster, and POST /conversations and POST /conversations/{id}/messages
// when it has a ConversationPoster.
func (h *Handler) Mount(r chi.Router) {
	if h.Logger == nil {
		h.Logger = slog.Default()
	}
	r.Post("/reviews", h.postReview)
	r.Post("/reviews/{id}/results", h.postResult)
	r.Post("/notices", h.postNotice)
	if h.Decisions != nil {
		r.Post("/decisions", h.postDecision)
		r.Post("/decisions/{id}/close", h.closeDecision)
	}
	if h.Conversations != nil {
		r.Post("/conversations", h.openConversation)
		r.Post("/conversations/{id}/messages", h.postConversationMessage)
	}
}

func (h *Handler) openConversation(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	var conversation channels.Conversation
	if !decodeBody(w, r, &conversation) {
		return
	}
	if err := conversation.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	receipt, err := h.Conversations.OpenConversation(r.Context(), conversation)
	switch {
	case errors.Is(err, channels.ErrAddresseeNotFound):
		http.Error(w, "person: "+err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.Logger.Error("conversation: open failed", "caller", caller.Username, "from", conversation.From, "error", err)
		http.Error(w, "opening the conversation failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("conversation opened", "record", "conversation_opened",
		"caller", caller.Username, "conversation", receipt.ID, "from", conversation.From, "channel", receipt.Channel, "ts", receipt.TS, "tool", conversation.Reply.Tool)
	writeJSON(w, http.StatusCreated, receipt)
}

// postConversationMessage posts into a conversation's thread. A conversation
// the gateway holds no record of — unknown, or quiet past its keep — is a
// 404.
func (h *Handler) postConversationMessage(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var message channels.ConversationMessage
	if !decodeBody(w, r, &message) {
		return
	}
	if err := message.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	receipt, err := h.Conversations.PostConversationMessage(r.Context(), id, message)
	switch {
	case errors.Is(err, channels.ErrConversationNotFound):
		http.Error(w, "no such conversation; it may have expired", http.StatusNotFound)
		return
	case err != nil:
		h.Logger.Error("conversation message: post failed", "caller", caller.Username, "conversation", id, "error", err)
		http.Error(w, "posting the message failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("conversation message posted", "record", "conversation_message_posted",
		"caller", caller.Username, "conversation", id, "channel", receipt.Channel, "ts", receipt.TS)
	writeJSON(w, http.StatusCreated, receipt)
}

func (h *Handler) postDecision(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	var decision channels.Decision
	if !decodeBody(w, r, &decision) {
		return
	}
	if err := decision.Validate(time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	receipt, err := h.Decisions.PostDecision(r.Context(), decision)
	switch {
	case errors.Is(err, channels.ErrAddresseeNotFound):
		http.Error(w, "person: "+err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.Logger.Error("decision: post failed", "caller", caller.Username, "note", decision.Note, "team", decision.Team, "channel", decision.Channel, "error", err)
		http.Error(w, "posting the decision failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("decision posted", "record", "decision_posted",
		"caller", caller.Username, "decision", receipt.ID, "note", decision.Note, "team", decision.Team, "channel", receipt.Channel, "ts", receipt.TS,
		"options", len(decision.Options), "due", decision.Due, "tool", decision.Answer.Tool)
	writeJSON(w, http.StatusCreated, receipt)
}

// closeDecision rewrites a decision's message to its outcome. A decision the
// gateway holds no record of — unknown, or past its due time plus seven days
// — is a 404.
func (h *Handler) closeDecision(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var closing channels.DecisionClose
	if !decodeBody(w, r, &closing) {
		return
	}
	if err := closing.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	receipt, err := h.Decisions.CloseDecision(r.Context(), id, closing)
	switch {
	case errors.Is(err, channels.ErrDecisionNotFound):
		http.Error(w, "no such decision; it may have expired", http.StatusNotFound)
		return
	case err != nil:
		h.Logger.Error("decision close: rewrite failed", "caller", caller.Username, "decision", id, "error", err)
		http.Error(w, "closing the decision failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("decision closed", "record", "decision_closed",
		"caller", caller.Username, "decision", id, "outcome", closing.Outcome, "channel", receipt.Channel, "ts", receipt.TS)
	writeJSON(w, http.StatusOK, receipt)
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
		h.Logger.Error("team review: post failed", "caller", caller.Username, "team", review.Team, "channel", review.Channel, "notice_channel", review.NoticeChannel, "error", err)
		http.Error(w, "posting the review failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("team review posted", "record", "team_review_posted",
		"caller", caller.Username, "team", review.Team, "channel", receipt.Channel, "ts", receipt.TS, "review", receipt.ID,
		"tool", review.Approve.Tool, "deny_tool", review.Deny.Tool, "actor", review.Actor, "pull_requests", len(review.PullRequests), "notice_ts", receipt.NoticeTS)
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

// postResult posts an action's outcome into its review's thread. A review the
// gateway holds no record of — unknown, or past its seven days — is a 404.
func (h *Handler) postResult(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var result channels.TeamReviewResult
	if !decodeBody(w, r, &result) {
		return
	}
	if err := result.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	receipt, err := h.Poster.PostTeamReviewResult(r.Context(), id, result)
	switch {
	case errors.Is(err, channels.ErrReviewNotFound):
		http.Error(w, "no such review; it may have expired", http.StatusNotFound)
		return
	case err != nil:
		h.Logger.Error("team review result: post failed", "caller", caller.Username, "review", id, "error", err)
		http.Error(w, "posting the result failed", http.StatusBadGateway)
		return
	}
	h.Logger.Info("team review result posted", "record", "team_review_result_posted",
		"caller", caller.Username, "review", id, "channel", receipt.Channel, "ts", receipt.TS)
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
		http.Error(w, "this ServiceAccount may not post to this endpoint", http.StatusForbidden)
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
	writeJSON(w, http.StatusCreated, receipt)
}

func writeJSON(w http.ResponseWriter, status int, receipt channels.PostReceipt) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(receipt)
}
