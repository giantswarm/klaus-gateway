package musterlink

import (
	"errors"
	"fmt"
	"time"
)

// The Linker keeps a process-local copy of every link it has read from or
// written to the Store. Two things depend on it:
//
//   - A refresh token muster has rotated must never be lost. TokenFor spends
//     the stored refresh token, muster invalidates it and issues a new one, and
//     only then is the new one written. When that write fails (the Kubernetes
//     API away, a resourceVersion conflict past the retries, a full disk under
//     the bolt file) the store still holds the spent token: the next refresh
//     would fail invalid_grant and the person would have to sign in again. So
//     save records the link here first and writes the store second; a failed
//     write leaves the entry dirty, to be written by flush -- in the background
//     with a doubling delay, and once more on Close.
//   - A store that cannot be read is not "nobody is linked". load serves the
//     copy it has, and reports a failed read as such only for a person this
//     process has never seen; the caller turns that into a transient error, not
//     a sign-in prompt.
//
// The copy also answers reads for linkCacheTTL before the store is asked again,
// so a Slack turn (which reads the link two or three times) costs one store
// read instead of one per read. Only links are cached, never their absence: a
// sign-in completed by another process is seen on the next read.

// linkCacheTTL is how long a link read from the store is served from the
// process-local copy before the store is asked again. Short, so a link written
// by another process (a second replica's refresh, a sign-out) is picked up
// within a turn or two; a refresh token rotated elsewhere in the meantime is
// handled by TokenFor's re-read on invalid_grant.
const linkCacheTTL = 30 * time.Second

// writeRetryDelay is the first delay before a failed store write is retried;
// every further failure doubles it up to maxWriteRetryDelay.
const (
	writeRetryDelay    = 2 * time.Second
	maxWriteRetryDelay = 30 * time.Second
)

// cachedLink is one entry of the Linker's process-local copy of the store.
type cachedLink struct {
	link    Link
	checked time.Time // when the store was last consulted for this user
	dirty   bool      // the store does not hold this version yet (a Put failed)
}

// load returns the link for slackUserID: from the process-local copy when it
// was checked against the store within cacheTTL or holds a write the store has
// not taken yet, else from the store. A store read that fails serves the copy
// when there is one (logged, and not retried before cacheTTL so a failing store
// is not hit on every message) and is otherwise returned wrapped, so the
// caller can tell a failed store from ErrNotLinked.
func (l *Linker) load(slackUserID string) (*Link, error) {
	now := l.now()
	l.linksMu.Lock()
	if c, ok := l.links[slackUserID]; ok && (c.dirty || now.Sub(c.checked) < l.cacheTTL) {
		link := c.link
		l.linksMu.Unlock()
		return &link, nil
	}
	l.linksMu.Unlock()

	link, err := l.store.Get(slackUserID)

	l.linksMu.Lock()
	defer l.linksMu.Unlock()
	// Re-read the entry: a save, reload or another load may have run while the
	// store answered, and what it left is newer than the store's answer (a
	// refresh that landed meanwhile rotated the token the answer carries).
	if c, ok := l.links[slackUserID]; ok && (c.dirty || !c.checked.Before(now)) {
		link := c.link
		return &link, nil
	}
	switch {
	case err == nil:
		l.links[slackUserID] = &cachedLink{link: *link, checked: now}
		return link, nil
	case errors.Is(err, ErrNotLinked):
		delete(l.links, slackUserID)
		return nil, ErrNotLinked
	default:
		if c, ok := l.links[slackUserID]; ok {
			l.logger.Warn("musterlink: link store read failed, serving the link this process knows", "slackUser", slackUserID, "err", err)
			c.checked = now
			link := c.link
			return &link, nil
		}
		return nil, fmt.Errorf("musterlink: read link: %w", err)
	}
}

// reload reads slackUserID from the store past the process-local copy and
// replaces the copy with what the store holds. TokenFor uses it before
// dropping a link on invalid_grant. The caller holds the per-user lock.
func (l *Linker) reload(slackUserID string) (*Link, error) {
	link, err := l.store.Get(slackUserID)
	if err != nil {
		return nil, err
	}
	l.linksMu.Lock()
	l.links[slackUserID] = &cachedLink{link: *link, checked: l.now()}
	l.linksMu.Unlock()
	return link, nil
}

// save records link as this process's current version for slackUserID and
// writes it to the store. A failed write keeps the link, marked dirty, and
// schedules a retry: the refresh token in it is the one muster holds now, and
// dropping it would sign the person out on the next refresh. The caller holds
// the per-user lock, which orders saves and drops of one user.
func (l *Linker) save(slackUserID string, link *Link) {
	l.linksMu.Lock()
	l.links[slackUserID] = &cachedLink{link: *link, checked: l.now(), dirty: true}
	l.linksMu.Unlock()
	if err := l.store.Put(slackUserID, link); err != nil {
		l.logger.Error("musterlink: link store write failed, keeping the link in memory and retrying", "slackUser", slackUserID, "err", err)
		l.scheduleFlush()
		return
	}
	l.markClean(slackUserID)
}

// markClean records that the store holds this process's version of the link.
func (l *Linker) markClean(slackUserID string) {
	l.linksMu.Lock()
	defer l.linksMu.Unlock()
	if c, ok := l.links[slackUserID]; ok {
		c.dirty = false
		c.checked = l.now()
	}
}

// drop forgets slackUserID in the process-local copy and deletes it from the
// store, returning the store's error. The caller holds the per-user lock.
func (l *Linker) drop(slackUserID string) error {
	l.linksMu.Lock()
	delete(l.links, slackUserID)
	l.linksMu.Unlock()
	if err := l.store.Delete(slackUserID); err != nil {
		return fmt.Errorf("musterlink: delete link: %w", err)
	}
	return nil
}

// dirtyIDs lists the users whose link the store does not hold yet.
func (l *Linker) dirtyIDs() []string {
	l.linksMu.Lock()
	defer l.linksMu.Unlock()
	var ids []string
	for id, c := range l.links {
		if c.dirty {
			ids = append(ids, id)
		}
	}
	return ids
}

// Flush writes every link the store has not taken yet and returns how many are
// still pending. The Linker flushes on its own in the background after a failed
// write; Close calls it once more so a shutdown loses as little as possible.
func (l *Linker) Flush() int {
	pending := 0
	for _, id := range l.dirtyIDs() {
		if !l.flushOne(id) {
			pending++
		}
	}
	return pending
}

// flushOne retries the store write for one user under their lock and reports
// whether the store holds the link afterwards (or nothing was left to write).
func (l *Linker) flushOne(slackUserID string) bool {
	unlock := l.lockUser(slackUserID)
	defer unlock()
	l.linksMu.Lock()
	c, ok := l.links[slackUserID]
	if !ok || !c.dirty {
		l.linksMu.Unlock()
		return true
	}
	link := c.link
	l.linksMu.Unlock()
	if err := l.store.Put(slackUserID, &link); err != nil {
		l.logger.Warn("musterlink: link store write retry failed", "slackUser", slackUserID, "err", err)
		return false
	}
	l.logger.Info("musterlink: link store write retry succeeded", "slackUser", slackUserID)
	l.markClean(slackUserID)
	return true
}

// scheduleFlush arms one background Flush after the current retry delay unless
// one is pending or the Linker is closed. A Flush that leaves links pending
// doubles the delay (up to maxWriteRetryDelay) and re-arms; one that writes
// everything resets it.
func (l *Linker) scheduleFlush() {
	l.linksMu.Lock()
	defer l.linksMu.Unlock()
	if l.flushTimer != nil || l.closed {
		return
	}
	l.flushTimer = time.AfterFunc(l.nextDelay, func() {
		l.linksMu.Lock()
		l.flushTimer = nil
		l.linksMu.Unlock()
		if l.Flush() == 0 {
			l.linksMu.Lock()
			l.nextDelay = l.retryDelay
			l.linksMu.Unlock()
			return
		}
		l.linksMu.Lock()
		l.nextDelay = min(l.nextDelay*2, maxWriteRetryDelay)
		l.linksMu.Unlock()
		l.scheduleFlush()
	})
}

// Close stops the background write retries and flushes the links the store
// has not taken yet, once. It returns an error naming how many links the store
// still does not hold. Call it before closing the store.
func (l *Linker) Close() error {
	l.linksMu.Lock()
	l.closed = true
	if l.flushTimer != nil {
		l.flushTimer.Stop()
		l.flushTimer = nil
	}
	l.linksMu.Unlock()
	if n := l.Flush(); n > 0 {
		return fmt.Errorf("musterlink: %d link(s) not written to the store", n)
	}
	return nil
}
