// Package channels defines the surface channel adapters (web, Slack, CLI)
// share with the rest of klaus-gateway.
//
// Adapters receive a Gateway facade from the server wiring and call into it
// to resolve identity to an instance, stream a completion, or fetch history.
// They never depend on the routing store, lifecycle driver, or upstream URL
// directly -- that wiring lives in the facade implementation.
package channels

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// BearerToken returns the raw value of an `Authorization: Bearer` header, or
// an empty string when absent.
func BearerToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// InstanceRef re-exports lifecycle.InstanceRef so adapters depend on this
// package only.
type InstanceRef = lifecycle.InstanceRef

// ChannelAdapter is the interface each channel (web, slack, cli) implements.
// Start is called once during server boot with the Gateway facade; Stop
// drains any adapter-owned goroutines on shutdown.
type ChannelAdapter interface {
	Name() string
	Start(ctx context.Context, gw Gateway) error
	Stop(ctx context.Context) error
}

// Gateway is the server-side surface adapters call back into. The wiring in
// main.go provides the concrete implementation (Facade).
type Gateway interface {
	Resolve(ctx context.Context, in InboundMessage) (InstanceRef, error)
	SendCompletion(ctx context.Context, ref InstanceRef, msg InboundMessage) (<-chan OutboundDelta, error)
	FetchHistory(ctx context.Context, ref InstanceRef) ([]Message, error)
}

// InboundMessage is the normalised shape each adapter hands to the gateway.
type InboundMessage struct {
	Channel     string
	ChannelID   string
	UserID      string
	ThreadID    string
	Text        string
	Attachments []Attachment
	ReplyTo     string
	// Subject is the authenticated user's OAuth `sub` when available.
	Subject string
	// BearerToken is the caller's raw inbound bearer token, forwarded on the
	// A2A egress request so kagent sees the end-user identity. Empty for
	// channels without a per-user token (e.g. Slack).
	BearerToken string
	// AgentRef is the target agent name. When set, SendCompletion routes
	// through the A2A executor instead of the OpenAI /v1 path.
	AgentRef string
}

// DeltaKind classifies the content of an OutboundDelta. The zero value is
// DeltaText so existing callers that leave Kind unset are unaffected.
type DeltaKind int

const (
	DeltaText   DeltaKind = iota // regular assistant text
	DeltaPrompt                  // agent is waiting for user input (input-required / auth-required)
)

// OutboundDelta is one chunk streamed from an instance back through an
// adapter. Content may be empty on the terminal delta. Err, when non-nil,
// signals an upstream or gateway failure; the channel is closed after.
type OutboundDelta struct {
	Kind    DeltaKind
	Content string // assistant text, or the prompt body for DeltaPrompt
	Done    bool   // terminal: no more deltas follow
	Err     error  // upstream/gateway failure; channel is closed after
}

// Attachment is an inbound file/image payload.
type Attachment struct {
	Filename    string
	ContentType string
	Bytes       []byte
}

// Message is a single stored turn returned by FetchHistory.
type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	SentAt  time.Time `json:"sent_at,omitempty"`
}
