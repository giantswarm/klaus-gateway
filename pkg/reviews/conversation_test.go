package reviews

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// recordingConversations records the conversations and messages it is asked
// for; the one conversation it knows is "D0123ABCDE-5.000", and a person at
// nobody@example.com is not in the workspace.
type recordingConversations struct {
	opened   []channels.Conversation
	messages []channels.ConversationMessage
}

func (p *recordingConversations) OpenConversation(_ context.Context, c channels.Conversation) (channels.PostReceipt, error) {
	if c.Person == "nobody@example.com" {
		return channels.PostReceipt{}, channels.ErrAddresseeNotFound
	}
	p.opened = append(p.opened, c)
	return channels.PostReceipt{ID: "D0123ABCDE-5.000", Channel: "D0123ABCDE", TS: "5.000"}, nil
}

func (p *recordingConversations) PostConversationMessage(_ context.Context, id string, m channels.ConversationMessage) (channels.PostReceipt, error) {
	if id != "D0123ABCDE-5.000" {
		return channels.PostReceipt{}, channels.ErrConversationNotFound
	}
	p.messages = append(p.messages, m)
	return channels.PostReceipt{ID: id, Channel: "D0123ABCDE", TS: "6.000"}, nil
}

func newConversationServer(t *testing.T, conversations channels.ConversationPoster) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	(&Handler{
		Auth:           &stubAuth{tokens: map[string]string{"good": managerSA, "other": otherSA}},
		AllowedCallers: []string{managerSA},
		Poster:         &recordingPoster{},
		Conversations:  conversations,
	}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func validConversation() map[string]any {
	return map[string]any{
		"person": "u1@example.com",
		"from":   "Your guide on teemow's machine",
		"text":   "The supervisor asks whether kagent v1.1.1 rolls onto gazelle tonight.",
		"reply":  map[string]any{"tool": "x_beekeeper_send_message", "arguments": map[string]any{"to": "local:machine/Guide"}},
	}
}

func TestOpenConversation_OpensAndAnswersWithTheReceipt(t *testing.T) {
	conversations := &recordingConversations{}
	srv := newConversationServer(t, conversations)

	resp := post(t, srv, "/conversations", "good", validConversation())

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var receipt channels.PostReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	require.Equal(t, "D0123ABCDE-5.000", receipt.ID)
	require.Len(t, conversations.opened, 1)
	require.Equal(t, "x_beekeeper_send_message", conversations.opened[0].Reply.Tool)
	require.Equal(t, map[string]any{"to": "local:machine/Guide"}, conversations.opened[0].Reply.Arguments)
}

func TestOpenConversation_Refusals(t *testing.T) {
	srv := newConversationServer(t, &recordingConversations{})
	with := func(k string, v any) map[string]any {
		c := validConversation()
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}

	for name, tc := range map[string]struct {
		bearer string
		body   map[string]any
		status int
	}{
		"no token":            {"", validConversation(), http.StatusUnauthorized},
		"other caller":        {"other", validConversation(), http.StatusForbidden},
		"unknown field":       {"good", with("channel", "C1"), http.StatusBadRequest},
		"no person":           {"good", with("person", nil), http.StatusBadRequest},
		"from on two lines":   {"good", with("from", "a\nb"), http.StatusBadRequest},
		"no text":             {"good", with("text", " "), http.StatusBadRequest},
		"text too long":       {"good", with("text", strings.Repeat("x", channels.ConversationTextMax+1)), http.StatusBadRequest},
		"no reply tool":       {"good", with("reply", map[string]any{}), http.StatusBadRequest},
		"message argument":    {"good", with("reply", map[string]any{"tool": "t", "arguments": map[string]any{"message": "x"}}), http.StatusBadRequest},
		"person not in Slack": {"good", with("person", "nobody@example.com"), http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.status, post(t, srv, "/conversations", tc.bearer, tc.body).StatusCode)
		})
	}
}

func TestPostConversationMessage(t *testing.T) {
	conversations := &recordingConversations{}
	srv := newConversationServer(t, conversations)

	resp := post(t, srv, "/conversations/D0123ABCDE-5.000/messages", "good", map[string]any{"text": "Noted, I tell the supervisor."})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var receipt channels.PostReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	require.Equal(t, "6.000", receipt.TS)
	require.Equal(t, []channels.ConversationMessage{{Text: "Noted, I tell the supervisor."}}, conversations.messages)

	require.Equal(t, http.StatusNotFound, post(t, srv, "/conversations/D0123ABCDE-9.000/messages", "good", map[string]any{"text": "hi"}).StatusCode)
	require.Equal(t, http.StatusBadRequest, post(t, srv, "/conversations/D0123ABCDE-5.000/messages", "good", map[string]any{"text": ""}).StatusCode)
	require.Equal(t, http.StatusForbidden, post(t, srv, "/conversations/D0123ABCDE-5.000/messages", "other", map[string]any{"text": "hi"}).StatusCode)
}

func TestConversations_UnmountedWithoutAPoster(t *testing.T) {
	srv := newServer(t, &stubAuth{tokens: map[string]string{"good": managerSA}}, &recordingPoster{})
	require.Equal(t, http.StatusNotFound, post(t, srv, "/conversations", "good", validConversation()).StatusCode)
}
