// Package channels defines the surface channel adapters share with the rest
// of klaus-gateway.
//
// Adapters receive a Gateway facade from the server wiring and call into it to
// stream a completion. They never depend on the routing store or the kagent
// client directly -- that wiring lives in the facade implementation.
package channels

import (
	"context"
	"errors"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// ErrShutdown is the cancellation cause of a turn the gateway's own shutdown
// cut short. A channel adapter cancels its lifecycle context with it (see
// context.WithCancelCause), so everything downstream can tell a restart from a
// user's /stop: the facade leaves such a task running at the controller — its
// result is delivered after the restart — where a plain cancellation cancels
// the task server-side.
var ErrShutdown = errors.New("channels: the gateway is shutting down")

// ChannelAdapter is the interface each channel implements.
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
	SendCompletion(ctx context.Context, msg InboundMessage) (<-chan OutboundDelta, error)
}

// InboundMessage is the normalised shape each adapter hands to the gateway.
type InboundMessage struct {
	Channel   string
	ChannelID string
	ThreadID  string
	// MessageID is the platform-specific ID of the triggering message (the Slack
	// message ts). Used as the target for progress reactions. May be empty.
	MessageID   string
	Text        string
	Attachments []Attachment
	// Context, when non-empty, is shared material the channel gathered for
	// this turn alone — the Slack adapter renders the messages a thread
	// already held when a conversation opened inside it. It reaches the agent
	// as a labelled leading part, so the model can tell what other people
	// wrote from what the user is asking. It is never thread state: only the
	// turn that opens a conversation carries it.
	Context string
	// Subject is the authenticated user's OAuth `sub` when available.
	Subject string
	// BearerToken is the person's token (the Slack user's Dex id_token),
	// forwarded on the A2A request so kagent sees the end-user identity. A turn
	// without one is refused.
	BearerToken string
	// Author, when non-empty, is the real end-user who wrote this turn in a
	// shared session that runs under a different (delegated) identity, such as a
	// Slack thread acting under its initiator. Surfaced to the agent as
	// attribution; BearerToken remains the acting identity.
	Author string
	// AgentRef is the target agent name: the agent the turn runs on.
	AgentRef string
	// Opener is set by a channel adapter when this message starts its
	// thread's conversation (no agent recorded for the thread before it): the
	// session title keys on it.
	Opener bool
	// Title, when set, is this message as a one-line title, from an adapter
	// that renders one itself — Slack drops the mention and the command a user
	// typed to address the bot. It names the kagent conversation a turn
	// creates. Empty means "not supplied", and the title is derived from Text
	// instead: an adapter with nothing to name a conversation after leaves Text
	// with nothing in it either, so the two agree on saying nothing.
	Title string
	// TaskID, when set, continues an existing A2A task rather than starting a
	// new one. Populated by the Slack adapter when a pending input-required task
	// exists for the thread.
	TaskID string
	// Decision, when set, resumes a paused input-required task with a structured
	// HITL answer (approve/reject or ask_user answers) sent as an A2A DataPart.
	// When nil the Text is sent as a plain text part.
	Decision *HitlDecision
	// Resume is what the adapter needs to deliver this turn's result should the
	// gateway restart while the turn runs: channel-private keys (the Slack
	// channel and user, the message to react on) the facade stores next to the
	// thread's binding and hands back on InFlightTurn.
	Resume map[string]string
	// ReceivedAt is when the channel received the message (the events POST,
	// the Socket Mode frame, the HTTP request): the start of the turn's
	// timeline (TurnTimer). Zero means "when the turn began".
	ReceivedAt time.Time
}

// InFlightTurn is a turn a previous gateway process left running at its
// controller: the task to resubscribe to and the thread it belongs to. Msg
// carries the thread's identity (Channel, ChannelID, ThreadID, AgentRef) and
// the Resume data the turn was dispatched with; Delivered is what the
// previous process had already rendered of the reply, for the adapter to
// continue from rather than repeat.
type InFlightTurn struct {
	Msg       InboundMessage
	TaskID    string
	Delivered store.Delivered
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
	// AwaitsApproval marks a result that is not the tool's output: the runtime
	// answered the call with its request for the person's approval, and the
	// tool runs only once the approval prompt is decided.
	AwaitsApproval bool
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

// isZero reports whether the delta carries no channel-visible payload. Used
// instead of `delta == OutboundDelta{}` because the struct embeds an error
// interface, and == panics when the concrete error type is not comparable.
func (d OutboundDelta) isZero() bool {
	return d.Kind == DeltaText && d.Content == "" && !d.Done && d.Err == nil &&
		d.TaskID == "" && d.Prompt == nil && d.Usage == nil && d.Tool == nil
}

// shows reports whether the delta puts something in front of the person.
func (d OutboundDelta) shows() bool {
	return d.Content != "" || d.Tool != nil || d.Prompt != nil || d.Kind == DeltaPrompt
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
