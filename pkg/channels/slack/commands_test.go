package slack

import (
	"context"
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
		{input: "/open @U123", name: "open", args: []string{"@U123"}},
		{input: "/LOCK", name: "lock", args: nil},
		{input: "  /quit  ", name: "quit", args: nil},
		{input: "hello /stop", wantNil: true},
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
	a.turns = map[string]context.CancelFunc{"T001": cancel}
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

func TestHandleCommand_OpenLock_OwnerOnly(t *testing.T) {
	a, srv := newTestAdapter(t)
	_ = a.getAccess("T001", "U001")

	// Non-owner open.
	consumed := a.handleCommand(t.Context(), &slashCommand{Name: "open"}, "U002", "C001", "T001")
	require.True(t, consumed)
	state := a.getAccess("T001", "U001")
	require.Equal(t, ModeLocked, state.Mode, "non-owner open must not take effect")

	// Owner open.
	consumed = a.handleCommand(t.Context(), &slashCommand{Name: "open"}, "U001", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, ModeOpen, state.Mode)

	// Owner lock.
	consumed = a.handleCommand(t.Context(), &slashCommand{Name: "lock"}, "U001", "C001", "T001")
	require.True(t, consumed)
	require.Equal(t, ModeLocked, state.Mode)

	require.Equal(t, int32(3), srv.posts.Load())
}

func TestHandleCommand_Observe_Toggle(t *testing.T) {
	a, _ := newTestAdapter(t)
	_ = a.getAccess("T001", "U001")
	state := a.getAccess("T001", "U001")

	a.handleCommand(t.Context(), &slashCommand{Name: "observe"}, "U001", "C001", "T001")
	require.True(t, state.Observe)

	a.handleCommand(t.Context(), &slashCommand{Name: "observe"}, "U001", "C001", "T001")
	require.False(t, state.Observe)
}
