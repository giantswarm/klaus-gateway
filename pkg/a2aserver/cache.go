package a2aserver

import (
	"crypto/sha256"
	"sync"
	"time"
)

// maxCachedExchanges bounds the exchanged tokens kept in memory.
const maxCachedExchanges = 10000

// exchangeCache keeps exchanged tokens by the SHA-256 of the A2A client's
// token. An entry is used only while the client's token is valid and the
// exchanged token has the minimum lifetime left, so a cached token never
// outlives the token it was exchanged for. The zero value is ready to use.
type exchangeCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]cachedExchange
}

type cachedExchange struct {
	token ExchangedToken
	// subjectExpiry is the A2A client's token's exp.
	subjectExpiry time.Time
}

func (c *exchangeCache) get(key [sha256.Size]byte, now time.Time, minLifetime time.Duration) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !entry.usable(now, minLifetime) {
		delete(c.entries, key)
		return "", false
	}
	return entry.token.Token, true
}

// put keeps token until subjectExpiry. A token whose subject states no
// expiry is not kept, nor is any token while the cache is full of entries
// that are still valid.
func (c *exchangeCache) put(key [sha256.Size]byte, token ExchangedToken, subjectExpiry, now time.Time) {
	if subjectExpiry.IsZero() || !subjectExpiry.After(now) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[[sha256.Size]byte]cachedExchange)
	}
	if len(c.entries) >= maxCachedExchanges {
		for k, entry := range c.entries {
			if !entry.usable(now, 0) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxCachedExchanges {
			return
		}
	}
	c.entries[key] = cachedExchange{token: token, subjectExpiry: subjectExpiry}
}

func (e cachedExchange) usable(now time.Time, minLifetime time.Duration) bool {
	return now.Before(e.subjectExpiry) && e.token.Expiry.Sub(now) >= max(minLifetime, 0) && now.Before(e.token.Expiry)
}
