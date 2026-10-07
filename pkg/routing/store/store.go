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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

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

// skippedKeysOnce guards the one Info line per process. Readiness lists on
// every probe for the memory and bolt stores, so the per-call record has to
// stay at Debug; the first skip is also the evidence an operator needs for the
// upgrade, and installations run at info.
var skippedKeysOnce sync.Once

// LogSkippedKeys reports the rows a List had to leave out because their key is
// of an older layout — every row written before the key lost its user slot.
// The first such List in the process logs once at Info, so the upgrade leaves
// evidence at the level installations run at; every List after it records the
// count at Debug. Nothing is logged when none were skipped.
func LogSkippedKeys(backend string, skipped int) {
	if skipped == 0 {
		return
	}
	skippedKeysOnce.Do(func() {
		slog.Info("routing store: rows written before the Slack-only release were skipped, their key carries the old user slot; the threads they belong to start over on their next message",
			"record", "store_keys_skipped", "backend", backend, "skipped", skipped)
	})
	slog.Debug("routing store: rows skipped, their key is of an older layout",
		"backend", backend, "skipped", skipped)
}

// ResetSkippedKeysOnce re-arms the one-per-process Info line. For tests only.
func ResetSkippedKeysOnce() { skippedKeysOnce = sync.Once{} }

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
// task record, the binding), so they write through Store.Update, and each
// keeps the fields it does not own.
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
	// Share is the AgentInstance share the thread's granted users run their
	// turns through: the instance belongs to its creator, and the share lets
	// another person act on it as themselves. Bound to AgentInstanceID and
	// dropped with the binding.
	Share *Share `json:"share,omitempty"`
	// InstanceCreator is the channel user whose token created
	// AgentInstanceID: the instance is theirs, so their turns need no share
	// and its lifecycle (minting and revoking the share, a reset) runs under
	// their token. Usually the initiator; a collaborator when their turn
	// opened the binding while the initiator was signed out. Empty on a row
	// written before it was recorded, which reads as the initiator. Dropped
	// with the binding.
	InstanceCreator string `json:"instance_creator,omitempty"`
	// Initiator is the user whose mention launched the thread, and Granted the
	// users that initiator allowed into it. Written by the channel adapter:
	// the facts it cannot recover after a restart.
	Initiator string   `json:"initiator,omitempty"`
	Granted   []string `json:"granted,omitempty"`
	// Held is what the channel adapter holds for the thread between turns
	// and must not lose across a restart: the messages it parked while their
	// sender signs in or the initiator decides on them, and the prompt a
	// paused task waits on. The adapter owns its shape and the expiry of what
	// it holds.
	Held json.RawMessage `json:"held,omitempty"`
	// MutedAt is the channel's timestamp of the message that muted the
	// thread, "" while it is not muted: the agent then answers only messages
	// that mention it, and the first one to reach a turn ends the mute. Its
	// own field, not part of Held: it lives only in the row, and the gate that
	// reads it on every thread reply does not decode Held.
	MutedAt string `json:"muted_at,omitempty"`

	CreatedAt time.Time     `json:"created_at"`
	LastSeen  time.Time     `json:"last_seen"`
	TTL       time.Duration `json:"ttl"`
}

// Share is a thread's AgentInstance share. The token is held sealed: it is a
// bearer capability on the instance, so a copy of the row must not be enough
// to use it.
type Share struct {
	// ID names the share at the controller, for a revoke.
	ID string `json:"id"`
	// InstanceID is the AgentInstance the share was minted for.
	InstanceID string `json:"instance_id"`
	// Sealed is the share token, encrypted and bound to the thread and the
	// instance.
	Sealed []byte `json:"sealed"`
}

// Delivered is the part of an in-flight turn's reply that has reached the
// channel: the answer text that landed and the message it landed in. A
// resubscription after a restart is handed the whole answer at completion, so
// this is what lets it post only the text that follows.
type Delivered struct {
	// TextLen is the length, in bytes, of the answer text posted so far.
	TextLen int `json:"text_len,omitempty"`
	// StreamTS is the streamed message the answer is landing in, empty when
	// none is open, and StreamLen how many bytes of it that message carries.
	// A process continuing the turn appends to that message instead of opening
	// a second one, and counts on toward the per-message text cap.
	StreamTS  string `json:"stream_ts,omitempty"`
	StreamLen int    `json:"stream_len,omitempty"`
}

// IsZero reports whether nothing has been delivered; encoding/json's omitzero
// drops the field then.
func (d Delivered) IsZero() bool {
	return d.TextLen == 0 && d.StreamTS == ""
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

	// Decision is set on a decision — a question put to a person or a team
	// through POST /decisions — and nil on a team review. A decision keeps the
	// review's fields for what they are there: Channel and TS its message,
	// Team the team it went to ("" for a person), Text the question, Tool and
	// Arguments the answer tool, and the claim (DecidedBy, ClaimedAt, Done,
	// Status) as for a review.
	Decision *Decision `json:"decision,omitempty"`
	// Conversation is set on a conversation — a thread a service holds with
	// one person through POST /conversations — and nil otherwise. It keeps
	// the review's fields for what they are there: Channel and TS its opening
	// message, Text the opening text, Tool and Arguments the tool a reply of
	// the person's calls. Its TTL slides with every message.
	Conversation *Conversation `json:"conversation,omitempty"`

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

// Decision is what a decision adds to its record: the question's context,
// the options, the due time with its default, and how it closed.
type Decision struct {
	// Note names the asker's note the decision is, for the message's context
	// line.
	Note      string           `json:"note,omitempty"`
	StatusQuo string           `json:"status_quo"`
	Options   []DecisionOption `json:"options,omitempty"`
	// Recommend is the recommended option, 1-based; 0 recommends none.
	Recommend int       `json:"recommend,omitempty"`
	Due       time.Time `json:"due"`
	Default   string    `json:"default"`
	AskedBy   string    `json:"asked_by"`

	// Outcome is how the decision closed (DecisionAnswered, DecisionDefaulted,
	// DecisionWithdrawn); "" while it is open. Choice (1-based, 0 for none)
	// and Answer are what the person answered here, ClosedAt when it closed,
	// and CloseText the text its close carried.
	Outcome   string    `json:"outcome,omitempty"`
	Choice    int       `json:"choice,omitempty"`
	Answer    string    `json:"answer,omitempty"`
	CloseText string    `json:"close_text,omitempty"`
	ClosedAt  time.Time `json:"closed_at,omitzero"`
}

// Conversation is what a conversation adds to its record: whom it is with
// and who talks to them.
type Conversation struct {
	// Person is the email of the person the conversation is with, and
	// SlackUser their Slack user: only their replies are delivered.
	Person    string `json:"person"`
	SlackUser string `json:"slack_user"`
	// From names the agent the person talks to.
	From string `json:"from"`
}

// DecisionOption is one answer a decision offers: a short label and what
// choosing it does.
type DecisionOption struct {
	Label       string `json:"label"`
	Consequence string `json:"consequence,omitempty"`
}

// The outcomes a decision closes with.
const (
	DecisionAnswered  = "answered"
	DecisionDefaulted = "defaulted"
	DecisionWithdrawn = "withdrawn"
)

// ReviewStore keeps team-review records (Review), decisions among them, by id.
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
