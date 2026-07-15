package slack

import (
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
	a.pendingAccess["t1"]["U-old"].storedAt = time.Now().Add(-pendingTTL - time.Minute)
	a.pendingAccessMu.Unlock()

	a.storePendingAccess("t2", "U-new", &pendingAccessReq{msg: channels.InboundMessage{Text: "new"}})

	require.Nil(t, a.takePendingAccess("t1", "U-old"), "expired parked request must be swept")
	require.NotNil(t, a.takePendingAccess("t2", "U-new"))
}

// storePendingAccess reports new only for the first parked request per (thread,
// user), so a burst replayed after sign-in posts the consent prompt once.
func TestStorePendingAccess_ReportsNewOnlyOnce(t *testing.T) {
	a := &Adapter{}
	first := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: "m1"}})
	require.True(t, first, "first parked request for a (thread, user) is new")

	second := a.storePendingAccess("t1", "U1", &pendingAccessReq{msg: channels.InboundMessage{Text: "m2"}})
	require.False(t, second, "a repeat before approval must not re-prompt")

	other := a.storePendingAccess("t1", "U2", &pendingAccessReq{msg: channels.InboundMessage{Text: "m3"}})
	require.True(t, other, "a different user in the same thread is prompted independently")
}
