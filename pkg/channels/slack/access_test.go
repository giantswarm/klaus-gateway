package slack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryAccess_InitiatorSetOnce(t *testing.T) {
	m := newMemoryAccess()
	require.Equal(t, "U001", m.SetInitiator("T001", "U001"), "first caller becomes initiator")
	require.Equal(t, "U001", m.SetInitiator("T001", "U002"), "later caller does not displace the initiator")
	require.Equal(t, "U001", m.Initiator("T001"))
	require.Equal(t, "", m.Initiator("T999"), "unknown thread has no initiator")
}

func TestMemoryAccess_InitiatorInstructsFreely(t *testing.T) {
	m := newMemoryAccess()
	m.SetInitiator("T001", "U001")
	require.True(t, m.Allowed("T001", "U001"), "initiator is allowed")
	require.False(t, m.Allowed("T001", "U002"), "newcomer is not allowed until granted")
}

func TestMemoryAccess_GrantIsAdditive(t *testing.T) {
	m := newMemoryAccess()
	m.SetInitiator("T001", "U001")
	m.Grant("T001", "U002")
	m.Grant("T001", "U003")
	require.True(t, m.Allowed("T001", "U002"))
	require.True(t, m.Allowed("T001", "U003"), "grant does not evict earlier grants")
	require.True(t, m.Allowed("T001", "U001"), "initiator stays allowed")
	require.False(t, m.Allowed("T001", "U004"))
}

func TestMemoryAccess_UnknownThreadDeniesAll(t *testing.T) {
	m := newMemoryAccess()
	require.False(t, m.Allowed("T999", "U001"))
}

func TestAccessPolicy_LazyDefault(t *testing.T) {
	a := &Adapter{}
	require.Equal(t, "U001", a.accessPolicy().SetInitiator("T001", "U001"))
	require.True(t, a.accessPolicy().Allowed("T001", "U001"))
}
