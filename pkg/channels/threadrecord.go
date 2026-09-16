package channels

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// DefaultThreadTTL is the default of --thread-ttl: the sliding lifetime of a
// thread's state, its record and its AgentInstance binding alike. Every
// handled message refreshes it; after it the thread is forgotten and the next
// mention starts a fresh conversation.
const DefaultThreadTTL = 90 * 24 * time.Hour

// ThreadRecord is a store.Thread with the timestamps of its entry.
type ThreadRecord struct {
	store.Thread
	CreatedAt time.Time
	LastSeen  time.Time
}

func threadKey(channel, channelID, threadID string) store.Key {
	return store.Key{Channel: channel, ChannelID: channelID, ThreadID: threadID}
}

var errNoThreadStore = errors.New("channels: no routing store for thread records")

// ThreadRecord reads the thread's record; ok is false when none exists.
func (f *Facade) ThreadRecord(ctx context.Context, channel, channelID, threadID string) (ThreadRecord, bool, error) {
	if f == nil || f.Routes == nil {
		return ThreadRecord{}, false, errNoThreadStore
	}
	e, ok, err := f.Routes.Get(ctx, threadKey(channel, channelID, threadID))
	if err != nil {
		return ThreadRecord{}, false, fmt.Errorf("channels: read thread record: %w", err)
	}
	if !ok || e.Thread == nil {
		return ThreadRecord{}, false, nil
	}
	return ThreadRecord{Thread: *e.Thread, CreatedAt: e.CreatedAt, LastSeen: e.LastSeen}, true, nil
}

// SaveThreadRecord writes t as the thread's record with LastSeen = now and the
// facade's sliding ThreadTTL. It merges into the entry at the thread key rather
// than replacing it: on the Klaus-instance path the router keeps the thread's
// instance route at this same key (Slack's user slot is empty), so the
// instance name, an existing CreatedAt and a TTL the router set are kept.
func (f *Facade) SaveThreadRecord(ctx context.Context, channel, channelID, threadID string, t store.Thread) error {
	if f == nil || f.Routes == nil {
		return errNoThreadStore
	}
	key := threadKey(channel, channelID, threadID)
	now := time.Now()
	e, ok, err := f.Routes.Get(ctx, key)
	if err != nil || !ok {
		e = store.Entry{}
	}
	e.Thread = &t
	e.LastSeen = now
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if e.TTL <= 0 {
		e.TTL = f.ThreadTTL
	}
	if err := f.Routes.Put(ctx, key, e); err != nil {
		return fmt.Errorf("channels: write thread record: %w", err)
	}
	return nil
}
