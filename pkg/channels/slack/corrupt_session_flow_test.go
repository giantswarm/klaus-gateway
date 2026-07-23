package slack_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// errCorruptHistory mimics the model API rejection that kagent relays when an
// interrupted turn left the session with a tool_use and no tool_result.
var errCorruptHistory = errors.New("a2a error -32603: Error code: 400 - {'type': 'error', 'error': {'type': 'invalid_request_error', 'message': 'messages.90: `tool_use` ids were found without `tool_result` blocks immediately after: toolu_01X. Each `tool_use` block must have a corresponding `tool_result` block in the next message.'}}")

// A turn failing on corrupt session history triggers a session reset and tells
// the user to resend, instead of leaving the thread failing forever.
func TestCorruptSession_ResetAndNotice(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var resetMsgs []channels.InboundMessage
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Err: errCorruptHistory}},
		onResetSession: func(msg channels.InboundMessage) (bool, error) {
			mu.Lock()
			resetMsgs = append(resetMsgs, msg)
			mu.Unlock()
			return true, nil
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "list my epics", "700.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "reset the session")
	}, 10*time.Second, 50*time.Millisecond, "reset notice posted")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, resetMsgs, 1, "the corrupt session is deleted once")
	require.Equal(t, "700.000", resetMsgs[0].ThreadID)
}

// A granted collaborator's turn on a shared thread runs under the initiator's
// token; when it fails on corrupt session history, the reset must present that
// same token, or kagent resolves a different principal and the delete misses
// the corrupt session.
func TestCorruptSession_CollaboratorTurnResetsUnderInitiatorToken(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	var mu sync.Mutex
	var resets []channels.InboundMessage
	gw := &stubGateway{
		// First send: the initiator's opening turn succeeds. Second send: the
		// collaborator's turn fails on corrupt history.
		sendQueue: [][]channels.OutboundDelta{
			{{Done: true}},
			{{Err: errCorruptHistory}},
		},
		onResetSession: func(msg channels.InboundMessage) (bool, error) {
			mu.Lock()
			resets = append(resets, msg)
			mu.Unlock()
			return true, nil
		},
	}
	a, srv := newEventsAdapter(t, gw, fakeURL, channelMode)
	a.OBO = &multiUserOBO{linked: map[string]string{"U001": "tok1", "U999": "tok2"}}

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool {
		names := fake.reactionNames("reactions.add")
		return len(names) > 0 && names[len(names)-1] == "white_check_mark"
	}, 2*time.Second, 50*time.Millisecond, "the initiator's turn completes and frees the thread slot")

	// Grant U999 so their reply dispatches instead of parking for consent.
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U999", fakeURL+"/response")
	fake.waitForPath(t, "response", 1)

	sendEvent(t, srv, mention("U999", "continue", "200.000", "100.000"))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(resets) == 1
	}, 2*time.Second, 50*time.Millisecond, "the corrupt session is deleted")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "100.000", resets[0].ThreadID)
	require.Equal(t, "tok1", resets[0].BearerToken,
		"the reset must present the initiator's token the turn ran under, not the collaborator's")
}

// When the session cannot be deleted, the notice advises a new thread rather
// than promising a reset that did not happen.
func TestCorruptSession_ResetUnavailableAdvisesNewThread(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Err: errCorruptHistory}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "list my epics", "701.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "start a new thread")
	}, 10*time.Second, 50*time.Millisecond, "stuck notice posted")
}

// An ordinary turn failure must not delete the session.
func TestCorruptSession_OtherErrorsDoNotReset(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	resets := 0
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Err: errors.New("a2a error -32603: SSE stream error: context deadline exceeded")}},
		onResetSession: func(channels.InboundMessage) (bool, error) {
			mu.Lock()
			resets++
			mu.Unlock()
			return true, nil
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "list my epics", "702.000"))
	require.Eventually(t, func() bool {
		names := fake.reactionNames("reactions.add")
		return len(names) > 0 && names[len(names)-1] == "x"
	}, 10*time.Second, 50*time.Millisecond, "turn signalled as failed")

	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, resets, "a non-corrupt failure must not delete the session")
}

// An error that merely quotes agent output mentioning tool_use/tool_result
// (no invalid_request_error class) must not trigger the recovery, which
// irreversibly deletes the session.
func TestCorruptSession_QuotedToolWordsDoNotReset(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	resets := 0
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Err: errors.New(`a2a error -32603: upstream failed while agent output discussed "tool_use blocks and tool_result pairing"`)}},
		onResetSession: func(channels.InboundMessage) (bool, error) {
			mu.Lock()
			resets++
			mu.Unlock()
			return true, nil
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "explain tool pairing", "703.000"))
	require.Eventually(t, func() bool {
		names := fake.reactionNames("reactions.add")
		return len(names) > 0 && names[len(names)-1] == "x"
	}, 10*time.Second, 50*time.Millisecond, "turn signalled as failed")

	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, resets, "quoted tool_use/tool_result wording alone must not delete the session")
}
