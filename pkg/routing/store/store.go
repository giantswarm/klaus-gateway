// Package store defines the interface for the klaus-gateway routing table.
//
// A routing entry maps (channel, channel-id, user, thread) to the klaus
// instance that owns the conversation. Stores persist this mapping across
// restarts where possible (bolt, configmap) or keep it in memory.
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

// Key identifies a conversation across channels.
type Key struct {
	Channel   string
	ChannelID string
	UserID    string
	ThreadID  string
	// Agent is the agentRef discriminator used by the A2A multi-agent path.
	// Empty for all other channels; omitted from the serialised form when empty
	// so existing 4-part keys remain byte-identical.
	Agent string
}

// String returns the canonical serialised form used as a storage key. The
// format is stable: stores rely on it for on-disk keys.
//
// When Agent is non-empty a fifth pipe-separated segment is appended;
// otherwise the output is the existing 4-part form so pre-existing store
// entries are not invalidated.
func (k Key) String() string {
	parts := []string{
		escape(k.Channel),
		escape(k.ChannelID),
		escape(k.UserID),
		escape(k.ThreadID),
	}
	if k.Agent != "" {
		parts = append(parts, escape(k.Agent))
	}
	return strings.Join(parts, "|")
}

// ParseKey inverts Key.String. It accepts both the legacy 4-part form and the
// extended 5-part form (with Agent).
func ParseKey(s string) (Key, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 4 && len(parts) != 5 {
		return Key{}, fmt.Errorf("invalid key %q: expected 4 or 5 parts", s)
	}
	k := Key{
		Channel:   unescape(parts[0]),
		ChannelID: unescape(parts[1]),
		UserID:    unescape(parts[2]),
		ThreadID:  unescape(parts[3]),
	}
	if len(parts) == 5 {
		k.Agent = unescape(parts[4])
	}
	return k, nil
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "|", `\p`)
}

func unescape(s string) string {
	s = strings.ReplaceAll(s, `\p`, "|")
	return strings.ReplaceAll(s, `\\`, `\`)
}

// Entry records an instance assignment.
type Entry struct {
	Instance  string        `json:"instance"`
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
	Close() error
}
