// Package a2a is klaus-gateway's client for a kagent API v2 controller: the
// A2A v1 protocol over gRPC for the agent turns, and kagent's own gRPC
// services (kagent.api.v1alpha1) for the Agent roster, the Session that holds
// a conversation, and the model behind an agent.
//
// Every call is made as the person behind the channel turn: the caller's Dex
// id_token rides as the `authorization` metadata entry. An A2A call names the
// Agent it is for as the request's tenant (`namespace/name`, the agent ref
// WithAgentRef put on the context) and the conversation as the message's
// context id, the Session's. The gateway never presents its own machine
// identity to the controller.
package a2a

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// ShareTokenHeader is the gRPC metadata entry that presents a Session share
// token. The controller authorizes a call carrying it on the shared session
// while the caller stays authenticated as themselves.
const ShareTokenHeader = "x-share-token"

// maxMessageBytes is the largest message the client receives: the
// controller's own gRPC limit (MaxMessageBytes, 16 MiB), so any event the
// controller can send arrives. gRPC's default is 4 MiB.
const maxMessageBytes = 16 << 20

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
	// its request exceeds the message size it accepts (gRPC's size limit, on the
	// stream's first read). Channels match it with errors.Is to render an
	// actionable "too large" notice instead of the generic turn-failed message.
	ErrPayloadTooLarge = errors.New("a2a: request payload too large")

	// ErrSessionBusy is returned when the controller refuses a new task because
	// the Session already has an active one. One task runs per session;
	// channels map it to their "still working" notice.
	ErrSessionBusy = errors.New("a2a: the conversation's agent is still working on a previous message")

	// ErrSessionNotFound is returned when the Session a thread is bound to no
	// longer exists at the controller.
	ErrSessionNotFound = errors.New("a2a: session not found")

	// ErrAgentUnknown is returned when an agent ref names no Agent in the
	// served namespace.
	ErrAgentUnknown = errors.New("a2a: unknown agent")

	// ErrInvalidSessionName is returned by a SessionStarter whose start was
	// refused as an invalid request: the display name is the only free text
	// of a start, so StartSession goes again unnamed.
	ErrInvalidSessionName = errors.New("a2a: session start refused as invalid")

	// ErrNoStarter is returned by StartSession on a Client configured without
	// a SessionStarter: the gateway starts a Session only through
	// agent-manager.
	ErrNoStarter = errors.New("a2a: no session starter configured (agent-manager's start_session through muster)")

	// ErrAgentUnavailable is returned when an Agent exists but cannot be
	// selected: its Ready condition is not True, so it has no revision a
	// Session can be created from. The error message carries the reason.
	ErrAgentUnavailable = errors.New("a2a: agent unavailable")

	// ErrAgentNotPermitted is returned when the caller may not use an Agent:
	// their roster does not list it, or the controller refused the call with
	// PermissionDenied. The two are one answer, since the controller lists
	// only the Agents a caller may use.
	ErrAgentNotPermitted = errors.New("a2a: agent not permitted for the caller")
)

// AgentNotPermittedError names the Agent and its tenant a caller may not use.
// It matches ErrAgentNotPermitted in errors.Is.
type AgentNotPermittedError struct {
	// Ref is the agent ref as selected.
	Ref string
	// Name is the Agent's name, and DisplayName its display name when the
	// gateway has seen one.
	Name, DisplayName string
	// Tenant is the tenant of the Agent's namespace (Tenant).
	Tenant string
	// Cause is the controller's or agent-manager's refusal, nil for an Agent
	// the caller's roster does not list.
	Cause error
}

func (e *AgentNotPermittedError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", ErrAgentNotPermitted.Error(), e.Ref, e.Cause)
	}
	return fmt.Sprintf("%s: %s is not in the caller's roster", ErrAgentNotPermitted.Error(), e.Ref)
}

func (e *AgentNotPermittedError) Is(target error) bool { return target == ErrAgentNotPermitted }

func (e *AgentNotPermittedError) Unwrap() error { return e.Cause }

// PlatformTenant is the tenant of the served namespace: the platform's own
// agents, which every signed-in person may use.
const PlatformTenant = "platform"

// organizationPrefix is the prefix of a Giant Swarm Organization's namespace.
const organizationPrefix = "org-"

// AgentUnavailableError says why an Agent that exists cannot start a
// conversation: its Ready condition is not True. It matches
// ErrAgentUnavailable in errors.Is, so a channel can render the reason instead
// of parsing the message.
type AgentUnavailableError struct {
	// Ref is the agent ref as selected.
	Ref string
	// Reason is the sentence the roster readiness check or the controller gave.
	Reason string
}

func (e *AgentUnavailableError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrAgentUnavailable.Error(), e.Ref, e.Reason)
}

func (e *AgentUnavailableError) Is(target error) bool { return target == ErrAgentUnavailable }

// ConnectRequiredError is muster's sign-in challenge on the way to the
// controller: the caller is signed in, but the muster server a call goes
// through (agent-manager, for a Session start) waits for their one-time
// consent, which LoginURL is where they give. Not ErrNoIdentity: a channel
// answers it with its Connect prompt for the server, never the sign-in
// prompt, and runs the turn again once they have connected.
type ConnectRequiredError struct {
	// Server is the muster server awaiting the consent.
	Server string
	// LoginURL is muster's sign-in link for it.
	LoginURL string
}

func (e *ConnectRequiredError) Error() string {
	return fmt.Sprintf("a2a: muster server %s waits for the caller's sign-in", e.Server)
}

// Config configures a Client.
type Config struct {
	// Target is the controller's gRPC endpoint, reached through agentgateway:
	// grpc://host:port (plaintext h2c) or grpcs://host:port (TLS).
	Target string
	// CAFile optionally names a PEM bundle trusted for a grpcs target in
	// addition to the system roots.
	CAFile string
	// Namespace is the platform tenant's namespace; a bare agent ref is
	// resolved in it. The roster spans every namespace the controller lets the
	// caller list.
	Namespace string
	// FallbackIconURLTemplate supplies an agent icon when neither the Agent
	// nor its AgentTemplate carries an icon-URL annotation. "{agent}" is replaced with the agent's
	// technical name. Empty leaves the icon empty.
	FallbackIconURLTemplate string
	// Starter starts a thread's Session: agent-manager's start_session,
	// called through muster as the person. Without one StartSession refuses
	// with ErrNoStarter.
	Starter SessionStarter
	Logger  *slog.Logger
}

// SessionStarter starts the Session of a conversation as the person bearer
// belongs to, with the Agent namespace/name, keyed by requestID (a retried
// start answers the Session the first one created) and named sessionName
// (empty: unnamed), in the workspace grant admits it to (zero: none). It
// returns the Session as created, ready or not. Its refusals are the errors a
// channel renders: AgentUnavailableError, ErrNoIdentity, ErrAgentUnknown,
// ErrInvalidSessionName, and ErrAgentNotPermitted for a caller the controller
// refuses the Agent to. *agentmanager.Starter satisfies it.
type SessionStarter interface {
	StartSession(ctx context.Context, bearer, namespace, name, requestID, sessionName string, grant workspaces.Grant) (Session, error)
}

// Client speaks to one kagent controller. It is safe for concurrent use.
type Client struct {
	a2a       *a2aclient.Client
	agents    apiv1alpha1.AgentServiceClient
	templates apiv1alpha1.AgentTemplateServiceClient
	sessions  apiv1alpha1.SessionServiceClient
	models    apiv1alpha1.ModelServiceClient
	starter   SessionStarter

	namespace    string
	iconTemplate string
	logger       *slog.Logger
	closeConn    func() error

	looks agentLooks
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
	if u.Host == "" || u.Path != "" || u.RawQuery != "" {
		return "", false, fmt.Errorf("a2a: target %q must be %s://host[:port] with no path", target, u.Scheme)
	}
	if u.Port() != "" {
		return u.Host, useTLS, nil
	}
	// A TLS target without a port is the edge on 443, the way an https URL is;
	// plaintext gRPC has no conventional port, so it must name one.
	if !useTLS {
		return "", false, fmt.Errorf("a2a: target %q must name a port", target)
	}
	return net.JoinHostPort(u.Hostname(), "443"), useTLS, nil
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
	// Every call is a client span under the caller's (the channel turn's)
	// span, and carries its trace context to the controller as traceparent,
	// so the controller's own SendStreamingMessage trace continues the
	// gateway's instead of starting a new one.
	conn, err := grpc.NewClient(hostPort, grpc.WithTransportCredentials(creds), grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageBytes)))
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
		agents:       apiv1alpha1.NewAgentServiceClient(conn),
		templates:    apiv1alpha1.NewAgentTemplateServiceClient(conn),
		sessions:     apiv1alpha1.NewSessionServiceClient(conn),
		models:       apiv1alpha1.NewModelServiceClient(conn),
		starter:      cfg.Starter,
		namespace:    cfg.Namespace,
		iconTemplate: cfg.FallbackIconURLTemplate,
		logger:       logger,
		closeConn:    func() error { return nil },
	}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	return c.closeConn()
}

// Namespace is the platform tenant's namespace, the one a bare ref names.
func (c *Client) Namespace() string { return c.namespace }

// Tenant is the tenant an Agent of namespace belongs to: PlatformTenant for
// the served namespace, the organization's name for an org-<name> namespace,
// the namespace itself otherwise.
func (c *Client) Tenant(namespace string) string {
	if namespace == c.namespace {
		return PlatformTenant
	}
	if org, ok := strings.CutPrefix(namespace, organizationPrefix); ok && org != "" {
		return "organization " + org
	}
	return "namespace " + namespace
}

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

// bearer returns the caller's forwarded token from ctx, or ErrNoIdentity: the
// controller is spoken to as the person behind the call only, never as the
// gateway's own identity.
func (c *Client) bearer(ctx context.Context) (string, error) {
	token := ForwardedTokenFromContext(ctx)
	if token == "" {
		return "", ErrNoIdentity
	}
	return token, nil
}

// serviceCtx attaches the caller's bearer, and the share token ctx carries,
// to a kagent service call.
func (c *Client) serviceCtx(ctx context.Context) (context.Context, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	kv := []string{"authorization", "Bearer " + token}
	if share := ShareTokenFromContext(ctx); share != "" {
		kv = append(kv, ShareTokenHeader, share)
	}
	return metadata.AppendToOutgoingContext(ctx, kv...), nil
}

// a2aCtx attaches the A2A service parameters of a call: the caller's bearer,
// the share token ctx carries, and the HITL extension request. The gRPC
// transport carries them as metadata (keys lower-cased), so the extension
// request lands as `a2a-extensions`.
func (c *Client) a2aCtx(ctx context.Context) (context.Context, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	params := a2aclient.ServiceParams{
		"authorization":           {"Bearer " + token},
		a2apkg.SvcParamExtensions: {HITLExtensionURI},
	}
	if share := ShareTokenFromContext(ctx); share != "" {
		params[ShareTokenHeader] = []string{share}
	}
	return a2aclient.AttachServiceParams(ctx, params), nil
}

// tenant is the A2A tenant of the agent ref ctx carries (WithAgentRef):
// `namespace/name` of the Agent, which the controller routes the call by.
func (c *Client) tenant(ctx context.Context) (string, error) {
	namespace, name, err := c.splitRef(AgentRefFromContext(ctx))
	if err != nil {
		return "", err
	}
	return namespace + "/" + name, nil
}

// Stream sends msg to the Session and yields the task's events. A message
// without a TaskID starts a new task; one carrying the id of a paused task
// resumes it. The message's ContextID is set to the session's: the controller
// owns the conversation's context id, equal to the session id, and rejects any
// other value. A session the controller reports SUSPENDED is resumed once and
// the message sent again.
func (c *Client) Stream(ctx context.Context, sessionID string, msg *a2apkg.Message) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		callCtx, err := c.a2aCtx(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		tenant, err := c.tenant(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		msg.ContextID = sessionID
		resumed := false
		for {
			var refused error
			first := true
			for event, err := range c.a2a.SendStreamingMessage(callCtx, &a2apkg.SendMessageRequest{Tenant: tenant, Message: msg}) {
				if err != nil {
					// The controller refuses an oversize request on the stream's first
					// read. The same refusal later is an event too large for this
					// client, which is not the person's message.
					if first && isSizeRefusal(err) {
						err = fmt.Errorf("%w: %s", ErrPayloadTooLarge, err.Error())
					}
					refused = c.mapA2AError(ctx, err)
					break
				}
				first = false
				if !yield(event, nil) {
					return
				}
			}
			if refused == nil {
				return
			}
			if resumed || !c.resumeIfSuspended(ctx, sessionID, refused) {
				yield(nil, refused)
				return
			}
			resumed = true
		}
	}
}

// resumeIfSuspended reports whether refusal is the controller declining a
// send on a SUSPENDED session, after resuming it. Only the protocol's
// unsupported-operation refusal is looked into; the busy refusal has its own
// sentinel and any other reason is the caller's to report.
func (c *Client) resumeIfSuspended(ctx context.Context, sessionID string, refusal error) bool {
	if errors.Is(refusal, ErrSessionBusy) || !errors.Is(refusal, a2apkg.ErrUnsupportedOperation) {
		return false
	}
	session, err := c.GetSession(ctx, sessionID)
	if err != nil || session.State != apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED.String() {
		return false
	}
	c.logger.Info("a2a: resuming the suspended session before the turn", "session", sessionID)
	if _, err := c.ResumeSession(ctx, sessionID); err != nil {
		c.logger.Warn("a2a: resume the suspended session failed", "session", sessionID, "error", err)
		return false
	}
	return true
}

// Subscribe attaches to a task the Session is already running and yields its
// events from here on: a task that has since quiesced arrives whole, as its
// one and only event. This is how a restarted gateway picks up the turns its
// predecessor left running.
func (c *Client) Subscribe(ctx context.Context, taskID a2apkg.TaskID) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		callCtx, err := c.a2aCtx(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		tenant, err := c.tenant(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		for event, err := range c.a2a.SubscribeToTask(callCtx, &a2apkg.SubscribeToTaskRequest{Tenant: tenant, ID: taskID}) {
			if err != nil {
				yield(nil, c.mapA2AError(ctx, err))
				return
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

// GetTask returns a task of the agent's, found by its id.
func (c *Client) GetTask(ctx context.Context, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	callCtx, err := c.a2aCtx(ctx)
	if err != nil {
		return nil, err
	}
	tenant, err := c.tenant(ctx)
	if err != nil {
		return nil, err
	}
	task, err := c.a2a.GetTask(callCtx, &a2apkg.GetTaskRequest{Tenant: tenant, ID: taskID})
	if err != nil {
		return nil, c.mapA2AError(ctx, err)
	}
	return task, nil
}

// CancelTask cancels a running task server-side and returns its final state.
func (c *Client) CancelTask(ctx context.Context, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	callCtx, err := c.a2aCtx(ctx)
	if err != nil {
		return nil, err
	}
	tenant, err := c.tenant(ctx)
	if err != nil {
		return nil, err
	}
	task, err := c.a2a.CancelTask(callCtx, &a2apkg.CancelTaskRequest{Tenant: tenant, ID: taskID})
	if err != nil {
		return nil, c.mapA2AError(ctx, err)
	}
	return task, nil
}

// mapA2AError translates the controller's refusals into the sentinels
// channels act on; everything else passes through. A PermissionDenied is the
// caller's lack of access to the Agent ctx names (WithAgentRef): the
// controller reviews every turn's sender against the Agent.
func (c *Client) mapA2AError(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, a2apkg.ErrUnsupportedOperation) && strings.Contains(err.Error(), "already has an active task"):
		return fmt.Errorf("%w: %s", ErrSessionBusy, err.Error())
	case errors.Is(err, a2apkg.ErrUnauthorized):
		return c.notPermitted(AgentRefFromContext(ctx), err)
	default:
		return err
	}
}

// isSizeRefusal reports whether err is gRPC's receive-side message-size limit.
// a2a-go turns a gRPC status it has no A2A error for, ResourceExhausted among
// them, into ErrInternalError with the status message alone, so the limit is
// known by gRPC's text. A quota or rate-limit refusal carries the same code and
// is not about size, and the controller failing to send its own first event (a
// resumed task's snapshot over its send limit, "trying to send message larger
// than max") is not the person's message either. A first event over this
// client's own receive limit would match too; the controller's send limit is
// the same 16 MiB, so it cannot send one.
func isSizeRefusal(err error) bool {
	msg := err.Error()
	return errors.Is(err, a2apkg.ErrInternalError) &&
		(strings.Contains(msg, "grpc: received message") || strings.Contains(msg, "grpc: message after decompression"))
}
