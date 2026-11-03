package channels

import (
	"context"
	"time"
)

// EgressRefusal is what the egress gateway refused one agent within one
// observation: a destination the agent's egress does not declare, the
// requests refused, and the Sessions that made them. The refusal notice
// addresses the agent's creator, who may declare the destination.
type EgressRefusal struct {
	// Namespace and Agent name the kagent Agent; DisplayName is how people
	// know it ("" falls back to Agent).
	Namespace   string
	Agent       string
	DisplayName string
	// Creator is the email of the person who created the Agent.
	Creator string
	// Scheme is http, https or tcp; Host and Port the refused destination.
	Scheme string
	Host   string
	Port   int
	// Count is the requests refused in this observation and Sessions the
	// Sessions they came from.
	Count    int
	Sessions []string
	// At is when the refusals were observed.
	At time.Time
	// AgentURL opens the agent where its egress is edited; "" for none.
	AgentURL string
}

// RefusalNotifier tells an agent's creator about its egress refusals: one
// notice per agent and destination within a window, later refusals counted
// into it.
type RefusalNotifier interface {
	NoteRefusal(ctx context.Context, r EgressRefusal) error
}
