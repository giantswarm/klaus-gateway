// Package bolt provides a bbolt-backed implementation of routing.Store.
// Entries survive process restarts; one bucket ("routes") holds the
// canonicalised key -> JSON-encoded entry mapping.
package bolt

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// EvictionInterval is how often the background goroutine scans for expired
// entries. It is a var so tests can shorten it.
var EvictionInterval = time.Minute

var bucketName = []byte("routes")

// Store is a bbolt-backed routing store.
type Store struct {
	db  *bolt.DB
	now func() time.Time

	stopOnce  sync.Once
	stopEvict chan struct{}
	evictDone chan struct{}
}

// Open opens or creates a bbolt database at path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{
		db:        db,
		now:       time.Now,
		stopEvict: make(chan struct{}),
		evictDone: make(chan struct{}),
	}
	go s.evictLoop()
	return s, nil
}

// Get returns the entry for k, or (_, false, nil) if absent or expired.
func (s *Store) Get(_ context.Context, k store.Key) (store.Entry, bool, error) {
	var e store.Entry
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(k.String()))
		if v == nil {
			return nil
		}
		if err := json.Unmarshal(v, &e); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return store.Entry{}, false, err
	}
	if !found {
		return store.Entry{}, false, nil
	}
	if e.Expired(s.now()) {
		return store.Entry{}, false, nil
	}
	return e, true, nil
}

// Put upserts an entry.
func (s *Store) Put(_ context.Context, k store.Key, e store.Entry) error {
	buf, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(k.String()), buf)
	})
}

// Delete removes an entry; missing keys are not an error.
func (s *Store) Delete(_ context.Context, k store.Key) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Delete([]byte(k.String()))
	})
}

// List returns every live entry. Expired entries are filtered out but not deleted;
// the background eviction pass handles deletion.
func (s *Store) List(_ context.Context) ([]store.KeyEntry, error) {
	var out []store.KeyEntry
	now := s.now()
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(k, v []byte) error {
			var e store.Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if e.Expired(now) {
				return nil
			}
			key, err := store.ParseKey(string(k))
			if err != nil {
				return err
			}
			out = append(out, store.KeyEntry{Key: key, Entry: e})
			return nil
		})
	})
	return out, err
}

// Close stops the eviction goroutine and closes the database.
func (s *Store) Close() error {
	s.stopOnce.Do(func() {
		close(s.stopEvict)
		<-s.evictDone
	})
	return s.db.Close()
}

func (s *Store) evictLoop() {
	defer close(s.evictDone)
	t := time.NewTicker(EvictionInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopEvict:
			return
		case <-t.C:
			_ = s.Evict()
		}
	}
}

// Evict scans the bucket and deletes expired entries. Exposed for tests.
func (s *Store) Evict() error {
	now := s.now()
	var toDelete [][]byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(k, v []byte) error {
			var e store.Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if e.Expired(now) {
				kc := make([]byte, len(k))
				copy(kc, k)
				toDelete = append(toDelete, kc)
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if len(toDelete) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		for _, k := range toDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetNowFunc is a test hook to override the clock.
func (s *Store) SetNowFunc(f func() time.Time) { s.now = f }
