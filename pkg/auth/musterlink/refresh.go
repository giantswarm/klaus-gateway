package musterlink

import (
	"context"
	"time"
)

// The id_token a turn forwards is Dex's, with Dex's lifetime; refreshing it
// spends a round trip to muster's token endpoint (about 1.2 s on an
// installation, including muster fetching the gateway's CIMD document). Until
// 1.10 that refresh ran inside TokenFor, on the turn's critical path: the
// first message after every expiry waited for it. The refresher below runs
// the same refresh ahead of time, off any turn, for the people whose token
// this process has recently handed to a turn, so TokenFor finds a fresh
// token and its own refresh stays the fallback for a refresher that could
// not reach muster in time.
const (
	// recordTokenRefresh is the `record` value of the log line every refresh
	// leaves, with `trigger` naming who ran it.
	recordTokenRefresh = "token_refresh"
	// refreshTriggerTurn is a refresh TokenFor ran on a turn's path;
	// refreshTriggerAhead one the refresher ran ahead of expiry.
	refreshTriggerTurn  = "turn"
	refreshTriggerAhead = "ahead"

	// refreshAhead is how long before its expiry a served id_token is
	// refreshed. Well above tokenRefreshSkew (the point at which TokenFor
	// refreshes on the path itself) and above one sweep interval, so a sweep
	// that misses muster once still leaves the next one time to succeed.
	refreshAhead = 5 * time.Minute
	// refreshSweepInterval is how often the refresher looks for due tokens.
	refreshSweepInterval = time.Minute
	// refreshServedWindow bounds whose tokens are kept fresh: a person whose
	// token no turn asked for in this long is left to TokenFor's own refresh
	// on their next message, so an idle person's link is not rotated every
	// lifetime for nobody.
	refreshServedWindow = 48 * time.Hour
	// refreshTimeout bounds one refresh of the sweep.
	refreshTimeout = 30 * time.Second
)

// noteServed records that a turn was just handed slackUserID's token, which
// makes the person eligible for the refresher.
func (l *Linker) noteServed(slackUserID string) {
	l.linksMu.Lock()
	defer l.linksMu.Unlock()
	if c, ok := l.links[slackUserID]; ok {
		c.served = l.now()
	}
}

// refreshDueAt reports whether link's id_token is within refreshAhead of
// expiry at now (an unknown expiry is TokenFor's to sort out).
func refreshDueAt(link *Link, now time.Time) bool {
	return !link.Expiry.IsZero() && !now.Add(refreshAhead).Before(link.Expiry)
}

// dueForRefresh lists the people whose token a turn asked for within the
// served window and whose cached id_token is due.
func (l *Linker) dueForRefresh(now time.Time) []string {
	l.linksMu.Lock()
	defer l.linksMu.Unlock()
	var due []string
	for id, c := range l.links {
		if c.served.IsZero() || now.Sub(c.served) > refreshServedWindow {
			continue
		}
		if refreshDueAt(&c.link, now) {
			due = append(due, id)
		}
	}
	return due
}

// RefreshDue refreshes every due token once (see dueForRefresh) and returns
// how many it refreshed. The refresher calls it every sweep; tests call it
// directly. Each refresh runs under the person's lock, so a turn that
// refreshes concurrently is not raced: whoever is second finds a fresh token
// and does nothing.
func (l *Linker) RefreshDue(ctx context.Context) int {
	refreshed := 0
	for _, id := range l.dueForRefresh(l.now()) {
		if l.refreshAheadOfExpiry(ctx, id) {
			refreshed++
		}
	}
	return refreshed
}

// refreshAheadOfExpiry refreshes one person's token ahead of its expiry and
// reports whether it did. A failure is logged by refreshLink and leaves the
// cached token in place: the next sweep tries again, and past expiry TokenFor
// refreshes on the turn as before. A refusal (invalid_grant) drops the link
// the way a turn's refresh would; the person is asked to sign in on their
// next message.
func (l *Linker) refreshAheadOfExpiry(ctx context.Context, slackUserID string) bool {
	unlock := l.lockUser(slackUserID)
	defer unlock()
	link, err := l.load(slackUserID)
	if err != nil {
		return false
	}
	if !refreshDueAt(link, l.now()) {
		return false // a turn refreshed it meanwhile
	}
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	_, _, err = l.refreshLink(rctx, slackUserID, link, refreshTriggerAhead)
	return err == nil
}

// runRefresher sweeps for due tokens every refreshSweepInterval until
// stopRefresher.
func (l *Linker) runRefresher() {
	defer close(l.refreshDone)
	ticker := time.NewTicker(refreshSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.refreshStop:
			return
		case <-ticker.C:
			l.RefreshDue(context.Background())
		}
	}
}

// stopRefresher ends the refresher and waits for a sweep in progress to
// finish. Idempotent.
func (l *Linker) stopRefresher() {
	select {
	case <-l.refreshStop:
	default:
		close(l.refreshStop)
	}
	<-l.refreshDone
}
