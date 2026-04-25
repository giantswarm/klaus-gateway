// Package klausctl implements the lifecycle.Manager backed by the local
// klausctl CLI. It shells out to `klausctl <subcommand> -o json` and parses
// the result.
package klausctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// Runner executes commands. Tests swap it with a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{ bin string }

func (r execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	// The binary name and args originate from this package's own configuration
	// (the klausctl path) and validated lifecycle requests. There is no
	// untrusted user input flowing into the command.
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("klausctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Manager shells out to klausctl.
type Manager struct {
	bin    string
	runner Runner
}

// Option customises a Manager.
type Option func(*Manager)

// WithRunner replaces the default exec-based runner. Tests use this.
func WithRunner(r Runner) Option { return func(m *Manager) { m.runner = r } }

// New returns a Manager that invokes the given binary. If bin is empty, the
// first klausctl on PATH is used.
func New(bin string, opts ...Option) (*Manager, error) {
	if bin == "" {
		bin = "klausctl"
	}
	m := &Manager{bin: bin, runner: execRunner{bin: bin}}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

type klausctlInstance struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url,omitempty"`
	MCPURL  string `json:"mcp_url,omitempty"`
	Status  string `json:"status,omitempty"`
}

func (k klausctlInstance) toRef() lifecycle.InstanceRef {
	return lifecycle.InstanceRef{Name: k.Name, BaseURL: k.BaseURL, MCPURL: k.MCPURL, Status: k.Status}
}

// Get returns the instance by name via `klausctl status`.
func (m *Manager) Get(ctx context.Context, name string) (lifecycle.InstanceRef, error) {
	out, err := m.runner.Run(ctx, m.bin, "status", name, "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
		}
		return lifecycle.InstanceRef{}, err
	}
	var inst klausctlInstance
	if err := json.Unmarshal(out, &inst); err != nil {
		return lifecycle.InstanceRef{}, fmt.Errorf("decode klausctl status: %w", err)
	}
	if inst.Name == "" {
		return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
	}
	return inst.toRef(), nil
}

// Create runs `klausctl run` with the spec as flags.
func (m *Manager) Create(ctx context.Context, spec lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
	if spec.Name == "" {
		return lifecycle.InstanceRef{}, errors.New("klausctl: spec.Name is required")
	}
	args := []string{"run", spec.Name, "-o", "json"}
	if spec.Channel != "" {
		args = append(args, "--channel", spec.Channel)
	}
	if spec.ChannelID != "" {
		args = append(args, "--channel-id", spec.ChannelID)
	}
	if spec.UserID != "" {
		args = append(args, "--user", spec.UserID)
	}
	if spec.ThreadID != "" {
		args = append(args, "--thread", spec.ThreadID)
	}
	out, err := m.runner.Run(ctx, m.bin, args...)
	if err != nil {
		return lifecycle.InstanceRef{}, err
	}
	var inst klausctlInstance
	if err := json.Unmarshal(out, &inst); err != nil {
		return lifecycle.InstanceRef{}, fmt.Errorf("decode klausctl run: %w", err)
	}
	return inst.toRef(), nil
}

// List runs `klausctl list`.
func (m *Manager) List(ctx context.Context) ([]lifecycle.InstanceRef, error) {
	out, err := m.runner.Run(ctx, m.bin, "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	var raw []klausctlInstance
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode klausctl list: %w", err)
	}
	refs := make([]lifecycle.InstanceRef, 0, len(raw))
	for _, r := range raw {
		refs = append(refs, r.toRef())
	}
	return refs, nil
}

// Stop runs `klausctl stop`.
func (m *Manager) Stop(ctx context.Context, name string) error {
	_, err := m.runner.Run(ctx, m.bin, "stop", name)
	return err
}
