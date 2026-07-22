package slack_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// A parked message that is nothing but a sign-in request ("login", "sign in",
// ...) was satisfied by the link completing; replaying it would just confuse
// the agent. A message that merely contains an auth keyword still replays.
func TestLoginReplay_DropsBareAuthUtterances(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onResolve: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	obo := &fakeOBO{linkedUser: "U123", token: "tok", notYetLinked: true}
	a.OBO = obo

	// Both messages are parked while U123 is unlinked.
	sendEvent(t, srv, mention("U123", "login", "100.000", ""))
	fake.waitForPath(t, "chat.postMessage", 1)
	sendEvent(t, srv, mention("U123", "login to grafana fails", "101.000", "100.000"))
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, gw.resolveCount(), "unlinked messages must be parked, not dispatched")

	obo.completeLink()
	a.OnUserLinked(t.Context(), "U123", "u123@example.com")

	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 50*time.Millisecond, "the real question replays after linking")
	// Give an erroneous replay of the bare "login" a chance to land.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount(), "the bare sign-in request must not replay")
	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, captured[0].Text, "login to grafana fails")
}

// A parked message whose post-sign-in replay fails must not leave the user in
// silence: the thread gets a failure note asking them to resend.
func TestLoginReplay_FailurePostsNote(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	obo := &fakeOBO{linkedUser: "U123", token: "tok", notYetLinked: true}
	a.OBO = obo

	sendEvent(t, srv, mention("U123", "what failed on prod?", "100.000", ""))
	fake.waitForPath(t, "chat.postMessage", 1)
	require.Zero(t, gw.resolveCount(), "the unlinked message must be parked")

	gw.mu.Lock()
	gw.resolveErr = errors.New("kagent unreachable")
	gw.mu.Unlock()
	obo.completeLink()
	a.OnUserLinked(t.Context(), "U123", "u123@example.com")

	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "couldn't pick your message back up")
	}, 10*time.Second, 50*time.Millisecond, "a failed replay must post a failure note in-thread")
}

// A newcomer who parks several messages before the initiator's grant has all of
// them replayed in order on approval, not just the last one.
func TestAccess_MultipleParkedMessagesReplayInOrder(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "ok", Done: true}},
		onResolve: func(msg channels.InboundMessage) {
			mu.Lock()
			captured = append(captured, msg)
			mu.Unlock()
		},
	}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode)

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 50*time.Millisecond, "initiator's mention dispatches")

	// The newcomer sends two messages before being granted: one consent prompt
	// (plus one waiting-ack), both messages held.
	sendEvent(t, srv, mention("U999", "first ask", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)
	sendEvent(t, srv, mention("U999", "second ask", "201.000", "100.000"))
	time.Sleep(150 * time.Millisecond)
	require.Len(t, fake.pathCalls("chat.postEphemeral"), 2,
		"a second parked message must not re-prompt the initiator")
	require.Equal(t, 1, gw.resolveCount(), "held newcomer messages must not reach the agent yet")

	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U999", fakeURL+"/response")
	require.Eventually(t, func() bool { return gw.resolveCount() == 3 },
		10*time.Second, 50*time.Millisecond, "both parked messages replay on grant")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "first ask", captured[1].Text, "replay preserves arrival order")
	require.Equal(t, "second ask", captured[2].Text, "replay preserves arrival order")
}

// A login replay that lands while another turn holds the thread slot is not
// dropped and posts no busy notice: it waits for the slot and runs after the
// in-flight turn releases it.
func TestLoginReplay_WaitsForBusyThread(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onResolve: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}
	a, srv := newEventsAdapter(t, gw, fakeURL, channelMode)
	obo := &multiUserOBO{linked: map[string]string{"U001": "tok1"}}
	a.OBO = obo

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 50*time.Millisecond, "initiator's mention dispatches")

	// Grant U999 up front (a grant with nothing parked still stands), so their
	// later message reaches the login-park path rather than the consent prompt.
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U999", fakeURL+"/response")
	fake.waitForPath(t, "response", 1)

	// U999 (granted, unlinked) posts while the thread is idle: parked for login.
	sendEvent(t, srv, mention("U999", "help me out", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Sign in so I can act as you")
	}, 10*time.Second, 50*time.Millisecond, "the unlinked granted user is prompted to sign in")
	require.Equal(t, 1, gw.resolveCount(), "the unlinked message is parked, not dispatched")

	// The initiator starts a turn that keeps the thread slot held.
	hold := make(chan struct{})
	gw.mu.Lock()
	gw.hold = hold
	gw.mu.Unlock()
	sendEvent(t, srv, mention("U001", "long task", "300.000", "100.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		10*time.Second, 50*time.Millisecond, "the holding turn starts")

	// U999's link completes mid-turn: the replay must wait, silently.
	obo.link("U999", "tok2")
	a.OnUserLinked(t.Context(), "U999", "u999@example.com")
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 2, gw.resolveCount(), "the replay must wait for the running turn")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "still finishing",
		"a deferred login replay must not post the busy notice")

	close(hold)
	require.Eventually(t, func() bool { return gw.resolveCount() == 3 },
		10*time.Second, 50*time.Millisecond, "the replay is delivered once the slot frees")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "help me out", captured[2].Text)
	require.Equal(t, "tok2", captured[2].BearerToken, "the replayed turn carries the newly linked token")
}

// raceLinkOBO reports the user unlinked exactly once and linked from then on,
// simulating a link callback that completes between dispatch's TokenFor miss
// and the park.
type raceLinkOBO struct {
	mu     sync.Mutex
	checks int
	token  string
}

func (o *raceLinkOBO) TokenFor(context.Context, string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.checks++
	if o.checks == 1 {
		return "", musterlink.ErrNotLinked
	}
	return o.token, nil
}

func (o *raceLinkOBO) LinkURL(string) string { return "https://gw.example.com/link" }
func (o *raceLinkOBO) Unlink(string)         {}

// A link that completes between the TokenFor miss and the park must not strand
// the parked message until the TTL sweep: the post-park re-check drains it
// immediately, and no sign-in prompt is posted for an already-linked user.
func TestLoginReplay_ParkAfterLinkRaceDrainsImmediately(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{onResolve: func(msg channels.InboundMessage) {
		mu.Lock()
		captured = append(captured, msg)
		mu.Unlock()
	}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.OBO = &raceLinkOBO{token: "raced-token"}

	sendEvent(t, srv, dmEvent("U1", "what is broken?", "700.000"))

	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 50*time.Millisecond, "the raced message drains without waiting for a link callback")
	mu.Lock()
	got := captured[0]
	mu.Unlock()
	require.Contains(t, got.Text, "what is broken?")
	require.Equal(t, "raced-token", got.BearerToken)
	for _, call := range fake.pathCalls("chat.postMessage") {
		text, _ := call.params["text"].(string)
		require.NotContains(t, strings.ToLower(text), "sign in",
			"no sign-in prompt for a user who turned out to be linked")
	}
}

// A burst of parked messages nudges the user to sign in once, not once per
// message.
func TestSignInPrompt_ThrottledPerThreadUser(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	a.OBO = &fakeOBO{linkedUser: "U123", token: "tok", notYetLinked: true}

	sendEvent(t, srv, mention("U123", "first question", "100.000", ""))
	fake.waitForPath(t, "chat.postMessage", 1)
	// The prompt is a real threaded reply anchoring the mention's thread.
	prompt := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "100.000", prompt.params["thread_ts"])
	sendEvent(t, srv, mention("U123", "second question", "101.000", "100.000"))

	time.Sleep(200 * time.Millisecond)
	require.Len(t, fake.pathCalls("chat.postMessage"), 1,
		"a second parked message within the window must not re-prompt")
	require.Zero(t, gw.resolveCount(), "both messages stay parked")
}
