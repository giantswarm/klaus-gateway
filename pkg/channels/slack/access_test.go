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

func TestAccessState_ObserveMode(t *testing.T) {
	s := &AccessState{owner: "U001", observe: true}
	// Non-owner gets delivered but not a reply.
	require.True(t, s.Deliver("U002"))
	require.False(t, s.Permitted("U002"))
	// Owner still gets a reply.
	require.True(t, s.Deliver("U001"))
	require.True(t, s.Permitted("U001"))
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
