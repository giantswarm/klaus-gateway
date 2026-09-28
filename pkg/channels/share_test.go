package channels_test

import (
	"encoding/json"
	"testing"
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
	return &channels.Facade{Agent: agent, Routes: mem, ThreadTTL: channels.DefaultThreadTTL, Sealer: sealer}, mem
}

func collaboratorMsg(text, ownerToken string) channels.InboundMessage {
	msg := slackMsg(text)
	msg.BearerToken = collaboratorJWT
	msg.Collaborator = true
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
	runTurn(t, f, slackMsg("open"))
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
	runTurn(t, f, slackMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))
	first := pkga2a.ShareTokenFromContext(agent.streamCtx)

	runTurn(t, f, collaboratorMsg("again", ""))

	require.Equal(t, first, pkga2a.ShareTokenFromContext(agent.streamCtx))
	require.Len(t, agent.shares, 1, "the stored share is reused, not minted again")
}

// Without a share and without the creator's token the turn goes out under the
// collaborator's token alone.
func TestFacade_CollaboratorTurnWithoutShareOrOwnerToken(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, _ := newSharingFacade(t, agent)
	runTurn(t, f, slackMsg("open"))

	runTurn(t, f, collaboratorMsg("join", ""))

	require.Empty(t, pkga2a.ShareTokenFromContext(agent.streamCtx))
	require.Empty(t, agent.sharedAs)
}

// With shares off, a collaborator's turn carries no share.
func TestFacade_CollaboratorTurnWithSharesOff(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, _ := newA2AFacade(agent)
	runTurn(t, f, slackMsg("open"))

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
	runTurn(t, f, slackMsg("open"))
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
	runTurn(t, f, slackMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))
	old := threadRow(t, routes).Share.ID

	msg := slackMsg("other agent")
	msg.AgentRef = "kagent/other"
	runTurn(t, f, msg)

	require.Equal(t, []string{old}, agent.revoked)
	require.Nil(t, threadRow(t, routes).Share)
}

// A reset deletes the instance and its share with it; the row forgets both.
func TestFacade_ResetSessionDropsTheShare(t *testing.T) {
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	runTurn(t, f, slackMsg("open"))
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
	agent := newFakeAgent(completedTurn()...)
	f, routes := newSharingFacade(t, agent)
	now := time.Now()
	clock := func() time.Time { return now }
	f.SetNowFunc(clock)
	routes.SetNowFunc(clock)
	runTurn(t, f, slackMsg("open"))
	runTurn(t, f, collaboratorMsg("join", ownerJWT))
	old := threadRow(t, routes).Share.ID

	now = now.Add(channels.DefaultThreadTTL + time.Hour)
	runTurn(t, f, slackMsg("back"))

	require.Equal(t, []string{old}, agent.revoked)
	require.Nil(t, threadRow(t, routes).Share)
}
