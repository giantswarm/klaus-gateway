// Package routing resolves (channel, channel-id, user, thread) to a klaus
// instance, creating a new instance on a cache miss when AutoCreate is on.
package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// ErrRouteNotFound is returned when a key is absent and auto-create is off.
var ErrRouteNotFound = errors.New("route not found")

// InboundMessage is the routing input.
type InboundMessage struct {
	Channel   string
	ChannelID string
	UserID    string
	ThreadID  string
	// NameHint is used as the instance name if auto-create fires. When empty
	// the router synthesises a deterministic name from the key.
	NameHint string
	Metadata map[string]string
}

// Key returns the store key for this message.
func (m InboundMessage) Key() store.Key {
	return store.Key{
		Channel:   m.Channel,
		ChannelID: m.ChannelID,
		UserID:    m.UserID,
		ThreadID:  m.ThreadID,
	}
}

// Router resolves inbound messages to instances.
type Router struct {
	Store      store.Store
	Lifecycle  lifecycle.Manager
	AutoCreate bool
	DefaultTTL time.Duration

	now func() time.Time
}

// New builds a Router with sensible defaults.
func New(s store.Store, lm lifecycle.Manager, autoCreate bool, ttl time.Duration) *Router {
	return &Router{
		Store:      s,
		Lifecycle:  lm,
		AutoCreate: autoCreate,
		DefaultTTL: ttl,
		now:        time.Now,
	}
}

// SetNowFunc is a test hook.
func (r *Router) SetNowFunc(f func() time.Time) { r.now = f }

// Resolve returns the instance for msg, creating one on miss when AutoCreate
// is enabled. On hit the entry's LastSeen is refreshed.
func (r *Router) Resolve(ctx context.Context, msg InboundMessage) (lifecycle.InstanceRef, error) {
	k := msg.Key()
	entry, ok, err := r.Store.Get(ctx, k)
	if err != nil {
		return lifecycle.InstanceRef{}, fmt.Errorf("store get: %w", err)
	}
	if ok {
		entry.LastSeen = r.now()
		if err := r.Store.Put(ctx, k, entry); err != nil {
			return lifecycle.InstanceRef{}, fmt.Errorf("store put: %w", err)
		}
		ref, err := r.Lifecycle.Get(ctx, entry.Instance)
		if err != nil {
			if errors.Is(err, lifecycle.ErrNotFound) {
				// Fall through to re-create if auto-create is on; otherwise
				// surface the miss so the caller can decide.
				if !r.AutoCreate {
					return lifecycle.InstanceRef{}, ErrRouteNotFound
				}
			} else {
				return lifecycle.InstanceRef{}, err
			}
		} else {
			return ref, nil
		}
	}

	if !r.AutoCreate {
		return lifecycle.InstanceRef{}, ErrRouteNotFound
	}

	name := msg.NameHint
	if name == "" {
		name = synthName(msg)
	}
	ref, err := r.Lifecycle.Create(ctx, lifecycle.CreateSpec{
		Name:      name,
		Channel:   msg.Channel,
		ChannelID: msg.ChannelID,
		UserID:    msg.UserID,
		ThreadID:  msg.ThreadID,
		Metadata:  msg.Metadata,
	})
	if err != nil {
		return lifecycle.InstanceRef{}, fmt.Errorf("lifecycle create: %w", err)
	}
	now := r.now()
	if err := r.Store.Put(ctx, k, store.Entry{
		Instance:  ref.Name,
		CreatedAt: now,
		LastSeen:  now,
		TTL:       r.DefaultTTL,
	}); err != nil {
		return lifecycle.InstanceRef{}, fmt.Errorf("store put: %w", err)
	}
	return ref, nil
}

func synthName(m InboundMessage) string {
	base := fmt.Sprintf("%s-%s-%s", safe(m.Channel), safe(m.ChannelID), safe(m.ThreadID))
	if len(base) > 60 {
		base = base[:60]
	}
	return "klaus-" + base
}

func safe(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "x"
	}
	return string(out)
}
