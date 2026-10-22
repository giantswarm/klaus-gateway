package channels

import "time"

// SetPickUpBackoff shortens the wait between the attempts to pick a task up
// again after its stream broke (first wait, cap) for t, so a test drives a
// failed attempt in milliseconds. The tests of this package do not run in
// parallel.
func SetPickUpBackoff(t interface{ Cleanup(func()) }, delay, maxDelay time.Duration) {
	oldDelay, oldMax := streamRecoverDelay, streamRecoverMax
	streamRecoverDelay, streamRecoverMax = delay, maxDelay
	t.Cleanup(func() { streamRecoverDelay, streamRecoverMax = oldDelay, oldMax })
}
