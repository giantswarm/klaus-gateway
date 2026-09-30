package reviews

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// recordingDecisions records the decisions and closes it is asked for; the
// one decision it knows is "C0123ABCDE-4.000", and a person at
// nobody@example.com is not in the workspace.
type recordingDecisions struct {
	decisions []channels.Decision
	closes    []channels.DecisionClose
}

func (p *recordingDecisions) PostDecision(_ context.Context, d channels.Decision) (channels.PostReceipt, error) {
	if d.Person == "nobody@example.com" {
		return channels.PostReceipt{}, channels.ErrAddresseeNotFound
	}
	p.decisions = append(p.decisions, d)
	return channels.PostReceipt{ID: "C0123ABCDE-4.000", Channel: "C0123ABCDE", TS: "4.000"}, nil
}

func (p *recordingDecisions) CloseDecision(_ context.Context, id string, c channels.DecisionClose) (channels.PostReceipt, error) {
	if id != "C0123ABCDE-4.000" {
		return channels.PostReceipt{}, channels.ErrDecisionNotFound
	}
	p.closes = append(p.closes, c)
	return channels.PostReceipt{ID: id, Channel: "C0123ABCDE", TS: "4.000"}, nil
}

func newDecisionServer(t *testing.T, decisions channels.DecisionPoster) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	(&Handler{
		Auth:           &stubAuth{tokens: map[string]string{"good": managerSA, "other": otherSA}},
		AllowedCallers: []string{managerSA},
		Poster:         &recordingPoster{},
		Decisions:      decisions,
	}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func validDecision() map[string]any {
	return map[string]any{
		"team":      "team-bumblebee",
		"channel":   "C0123ABCDE",
		"note":      "614",
		"question":  "Roll kagent v1.1.1 onto gazelle tonight?",
		"statusQuo": "graveler and glean run v1.1.1 since Tuesday without a restart.",
		"options": []any{
			map[string]any{"label": "Roll tonight", "consequence": "The supervisor clears the lane at 22:00."},
			map[string]any{"label": "Wait for Monday"},
		},
		"recommend": 1,
		"due":       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"default":   "Wait for Monday.",
		"askedBy":   "Board pull 99 on the lab machine",
		"answer":    map[string]any{"tool": "x_beekeeper_note_answer", "arguments": map[string]any{"note": "614"}},
	}
}

func TestPostDecision_PostsAndAnswersWithTheReceipt(t *testing.T) {
	decisions := &recordingDecisions{}
	srv := newDecisionServer(t, decisions)

	resp := post(t, srv, "/decisions", "good", validDecision())

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var receipt channels.PostReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	require.Equal(t, "C0123ABCDE-4.000", receipt.ID)
	require.Len(t, decisions.decisions, 1)
	d := decisions.decisions[0]
	require.Equal(t, "x_beekeeper_note_answer", d.Answer.Tool)
	require.Equal(t, 1, d.Recommend)
	require.Equal(t, "Roll tonight", d.Options[0].Label)
}

func TestPostDecision_Refusals(t *testing.T) {
	srv := newDecisionServer(t, &recordingDecisions{})
	with := func(k string, v any) map[string]any {
		d := validDecision()
		if v == nil {
			delete(d, k)
		} else {
			d[k] = v
		}
		return d
	}

	for name, tc := range map[string]struct {
		bearer string
		body   map[string]any
		status int
	}{
		"no token":        {"", validDecision(), http.StatusUnauthorized},
		"other caller":    {"other", validDecision(), http.StatusForbidden},
		"unknown field":   {"good", with("urgency", "high"), http.StatusBadRequest},
		"no addressee":    {"good", with("team", nil), http.StatusBadRequest},
		"due in the past": {"good", with("due", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)), http.StatusBadRequest},
		"person not in Slack": {"good", func() map[string]any {
			d := with("team", nil)
			delete(d, "channel")
			d["person"] = "nobody@example.com"
			return d
		}(), http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.status, post(t, srv, "/decisions", tc.bearer, tc.body).StatusCode)
		})
	}
}

func TestCloseDecision(t *testing.T) {
	decisions := &recordingDecisions{}
	srv := newDecisionServer(t, decisions)

	require.Equal(t, http.StatusOK, post(t, srv, "/decisions/C0123ABCDE-4.000/close", "good", map[string]any{"outcome": "defaulted"}).StatusCode)
	require.Equal(t, []channels.DecisionClose{{Outcome: "defaulted"}}, decisions.closes)

	require.Equal(t, http.StatusNotFound, post(t, srv, "/decisions/C0123ABCDE-9.000/close", "good", map[string]any{"outcome": "answered"}).StatusCode)
	require.Equal(t, http.StatusBadRequest, post(t, srv, "/decisions/C0123ABCDE-4.000/close", "good", map[string]any{"outcome": "maybe"}).StatusCode)
	require.Equal(t, http.StatusForbidden, post(t, srv, "/decisions/C0123ABCDE-4.000/close", "other", map[string]any{"outcome": "withdrawn"}).StatusCode)
}

func TestDecisions_UnmountedWithoutAPoster(t *testing.T) {
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, &recordingPoster{})
	require.Equal(t, http.StatusNotFound, post(t, srv, "/decisions", "good", validDecision()).StatusCode)
}
