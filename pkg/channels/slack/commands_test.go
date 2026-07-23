package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestHelpText(t *testing.T) {
	named := helpText("swarmgeist")
	require.Contains(t, named, "`@swarmgeist /stop`")
	require.NotContains(t, named, "@klaus")

	unnamed := helpText("")
	require.NotContains(t, unnamed, "@")
	require.Contains(t, unnamed, "`/stop`")

	// The command list is shared regardless of naming.
	require.Contains(t, named, "`/help`")
	require.Contains(t, unnamed, "`/help`")
}

// A transient users.info failure must not be cached: the guarantee is
// "resolve once", not "attempt once". The next call retries the name lookup
// (without repeating auth.test) and picks up the profile display name.
func TestResolveIdentity_RetriesNameLookupAfterTransientFailure(t *testing.T) {
	var authTestCalls, usersInfoCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		authTestCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"user_id":"UBOT","user":"klaus_bot"}`))
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if usersInfoCalls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"ok":false,"error":"internal_error"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"user":{"profile":{"display_name":"Swarmgeist"}}}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	a := &Adapter{
		APIBase: ts.URL,
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		Logger:  slog.New(slog.DiscardHandler),
	}

	id, name := a.resolveIdentity(t.Context())
	require.Equal(t, "UBOT", id)
	require.Equal(t, "klaus_bot", name, "the failed lookup falls back to the auth.test username")

	id, name = a.resolveIdentity(t.Context())
	require.Equal(t, "UBOT", id)
	require.Equal(t, "Swarmgeist", name, "the failed lookup is retried, not cached")
	require.Equal(t, int32(1), authTestCalls.Load(), "the ID from auth.test is kept across the retry")

	_, name = a.resolveIdentity(t.Context())
	require.Equal(t, "Swarmgeist", name)
	require.Equal(t, int32(2), usersInfoCalls.Load(), "the resolved identity is cached")
}

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

// fakeSlackServer records postMessage and postEphemeral calls and returns
// minimal OK responses.
type fakeSlackServer struct {
	posts      atomic.Int32
	ephemerals atomic.Int32

	mu             sync.Mutex
	postTexts      []string
	ephemeralTexts []string
}

func (f *fakeSlackServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		f.posts.Add(1)
		_ = r.ParseForm()
		if text := r.PostFormValue("text"); text != "" {
			f.mu.Lock()
			f.postTexts = append(f.postTexts, text)
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	})
	mux.HandleFunc("/chat.postEphemeral", func(w http.ResponseWriter, r *http.Request) {
		f.ephemerals.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if text, _ := body["text"].(string); text != "" {
			f.mu.Lock()
			f.ephemeralTexts = append(f.ephemeralTexts, text)
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
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
		Logger:  slog.New(slog.DiscardHandler),
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

	a.threadsMu.Lock()
	a.threads = map[string]*threadState{"T001": {slot: &turnSlot{turn: &turn{cancel: cancel}}}}
	a.threadsMu.Unlock()

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
	require.Equal(t, int32(1), srv.posts.Load(), `the idle thread gets the "nothing is running" reply`)
}

// A /stop during a turn's start window (thread slot held, turn not yet
// registered) must stop that turn: the request is recorded on the slot,
// consumed by registerTurn (cancelling the fresh turn), and dies with the
// slot when the turn never registers, so it cannot leak into a later turn.
func TestStopThread_StartWindow(t *testing.T) {
	a := &Adapter{Logger: slog.New(slog.DiscardHandler)}
	require.False(t, a.stopThread("T1"), "an idle thread has nothing to stop")

	require.True(t, a.acquireThread("T1"))
	require.True(t, a.stopThread("T1"), "a turn in its start window is stoppable")
	turnCtx, done := a.registerTurn(t.Context(), "T1")
	require.ErrorIs(t, turnCtx.Err(), context.Canceled, "the recorded stop cancels the turn at registration")
	done()
	a.releaseThread("T1")

	require.True(t, a.acquireThread("T1"))
	require.True(t, a.stopThread("T1"))
	a.releaseThread("T1") // the turn aborted before registering
	require.True(t, a.acquireThread("T1"))
	turnCtx2, done2 := a.registerTurn(t.Context(), "T1")
	require.NoError(t, turnCtx2.Err(), "a stop that died with its slot must not cancel a later turn")
	done2()
	a.releaseThread("T1")

	require.False(t, a.stopThread("T2"), "a stop cannot be recorded against an idle thread")
	require.True(t, a.acquireThread("T2"))
	turnCtx3, done3 := a.registerTurn(t.Context(), "T2")
	require.NoError(t, turnCtx3.Err(), "an idle-thread stop attempt leaves nothing behind")
	done3()
	a.releaseThread("T2")
}

// blockingOBO stalls TokenFor until release is closed, so a test can hold a
// dispatch inside the sender's own pre-slot token mint.
type blockingOBO struct {
	entered chan struct{} // closed when TokenFor is first entered
	release chan struct{} // TokenFor returns once closed
	once    sync.Once
}

func (o *blockingOBO) TokenFor(context.Context, string) (string, error) {
	o.once.Do(func() { close(o.entered) })
	<-o.release
	return "", errors.New("transient token-mint failure")
}
func (o *blockingOBO) LinkURL(string) string { return "" }
func (o *blockingOBO) Unlink(string)         {}

// A /stop landing while the sender's own token mint is still running finds
// the thread slot untaken (the mint runs before the slot so a signed-out
// sender parks instead of bouncing busy) and reports nothing running: the
// accepted trade documented on stopThread.
func TestHandleCommand_Stop_DuringSenderMint_ReportsNothingRunning(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.Mode = ModeEvents
	a.DefaultAgent = "agent"
	obo := &blockingOBO{entered: make(chan struct{}), release: make(chan struct{})}
	a.OBO = obo
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "C001", ThreadID: "T001", MessageID: "T001", Subject: "U001", Text: "long question"}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- a.dispatch(t.Context(), msg, "C001") }()
	<-obo.entered

	consumed := a.handleCommand(t.Context(), &slashCommand{Name: "stop"}, "U001", "C001", "T001")
	require.True(t, consumed)
	srv.mu.Lock()
	texts := append([]string(nil), srv.postTexts...)
	srv.mu.Unlock()
	require.Contains(t, texts, stopNothingRunningNotice)

	close(obo.release)
	require.NoError(t, <-dispatchDone)
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
	require.Equal(t, int32(2), srv.ephemerals.Load())
	require.Equal(t, int32(0), srv.posts.Load())
}

// A linked user's /login confirms their identity ephemerally: the linked email
// must never land as a regular message in a shared thread.
func TestHandleCommand_LoginLinkedConfirmsEphemerally(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = identOBO{}

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "login"}, "U1", "C1", "T1"))
	require.Equal(t, int32(0), srv.posts.Load(), "the identity confirmation must not be a public message")
	require.Equal(t, int32(1), srv.ephemerals.Load())
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, srv.ephemeralTexts[0], "user@example.test")
}

// deadLinkOBO reports a linked identity whose tokens no longer work, like a
// store entry surviving an identity-provider revocation.
type deadLinkOBO struct{ identOBO }

func (deadLinkOBO) TokenFor(context.Context, string) (string, error) {
	return "", errors.New("refresh token revoked")
}

// A stored link is not proof the link works: /login must probe the token and
// re-prompt sign-in when the provider has revoked it, instead of confirming a
// sign-in that fails on the next turn.
func TestHandleCommand_LoginLinkedButDeadTokenRepromptsSignIn(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "login"}, "U1", "C1", "T1"))
	require.Equal(t, int32(0), srv.ephemerals.Load(), "no signed-in confirmation for a dead link")
	require.Equal(t, int32(1), srv.posts.Load(), "the sign-in prompt is posted to the thread")
}

// /logout confirms ephemerally: sign-in state is caller-only information.
func TestHandleCommand_LogoutConfirmsEphemerally(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = identOBO{}

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "logout"}, "U1", "C1", "T1"))
	require.Equal(t, int32(0), srv.posts.Load())
	require.Equal(t, int32(1), srv.ephemerals.Load())
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
	a.threadsMu.Lock()
	a.threads["T001"].slot = &turnSlot{turn: &turn{cancel: func() { close(cancelled) }}}
	a.threadsMu.Unlock()

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
