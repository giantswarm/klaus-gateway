package store_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	boltstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/bolt"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/storetest"
	valkeystore "github.com/giantswarm/klaus-gateway/pkg/routing/store/valkey"
)

const channelSlack = "slack"

// runConformance exercises the Store contract. Every backend must pass.
func runConformance(t *testing.T, factory func(t *testing.T) store.Store) {
	t.Helper()

	t.Run("put-get-expire", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "c1", ThreadID: "t1"}
		e := store.Entry{AgentInstanceID: "i1", CreatedAt: time.Now(), LastSeen: time.Now(), TTL: time.Hour}

		_, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok)

		require.NoError(t, storetest.Put(ctx, s, k, e))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, e.AgentInstanceID, got.AgentInstanceID)

		require.NoError(t, storetest.Expire(ctx, s, k))
		_, ok, err = s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("list", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		keys := []store.Key{
			{Channel: channelSlack, ChannelID: "c1", ThreadID: "t1"},
			{Channel: "slack", ChannelID: "c2", ThreadID: "t2"},
		}
		for _, k := range keys {
			require.NoError(t, storetest.Put(ctx, s, k, store.Entry{
				AgentInstanceID: "inst", CreatedAt: time.Now(), LastSeen: time.Now(), TTL: time.Hour,
			}))
		}
		entries, err := s.List(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 2)
	})

	t.Run("keys-with-pipes-round-trip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "c|pipe", ThreadID: `t\back`}
		require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "inst", LastSeen: time.Now(), TTL: time.Hour}))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "inst", got.AgentInstanceID)
	})

	t.Run("agent-instance-round-trip", func(t *testing.T) {
		// A thread is bound to an AgentInstance; the id must survive the
		// backend's serialisation.
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700000000.000100"}
		e := store.Entry{AgentInstanceID: "0192f1c2-7d1e-7a3b-9c4d-5e6f7a8b9c0d", CreatedAt: time.Now(), LastSeen: time.Now()}

		require.NoError(t, storetest.Put(ctx, s, k, e))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, e.AgentInstanceID, got.AgentInstanceID)

		entries, err := s.List(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, k, entries[0].Key)
		require.Equal(t, e.AgentInstanceID, entries[0].Entry.AgentInstanceID)
	})

	t.Run("one-row-per-thread", func(t *testing.T) {
		// A thread has one row: the agent it is bound to, the AgentInstance and
		// the task in flight on it, and the channel's initiator and grants.
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "C1", ThreadID: "1700000000.000100"}
		now := time.Now().UTC().Truncate(time.Second)
		in := store.Entry{
			AgentRef: "kagent/sre-agent", AgentInstanceID: "i-1", TaskID: "task-7",
			Resume: map[string]string{"slack_user": "U1"},
			Delivered: store.Delivered{
				TextLen: 42, StreamTS: "1700000000.000200", StreamLen: 30,
				ToolSteps: 3, OpenStepID: "step-3", OpenStepTitle: "Kubernetes list",
			},
			Initiator: "U1", Granted: []string{"U2", "U3"},
			CreatedAt: now, LastSeen: now, TTL: 30 * 24 * time.Hour,
		}
		require.NoError(t, storetest.Put(ctx, s, k, in))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, in.AgentRef, got.AgentRef)
		require.Equal(t, in.AgentInstanceID, got.AgentInstanceID)
		require.Equal(t, in.TaskID, got.TaskID)
		require.Equal(t, in.Resume, got.Resume)
		require.Equal(t, in.Delivered, got.Delivered)
		require.Equal(t, in.Initiator, got.Initiator)
		require.Equal(t, in.Granted, got.Granted)

		entries, err := s.List(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, k, entries[0].Key)
		require.Equal(t, in.AgentInstanceID, entries[0].Entry.AgentInstanceID)
		require.Equal(t, in.Initiator, entries[0].Entry.Initiator)
	})

	t.Run("update-creates-and-merges", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "C1", ThreadID: "T1"}

		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, found bool) bool {
			require.False(t, found)
			require.Equal(t, store.Entry{}, *e)
			e.AgentInstanceID, e.LastSeen, e.TTL = "i1", time.Now(), time.Hour
			return true
		}))
		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, found bool) bool {
			require.True(t, found)
			require.Equal(t, "i1", e.AgentInstanceID)
			e.Initiator, e.Granted = "U1", []string{"U2"}
			return true
		}))

		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "i1", got.AgentInstanceID, "the first writer's field survives the second")
		require.Equal(t, "U1", got.Initiator)
		require.Equal(t, []string{"U2"}, got.Granted)
	})

	t.Run("update-no-write-when-unchanged", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "C1", ThreadID: "T1"}

		require.NoError(t, s.Update(ctx, k, func(*store.Entry, bool) bool { return false }))
		_, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok, "a mutate that reports no change creates nothing")

		require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i1", LastSeen: time.Now(), TTL: time.Hour}))
		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, _ bool) bool {
			e.AgentInstanceID = "other"
			return false
		}))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "i1", got.AgentInstanceID, "the row is untouched")
	})

	t.Run("update-expired-is-absent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "C1", ThreadID: "T1"}
		require.NoError(t, storetest.Put(ctx, s, k, store.Entry{
			AgentInstanceID: "i1", Initiator: "U1",
			CreatedAt: time.Now().Add(-2 * time.Hour), LastSeen: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		}))
		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, found bool) bool {
			require.False(t, found, "an expired entry is absent")
			require.Equal(t, store.Entry{}, *e)
			return false
		}))
	})

	t.Run("update-serialises-writers", func(t *testing.T) {
		// The grant of one user must not erase another's: every writer reads
		// what the previous one wrote.
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "C1", ThreadID: "T1"}
		const writers = 32
		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = s.Update(ctx, k, func(e *store.Entry, _ bool) bool {
					e.Granted = append(e.Granted, fmt.Sprintf("U%02d", i))
					e.LastSeen, e.TTL = time.Now(), time.Hour
					return true
				})
			}()
		}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Len(t, got.Granted, writers, "no writer lost another's grant")
	})

	t.Run("review-put-get", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		_, ok, err := s.GetReview(ctx, "r1")
		require.NoError(t, err)
		require.False(t, ok)

		in := sampleReview("r1", time.Now())
		require.NoError(t, s.PutReview(ctx, in))
		got, ok, err := s.GetReview(ctx, "r1")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, in.ID, got.ID)
		require.Equal(t, in.Channel, got.Channel)
		require.Equal(t, in.TS, got.TS)
		require.Equal(t, in.Team, got.Team)
		require.Equal(t, in.Text, got.Text)
		require.Equal(t, in.Link, got.Link)
		require.Equal(t, in.Tool, got.Tool)
		require.Equal(t, in.Arguments, got.Arguments, "the arguments come back as JSON would hand them over")
		require.Equal(t, in.Status, got.Status)
		require.Empty(t, got.DecidedBy)
		require.False(t, got.Done)
		require.True(t, in.PostedAt.Equal(got.PostedAt))
		require.Equal(t, in.TTL, got.TTL)
	})

	t.Run("review-expired-is-absent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		stale := sampleReview("r1", time.Now().Add(-8*24*time.Hour))
		require.NoError(t, s.PutReview(ctx, stale))
		_, ok, err := s.GetReview(ctx, "r1")
		require.NoError(t, err)
		require.False(t, ok, "a review past its TTL is gone")
		found, err := s.UpdateReview(ctx, "r1", func(*store.Review) bool {
			t.Fatal("mutate must not run for an expired review")
			return false
		})
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("review-update-writes-and-reports", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		found, err := s.UpdateReview(ctx, "nope", func(*store.Review) bool { return true })
		require.NoError(t, err)
		require.False(t, found, "an unknown review is reported, not created")

		require.NoError(t, s.PutReview(ctx, sampleReview("r1", time.Now())))
		found, err = s.UpdateReview(ctx, "r1", func(r *store.Review) bool {
			r.DecidedBy, r.Status = "U1", "🔗 <@U1> is connecting"
			return true
		})
		require.NoError(t, err)
		require.True(t, found)
		got, ok, err := s.GetReview(ctx, "r1")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "U1", got.DecidedBy)
		require.Equal(t, "🔗 <@U1> is connecting", got.Status)
		require.Equal(t, "team-bumblebee", got.Team, "the ask survives the update")

		found, err = s.UpdateReview(ctx, "r1", func(r *store.Review) bool {
			r.DecidedBy = "U2"
			return false
		})
		require.NoError(t, err)
		require.True(t, found)
		got, _, err = s.GetReview(ctx, "r1")
		require.NoError(t, err)
		require.Equal(t, "U1", got.DecidedBy, "a mutate that reports no change writes nothing")
	})

	t.Run("review-claimed-once-under-contention", func(t *testing.T) {
		// Every clicker tries to take the review at once; the record's
		// decided_by is the arbiter and exactly one claim lands.
		s := factory(t)
		ctx := context.Background()
		require.NoError(t, s.PutReview(ctx, sampleReview("r1", time.Now())))
		const clickers = 32
		var wg sync.WaitGroup
		wins := make([]bool, clickers)
		errs := make([]error, clickers)
		for i := range clickers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				user := fmt.Sprintf("U%02d", i)
				_, errs[i] = s.UpdateReview(ctx, "r1", func(r *store.Review) bool {
					wins[i] = false
					if r.DecidedBy != "" {
						return false
					}
					r.DecidedBy, wins[i] = user, true
					return true
				})
			}()
		}
		wg.Wait()
		var winners []int
		for i := range clickers {
			require.NoError(t, errs[i])
			if wins[i] {
				winners = append(winners, i)
			}
		}
		require.Len(t, winners, 1, "one claim lands, every other clicker is told who holds it")
		got, ok, err := s.GetReview(ctx, "r1")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf("U%02d", winners[0]), got.DecidedBy)
	})

	t.Run("ttl-expired-filtered", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelSlack, ChannelID: "c1", ThreadID: "t1"}
		e := store.Entry{
			AgentInstanceID: "i1",
			CreatedAt:       time.Now().Add(-2 * time.Hour),
			LastSeen:        time.Now().Add(-2 * time.Hour),
			TTL:             time.Hour,
		}
		require.NoError(t, storetest.Put(ctx, s, k, e))
		_, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok, "expired entry should not be returned")

		entries, err := s.List(ctx)
		require.NoError(t, err)
		for _, kv := range entries {
			require.NotEqual(t, "i1", kv.Entry.AgentInstanceID)
		}
	})
}

// sampleReview is a posted team review as the Slack adapter records it. The
// arguments carry a float64, the type JSON decodes a number to, so the record
// compares equal whether or not a backend serialises it.
func sampleReview(id string, postedAt time.Time) store.Review {
	return store.Review{
		ID: id, Channel: "C1", TS: "1700000000.000100",
		Team: "team-bumblebee", Text: "*Archive* `giantswarm/old-thing`.", Link: "https://github.com/giantswarm/github/pull/4711",
		Tool: "x_giantswarm-repo-manager_approve_change", Arguments: map[string]any{"pr": float64(4711)},
		Status:   "",
		PostedAt: postedAt.UTC().Truncate(time.Millisecond), TTL: 7 * 24 * time.Hour,
	}
}

func TestMemoryStore_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) store.Store {
		s := memory.New()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestBoltStore_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) store.Store {
		path := t.TempDir() + "/routes.bolt"
		s, err := boltstore.Open(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// The Valkey store is exercised against a server speaking the real protocol
// over TCP (miniredis), not a fake client: a fake validates nothing about the
// wire format.
func TestValkeyStore_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) store.Store {
		m := miniredis.RunT(t)
		s, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), Timeout: time.Second})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestValkeyStore_ConformanceReal runs the same contract against a real Valkey
// named by KLAUS_GATEWAY_TEST_VALKEY_URL (host:port; password in
// KLAUS_GATEWAY_TEST_VALKEY_PASSWORD), for the cases where miniredis and the
// server diverge. Every run uses its own key prefix and cleans up after itself.
func TestValkeyStore_ConformanceReal(t *testing.T) {
	url := os.Getenv("KLAUS_GATEWAY_TEST_VALKEY_URL")
	if url == "" {
		t.Skip("KLAUS_GATEWAY_TEST_VALKEY_URL not set")
	}
	runConformance(t, func(t *testing.T) store.Store {
		s, err := valkeystore.New(valkeystore.Options{
			URL:       url,
			Password:  os.Getenv("KLAUS_GATEWAY_TEST_VALKEY_PASSWORD"),
			KeyPrefix: "klaus-gateway-test:" + t.Name() + ":" + time.Now().Format("150405.000") + ":",
			Timeout:   2 * time.Second,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx := context.Background()
			entries, err := s.List(ctx)
			require.NoError(t, err)
			for _, ke := range entries {
				require.NoError(t, storetest.Expire(ctx, s, ke.Key))
			}
			_ = s.Close()
		})
		return s
	})
}

func TestKey_StringRoundTrip(t *testing.T) {
	cases := []store.Key{
		{Channel: channelSlack, ChannelID: "abc", ThreadID: "t1"},
		{Channel: "slack", ChannelID: "C|123", ThreadID: ""},
	}
	for _, k := range cases {
		parsed, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, parsed)
	}
}
