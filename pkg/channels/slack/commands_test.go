package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
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
		{input: "/invite <@U123>", name: "invite", args: []string{"<@U123>"}},
		{input: "/LOCK", name: "lock", args: nil},
		{input: "  /quit  ", name: "quit", args: nil},
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

func TestParseUserIDs(t *testing.T) {
	require.Equal(t, []string{"U123"}, parseUserIDs([]string{"<@U123>"}))
	require.Equal(t, []string{"U123", "W456"}, parseUserIDs([]string{"<@U123>", "<@W456>"}))
	require.Nil(t, parseUserIDs([]string{"@U123"}), "bare @mention is not Slack's encoding")
	require.Nil(t, parseUserIDs([]string{"U123"}), "a raw id is not a mention token")
	require.Nil(t, parseUserIDs([]string{"everyone"}), "non-mention tokens are skipped")
	require.Equal(t, []string{"U123"}, parseUserIDs([]string{"<@U123>", "chatter"}))
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

func TestHandleCommand_Quit_OwnerOnly(t *testing.T) {
	a, srv := newTestAdapter(t)
	// Prime access state so U001 is owner.
	_ = a.getAccess("T001", "U001")

	// Non-owner should be refused.
	cmd := &slashCommand{Name: "quit"}
	consumed := a.handleCommand(t.Context(), cmd, "U002", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, int32(1), srv.posts.Load()) // rejection message

	// Owner clears the state.
	_ = a.handleCommand(t.Context(), cmd, "U001", "C001", "T001")
	require.Equal(t, int32(2), srv.posts.Load())

	a.accessMu.Lock()
	_, stillExists := a.access["T001"]
	a.accessMu.Unlock()
	require.False(t, stillExists, "quit should remove access entry")
}

func TestHandleCommand_InviteLock_OwnerOnly(t *testing.T) {
	a, srv := newTestAdapter(t)
	_ = a.getAccess("T001", "U001")
	state := a.getAccess("T001", "U001")

	// Non-owner invite must not take effect.
	consumed := a.handleCommand(t.Context(), &slashCommand{Name: "invite", Args: []string{"<@U002>"}}, "U002", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, ModeLocked, state.mode, "non-owner invite must not take effect")

	// Owner invite grants selective access to the mentioned user.
	consumed = a.handleCommand(t.Context(), &slashCommand{Name: "invite", Args: []string{"<@U002>"}}, "U001", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, ModeSelective, state.mode)
	require.True(t, state.Permitted("U001"), "owner stays permitted")
	require.True(t, state.Permitted("U002"), "invited user is permitted")
	require.False(t, state.Permitted("U999"), "everyone else stays excluded")

	// Owner lock restricts back to owner only.
	consumed = a.handleCommand(t.Context(), &slashCommand{Name: "lock"}, "U001", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, ModeLocked, state.mode)
	require.False(t, state.Permitted("U002"), "lock revokes the invite")

	require.Equal(t, int32(3), srv.posts.Load())
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

func TestHandleCommand_Invite_NoMentionShowsUsage(t *testing.T) {
	a, srv := newTestAdapter(t)
	_ = a.getAccess("T001", "U001")
	state := a.getAccess("T001", "U001")

	consumed := a.handleCommand(t.Context(), &slashCommand{Name: "invite"}, "U001", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, ModeLocked, state.mode, "a bare /invite must not change access")
	require.Equal(t, int32(1), srv.posts.Load(), "a usage hint is posted")
}
