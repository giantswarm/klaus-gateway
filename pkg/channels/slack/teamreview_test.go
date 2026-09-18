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

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	valkeystore "github.com/giantswarm/klaus-gateway/pkg/routing/store/valkey"
)

const signInLink = "https://gateway.example/auth/slack/link"

// linkedOBO knows a fixed set of linked Slack users, their tokens and the
// emails their accounts were linked to; anyone else is unlinked.
type linkedOBO struct {
	tokens map[string]string
	emails map[string]string
}

func (o linkedOBO) TokenFor(_ context.Context, slackUser string) (string, error) {
	if tok, ok := o.tokens[slackUser]; ok {
		return tok, nil
	}
	return "", musterlink.ErrNotLinked
}
func (linkedOBO) LinkURL(string) string { return signInLink }
func (linkedOBO) Unlink(string) error   { return nil }

// LinkedIdentity exposes the linked person's identity, as *musterlink.Linker
// does: the subject and the email.
func (o linkedOBO) LinkedIdentity(slackUser string) (sub, email string, ok bool) {
	if _, linked := o.tokens[slackUser]; !linked {
		return "", "", false
	}
	return "sub-" + slackUser, o.emails[slackUser], true
}

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
	a, srv := teamReviewAdapter(t, fake.server(t).URL, tools, opts...)
	return a, srv, fake
}

// teamReviewAdapter is one gateway process serving team reviews against the
// fake Slack API at apiURL. Two of them over one store and one fake are a
// gateway before and after a restart.
func teamReviewAdapter(t *testing.T, apiURL string, tools *recordingTools, opts ...func(*slackadapter.Adapter)) (*slackadapter.Adapter, *httptest.Server) {
	t.Helper()
	obo := linkedOBO{
		tokens: map[string]string{"U1": "id-token-U1", "U2": "id-token-U2"},
		emails: map[string]string{"U1": "u1@example.com", "U2": "u2@example.com"},
	}
	options := append([]func(*slackadapter.Adapter){channelMode, func(a *slackadapter.Adapter) {
		a.OBO = obo
		a.Tools = tools
	}}, opts...)
	return newEventsAdapter(t, &stubGateway{}, apiURL, options...)
}

// withReviews gives the adapter the store its review records live in.
func withReviews(s store.ReviewStore) func(*slackadapter.Adapter) {
	return func(a *slackadapter.Adapter) { a.Reviews = s }
}

// restart stops the gateway process a and its server: the completion states
// it held in memory are gone, the store is what the next process finds.
func restart(t *testing.T, a *slackadapter.Adapter, srv *httptest.Server) {
	t.Helper()
	require.NoError(t, a.Stop(context.Background()))
	srv.Close()
}

// reviewStores are the stores a gateway keeps its reviews in: the one
// installations run, speaking the real protocol to a miniredis, and the
// in-process store. Both are shared by the adapters of one test the way the
// server is shared by the processes of one deployment.
func reviewStores(t *testing.T) map[string]store.Store {
	t.Helper()
	m := miniredis.RunT(t)
	vk, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), Timeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = vk.Close() })
	mem := memory.New()
	t.Cleanup(func() { _ = mem.Close() })
	return map[string]store.Store{"valkey": vk, "memory": mem}
}

// failingReviews is a review store whose reads or writes fail: Valkey down.
type failingReviews struct {
	store.ReviewStore
	failGet, failPut, failUpdate bool
}

var errStoreDown = errors.New("valkey: get review: dial tcp: connection refused")

func (f failingReviews) GetReview(ctx context.Context, id string) (store.Review, bool, error) {
	if f.failGet {
		return store.Review{}, false, errStoreDown
	}
	return f.ReviewStore.GetReview(ctx, id)
}

func (f failingReviews) PutReview(ctx context.Context, r store.Review) error {
	if f.failPut {
		return errStoreDown
	}
	return f.ReviewStore.PutReview(ctx, r)
}

func (f failingReviews) UpdateReview(ctx context.Context, id string, mutate func(r *store.Review) bool) (bool, error) {
	if f.failUpdate {
		return false, errStoreDown
	}
	return f.ReviewStore.UpdateReview(ctx, id, mutate)
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
	postInteraction(t, srv, reviewClick(clicker, "team_review_approve", string(value), messageTS))
}

// reviewClick is the block_actions payload of a click on one of a review's
// buttons.
func reviewClick(clicker, actionID, value, messageTS string) map[string]any {
	return map[string]any{
		"type":       "block_actions",
		"trigger_id": "trigger-" + clicker,
		"user":       map[string]any{"id": clicker},
		"channel":    map[string]any{"id": "C1"},
		"container":  map[string]any{"message_ts": messageTS},
		"message":    map[string]any{"ts": messageTS},
		"actions":    []any{map[string]any{"action_id": actionID, "value": value}},
	}
}

// postInteraction posts a signed interaction payload the way Slack does.
func postInteraction(t *testing.T, srv *httptest.Server, inner map[string]any) {
	t.Helper()
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

// A gateway restart between the post and the click — every release rolls the
// pod — changes nothing for the team: the review, its status line and its
// decision state are the store's, not the process's. The first process posts
// the review and takes a refused approval, whose status line the team reads;
// the second process approves it from a click, tells a late clicker who
// decided, and renders the ask it never posted itself.
func TestTeamReview_SurvivesRestart(t *testing.T) {
	for name, reviews := range reviewStores(t) {
		t.Run(name, func(t *testing.T) {
			tools := &recordingTools{results: []muster.Result{
				{IsError: true, Text: "not a member of team-bumblebee"},
				{Text: "review submitted on giantswarm/github#4711"},
			}}
			fake := newFakeSlackAPI()
			apiURL := fake.server(t).URL

			a1, srv1 := teamReviewAdapter(t, apiURL, tools, withReviews(reviews))
			receipt, err := a1.PostTeamReview(context.Background(), archiveReview())
			require.NoError(t, err)
			clickApprove(t, srv1, "U1", receipt.ID, receipt.TS)
			waitFor(t, "the refusal is written under the buttons before the restart", func() bool {
				return strings.Contains(statusLine(fake, receipt.TS), "<@U1>'s approval was not accepted")
			})
			restart(t, a1, srv1)

			_, srv2 := teamReviewAdapter(t, apiURL, tools, withReviews(reviews))
			clickApprove(t, srv2, "U2", receipt.ID, receipt.TS)
			waitFor(t, "the approval lands on the process that never posted the review", func() bool {
				return updatedWith(fake, receipt.TS, "Approved", "<@U2>", "review submitted")
			})
			calls := tools.recorded()
			require.Len(t, calls, 2)
			require.Equal(t, "id-token-U2", calls[1].bearer)
			require.Equal(t, "x_giantswarm-repo-manager_approve_change", calls[1].tool)
			wantArgs, _ := json.Marshal(map[string]any{"pr": 4711})
			gotArgs, _ := json.Marshal(calls[1].args)
			require.JSONEq(t, string(wantArgs), string(gotArgs), "the arguments reach the tool as posted")
			require.False(t, updatedWith(fake, receipt.TS, "expired"), "the restart did not expire the review")
			require.Empty(t, latestButtons(fake, receipt.TS))
			require.Equal(t, "<https://github.com/giantswarm/github/pull/4711|Open PR>", statusLine(fake, receipt.TS), "the link survived with the ask")

			clickApprove(t, srv2, "U1", receipt.ID, receipt.TS)
			waitFor(t, "a late clicker is told who decided", func() bool { return ephemeralTo(fake, "U1", "already approved by <@U2>") })
			require.Len(t, tools.recorded(), 2, "the late click calls no tool")
		})
	}
}

// The decision made before a restart holds after it: the next process reads
// it from the store and refuses the second click with the decider.
func TestTeamReview_DecisionSurvivesRestart(t *testing.T) {
	for name, reviews := range reviewStores(t) {
		t.Run(name, func(t *testing.T) {
			tools := &recordingTools{}
			fake := newFakeSlackAPI()
			apiURL := fake.server(t).URL

			a1, srv1 := teamReviewAdapter(t, apiURL, tools, withReviews(reviews))
			receipt, err := a1.PostTeamReview(context.Background(), archiveReview())
			require.NoError(t, err)
			clickApprove(t, srv1, "U1", receipt.ID, receipt.TS)
			waitFor(t, "the approval lands", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U1>") })
			restart(t, a1, srv1)

			_, srv2 := teamReviewAdapter(t, apiURL, tools, withReviews(reviews))
			clickApprove(t, srv2, "U2", receipt.ID, receipt.TS)
			waitFor(t, "the second clicker is told who decided", func() bool { return ephemeralTo(fake, "U2", "already approved by <@U1>") })
			require.Len(t, tools.recorded(), 1)
			require.Len(t, fake.pathCalls("chat.update"), 1, "the approved message is not rewritten")
		})
	}
}

// The completion state behind a review's Connect button is the process's
// alone (it lives minutes); the review is not. After a restart the landing
// says the link is gone and to click again, and the click approves: the
// person connected the backend meanwhile.
func TestTeamReview_ConnectLandingGoneAfterRestartClickApproves(t *testing.T) {
	reviews := reviewStores(t)["valkey"]
	tools := &recordingTools{results: []muster.Result{
		{IsError: true, Text: reviewAuthChallenge},
		{Text: "review submitted"},
	}}
	fake := newFakeSlackAPI()
	apiURL := fake.server(t).URL
	public := func(a *slackadapter.Adapter) { a.PublicBaseURL = "https://gw.example" }

	a1, srv1 := teamReviewAdapter(t, apiURL, tools, withReviews(reviews), public)
	receipt, err := a1.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)
	clickApprove(t, srv1, "U1", receipt.ID, receipt.TS)
	var stateID string
	waitFor(t, "a Connect prompt is posted", func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	})
	waitFor(t, "the team sees who is connecting", func() bool {
		return strings.Contains(statusLine(fake, receipt.TS), "<@U1> is connecting")
	})
	restart(t, a1, srv1)

	_, srv2 := teamReviewAdapter(t, apiURL, tools, withReviews(reviews), public)
	resp, err := http.Get(srv2.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, string(page), "click the button you came from again")
	require.Len(t, tools.recorded(), 1, "the landing submits nothing: the state it stood for is gone")

	clickApprove(t, srv2, "U1", receipt.ID, receipt.TS)
	waitFor(t, "the click approves on the new process", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U1>") })
	require.Len(t, tools.recorded(), 2)
}

// A store that does not answer is not "expired": the clicker is told to try
// again, the message keeps its buttons, no tool is called.
func TestTeamReview_StoreOutageOnClickKeepsTheReview(t *testing.T) {
	for name, fail := range map[string]failingReviews{
		"lookup fails": {failGet: true},
		"claim fails":  {failUpdate: true},
	} {
		t.Run(name, func(t *testing.T) {
			tools := &recordingTools{}
			mem := memory.New()
			t.Cleanup(func() { _ = mem.Close() })
			fail.ReviewStore = mem
			a, srv, fake := teamReviewHarness(t, tools, withReviews(mem))
			receipt, err := a.PostTeamReview(context.Background(), archiveReview())
			require.NoError(t, err)
			a.Reviews = fail // the outage begins after the post

			clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
			waitFor(t, "the clicker is told to try again", func() bool {
				return ephemeralTo(fake, "U1", "could not be looked up right now")
			})
			require.Empty(t, tools.recorded(), "no tool call without a claim")
			require.Empty(t, fake.pathCalls("chat.update"), "the message keeps its buttons; an outage is not an expiry")

			a.Reviews = mem // the store is back
			clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
			waitFor(t, "the next click approves", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U1>") })
		})
	}
}

// A review the gateway posted but could not record would carry a button that
// never resolves: the message says so instead, and the manager is told the
// post failed.
func TestTeamReview_UnrecordedReviewIsReplaced(t *testing.T) {
	mem := memory.New()
	t.Cleanup(func() { _ = mem.Close() })
	a, _, fake := teamReviewHarness(t, &recordingTools{}, withReviews(failingReviews{ReviewStore: mem, failPut: true}))

	_, err := a.PostTeamReview(context.Background(), archiveReview())
	require.ErrorIs(t, err, errStoreDown)
	require.Len(t, fake.pathCalls("chat.postMessage"), 1)
	updates := fake.pathCalls("chat.update")
	require.Len(t, updates, 1, "the posted message is rewritten in place")
	require.Contains(t, updates[0].params["text"], "could not be recorded")
	require.Empty(t, actionIDs(blocksOf(updates[0])), "no button that would read expired on the first click")
}

// A claim a process died with — a restart during the tool call — does not
// hold the review for seven days: past the lease the next click takes it.
func TestTeamReview_StaleClaimIsTakenOver(t *testing.T) {
	mem := memory.New()
	t.Cleanup(func() { _ = mem.Close() })
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools, withReviews(mem))
	receipt, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)

	// The record as a process that died mid-call leaves it.
	found, err := mem.UpdateReview(context.Background(), receipt.ID, func(r *store.Review) bool {
		r.DecidedBy, r.ClaimedAt = "U9", time.Now().Add(-10*time.Minute)
		return true
	})
	require.NoError(t, err)
	require.True(t, found)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "the stale claim is taken over", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U1>") })

	// A fresh claim is not.
	receipt2, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)
	_, err = mem.UpdateReview(context.Background(), receipt2.ID, func(r *store.Review) bool {
		r.DecidedBy, r.ClaimedAt = "U9", time.Now()
		return true
	})
	require.NoError(t, err)
	clickApprove(t, srv, "U1", receipt2.ID, receipt2.TS)
	waitFor(t, "an approval in flight is reported", func() bool { return ephemeralTo(fake, "U1", "<@U9>'s approval is being submitted right now") })
	require.Len(t, tools.recorded(), 1)
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
