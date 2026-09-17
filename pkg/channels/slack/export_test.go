package slack

import "github.com/giantswarm/klaus-gateway/pkg/channels"

// Test hooks: the external test package builds adapters around a shared
// in-process recorder to simulate a restart with a surviving store.

type MemoryRecorder = channels.Facade

func NewMemoryRecorder() *MemoryRecorder { return newMemoryRecorder() }

// ThreadIdle reports whether threadID's single turn slot is free. A turn that
// arrives while another holds the slot is refused as busy and never
// dispatched, so a test that sends a follow-up into a thread it has already
// driven waits for this before sending.
func (a *Adapter) ThreadIdle(threadID string) bool {
	idle := false
	a.withThread(threadID, func(st *threadState) { idle = st.slot == nil })
	return idle
}

// FlowWait re-exports the flow tests' wait budget for the external test
// package, so the value and its rationale live in one place.
const FlowWait = flowWait
