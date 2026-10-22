package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every code of the table in giantswarm/klaus-gateway#529 is permanent
// (checked on chat.appendStream). The converging answers count as done on the
// methods they converge on, and only there: message_not_found converges on a
// delete or a reaction removal. An unknown code, or an error Slack never
// answered, is worth another attempt.
func TestSlackErrorClass(t *testing.T) {
	refusal := func(method, code string) error { return &apiError{method: method, code: code} }
	for _, code := range []string{
		"account_inactive", "as_user_not_supported", "cant_delete_message", "cant_update_message",
		"channel_not_found", "compliance_exports_prevent_deletion", "edit_window_closed", "invalid_auth",
		"invalid_blocks", "invalid_thread_ts", "is_archived", "message_not_found", "missing_scope",
		"msg_too_long", "no_text", "not_allowed_token_type", "not_authed", "not_in_channel",
		"restricted_action", "restricted_action_non_threadable_channel", "restricted_action_read_only_channel",
		"restricted_action_thread_locked", "restricted_action_thread_only_channel", "team_access_not_granted",
		"thread_locked", "token_expired", "token_revoked",
	} {
		require.Equal(t, classPermanent, slackErrorClass(methodChatAppendStream, refusal(methodChatAppendStream, code)), code)
	}

	for _, tc := range []struct {
		method, code string
		want         errorClass
	}{
		{methodReactionsAdd, "already_reacted", classConverged},
		{methodReactionsRemove, "no_reaction", classConverged},
		{methodReactionsRemove, "message_not_found", classConverged},
		{methodChatDelete, "message_not_found", classConverged},
		{"chat.update", "message_not_found", classPermanent},
		{methodReactionsRemove, "already_reacted", classRetryable},
		{methodChatPostMessage, "internal_error", classRetryable},
	} {
		require.Equal(t, tc.want, slackErrorClass(tc.method, refusal(tc.method, tc.code)), "%s %s", tc.method, tc.code)
	}

	require.Equal(t, classRetryable, slackErrorClass(methodChatPostMessage, errors.New("slack chat.postMessage: http status 503")),
		"an error with no Slack answer in it")
}

// recordingCalls is a CallRecorder that keeps what it was given, as
// "<method> <class>" pairs.
type recordingCalls struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingCalls) RecordSlackCall(method, class string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, method+" "+class)
}

func (r *recordingCalls) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// Every Web API call is counted under how it ended, and a refusal that finds
// the call's effect already in place is a success: a reaction already there,
// one already gone, a delete of a message that is gone. A gone message is a
// failure for an update, which cannot converge.
func TestClient_CountsEveryCallUnderItsClass(t *testing.T) {
	postNote := func(ctx context.Context, c *slackAPIClient) error {
		_, err := c.postNote(ctx, "C1", "a note", "1.0")
		return err
	}
	for _, tc := range []struct {
		name   string
		answer string // the body Slack answers with; "" answers HTTP 503
		call   func(context.Context, *slackAPIClient) error
		fails  bool
		want   string
	}{
		{name: "accepted", answer: `{"ok":true,"ts":"1.2"}`, call: postNote, want: "chat.postMessage ok"},
		{
			name: "reaction already there", answer: `{"ok":false,"error":"already_reacted"}`,
			call: func(ctx context.Context, c *slackAPIClient) error { return c.reactionsAdd(ctx, "C1", "1.0", "eyes") },
			want: "reactions.add converged",
		},
		{
			name: "reaction already gone", answer: `{"ok":false,"error":"no_reaction"}`,
			call: func(ctx context.Context, c *slackAPIClient) error { return c.reactionsRemove(ctx, "C1", "1.0", "eyes") },
			want: "reactions.remove converged",
		},
		{
			name: "message to delete is gone", answer: `{"ok":false,"error":"message_not_found"}`,
			call: func(ctx context.Context, c *slackAPIClient) error { return c.deleteMessage(ctx, "C1", "1.0") },
			want: "chat.delete converged",
		},
		{
			name: "message to update is gone", answer: `{"ok":false,"error":"message_not_found"}`,
			call: func(ctx context.Context, c *slackAPIClient) error {
				return c.chatUpdateBlocks(ctx, "C1", "1.0", "text")
			},
			fails: true, want: "chat.update permanent",
		},
		{name: "outage", call: postNote, fails: true, want: "chat.postMessage retryable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.answer == "" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.answer)
			}))
			t.Cleanup(srv.Close)
			rec := &recordingCalls{}

			err := tc.call(t.Context(), &slackAPIClient{botToken: "t", baseURL: srv.URL, calls: rec})

			require.Equal(t, tc.fails, err != nil, "error: %v", err)
			require.Equal(t, []string{tc.want}, rec.recorded())
		})
	}
}
