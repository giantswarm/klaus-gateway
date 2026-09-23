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
// turn refreshes it; after it the conversation has ended and the next mention
// starts it over.
const DefaultThreadTTL = 90 * 24 * time.Hour

// threadKey is the routing-store key of a thread's row: a thread is shared by
// its participants, so every one of them reaches the same row.
func threadKey(channel, channelID, threadID string) store.Key {
	return store.Key{Channel: channel, ChannelID: channelID, ThreadID: threadID}
}

var errNoThreadStore = errors.New("channels: no routing store for thread records")

// storeTTL is the lifetime a thread's row is written with: twice ThreadTTL.
// The conversation itself ends after ThreadTTL of silence (see threadClosed),
// but the row outlives it by as much again so a reply in a thread that ended
// can be told so instead of being ignored. 0 (never expire) stays 0.
func (f *Facade) storeTTL() time.Duration {
	if f.ThreadTTL <= 0 {
		return 0
	}
	return 2 * f.ThreadTTL
}

// threadClosed reports whether the conversation on e has ended: its last
// message is older than ThreadTTL. A closed row is still in the store — it
// expires at storeTTL — but counts as absent for routing, the agent binding
// and the grants. A zero ThreadTTL never closes a thread, and neither does a
// row of unknown age: a row with no LastSeen is one nothing stamped, and the
// store serving it is the only thing that says it is still current.
func (f *Facade) threadClosed(e store.Entry, now time.Time) bool {
	if f.ThreadTTL <= 0 || e.LastSeen.IsZero() {
		return false
	}
	return now.Sub(e.LastSeen) > f.ThreadTTL
}

// liveEntry applies the closed predicate to a row read straight from the
// store: every reader of a thread's state sees a closed row as no row.
func (f *Facade) liveEntry(e store.Entry, ok bool) (store.Entry, bool) {
	if !ok || f.threadClosed(e, f.clock()) {
		return store.Entry{}, false
	}
	return e, true
}

// reopen empties a closed row in place and reports it to the writer as not
// found, so a write starts the thread over — new initiator, no grants, no
// binding — exactly as on a thread the store has forgotten.
func (f *Facade) reopen(e *store.Entry, found bool, now time.Time) bool {
	if !found || !f.threadClosed(*e, now) {
		return found
	}
	*e = store.Entry{}
	return false
}

// ThreadState is what one read of a thread's row tells its readers: the live
// record, or — when the conversation on it has ended while the row is still in
// the store — that it is Closed, and the Lifetime it ended after. Found and
// Closed are never both true, and both are false for a thread the store has no
// row for at all.
type ThreadState struct {
	Entry    store.Entry
	Found    bool
	Closed   bool
	Lifetime time.Duration
}

// ThreadState reads the thread's row once and reports both of the things a
// reader wants from it: what a live row holds, and whether the conversation on
// it has ended — silent for longer than the lifetime, but not yet dropped at
// twice it. A closed row is good for one thing only: a channel adapter tells
// the author of a reply that the conversation ended rather than ignoring them.
// It comes from the same read as the record, so the most frequent path in a
// served channel — a reply in a thread the bot has no session in — costs one
// store call, not two.
func (f *Facade) ThreadState(ctx context.Context, channel, channelID, threadID string) (ThreadState, error) {
	if f == nil || f.Routes == nil {
		return ThreadState{}, errNoThreadStore
	}
	e, ok, err := f.Routes.Get(ctx, threadKey(channel, channelID, threadID))
	if err != nil {
		return ThreadState{}, fmt.Errorf("channels: read thread record: %w", err)
	}
	if !ok {
		return ThreadState{}, nil
	}
	if f.threadClosed(e, f.clock()) {
		return ThreadState{Closed: true, Lifetime: f.ThreadTTL}, nil
	}
	return ThreadState{Entry: e, Found: true}, nil
}

// ThreadRecord reads the thread's row; ok is false when the thread has none or
// its conversation has ended (ThreadState tells the two apart).
func (f *Facade) ThreadRecord(ctx context.Context, channel, channelID, threadID string) (store.Entry, bool, error) {
	st, err := f.ThreadState(ctx, channel, channelID, threadID)
	if err != nil {
		return store.Entry{}, false, err
	}
	return st.Entry, st.Found, nil
}

// UpdateThreadRecord applies mutate to the thread's row through the store's
// per-key serialisation and, when mutate reports a change, stamps LastSeen =
// now, CreatedAt when unset and the facade's storeTTL: every handled message
// slides the thread's lifetime and brings the row to the configured one. A
// row whose conversation has ended is handed to mutate empty and as not
// found, so the write starts the thread over instead of merging into it.
func (f *Facade) UpdateThreadRecord(ctx context.Context, channel, channelID, threadID string, mutate func(e *store.Entry, found bool) bool) error {
	if f == nil || f.Routes == nil {
		return errNoThreadStore
	}
	now := f.clock()
	err := f.Routes.Update(ctx, threadKey(channel, channelID, threadID), func(e *store.Entry, found bool) bool {
		found = f.reopen(e, found, now)
		if !mutate(e, found) {
			return false
		}
		e.LastSeen = now
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		// The row's expiry is re-stamped on every change, not only when it is
		// unset: a row written under an earlier lifetime — or under an
		// earlier --thread-ttl — adopts the configured one on its next
		// message. A thread nobody writes in again keeps the TTL it has and
		// expires under it.
		e.TTL = f.storeTTL()
		return true
	})
	if err != nil {
		return fmt.Errorf("channels: write thread record: %w", err)
	}
	return nil
}
