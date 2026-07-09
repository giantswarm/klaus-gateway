package slack_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// Access-consent action IDs (mirrors the unexported constants in the adapter).
const (
	accessAllowAction = "access_allow"
	accessDenyAction  = "access_deny"
)

// mention builds an app_mention event; threadTS empty starts a new thread.
func mention(user, text, ts, threadTS string) string {
	if threadTS == "" {
		return fmt.Sprintf(`{"type":"event_callback","event":{"type":"app_mention","user":%q,"text":%q,"channel":"C1","ts":%q}}`, user, text, ts)
	}
	return fmt.Sprintf(`{"type":"event_callback","event":{"type":"app_mention","user":%q,"text":%q,"channel":"C1","ts":%q,"thread_ts":%q}}`, user, text, ts, threadTS)
}

// sendAccessInteraction posts a signed access-consent block_actions click. The
// button value encodes the thread and the newcomer, matching encodeAccessValue.
func sendAccessInteraction(t *testing.T, srv *httptest.Server, clicker, actionID, threadID, newcomer, responseURL string) {
	t.Helper()
	value, err := json.Marshal(map[string]any{"t": threadID, "u": newcomer})
	require.NoError(t, err)
	inner := map[string]any{
		"type":         "block_actions",
		"user":         map[string]any{"id": clicker},
		"channel":      map[string]any{"id": "C1"},
		"container":    map[string]any{"message_ts": "prompt.000"},
		"message":      map[string]any{"thread_ts": threadID},
		"response_url": responseURL,
		"actions":      []any{map[string]any{"action_id": actionID, "value": string(value)}},
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
}

// An unknown (unlinked) newcomer posting into a thread with an initiator is
// prompted to sign in, and their message never reaches the agent.
func TestAccess_UnlinkedNewcomerPromptedToSignIn(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "hi", Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.OBO = &fakeOBO{linkedUser: "U001", token: "tok"} // U001 linked, U999 not

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "initiator's mention dispatches")

	sendEvent(t, srv, mention("U999", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "Sign in to Giant Swarm")
	require.Equal(t, 1, gw.resolveCount(), "an unlinked newcomer must not reach the agent")
}

// A known newcomer is held pending the initiator's consent; on Yes their held
// message is replayed to the agent.
func TestAccess_NewcomerApprovedReplaysMessage(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	// OBO nil: the newcomer is authenticated, so the flow skips sign-in.
	_, srv := newEventsAdapter(t, gw, fakeURL)

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "initiator's mention dispatches")

	// Newcomer posts: initiator gets an ephemeral consent prompt, newcomer an ack.
	sendEvent(t, srv, mention("U999", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)
	ephemeral := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, ephemeral, "allowed to instruct the agent to work on your behalf")
	require.Contains(t, ephemeral, "waiting for the thread owner")
	require.Equal(t, 1, gw.resolveCount(), "held newcomer message must not reach the agent yet")

	// Initiator approves: the ephemeral prompt is updated and the message replays.
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U999", fakeURL+"/response")
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "approval replays the newcomer's message to the agent")
	require.Contains(t, allText(fake.pathCalls("response")), "allowed", "prompt updated to allowed via response_url")
}

func TestAccess_NewcomerDeclinedDropsMessage(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fakeURL)

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "initiator's mention dispatches")
	sendEvent(t, srv, mention("U999", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)

	sendAccessInteraction(t, srv, "U001", accessDenyAction, "100.000", "U999", fakeURL+"/response")
	fake.waitForPath(t, "response", 1)
	require.Contains(t, allText(fake.pathCalls("response")), "Declined")
	// Give any erroneous replay a chance to land, then confirm none did.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount(), "a declined newcomer's message must not reach the agent")
}

// Only the initiator may grant a newcomer. A click from anyone else (here the
// newcomer clicking their own consent button) is ignored: no grant, no replay.
func TestAccess_NonInitiatorCannotGrant(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fakeURL)

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "initiator's mention dispatches")
	sendEvent(t, srv, mention("U999", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)

	// The newcomer (not the initiator) clicks Allow on their own request.
	sendAccessInteraction(t, srv, "U999", accessAllowAction, "100.000", "U999", fakeURL+"/response")

	// Give any erroneous grant/replay a chance to land, then confirm none did.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount(), "a non-initiator must not be able to grant access")
	require.NotContains(t, allText(fake.pathCalls("response")), "allowed", "no grant confirmation for a non-initiator click")
}

// A tool-approval prompt is surfaced for human approval and does not auto-resume.
func TestHITL_ToolPromptSurfacedForApproval(t *testing.T) {
	fake := newFakeSlackAPI()
	prompt := channels.OutboundDelta{
		Kind:   channels.DeltaPrompt,
		TaskID: "task-1",
		Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"},
	}
	gw := &stubGateway{sendQueue: [][]channels.OutboundDelta{{prompt}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "clean up", "400.000"))

	fake.waitForPath(t, "chat.postMessage", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "Waiting for approval",
		"a tool prompt is surfaced for human approval")
	require.Equal(t, 1, gw.resolveCount(), "the prompt is not resumed without a human decision")
}
