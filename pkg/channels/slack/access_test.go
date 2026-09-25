package slack

import (
	"context"
	"testing"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// countingStore counts the updates that wrote a row, so a test can tell a
// refresh from a no-op.
type countingStore struct {
	store.Store
	writes int
}

func (c *countingStore) Update(ctx context.Context, k store.Key, mutate func(e *store.Entry, found bool) bool) error {
	return c.Store.Update(ctx, k, func(e *store.Entry, found bool) bool {
		changed := mutate(e, found)
		if changed {
			c.writes++
		}
		return changed
	})
}

// newRecordAccessForTest builds the policy over a memory-store facade with
// one controllable clock on the store and the facade: advancing *now ages the
// rows past ttl.
func newRecordAccessForTest(now *time.Time, ttl time.Duration) (*recordAccess, *channels.Facade, *countingStore) {
	clock := func() time.Time { return *now }
	ms := memory.New()
	ms.SetNowFunc(clock)
	cs := &countingStore{Store: ms}
	rec := &channels.Facade{Routes: cs, ThreadTTL: ttl}
	rec.SetNowFunc(clock)
	return &recordAccess{rec: rec, channel: ChannelName}, rec, cs
}

func TestRecordAccess_InitiatorSetOnce(t *testing.T) {
	now := time.Now()
	p, _, _ := newRecordAccessForTest(&now, channels.DefaultThreadTTL)
	ctx := context.Background()
	if got := p.SetInitiator(ctx, "C1", "T1", "U1"); got != "U1" {
		t.Fatalf("first SetInitiator = %q", got)
	}
	if got := p.SetInitiator(ctx, "C1", "T1", "U2"); got != "U1" {
		t.Fatalf("second SetInitiator displaced the initiator: %q", got)
	}
	if !p.Allowed(ctx, "C1", "T1", "U1") || p.Allowed(ctx, "C1", "T1", "U2") {
		t.Fatal("initiator must instruct, onlooker must not")
	}
	if p.Allowed(ctx, "C1", "T9", "U1") {
		t.Fatal("an unknown thread denies everyone")
	}
}

func TestRecordAccess_GrantIsAdditiveAndSurvivesAnotherAdapter(t *testing.T) {
	now := time.Now()
	p, rec, _ := newRecordAccessForTest(&now, channels.DefaultThreadTTL)
	ctx := context.Background()
	p.SetInitiator(ctx, "C1", "T1", "U1")
	p.Grant(ctx, "C1", "T1", "U2")
	p.Grant(ctx, "C1", "T1", "U3")
	p.Grant(ctx, "C1", "T1", "U2") // idempotent
	// A second policy over the same recorder is what a restarted gateway sees.
	q := &recordAccess{rec: rec, channel: ChannelName}
	for _, u := range []string{"U1", "U2", "U3"} {
		if !q.Allowed(ctx, "C1", "T1", u) {
			t.Fatalf("%s must be allowed after the restart", u)
		}
	}
	if q.Allowed(ctx, "C1", "T1", "U9") {
		t.Fatal("unknown user allowed")
	}
	e, _, _ := rec.ThreadRecord(ctx, ChannelName, "C1", "T1")
	if len(e.Granted) != 2 {
		t.Fatalf("grants must be a set, got %v", e.Granted)
	}
}

// Past its lifetime the thread is forgotten whole — initiator, grants and the
// agent it was bound to — and the next mentioner starts it over.
func TestRecordAccess_ForgottenThreadStartsOver(t *testing.T) {
	now := time.Now()
	p, rec, _ := newRecordAccessForTest(&now, 48*time.Hour)
	ctx := context.Background()
	_ = rec.UpdateThreadRecord(ctx, ChannelName, "C1", "T1", func(e *store.Entry, _ bool) bool {
		e.AgentRef = "issue-agent"
		return true
	})
	p.SetInitiator(ctx, "C1", "T1", "U1")
	p.Grant(ctx, "C1", "T1", "U2")
	now = now.Add(49 * time.Hour)
	if p.Initiator(ctx, "C1", "T1") != "" {
		t.Fatal("a forgotten thread has no initiator")
	}
	if p.Allowed(ctx, "C1", "T1", "U1") || p.Allowed(ctx, "C1", "T1", "U2") {
		t.Fatal("a forgotten thread allows nobody, initiator or granted")
	}
	if got := p.SetInitiator(ctx, "C1", "T1", "U5"); got != "U5" {
		t.Fatalf("the next mentioner must become initiator, got %q", got)
	}
	e, _, _ := rec.ThreadRecord(ctx, ChannelName, "C1", "T1")
	if e.Initiator != "U5" || len(e.Granted) != 0 || e.AgentRef != "" {
		t.Fatalf("the thread was forgotten, not reset: %+v", e)
	}
}

// SetInitiator writes the row once, when it sets the initiator. A later
// message — the initiator's own, or a stranger's that ends up parked — writes
// nothing: the turn refreshes the lifetime, a parked message must not.
func TestRecordAccess_SetInitiatorWritesOnce(t *testing.T) {
	now := time.Now()
	p, _, cs := newRecordAccessForTest(&now, 48*time.Hour)
	ctx := context.Background()
	p.SetInitiator(ctx, "C1", "T1", "U1")
	if cs.writes != 1 {
		t.Fatalf("setting the initiator must write once, got %d", cs.writes)
	}
	p.SetInitiator(ctx, "C1", "T1", "U1")
	p.SetInitiator(ctx, "C1", "T1", "U2")
	if cs.writes != 1 {
		t.Fatalf("later messages must not write the row, got %d writes", cs.writes)
	}
	p.Grant(ctx, "C1", "T1", "U2")
	if cs.writes != 2 {
		t.Fatalf("a grant writes the row, got %d writes", cs.writes)
	}
	now = now.Add(49 * time.Hour)
	if p.Allowed(ctx, "C1", "T1", "U1") {
		t.Fatal("a whole lifetime of silence forgets the thread")
	}
}
