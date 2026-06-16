package slack

import "sync"

// BaseMode controls who the agent responds to in a thread.
type BaseMode int

const (
	ModeLocked   BaseMode = iota // owner only (default)
	ModeSelective                // owner + allowlist
	ModeOpen                     // everyone
)

// AccessState tracks per-thread access configuration.
// All methods are safe for concurrent use.
type AccessState struct {
	mu      sync.RWMutex
	Owner   string          // Slack user ID of the thread owner
	Mode    BaseMode
	Observe bool            // ingest all messages but respond only to authorized users
	Allowed map[string]bool // additional authorized users (ModeSelective)
	Banned  map[string]bool // explicitly banned users (overrides all modes)
}

// Permitted returns true when userID may receive an agent response.
func (a *AccessState) Permitted(userID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Banned[userID] {
		return false
	}
	switch a.Mode {
	case ModeOpen:
		return true
	case ModeSelective:
		return userID == a.Owner || a.Allowed[userID]
	default: // ModeLocked
		return userID == a.Owner
	}
}

// Deliver returns true when the message should be forwarded to the agent.
// In observe mode all non-banned messages are delivered; whether the user
// gets a reply is controlled separately by Permitted.
func (a *AccessState) Deliver(userID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Banned[userID] {
		return false
	}
	if a.Observe {
		return true
	}
	// Recheck permission without taking the lock again.
	switch a.Mode {
	case ModeOpen:
		return true
	case ModeSelective:
		return userID == a.Owner || a.Allowed[userID]
	default:
		return userID == a.Owner
	}
}

func (a *AccessState) Open() {
	a.mu.Lock()
	a.Mode = ModeOpen
	a.mu.Unlock()
}

func (a *AccessState) Lock() {
	a.mu.Lock()
	a.Mode = ModeLocked
	a.Allowed = nil
	a.Observe = false
	a.mu.Unlock()
}

func (a *AccessState) ToggleObserve() {
	a.mu.Lock()
	a.Observe = !a.Observe
	a.mu.Unlock()
}

func (a *AccessState) Allow(userID string) {
	a.mu.Lock()
	if a.Allowed == nil {
		a.Allowed = make(map[string]bool)
	}
	a.Allowed[userID] = true
	a.Mode = ModeSelective
	a.mu.Unlock()
}

func (a *AccessState) Ban(userID string) {
	a.mu.Lock()
	if a.Banned == nil {
		a.Banned = make(map[string]bool)
	}
	a.Banned[userID] = true
	a.mu.Unlock()
}
