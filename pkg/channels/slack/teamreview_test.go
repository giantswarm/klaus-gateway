package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

func teamReviewHarness(t *testing.T, tools *recordingTools, opts ...func(*slackadapter.Adapter)) (*slackadapter.Adapter, *httptest.Server, *fakeSlackAPI) {
	t.Helper()
	fake := newFakeSlackAPI()
	obo := linkedOBO{tokens: map[string]string{"U1": "id-token-U1", "U2": "id-token-U2"}}
	options := append([]func(*slackadapter.Adapter){channelMode, func(a *slackadapter.Adapter) {
		a.OBO = obo
		a.Tools = tools
	}}, opts...)
	a, srv := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, options...)
	return a, srv, fake
}

// reviewAuthChallenge is what muster's call_tool answers for a tool whose
// server holds no grant for the person yet: an error result naming the server
// and carrying the sign-in link, as seen on a live install.
const reviewAuthChallenge = "auth_required: server 'giantswarm-repo-manager' requires authentication before its tools can be called (this session is not authenticated to it).\n\n" +
	"Authentication Required\n\n" +
	"Server: giantswarm-repo-manager\n" +
	"Status: Authentication required for giantswarm-repo-manager. Please visit the link below to authenticate.\n\n" +
	"Please sign in to connect to this server:\n\n" +
	"https://muster.example/oauth/proxy/start?state=abc123\n\n" +
	"After signing in, run this tool again to complete the connection."

// statusLine returns the context line under the review's buttons in its
// latest rewrite, or "" when the latest rewrite carries none.
func statusLine(fake *fakeSlackAPI, ts string) string {
	updates := fake.pathCalls("chat.update")
	for i := len(updates) - 1; i >= 0; i-- {
		if updates[i].params["ts"] != ts {
			continue
		}
		for _, b := range blocksOf(updates[i]) {
			if b["type"] != "context" {
				continue
			}
			elements, _ := b["elements"].([]any)
			if len(elements) == 0 {
				continue
			}
			text, _ := elements[0].(map[string]any)["text"].(string)
			return text
		}
		return ""
	}
	return ""
}

// latestButtons lists the action_ids of the review message's latest rewrite.
func latestButtons(fake *fakeSlackAPI, ts string) []string {
	updates := fake.pathCalls("chat.update")
	for i := len(updates) - 1; i >= 0; i-- {
		if updates[i].params["ts"] == ts {
			return actionIDs(blocksOf(updates[i]))
		}
	}
	return nil
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
	require.Eventually(t, cond, flowWait, 20*time.Millisecond, what)
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

// The tool's answer reaches the team as a sentence: a plain text as written,
// a JSON object by its message field, structured data without one not at all.
func TestTeamReview_OutcomeShowsTheToolsMessageNotItsJSON(t *testing.T) {
	for name, tc := range map[string]struct{ result, shown, hidden string }{
		"plain text":           {result: "review submitted on giantswarm/github#4711", shown: "review submitted on giantswarm/github#4711"},
		"object with message":  {result: `{"pullRequest":4711,"login":"carol","member":true,"message":"Approved as carol and merged: giantswarm/github#4711."}`, shown: "Approved as carol and merged: giantswarm/github#4711.", hidden: `"login"`},
		"object without one":   {result: `{"pullRequest":4711,"login":"carol","member":true}`, hidden: `"pullRequest"`},
		"array":                {result: `[{"pullRequest":4711}]`, hidden: "4711"},
		"empty":                {result: ""},
		"whitespace around it": {result: "  \n{\"message\": \" merged. \"}\n", shown: "_merged._"},
	} {
		t.Run(name, func(t *testing.T) {
			tools := &recordingTools{results: []muster.Result{{Text: tc.result}}}
			a, srv, fake := teamReviewHarness(t, tools)
			receipt, err := a.PostTeamReview(context.Background(), archiveReview())
			require.NoError(t, err)
			clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
			waitFor(t, "the approval lands", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U1>") })
			var text string
			for _, u := range fake.pathCalls("chat.update") {
				if u.params["ts"] == receipt.TS {
					text, _ = u.params["text"].(string)
				}
			}
			if tc.shown != "" {
				require.Contains(t, text, tc.shown)
			}
			if tc.hidden != "" {
				require.NotContains(t, text, tc.hidden)
			}
			if tc.shown == "" {
				require.False(t, strings.Contains(text, "_"), "nothing of the answer is shown: %q", text)
			}
		})
	}
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
	waitFor(t, "the refusal is written under the buttons, naming U1 and the reason", func() bool {
		return strings.Contains(statusLine(fake, receipt.TS), "<@U1>'s approval was not accepted: not a member of team-bumblebee")
	})
	require.Equal(t, []string{"team_review_approve", "team_review_open"}, latestButtons(fake, receipt.TS), "the buttons stay")
	require.False(t, updatedWith(fake, receipt.TS, "Approved"), "a refused approval leaves the ask open")
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "the status line is the one place the refusal is read, by U1 and the team alike")

	clickApprove(t, srv, "U2", receipt.ID, receipt.TS)
	waitFor(t, "U2's approval lands", func() bool { return updatedWith(fake, receipt.TS, "<@U2>") })
	calls := tools.recorded()
	require.Len(t, calls, 2)
	require.Equal(t, "id-token-U1", calls[0].bearer)
	require.Equal(t, "id-token-U2", calls[1].bearer)
	require.Empty(t, latestButtons(fake, receipt.TS), "the approved message carries no buttons")
	require.Equal(t, "<https://github.com/giantswarm/github/pull/4711|Open PR>", statusLine(fake, receipt.TS), "the pull request stays one click away")
}

func TestTeamReview_ToolOutageKeepsTheReviewOpen(t *testing.T) {
	tools := &recordingTools{err: errors.New("muster: status 502")}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "the failed attempt is written under the buttons", func() bool {
		return strings.Contains(statusLine(fake, receipt.TS), "<@U1>'s approval could not be submitted")
	})
	require.False(t, updatedWith(fake, receipt.TS, "Approved"))
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "nothing is repeated to U1 privately")
}

// The manager's backend holds no grant for the clicker yet: muster answers the
// approval with a sign-in challenge. The clicker gets a Connect button whose
// link lands back on the gateway, the team sees who is connecting, and once
// the sign-in lands the approval is submitted again as the person — no second
// click.
func TestTeamReview_AuthChallengeConnectsThenApproves(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{
		{IsError: true, Text: reviewAuthChallenge},
		{Text: "review submitted on giantswarm/github#4711"},
	}}
	a, srv, fake := teamReviewHarness(t, tools, func(a *slackadapter.Adapter) { a.PublicBaseURL = "https://gw.example" })
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)

	var buttonURL, stateID string
	waitFor(t, "a Connect prompt is posted to the clicker", func() bool {
		var ok bool
		buttonURL, stateID, ok = connectButton(fake)
		return ok
	})
	prompts := fake.pathCalls("chat.postEphemeral")
	require.Len(t, prompts, 1)
	require.Equal(t, "U1", prompts[0].params["user"])
	require.Nil(t, prompts[0].params["thread_ts"], "the prompt is a channel-level ephemeral, where the click happened")
	require.Contains(t, prompts[0].params["text"], "connect *giantswarm-repo-manager* once")
	require.Contains(t, prompts[0].params["text"], "submitted as soon as you're back")
	require.Equal(t, []string{"connector_connect"}, actionIDs(blocksOf(prompts[0])), "no 'Not now': an ignored prompt simply lapses")
	parsed, err := url.Parse(buttonURL)
	require.NoError(t, err)
	require.Equal(t, "muster.example", parsed.Host)
	require.Equal(t, "abc123", parsed.Query().Get("state"), "the login link's own query survives")
	require.Equal(t, "https://gw.example/connectors/complete?s="+stateID, parsed.Query().Get("redirect"))
	require.NotContains(t, statusLine(fake, receipt.TS), "not accepted", "a sign-in challenge is not a refusal")

	waitFor(t, "the team sees who is connecting", func() bool {
		return strings.Contains(statusLine(fake, receipt.TS), "<@U1> is connecting *giantswarm-repo-manager*")
	})
	require.Equal(t, []string{"team_review_approve", "team_review_open"}, latestButtons(fake, receipt.TS), "the review stays open for the team")
	require.Len(t, tools.recorded(), 1)

	// The browser lands on the gateway after the consent flow.
	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID) + "&server=giantswarm-repo-manager")
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(page), "your approval is being submitted")

	waitFor(t, "the approval is submitted again as the person", func() bool { return len(tools.recorded()) == 2 })
	calls := tools.recorded()
	require.Equal(t, "id-token-U1", calls[1].bearer)
	require.Equal(t, calls[0].tool, calls[1].tool)
	require.Equal(t, calls[0].args, calls[1].args)
	waitFor(t, "the message shows the outcome and the decider", func() bool {
		return updatedWith(fake, receipt.TS, "Approved", "<@U1>", "review submitted")
	})
	require.Empty(t, latestButtons(fake, receipt.TS))

	// A reload of the landing does not submit the approval a third time.
	resp, err = http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	time.Sleep(50 * time.Millisecond)
	require.Len(t, tools.recorded(), 2)
}

// Without a public base URL the sign-in cannot land back on the gateway: the
// Connect button opens the plain login link and the prompt says to click
// Approve again afterwards.
func TestTeamReview_AuthChallengeWithoutLandingAsksToClickAgain(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{
		{IsError: true, Text: reviewAuthChallenge},
		{Text: "review submitted"},
	}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	var buttonURL, value string
	waitFor(t, "a Connect prompt is posted", func() bool {
		var ok bool
		buttonURL, value, ok = connectButton(fake)
		return ok
	})
	require.Equal(t, "https://muster.example/oauth/proxy/start?state=abc123", buttonURL, "the plain login link")
	require.Equal(t, "giantswarm-repo-manager", value, "no completion state: the click is a no-op")
	require.True(t, ephemeralTo(fake, "U1", "then click *Approve* again"))

	// Connected meanwhile; the second click goes through.
	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "the approval lands", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U1>") })
}

// A backend that still challenges after the sign-in landed is not looped: the
// person is told, once, and the review stays open.
func TestTeamReview_StillChallengedAfterSignInIsNotLooped(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{IsError: true, Text: reviewAuthChallenge}}}
	a, srv, fake := teamReviewHarness(t, tools, func(a *slackadapter.Adapter) { a.PublicBaseURL = "https://gw.example" })
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	var stateID string
	waitFor(t, "a Connect prompt is posted", func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	})
	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	_ = resp.Body.Close()

	waitFor(t, "the status line says the backend still challenges after the sign-in", func() bool {
		return strings.Contains(statusLine(fake, receipt.TS), "<@U1> connected *giantswarm-repo-manager*, but the manager still asks them to sign in")
	})
	require.Len(t, tools.recorded(), 2, "one call per attempt, no loop")
	require.Len(t, fake.pathCalls("chat.postEphemeral"), 1, "the one Connect prompt; no second one, no private repeat")
	require.Equal(t, []string{"team_review_approve", "team_review_open"}, latestButtons(fake, receipt.TS), "the review stays open")
}

// Someone else approves while the clicker is connecting: the landing's
// resubmission is told who decided instead of approving twice.
func TestTeamReview_DecidedWhileConnecting(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{
		{IsError: true, Text: reviewAuthChallenge},
		{Text: "review submitted"},
	}}
	a, srv, fake := teamReviewHarness(t, tools, func(a *slackadapter.Adapter) { a.PublicBaseURL = "https://gw.example" })
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	var stateID string
	waitFor(t, "a Connect prompt is posted", func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	})
	clickApprove(t, srv, "U2", receipt.ID, receipt.TS)
	waitFor(t, "U2's approval lands", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U2>") })

	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	_ = resp.Body.Close()
	waitFor(t, "U1 is told who decided", func() bool { return ephemeralTo(fake, "U1", "already approved by <@U2>") })
	require.Len(t, tools.recorded(), 2, "the landing calls no tool")
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
