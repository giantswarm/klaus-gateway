package slack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccessState_LockedMode(t *testing.T) {
	s := &AccessState{Owner: "U001"}
	require.True(t, s.Permitted("U001"), "owner permitted")
	require.False(t, s.Permitted("U002"), "non-owner blocked in locked mode")
	require.True(t, s.Deliver("U001"))
	require.False(t, s.Deliver("U002"))
}

func TestAccessState_OpenMode(t *testing.T) {
	s := &AccessState{Owner: "U001", Mode: ModeOpen}
	require.True(t, s.Permitted("U002"))
	require.True(t, s.Deliver("U002"))
}

func TestAccessState_SelectiveMode(t *testing.T) {
	s := &AccessState{Owner: "U001", Mode: ModeSelective, Allowed: map[string]bool{"U002": true}}
	require.True(t, s.Permitted("U001"))
	require.True(t, s.Permitted("U002"))
	require.False(t, s.Permitted("U003"))
}

func TestAccessState_BanOverridesOpen(t *testing.T) {
	s := &AccessState{Owner: "U001", Mode: ModeOpen, Banned: map[string]bool{"U002": true}}
	require.False(t, s.Permitted("U002"))
	require.False(t, s.Deliver("U002"))
}

func TestAccessState_ObserveMode(t *testing.T) {
	s := &AccessState{Owner: "U001", Observe: true}
	// Non-owner gets delivered but not a reply.
	require.True(t, s.Deliver("U002"))
	require.False(t, s.Permitted("U002"))
	// Owner still gets a reply.
	require.True(t, s.Deliver("U001"))
	require.True(t, s.Permitted("U001"))
}

func TestAccessState_ObserveBanStillBlocks(t *testing.T) {
	s := &AccessState{Owner: "U001", Observe: true, Banned: map[string]bool{"U999": true}}
	require.False(t, s.Deliver("U999"))
}

func TestAccessState_Allow(t *testing.T) {
	s := &AccessState{Owner: "U001"}
	s.Allow("U002")
	require.Equal(t, ModeSelective, s.Mode)
	require.True(t, s.Permitted("U002"))
}

func TestAccessState_Lock(t *testing.T) {
	s := &AccessState{Owner: "U001", Mode: ModeOpen, Observe: true}
	s.Lock()
	require.Equal(t, ModeLocked, s.Mode)
	require.False(t, s.Observe)
	require.False(t, s.Permitted("U002"))
}

func TestAccessState_ToggleObserve(t *testing.T) {
	s := &AccessState{Owner: "U001"}
	s.ToggleObserve()
	require.True(t, s.Observe)
	s.ToggleObserve()
	require.False(t, s.Observe)
}

func TestGetAccess_DefaultLocked(t *testing.T) {
	a := &Adapter{}
	state := a.getAccess("T001", "U001")
	require.Equal(t, "U001", state.Owner)
	require.Equal(t, ModeLocked, state.Mode)
	// Same thread returns same state.
	state2 := a.getAccess("T001", "U999")
	require.Same(t, state, state2)
}

func TestGetAccess_DefaultOpen(t *testing.T) {
	a := &Adapter{DefaultAccessMode: "open"}
	state := a.getAccess("T001", "U001")
	require.Equal(t, ModeOpen, state.Mode)
}

func TestGetAccess_AllowedUsers(t *testing.T) {
	a := &Adapter{AllowedUsers: []string{"U001", "U002"}}
	state := a.getAccess("T001", "U999")
	require.Equal(t, "U001", state.Owner)
	require.Equal(t, ModeSelective, state.Mode)
	require.True(t, state.Permitted("U002"))
	require.False(t, state.Permitted("U999"))
}
