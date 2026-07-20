package slack

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// A failed reactions.remove must keep the working flag set so a later terminal
// hook retries the removal instead of stranding the working reaction.
func TestRemoveWorking_RetriesAfterTransientFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"ok":false,"error":"fatal_error"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	p := &progressState{
		client:  &slackAPIClient{botToken: "t", baseURL: srv.URL},
		channel: "C1",
		reactTS: "1.1",
		working: true,
		emojis:  progressEmojis{working: "eyes", done: "white_check_mark", failed: "x"},
		logger:  slog.Default(),
	}

	p.removeWorking(t.Context())
	require.True(t, p.working, "a failed removal must leave the reaction marked present")

	p.removeWorking(t.Context())
	require.False(t, p.working)
	require.Equal(t, int32(2), calls.Load())
}
