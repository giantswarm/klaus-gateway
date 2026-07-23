package slack

import (
	"context"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// threadState is everything the adapter tracks for one Slack thread's turn
// lifecycle, guarded as a unit by threadsMu.
type threadState struct {
	// slot is non-nil while a turn holds the thread's single in-flight slot.
	slot *turnSlot
	// pending is the paused input-required task awaiting resume.
	pending *pendingTask
	// idleWaiters are deferred replays, run when the slot frees.
	idleWaiters []func()
}

// empty reports whether the thread carries no live turn state; an empty entry
// is dropped from the threads map.
func (st *threadState) empty() bool {
	return st.slot == nil && st.pending == nil && len(st.idleWaiters) == 0
}

// turnSlot is the state that lives exactly as long as one turn holds the
// thread: releasing the slot drops it, so a recorded stop or registered turn
// can never outlive the turn it targeted and affect a later one.
type turnSlot struct {
	// stopPending records a /stop that arrived during the turn's start window
	// (before it registered a cancelable turn), consumed by registerTurn.
	stopPending bool
	// turn is the cancelable in-flight turn, set by registerTurn.
	turn *turn
}

// turn is an in-flight agent turn. The pointer identity lets a turn's cleanup
// release only its own registration. Turns on a thread are serialized (see
// acquireThread), so /stop cancels the single registered turn.
type turn struct {
	cancel context.CancelFunc
}

// pendingTask records the A2A task paused at input-required for a thread.
type pendingTask struct {
	TaskID    string
	AgentRef  string
	Channel   string // Slack channel ID for posting the resumed response
	ChannelID string // logical channel ID used in the routing key
	// Prompt is the structured approval request the task is paused on, used to
	// map a free-text reply or choice click back to a HITL decision.
	Prompt *channels.HitlPrompt
	// Usage carries the paused turn's token counts so the resuming turn reports
	// the whole turn, not just its tail.
	Usage channels.TurnUsage

	storedAt time.Time // set by storePendingTask; drives the TTL sweep
}

// withThread runs fn with threadID's state under threadsMu, creating the
// entry on demand and dropping it when fn leaves it empty, so the map stays
// bounded by live threads and read-only calls on untracked threads never
// touch it. fn must not call other thread methods: threadsMu is not
// reentrant. storePendingTask open-codes this lock to also sweep expired
// entries map-wide.
func (a *Adapter) withThread(threadID string, fn func(st *threadState)) {
	a.threadsMu.Lock()
	defer a.threadsMu.Unlock()
	st, tracked := a.threads[threadID]
	if !tracked {
		st = &threadState{}
	}
	fn(st)
	switch {
	case tracked && st.empty():
		delete(a.threads, threadID)
	case !tracked && !st.empty():
		if a.threads == nil {
			a.threads = make(map[string]*threadState)
		}
		a.threads[threadID] = st
	}
}

// acquireThread reserves the single in-flight turn slot for threadID, returning
// false when a turn is already running (the caller rejects the new turn).
func (a *Adapter) acquireThread(threadID string) bool {
	acquired := false
	a.withThread(threadID, func(st *threadState) {
		if st.slot == nil {
			st.slot = &turnSlot{}
			acquired = true
		}
	})
	return acquired
}

// releaseThread frees the thread's turn slot and runs any deferred replays,
// each on its own goroutine, outside the lock.
func (a *Adapter) releaseThread(threadID string) {
	var waiters []func()
	a.withThread(threadID, func(st *threadState) {
		st.slot = nil
		waiters = st.idleWaiters
		st.idleWaiters = nil
	})
	for _, waiter := range waiters {
		a.background(func(_ context.Context) { waiter() })
	}
}

// stopThread cancels the thread's in-flight turn, reporting whether there was
// one to stop. A turn spends its network-bound start window (email lookup,
// initiator token mint, resume check, agent resolve) holding the slot before
// it registers a cancelable turn; a /stop landing in that window is recorded
// on the slot for registerTurn to consume, so it still stops the turn. The
// sender's own token mint runs before the slot is taken (so a signed-out
// sender parks instead of bouncing busy); a /stop during that mint finds the
// thread idle and reports nothing running, an accepted trade of the ordering.
func (a *Adapter) stopThread(threadID string) bool {
	stopped := false
	a.withThread(threadID, func(st *threadState) {
		switch {
		case st.slot == nil:
		case st.slot.turn != nil:
			st.slot.turn.cancel()
			stopped = true
		default:
			st.slot.stopPending = true
			stopped = true
		}
	})
	return stopped
}

// whenThreadIdle runs fn once threadID's turn slot is free: synchronously when
// it is free now, otherwise on its own goroutine when the holding turn releases
// it. fn must re-acquire the slot itself (typically via dispatch) and handle
// losing that race to a concurrently arriving turn.
func (a *Adapter) whenThreadIdle(threadID string, fn func()) {
	idle := false
	a.withThread(threadID, func(st *threadState) {
		if st.slot == nil {
			idle = true
			return
		}
		st.idleWaiters = append(st.idleWaiters, fn)
	})
	if idle {
		fn()
	}
}

// maxTurnDuration bounds a single turn's stream. The A2A hop has no HTTP
// client timeout (one would sever long turns mid-stream and corrupt the agent
// session), so this is the backstop that eventually frees the thread slot when
// the upstream wedges without closing the connection.
const maxTurnDuration = 30 * time.Minute

// registerTurn installs a cancelable in-flight turn for threadID so /stop can
// cancel it, and returns the turn context plus a cleanup func that cancels the
// turn and releases only this turn's registration. A /stop that arrived during
// the turn's start window (before this registration) is honoured by cancelling
// the turn immediately. The caller must hold the thread's slot.
func (a *Adapter) registerTurn(ctx context.Context, threadID string) (context.Context, func()) {
	turnCtx, cancel := context.WithTimeout(ctx, maxTurnDuration)
	t := &turn{cancel: cancel}
	a.withThread(threadID, func(st *threadState) {
		st.slot.turn = t
		if st.slot.stopPending {
			st.slot.stopPending = false
			cancel()
		}
	})
	return turnCtx, func() {
		cancel()
		a.withThread(threadID, func(st *threadState) {
			if st.slot != nil && st.slot.turn == t {
				st.slot.turn = nil
			}
		})
	}
}

// storePendingTask records a paused input-required task for a thread.
// Any existing pending task for that thread is replaced. Abandoned entries are
// swept opportunistically so the map does not grow for the process lifetime.
func (a *Adapter) storePendingTask(threadID string, task *pendingTask) {
	a.threadsMu.Lock()
	defer a.threadsMu.Unlock()
	st := a.threads[threadID]
	if st == nil {
		st = &threadState{}
		if a.threads == nil {
			a.threads = make(map[string]*threadState)
		}
		a.threads[threadID] = st
	}
	task.storedAt = time.Now()
	st.pending = task
	for thread, other := range a.threads {
		if other.pending != nil && time.Since(other.pending.storedAt) > pendingTTL {
			other.pending = nil
			if other.empty() {
				delete(a.threads, thread)
			}
		}
	}
}

// takePendingTask atomically retrieves and removes a pending task for a thread.
// Returns nil when no task is pending.
func (a *Adapter) takePendingTask(threadID string) *pendingTask {
	var task *pendingTask
	a.withThread(threadID, func(st *threadState) {
		task = st.pending
		st.pending = nil
	})
	return task
}

// hasPendingTask reports whether a thread has a pending input-required task.
func (a *Adapter) hasPendingTask(threadID string) bool {
	ok := false
	a.withThread(threadID, func(st *threadState) {
		ok = st.pending != nil
	})
	return ok
}

// peekPendingTask returns a thread's pending task without removing it, or nil
// when none is pending. A caller that relies on the task still being pending on
// a later takePendingTask must hold the thread slot across both.
func (a *Adapter) peekPendingTask(threadID string) *pendingTask {
	var task *pendingTask
	a.withThread(threadID, func(st *threadState) {
		task = st.pending
	})
	return task
}
