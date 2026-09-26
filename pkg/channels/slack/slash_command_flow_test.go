package slack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// The native slash command opens the agent picker; submitting it opens a
// conversation the way a prefixed mention does, with the gateway posting the
// root itself. These flows drive the adapter through the signed HTTP
// endpoints and observe the Slack Web API calls and what reaches the gateway.

// sendSlashCommand posts a signed slash command payload (the form Slack sends
// to the command's request URL) and returns the HTTP status.
func sendSlashCommand(t *testing.T, srv *httptest.Server, channel, user, text, responseURL string) int {
	t.Helper()
	form := url.Values{
		"command":      {"/swarmgeist"},
		"text":         {text},
		"user_id":      {user},
		"channel_id":   {channel},
		"trigger_id":   {"123.456.abcdef"},
		"response_url": {responseURL},
	}
	body := []byte(form.Encode())
	stamp, sig := signBody(t, "signing-secret", body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/commands", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// sendAskAgentSubmission posts a signed view_submission of the agent picker,
// as Slack sends it when the user clicks Ask.
func sendAskAgentSubmission(t *testing.T, srv *httptest.Server, user, privateMetadata, agentRef, question string) {
	t.Helper()
	inner := map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": user},
		"view": map[string]any{
			"id":               "V1",
			"callback_id":      "ask_agent",
			"private_metadata": privateMetadata,
			"state": map[string]any{"values": map[string]any{
				"ask_agent_agent":    map[string]any{"agent": map[string]any{"type": "static_select", "selected_option": map[string]any{"value": agentRef}}},
				"ask_agent_question": map[string]any{"question": map[string]any{"type": "plain_text_input", "value": question}},
			}},
		},
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

// openedView waits for the views.open call and returns its view payload.
func openedView(t *testing.T, fake *fakeSlackAPI) map[string]any {
	t.Helper()
	fake.waitForPath(t, "views.open", 1)
	view, ok := fake.pathCalls("views.open")[0].params["view"].(map[string]any)
	require.True(t, ok, "views.open carries a view object")
	return view
}

// responseURLTexts returns the texts posted through the slash command's
// response_url (the fake serves it at /response_url).
func responseURLTexts(fake *fakeSlackAPI) string {
	return allText(fake.pathCalls("response_url"))
}

func pickerRoster() *fakeRoster {
	return &fakeRoster{agents: []pkga2a.AgentInfo{
		{Name: "swarmgeist", Namespace: "kagent", DisplayName: "Swarmgeist", Description: "General assistant"},
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent", Description: "Investigates infra issues"},
		{Name: "grill-master", Namespace: "kagent", Description: "BBQ"},
	}}
}

func pickerCards() *fakeCards {
	return &fakeCards{known: map[string]string{
		"kagent/swarmgeist": "Swarmgeist",
		"kagent/sre-agent":  "SRE Agent",
	}}
}

// The slash command opens a modal over the roster: one option per agent
// labelled by display name (technical name when none), the default agent
// preselected, the command's text prefilled as the question, and the origin
// stashed in private_metadata.
func TestSlashCommand_OpensAgentPicker(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	require.Equal(t, http.StatusOK, sendSlashCommand(t, srv, "C1", "U1", "why are pods crashlooping?", api.URL+"/response_url"))

	view := openedView(t, fake)
	require.Equal(t, "ask_agent", view["callback_id"])
	require.Equal(t, "modal", view["type"])

	var pm map[string]string
	require.NoError(t, json.Unmarshal([]byte(view["private_metadata"].(string)), &pm))
	require.Equal(t, "C1", pm["c"], "the channel the command was typed in")
	require.Equal(t, "U1", pm["u"], "the invoking user")
	require.Equal(t, api.URL+"/response_url", pm["r"])

	require.Equal(t, "New conversation", view["title"].(map[string]any)["text"])
	require.Equal(t, "Start thread", view["submit"].(map[string]any)["text"])

	blocks := view["blocks"].([]any)
	require.Len(t, blocks, 3)
	lead := blocks[0].(map[string]any)
	require.Equal(t, "context", lead["type"])
	require.Equal(t, "Starts a thread in <#C1> under the agent's name. Anyone in the channel can read it; you decide who may instruct the agent.",
		lead["elements"].([]any)[0].(map[string]any)["text"], "the line names where the conversation lands")
	agentInput := blocks[1].(map[string]any)
	require.Equal(t, "Swarmgeist is the default for this workspace.", agentInput["hint"].(map[string]any)["text"])
	agentSelect := agentInput["element"].(map[string]any)
	require.Equal(t, "static_select", agentSelect["type"])
	var labels, values []string
	for _, o := range agentSelect["options"].([]any) {
		opt := o.(map[string]any)
		labels = append(labels, opt["text"].(map[string]any)["text"].(string))
		values = append(values, opt["value"].(string))
	}
	require.Equal(t, []string{"Swarmgeist", "SRE Agent", "grill-master"}, labels, "display name, technical name when none")
	require.Equal(t, []string{"kagent/swarmgeist", "kagent/sre-agent", "kagent/grill-master"}, values, "values are the A2A refs")
	require.Equal(t, "kagent/swarmgeist", agentSelect["initial_option"].(map[string]any)["value"], "the default agent is preselected")

	require.Equal(t, "Prompt", blocks[2].(map[string]any)["label"].(map[string]any)["text"])
	question := blocks[2].(map[string]any)["element"].(map[string]any)
	require.Equal(t, "plain_text_input", question["type"])
	require.Equal(t, true, question["multiline"])
	require.Equal(t, "why are pods crashlooping?", question["initial_value"], "the command's text prefills the question")

	require.Empty(t, fake.pathCalls("response_url"), "nothing to tell the user privately")
}

// Slack offers the command in the agent pane's composer, so it opens the
// picker there too: the same modal, and a line that names no channel because
// the conversation lands in the direct message itself.
func TestSlashCommand_DMOpensAgentPicker(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	// The default harness serves DMs and no channel: the picker's gate asks
	// about DMs, not about the channel allowlist that never covers one.
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, withSelection(pickerRoster(), pickerCards()))

	require.Equal(t, http.StatusOK, sendSlashCommand(t, srv, "D1", "U1", "hello", api.URL+"/response_url"))

	view := openedView(t, fake)
	require.Equal(t, "ask_agent", view["callback_id"])
	lead := view["blocks"].([]any)[0].(map[string]any)
	require.Equal(t, "Starts a conversation here under the agent's name.",
		lead["elements"].([]any)[0].(map[string]any)["text"],
		"no channel to name, and no one else who could read it")

	var pm map[string]string
	require.NoError(t, json.Unmarshal([]byte(view["private_metadata"].(string)), &pm))
	require.Equal(t, "D1", pm["c"])
	require.Empty(t, pm["t"], "the command carries no thread: its own root is the conversation")
	require.Empty(t, fake.pathCalls("response_url"), "nothing to tell the user privately")
}

// An installation that redirects DMs refuses the command there with the same
// notice a message in that DM gets, and opens no picker.
func TestSlashCommand_DMRedirectModeIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	// channelMode serves channels and redirects DMs.
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "D1", "U1", "hello", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "works in channels, not in direct messages")
	require.Empty(t, fake.pathCalls("views.open"))
}

// A command in a channel outside the served set gets the same private notice a
// mention there gets, and no modal.
func TestSlashCommand_UnservedChannelIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	// Default harness: channelMode none.
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "hello", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "channel is not enabled")
	require.Empty(t, fake.pathCalls("views.open"))
}

// The picker needs the roster; when kagent cannot be listed the user is told
// so instead of getting an empty form.
func TestSlashCommand_RosterUnavailableIsLoud(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	roster := &fakeRoster{err: fmt.Errorf("kagent down")}
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, channelMode, withSelection(roster, pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "agents cannot be listed")
	require.Empty(t, fake.pathCalls("views.open"))
}

// tokenRoster records the caller token the roster read carried, so a test
// can assert the picker lists agents as the invoking user.
type tokenRoster struct {
	mu     sync.Mutex
	agents []pkga2a.AgentInfo
	tokens []string
}

func (r *tokenRoster) ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = append(r.tokens, pkga2a.ForwardedTokenFromContext(ctx))
	return r.agents, nil
}

func (r *tokenRoster) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

// oneUserOBO mints a fixed token for one linked Slack user and reports everyone
// else as not linked.
type oneUserOBO struct {
	user  string
	token string
}

func (o oneUserOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	if slackUserID == o.user {
		return o.token, nil
	}
	return "", musterlink.ErrNotLinked
}
func (o oneUserOBO) LinkURL(string) string { return "https://gw.example.com/link" }
func (o oneUserOBO) Unlink(string) error   { return nil }

// The kagent controller lists AgentTemplates to a human identity, so the
// picker's roster read runs as the invoking user: their linked token travels
// on the context.
func TestSlashCommand_ListsRosterAsCaller(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	roster := &tokenRoster{agents: pickerRoster().agents}
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, channelMode, func(a *slackadapter.Adapter) {
		a.DefaultAgent = "kagent/swarmgeist"
		a.Roster = roster
		a.AgentCards = pickerCards()
		a.OBO = oneUserOBO{user: "U1", token: "tok-u1"}
	})

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")

	openedView(t, fake)
	require.Equal(t, []string{"tok-u1"}, roster.seen(), "the roster is listed with the caller's token")
}

// Without a caller identity and with a cold roster cache, the controller
// refuses the listing; the user is told to sign in rather than that the
// roster is down.
func TestSlashCommand_UnlinkedCallerIsAskedToSignIn(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	roster := &fakeRoster{err: pkga2a.ErrNoIdentity}
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, channelMode, withSelection(roster, pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "`/login`")
	require.Empty(t, fake.pathCalls("views.open"))
}

// slowThenFastRoster stalls its first listing until the caller gives up, then
// answers normally: the cold-cache read that outlives the trigger_id budget.
type slowThenFastRoster struct {
	mu     sync.Mutex
	calls  int
	agents []pkga2a.AgentInfo
}

func (r *slowThenFastRoster) ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if first {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.agents, nil
}

// A roster read that outlives the trigger_id budget tells the user to retry,
// and does not poison the roster's negative cache: the retry lists again and
// opens the picker.
func TestSlashCommand_SlowRosterTellsUserToRetry(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	roster := &slowThenFastRoster{agents: pickerRoster().agents}
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, channelMode, func(a *slackadapter.Adapter) {
		a.DefaultAgent = "kagent/swarmgeist"
		a.Roster = roster
		a.AgentCards = pickerCards()
	})

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	require.Eventually(t, func() bool { return strings.Contains(responseURLTexts(fake), "took too long") },
		6*time.Second, 50*time.Millisecond, "the budget expires and the user is told to retry")
	require.Empty(t, fake.pathCalls("views.open"), "no picker after the budget")

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	openedView(t, fake)
}

func TestSlashCommand_InvalidSignatureRejected(t *testing.T) {
	_, srv := newEventsAdapter(t, &stubGateway{}, "", channelMode)

	body := []byte("command=%2Fswarmgeist&user_id=U1&channel_id=C1&trigger_id=1.2.3")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/channels/slack/commands", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("X-Slack-Signature", "v0=badsig")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// Submitting the picker opens the conversation: the gateway posts the root
// under the agent's identity, the submitter is the initiator, the thread is
// bound, and the question is the first turn. Replies then behave as in any
// conversation: the submitter's reply inherits the agent, a newcomer waits for
// the submitter's consent.
func TestAskAgentSubmission_OpensConversation(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	a, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)

	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "why are pods crashlooping?")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the submission dispatches the first turn")

	// The root: the question as a branded message, who asked as context
	// under it. The binding and the initiator live in the thread record.
	root := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "C1", root.params["channel"])
	require.Nil(t, root.params["thread_ts"], "the root is a top-level message")
	require.Equal(t, "SRE Agent", root.params["username"], "posted under the agent's identity")
	requireQuestionMessage(t, root.params, "why are pods crashlooping?", "U1")

	msgs := dispatched()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef)
	require.Equal(t, "why are pods crashlooping?", msgs[0].Text, "the question is the turn, without decoration")
	require.Equal(t, "U1", msgs[0].Subject, "the turn runs as the submitter")
	require.Equal(t, "C1", msgs[0].ChannelID)
	require.NotEmpty(t, msgs[0].ThreadID)
	require.Equal(t, msgs[0].ThreadID, msgs[0].MessageID, "the bot root is the thread")
	rootTS := msgs[0].ThreadID

	// The submitter's reply inherits the agent without re-selecting.
	waitThreadIdle(t, a, rootTS)
	sendEvent(t, srv, mention("U1", "and the nodes?", "200.000", rootTS))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 50*time.Millisecond, "the reply dispatches")
	require.Equal(t, "kagent/sre-agent", dispatched()[1].AgentRef, "replies inherit the conversation's agent")

	// A newcomer is gated on the submitter's consent: nothing dispatches, the
	// submitter gets the consent prompt.
	waitThreadIdle(t, a, rootTS)
	sendEvent(t, srv, mention("U2", "me too", "300.000", rootTS))
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postEphemeral") {
			if c.params["user"] == "U1" {
				return true
			}
		}
		return false
	}, flowWait, 50*time.Millisecond, "the consent prompt goes to the submitter, the initiator")
	require.Equal(t, 2, gw.dispatchCount(), "the newcomer's message waits")
	require.Empty(t, fake.pathCalls("response_url"), "a clean submission needs no private notice")
}

// The same submission in a direct message: the root is posted there, the turn
// runs under the picked agent, and the channel allowlist — which no DM is ever
// on — does not refuse it.
func TestAskAgentSubmission_DMOpensConversation(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "D1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)

	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "why are pods crashlooping?")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the submission dispatches the first turn")

	root := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "D1", root.params["channel"])
	require.Nil(t, root.params["thread_ts"], "the root is a top-level message")
	require.Equal(t, "SRE Agent", root.params["username"], "posted under the agent's identity")

	msgs := dispatched()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef, "the picked agent, not the default")
	require.Equal(t, "D1", msgs[0].ChannelID)
	require.Equal(t, msgs[0].ThreadID, msgs[0].MessageID, "the bot root is the thread")
	require.Empty(t, fake.pathCalls("response_url"), "a clean submission needs no private notice")
}

// A picked agent that no longer validates fails loudly through the response
// URL: no root, no turn, never a substitute.
func TestAskAgentSubmission_UnknownAgentFailsLoudly(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)

	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/grill-master", "smoke a brisket")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "No agent named `kagent/grill-master` is available")
	require.Empty(t, fake.pathCalls("chat.postMessage"), "no root is posted")
	require.Equal(t, 0, gw.dispatchCount())
}

// A public channel the bot was never invited to: the root post fails with
// not_in_channel, the gateway joins and retries, and the conversation opens.
func TestAskAgentSubmission_JoinsPublicChannelOnNotInChannel(t *testing.T) {
	fake := newFakeSlackAPI()
	var joined atomic.Bool
	fake.failIf = func(path string, params map[string]any) string {
		if path == "conversations.join" {
			joined.Store(true)
			return ""
		}
		if path == "chat.postMessage" && !joined.Load() {
			return "not_in_channel"
		}
		return ""
	}
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "hello")

	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the retried root post opens the conversation")
	require.Len(t, fake.pathCalls("conversations.join"), 1)
	require.Equal(t, "C1", fake.pathCalls("conversations.join")[0].params["channel"])
}

// A private channel refuses the join: the user is asked to invite the bot.
func TestAskAgentSubmission_PrivateChannelAsksForInvite(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("chat.postMessage", "not_in_channel")
	fake.setFail("conversations.join", "method_not_supported_for_channel_type")
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "hello")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "Invite the bot to the channel")
	require.Equal(t, 0, gw.dispatchCount())
}

// The conversation the picker opens is rooted by a message the gateway posted,
// so Slack, left to read the starter off the root, would attribute the session
// to the app. The creating status call names the submitter instead.
func TestAskAgentSubmission_SessionNamesTheSubmitter(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "why are pods crashlooping?")

	fake.waitForPath(t, "agents.sessions.setStatus", 1)
	create := fake.pathCalls("agents.sessions.setStatus")[0]
	require.Equal(t, "processing", create.params["status"], "the creating call")
	require.Equal(t, "U1", create.params["initiator_user_id"],
		"the session belongs to the person who submitted the picker, not to the bot that posted the root")
}

// The turn's dispatch record names the picker as the agent's source.
func TestAskAgentSubmission_DispatchRecordNamesCommandSource(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	logs := &syncBuffer{}
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()), func(a *slackadapter.Adapter) {
		a.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "hello")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "agent_source=command")
	}, flowWait, 50*time.Millisecond, "turn_dispatch records agent_source=command")
}

// syncBuffer is a goroutine-safe strings.Builder for capturing adapter logs.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var _ = channels.InboundMessage{}

// A picked agent that stopped being runnable between the listing and the
// submit is refused with the reason, through the response URL: no root, no
// turn, and not as an unknown name.
func TestAskAgentSubmission_NotRunnableAgentIsRefusedWithReason(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, func(a *slackadapter.Adapter) {
		a.DefaultAgent = "kagent/swarmgeist"
		a.Roster = pickerRoster()
		a.AgentCards = notRunnableCards{}
	})

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)

	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "why are pods crashlooping?")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake),
		"is installed but cannot start a conversation right now: no Harness admits this AgentTemplate")
	require.NotContains(t, responseURLTexts(fake), "No agent named")
	require.Empty(t, fake.pathCalls("chat.postMessage"), "no root is posted")
	require.Equal(t, 0, gw.dispatchCount())
}

// requireQuestionMessage checks the message that opens a conversation from the
// picker: the question as the message and its fallback text, who asked as the
// only context line under it.
func requireQuestionMessage(t *testing.T, params map[string]any, question, user string) {
	t.Helper()
	require.Equal(t, question, params["text"])
	blocks := params["blocks"].([]any)
	require.Len(t, blocks, 2)
	section := blocks[0].(map[string]any)
	require.Equal(t, "section", section["type"])
	require.Equal(t, question, section["text"].(map[string]any)["text"])
	ctxBlock := blocks[1].(map[string]any)
	require.Equal(t, "context", ctxBlock["type"])
	require.Equal(t, "Asked by <@"+user+">", ctxBlock["elements"].([]any)[0].(map[string]any)["text"])
}
