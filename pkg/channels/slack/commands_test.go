package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input   string
		wantNil bool
		name    string
		args    []string
	}{
		{input: "/stop", name: "stop", args: nil},
		{input: "/help", name: "help", args: nil},
		{input: "/details on", name: "details", args: []string{"on"}},
		{input: "/LOGIN", name: "login", args: nil},
		{input: "  /logout  ", name: "logout", args: nil},
		{input: "hello /stop", wantNil: true},
		{input: "!stop", wantNil: true},
		{input: "", wantNil: true},
		{input: "/", wantNil: true},
		{input: "no command here", wantNil: true},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			cmd := parseCommand(tc.input)
			if tc.wantNil {
				require.Nil(t, cmd)
				return
			}
			require.NotNil(t, cmd)
			require.Equal(t, tc.name, cmd.Name)
			require.Equal(t, tc.args, cmd.Args)
		})
	}
}

// fakeSlackServer records postMessage calls and returns minimal OK responses.
type fakeSlackServer struct {
	posts atomic.Int32
}

func (f *fakeSlackServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		f.posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	})
	return mux
}

func newTestAdapter(t *testing.T) (*Adapter, *fakeSlackServer) {
	t.Helper()
	srv := &fakeSlackServer{}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	a := &Adapter{
		APIBase: ts.URL,
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
	}
	return a, srv
}

func TestHandleCommand_Help(t *testing.T) {
	a, srv := newTestAdapter(t)
	cmd := &slashCommand{Name: "help"}
	consumed := a.handleCommand(t.Context(), cmd, "U001", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, int32(1), srv.posts.Load())
}

func TestHandleCommand_UnknownCommand(t *testing.T) {
	a, _ := newTestAdapter(t)
	cmd := &slashCommand{Name: "frobulate"}
	consumed := a.handleCommand(t.Context(), cmd, "U001", "C001", "T001")
	require.False(t, consumed)
}

func TestHandleCommand_Stop_CancelsInFlightTurn(t *testing.T) {
	a, srv := newTestAdapter(t)

	cancelled := make(chan struct{})
	cancel := func() { close(cancelled) }

	a.turnsMu.Lock()
	a.turns = map[string]*turn{"T001": {cancel: cancel}}
	a.turnsMu.Unlock()

	// U001 is the first to interact, so becomes the initiator and is permitted.
	cmd := &slashCommand{Name: "stop"}
	consumed := a.handleCommand(t.Context(), cmd, "U001", "C001", "T001")
	require.True(t, consumed)
	select {
	case <-cancelled:
	default:
		t.Fatal("expected cancel to be called")
	}
	require.Equal(t, int32(1), srv.posts.Load()) // the "⏹ Stopped." reply
}

func TestHandleCommand_Stop_NoTurnIsNoop(t *testing.T) {
	a, srv := newTestAdapter(t)
	cmd := &slashCommand{Name: "stop"}
	consumed := a.handleCommand(t.Context(), cmd, "U001", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, int32(1), srv.posts.Load())
}

func TestHandleCommand_Details_SetsLevel(t *testing.T) {
	a, srv := newTestAdapter(t)

	require.Equal(t, detailsOn, a.detailsLevel("T1"), "default is on")

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "details", Args: []string{"off"}}, "U1", "C1", "T1"))
	require.Equal(t, detailsOff, a.detailsLevel("T1"))

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "details", Args: []string{"full"}}, "U1", "C1", "T1"))
	require.Equal(t, detailsFull, a.detailsLevel("T1"))

	// No arg reports the current level; a bad arg shows usage — both consumed,
	// neither changes the level.
	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "details"}, "U1", "C1", "T1"))
	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "details", Args: []string{"loud"}}, "U1", "C1", "T1"))
	require.Equal(t, detailsFull, a.detailsLevel("T1"))

	require.Equal(t, int32(4), srv.posts.Load())
}

func TestHandleCommand_Usage_Consumed(t *testing.T) {
	a, srv := newTestAdapter(t)
	consumed := a.handleCommand(t.Context(), &slashCommand{Name: "usage"}, "U1", "C1", "T1")
	require.True(t, consumed)
	require.Equal(t, int32(1), srv.posts.Load())
}

// TestHandleCommand_OnlookerRefused verifies #124: once a thread has an
// initiator, a user who has not been allowed to instruct cannot run the
// state-changing / info commands.
func TestHandleCommand_OnlookerRefused(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.accessPolicy().SetInitiator("T001", "U001") // U001 initiates

	for _, name := range []string{"stop", "usage"} {
		require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: name}, "U002", "C001", "T001"))
	}
	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "details", Args: []string{"off"}}, "U002", "C001", "T001"))
	require.Equal(t, detailsOn, a.detailsLevel("T001"), "an onlooker cannot change details")
	require.Equal(t, int32(3), srv.posts.Load(), "each refusal posts one message")
}

// TestHandleCommand_GrantedUserAllowed verifies a collaborator the initiator
// approved may run the gated commands.
func TestHandleCommand_GrantedUserAllowed(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.accessPolicy().SetInitiator("T001", "U001")
	a.accessPolicy().Grant("T001", "U002")

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "details", Args: []string{"off"}}, "U002", "C001", "T001"))
	require.Equal(t, detailsOff, a.detailsLevel("T001"), "a granted collaborator can change details")
	require.Equal(t, int32(1), srv.posts.Load())
}

// TestHandleCommand_LoginLogout_OBODisabled confirms /login and /logout are
// open (no permission gate) and report OBO being disabled rather than
// dispatching to the agent.
func TestHandleCommand_LoginLogout_OBODisabled(t *testing.T) {
	a, srv := newTestAdapter(t)
	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "login"}, "U1", "C1", "T1"))
	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "logout"}, "U1", "C1", "T1"))
	require.Equal(t, int32(2), srv.posts.Load())
}

// A thread paused on input-required has no in-flight turn; /stop must fall
// through to dispatch so the paused task is resolved as a structured reject
// instead of staying armed after the "Stopped." reply.
func TestHandleCommand_Stop_PausedThreadFallsThroughToDispatch(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.storePendingTask("T001", &pendingTask{TaskID: "task-1", AgentRef: "worker", ChannelID: "C001"})

	cmd := &slashCommand{Name: "stop"}
	consumed := a.handleCommand(t.Context(), cmd, "U001", "C001", "T001")
	require.False(t, consumed, "/stop on a paused thread must be dispatched as a deny")
	require.NotNil(t, a.takePendingTask("T001"), "the pending task is resolved by dispatch, not the command handler")
	require.Equal(t, int32(0), srv.posts.Load())
}

// While a turn is in flight, /stop cancels it even when a pending task exists.
func TestHandleCommand_Stop_RunningTurnStillCancels(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.storePendingTask("T001", &pendingTask{TaskID: "task-1", AgentRef: "worker", ChannelID: "C001"})

	cancelled := make(chan struct{})
	a.turnsMu.Lock()
	a.turns = map[string]*turn{"T001": {cancel: func() { close(cancelled) }}}
	a.turnsMu.Unlock()

	cmd := &slashCommand{Name: "stop"}
	require.True(t, a.handleCommand(t.Context(), cmd, "U001", "C001", "T001"))
	select {
	case <-cancelled:
	default:
		t.Fatal("expected cancel to be called")
	}
	require.Equal(t, int32(1), srv.posts.Load())
}

func TestDecisionFromText_SlashStopIsDeny(t *testing.T) {
	d := decisionFromText(&channels.HitlPrompt{ToolName: "delete_file"}, "/stop")
	require.Equal(t, channels.DecisionReject, d.Type)
	require.Empty(t, d.RejectionReason, "/stop is a plain deny, not a reject-with-reason")
}
