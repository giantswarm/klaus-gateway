package slack

import (
	"context"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// threadRecorder is the thread-row capability of the gateway. The Facade
// implements it over the routing store, which serialises the read-modify-write
// of a row several writers share; a gateway without it (tests, the
// Klaus-instance path) gets the in-process memoryRecorder, which behaves like
// the store on --store=memory: state lives for the life of the process.
type threadRecorder interface {
	ThreadRecord(ctx context.Context, channel, channelID, threadID string) (store.Entry, bool, error)
	UpdateThreadRecord(ctx context.Context, channel, channelID, threadID string, mutate func(e *store.Entry, found bool) bool) error
}

// records returns the gateway's thread recorder, or the in-process fallback.
func (a *Adapter) records() threadRecorder {
	if r, ok := a.gw.(threadRecorder); ok {
		return r
	}
	a.recordsMu.Lock()
	defer a.recordsMu.Unlock()
	if a.memRecords == nil {
		a.memRecords = newMemoryRecorder()
	}
	return a.memRecords
}

// memoryRecorder is threadRecorder over a map. Tests share one between two
// adapters to simulate a restart with a surviving store. Like --store=memory
// it expires a row after ttl of silence; ttl 0 never expires.
type memoryRecorder struct {
	mu   sync.Mutex
	recs map[string]store.Entry
	ttl  time.Duration
	now  func() time.Time
}

func newMemoryRecorder() *memoryRecorder {
	return &memoryRecorder{recs: map[string]store.Entry{}, ttl: channels.DefaultThreadTTL, now: time.Now}
}

func recKey(channel, channelID, threadID string) string {
	return channel + "|" + channelID + "|" + threadID
}

func (m *memoryRecorder) ThreadRecord(_ context.Context, channel, channelID, threadID string) (store.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.lookup(channel, channelID, threadID)
	return e, ok, nil
}

func (m *memoryRecorder) UpdateThreadRecord(_ context.Context, channel, channelID, threadID string, mutate func(e *store.Entry, found bool) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, found := m.lookup(channel, channelID, threadID)
	if !mutate(&e, found) {
		return nil
	}
	now := m.now()
	e.LastSeen = now
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	m.recs[recKey(channel, channelID, threadID)] = e
	return nil
}

// lookup returns the live row, dropping one that has aged past the ttl. The
// caller holds the lock.
func (m *memoryRecorder) lookup(channel, channelID, threadID string) (store.Entry, bool) {
	k := recKey(channel, channelID, threadID)
	e, ok := m.recs[k]
	if ok && m.ttl > 0 && m.now().Sub(e.LastSeen) > m.ttl {
		delete(m.recs, k)
		return store.Entry{}, false
	}
	if !ok {
		return store.Entry{}, false
	}
	return e, true
}
