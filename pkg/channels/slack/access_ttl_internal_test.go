package slack

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryAccess_ExpiresIdleThreads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemoryAccess()
		m.SetInitiator("T1", "U1")
		m.Grant("T1", "U2")
		require.Equal(t, "U1", m.Initiator("T1"))
		require.True(t, m.Allowed("T1", "U2"))

		time.Sleep(threadAccessTTL + time.Minute)

		require.Empty(t, m.Initiator("T1"), "an idle thread past the TTL is no longer active")
		require.False(t, m.Allowed("T1", "U1"), "the expired initiator loses access")
		require.False(t, m.Allowed("T1", "U2"), "expired grants are gone")

		got := m.SetInitiator("T1", "U3")
		require.Equal(t, "U3", got, "a fresh mention re-establishes a new initiator")
	})
}

func TestMemoryAccess_ActivityRefreshesTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemoryAccess()
		m.SetInitiator("T1", "U1")

		time.Sleep(threadAccessTTL - time.Hour)
		m.SetInitiator("T1", "U1") // every handled message calls SetInitiator
		time.Sleep(threadAccessTTL - time.Hour)

		require.Equal(t, "U1", m.Initiator("T1"), "activity slides the deadline")
	})
}

func TestMemoryAccess_SweepsExpiredSiblings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMemoryAccess()
		m.SetInitiator("T1", "U1")
		time.Sleep(threadAccessTTL + time.Minute)
		m.SetInitiator("T2", "U2")

		m.mu.Lock()
		defer m.mu.Unlock()
		require.NotContains(t, m.threads, "T1", "touching one thread sweeps expired siblings")
		require.Contains(t, m.threads, "T2")
	})
}
