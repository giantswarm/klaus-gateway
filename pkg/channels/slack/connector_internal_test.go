package slack

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// markConnectorPrompted is a check-and-set: concurrent turns carrying the same
// single-use login URL must collapse to exactly one Connect prompt, while a
// challenge carrying a NEW URL supersedes the cooldown (the auth server
// invalidated the old link, so the posted button is dead).
func TestMarkConnectorPrompted_ConcurrencyAndURLSupersede(t *testing.T) {
	a := &Adapter{}

	const goroutines = 32
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.markConnectorPrompted("U1", "pro", "https://idp.example/authorize?state=abc") {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), allowed.Load(), "same-URL races must collapse to one prompt")

	require.True(t, a.markConnectorPrompted("U1", "pro", "https://idp.example/authorize?state=def"),
		"a new URL supersedes the cooldown")
	require.False(t, a.markConnectorPrompted("U1", "pro", "https://idp.example/authorize?state=def"),
		"the superseding URL then cools down itself")

	a.clearConnectorPrompted("U1", "pro")
	require.True(t, a.markConnectorPrompted("U1", "pro", "https://idp.example/authorize?state=def"),
		"a cleared record (failed post) allows an immediate retry")
}
