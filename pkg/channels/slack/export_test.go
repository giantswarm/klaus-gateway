package slack

// Test hooks: the external test package builds adapters around a shared
// in-process recorder to simulate a restart with a surviving store.

type MemoryRecorder = memoryRecorder

func NewMemoryRecorder() *MemoryRecorder { return newMemoryRecorder() }

func (m *MemoryRecorder) SetBinding(channel, channelID, threadID, ref string) {
	m.setBinding(channel, channelID, threadID, ref)
}
