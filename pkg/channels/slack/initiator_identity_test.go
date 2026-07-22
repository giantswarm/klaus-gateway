package slack_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// perUserOBO mints a distinct token per Slack user so a test can tell whose
// identity a turn ran under. Users in unlinked are treated as not linked.
type perUserOBO struct {
	tokens   map[string]string
	unlinked map[string]bool
}

func (o perUserOBO) TokenFor(_ context.Context, slackUserID string) (string, error) {
	if o.unlinked[slackUserID] {
		return "", musterlink.ErrNotLinked
	}
	if tok, ok := o.tokens[slackUserID]; ok {
		return tok, nil
	}
	return "", musterlink.ErrNotLinked
}

func (perUserOBO) LinkURL(string) string { return "https://gw.example/link" }
func (perUserOBO) Unlink(string)         {}

// A granted collaborator's turn runs under the thread initiator's token (one
// shared session), and the real author is attached as attribution. The
// initiator's own turn carries their own token and no attribution.
func TestInitiator_CollaboratorTurnForwardsInitiatorToken(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	fake.setResponse("users.info", `{"ok":true,"user":{"profile":{"email":"collaborator@example.com"}}}`)

	var mu sync.Mutex
	var msgs []channels.InboundMessage
	gw := &stubGateway{
		deltas:    []channels.OutboundDelta{{Content: "ok", Done: true}},
		onResolve: func(m channels.InboundMessage) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() },
	}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) { a.OBO = obo })

	// Initiator starts the thread; their turn runs under their own token.
	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 50*time.Millisecond, "initiator's mention dispatches")

	// Collaborator posts: held pending consent, then approved by the initiator.
	sendEvent(t, srv, mention("U002", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		10*time.Second, 50*time.Millisecond, "approval replays the collaborator's message")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, msgs, 2)

	initiatorTurn := msgs[0]
	require.Equal(t, "tok-initiator", initiatorTurn.BearerToken, "initiator's turn runs under their own token")
	require.Empty(t, initiatorTurn.Author, "initiator's own turn is not delegated")

	collaboratorTurn := msgs[1]
	require.Equal(t, "tok-initiator", collaboratorTurn.BearerToken,
		"collaborator's turn runs under the initiator's token, not the collaborator's")
	require.Equal(t, "collaborator@example.com", collaboratorTurn.Author,
		"the real author is attached as attribution")
}

// When the initiator's token cannot be minted (unlinked), a collaborator's turn
// falls back to the collaborator's own identity rather than the gateway SA.
func TestInitiator_FallsBackToSenderWhenTokenUnavailable(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	fake.setResponse("users.info", `{"ok":true,"user":{"profile":{"email":"collaborator@example.com"}}}`)

	var mu sync.Mutex
	var msgs []channels.InboundMessage
	gw := &stubGateway{
		deltas:    []channels.OutboundDelta{{Content: "ok", Done: true}},
		onResolve: func(m channels.InboundMessage) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() },
	}
	// U001 is the initiator but unlinked; U002 (collaborator) is linked.
	obo := perUserOBO{
		tokens:   map[string]string{"U002": "tok-collab"},
		unlinked: map[string]bool{"U001": true},
	}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) { a.OBO = obo })

	// Initiator's mention records them as initiator but parks for sign-in. Wait
	// for that prompt so U001 is the recorded initiator before U002 posts.
	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Sign in so I can act as you")
	}, 10*time.Second, 50*time.Millisecond, "the unlinked initiator is prompted to sign in")

	// Collaborator posts, held pending consent; the initiator approves.
	sendEvent(t, srv, mention("U002", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 },
		10*time.Second, 50*time.Millisecond, "the collaborator's message reaches the agent")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, msgs, 1)
	fallback := msgs[0]
	require.Equal(t, "tok-collab", fallback.BearerToken,
		"with the initiator unlinked, the turn falls back to the sender's own token")
	require.Empty(t, fallback.Author, "a non-delegated fallback turn carries no attribution")
}
