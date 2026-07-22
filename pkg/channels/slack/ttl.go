package slack

import (
	"sync"
	"time"
)

// threadStateTTL bounds how long per-thread transparency state (details level,
// usage figures, resume-check marks) is retained. Entries past the TTL are
// swept opportunistically on insert, so an idle thread's state cannot
// accumulate forever on a long-lived pod. Active threads refresh their entries
// on every turn.
const threadStateTTL = 24 * time.Hour

// ttlEntry pairs a value with its eviction deadline.
type ttlEntry[V any] struct {
	value   V
	expires time.Time
}

// sweepExpired deletes entries past their deadline. The caller holds the lock
// guarding entries.
func sweepExpired[K comparable, V any](entries map[K]ttlEntry[V], now time.Time) {
	for key, entry := range entries {
		if now.After(entry.expires) {
			delete(entries, key)
		}
	}
}

// markOnce claims key in the throttle map for ttl and reports whether the
// caller won the claim (no live entry existed). Check and set are atomic under
// mu, so concurrent callers act at most once per window; expired siblings are
// swept on insert. entries points at the (possibly nil) map field so the lazy
// init lands back on the adapter.
func markOnce[K comparable](mu *sync.Mutex, entries *map[K]ttlEntry[struct{}], key K, ttl time.Duration) bool {
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if entry, seen := (*entries)[key]; seen && now.Before(entry.expires) {
		return false
	}
	if *entries == nil {
		*entries = make(map[K]ttlEntry[struct{}])
	}
	sweepExpired(*entries, now)
	(*entries)[key] = ttlEntry[struct{}]{expires: now.Add(ttl)}
	return true
}
