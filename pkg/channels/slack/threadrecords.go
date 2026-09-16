package slack

import (
	"context"
	"hash/fnv"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// threadRecorder is the thread-record capability of the gateway. The Facade
// implements it over the routing store; a gateway without it (tests, the
// Klaus-instance path) gets the in-process memoryRecorder, which behaves like
// the store on --store=memory: state lives for the life of the process.
type threadRecorder interface {
	ThreadRecord(ctx context.Context, channel, channelID, threadID string) (channels.ThreadRecord, bool, error)
	SaveThreadRecord(ctx context.Context, channel, channelID, threadID string, t store.Thread) error
	FindThreadBinding(ctx context.Context, channel, channelID, threadID string) (string, bool, error)
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

// recordLock serialises read-modify-write on one thread's record inside the
// process (64 stripes keyed by channel and thread).
func (a *Adapter) recordLock(channelID, threadID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(channelID + "\x00" + threadID))
	return &a.recordLocks[h.Sum32()%uint32(len(a.recordLocks))]
}

// memoryRecorder is threadRecorder over a map. Tests share one between two
// adapters to simulate a restart with a surviving store.
type memoryRecorder struct {
	mu   sync.Mutex
	recs map[string]channels.ThreadRecord
	bind map[string]string // channel|channelID|threadID -> agent ref, for FindThreadBinding
	now  func() time.Time
}

func newMemoryRecorder() *memoryRecorder {
	return &memoryRecorder{recs: map[string]channels.ThreadRecord{}, bind: map[string]string{}, now: time.Now}
}

func recKey(channel, channelID, threadID string) string {
	return channel + "|" + channelID + "|" + threadID
}

func (m *memoryRecorder) ThreadRecord(_ context.Context, channel, channelID, threadID string) (channels.ThreadRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := recKey(channel, channelID, threadID)
	r, ok := m.recs[k]
	if ok && m.now().Sub(r.LastSeen) > channels.ThreadRecordTTL {
		delete(m.recs, k)
		return channels.ThreadRecord{}, false, nil
	}
	return r, ok, nil
}

func (m *memoryRecorder) SaveThreadRecord(_ context.Context, channel, channelID, threadID string, t store.Thread) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := recKey(channel, channelID, threadID)
	now := m.now()
	created := now
	if old, ok := m.recs[k]; ok {
		created = old.CreatedAt
	}
	m.recs[k] = channels.ThreadRecord{Thread: t, CreatedAt: created, LastSeen: now}
	return nil
}

func (m *memoryRecorder) FindThreadBinding(_ context.Context, channel, channelID, threadID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.bind[recKey(channel, channelID, threadID)]
	return ref, ok, nil
}

// setBinding records an instance binding of the thread for FindThreadBinding
// (test-only shape of what the store holds at the 5-part key).
func (m *memoryRecorder) setBinding(channel, channelID, threadID, ref string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bind[recKey(channel, channelID, threadID)] = ref
}
