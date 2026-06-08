package a2a

import (
	"context"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv/push"
)

// pushConfigTTL is how long a task's push subscriptions are kept after the
// last Save. The framework never calls DeleteAll on task completion, so
// without this the store grows monotonically.
const pushConfigTTL = 24 * time.Hour

type timerEntry struct {
	timer *time.Timer
	gen   uint64
}

// ttlPushStore wraps InMemoryPushConfigStore and evicts per-task entries
// after pushConfigTTL has elapsed since the last Save for that task.
type ttlPushStore struct {
	inner   *push.InMemoryPushConfigStore
	mu      sync.Mutex
	entries map[a2a.TaskID]timerEntry
	nextGen uint64
}

func newTTLPushStore() *ttlPushStore {
	return &ttlPushStore{
		inner:   push.NewInMemoryStore(),
		entries: make(map[a2a.TaskID]timerEntry),
	}
}

func (s *ttlPushStore) Save(ctx context.Context, taskID a2a.TaskID, config *a2a.PushConfig) (*a2a.PushConfig, error) {
	saved, err := s.inner.Save(ctx, taskID, config)
	if err != nil {
		return nil, err
	}
	s.resetTimer(taskID)
	return saved, nil
}

func (s *ttlPushStore) Get(ctx context.Context, taskID a2a.TaskID, configID string) (*a2a.PushConfig, error) {
	return s.inner.Get(ctx, taskID, configID)
}

func (s *ttlPushStore) List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error) {
	return s.inner.List(ctx, taskID)
}

func (s *ttlPushStore) Delete(ctx context.Context, taskID a2a.TaskID, configID string) error {
	return s.inner.Delete(ctx, taskID, configID)
}

func (s *ttlPushStore) DeleteAll(ctx context.Context, taskID a2a.TaskID) error {
	s.cancelTimer(taskID)
	return s.inner.DeleteAll(ctx, taskID)
}

func (s *ttlPushStore) resetTimer(taskID a2a.TaskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[taskID]; ok {
		e.timer.Stop()
	}
	s.nextGen++
	gen := s.nextGen
	t := time.AfterFunc(pushConfigTTL, func() {
		s.mu.Lock()
		e, ok := s.entries[taskID]
		if !ok || e.gen != gen {
			s.mu.Unlock()
			return
		}
		delete(s.entries, taskID)
		s.mu.Unlock()
		_ = s.inner.DeleteAll(context.Background(), taskID)
	})
	s.entries[taskID] = timerEntry{timer: t, gen: gen}
}

func (s *ttlPushStore) cancelTimer(taskID a2a.TaskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[taskID]; ok {
		e.timer.Stop()
		delete(s.entries, taskID)
	}
}
