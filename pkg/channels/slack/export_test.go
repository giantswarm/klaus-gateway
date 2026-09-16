package slack

import "time"

// Test hooks: the external test package builds adapters around a shared
// in-process recorder to simulate a restart with a surviving store.

type MemoryRecorder = memoryRecorder

func NewMemoryRecorder() *MemoryRecorder { return newMemoryRecorder() }

// SetTTL shortens the recorder's thread lifetime so a test can let a thread
// be forgotten without waiting for the default.
func (m *MemoryRecorder) SetTTL(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ttl = d
}
