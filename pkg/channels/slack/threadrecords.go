package slack

import (
	"context"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// threadRecorder is the thread-row capability of the gateway. The Facade
// implements it over the routing store, which serialises the read-modify-write
// of a row several writers share. A gateway without it (the test stub) gets an
// in-process Facade over the memory store, so one implementation of the row's
// stamping and expiry rules serves every path: state lives for the life of the
// process, as on --store=memory.
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

// newMemoryRecorder is the in-process fallback: the Facade's own row logic over
// a memory store, with the default thread lifetime.
func newMemoryRecorder() *channels.Facade {
	return &channels.Facade{Routes: memory.New(), ThreadTTL: channels.DefaultThreadTTL}
}
