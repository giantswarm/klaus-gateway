package slack

// BaseMode controls who the agent responds to in a thread.
type BaseMode int

const (
	ModeLocked    BaseMode = iota // owner only (default)
	ModeSelective                 // owner + allowlist
	ModeOpen                      // everyone
)

// AccessState is the per-thread access policy. It is built once by
// Adapter.getAccess under the adapter lock and treated as immutable
// afterwards (config seeds owner/mode/allowlist; there is no runtime
// mutation path yet), so the read methods need no synchronisation.
type AccessState struct {
	owner   string          // Slack user ID of the thread owner
	mode    BaseMode        // base response policy
	observe bool            // ingest all messages but respond only to authorized users
	allowed map[string]bool // additional authorized users (ModeSelective)
}

// Permitted reports whether userID may receive an agent response.
func (a *AccessState) Permitted(userID string) bool {
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
	if a.observe {
		return true
	}
	return a.Permitted(userID)
}
