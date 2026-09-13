// Package a2a is klaus-gateway's client for a kagent API v2 controller: the
// A2A v1 protocol over gRPC for the agent turns, and kagent's own gRPC
// services (kagent.api.v1alpha1) for the AgentTemplate roster, the
// AgentInstance that holds a conversation, and the model behind a template.
//
// Every call is made as the person behind the channel turn: the caller's Dex
// id_token rides as the `authorization` metadata entry and, on the A2A calls,
// the conversation's AgentInstance id as `x-kagent-agent-instance-id`. The
// gateway never presents its own machine identity to the controller.
package a2a

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// InstanceIDHeader is the gRPC metadata entry that routes an A2A call to the
// AgentInstance holding the conversation. The controller requires exactly one.
const InstanceIDHeader = "x-kagent-agent-instance-id"

// Target schemes accepted by ParseTarget.
const (
	SchemePlaintext = "grpc"  // h2c, the in-cluster agentgateway Service
	SchemeTLS       = "grpcs" // TLS, the public hostname
)

var (
	// ErrNoIdentity is returned when a call needs the caller's bearer token and
	// the request context carries none. The controller is only ever spoken to
	// as a person; there is no machine-identity fallback.
	ErrNoIdentity = errors.New("a2a: no caller identity for the kagent controller")

	// ErrPayloadTooLarge is returned when the controller rejects a turn because
	// its request exceeds the message size the route accepts. Channels match it
	// with errors.Is to render an actionable "too large" notice instead of the
	// generic turn-failed message.
	ErrPayloadTooLarge = errors.New("a2a: request payload too large")

	// ErrInstanceBusy is returned when the controller refuses a new task because
	// the AgentInstance already has an active one. One task runs per instance;
	// channels map it to their "still working" notice.
	ErrInstanceBusy = errors.New("a2a: the conversation's agent is still working on a previous message")

	// ErrInstanceNotFound is returned when the AgentInstance a thread is bound to
	// no longer exists at the controller.
	ErrInstanceNotFound = errors.New("a2a: agent instance not found")

	// ErrAgentUnknown is returned when an agent ref names no AgentTemplate in
	// the served namespace.
	ErrAgentUnknown = errors.New("a2a: unknown agent")

	// ErrAgentUnavailable is returned when an AgentTemplate exists but cannot be
	// selected: no Harness admits it, or the admitting Harness has not compiled
	// a ready revision yet. The error message carries the reason.
	ErrAgentUnavailable = errors.New("a2a: agent unavailable")
)

// Config configures a Client.
type Config struct {
	// Target is the controller's gRPC endpoint, reached through agentgateway:
	// grpc://host:port (plaintext h2c) or grpcs://host:port (TLS).
	Target string
	// CAFile optionally names a PEM bundle trusted for a grpcs target in
	// addition to the system roots.
	CAFile string
	// Namespace is the namespace whose AgentTemplates are served; a bare agent
	// ref is resolved in it.
	Namespace string
	// TokenSource yields the caller's bearer token for every call.
	TokenSource TokenSource
	// FallbackIconURLTemplate supplies an agent icon when the AgentTemplate
	// carries no icon-URL annotation. "{agent}" is replaced with the agent's
	// technical name. Empty leaves the icon empty.
	FallbackIconURLTemplate string
	Logger                  *slog.Logger
}

// Client speaks to one kagent controller. It is safe for concurrent use.
type Client struct {
	a2a       *a2aclient.Client
	templates apiv1alpha1.AgentTemplateServiceClient
	instances apiv1alpha1.AgentInstanceServiceClient
	models    apiv1alpha1.ModelServiceClient

	namespace    string
	tokens       TokenSource
	iconTemplate string
	logger       *slog.Logger
	closeConn    func() error

	roster rosterCache
}

// ParseTarget splits a grpc:// or grpcs:// target into the host:port the gRPC
// client dials and whether the connection uses TLS.
func ParseTarget(target string) (hostPort string, useTLS bool, err error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", false, fmt.Errorf("a2a: parse target %q: %w", target, err)
	}
	switch u.Scheme {
	case SchemePlaintext:
	case SchemeTLS:
		useTLS = true
	default:
		return "", false, fmt.Errorf("a2a: target %q must use the %s:// or %s:// scheme", target, SchemePlaintext, SchemeTLS)
	}
	if u.Host == "" || u.Port() == "" || u.Path != "" || u.RawQuery != "" {
		return "", false, fmt.Errorf("a2a: target %q must be %s://host:port with no path", target, u.Scheme)
	}
	return u.Host, useTLS, nil
}

// Dial builds a Client for cfg.Target. The connection is established lazily
// on the first call.
func Dial(cfg Config) (*Client, error) {
	hostPort, useTLS, err := ParseTarget(cfg.Target)
	if err != nil {
		return nil, err
	}
	creds, err := transportCredentials(useTLS, hostPort, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(hostPort, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("a2a: dial %s: %w", cfg.Target, err)
	}
	c, err := NewClient(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	c.closeConn = conn.Close
	return c, nil
}

// NewClient builds a Client on an existing connection (tests use bufconn).
func NewClient(conn grpc.ClientConnInterface, cfg Config) (*Client, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tokens := cfg.TokenSource
	if tokens == nil {
		tokens = ForwardedTokenSource{}
	}
	transport := a2agrpc.NewGRPCTransportFromClient(a2apb.NewA2AServiceClient(conn))
	// NewFromEndpoints does no I/O: the factory below hands back the transport
	// on the shared connection, so the only error source is a configuration
	// mismatch between the endpoint and the registered transport.
	a2aClient, err := a2aclient.NewFromEndpoints(context.Background(),
		[]*a2apkg.AgentInterface{{URL: cfg.Target, ProtocolBinding: a2apkg.TransportProtocolGRPC, ProtocolVersion: a2apkg.Version}},
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithTransport(a2apkg.TransportProtocolGRPC, a2aclient.TransportFactoryFn(
			func(context.Context, *a2apkg.AgentCard, *a2apkg.AgentInterface) (a2aclient.Transport, error) {
				return transport, nil
			})),
	)
	if err != nil {
		return nil, fmt.Errorf("a2a: build A2A client: %w", err)
	}
	return &Client{
		a2a:          a2aClient,
		templates:    apiv1alpha1.NewAgentTemplateServiceClient(conn),
		instances:    apiv1alpha1.NewAgentInstanceServiceClient(conn),
		models:       apiv1alpha1.NewModelServiceClient(conn),
		namespace:    cfg.Namespace,
		tokens:       tokens,
		iconTemplate: cfg.FallbackIconURLTemplate,
		logger:       logger,
		closeConn:    func() error { return nil },
	}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	return c.closeConn()
}

// Namespace is the namespace whose AgentTemplates the client serves.
func (c *Client) Namespace() string { return c.namespace }

func transportCredentials(useTLS bool, hostPort, caFile string) (credentials.TransportCredentials, error) {
	if !useTLS {
		return insecure.NewCredentials(), nil
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // G304: the CA bundle path is operator configuration (a chart value), not user input
		if err != nil {
			return nil, fmt.Errorf("a2a: read CA bundle: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("a2a: CA bundle %s holds no certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	_ = hostPort // the gRPC client derives ServerName from the dialled authority
	return credentials.NewTLS(tlsCfg), nil
}

// bearer returns the caller's token from ctx, or ErrNoIdentity.
func (c *Client) bearer(ctx context.Context) (string, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrNoIdentity
	}
	return token, nil
}

// serviceCtx attaches the caller's bearer to a kagent service call.
func (c *Client) serviceCtx(ctx context.Context) (context.Context, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), nil
}

// a2aCtx attaches the A2A service parameters of a call on instanceID: the
// caller's bearer, the instance route, and the HITL extension request. The
// gRPC transport carries them as metadata (keys lower-cased), so the
// extension request lands as `a2a-extensions`.
func (c *Client) a2aCtx(ctx context.Context, instanceID string) (context.Context, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	return a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{
		"authorization":           {"Bearer " + token},
		InstanceIDHeader:          {instanceID},
		a2apkg.SvcParamExtensions: {HITLExtensionURI},
	}), nil
}

// Stream sends msg to the AgentInstance and yields the task's events. A
// message without a TaskID starts a new task; one carrying the id of a paused
// task resumes it. The message's ContextID stays empty: the controller owns
// the conversation's context id and rejects any other value.
func (c *Client) Stream(ctx context.Context, instanceID string, msg *a2apkg.Message) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		callCtx, err := c.a2aCtx(ctx, instanceID)
		if err != nil {
			yield(nil, err)
			return
		}
		c.refreshRosterInBackground(ctx)
		for event, err := range c.a2a.SendStreamingMessage(callCtx, &a2apkg.SendMessageRequest{Message: msg}) {
			if err != nil {
				yield(nil, mapA2AError(err))
				return
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

// GetTask returns a task of the AgentInstance.
func (c *Client) GetTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	callCtx, err := c.a2aCtx(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	task, err := c.a2a.GetTask(callCtx, &a2apkg.GetTaskRequest{ID: taskID})
	if err != nil {
		return nil, mapA2AError(err)
	}
	return task, nil
}

// CancelTask cancels a running task server-side and returns its final state.
func (c *Client) CancelTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	callCtx, err := c.a2aCtx(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	task, err := c.a2a.CancelTask(callCtx, &a2apkg.CancelTaskRequest{ID: taskID})
	if err != nil {
		return nil, mapA2AError(err)
	}
	return task, nil
}

// mapA2AError translates the controller's refusals into the sentinels
// channels act on; everything else passes through.
func mapA2AError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, a2apkg.ErrUnsupportedOperation) && strings.Contains(err.Error(), "already has an active task"):
		return fmt.Errorf("%w: %s", ErrInstanceBusy, err.Error())
	case status.Code(err) == codes.ResourceExhausted:
		return fmt.Errorf("%w: %s", ErrPayloadTooLarge, err.Error())
	default:
		return err
	}
}

// rosterCache is the last fetched AgentTemplate roster. Discovery runs as the
// person behind a call, but branding a reply and recovering a thread's agent
// after a restart happen where no person's token is at hand, so those callers
// are served from the cache, which every authenticated call refreshes once it
// is older than rosterTTL.
type rosterCache struct {
	mu         sync.Mutex
	agents     []AgentInfo
	fetchedAt  time.Time
	refreshing bool
}

// rosterTTL is how long a fetched roster is considered current.
const rosterTTL = 30 * time.Second

// rosterRefreshTimeout bounds a background roster refresh.
const rosterRefreshTimeout = 10 * time.Second

func (c *Client) cachedRoster() ([]AgentInfo, bool, bool) {
	c.roster.mu.Lock()
	defer c.roster.mu.Unlock()
	if c.roster.agents == nil {
		return nil, false, false
	}
	return c.roster.agents, true, time.Since(c.roster.fetchedAt) < rosterTTL
}

func (c *Client) storeRoster(agents []AgentInfo) {
	c.roster.mu.Lock()
	defer c.roster.mu.Unlock()
	c.roster.agents = agents
	c.roster.fetchedAt = time.Now()
}

// refreshRosterInBackground refreshes a stale roster under the caller's token
// without blocking the call that carried it. One refresh runs at a time. The
// refresh keeps the call's identity but not its cancellation: a turn that ends
// early must not leave the roster stale.
func (c *Client) refreshRosterInBackground(ctx context.Context) {
	if _, cached, fresh := c.cachedRoster(); cached && fresh {
		return
	}
	if _, err := c.bearer(ctx); err != nil {
		return
	}
	c.roster.mu.Lock()
	if c.roster.refreshing {
		c.roster.mu.Unlock()
		return
	}
	c.roster.refreshing = true
	c.roster.mu.Unlock()
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rosterRefreshTimeout)
	go func() {
		defer cancel()
		defer func() {
			c.roster.mu.Lock()
			c.roster.refreshing = false
			c.roster.mu.Unlock()
		}()
		if _, err := c.fetchTemplates(bctx); err != nil {
			c.logger.Warn("a2a: background roster refresh failed", "error", err)
		}
	}()
}
