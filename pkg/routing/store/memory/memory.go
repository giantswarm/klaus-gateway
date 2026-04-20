// Package memory provides an in-process implementation of routing.Store.
// It is the default in tests and in single-instance deployments where the
// routing table can be rebuilt from channel adapters on restart.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// EvictionInterval is how often the background goroutine scans for expired
// entries. It is a var so tests can shorten it.
var EvictionInterval = time.Minute

// Store is a concurrency-safe in-memory routing store with TTL eviction.
type Store struct {
	mu   sync.RWMutex
	data map[string]store.Entry
	now  func() time.Time

	evictOnce sync.Once
	stopEvict chan struct{}
	evictDone chan struct{}
}

// New returns an empty Store.
func New() *Store {
	return &Store{
		data:      make(map[string]store.Entry),
		now:       time.Now,
		stopEvict: make(chan struct{}),
		evictDone: make(chan struct{}),
	}
}

// Get looks up an entry. Returns (_, false, nil) when absent or expired.
func (s *Store) Get(_ context.Context, k store.Key) (store.Entry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[k.String()]
	if !ok {
		return store.Entry{}, false, nil
	}
	if e.Expired(s.now()) {
		return store.Entry{}, false, nil
	}
	return e, true, nil
}

// Put upserts an entry and starts the eviction goroutine if not already running.
func (s *Store) Put(_ context.Context, k store.Key, e store.Entry) error {
	s.mu.Lock()
	s.data[k.String()] = e
	s.mu.Unlock()
	s.evictOnce.Do(s.startEvict)
	return nil
}

// Delete removes an entry. Missing keys are not an error.
func (s *Store) Delete(_ context.Context, k store.Key) error {
	s.mu.Lock()
	delete(s.data, k.String())
	s.mu.Unlock()
	return nil
}

// List returns every live entry.
func (s *Store) List(_ context.Context) ([]store.KeyEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.KeyEntry, 0, len(s.data))
	now := s.now()
	for ks, e := range s.data {
		if e.Expired(now) {
			continue
		}
		k, err := store.ParseKey(ks)
		if err != nil {
			return nil, err
		}
		out = append(out, store.KeyEntry{Key: k, Entry: e})
	}
	return out, nil
}

// Close stops the eviction goroutine and releases resources.
func (s *Store) Close() error {
	select {
	case <-s.stopEvict:
		return nil
	default:
	}
	close(s.stopEvict)
	// Only wait for the goroutine if it was ever started.
	started := true
	s.evictOnce.Do(func() { started = false })
	if started {
		<-s.evictDone
	}
	return nil
}

func (s *Store) startEvict() {
	go func() {
		defer close(s.evictDone)
		t := time.NewTicker(EvictionInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stopEvict:
				return
			case <-t.C:
				s.evict()
			}
		}
	}()
}

func (s *Store) evict() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.data {
		if e.Expired(now) {
			delete(s.data, k)
		}
	}
}

// SetNowFunc is a test hook to override the clock.
func (s *Store) SetNowFunc(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = f
}

// EvictNow triggers a synchronous eviction pass, for tests.
func (s *Store) EvictNow() { s.evict() }
