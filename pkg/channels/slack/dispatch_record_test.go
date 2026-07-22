package slack

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// recordingHandler collects slog records so tests can assert on structured
// log output.
type recordingHandler struct {
	mu      sync.Mutex
	records []map[string]any
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, fields)
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first collected record with the given key=value field.
func (h *recordingHandler) find(key string, value any) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r[key] == value {
			return r
		}
	}
	return nil
}

// identOBO is an OBOTokenSource that also exposes a linked muster identity,
// like *musterlink.Linker does.
type identOBO struct{}

func (identOBO) TokenFor(context.Context, string) (string, error) { return "tok-abc", nil }
func (identOBO) LinkURL(string) string                            { return "https://example.test/link" }
func (identOBO) Unlink(string)                                    {}
func (identOBO) LinkedIdentity(string) (string, string, bool) {
	return "sub-123", "user@example.test", true
}

// A dispatched turn must emit exactly one turn_dispatch record carrying the
// audit-join fields (agent, Slack user, muster sub, thread).
func TestDispatch_EmitsTurnDispatchRecord(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(fake.Close)

	h := &recordingHandler{}
	a := &Adapter{
		Logger:       slog.New(h),
		Mode:         ModeEvents,
		Secrets:      Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
		APIBase:      fake.URL,
		DefaultAgent: "agent-1",
		OBO:          identOBO{},
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T1", MessageID: "M1", Subject: "U1", Text: "hello"}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))

	rec := h.find("record", "turn_dispatch")
	require.NotNil(t, rec, "dispatch must emit a turn_dispatch record")
	require.Equal(t, "agent-1", rec["agent"])
	require.Equal(t, "U1", rec["slack_user"])
	require.Equal(t, "sub-123", rec["sub"])
	require.Equal(t, "T1", rec["thread_id"])
	require.Equal(t, "M1", rec["message_id"])
	require.Equal(t, false, rec["resume"])
	require.Equal(t, agentSourceDefault, rec["agent_source"])
}

// The agent_source field marks how the turn's agent was chosen: a message
// carrying an /agent prefix logs "prefix", a reply inheriting its
// conversation's binding logs "thread", everything else "default".
func TestDispatch_TurnDispatchRecord_AgentSource(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(fake.Close)

	newRecorded := func(t *testing.T) (*Adapter, *recordingHandler) {
		h := &recordingHandler{}
		a := &Adapter{
			Logger:       slog.New(h),
			Mode:         ModeEvents,
			Secrets:      Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
			APIBase:      fake.URL,
			DefaultAgent: "agent-1",
		}
		require.NoError(t, a.Start(t.Context(), &fakeGateway{}))
		return a, h
	}

	t.Run("prefix", func(t *testing.T) {
		a, h := newRecorded(t)
		// handleAgentSelection stamps the ref before dispatch.
		msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "C1", ThreadID: "T1", MessageID: "T1", Subject: "U1", Text: "hi", AgentRef: "sre-agent"}
		require.NoError(t, a.dispatch(t.Context(), msg, "C1"))
		rec := h.find("record", "turn_dispatch")
		require.NotNil(t, rec)
		require.Equal(t, agentSourcePrefix, rec["agent_source"])
		require.Equal(t, "sre-agent", rec["agent"])
	})

	t.Run("thread", func(t *testing.T) {
		a, h := newRecorded(t)
		a.bindThreadAgent("T2", "sre-agent")
		msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "C1", ThreadID: "T2", MessageID: "M2", Subject: "U1", Text: "reply"}
		require.NoError(t, a.dispatch(t.Context(), msg, "C1"))
		rec := h.find("record", "turn_dispatch")
		require.NotNil(t, rec)
		require.Equal(t, agentSourceThread, rec["agent_source"])
		require.Equal(t, "sre-agent", rec["agent"])
	})

	t.Run("default", func(t *testing.T) {
		a, h := newRecorded(t)
		msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "C1", ThreadID: "T3", MessageID: "T3", Subject: "U1", Text: "hi"}
		require.NoError(t, a.dispatch(t.Context(), msg, "C1"))
		rec := h.find("record", "turn_dispatch")
		require.NotNil(t, rec)
		require.Equal(t, agentSourceDefault, rec["agent_source"])
		require.Equal(t, "agent-1", rec["agent"])
	})
}

// An OBO source without the LinkedIdentity extension (or OBO disabled) still
// dispatches and logs the record, with an empty sub.
func TestDispatch_TurnDispatchRecord_NoIdentitySource(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(fake.Close)

	h := &recordingHandler{}
	a := &Adapter{
		Logger:       slog.New(h),
		Mode:         ModeEvents,
		Secrets:      Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
		APIBase:      fake.URL,
		DefaultAgent: "agent-1",
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T2", MessageID: "M2", Subject: "U2", Text: "hi"}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))

	rec := h.find("record", "turn_dispatch")
	require.NotNil(t, rec)
	require.Equal(t, "", rec["sub"])
}

// A turn resuming a paused input-required task is marked resume=true and
// carries the task id.
func TestDispatch_TurnDispatchRecord_Resume(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.2"}`))
	}))
	t.Cleanup(fake.Close)

	h := &recordingHandler{}
	a := &Adapter{
		Logger:       slog.New(h),
		Mode:         ModeEvents,
		Secrets:      Secrets{BotToken: "b", SigningSecret: "s"}, //nolint:gosec // dummy test creds
		APIBase:      fake.URL,
		DefaultAgent: "agent-1",
	}
	require.NoError(t, a.Start(t.Context(), &fakeGateway{}))

	a.storePendingTask("T3", &pendingTask{TaskID: "task-9", AgentRef: "agent-1", Channel: "D1", ChannelID: "D1"})

	msg := channels.InboundMessage{Channel: ChannelName, ChannelID: "D1", ThreadID: "T3", MessageID: "M3", Subject: "U3", Text: "approve"}
	require.NoError(t, a.dispatch(t.Context(), msg, "D1"))

	rec := h.find("record", "turn_dispatch")
	require.NotNil(t, rec)
	require.Equal(t, true, rec["resume"])
	require.Equal(t, "task-9", rec["task_id"])
}
