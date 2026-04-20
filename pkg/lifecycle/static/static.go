// Package static is a lifecycle.Manager that serves a fixed set of
// instances declared at startup.
//
// It is intended for compose / CI smoke harnesses where neither klausctl nor
// klaus-operator is available, and as a minimal fallback for single-instance
// deployments. Create returns the existing entry when name matches and
// errors otherwise -- the static driver never provisions new instances.
package static

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// Manager holds the instance mapping in memory.
type Manager struct {
	mu        sync.RWMutex
	instances map[string]lifecycle.InstanceRef
}

// New parses a comma-separated spec of the form `name=baseURL[,...]`.
// Whitespace around entries is ignored. An empty spec yields a Manager with
// no instances.
func New(spec string) (*Manager, error) {
	m := &Manager{instances: map[string]lifecycle.InstanceRef{}}
	for _, raw := range strings.Split(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		name, url, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		url = strings.TrimSpace(url)
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("static: invalid entry %q: expected name=baseURL", raw)
		}
		m.instances[name] = lifecycle.InstanceRef{Name: name, BaseURL: url, Status: "ready"}
	}
	return m, nil
}

// Get returns the ref for name or lifecycle.ErrNotFound.
func (m *Manager) Get(_ context.Context, name string) (lifecycle.InstanceRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ref, ok := m.instances[name]; ok {
		return ref, nil
	}
	return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
}

// Create returns the existing entry when spec.Name matches a configured
// instance. When exactly one instance is configured, it is used as the
// fallback for any name so channel-driven auto-create flows resolve to the
// single known instance (this is the intended behaviour for the compose /
// single-instance use case). It never provisions new instances.
func (m *Manager) Create(_ context.Context, spec lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ref, ok := m.instances[spec.Name]; ok {
		return ref, nil
	}
	if len(m.instances) == 1 {
		for _, ref := range m.instances {
			return ref, nil
		}
	}
	return lifecycle.InstanceRef{}, fmt.Errorf("static: refusing to create %q: not in the pre-configured instance set", spec.Name)
}

// List returns every instance in the set, in no guaranteed order.
func (m *Manager) List(context.Context) ([]lifecycle.InstanceRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]lifecycle.InstanceRef, 0, len(m.instances))
	for _, ref := range m.instances {
		out = append(out, ref)
	}
	return out, nil
}

// Stop is a no-op; static instances are externally managed.
func (m *Manager) Stop(context.Context, string) error { return nil }
