// Package store defines the interface for the klaus-gateway routing table.
//
// A routing entry maps (channel, channel-id, user, thread) to everything the
// gateway holds for that conversation: the klaus instance that owns it, or the
// agent and the kagent AgentInstance its turns are routed to. Stores persist
// this mapping across restarts where possible (bolt, valkey) or keep it in
// memory.
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

// Key identifies a conversation across channels. The user slot is empty for a
// thread shared by its participants — Slack, and every kagent binding — so
// every participant reaches the same row; web and CLI route per user.
type Key struct {
	Channel   string
	ChannelID string
	UserID    string
	ThreadID  string
}

// String returns the canonical serialised form used as a storage key: four
// pipe-separated parts. The format is stable: stores rely on it for on-disk
// keys.
func (k Key) String() string {
	return strings.Join([]string{
		escape(k.Channel),
		escape(k.ChannelID),
		escape(k.UserID),
		escape(k.ThreadID),
	}, "|")
}

// ParseKey inverts Key.String.
func ParseKey(s string) (Key, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 4 {
		return Key{}, fmt.Errorf("invalid key %q: expected 4 parts", s)
	}
	return Key{
		Channel:   unescape(parts[0]),
		ChannelID: unescape(parts[1]),
		UserID:    unescape(parts[2]),
		ThreadID:  unescape(parts[3]),
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
// not lose across a restart. The Klaus instance that owns the conversation
// (Instance), or — on the kagent path — the agent the thread is bound to and
// its AgentInstance plus the task in flight on it, and the channel's own facts
// about the thread, its initiator and the users it granted.
//
// Several writers read-modify-write this row (a channel's grant, the facade's
// task record, the binding), so they write through Store.Update rather than
// Put, and each keeps the fields it does not own.
type Entry struct {
	// Instance is the name of the Klaus instance that owns the conversation.
	// Empty for a kagent conversation.
	Instance string `json:"instance,omitempty"`
	// AgentRef is the agent the thread is bound to, in the shape the
	// deployment spells it. Never "" standing for the default: a changed
	// default must not fork the conversation.
	AgentRef string `json:"agent_ref,omitempty"`
	// AgentInstanceID is the kagent AgentInstance (a controller-assigned UUID)
	// the conversation's A2A turns are routed to. Empty for a Klaus conversation.
	AgentInstanceID string `json:"agent_instance_id,omitempty"`
	// TaskID is the A2A task running on AgentInstanceID while a turn is in
	// flight, cleared when the turn ends. A gateway that restarts mid-turn finds
	// here the tasks it has to resubscribe to.
	TaskID string `json:"task_id,omitempty"`
	// Resume is the channel-private data needed to deliver TaskID's result
	// after a restart (the channel adapter owns its keys). Set with TaskID.
	Resume map[string]string `json:"resume,omitempty"`
	// Initiator is the user whose mention launched the thread, and Granted the
	// users that initiator allowed into it. Written by the channel adapter:
	// the facts it cannot recover after a restart.
	Initiator string   `json:"initiator,omitempty"`
	Granted   []string `json:"granted,omitempty"`

	CreatedAt time.Time     `json:"created_at"`
	LastSeen  time.Time     `json:"last_seen"`
	TTL       time.Duration `json:"ttl"`
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

// Store is the routing-table backend.
type Store interface {
	Get(ctx context.Context, k Key) (Entry, bool, error)
	Put(ctx context.Context, k Key, e Entry) error
	Delete(ctx context.Context, k Key) error
	List(ctx context.Context) ([]KeyEntry, error)
	// Update applies mutate to the entry at k — the zero Entry when none is
	// live — and writes the result when mutate returns true. Read-modify-write
	// by different writers on one key (a channel's grant, the facade's task
	// record, the binding) is serialised inside the process, so no writer loses
	// another's fields. A second replica is not protected: the stores have no CAS.
	Update(ctx context.Context, k Key, mutate func(e *Entry, found bool) bool) error
	Close() error
}
