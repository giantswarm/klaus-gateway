package slack

import "sync"

// BaseMode controls who the agent responds to in a thread.
type BaseMode int

const (
	ModeLocked    BaseMode = iota // owner only (default)
	ModeSelective                 // owner + allowlist
	ModeOpen                      // everyone
)

// Access-mode config strings accepted in SlackConfig.DefaultAccessMode.
const (
	accessModeLocked  = "locked"
	accessModeOpen    = "open"
	accessModeObserve = "observe"
)

// AccessState is the per-thread access policy. It is seeded once by
// Adapter.getAccess (owner/mode/allowlist) under the adapter lock, and may
// then be mutated at runtime by in-thread commands (/open, /lock, /observe).
// Since command handling and message dispatch can run concurrently, all
// reads and mutations go through mu.
type AccessState struct {
	mu      sync.RWMutex
	owner   string          // Slack user ID of the thread owner
	mode    BaseMode        // base response policy
	observe bool            // ingest all messages but respond only to authorized users
	allowed map[string]bool // additional authorized users (ModeSelective)
}

// Permitted reports whether userID may receive an agent response.
func (a *AccessState) Permitted(userID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.permittedLocked(userID)
}

func (a *AccessState) permittedLocked(userID string) bool {
	switch a.mode {
	case ModeOpen:
		return true
	case ModeSelective:
		return userID == a.owner || a.allowed[userID]
	default: // ModeLocked
		return userID == a.owner
	}
}

// Deliver reports whether the message should be forwarded to the agent. In
// observe mode every message is forwarded for context; whether the user gets
// a reply is decided separately by Permitted.
func (a *AccessState) Deliver(userID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.observe {
		return true
	}
	return a.permittedLocked(userID)
}

// IsOwner reports whether userID is the thread owner.
func (a *AccessState) IsOwner(userID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return userID == a.owner
}

// Invite adds the named users to the thread's allowlist and switches to
// selective mode (owner + allowlist). It is additive: inviting more users
// keeps the previously invited ones.
func (a *AccessState) Invite(userIDs ...string) {
	a.mu.Lock()
	a.mode = ModeSelective
	if a.allowed == nil {
		a.allowed = make(map[string]bool, len(userIDs))
	}
	for _, u := range userIDs {
		a.allowed[u] = true
	}
	a.mu.Unlock()
}

// Lock restricts responses to the thread owner and clears observe/allowlist.
func (a *AccessState) Lock() {
	a.mu.Lock()
	a.mode = ModeLocked
	a.allowed = nil
	a.observe = false
	a.mu.Unlock()
}
