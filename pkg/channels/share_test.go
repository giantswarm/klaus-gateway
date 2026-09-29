package channels_test

import (
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	"github.com/giantswarm/klaus-gateway/pkg/seal"
)

const (
	ownerJWT        = "user-jwt"
	collaboratorJWT = "collaborator-jwt"
)

func completedTurn() []a2apkg.Event {
	return []a2apkg.Event{
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart("done")),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	}
}

// newSharingFacade is newA2AFacade with shares on.
func newSharingFacade(t *testing.T, agent *fakeAgent) (*channels.Facade, *memory.Store) {
	t.Helper()
	sealer, err := seal.Ephemeral()
	require.NoError(t, err)
	mem := memory.New()
	t.Cleanup(func() { require.NoError(t, mem.Close()) })
	return &channels.Facade{Agent: agent, Routes: mem, ThreadTTL: channels.DefaultThreadTTL, Sealer: sealer}, mem
}

const (
	initiatorID    = "U001"
	collaboratorID = "U002"
)

// initiatorMsg is the thread initiator's own turn.
func initiatorMsg(text string) channels.InboundMessage {
	msg := slackMsg(text)
	msg.SenderID = initiatorID
	return msg
}

// collaboratorMsg is a collaborator's turn on the initiator's instance, with
// the initiator's token as ownerToken ("" while they are signed out).
func collaboratorMsg(text, ownerToken string) channels.InboundMessage {
	msg := slackMsg(text)
	msg.SenderID = collaboratorID
	msg.BearerToken = collaboratorJWT
	msg.Collaborator = true
	msg.OwnerID = initiatorID
	msg.OwnerToken = ownerToken
	return msg
}

func threadRow(t *testing.T, routes store.Store) store.Entry {
	t.Helper()
	msg := slackMsg("")
	e, ok, err := routes.Get(t.Context(), store.Key{Channel: msg.Channel, ChannelID: msg.ChannelID, ThreadID: msg.ThreadID})
	require.NoError(t, err)
	require.True(t, ok)
	return e
}

func runTurn(t *testing.T, f *channels.Facade, msg channels.InboundMessage) {
	t.Helper()
	deltas, err := f.SendCompletion(t.Context(), msg)
	require.NoError(t, err)
	drain(t, deltas)
}

// A collaborator's turn runs under their own token and the thread's share,
// which the instance's creator minted; the row keeps the share sealed.
func TestFacade_CollaboratorTurnRunsAsTheCollaboratorThroughAShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	require.Empty(t, agent.shares, "the creator's own turn needs no share")

	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	require.Equal(t, collaboratorJWT, pkga2a.ForwardedTokenFromContext(agent.streamCtx), "the turn runs as the collaborator")
	share := pkga2a.ShareTokenFromContext(agent.streamCtx)
	require.NotEmpty(t, share)
	require.Equal(t, []string{ownerJWT}, agent.sharedAs, "only the instance's creator may share it")
	require.Equal(t, 1, agent.created, "the collaborator's turn stays on the creator's instance")

	row := threadRow(t, routes)
	require.NotNil(t, row.Share)
	require.Equal(t, row.AgentInstanceID, row.Share.InstanceID)
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	require.NotContains(t, string(raw), share, "the stored row holds the share sealed")
}

// The share outlives the creator's token: once minted, a collaborator's turn
// goes through it even when the creator's token cannot be minted any more.
func TestFacade_CollaboratorTurnReusesTheStoredShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, _ := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))
	first := pkga2a.ShareTokenFromContext(agent.streamCtx)

	runTurn(t, f, collaboratorMsg("again", ""))

	require.Equal(t, first, pkga2a.ShareTokenFromContext(agent.streamCtx))
	require.Len(t, agent.shares, 1, "the stored share is reused, not minted again")
}

// Without a share and without the creator's token the collaborator's turn is
// refused before it reaches the controller, which would refuse it anyway.
func TestFacade_CollaboratorTurnWithoutShareOrOwnerToken(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, _ := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	streamed := len(agent.streamed)

	_, err := f.SendCompletion(t.Context(), collaboratorMsg("join", ""))

	require.ErrorIs(t, err, channels.ErrShareUnavailable)
	require.Len(t, agent.streamed, streamed, "the refused turn is not sent")
	require.Empty(t, agent.sharedAs)
}

// A collaborator who opens the thread's binding while the initiator is signed
// out creates the instance under their own token: it is theirs, so their
// turns need no share, now or once the initiator is back.
func TestFacade_CollaboratorCreatedInstanceNeedsNoShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)

	runTurn(t, f, collaboratorMsg("open", ""))
	require.Equal(t, []string{collaboratorJWT}, agent.createdAs)
	require.Equal(t, collaboratorID, threadRow(t, routes).InstanceCreator)

	runTurn(t, f, collaboratorMsg("again", ownerJWT))

	require.Empty(t, agent.sharedAs, "no share is minted of the collaborator's own instance")
	require.Empty(t, pkga2a.ShareTokenFromContext(agent.streamCtx))
}

// Two collaborator turns that mint a share at once keep one: the turn that
// finds the other's share stored uses it and revokes its own.
func TestFacade_ConcurrentShareMintKeepsOne(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	agent.onShare = func() { runTurn(t, f, collaboratorMsg("other", ownerJWT)) }

	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	require.Equal(t, []string{"share-1", "share-2"}, agent.shares)
	require.Equal(t, []string{"share-1"}, agent.revoked, "the share that lost the write is revoked")
	require.Equal(t, "share-2", threadRow(t, routes).Share.ID)
	require.Contains(t, pkga2a.ShareTokenFromContext(agent.streamCtx), "share-2")
}

// A share minted for an instance the thread left meanwhile is revoked at once,
// not stored or used.
func TestFacade_ShareOfALeftInstanceIsRevoked(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	msg := slackMsg("")
	key := store.Key{Channel: msg.Channel, ChannelID: msg.ChannelID, ThreadID: msg.ThreadID}
	agent.onShare = func() {
		require.NoError(t, routes.Update(t.Context(), key, func(e *store.Entry, _ bool) bool {
			e.AgentInstanceID = "elsewhere"
			return true
		}))
	}

	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	require.Equal(t, []string{"share-1"}, agent.revoked)
	require.Nil(t, threadRow(t, routes).Share)
	require.Empty(t, pkga2a.ShareTokenFromContext(agent.streamCtx))
}

// With shares off, a collaborator's turn carries no share.
func TestFacade_CollaboratorTurnWithSharesOff(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, _ := newA2AFacade(agent)
	runTurn(t, f, initiatorMsg("open"))

	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	require.Empty(t, pkga2a.ShareTokenFromContext(agent.streamCtx))
	require.Empty(t, agent.sharedAs)
}

// A collaborator's turn that opens the binding creates the instance under its
// creator's token, so the thread's conversation stays theirs.
func TestFacade_CollaboratorTurnCreatesTheInstanceAsItsCreator(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, _ := newSharingFacade(t, agent)

	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	require.Equal(t, []string{ownerJWT}, agent.createdAs)
	require.Equal(t, collaboratorJWT, pkga2a.ForwardedTokenFromContext(agent.streamCtx))
	require.NotEmpty(t, pkga2a.ShareTokenFromContext(agent.streamCtx))
}

// A share the row holds that does not open under this process's key (another
// replica's, a rotated key) is replaced, and the old one revoked.
func TestFacade_UnopenableShareIsReplaced(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))
	old := threadRow(t, routes).Share.ID

	other, err := seal.Ephemeral()
	require.NoError(t, err)
	f.Sealer = other
	runTurn(t, f, collaboratorMsg("again", ownerJWT))

	require.Len(t, agent.shares, 2)
	require.Equal(t, []string{old}, agent.revoked)
	require.NotEqual(t, old, threadRow(t, routes).Share.ID)
}

// A rebind to another agent leaves the old instance behind, and its share is
// revoked.
func TestFacade_RebindRevokesTheOldShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))
	old := threadRow(t, routes).Share.ID

	msg := initiatorMsg("other agent")
	msg.AgentRef = "kagent/other"
	runTurn(t, f, msg)

	require.Equal(t, []string{old}, agent.revoked)
	require.Nil(t, threadRow(t, routes).Share)
}

// A reset deletes the instance and its share with it; the row forgets both.
func TestFacade_ResetSessionDropsTheShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	reset, err := f.ResetSession(t.Context(), collaboratorMsg("reset", ownerJWT))
	require.NoError(t, err)
	require.True(t, reset)
	require.Len(t, agent.deleted, 1)
	require.Nil(t, threadRow(t, routes).Share)
}

// The next turn after the conversation ended revokes the share of the
// instance the thread no longer uses.
func TestFacade_ClosedThreadRevokesItsShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent := newFakeAgent(completedTurn()...)
		f, routes := newSharingFacade(t, agent)
		runTurn(t, f, initiatorMsg("open"))
		runTurn(t, f, collaboratorMsg("join", ownerJWT))
		old := threadRow(t, routes).Share.ID

		time.Sleep(channels.DefaultThreadTTL + time.Hour)
		runTurn(t, f, initiatorMsg("back"))

		require.Equal(t, []string{old}, agent.revoked)
		require.Nil(t, threadRow(t, routes).Share)
	})
}

// A collaborator without the creator's token or a share cannot see the
// instance, so the resume check is indeterminate: the controller is not asked,
// and the initiator's binding stays.
func TestFacade_SessionResumableWithoutShareOrOwnerTokenIsIndeterminate(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	bound := threadRow(t, routes).AgentInstanceID

	exists, checked := f.SessionResumable(t.Context(), collaboratorMsg("join", ""))

	require.False(t, checked)
	require.False(t, exists)
	require.Zero(t, agent.gotIns, "the controller is not asked")
	require.Equal(t, bound, threadRow(t, routes).AgentInstanceID, "the binding stays")
}

// With the thread's share, the collaborator's resume check reads the instance
// through it.
func TestFacade_SessionResumableThroughTheStoredShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, _ := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	exists, checked := f.SessionResumable(t.Context(), collaboratorMsg("again", ""))

	require.True(t, checked)
	require.True(t, exists)
	require.Equal(t, 1, agent.gotIns)
}

// A reset from a collaborator without the creator's token or a share is
// refused: it would delete nothing at the controller.
func TestFacade_ResetSessionWithoutShareOrOwnerTokenIsRefused(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	bound := threadRow(t, routes).AgentInstanceID

	reset, err := f.ResetSession(t.Context(), collaboratorMsg("reset", ""))

	require.ErrorIs(t, err, channels.ErrShareUnavailable)
	require.False(t, reset)
	require.Empty(t, agent.deleted)
	require.Equal(t, bound, threadRow(t, routes).AgentInstanceID)
}

// A share is revoked only under its instance creator's token: a collaborator
// who rebinds the thread while the initiator is signed out leaves the old
// share alone, and the new instance is recorded as theirs.
func TestFacade_RebindWithoutTheCreatorsTokenKeepsTheOldShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	msg := collaboratorMsg("other agent", "")
	msg.AgentRef = "kagent/other"
	runTurn(t, f, msg)

	require.Empty(t, agent.revoked, "the collaborator cannot revoke the initiator's share")
	row := threadRow(t, routes)
	require.Nil(t, row.Share)
	require.Equal(t, collaboratorID, row.InstanceCreator)
}

// The instance's creator is recorded: the initiator for their own turn, and
// the initiator too for a collaborator turn that creates it under the
// initiator's token.
func TestFacade_InstanceCreatorIsRecorded(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	require.Equal(t, initiatorID, threadRow(t, routes).InstanceCreator)

	msg := collaboratorMsg("other agent", ownerJWT)
	msg.AgentRef = "kagent/other"
	runTurn(t, f, msg)
	require.Equal(t, initiatorID, threadRow(t, routes).InstanceCreator)
}

// A share another replica stored meanwhile, which this process cannot open,
// is overwritten and revoked, so it does not stay valid unreferenced.
func TestFacade_OverwrittenUnopenableShareIsRevoked(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, initiatorMsg("open"))
	msg := slackMsg("")
	key := store.Key{Channel: msg.Channel, ChannelID: msg.ChannelID, ThreadID: msg.ThreadID}
	agent.onShare = func() {
		require.NoError(t, routes.Update(t.Context(), key, func(e *store.Entry, _ bool) bool {
			e.Share = &store.Share{ID: "replica-share", InstanceID: e.AgentInstanceID, Sealed: []byte("sealed elsewhere")}
			return true
		}))
	}

	runTurn(t, f, collaboratorMsg("join", ownerJWT))

	require.Equal(t, []string{"replica-share"}, agent.revoked)
	require.Equal(t, "share-1", threadRow(t, routes).Share.ID)
}

// Someone else who mentions the bot after the conversation ended starts it
// over under their own token, which cannot revoke the old share.
func TestFacade_ClosedThreadReopenedBySomeoneElseKeepsTheOldShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent := newFakeAgent(completedTurn()...)
		f, routes := newSharingFacade(t, agent)
		runTurn(t, f, initiatorMsg("open"))
		runTurn(t, f, collaboratorMsg("join", ownerJWT))

		time.Sleep(channels.DefaultThreadTTL + time.Hour)
		msg := slackMsg("back")
		msg.SenderID, msg.BearerToken = "U003", "other-jwt"
		runTurn(t, f, msg)

		require.Empty(t, agent.revoked)
		row := threadRow(t, routes)
		require.Nil(t, row.Share)
		require.Equal(t, "U003", row.InstanceCreator)
	})
}
