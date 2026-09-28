package slack_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
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
func (perUserOBO) Unlink(string) error   { return nil }

// A granted collaborator's turn runs under their own token, marked as a
// collaborator's with the initiator's token as the instance owner's, and the
// author is attached as attribution. The initiator's own turn carries their own
// token, no owner token and no attribution.
func TestInitiator_CollaboratorTurnRunsAsTheCollaborator(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	fake.setResponse("users.info", `{"ok":true,"user":{"profile":{"email":"collaborator@example.com"}}}`)

	var mu sync.Mutex
	var msgs []channels.InboundMessage
	gw := &stubGateway{
		deltas:     []channels.OutboundDelta{{Content: "ok", Done: true}},
		onDispatch: func(m channels.InboundMessage) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() },
	}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) { a.OBO = obo })

	// Initiator starts the thread; their turn runs under their own token.
	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "initiator's mention dispatches")

	// Collaborator posts: held pending consent, then approved by the initiator.
	sendEvent(t, srv, mention("U002", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	// The click is acknowledged before the grant is written; the prompt rewrite
	// that follows the grant is the proof it landed.
	fake.waitForPath(t, "response", 1)
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 50*time.Millisecond, "approval replays the collaborator's message")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, msgs, 2)

	initiatorTurn := msgs[0]
	require.Equal(t, "tok-initiator", initiatorTurn.BearerToken, "initiator's turn runs under their own token")
	require.Empty(t, initiatorTurn.Author, "initiator's own turn needs no attribution")
	require.False(t, initiatorTurn.Collaborator)
	require.Empty(t, initiatorTurn.OwnerToken)

	collaboratorTurn := msgs[1]
	require.Equal(t, "tok-collab", collaboratorTurn.BearerToken,
		"collaborator's turn runs under their own token")
	require.True(t, collaboratorTurn.Collaborator)
	require.Equal(t, "tok-initiator", collaboratorTurn.OwnerToken,
		"the initiator's token rides along for the instance they created")
	require.Equal(t, "collaborator@example.com", collaboratorTurn.Author,
		"the author is attached as attribution")
}

// When the initiator's token cannot be minted (unlinked), a collaborator's turn
// still runs as the collaborator, without an owner token: it relies on the
// share the thread already holds.
func TestInitiator_FallsBackToSenderWhenTokenUnavailable(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	fake.setResponse("users.info", `{"ok":true,"user":{"profile":{"email":"collaborator@example.com"}}}`)

	var mu sync.Mutex
	var msgs []channels.InboundMessage
	gw := &stubGateway{
		deltas:     []channels.OutboundDelta{{Content: "ok", Done: true}},
		onDispatch: func(m channels.InboundMessage) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() },
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
		return signInPrompted(fake)
	}, flowWait, 50*time.Millisecond, "the unlinked initiator is prompted to sign in")

	// Collaborator posts, held pending consent; the initiator approves.
	sendEvent(t, srv, mention("U002", "help", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	// The click is acknowledged before the grant is written; the prompt rewrite
	// that follows the grant is the proof it landed.
	fake.waitForPath(t, "response", 1)
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the collaborator's message reaches the agent")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, msgs, 1)
	fallback := msgs[0]
	require.Equal(t, "tok-collab", fallback.BearerToken, "the turn runs under the sender's own token")
	require.True(t, fallback.Collaborator)
	require.Empty(t, fallback.OwnerToken, "the unlinked initiator has no token to lend")
	require.Equal(t, "collaborator@example.com", fallback.Author)
}

// A collaborator's turn a restart cut short resubscribes the way it ran: under
// the collaborator's own token, marked as a collaborator's, with the
// initiator's token as the instance owner's.
func TestInitiator_RecoveredCollaboratorTurnRunsAsTheCollaborator(t *testing.T) {
	fake := newFakeSlackAPI()
	records := slackadapter.NewMemoryRecorder()
	require.NoError(t, records.UpdateThreadRecord(t.Context(), "slack", "D1", "700.000", func(e *store.Entry, _ bool) bool {
		e.Initiator, e.Granted = "U001", []string{"U002"}
		return true
	}))
	turn := leftoverTurn("task-9")
	turn.Msg.Resume = map[string]string{"slack_user": "U002", "message_ts": "700.000"}
	gw := &stubGateway{records: records, resumes: &stubResumes{
		durable: true,
		turns:   []channels.InFlightTurn{turn},
		deltas:  map[string][]channels.OutboundDelta{"task-9": {{Content: "done"}, {Done: true}}},
	}}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	a, _ := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) { a.OBO = obo })

	a.RecoverTurns()

	require.Eventually(t, func() bool {
		gw.mu.Lock()
		defer gw.mu.Unlock()
		return len(gw.resumes.resumed) == 1
	}, flowWait, 20*time.Millisecond, "the turn is resubscribed")
	gw.mu.Lock()
	defer gw.mu.Unlock()
	resumed := gw.resumes.resumed[0]
	require.Equal(t, "tok-collab", resumed.BearerToken)
	require.True(t, resumed.Collaborator)
	require.Equal(t, "tok-initiator", resumed.OwnerToken)
}
