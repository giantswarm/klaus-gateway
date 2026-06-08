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

// ttlPushStore wraps InMemoryPushConfigStore and evicts per-task entries
// after pushConfigTTL has elapsed since the last Save for that task.
type ttlPushStore struct {
	inner  *push.InMemoryPushConfigStore
	mu     sync.Mutex
	timers map[a2a.TaskID]*time.Timer
}

func newTTLPushStore() *ttlPushStore {
	return &ttlPushStore{
		inner:  push.NewInMemoryStore(),
		timers: make(map[a2a.TaskID]*time.Timer),
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
	if t, ok := s.timers[taskID]; ok {
		t.Stop()
	}
	s.timers[taskID] = time.AfterFunc(pushConfigTTL, func() {
		_ = s.inner.DeleteAll(context.Background(), taskID)
		s.mu.Lock()
		delete(s.timers, taskID)
		s.mu.Unlock()
	})
}

func (s *ttlPushStore) cancelTimer(taskID a2a.TaskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.timers[taskID]; ok {
		t.Stop()
		delete(s.timers, taskID)
	}
}
