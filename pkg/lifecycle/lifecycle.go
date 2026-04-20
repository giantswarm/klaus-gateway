// Package lifecycle defines the interface klaus-gateway uses to create and
// query klaus instances. The `klausctl` driver shells out to the local
// CLI; the `operator` driver speaks to klaus-operator over MCP.
package lifecycle

import (
	"context"
	"errors"
)

// ErrNotFound is returned when an instance lookup fails.
var ErrNotFound = errors.New("instance not found")

// InstanceRef describes a reachable klaus instance.
type InstanceRef struct {
	Name    string
	BaseURL string
	MCPURL  string
	Status  string
}

// CreateSpec is the input to Manager.Create. Fields are a deliberately small
// superset of what klausctl and klaus-operator both understand; driver-specific
// extensions live in driver packages.
type CreateSpec struct {
	Name      string
	Channel   string
	ChannelID string
	UserID    string
	ThreadID  string
	Metadata  map[string]string
}

// Manager is the lifecycle driver interface.
type Manager interface {
	Get(ctx context.Context, name string) (InstanceRef, error)
	Create(ctx context.Context, spec CreateSpec) (InstanceRef, error)
	List(ctx context.Context) ([]InstanceRef, error)
	Stop(ctx context.Context, name string) error
}
