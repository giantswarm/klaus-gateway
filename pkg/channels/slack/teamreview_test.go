package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

const signInLink = "https://gateway.example/auth/slack/link"

// linkedOBO knows a fixed set of linked Slack users and their tokens; anyone
// else is unlinked.
type linkedOBO struct{ tokens map[string]string }

func (o linkedOBO) TokenFor(_ context.Context, slackUser string) (string, error) {
	if tok, ok := o.tokens[slackUser]; ok {
		return tok, nil
	}
	return "", musterlink.ErrNotLinked
}
func (linkedOBO) LinkURL(string) string { return signInLink }
func (linkedOBO) Unlink(string) error   { return nil }

// toolCall is one recorded CallTool.
type toolCall struct {
	bearer, tool string
	args         map[string]any
}

// recordingTools records tool calls and answers with the queued results, the
// last one repeating.
type recordingTools struct {
	mu      sync.Mutex
	results []muster.Result
	err     error
	calls   []toolCall
}

func (r *recordingTools) CallTool(_ context.Context, bearer, tool string, args map[string]any) (muster.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, toolCall{bearer: bearer, tool: tool, args: args})
	if r.err != nil {
		return muster.Result{}, r.err
	}
	if len(r.results) == 0 {
		return muster.Result{Text: "approved"}, nil
	}
	res := r.results[0]
	if len(r.results) > 1 {
		r.results = r.results[1:]
	}
	return res, nil
}

func (r *recordingTools) recorded() []toolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]toolCall(nil), r.calls...)
}

func teamReviewHarness(t *testing.T, tools *recordingTools) (*slackadapter.Adapter, *httptest.Server, *fakeSlackAPI) {
	t.Helper()
	fake := newFakeSlackAPI()
	obo := linkedOBO{tokens: map[string]string{"U1": "id-token-U1", "U2": "id-token-U2"}}
	a, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, channelMode, func(a *slackadapter.Adapter) {
		a.OBO = obo
		a.Tools = tools
	})
	return a, srv, fake
}

func archiveReview() channels.TeamReview {
	return channels.TeamReview{
		Team:    "team-bumblebee",
		Channel: "C1",
		Text:    "*Archive* `giantswarm/old-thing`, owned by team-bumblebee.",
		Link:    "https://github.com/giantswarm/github/pull/4711",
		Approve: channels.ToolInvocation{Tool: "x_giantswarm-repo-manager_approve_change", Arguments: map[string]any{"pr": 4711}},
	}
}

// clickApprove posts a signed team_review_approve block_actions click.
func clickApprove(t *testing.T, srv *httptest.Server, clicker, reviewID, messageTS string) {
	t.Helper()
	value, err := json.Marshal(map[string]any{"r": reviewID})
	require.NoError(t, err)
	inner := map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": clicker},
		"channel":   map[string]any{"id": "C1"},
		"container": map[string]any{"message_ts": messageTS},
		"message":   map[string]any{"ts": messageTS},
		"actions":   []any{map[string]any{"action_id": "team_review_approve", "value": string(value)}},
	}
	data, err := json.Marshal(inner)
	require.NoError(t, err)
	body := []byte("payload=" + url.QueryEscape(string(data)))
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// blocksOf returns the decoded blocks of a recorded Slack call.
func blocksOf(call recordedCall) []map[string]any {
	raw, _ := call.params["blocks"].([]any)
	blocks := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		if m, ok := b.(map[string]any); ok {
			blocks = append(blocks, m)
		}
	}
	return blocks
}

// actionIDs lists the action_ids of every button in the blocks.
func actionIDs(blocks []map[string]any) []string {
	var ids []string
	for _, b := range blocks {
		if b["type"] != "actions" {
			continue
		}
		for _, e := range b["elements"].([]any) {
			ids = append(ids, e.(map[string]any)["action_id"].(string))
		}
	}
	return ids
}

// ephemeralTo reports whether an ephemeral containing text was posted to user.
func ephemeralTo(fake *fakeSlackAPI, user, text string) bool {
	for _, e := range fake.pathCalls("chat.postEphemeral") {
		got, _ := e.params["text"].(string)
		if e.params["user"] == user && strings.Contains(got, text) {
			return true
		}
	}
	return false
}

// updatedWith reports whether the message at ts was rewritten to contain every
// given text.
func updatedWith(fake *fakeSlackAPI, ts string, texts ...string) bool {
	for _, u := range fake.pathCalls("chat.update") {
		if u.params["ts"] != ts {
			continue
		}
		got, _ := u.params["text"].(string)
		all := true
		for _, want := range texts {
			all = all && strings.Contains(got, want)
		}
		if all {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 2*time.Second, 20*time.Millisecond, what)
}

func TestTeamReview_RendersApproveAndLink(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})

	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, "C1", receipt.Channel)
	require.NotEmpty(t, receipt.TS)

	posts := fake.pathCalls("chat.postMessage")
	require.Len(t, posts, 1)
	require.Equal(t, "C1", posts[0].params["channel"])
	require.Nil(t, posts[0].params["thread_ts"], "a review is a channel message, not a thread reply")
	blocks := blocksOf(posts[0])
	require.Equal(t, []string{"team_review_approve", "team_review_open"}, actionIDs(blocks))
	require.Contains(t, blocks[0]["text"].(map[string]any)["text"], "team-bumblebee")
	require.Contains(t, blocks[0]["text"].(map[string]any)["text"], "`giantswarm/old-thing`")
	openButton := blocks[1]["elements"].([]any)[1].(map[string]any)
	require.Equal(t, "https://github.com/giantswarm/github/pull/4711", openButton["url"])
}

func TestTeamNotice_RendersWithoutButtons(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})

	receipt, err := a.PostTeamNotice(context.Background(), channels.TeamNotice{
		Team: "team-honeybadger", Channel: "C1", Text: "`giantswarm/old-thing` moved to team-bumblebee.", Link: "https://github.com/giantswarm/github/pull/4711",
	})
	require.NoError(t, err)
	require.Empty(t, receipt.ID, "a notice has no decision to record")

	posts := fake.pathCalls("chat.postMessage")
	require.Len(t, posts, 1)
	blocks := blocksOf(posts[0])
	require.Empty(t, actionIDs(blocks), "a notice carries no buttons")
	for _, b := range blocks {
		require.NotEqual(t, "actions", b["type"])
	}
	require.Contains(t, blocks[1]["elements"].([]any)[0].(map[string]any)["text"], "pull/4711", "the link is inline")
}

func TestTeamReview_LinkedMemberApprovesAsThemselves(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{Text: "review submitted on giantswarm/github#4711"}}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)

	waitFor(t, "the tool is called once", func() bool { return len(tools.recorded()) == 1 })
	call := tools.recorded()[0]
	require.Equal(t, "id-token-U1", call.bearer, "the call runs under the clicking member's own token")
	require.Equal(t, "x_giantswarm-repo-manager_approve_change", call.tool)
	require.Equal(t, map[string]any{"pr": 4711}, call.args)

	waitFor(t, "the message shows the outcome and the decider", func() bool {
		return updatedWith(fake, receipt.TS, "Approved", "<@U1>", "review submitted")
	})
}

func TestTeamReview_SecondClickIsRefused(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "first approval lands", func() bool { return updatedWith(fake, receipt.TS, "<@U1>") })

	clickApprove(t, srv, "U2", receipt.ID, receipt.TS)
	waitFor(t, "the second clicker is told who decided", func() bool {
		return ephemeralTo(fake, "U2", "already approved by <@U1>")
	})
	require.Len(t, tools.recorded(), 1, "the second click calls no tool")
	require.Len(t, fake.pathCalls("chat.update"), 1, "the message is not rewritten again")
}

func TestTeamReview_UnlinkedClickerIsAskedToSignIn(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U9", receipt.ID, receipt.TS)
	waitFor(t, "a sign-in prompt carrying the link is posted for the unlinked clicker", func() bool {
		// The prompt is a message or an ephemeral, per the adapter's sign-in
		// surface; either way it carries the link URL in its button.
		for _, path := range []string{"chat.postMessage", "chat.postEphemeral"} {
			for _, c := range fake.pathCalls(path) {
				if raw, _ := json.Marshal(c.params); strings.Contains(string(raw), signInLink) {
					return true
				}
			}
		}
		return false
	})
	require.Empty(t, tools.recorded(), "nobody to act as, so no tool call")
	require.Empty(t, fake.pathCalls("chat.update"), "the review stays open")

	// A linked member can still decide it.
	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "the linked member's approval lands", func() bool { return updatedWith(fake, receipt.TS, "<@U1>") })
	require.Equal(t, "id-token-U1", tools.recorded()[0].bearer)
}

func TestTeamReview_ToolRefusalReopensTheReview(t *testing.T) {
	// The manager finds U1 outside team-bumblebee and refuses; U2 is a member.
	tools := &recordingTools{results: []muster.Result{
		{IsError: true, Text: "not a member of team-bumblebee"},
		{Text: "review submitted"},
	}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "U1 is told the refusal, privately", func() bool {
		return ephemeralTo(fake, "U1", "not a member of team-bumblebee")
	})
	require.Empty(t, fake.pathCalls("chat.update"), "a refused approval leaves the ask open")

	clickApprove(t, srv, "U2", receipt.ID, receipt.TS)
	waitFor(t, "U2's approval lands", func() bool { return updatedWith(fake, receipt.TS, "<@U2>") })
	calls := tools.recorded()
	require.Len(t, calls, 2)
	require.Equal(t, "id-token-U1", calls[0].bearer)
	require.Equal(t, "id-token-U2", calls[1].bearer)
}

func TestTeamReview_ToolOutageKeepsTheReviewOpen(t *testing.T) {
	tools := &recordingTools{err: errors.New("muster: status 502")}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "U1 is told to try again", func() bool { return ephemeralTo(fake, "U1", "Try again") })
	require.Empty(t, fake.pathCalls("chat.update"))
}

func TestTeamReview_UnknownReviewRewritesToExpired(t *testing.T) {
	_, srv, fake := teamReviewHarness(t, &recordingTools{})

	clickApprove(t, srv, "U1", "no-such-review", "9.000")
	waitFor(t, "the dead button is rewritten", func() bool { return updatedWith(fake, "9.000", "expired") })
}

func TestTeamReview_NeedsToolsAndLinking(t *testing.T) {
	fake := newFakeSlackAPI()
	a, _ := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, channelMode)
	_, err := a.PostTeamReview(context.Background(), archiveReview())
	require.Error(t, err)
	require.Empty(t, fake.pathCalls("chat.postMessage"))
}
