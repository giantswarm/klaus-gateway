package slack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccessState_LockedMode(t *testing.T) {
	s := &AccessState{owner: "U001"}
	require.True(t, s.Permitted("U001"), "owner permitted")
	require.False(t, s.Permitted("U002"), "non-owner blocked in locked mode")
	require.True(t, s.Deliver("U001"))
	require.False(t, s.Deliver("U002"))
}

func TestAccessState_OpenMode(t *testing.T) {
	s := &AccessState{owner: "U001", mode: ModeOpen}
	require.True(t, s.Permitted("U002"))
	require.True(t, s.Deliver("U002"))
}

func TestAccessState_SelectiveMode(t *testing.T) {
	s := &AccessState{owner: "U001", mode: ModeSelective, allowed: map[string]bool{"U002": true}}
	require.True(t, s.Permitted("U001"))
	require.True(t, s.Permitted("U002"))
	require.False(t, s.Permitted("U003"))
}

func TestAccessState_BanOverridesOpen(t *testing.T) {
	s := &AccessState{owner: "U001", mode: ModeOpen, banned: map[string]bool{"U002": true}}
	require.False(t, s.Permitted("U002"))
	require.False(t, s.Deliver("U002"))
}

func TestAccessState_ObserveMode(t *testing.T) {
	s := &AccessState{owner: "U001", observe: true}
	// Non-owner gets delivered but not a reply.
	require.True(t, s.Deliver("U002"))
	require.False(t, s.Permitted("U002"))
	// Owner still gets a reply.
	require.True(t, s.Deliver("U001"))
	require.True(t, s.Permitted("U001"))
}

func TestAccessState_ObserveBanStillBlocks(t *testing.T) {
	s := &AccessState{owner: "U001", observe: true, banned: map[string]bool{"U999": true}}
	require.False(t, s.Deliver("U999"))
}

func TestAccessState_Allow(t *testing.T) {
	s := &AccessState{owner: "U001"}
	s.Allow("U002")
	require.Equal(t, ModeSelective, s.mode)
	require.True(t, s.Permitted("U002"))
}

func TestAccessState_Lock(t *testing.T) {
	s := &AccessState{owner: "U001", mode: ModeOpen, observe: true}
	s.Lock()
	require.Equal(t, ModeLocked, s.mode)
	require.False(t, s.observe)
	require.False(t, s.Permitted("U002"))
}

func TestAccessState_ToggleObserve(t *testing.T) {
	s := &AccessState{owner: "U001"}
	s.ToggleObserve()
	require.True(t, s.observe)
	s.ToggleObserve()
	require.False(t, s.observe)
}

func TestGetAccess_DefaultLocked(t *testing.T) {
	a := &Adapter{}
	state := a.getAccess("T001", "U001")
	require.Equal(t, "U001", state.owner)
	require.Equal(t, ModeLocked, state.mode)
	// Same thread returns same state.
	state2 := a.getAccess("T001", "U999")
	require.Same(t, state, state2)
}

func TestGetAccess_DefaultOpen(t *testing.T) {
	a := &Adapter{DefaultAccessMode: "open"}
	state := a.getAccess("T001", "U001")
	require.Equal(t, ModeOpen, state.mode)
}

func TestGetAccess_AllowedUsers(t *testing.T) {
	a := &Adapter{AllowedUsers: []string{"U001", "U002"}}
	state := a.getAccess("T001", "U999")
	require.Equal(t, "U001", state.owner)
	require.Equal(t, ModeSelective, state.mode)
	require.True(t, state.Permitted("U002"))
	require.False(t, state.Permitted("U999"))
}
