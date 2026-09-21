package slack_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// flowWait is the budget of every wait for a dispatch or a post; it is defined
// once, next to the adapter (flowwait_internal_test.go), and re-exported for
// this package by export_test.go.
const flowWait = slackadapter.FlowWait

// waitThreadIdle blocks until the adapter's turn slot for threadID is free.
// The adapter runs one turn per thread and refuses a turn that arrives while
// another holds the slot with the busy notice, dropping it for good, so a test
// that sends a follow-up into a thread it has already driven must wait for the
// previous turn to finish rather than for it to have merely started.
func waitThreadIdle(t *testing.T, a *slackadapter.Adapter, threadID string) {
	t.Helper()
	require.Eventually(t, func() bool { return a.ThreadIdle(threadID) }, flowWait, 10*time.Millisecond,
		"the previous turn in thread %s released the thread slot", threadID)
}

// waitTurnStreaming blocks until the first turn of the thread is draining its
// stream. The fake records the working reaction when the request arrives, while
// the adapter notes the reaction only once the response is back, so a stop or a
// shutdown sent on the reaction alone can find nothing to clear. Marking the
// session "processing" is the writer's first act after that bookkeeping, and it
// is the turn's first status call — the exit status usually rides the stop that
// closes the streamed answer instead of a call of its own.
func waitTurnStreaming(t *testing.T, fake *fakeSlackAPI) {
	t.Helper()
	fake.waitForPath(t, "agents.sessions.setStatus", 1)
}
