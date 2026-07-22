package slack

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestStorePendingTask_SweepsExpiredEntries(t *testing.T) {
	a := &Adapter{}
	a.storePendingTask("t-old", &pendingTask{TaskID: "old"})
	a.pendingMu.Lock()
	a.pendingTasks["t-old"].storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.pendingMu.Unlock()

	a.storePendingTask("t-new", &pendingTask{TaskID: "new"})

	require.False(t, a.hasPendingTask("t-old"), "expired pending task must be swept")
	require.True(t, a.hasPendingTask("t-new"))
}

func TestStorePendingAccess_SweepsExpiredEntries(t *testing.T) {
	a := &Adapter{}
	a.storePendingAccess("t1", "U-old", &pendingAccessReq{msg: channels.InboundMessage{Text: "old"}})
	a.pendingAccessMu.Lock()
	a.pendingAccess["t1"]["U-old"][0].storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.pendingAccessMu.Unlock()

	a.storePendingAccess("t2", "U-new", &pendingAccessReq{msg: channels.InboundMessage{Text: "new"}})

	require.Empty(t, a.takePendingAccess("t1", "U-old"), "expired parked request must be swept")
	require.Len(t, a.takePendingAccess("t2", "U-new"), 1)
}

// storePendingAccess reports new only for the first parked request per (thread,
// user), so a burst replayed after sign-in posts the consent prompt once.
func TestStorePendingAccess_ReportsNewOnlyOnce(t *testing.T) {
	a := &Adapter{}
	first, _ := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: "m1"}})
	require.True(t, first, "first parked request for a (thread, user) is new")

	second, _ := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: "m2"}})
	require.False(t, second, "a repeat before approval must not re-prompt")

	other, _ := a.storePendingAccess("t1", "U2", &pendingAccessReq{msg: channels.InboundMessage{Text: "m3"}})
	require.True(t, other, "a different user in the same thread is prompted independently")
}

// The pending-access queue keeps every message a newcomer sends before the
// grant, in order, bounded by maxParkedPerThread (oldest dropped).
func TestStorePendingAccess_QueueOrderAndCap(t *testing.T) {
	a := &Adapter{}
	for i := 1; i <= maxParkedPerThread+2; i++ {
		_, dropped := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: fmt.Sprintf("m%d", i)}})
		require.Equal(t, i > maxParkedPerThread, dropped, "an eviction is reported exactly when the cap overflows (message %d)", i)
	}

	got := a.takePendingAccess("t1", "U1")
	require.Len(t, got, maxParkedPerThread, "the queue is capped")
	var texts []string
	for _, r := range got {
		texts = append(texts, r.msg.Text)
	}
	require.Equal(t, []string{"m3", "m4", "m5", "m6", "m7"}, texts, "oldest dropped past the cap, order preserved")
	require.Empty(t, a.takePendingAccess("t1", "U1"), "take clears the queue")
}

// takePendingLogin drops entries older than pendingTTL, so a sign-in completed
// long after the message was parked does not replay a stale question.
func TestTakePendingLogin_DropsExpiredEntries(t *testing.T) {
	a := &Adapter{}
	a.parkPendingLogin("U1", &pendingLoginReq{msg: channels.InboundMessage{ThreadID: "T1", Text: "stale"}})
	a.parkPendingLogin("U1", &pendingLoginReq{msg: channels.InboundMessage{ThreadID: "T1", Text: "fresh"}})
	a.pendingLoginMu.Lock()
	a.pendingLogin["U1"]["T1"][0].storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.pendingLoginMu.Unlock()

	got := a.takePendingLogin("U1")
	require.Len(t, got["T1"], 1)
	require.Equal(t, "fresh", got["T1"][0].msg.Text, "the expired entry is dropped, the fresh one survives")
}

func TestIsBareAuthUtterance(t *testing.T) {
	bare := []string{
		"login",
		"Login",
		"log in",
		"Sign In",
		"signin",
		"connect",
		"/login",
		"login!",
		"  sign in.  ",
		"<@BOT123> login",
	}
	for _, text := range bare {
		require.True(t, isBareAuthUtterance(text), "expected bare auth utterance: %q", text)
	}
	replayable := []string{
		"login to grafana is broken",
		"why does login fail",
		"connect the dots",
		"can you sign in to the cluster registry",
		"",
	}
	for _, text := range replayable {
		require.False(t, isBareAuthUtterance(text), "expected replayable message: %q", text)
	}
}
