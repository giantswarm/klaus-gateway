package channels_test

import (
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	boltstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/bolt"
	valkeystore "github.com/giantswarm/klaus-gateway/pkg/routing/store/valkey"
)

var (
	threadKey = store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}
	klausDev  = &store.WorkspaceChoice{Namespace: "kagent", Name: "klaus-dev"}
)

func withWorkspace(msg channels.InboundMessage, ws *store.WorkspaceChoice) channels.InboundMessage {
	msg.Workspace = ws
	return msg
}

func readRow(t *testing.T, s store.Store) store.Entry {
	t.Helper()
	e, ok, err := s.Get(t.Context(), threadKey)
	require.NoError(t, err)
	require.True(t, ok)
	return e
}

// The first turn records the thread's choice with its Session; a later turn
// that makes no choice, or the same one, reuses both; a different choice
// starts a new Session under a key of its own and records it.
func TestSessionFor_WorkspaceChoice(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)

	runTurn(t, f, withWorkspace(slackMsg("hi"), klausDev))
	row := readRow(t, routes)
	require.Equal(t, klausDev, row.Workspace)
	require.Equal(t, "inst-kagent/worker-1", row.AgentInstanceID)

	runTurn(t, f, slackMsg("and now?"))
	runTurn(t, f, withWorkspace(slackMsg("again"), &store.WorkspaceChoice{Namespace: "kagent", Name: "klaus-dev"}))
	require.Len(t, agent.createRequests, 1, "a turn without a choice, or with the same one, reuses the Session")
	require.Equal(t, klausDev, readRow(t, routes).Workspace)

	other := &store.WorkspaceChoice{Namespace: "kagent", Name: "other"}
	runTurn(t, f, withWorkspace(slackMsg("elsewhere"), other))
	require.Len(t, agent.createRequests, 2)
	require.NotEqual(t, agent.createRequests[0], agent.createRequests[1], "another workspace, another key")
	row = readRow(t, routes)
	require.Equal(t, other, row.Workspace)
	require.Equal(t, "inst-kagent/worker-2", row.AgentInstanceID)
}

// A thread that chose no workspace keeps the key, and with it the Session, a
// thread had before workspaces existed.
func TestSessionFor_NoWorkspaceKeepsTheSession(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)

	runTurn(t, f, slackMsg("hi"))
	require.Nil(t, readRow(t, routes).Workspace)

	runTurn(t, f, withWorkspace(slackMsg("no workspace"), &store.WorkspaceChoice{None: true}))
	require.Equal(t, 1, agent.created, "an explicit none gets the thread's Session back")
	row := readRow(t, routes)
	require.Equal(t, &store.WorkspaceChoice{None: true}, row.Workspace)
	require.Equal(t, "inst-kagent/worker-1", row.AgentInstanceID)
}

// A rebind to another agent drops the thread's choice with the binding; the
// new agent's turn makes its own.
func TestSessionFor_RebindDropsTheWorkspace(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)

	runTurn(t, f, withWorkspace(slackMsg("hi"), klausDev))
	other := slackMsg("you then")
	other.AgentRef = "kagent/other"
	runTurn(t, f, other)

	row := readRow(t, routes)
	require.Equal(t, "kagent/other", row.AgentRef)
	require.Nil(t, row.Workspace)
}

// A gateway restart on a durable store keeps the thread's choice and its
// Session: the next process's turn, which makes no choice, runs on the
// Session the first one created, and creates none.
func TestSessionFor_WorkspaceSurvivesRestart(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) func() store.Store{
		"bolt": func(t *testing.T) func() store.Store {
			path := t.TempDir() + "/routes.bolt"
			return func() store.Store {
				s, err := boltstore.Open(path)
				require.NoError(t, err)
				return s
			}
		},
		"valkey": func(t *testing.T) func() store.Store {
			m := miniredis.RunT(t)
			return func() store.Store {
				s, err := valkeystore.New(valkeystore.Options{URL: m.Addr()})
				require.NoError(t, err)
				return s
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
			reopen := open(t)

			s1 := reopen()
			runTurn(t, &channels.Facade{Agent: agent, Routes: s1, ThreadTTL: channels.DefaultThreadTTL, Durable: true},
				withWorkspace(slackMsg("hi"), klausDev))
			require.NoError(t, s1.Close())

			s2 := reopen()
			t.Cleanup(func() { _ = s2.Close() })
			row := readRow(t, s2)
			require.Equal(t, klausDev, row.Workspace, "the choice survives the restart")
			require.Equal(t, "inst-kagent/worker-1", row.AgentInstanceID)

			runTurn(t, &channels.Facade{Agent: agent, Routes: s2, ThreadTTL: channels.DefaultThreadTTL, Durable: true},
				slackMsg("still there?"))
			require.Len(t, agent.createRequests, 1, "the restarted gateway reuses the Session")
			require.Equal(t, []string{"inst-kagent/worker-1", "inst-kagent/worker-1"}, agent.streamedOn)
			require.Equal(t, klausDev, readRow(t, s2).Workspace)
		})
	}
}
