package slack

import (
	"sync"
	"time"
)

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

// threadAccessTTL bounds how long a thread stays "active" (initiator and
// grants retained) without any interaction. A thread past the TTL needs a
// fresh @-mention to re-engage the bot, so a long-lived pod does not keep
// consuming un-mentioned replies in abandoned threads. Sliding: every handled
// message refreshes the deadline via SetInitiator.
const threadAccessTTL = 24 * time.Hour

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
	expires   time.Time
}

func newMemoryAccess() *memoryAccess {
	return &memoryAccess{threads: make(map[string]*threadAccess)}
}

// thread returns the per-thread record, creating it on first use and
// refreshing its eviction deadline. Expired siblings are swept
// opportunistically. Caller holds mu.
func (m *memoryAccess) thread(threadID string, now time.Time) *threadAccess {
	for id, t := range m.threads {
		if now.After(t.expires) {
			delete(m.threads, id)
		}
	}
	t, ok := m.threads[threadID]
	if !ok {
		t = &threadAccess{}
		m.threads[threadID] = t
	}
	t.expires = now.Add(threadAccessTTL)
	return t
}

// live returns the thread record if present and not expired. Caller holds mu.
func (m *memoryAccess) live(threadID string, now time.Time) (*threadAccess, bool) {
	t, ok := m.threads[threadID]
	if !ok || now.After(t.expires) {
		return nil, false
	}
	return t, true
}

func (m *memoryAccess) SetInitiator(threadID, userID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.thread(threadID, time.Now())
	if t.initiator == "" {
		t.initiator = userID
	}
	return t.initiator
}

func (m *memoryAccess) Initiator(threadID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.live(threadID, time.Now()); ok {
		return t.initiator
	}
	return ""
}

func (m *memoryAccess) Allowed(threadID, userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.live(threadID, time.Now())
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
	t := m.thread(threadID, time.Now())
	if t.granted == nil {
		t.granted = make(map[string]bool)
	}
	t.granted[userID] = true
}
