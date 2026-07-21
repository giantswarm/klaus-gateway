package slack

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rootAuthorServer answers conversations.replies with a single human root author.
func rootAuthorServer(t *testing.T, author string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"messages":[{"user":%q,"ts":"100.000"}]}`, author)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSeedInitiatorFromRoot_RestartWindowRestoresRootAuthor(t *testing.T) {
	srv := rootAuthorServer(t, "U_ROOT")
	a := &Adapter{
		Logger:    slog.Default(),
		APIBase:   srv.URL,
		Secrets:   Secrets{BotToken: "tok"},
		startUnix: time.Now().Unix(), // fresh process: within the restart window
	}

	a.seedInitiatorFromRoot(t.Context(), "C1", "100.000", "200.000")

	require.Equal(t, "U_ROOT", a.accessPolicy().Initiator("100.000"),
		"within threadAccessTTL of start, an unrecorded thread is a restart and the root author is restored")
}

func TestSeedInitiatorFromRoot_PastTTLLeavesMentionerToWin(t *testing.T) {
	srv := rootAuthorServer(t, "U_ROOT")
	a := &Adapter{
		Logger:  slog.Default(),
		APIBase: srv.URL,
		Secrets: Secrets{BotToken: "tok"},
		// Process has run longer than the TTL: an unrecorded thread was swept by
		// the TTL, not lost to a restart, so the reseed must not fire.
		startUnix: time.Now().Unix() - int64(threadAccessTTL.Seconds()) - 60,
	}

	a.seedInitiatorFromRoot(t.Context(), "C1", "100.000", "200.000")

	require.Empty(t, a.accessPolicy().Initiator("100.000"),
		"past threadAccessTTL the reseed is suppressed so the fresh mention re-establishes the initiator")

	// Dispatch's SetInitiator then installs the mentioner, not the stale root.
	require.Equal(t, "U_MENTIONER", a.accessPolicy().SetInitiator("100.000", "U_MENTIONER"))
}

func TestSeedInitiatorFromRoot_UnstartedAdapterSkipsReseed(t *testing.T) {
	srv := rootAuthorServer(t, "U_ROOT")
	a := &Adapter{
		Logger:  slog.Default(),
		APIBase: srv.URL,
		Secrets: Secrets{BotToken: "tok"},
		// startUnix zero: uptime unknown, so behave as steady state (no reseed).
	}

	a.seedInitiatorFromRoot(t.Context(), "C1", "100.000", "200.000")

	require.Empty(t, a.accessPolicy().Initiator("100.000"))
}
