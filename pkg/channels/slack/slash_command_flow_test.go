package slack_test

import (
	"bytes"
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

	blocks := view["blocks"].([]any)
	require.Len(t, blocks, 2)
	agentSelect := blocks[0].(map[string]any)["element"].(map[string]any)
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

	question := blocks[1].(map[string]any)["element"].(map[string]any)
	require.Equal(t, "plain_text_input", question["type"])
	require.Equal(t, true, question["multiline"])
	require.Equal(t, "why are pods crashlooping?", question["initial_value"], "the command's text prefills the question")

	require.Empty(t, fake.pathCalls("response_url"), "nothing to tell the user privately")
}

// Slack hides the command in the agent pane, but a plain DM composer may
// still offer it: the command only opens channel conversations, and says so.
func TestSlashCommand_DMIsAnsweredPrivately(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	_, srv := newEventsAdapter(t, &stubGateway{}, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "D1", "U1", "hello", api.URL+"/response_url")

	fake.waitForPath(t, "response_url", 1)
	require.Contains(t, responseURLTexts(fake), "opens a conversation in a channel")
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
	require.Contains(t, responseURLTexts(fake), "not enabled in this channel")
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
	require.Contains(t, responseURLTexts(fake), "can't list the available agents")
	require.Empty(t, fake.pathCalls("views.open"))
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
// under the agent's identity with the conversation metadata, the submitter is
// the initiator, the thread is bound, and the question is the first turn.
// Replies then behave as in any conversation: the submitter's reply inherits
// the agent, a newcomer waits for the submitter's consent.
func TestAskAgentSubmission_OpensConversation(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	gw, resolved := capturingGateway()
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "C1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)

	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "why are pods crashlooping?")
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the submission dispatches the first turn")

	// The root: branded as the agent, naming who asked and quoting the question,
	// carrying the binding and the initiator as message metadata.
	var root recordedCall
	for _, c := range fake.pathCalls("chat.postMessage") {
		if _, ok := c.params["metadata"]; ok {
			root = c
			break
		}
	}
	require.NotNil(t, root.params, "a root message with metadata is posted")
	require.Equal(t, "C1", root.params["channel"])
	require.Nil(t, root.params["thread_ts"], "the root is a top-level message")
	require.Equal(t, "SRE Agent", root.params["username"], "posted under the agent's identity")
	rootText := root.params["text"].(string)
	require.Contains(t, rootText, "<@U1> asked *SRE Agent*")
	require.Contains(t, rootText, "> why are pods crashlooping?")
	meta := root.params["metadata"].(map[string]any)
	require.Equal(t, "klaus_gateway.agent_conversation", meta["event_type"])
	payload := meta["event_payload"].(map[string]any)
	require.Equal(t, "kagent/sre-agent", payload["agent_ref"])
	require.Equal(t, "U1", payload["initiator_user_id"])
	require.Equal(t, "slash_command", payload["entry_point"])
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Bringing in", "the root already names the agent; no launch intro")

	msgs := resolved()
	require.Equal(t, "kagent/sre-agent", msgs[0].AgentRef)
	require.Equal(t, "why are pods crashlooping?", msgs[0].Text, "the question is the turn, without decoration")
	require.Equal(t, "U1", msgs[0].Subject, "the turn runs as the submitter")
	require.Equal(t, "C1", msgs[0].ChannelID)
	require.NotEmpty(t, msgs[0].ThreadID)
	require.Equal(t, msgs[0].ThreadID, msgs[0].MessageID, "the bot root is the thread")
	rootTS := msgs[0].ThreadID

	// The submitter's reply inherits the agent without re-selecting.
	sendEvent(t, srv, mention("U1", "and the nodes?", "200.000", rootTS))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "the reply dispatches")
	require.Equal(t, "kagent/sre-agent", resolved()[1].AgentRef, "replies inherit the conversation's agent")

	// A newcomer is gated on the submitter's consent: nothing dispatches, the
	// submitter gets the consent prompt.
	sendEvent(t, srv, mention("U2", "me too", "300.000", rootTS))
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postEphemeral") {
			if c.params["user"] == "U1" {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond, "the consent prompt goes to the submitter, the initiator")
	require.Equal(t, 2, gw.resolveCount(), "the newcomer's message waits")
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
	require.Contains(t, responseURLTexts(fake), "I don't know an agent named `kagent/grill-master`")
	require.Empty(t, fake.pathCalls("chat.postMessage"), "no root is posted")
	require.Equal(t, 0, gw.resolveCount())
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
		if path == "chat.postMessage" && params["metadata"] != nil && !joined.Load() {
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

	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the retried root post opens the conversation")
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
	require.Contains(t, responseURLTexts(fake), "Invite me to the channel")
	require.Equal(t, 0, gw.resolveCount())
}

// After a restart the in-memory binding and initiator are gone. A bot-rooted
// conversation has no /agent prefix to re-derive from; its root metadata
// restores both: the reply reaches the picked agent (not the default), and the
// submitter — not the first human to reply — is the initiator.
func TestAskAgentRecovery_RootMetadataRestoresAgentAndInitiator(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	fake.setResponse("conversations.replies", `{"ok":true,"messages":[
		{"type":"message","user":"UBOT","bot_id":"B1","ts":"100.000","text":"💬 <@U1> asked *SRE Agent*:\n> hello",
		 "metadata":{"event_type":"klaus_gateway.agent_conversation","event_payload":{"agent_ref":"kagent/sre-agent","initiator_user_id":"U1","entry_point":"slash_command"}}},
		{"type":"message","user":"U2","ts":"150.000","text":"<@UBOT> what about me"}
	]}`)
	gw, resolved := capturingGateway()
	// A fresh adapter: nothing in memory about thread 100.000.
	_, srv := newEventsAdapter(t, gw, api.URL, channelMode, withSelection(pickerRoster(), pickerCards()))

	sendEvent(t, srv, mention("U1", "any update?", "200.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the submitter's reply dispatches")
	require.Equal(t, "kagent/sre-agent", resolved()[0].AgentRef, "the agent comes from the root metadata, not the default")

	// U2 replied earlier in the thread, but the metadata names U1 as initiator,
	// so U2 is a newcomer waiting on U1's consent.
	sendEvent(t, srv, mention("U2", "and me?", "300.000", "100.000"))
	require.Eventually(t, func() bool {
		for _, c := range fake.pathCalls("chat.postEphemeral") {
			if c.params["user"] == "U1" {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond, "the consent prompt goes to the initiator from the metadata")
	require.Equal(t, 1, gw.resolveCount(), "the newcomer's reply waits")
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
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 }, 2*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "agent_source=command")
	}, 2*time.Second, 50*time.Millisecond, "turn_dispatch records agent_source=command")
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
