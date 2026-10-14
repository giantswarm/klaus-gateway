package slack_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// grantStatus is a core_auth_status answer muster sends through call_tool.
func grantStatus(t *testing.T, servers ...map[string]any) muster.Result {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"servers": servers})
	require.NoError(t, err)
	return muster.Result{Text: string(raw), Structured: raw, Dispatched: true}
}

func waitingGrant(name string) map[string]any {
	return map[string]any{
		"name": name, "status": "auth_required", "kind": "grant", "grant_scope": "subject",
		"auth_tool": "core_auth_login", "auth_url": "https://muster.example/oauth/proxy/start?state=" + name,
	}
}

func connectedGrant(name string) map[string]any {
	return map[string]any{"name": name, "status": "connected", "kind": "grant", "grant_scope": "subject"}
}

func signInServer(name string) map[string]any {
	return map[string]any{"name": name, "status": "connected", "kind": "sso", "token_forwarding_enabled": true}
}

// withGrants gives the adapter the grant status reader the modal reads.
func withGrants(tools *recordingTools) func(*slackadapter.Adapter) {
	return func(a *slackadapter.Adapter) { a.Grants = tools }
}

// authorizeAdapter is the session-connect adapter with grants and a views.open
// that answers the modal's view ID.
func authorizeAdapter(t *testing.T, gw *stubGateway, tools *recordingTools) (*fakeSlackAPI, *httptest.Server) {
	t.Helper()
	fake, srv, _ := sessionConnectAdapter(t, gw, withGrants(tools))
	fake.setResponse("views.open", `{"ok":true,"view":{"id":"V1"}}`)
	return fake, srv
}

// modalRows are the server rows of the last views.update of view V1: the
// server's section text and its Authorize button's url ("" without one).
func modalRows(t *testing.T, fake *fakeSlackAPI) map[string]string {
	t.Helper()
	updates := fake.pathCalls("views.update")
	require.NotEmpty(t, updates)
	last := updates[len(updates)-1]
	require.Equal(t, "V1", last.params["view_id"])
	view, _ := last.params["view"].(map[string]any)
	blocks, _ := view["blocks"].([]any)
	rows := map[string]string{}
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		text, _ := block["text"].(map[string]any)
		line, _ := text["text"].(string)
		// A row is "*<server>*\n<state>".
		head, _, isRow := strings.Cut(line, "\n")
		name, ok := strings.CutPrefix(head, "*")
		if !ok || !isRow {
			continue
		}
		name = strings.TrimSuffix(name, "*")
		accessory, _ := block["accessory"].(map[string]any)
		buttonURL, _ := accessory["url"].(string)
		rows[name] = buttonURL
	}
	return rows
}

func modalText(fake *fakeSlackAPI) string {
	updates := fake.pathCalls("views.update")
	if len(updates) == 0 {
		return ""
	}
	return fmt.Sprint(updates[len(updates)-1].params["view"])
}

// The modal opens at once on the click, then lists every server the person
// reaches with a grant: the waiting one with an Authorize button whose link
// lands on the gateway, the authorized one without, and the servers that come
// with the sign-in as a count. Granting from the modal redraws it, with no
// conversation resumed.
func TestAuthorizeModal_ListsGrantsAndRedrawsAsEachLands(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{
		grantStatus(t, waitingGrant("agent-manager"), connectedGrant("github"), signInServer("kubernetes")),
		grantStatus(t, connectedGrant("agent-manager"), connectedGrant("github"), signInServer("kubernetes")),
	}}
	gw := &stubGateway{}
	fake, srv := authorizeAdapter(t, gw, tools)

	postInteraction(t, srv, map[string]any{
		"type": "block_actions", "trigger_id": "trigger-U1", "user": map[string]any{"id": "U1"},
		"actions": []any{map[string]any{"action_id": "authorize_all_open", "value": ""}},
	})

	require.Eventually(t, func() bool { return len(fake.pathCalls("views.update")) == 1 }, flowWait, 20*time.Millisecond)
	opened := fake.pathCalls("views.open")
	require.Len(t, opened, 1)
	require.Equal(t, "trigger-U1", opened[0].params["trigger_id"], "the trigger opens the loading view before muster answers")
	require.Contains(t, fmt.Sprint(opened[0].params["view"]), "Reading which tools")

	calls := tools.recorded()
	require.Len(t, calls, 1)
	require.Equal(t, "core_auth_status", calls[0].tool)
	require.Equal(t, "tok", calls[0].bearer, "muster is asked as the person")
	require.Equal(t, true, calls[0].args["auth_urls"])

	rows := modalRows(t, fake)
	require.Len(t, rows, 2, "one row per grant server; the sign-in servers are a count")
	require.Empty(t, rows["github"], "an authorized server has no button")
	link := rows["agent-manager"]
	require.NotEmpty(t, link)
	parsed, err := url.Parse(link)
	require.NoError(t, err)
	require.Equal(t, "agent-manager", parsed.Query().Get("state"), "muster's own link survives decoration")
	require.True(t, strings.HasPrefix(parsed.Query().Get("redirect"), "https://gw.example/connectors/complete?s="), "the link lands on the gateway")
	text := modalText(fake)
	require.Contains(t, text, "1 of 2")
	require.Contains(t, text, "1 more come with your sign-in")

	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateOf(t, link)) + "&server=agent-manager")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Eventually(t, func() bool { return len(fake.pathCalls("views.update")) == 2 }, flowWait, 20*time.Millisecond, "the landing redraws the modal")
	require.Empty(t, modalRows(t, fake)["agent-manager"], "the granted server has no button any more")
	require.Contains(t, modalText(fake), "Every tool here is authorized")
	require.Zero(t, gw.dispatchCount(), "a modal row resumes no conversation")
}

// `/swarmgeist connect` opens the modal, wherever it is typed, instead of the
// agent picker.
func TestAuthorizeModal_SlashConnectOpensIt(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{grantStatus(t, waitingGrant("cluster-manager"))}}
	fake, srv := authorizeAdapter(t, &stubGateway{}, tools)

	require.Equal(t, http.StatusOK, sendSlashCommand(t, srv, "C1", "U1", "connect", ""))

	require.Eventually(t, func() bool { return len(fake.pathCalls("views.update")) == 1 }, flowWait, 20*time.Millisecond)
	require.Contains(t, fmt.Sprint(fake.pathCalls("views.open")[0].params["view"]), "authorize_modal")
	require.NotEmpty(t, modalRows(t, fake)["cluster-manager"])
}

// Right after the sign-in, a person with grants still missing gets a direct
// message with the button that opens the modal; one with none gets nothing.
func TestAuthorizeModal_OfferedAfterSignIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status muster.Result
		offer  bool
	}{
		{"grants missing", grantStatus(t, waitingGrant("agent-manager"), waitingGrant("github")), true},
		{"all granted", grantStatus(t, connectedGrant("agent-manager"), signInServer("kubernetes")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := &recordingTools{results: []muster.Result{tc.status}}
			fake := newFakeSlackAPI()
			a, _ := newEventsAdapter(t, &stubGateway{}, fake.server(t).URL, func(a *slackadapter.Adapter) {
				a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
				a.ConnectorPrompts = true
				a.PublicBaseURL = "https://gw.example"
				a.Grants = tools
			})

			a.OnUserLinked(t.Context(), "U1", "u1@example.com")

			require.Eventually(t, func() bool { return len(tools.recorded()) == 1 }, flowWait, 20*time.Millisecond)
			require.Equal(t, false, tools.recorded()[0].args["auth_urls"], "the offer issues no link")
			if !tc.offer {
				require.Never(t, func() bool { return len(postsTo(fake, "U1")) > 0 }, 200*time.Millisecond, 20*time.Millisecond)
				return
			}
			require.Eventually(t, func() bool { return len(postsTo(fake, "U1")) == 1 }, flowWait, 20*time.Millisecond)
			offer := fmt.Sprint(postsTo(fake, "U1")[0].params["blocks"])
			require.Contains(t, offer, "2 tools need your one-time authorization")
			require.Contains(t, offer, "authorize_all_open")
		})
	}
}

// A Session start muster refuses carries "Authorize all" beside its Connect
// button. Granting the refused server from the modal lands on the prompt's
// own completion: the held message runs, as the Connect button would have.
func TestAuthorizeModal_FromTheSessionConnectPromptRunsTheHeldMessage(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{
		grantStatus(t, waitingGrant("agent-manager"), waitingGrant("github")),
		grantStatus(t, connectedGrant("agent-manager"), waitingGrant("github")),
	}}
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	fake, srv := authorizeAdapter(t, gw, tools)

	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "140.000", ""))

	var stateID string
	require.Eventually(t, func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond)
	ephemeral := fmt.Sprint(fake.pathCalls("chat.postEphemeral")[0].params["blocks"])
	require.Contains(t, ephemeral, "authorize_all_open", "the Connect prompt offers the modal")
	require.Contains(t, ephemeral, stateID, "Authorize all carries the prompt's completion")

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	postInteraction(t, srv, map[string]any{
		"type": "block_actions", "trigger_id": "trigger-U1", "user": map[string]any{"id": "U1"},
		"actions": []any{map[string]any{"action_id": "authorize_all_open", "value": stateID}},
	})
	require.Eventually(t, func() bool { return len(fake.pathCalls("views.update")) == 1 }, flowWait, 20*time.Millisecond)
	rows := modalRows(t, fake)
	require.Equal(t, stateID, stateOf(t, rows["agent-manager"]), "the refused server's row lands on the prompt's completion")
	require.NotEqual(t, stateID, stateOf(t, rows["github"]), "another server's row has a completion of its own")

	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID) + "&server=agent-manager")
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond, "the held message runs")
	require.Eventually(t, func() bool { return len(fake.pathCalls("views.update")) == 2 }, flowWait, 20*time.Millisecond, "and the modal redraws")
	require.Empty(t, modalRows(t, fake)["agent-manager"])
	require.NotEmpty(t, modalRows(t, fake)["github"])
}

// Without a grant status reader the Connect prompts stay as they were.
func TestAuthorizeModal_NotOfferedWithoutGrants(t *testing.T) {
	gw := &stubGateway{}
	gw.dispatchErr = sessionConnectChallenge()
	fake, srv, _ := sessionConnectAdapter(t, gw)

	sendEvent(t, srv, mention("U1", "hi", "150.000", ""))

	require.Eventually(t, func() bool { _, _, ok := connectButton(fake); return ok }, flowWait, 20*time.Millisecond)
	require.NotContains(t, fmt.Sprint(fake.pathCalls("chat.postEphemeral")[0].params["blocks"]), "authorize_all_open")
}

// A muster without core_auth_status refuses the tool itself: the modal says
// so once, and the Connect prompts stop offering it.
func TestAuthorizeModal_WithdrawnWhenMusterLacksTheTool(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{Text: "tool core_auth_status not found", IsError: true}}}
	gw := &stubGateway{}
	gw.dispatchErr = sessionConnectChallenge()
	fake, srv := authorizeAdapter(t, gw, tools)

	postInteraction(t, srv, map[string]any{
		"type": "block_actions", "trigger_id": "trigger-U1", "user": map[string]any{"id": "U1"},
		"actions": []any{map[string]any{"action_id": "authorize_all_open", "value": ""}},
	})
	require.Eventually(t, func() bool { return len(fake.pathCalls("views.update")) == 1 }, flowWait, 20*time.Millisecond)
	require.Contains(t, modalText(fake), "could not be read")

	sendEvent(t, srv, mention("U1", "hi", "170.000", ""))
	require.Eventually(t, func() bool { _, _, ok := connectButton(fake); return ok }, flowWait, 20*time.Millisecond)
	require.NotContains(t, fmt.Sprint(fake.pathCalls("chat.postEphemeral")[0].params["blocks"]), "authorize_all_open")
}
