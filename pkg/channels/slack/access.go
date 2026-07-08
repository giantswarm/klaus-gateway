package slack

import "sync"

// AccessPolicy decides who may instruct the agent in a thread. Reading a thread
// is never gated; the policy governs instructing only. A thread has one
// initiator (the user whose mention launched it) who may always instruct;
// additional users are granted on the fly once the initiator approves them.
//
// The decision is wrapped in this interface so a platform-backed policy
// (team-based auto-allow, shared collaborator lists) can replace the in-memory
// default without touching dispatch.
type AccessPolicy interface {
	// SetInitiator records userID as the thread initiator when none is set yet
	// and returns the effective initiator. Idempotent: a later caller never
	// displaces the first.
	SetInitiator(threadID, userID string) string
	// Initiator returns the thread initiator, or "" when none is set.
	Initiator(threadID string) string
	// Allowed reports whether userID may instruct the agent in threadID (the
	// initiator, or a user granted via Grant).
	Allowed(threadID, userID string) bool
	// Grant adds userID to the thread's allowed interactors. Additive.
	Grant(threadID, userID string)
}

// memoryAccess is the default in-memory AccessPolicy. State is per-thread and
// lost on restart; the initiator is then re-established by the next interaction
// (durable state is PR D).
type memoryAccess struct {
	mu      sync.Mutex
	threads map[string]*threadAccess
}

type threadAccess struct {
	initiator string
	granted   map[string]bool
}

func newMemoryAccess() *memoryAccess {
	return &memoryAccess{threads: make(map[string]*threadAccess)}
}

// thread returns the per-thread record, creating it on first use. Caller holds mu.
func (m *memoryAccess) thread(threadID string) *threadAccess {
	t, ok := m.threads[threadID]
	if !ok {
		t = &threadAccess{}
		m.threads[threadID] = t
	}
	return t
}

func (m *memoryAccess) SetInitiator(threadID, userID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.thread(threadID)
	if t.initiator == "" {
		t.initiator = userID
	}
	return t.initiator
}

func (m *memoryAccess) Initiator(threadID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.threads[threadID]; ok {
		return t.initiator
	}
	return ""
}

func (m *memoryAccess) Allowed(threadID, userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[threadID]
	if !ok {
		return false
	}
	if userID != "" && userID == t.initiator {
		return true
	}
	return t.granted[userID]
}

func (m *memoryAccess) Grant(threadID, userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.thread(threadID)
	if t.granted == nil {
		t.granted = make(map[string]bool)
	}
	t.granted[userID] = true
}
