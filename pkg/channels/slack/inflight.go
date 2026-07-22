package slack

// acquireThread reserves the single in-flight turn slot for threadID, returning
// false when a turn is already running (the caller rejects the new turn).
func (a *Adapter) acquireThread(threadID string) bool {
	a.inflightMu.Lock()
	defer a.inflightMu.Unlock()
	if a.inflight == nil {
		a.inflight = make(map[string]bool)
	}
	if _, busy := a.inflight[threadID]; busy {
		return false
	}
	a.inflight[threadID] = false
	return true
}

func (a *Adapter) releaseThread(threadID string) {
	a.inflightMu.Lock()
	delete(a.inflight, threadID)
	waiters := a.idleWaiters[threadID]
	delete(a.idleWaiters, threadID)
	a.inflightMu.Unlock()
	for _, waiter := range waiters {
		go waiter()
	}
}

// requestStopIfBusy records a /stop against the turn holding threadID's
// inflight slot, reporting whether one was there to stop. The flag lives on
// the slot entry itself: it can only be set while the slot is held and dies
// with the slot on release, so a stop can never outlive the turn it targeted
// and cancel a later one. registerTurn consumes it via takeStopRequest.
func (a *Adapter) requestStopIfBusy(threadID string) bool {
	a.inflightMu.Lock()
	defer a.inflightMu.Unlock()
	if _, busy := a.inflight[threadID]; !busy {
		return false
	}
	a.inflight[threadID] = true
	return true
}

// takeStopRequest consumes a stop recorded against threadID's held inflight
// slot, reporting whether one was pending.
func (a *Adapter) takeStopRequest(threadID string) bool {
	a.inflightMu.Lock()
	defer a.inflightMu.Unlock()
	if !a.inflight[threadID] {
		return false
	}
	a.inflight[threadID] = false
	return true
}

// whenThreadIdle runs fn once threadID's turn slot is free: synchronously when
// it is free now, otherwise on its own goroutine when the holding turn releases
// it. fn must re-acquire the slot itself (typically via dispatch) and handle
// losing that race to a concurrently arriving turn.
func (a *Adapter) whenThreadIdle(threadID string, fn func()) {
	a.inflightMu.Lock()
	if _, busy := a.inflight[threadID]; busy {
		if a.idleWaiters == nil {
			a.idleWaiters = make(map[string][]func())
		}
		a.idleWaiters[threadID] = append(a.idleWaiters[threadID], fn)
		a.inflightMu.Unlock()
		return
	}
	a.inflightMu.Unlock()
	fn()
}
