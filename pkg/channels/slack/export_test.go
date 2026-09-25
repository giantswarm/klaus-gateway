package slack

import (
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// Test hooks: the external test package builds adapters around a shared
// in-process recorder to simulate a restart with a surviving store.

type MemoryRecorder = channels.Facade

func NewMemoryRecorder() *MemoryRecorder { return newMemoryRecorder() }

// newMemoryRecorder is a gateway for tests: the Facade's own row logic over a
// memory store, with the default thread lifetime and no kagent client, so
// every turn, resume and session call answers "not available".
func newMemoryRecorder() *channels.Facade {
	return &channels.Facade{Routes: memory.New(), ThreadTTL: channels.DefaultThreadTTL}
}

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
