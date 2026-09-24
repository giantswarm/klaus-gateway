// Package storetest holds helpers for tests that put rows in a routing store.
// The store has no plain write: every writer goes through Store.Update.
package storetest

import (
	"context"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// Put writes e at k through Update, replacing whatever the row held.
func Put(ctx context.Context, s store.Store, k store.Key, e store.Entry) error {
	return s.Update(ctx, k, func(cur *store.Entry, _ bool) bool {
		*cur = e
		return true
	})
}

// Expire replaces the row at k with one that has already expired, which every
// store reads as absent (Valkey removes the key): the stand-in for a row that
// was lost or aged out.
func Expire(ctx context.Context, s store.Store, k store.Key) error {
	return Put(ctx, s, k, store.Entry{LastSeen: time.Unix(1, 0), TTL: time.Nanosecond})
}
