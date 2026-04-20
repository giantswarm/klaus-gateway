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
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

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
// cmd/klaus-gateway provides the concrete implementation (Facade).
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
}

// OutboundDelta is one chunk streamed from an instance back through an
// adapter. Content may be empty on the terminal delta. Err, when non-nil,
// signals an upstream or gateway failure; the channel is closed after.
type OutboundDelta struct {
	Content string
	Done    bool
	Err     error
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
