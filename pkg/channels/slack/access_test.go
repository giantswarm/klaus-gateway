package slack

import (
	"context"
	"testing"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func newRecordAccessForTest(now *time.Time) (*recordAccess, *memoryRecorder) {
	rec := newMemoryRecorder()
	rec.now = func() time.Time { return *now }
	a := &Adapter{}
	p := &recordAccess{rec: rec, channel: ChannelName, lock: a.recordLock}
	return p, rec
}

func TestRecordAccess_InitiatorSetOnce(t *testing.T) {
	now := time.Now()
	p, _ := newRecordAccessForTest(&now)
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
	p, rec := newRecordAccessForTest(&now)
	ctx := context.Background()
	p.SetInitiator(ctx, "C1", "T1", "U1")
	p.Grant(ctx, "C1", "T1", "U2")
	p.Grant(ctx, "C1", "T1", "U3")
	p.Grant(ctx, "C1", "T1", "U2") // idempotent
	// A second policy over the same recorder is what a restarted gateway sees.
	b := &Adapter{}
	q := &recordAccess{rec: rec, channel: ChannelName, lock: b.recordLock}
	for _, u := range []string{"U1", "U2", "U3"} {
		if !q.Allowed(ctx, "C1", "T1", u) {
			t.Fatalf("%s must be allowed after the restart", u)
		}
	}
	if q.Allowed(ctx, "C1", "T1", "U9") {
		t.Fatal("unknown user allowed")
	}
	r, _, _ := rec.ThreadRecord(ctx, ChannelName, "C1", "T1")
	if len(r.Granted) != 2 {
		t.Fatalf("grants must be a set, got %v", r.Granted)
	}
}

// Past its lifetime the thread is forgotten whole — initiator, grants and the
// agent it was bound to — and the next mentioner opens a fresh conversation.
func TestRecordAccess_ForgottenThreadStartsOver(t *testing.T) {
	now := time.Now()
	p, rec := newRecordAccessForTest(&now)
	rec.SetTTL(48 * time.Hour)
	ctx := context.Background()
	_ = rec.SaveThreadRecord(ctx, ChannelName, "C1", "T1", store.Thread{AgentRef: "issue-agent"})
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
	r, _, _ := rec.ThreadRecord(ctx, ChannelName, "C1", "T1")
	if r.Initiator != "U5" || len(r.Granted) != 0 || r.AgentRef != "" {
		t.Fatalf("the thread was forgotten, not reset: %+v", r.Thread)
	}
}

func TestRecordAccess_ActivityRefreshesTheLifetime(t *testing.T) {
	now := time.Now()
	p, rec := newRecordAccessForTest(&now)
	rec.SetTTL(48 * time.Hour)
	ctx := context.Background()
	p.SetInitiator(ctx, "C1", "T1", "U1")
	now = now.Add(40 * time.Hour)
	p.SetInitiator(ctx, "C1", "T1", "U1") // a handled message refreshes LastSeen
	now = now.Add(40 * time.Hour)
	if !p.Allowed(ctx, "C1", "T1", "U1") {
		t.Fatal("activity inside the lifetime must keep the thread alive")
	}
	now = now.Add(49 * time.Hour)
	if p.Allowed(ctx, "C1", "T1", "U1") {
		t.Fatal("a whole lifetime of silence forgets the thread")
	}
}

func TestAccessPolicy_InProcessFallback(t *testing.T) {
	a := &Adapter{}
	ctx := context.Background()
	if got := a.accessPolicy().SetInitiator(ctx, "C1", "T001", "U001"); got != "U001" {
		t.Fatalf("SetInitiator over the in-process fallback = %q", got)
	}
	if !a.accessPolicy().Allowed(ctx, "C1", "T001", "U001") {
		t.Fatal("the in-process fallback must remember the initiator")
	}
}
