package slack_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	a.OBO = &fakeOBO{linkedUser: "U001", token: "tok"} // U001 linked, U999 not

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "initiator's mention dispatches")

	sendEvent(t, srv, mention("U999", "help", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Sign in to Giant Swarm")
	}, 2*time.Second, 50*time.Millisecond, "the newcomer is prompted to sign in")
	require.Equal(t, 1, gw.resolveCount(), "an unlinked newcomer must not reach the agent")
}

// A known newcomer is held pending the initiator's consent; on Yes their held
// message is replayed to the agent.
func TestAccess_NewcomerApprovedReplaysMessage(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	// OBO nil: the newcomer is authenticated, so the flow skips sign-in.
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode)

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
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode)

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
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode)

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

// A grant clicked while a turn holds the thread slot must not lose the parked
// message: the replay is deferred and delivered once the running turn releases
// the slot.
func TestAccess_GrantWhileThreadBusyDeliversAfterRelease(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	hold := make(chan struct{})
	gw := &stubGateway{hold: hold}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode)

	// The initiator's mention starts a turn that keeps the thread slot held.
	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "initiator's mention dispatches")

	// Newcomer posts and is parked pending consent.
	sendEvent(t, srv, mention("U999", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)

	// The initiator grants while the turn is still in flight: the replay must be
	// deferred, not rejected with the busy notice.
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U999", fakeURL+"/response")
	fake.waitForPath(t, "response", 1)
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount(), "replay must wait for the running turn")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "still finishing",
		"a deferred replay must not post the busy notice")

	// The running turn finishes; the deferred replay is delivered.
	close(hold)
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "the parked message is delivered once the slot frees")
}

// After a restart the access policy is empty; a reply into a pre-existing
// thread must not let the replier take the thread over. The initiator is seeded
// from the thread root's author, so the replier is treated as a newcomer.
func TestAccess_RestartSeedsInitiatorFromThreadRoot(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setResponse("conversations.replies", `{"ok":true,"messages":[{"user":"U001","ts":"100.000"}]}`)
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	// Fresh process: the first event it ever sees is a reply into an existing
	// thread rooted by U001.
	sendEvent(t, srv, mention("U999", "take over", "300.000", "100.000"))

	fake.waitForPath(t, "conversations.replies", 1)
	replies := fake.pathCalls("conversations.replies")[0].params
	require.Equal(t, "C1", replies["channel"])
	require.Equal(t, "100.000", replies["ts"])
	require.Equal(t, "50", replies["limit"])

	// U999 is gated as a newcomer: consent prompt to the root author, no dispatch.
	fake.waitForPath(t, "chat.postEphemeral", 2)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "allowed to instruct the agent")
	var promptedUsers []string
	for _, call := range fake.pathCalls("chat.postEphemeral") {
		if user, ok := call.params["user"].(string); ok {
			promptedUsers = append(promptedUsers, user)
		}
	}
	require.Contains(t, promptedUsers, "U001", "the consent prompt goes to the thread root author")
	require.Zero(t, gw.resolveCount(), "the replier must not take over the thread as initiator")
}

// When the thread root author cannot be fetched, seeding falls back to the
// current first-poster behavior instead of blocking the thread.
func TestAccess_RootAuthorLookupFailureFallsBackToFirstPoster(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("conversations.replies", "channel_not_found")
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U999", "hello", "300.000", "100.000"))

	fake.waitForPath(t, "conversations.replies", 1)
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "on lookup failure the first poster becomes initiator and dispatches")
}

// A 1:1 DM has a single human, so the root-author reseed must be skipped
// entirely: a reply into a pre-existing DM thread dispatches as the sole human
// rather than being gated against whatever authored the thread root (commonly a
// bot message, which would otherwise lock the human out of their own DM).
func TestAccess_DMReplySkipsRootAuthorReseed(t *testing.T) {
	fake := newFakeSlackAPI()
	// A different author on the root: were the reseed to run in a DM it would
	// seed U999 and gate U1. It must not run at all.
	fake.setResponse("conversations.replies", `{"ok":true,"messages":[{"user":"U999","ts":"100.000"}]}`)
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmThreadEvent("U1", "hello", "300.000", "100.000"))

	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "the sole human in a DM dispatches without a consent gate")
	require.Empty(t, fake.pathCalls("conversations.replies"),
		"a DM has one human; the root-author reseed must be skipped")
}

// A bot-authored thread root is not a human initiator, so seeding falls back to
// first-poster instead of installing the bot (which no human could then get
// consent from).
func TestAccess_BotAuthoredRootFallsBackToFirstPoster(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setResponse("conversations.replies", `{"ok":true,"messages":[{"bot_id":"B001","user":"UBOT","ts":"100.000"}]}`)
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U999", "hello", "300.000", "100.000"))

	fake.waitForPath(t, "conversations.replies", 1)
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		2*time.Second, 50*time.Millisecond, "a bot-authored root is not a human initiator; the first poster dispatches")
}

// A transient token-mint failure for a newcomer is surfaced immediately instead
// of parking the message and deferring the error to a post-consent replay.
func TestAccess_NewcomerTransientTokenErrorSurfaced(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	// U999 is linked but minting fails transiently; U001 (the initiator) is
	// unlinked, so their own turn aborts at the sign-in prompt after they are
	// recorded as initiator.
	a.OBO = &fakeOBO{linkedUser: "U999", token: "tok", tokenErr: errors.New("refresh failed")}

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Sign in to Giant Swarm")
	}, 2*time.Second, 50*time.Millisecond, "the unlinked initiator is prompted to sign in")

	sendEvent(t, srv, mention("U999", "help", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "couldn't refresh your Giant Swarm sign-in")
	}, 2*time.Second, 50*time.Millisecond, "the token error is surfaced to the newcomer")
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), "waiting for the thread owner",
		"a failing newcomer must not be parked pending consent")
	require.Zero(t, gw.resolveCount(), "no message reaches the agent")
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
