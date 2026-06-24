package slack

import "sync"

// BaseMode controls who the agent responds to in a thread.
type BaseMode int

const (
	ModeLocked    BaseMode = iota // owner only (default)
	ModeSelective                 // owner + allowlist
	ModeOpen                      // everyone
)

// AccessState tracks per-thread access configuration. The fields are
// unexported and guarded by mu so the mutex is the only path to mutate them;
// all methods are safe for concurrent use.
type AccessState struct {
	mu      sync.RWMutex
	owner   string          // Slack user ID of the thread owner
	mode    BaseMode        // base response policy
	observe bool            // ingest all messages but respond only to authorized users
	allowed map[string]bool // additional authorized users (ModeSelective)
	banned  map[string]bool // explicitly banned users (overrides all modes)
}

// permittedLocked is the response policy. The caller must hold mu (read or
// write). Keeping it lock-free lets Deliver reuse it without recursively
// read-locking, which can deadlock if a writer is queued in between.
func (a *AccessState) permittedLocked(userID string) bool {
	if a.banned[userID] {
		return false
	}
	switch a.mode {
	case ModeOpen:
		return true
	case ModeSelective:
		return userID == a.owner || a.allowed[userID]
	default: // ModeLocked
		return userID == a.owner
	}
}

// Permitted returns true when userID may receive an agent response.
func (a *AccessState) Permitted(userID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.permittedLocked(userID)
}

// Deliver returns true when the message should be forwarded to the agent.
// In observe mode all non-banned messages are delivered; whether the user
// gets a reply is controlled separately by Permitted.
func (a *AccessState) Deliver(userID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.banned[userID] {
		return false
	}
	if a.observe {
		return true
	}
	return a.permittedLocked(userID)
}

func (a *AccessState) Open() {
	a.mu.Lock()
	a.mode = ModeOpen
	a.mu.Unlock()
}

func (a *AccessState) Lock() {
	a.mu.Lock()
	a.mode = ModeLocked
	a.allowed = nil
	a.observe = false
	a.mu.Unlock()
}

func (a *AccessState) ToggleObserve() {
	a.mu.Lock()
	a.observe = !a.observe
	a.mu.Unlock()
}

func (a *AccessState) Allow(userID string) {
	a.mu.Lock()
	if a.allowed == nil {
		a.allowed = make(map[string]bool)
	}
	a.allowed[userID] = true
	a.mode = ModeSelective
	a.mu.Unlock()
}

func (a *AccessState) Ban(userID string) {
	a.mu.Lock()
	if a.banned == nil {
		a.banned = make(map[string]bool)
	}
	a.banned[userID] = true
	a.mu.Unlock()
}
