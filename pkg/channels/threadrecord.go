package channels

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// DefaultThreadTTL is the default of --thread-ttl: the sliding lifetime of a
// thread's row, its channel record and its AgentInstance binding alike. Every
// turn refreshes it; after it the thread is forgotten and the next mention
// starts it over.
const DefaultThreadTTL = 90 * 24 * time.Hour

// threadKey is the routing-store key of a thread's row. The user slot is empty
// on purpose: a thread is shared by its participants, so every one of them
// reaches the same row.
func threadKey(channel, channelID, threadID string) store.Key {
	return store.Key{Channel: channel, ChannelID: channelID, ThreadID: threadID}
}

var errNoThreadStore = errors.New("channels: no routing store for thread records")

// ThreadRecord reads the thread's row; ok is false when the thread has none.
func (f *Facade) ThreadRecord(ctx context.Context, channel, channelID, threadID string) (store.Entry, bool, error) {
	if f == nil || f.Routes == nil {
		return store.Entry{}, false, errNoThreadStore
	}
	e, ok, err := f.Routes.Get(ctx, threadKey(channel, channelID, threadID))
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("channels: read thread record: %w", err)
	}
	if !ok {
		return store.Entry{}, false, nil
	}
	return e, true, nil
}

// UpdateThreadRecord applies mutate to the thread's row through the store's
// per-key serialisation and, when mutate reports a change, stamps LastSeen =
// now, CreatedAt when unset and the facade's ThreadTTL when the row has no
// TTL: every handled message slides the thread's lifetime.
func (f *Facade) UpdateThreadRecord(ctx context.Context, channel, channelID, threadID string, mutate func(e *store.Entry, found bool) bool) error {
	if f == nil || f.Routes == nil {
		return errNoThreadStore
	}
	now := f.clock()
	err := f.Routes.Update(ctx, threadKey(channel, channelID, threadID), func(e *store.Entry, found bool) bool {
		if !mutate(e, found) {
			return false
		}
		e.LastSeen = now
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		if e.TTL <= 0 {
			e.TTL = f.ThreadTTL
		}
		return true
	})
	if err != nil {
		return fmt.Errorf("channels: write thread record: %w", err)
	}
	return nil
}
