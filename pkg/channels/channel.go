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
	Channel   string
	ChannelID string
	UserID    string
	ThreadID  string
	// MessageID is the platform-specific ID of the triggering message (the Slack
	// message ts). Used as the target for progress reactions. May be empty.
	MessageID   string
	Text        string
	Attachments []Attachment
	ReplyTo     string
	// Subject is the authenticated user's OAuth `sub` when available.
	Subject string
	// BearerToken is the caller's raw inbound bearer token, forwarded on the
	// A2A egress request so kagent sees the end-user identity. Empty for
	// channels without a per-user token (e.g. Slack).
	BearerToken string
	// Author, when non-empty, is the real end-user who wrote this turn in a
	// shared session that runs under a different (delegated) identity, such as a
	// Slack thread acting under its initiator. Surfaced to the agent as
	// attribution; BearerToken remains the acting identity.
	Author string
	// AgentRef is the target agent name. When set, SendCompletion routes
	// through the A2A executor instead of the OpenAI /v1 path.
	AgentRef string
	// TaskID, when set, continues an existing A2A task rather than starting a
	// new one. Populated by the Slack adapter when a pending input-required task
	// exists for the thread.
	TaskID string
	// Decision, when set, resumes a paused input-required task with a structured
	// HITL answer (approve/reject or ask_user answers) sent as an A2A DataPart.
	// When nil the Text is sent as a plain text part.
	Decision *HitlDecision
}

// DeltaKind classifies the content of an OutboundDelta. The zero value is
// DeltaText so existing callers that leave Kind unset are unaffected.
type DeltaKind int

const (
	DeltaText         DeltaKind = iota // regular assistant text
	DeltaPrompt                        // agent is waiting for user input (input-required / auth-required)
	DeltaToolActivity                  // agent invoked or received a tool result
	DeltaNarration                     // interim prose the agent wrote before firing its tool calls
)

// TurnUsage holds the token counts reported for a turn, in provider-neutral
// terms aligned with the OpenTelemetry GenAI semantic conventions
// (gen_ai.usage.input_tokens / gen_ai.usage.output_tokens). Each producer maps
// its own vocabulary in (kagent/genai candidatesTokenCount, OpenAI
// completion_tokens, ...). Any field the provider does not report stays zero.
type TurnUsage struct {
	InputTokens  int // gen_ai.usage.input_tokens
	OutputTokens int // gen_ai.usage.output_tokens
	TotalTokens  int // provider-reported total; no OTel semconv key (usually input+output)
}

// ToolActivityKind distinguishes a tool call from its result.
type ToolActivityKind int

const (
	ToolCall ToolActivityKind = iota
	ToolResult
)

// ToolActivity is the provider-neutral shape of a tool call or its result,
// surfaced on a DeltaToolActivity. Adapters render it without knowing the
// upstream (kagent/ADK) metadata layout; the translation lives in hitl_parse.go.
type ToolActivity struct {
	Name     string           // tool name
	Kind     ToolActivityKind // call or result
	CallID   string           // correlates a call with its response
	Args     map[string]any   // call arguments; nil for a result
	Response map[string]any   // result payload; nil for a call
}

// OutboundDelta is one chunk streamed from an instance back through an
// adapter. Content may be empty on the terminal delta. Err, when non-nil,
// signals an upstream or gateway failure; the channel is closed after.
type OutboundDelta struct {
	Kind    DeltaKind
	Content string // assistant text, or the prompt body for DeltaPrompt
	Done    bool   // terminal: no more deltas follow
	Err     error  // upstream/gateway failure; channel is closed after
	// TaskID is populated on DeltaPrompt deltas to identify the A2A task that
	// is paused waiting for input. The Slack adapter stores it so the next
	// message (or button click) can resume the same task.
	TaskID string
	// Prompt is populated on DeltaPrompt deltas when the input-required status
	// carried a structured adk_request_confirmation DataPart (tool approval or
	// ask_user). Nil for a plain-text prompt.
	Prompt *HitlPrompt
	// Usage carries the token counts reported for the turn. Populated on the
	// terminal delta (and any interim event that reports usage); nil otherwise.
	Usage *TurnUsage
	// Tool is populated on DeltaToolActivity deltas with the tool call or result;
	// nil otherwise.
	Tool *ToolActivity
}

// StreamText is the delta's content as a plain-text stream renders it. Adapters
// that concatenate every chunk into one reply (web, cli) have no side-message
// concept, so narration is closed with a paragraph break instead of running into
// the answer that follows it.
func (d OutboundDelta) StreamText() string {
	if d.Kind == DeltaNarration && d.Content != "" {
		return d.Content + "\n\n"
	}
	return d.Content
}

// isZero reports whether the delta carries no channel-visible payload. Used
// instead of `delta == OutboundDelta{}` because the struct embeds an error
// interface, and == panics when the concrete error type is not comparable.
func (d OutboundDelta) isZero() bool {
	return d.Kind == DeltaText && d.Content == "" && !d.Done && d.Err == nil &&
		d.TaskID == "" && d.Prompt == nil && d.Usage == nil && d.Tool == nil
}

// Attachment is an inbound file/image payload.
type Attachment struct {
	Filename    string
	ContentType string
	// SourceURL is where the raw bytes are fetched from when they are not
	// inlined at parse time (e.g. a Slack url_private needing the bot token).
	// A channel adapter downloads it and fills Bytes before the turn dispatches.
	SourceURL string
	// Size is the source's declared byte size, 0 when unknown. It bounds the
	// download as a memory guard; it is not a product limit.
	Size  int
	Bytes []byte
}

// Message is a single stored turn returned by FetchHistory.
type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	SentAt  time.Time `json:"sent_at,omitzero"`
}
