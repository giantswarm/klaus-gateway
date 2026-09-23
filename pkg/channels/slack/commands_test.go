package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
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
		{input: "/agent list", name: "agent", args: []string{"list"}},
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
	updates    atomic.Int32

	mu             sync.Mutex
	postTexts      []string
	postBodies     []string
	ephemeralTexts []string
	updateTexts    []string
}

// requestText is the text field of a form or JSON Slack call, and the raw body
// for the record.
func requestText(r *http.Request) (text, raw string) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		text, _ = body["text"].(string)
		return text, string(b)
	}
	_ = r.ParseForm()
	return r.PostFormValue("text"), r.PostForm.Encode()
}

func (f *fakeSlackServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		f.posts.Add(1)
		text, raw := requestText(r)
		f.mu.Lock()
		f.postBodies = append(f.postBodies, raw)
		f.mu.Unlock()
		if text != "" {
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
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		f.updates.Add(1)
		text, _ := requestText(r)
		f.mu.Lock()
		f.updateTexts = append(f.updateTexts, text)
		f.mu.Unlock()
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
func (o *blockingOBO) Unlink(string) error   { return nil }

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
	a.accessPolicy().SetInitiator(t.Context(), "C001", "T001", "U001") // U001 initiates

	for _, name := range []string{"stop", "usage"} {
		require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: name}, "U002", "C001", "T001"))
	}
	require.Equal(t, int32(2), srv.posts.Load(), "each refusal posts one message")
}

// TestHandleCommand_GrantedUserAllowed verifies a collaborator the initiator
// approved may run the gated commands.
func TestHandleCommand_GrantedUserAllowed(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.accessPolicy().SetInitiator(t.Context(), "C001", "T001", "U001")
	a.accessPolicy().Grant(t.Context(), "C001", "T001", "U002")

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "usage"}, "U002", "C001", "T001"))
	require.Equal(t, int32(1), srv.posts.Load(), "a granted collaborator can run the gated commands")
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
// store entry surviving an identity-provider revocation: the linker drops the
// link on invalid_grant and reports ErrNotLinked.
type deadLinkOBO struct{ identOBO }

func (deadLinkOBO) TokenFor(context.Context, string) (string, error) {
	return "", musterlink.ErrNotLinked
}

// storeDownOBO is a linked identity behind a link store that is not answering:
// the token cannot be minted right now, but the person is signed in.
type storeDownOBO struct{ identOBO }

func (storeDownOBO) TokenFor(context.Context, string) (string, error) {
	return "", errors.New("musterlink: read link: connection refused")
}

func (storeDownOBO) Unlink(string) error {
	return errors.New("musterlink: delete link: connection refused")
}

// A stored link is not proof the link works: /login must probe the token and
// re-prompt sign-in when the provider has revoked it, instead of confirming a
// sign-in that fails on the next turn.
func TestHandleCommand_LoginLinkedButDeadTokenRepromptsSignIn(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "login"}, "U1", "C1", "T1"))
	require.Equal(t, int32(1), srv.ephemerals.Load(), "the sign-in prompt reaches the caller only")
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, srv.ephemeralTexts[0], "*Sign in to Giant Swarm*",
		"a dead link re-prompts instead of confirming a sign-in")
	require.NotContains(t, srv.ephemeralTexts[0], signInForMessageLine, "/login holds no message")
	require.Equal(t, int32(1), srv.posts.Load(), "the thread notice anchors the ephemeral prompt")
}

// /logout confirms ephemerally: sign-in state is caller-only information.
func TestHandleCommand_LogoutConfirmsEphemerally(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = identOBO{}

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "logout"}, "U1", "C1", "T1"))
	require.Equal(t, int32(0), srv.posts.Load())
	require.Equal(t, int32(1), srv.ephemerals.Load())
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Equal(t, logoutNotice, srv.ephemeralTexts[0])
}

// A link store that is briefly away is not a dead link: /login must tell the
// person to retry instead of sending a signed-in person through a new sign-in
// (whose result the same store could not take either).
func TestHandleCommand_LoginStoreDownRepliesTransientNotice(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = storeDownOBO{}

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "login"}, "U1", "C1", "T1"))
	require.Equal(t, int32(0), srv.posts.Load(), "no sign-in prompt anchor: the person is not asked to sign in")
	require.Equal(t, int32(1), srv.ephemerals.Load())
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, srv.ephemeralTexts[0], tokenErrorNotice)
	require.NotContains(t, srv.ephemeralTexts[0], "*Sign in to Giant Swarm*")
}

// A sign-out the store refused is reported as such: confirming it would leave
// the person believing their refresh token is gone while the store keeps it.
func TestHandleCommand_LogoutStoreDownReportsFailure(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = storeDownOBO{}

	require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "logout"}, "U1", "C1", "T1"))
	require.Equal(t, int32(0), srv.posts.Load())
	require.Equal(t, int32(1), srv.ephemerals.Load())
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Contains(t, srv.ephemeralTexts[0], logoutFailedNotice)
	require.NotContains(t, srv.ephemeralTexts[0], "Signed out")
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

// The busy notice names the way out of a running turn.
func TestBusyNoticeNamesStop(t *testing.T) {
	require.Contains(t, busyNotice, "`/stop`")
}

// A bare "stop" is the word alone, in any case, with optional trailing
// punctuation; a sentence containing it or the slash form is not.
func TestIsBareStop(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"stop", true},
		{"Stop.", true},
		{"STOP!", true},
		{" stop ", true},
		{"stop?", true},
		{"/stop", false},
		{"stop watching the thread", false},
		{"please stop", false},
		{"stopped", false},
		{"", false},
	} {
		require.Equal(t, tc.want, isBareStop(tc.text), "%q", tc.text)
	}
}

// A command sent as a top-level message is its thread's root, and Slack does
// not show a thread-scoped ephemeral in a thread without replies
// (klaus-gateway#156): its private reply goes to the channel. A command sent
// as a reply keeps its reply in the thread.
func TestHandleCommand_RootCommandRepliesInTheChannel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		root   bool
		thread any
	}{
		{"top-level message", true, nil},
		{"reply in a thread", false, "T1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/chat.postEphemeral" {
					_ = json.NewDecoder(r.Body).Decode(&body)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			t.Cleanup(srv.Close)
			a := &Adapter{APIBase: srv.URL, Secrets: Secrets{BotToken: "t"}, Logger: slog.New(slog.DiscardHandler), OBO: identOBO{}}

			require.True(t, a.handleCommand(t.Context(), &slashCommand{Name: "logout", Root: tc.root}, "U1", "C1", "T1"))
			require.Equal(t, logoutNotice, body["text"])
			require.Equal(t, tc.thread, body["thread_ts"])
		})
	}
}
