package slack

import "github.com/giantswarm/klaus-gateway/pkg/channels"

// Test hooks: the external test package builds adapters around a shared
// in-process recorder to simulate a restart with a surviving store.

type MemoryRecorder = channels.Facade

func NewMemoryRecorder() *MemoryRecorder { return newMemoryRecorder() }
