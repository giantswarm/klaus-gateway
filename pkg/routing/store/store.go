// Package store defines the interface for the klaus-gateway routing table and
// the other records the gateway must not lose while their Slack message is
// live: the team reviews.
//
// A routing entry maps (channel, channel-id, thread) to everything the gateway
// holds for that conversation: the agent and the kagent AgentInstance its
// turns are routed to. A review record is one posted team review and its
// decision state (Review). Stores persist both across restarts where possible
// (bolt, valkey) or keep them in memory.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by Get when no entry matches the key.
var ErrNotFound = errors.New("routing entry not found")

// Key identifies a conversation thread. A thread is shared by its
// participants, so every participant reaches the same row.
type Key struct {
	Channel   string
	ChannelID string
	ThreadID  string
}

// String returns the canonical serialised form used as a storage key: three
// pipe-separated parts. The format is stable: stores rely on it for on-disk
// keys.
func (k Key) String() string {
	return strings.Join([]string{
		escape(k.Channel),
		escape(k.ChannelID),
		escape(k.ThreadID),
	}, "|")
}

// ParseKey inverts Key.String. A key written by a gateway older than the
// Slack-only release carried a fourth (user) part; it no longer parses, and a
// store that lists its keys skips the row — the thread's next message writes
// it afresh.
func ParseKey(s string) (Key, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 {
		return Key{}, fmt.Errorf("invalid key %q: expected 3 parts", s)
	}
	return Key{
		Channel:   unescape(parts[0]),
		ChannelID: unescape(parts[1]),
		ThreadID:  unescape(parts[2]),
	}, nil
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "|", `\p`)
}

func unescape(s string) string {
	s = strings.ReplaceAll(s, `\p`, "|")
	return strings.ReplaceAll(s, `\\`, `\`)
}

// Entry is the one row a conversation thread has: everything the gateway must
// not lose across a restart. The agent the thread is bound to and its
// AgentInstance plus the task in flight on it, and the channel's own facts
// about the thread, its initiator and the users it granted.
//
// Several writers read-modify-write this row (a channel's grant, the facade's
// task record, the binding), so they write through Store.Update rather than
// Put, and each keeps the fields it does not own.
type Entry struct {
	// AgentRef is the agent the thread is bound to, in the shape the
	// deployment spells it. Never "" standing for the default: a changed
	// default must not fork the conversation.
	AgentRef string `json:"agent_ref,omitempty"`
	// AgentInstanceID is the kagent AgentInstance (a controller-assigned UUID)
	// the conversation's A2A turns are routed to.
	AgentInstanceID string `json:"agent_instance_id,omitempty"`
	// TaskID is the A2A task running on AgentInstanceID while a turn is in
	// flight, cleared when the turn ends. A gateway that restarts mid-turn finds
	// here the tasks it has to resubscribe to.
	TaskID string `json:"task_id,omitempty"`
	// Resume is the channel-private data needed to deliver TaskID's result
	// after a restart (the channel adapter owns its keys). Set with TaskID.
	Resume map[string]string `json:"resume,omitempty"`
	// Delivered is what the channel adapter has already rendered of TaskID's
	// turn, written as the turn streams, so the process that resubscribes
	// after a restart continues the reply where this one left it instead of
	// repeating it. Reset with TaskID, cleared with it.
	Delivered Delivered `json:"delivered,omitzero"`
	// Initiator is the user whose mention launched the thread, and Granted the
	// users that initiator allowed into it. Written by the channel adapter:
	// the facts it cannot recover after a restart.
	Initiator string   `json:"initiator,omitempty"`
	Granted   []string `json:"granted,omitempty"`

	CreatedAt time.Time     `json:"created_at"`
	LastSeen  time.Time     `json:"last_seen"`
	TTL       time.Duration `json:"ttl"`
}

// Delivered is the part of an in-flight turn's reply that has reached the
// channel: the answer text that landed and the tool-step receipt still open.
// A resubscription after a restart is handed the whole answer at completion
// and no replay of the tool calls it missed, so this is what lets it post
// only the text that follows and keep counting the steps.
type Delivered struct {
	// TextLen is the length, in bytes, of the answer text posted so far.
	TextLen int `json:"text_len,omitempty"`
	// StreamTS is the streamed message the answer is landing in, empty when
	// none is open, and StreamLen how many bytes of it that message carries.
	// A process continuing the turn appends to that message instead of opening
	// a second one, and counts on toward the per-message text cap.
	StreamTS  string `json:"stream_ts,omitempty"`
	StreamLen int    `json:"stream_len,omitempty"`
	// ToolSteps counts the tool calls of the receipt segment open when the
	// record was written, ToolOrder their distinct names in first-use order
	// and ToolCounts the calls per name.
	ToolSteps  int            `json:"tool_steps,omitempty"`
	ToolOrder  []string       `json:"tool_order,omitempty"`
	ToolCounts map[string]int `json:"tool_counts,omitempty"`
}

// IsZero reports whether nothing has been delivered; encoding/json's omitzero
// drops the field then.
func (d Delivered) IsZero() bool {
	return d.TextLen == 0 && d.StreamTS == "" && d.ToolSteps == 0
}

// Expired reports whether the entry has aged past its TTL relative to now.
// A zero TTL means never expire.
func (e Entry) Expired(now time.Time) bool {
	if e.TTL <= 0 {
		return false
	}
	return now.Sub(e.LastSeen) > e.TTL
}

// KeyEntry pairs a Key with its Entry for listing.
type KeyEntry struct {
	Key   Key
	Entry Entry
}

// Review is one team review — the ask a manager posted into a team's channel
// through POST /reviews — and its decision state: everything the Slack
// adapter needs to resolve a click on the message's Approve button, kept for
// as long as the button is live (TTL from PostedAt) so a gateway restart does
// not turn an open review into an expired one.
type Review struct {
	// ID is the gateway's handle on the review; the Approve button carries it.
	ID string `json:"id"`
	// Channel is the Slack channel the review was posted to and TS the message
	// it is: the decision rewrites that message in place.
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	// Team, Text and Link are the ask as posted: the team whose linked members
	// may decide, the change spelled out, the link for anything else.
	Team string `json:"team"`
	Text string `json:"text"`
	Link string `json:"link,omitempty"`
	// Actor is the email of the person whose action the review decides; ""
	// for a review without one. Their own approval is refused.
	Actor string `json:"actor,omitempty"`
	// PullRequests are the pull requests the change lands as, as URLs.
	PullRequests []string `json:"pull_requests,omitempty"`
	// Tool and Arguments are the muster tool call a member's approval makes,
	// verbatim, under that member's identity.
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// DenyTool and DenyArguments are the call a member's denial makes, with
	// the typed reason added; "" for a review without a Deny button.
	DenyTool      string         `json:"deny_tool,omitempty"`
	DenyArguments map[string]any `json:"deny_arguments,omitempty"`

	// DecidedBy is the user whose decision is in flight or done; "" while the
	// review is open. ClaimedAt is when that user's click took the review.
	DecidedBy string    `json:"decided_by,omitempty"`
	ClaimedAt time.Time `json:"claimed_at,omitzero"`
	// Done is set once the decision went through; the record then stays until
	// its TTL so a late click is told who decided. Denied says the decision
	// in flight or done is a denial.
	Done   bool `json:"done,omitempty"`
	Denied bool `json:"denied,omitempty"`
	// Status is the line the team reads under the buttons: the latest attempt
	// that did not decide the review. "" shows none.
	Status string `json:"status,omitempty"`

	PostedAt time.Time     `json:"posted_at"`
	TTL      time.Duration `json:"ttl"`
}

// Expired reports whether the review has aged past its TTL relative to now.
// A zero TTL means never expire.
func (r Review) Expired(now time.Time) bool {
	if r.TTL <= 0 {
		return false
	}
	return now.Sub(r.PostedAt) > r.TTL
}

// ReviewStore keeps team-review records (Review) by id.
type ReviewStore interface {
	// PutReview upserts a review record; one that has already expired is
	// removed instead.
	PutReview(ctx context.Context, r Review) error
	// GetReview returns the record for id, or (_, false, nil) when it is
	// unknown or expired.
	GetReview(ctx context.Context, id string) (Review, bool, error)
	// UpdateReview applies mutate to the record at id and writes the result
	// when mutate returns true. found is false for an unknown or expired
	// review; mutate is not called then. The read-modify-write is atomic
	// against every other writer of the record, replicas included: a backend
	// shared by processes writes only when the record is unchanged since it
	// was read and re-reads otherwise, so mutate may run more than once and
	// must derive what it reports from the record it is given.
	UpdateReview(ctx context.Context, id string, mutate func(r *Review) bool) (found bool, err error)
}

// Store is the routing-table backend, and the review store with it: one
// durable state for the gateway.
type Store interface {
	Get(ctx context.Context, k Key) (Entry, bool, error)
	Put(ctx context.Context, k Key, e Entry) error
	Delete(ctx context.Context, k Key) error
	List(ctx context.Context) ([]KeyEntry, error)
	// Update applies mutate to the entry at k — the zero Entry when none is
	// live — and writes the result when mutate returns true. Read-modify-write
	// by different writers on one key (a channel's grant, the facade's task
	// record, the binding) is serialised inside the process, so no writer loses
	// another's fields. A second replica is not protected: the stores have no
	// CAS here (UpdateReview has one).
	Update(ctx context.Context, k Key, mutate func(e *Entry, found bool) bool) error
	ReviewStore
	Close() error
}
