package slack

import (
	"time"

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

// ThreadIdle reports whether threadID's single turn slot is free and no
// drainer runs its inbox: every message the thread got has run, and left the
// row's inbox, or waits behind a prompt.
func (a *Adapter) ThreadIdle(threadID string) bool {
	idle := false
	a.withThread(threadID, func(st *threadState) { idle = st.slot == nil })
	a.inboxMu.Lock()
	defer a.inboxMu.Unlock()
	if st := a.inboxes[threadID]; st != nil && st.draining {
		return false
	}
	return idle
}

// FlowWait re-exports the flow tests' wait budget for the external test
// package, so the value and its rationale live in one place.
const FlowWait = flowWait

// SetRecoverBackoff shortens the start-up recovery's backoff (first wait, cap,
// whole window) for t, so a test drives retries and the give-up in
// milliseconds. The tests of this package do not run in parallel.
func SetRecoverBackoff(t interface{ Cleanup(func()) }, delay, maxDelay, window time.Duration) {
	oldDelay, oldMax, oldWindow := recoverRetryDelay, recoverRetryMax, recoverWindow
	recoverRetryDelay, recoverRetryMax, recoverWindow = delay, maxDelay, window
	t.Cleanup(func() { recoverRetryDelay, recoverRetryMax, recoverWindow = oldDelay, oldMax, oldWindow })
}

// SetRestoredInboxGrace shortens how long a restarted adapter leaves the
// inboxes it read back alone, for t.
func SetRestoredInboxGrace(t interface{ Cleanup(func()) }, grace time.Duration) {
	old := restoredInboxGrace
	restoredInboxGrace = grace
	t.Cleanup(func() { restoredInboxGrace = old })
}

// WaitHeldRestored blocks until RecoverTurns' restore of the held thread state
// has read the rows back, so a test acts on the restored state.
func (a *Adapter) WaitHeldRestored() {
	if ch := a.heldRestored.Load(); ch != nil {
		<-*ch
	}
}
