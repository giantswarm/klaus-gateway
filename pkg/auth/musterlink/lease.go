package musterlink

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// muster rotates the refresh token on every refresh request and treats a
// rotated token presented again as a replay: it revokes the token's whole
// family, the token it has just issued included (refresh token rotation with
// reuse detection, RFC 9700 section 4.14.2, as mcp-oauth implements it).
// Gateway processes that share a link store must therefore never present a
// refresh token another process is presenting or has presented. The per-user
// lock orders the refresh requests of one process; the refresh lease below
// orders them across processes, through the store's Update:
//
//   - Before presenting a refresh token, a process writes a lease on it into
//     the stored link, and only if the store still holds that token and no
//     other process holds an unexpired lease on it.
//   - A store that holds another token was written by another process (a
//     refresh, a sign-in): its link is adopted instead, and its id_token is
//     served when it covers a turn.
//   - A lease held by another process is waited out: its holder writes the
//     rotated link, which this process then adopts. A lease write that keeps
//     conflicting with other writers is waited out the same way: the
//     contention is the other processes, likely refreshing this very link.
//   - The rotated link is written only while the store still holds the
//     presented token, which also releases the lease; a link written
//     meanwhile by a sign-in wins.
//
// A lease expires after leaseTTL, longer than a refresh request may take
// (refreshTimeout), so a holder that dies mid-request blocks the token for at
// most that long. Pods whose clocks drift apart by more than leaseTTL minus
// refreshTimeout can both present it.
//
// Two cases present the token without a lease: a link this process holds
// dirty (only this process knows its token, so nobody else can present it),
// and a store that fails the lease's read or write (the refresh then keeps
// the turn working, at the risk of racing a second replica that is cut off
// from the store at the same moment). A store that answers but stays
// contended is not such a failure: the lease is retried until the wait runs
// out.

// refreshAttempts bounds how many times one refresh adopts a link another
// process wrote and tries again.
const refreshAttempts = 3

// refreshLeaseTTL is how long a refresh lease holds before it expires: twice
// the bound on one refresh request, so a live holder always finishes first.
const refreshLeaseTTL = 2 * refreshTimeout

// leaseWaitTimeout bounds how long a process waits for another one's lease;
// a lease cannot legitimately outlast its TTL.
const leaseWaitTimeout = refreshLeaseTTL

// leasePollInterval is how often a process waiting on another one's lease
// re-reads the store.
const leasePollInterval = 250 * time.Millisecond

// errRefreshContended is a transient refresh failure: another gateway process
// held the refresh lease past the wait, or other writers kept replacing the
// link while this process tried to refresh it.
var errRefreshContended = errors.New("musterlink: link contended by another writer during refresh")

// leaseResult is what acquireRefreshLease found in the store.
type leaseResult int

const (
	// leaseHeld: this process holds the lease; present the token.
	leaseHeld leaseResult = iota
	// leaseSkipped: no lease was taken (the link is dirty here, or the store
	// failed); present the token without one.
	leaseSkipped
	// leaseAdopted: the store holds a newer link, returned alongside; use it
	// instead of presenting the token.
	leaseAdopted
)

// acquireRefreshLease takes the refresh lease on link's refresh token for this
// process, waiting while another process holds an unexpired lease on it or
// the store stays contended. It returns ErrNotLinked when the store holds no
// link (a sign-out elsewhere), and leaseAdopted with the stored link when the
// store holds another token. The caller holds the per-user lock.
func (l *Linker) acquireRefreshLease(ctx context.Context, slackUserID string, link *Link) (leaseResult, *Link, error) {
	if l.isDirty(slackUserID) {
		return leaseSkipped, nil, nil
	}
	// The wait is bounded on the wall clock, not l.now: leases are judged by
	// l.now, and a wait bounded by the same clock never ends if it stands still.
	waitUntil := time.Now().Add(l.leaseWait)
	for {
		var (
			absent, busy bool
			stored       *Link
		)
		now := l.now()
		err := l.store.Update(slackUserID, func(current *Link) (*Link, bool) {
			absent, busy, stored = false, false, nil
			switch {
			case current == nil:
				absent = true
				return nil, false
			case current.RefreshToken != link.RefreshToken:
				stored = current
				return nil, false
			case current.Lease != nil && current.Lease.Holder != l.leaseHolder && now.Before(current.Lease.Until):
				busy = true
				return nil, false
			}
			next := *current
			next.Lease = &RefreshLease{Holder: l.leaseHolder, Until: now.Add(l.leaseTTL)}
			return &next, true
		})
		switch {
		case errors.Is(err, errStoreConflict):
			busy = true
		case err != nil:
			l.logger.Warn("musterlink: taking the refresh lease failed, refreshing without one", "slackUser", slackUserID, "err", err)
			return leaseSkipped, nil, nil
		case absent:
			l.forget(slackUserID)
			return leaseSkipped, nil, ErrNotLinked
		case stored != nil:
			l.adopt(slackUserID, stored)
			return leaseAdopted, stored, nil
		}
		if !busy {
			return leaseHeld, nil, nil
		}
		if !time.Now().Before(waitUntil) {
			return leaseSkipped, nil, fmt.Errorf("musterlink: another gateway process still holds the refresh lease: %w", errRefreshContended)
		}
		select {
		case <-ctx.Done():
			return leaseSkipped, nil, fmt.Errorf("musterlink: wait for another gateway process's refresh: %w", ctx.Err())
		case <-time.After(l.leasePoll):
		}
	}
}

// releaseRefreshLease clears this process's lease on presented after a
// refresh request that left the token valid, so other processes need not wait
// for the lease to expire. Best effort: a lease it cannot clear expires on its
// own.
func (l *Linker) releaseRefreshLease(slackUserID, presented string) {
	if err := l.store.Update(slackUserID, func(current *Link) (*Link, bool) {
		if current == nil || current.RefreshToken != presented || current.Lease == nil || current.Lease.Holder != l.leaseHolder {
			return nil, false
		}
		next := *current
		next.Lease = nil
		return &next, true
	}); err != nil {
		l.logger.Warn("musterlink: releasing the refresh lease failed, it expires on its own", "slackUser", slackUserID, "err", err)
	}
}

// saveRotated records updated, the link after presenting presented under this
// process's lease, and writes it to the store only while the store still
// holds presented. It returns the link the store holds instead when a sign-in
// replaced it meanwhile, and ErrNotLinked when a sign-out removed it. A store
// that fails the write keeps updated dirty for the retries, as save does.
func (l *Linker) saveRotated(slackUserID, presented string, updated *Link) (*Link, error) {
	l.linksMu.Lock()
	l.put(slackUserID, *updated, l.now(), true)
	l.linksMu.Unlock()
	var (
		absent bool
		stored *Link
	)
	err := l.store.Update(slackUserID, func(current *Link) (*Link, bool) {
		absent, stored = false, nil
		switch {
		case current == nil:
			absent = true
			return nil, false
		case current.RefreshToken != presented:
			stored = current
			return nil, false
		}
		return updated, true
	})
	switch {
	case err != nil:
		l.logger.Error("musterlink: link store write failed, keeping the link in memory and retrying", "slackUser", slackUserID, "err", err)
		l.scheduleFlush()
		return nil, nil
	case absent:
		l.forget(slackUserID)
		return nil, ErrNotLinked
	case stored != nil:
		l.adopt(slackUserID, stored)
		return stored, nil
	}
	l.markClean(slackUserID)
	return nil, nil
}

// dropRefused removes a link whose refresh token presented muster refused
// with invalid_grant, only while the store still holds presented, and returns
// ErrNotLinked so the caller prompts sign-in on this same turn rather than a
// turn later. It returns the stored link instead when the store holds another
// token (a sign-in landed meanwhile). A store that refuses the delete is
// logged: the next read finds the dead link again and lands here again, which
// is harmless.
func (l *Linker) dropRefused(slackUserID, presented string) (*Link, error) {
	l.forget(slackUserID)
	var stored *Link
	err := l.store.Update(slackUserID, func(current *Link) (*Link, bool) {
		stored = nil
		switch {
		case current == nil:
			return nil, false
		case current.RefreshToken != presented:
			stored = current
			return nil, false
		}
		return nil, true
	})
	switch {
	case err != nil:
		l.logger.Warn("musterlink: dropping a dead link from the store failed", "slackUser", slackUserID, "err", err)
		return nil, ErrNotLinked
	case stored != nil:
		l.adopt(slackUserID, stored)
		return stored, nil
	}
	return nil, ErrNotLinked
}

// adopt replaces this process's copy with link, read from the store just now.
func (l *Linker) adopt(slackUserID string, link *Link) {
	l.linksMu.Lock()
	l.put(slackUserID, *link, l.now(), false)
	l.linksMu.Unlock()
}

// isDirty reports whether this process holds a version of slackUserID's link
// the store has not taken yet.
func (l *Linker) isDirty(slackUserID string) bool {
	l.linksMu.Lock()
	defer l.linksMu.Unlock()
	c, ok := l.links[slackUserID]
	return ok && c.dirty
}
