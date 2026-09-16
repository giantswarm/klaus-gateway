package channels

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// ThreadRecordTTL is the sliding lifetime of a thread record: refreshed on
// every handled message, an idle thread's record expires after it and the
// next message starts a conversation. The instance binding keeps no TTL.
const ThreadRecordTTL = 30 * 24 * time.Hour

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
// sliding ThreadRecordTTL. It merges into the entry at the thread key rather
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
		e.TTL = ThreadRecordTTL
	}
	if err := f.Routes.Put(ctx, key, e); err != nil {
		return fmt.Errorf("channels: write thread record: %w", err)
	}
	return nil
}

// FindThreadBinding reports the agent of an instance binding the thread
// already has, for a thread whose record is gone (expired after
// ThreadRecordTTL of silence) while its binding, which never expires, is not.
// The newest binding wins when several exist. A full List is acceptable: this
// runs only for a thread with no record.
func (f *Facade) FindThreadBinding(ctx context.Context, channel, channelID, threadID string) (string, bool, error) {
	if f == nil || f.Routes == nil {
		return "", false, errNoThreadStore
	}
	entries, err := f.Routes.List(ctx)
	if err != nil {
		return "", false, fmt.Errorf("channels: list bindings: %w", err)
	}
	var ref string
	var seen time.Time
	for _, ke := range entries {
		k := ke.Key
		if k.Channel != channel || k.ChannelID != channelID || k.ThreadID != threadID || k.Agent == "" || ke.Entry.AgentInstanceID == "" {
			continue
		}
		if ref == "" || ke.Entry.LastSeen.After(seen) {
			ref, seen = k.Agent, ke.Entry.LastSeen
		}
	}
	return ref, ref != "", nil
}
