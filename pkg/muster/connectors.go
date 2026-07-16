package muster

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// defaultStatusTTL is how long a user's auth://status read is served from
// cache. Short: the status drives UX prompts, not authorization.
const defaultStatusTTL = 30 * time.Second

// TokenSource returns the caller's own muster bearer for a Slack user;
// musterlink.(*Linker).TokenFor satisfies it. Errors propagate unwrapped so
// callers can match musterlink.ErrNotLinked.
type TokenSource func(ctx context.Context, slackUserID string) (string, error)

// Connectors reports per-user connector-backend auth status and mints login
// URLs, using each user's own token. It is the seam channel adapters consume;
// the gateway's calls run over the same bearer as the user's agent traffic,
// so both resolve to the same muster session.
type Connectors struct {
	Client *Client
	Token  TokenSource
	// TTL overrides the status cache duration. Zero uses defaultStatusTTL.
	TTL    time.Duration
	Logger *slog.Logger

	mu    sync.Mutex
	cache map[string]statusEntry // slackUserID -> cached statuses
}

// statusEntry is a cached per-user status read with its expiry.
type statusEntry struct {
	servers []ServerStatus
	expires time.Time
}

func (s *Connectors) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return defaultStatusTTL
}

// Status returns the user's per-backend auth statuses, served from a short
// cache so per-turn checks stay cheap on chatty threads.
func (s *Connectors) Status(ctx context.Context, slackUserID string) ([]ServerStatus, error) {
	s.mu.Lock()
	entry, ok := s.cache[slackUserID]
	s.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.servers, nil
	}
	return s.FreshStatus(ctx, slackUserID)
}

// FreshStatus bypasses the cache (and repopulates it): the /login listing
// must reflect a login the user just completed.
func (s *Connectors) FreshStatus(ctx context.Context, slackUserID string) ([]ServerStatus, error) {
	token, err := s.Token(ctx, slackUserID)
	if err != nil {
		return nil, err
	}
	servers, err := s.Client.AuthStatus(ctx, token)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = make(map[string]statusEntry)
	}
	s.cache[slackUserID] = statusEntry{servers: servers, expires: time.Now().Add(s.ttl())}
	s.mu.Unlock()
	return servers, nil
}

// LoginURL mints a login URL for the named backend with the user's own token.
// Never cached: each URL carries a single-use state with a short expiry, so
// it is fetched only at the moment it is presented.
func (s *Connectors) LoginURL(ctx context.Context, slackUserID, server string) (string, error) {
	token, err := s.Token(ctx, slackUserID)
	if err != nil {
		return "", err
	}
	return s.Client.LoginURL(ctx, token, server)
}

// Logout disconnects the named backend for the user and invalidates their
// cached status.
func (s *Connectors) Logout(ctx context.Context, slackUserID, server string) error {
	token, err := s.Token(ctx, slackUserID)
	if err != nil {
		return err
	}
	if err := s.Client.Logout(ctx, token, server); err != nil {
		return err
	}
	s.Invalidate(slackUserID)
	return nil
}

// Invalidate drops the user's cached status so the next Status call re-reads.
func (s *Connectors) Invalidate(slackUserID string) {
	s.mu.Lock()
	delete(s.cache, slackUserID)
	s.mu.Unlock()
}
