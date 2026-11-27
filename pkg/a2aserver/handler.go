package a2aserver

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// Agents is the platform side of the channel: the Sessions and turns of the
// agents the caller may use, as the caller.
type Agents interface {
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
	StartSession(ctx context.Context, agentRef, requestID, name string, grant workspaces.Grant) (pkga2a.Session, error)
	SendMessage(ctx context.Context, sessionID string, message *a2a.Message, configuration *a2a.SendMessageConfig) (a2a.SendMessageResult, error)
	StreamWithConfig(ctx context.Context, sessionID string, message *a2a.Message, configuration *a2a.SendMessageConfig) iter.Seq2[a2a.Event, error]
	Subscribe(ctx context.Context, taskID a2a.TaskID) iter.Seq2[a2a.Event, error]
	GetTaskWithHistory(ctx context.Context, taskID a2a.TaskID, historyLength *int) (*a2a.Task, error)
	CancelTask(ctx context.Context, taskID a2a.TaskID) (*a2a.Task, error)
	// AgentCard is the Agent Card of the agent ctx names (pkga2a.WithAgentRef).
	AgentCard(ctx context.Context) (*a2a.AgentCard, error)
}

// Handler serves the A2A protocol methods of the channel. Push notification
// configs and task listing are not offered.
type Handler struct {
	Agents Agents
	// Card supplies the public endpoint and security schemes for per-agent cards.
	Card *a2a.AgentCard
	// Organizations names the organization of an agent's namespace. An
	// A2A client addresses an agent as <organization>/<agent>; agents
	// outside an organization, or of an organization whose name is not
	// unique, are not offered.
	Organizations pkga2a.OrganizationNames
	Logger        *slog.Logger

	cards cardCache
}

var _ a2asrv.RequestHandler = (*Handler)(nil)

// errAgentNotOffered refuses a tenant that names no agent the caller may use.
var errAgentNotOffered = fmt.Errorf("%w: the agent is not available to this caller", a2a.ErrUnauthorized)

// tenant is the tenant an A2A client addresses agent by,
// <organization>/<agent>; ok is false for an agent not offered to
// A2A clients.
func (h *Handler) tenant(agent pkga2a.AgentInfo) (string, bool) {
	if h.Organizations == nil || agent.Unavailable != "" {
		return "", false
	}
	org, _, ok := h.Organizations.Organization(agent.Namespace)
	if !ok || h.Organizations.Shared(org) {
		return "", false
	}
	return org + "/" + agent.Name, true
}

// agentContext returns ctx naming the agent tenant addresses, and the agent's
// reference, resolved among the agents the caller may use.
func (h *Handler) agentContext(ctx context.Context, tenant string) (context.Context, string, error) {
	org, name, ok := strings.Cut(tenant, "/")
	if !ok || org == "" || name == "" || strings.Contains(name, "/") {
		return nil, "", fmt.Errorf("%w: tenant must name the agent as organization/agent", a2a.ErrInvalidParams)
	}
	agents, err := h.Agents.ListAgents(ctx)
	if err != nil {
		return nil, "", h.refusal(ctx, "list agents", err)
	}
	for _, agent := range agents {
		if t, ok := h.tenant(agent); ok && t == tenant {
			ref := agent.Namespace + "/" + agent.Name
			return pkga2a.WithAgentRef(ctx, ref), ref, nil
		}
	}
	return nil, "", errAgentNotOffered
}

// SendStreamingMessage starts a Session for a message without a context id
// and streams the turn's events.
func (h *Handler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		ctx, sessionID, err := h.sendContext(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}
		for event, err := range h.Agents.StreamWithConfig(ctx, sessionID, req.Message, req.Config) {
			if err != nil {
				yield(nil, h.refusal(ctx, "stream", err))
				return
			}
			if task, ok := event.(*a2a.Task); ok {
				event = limitHistory(task, sendHistoryLength(req))
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

// SendMessage preserves the caller's blocking and response configuration.
func (h *Handler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	ctx, sessionID, err := h.sendContext(ctx, req)
	if err != nil {
		return nil, err
	}
	result, err := h.Agents.SendMessage(ctx, sessionID, req.Message, req.Config)
	if err != nil {
		return nil, h.refusal(ctx, "send message", err)
	}
	if task, ok := result.(*a2a.Task); ok {
		return limitHistory(task, sendHistoryLength(req)), nil
	}
	return result, nil
}

func (h *Handler) sendContext(ctx context.Context, request *a2a.SendMessageRequest) (context.Context, string, error) {
	if request.Message == nil {
		return nil, "", fmt.Errorf("%w: message is required", a2a.ErrInvalidParams)
	}
	if historyLength := sendHistoryLength(request); historyLength != nil && *historyLength < 0 {
		return nil, "", fmt.Errorf("%w: historyLength must be nonnegative", a2a.ErrInvalidParams)
	}
	ctx, ref, err := h.agentContext(ctx, request.Tenant)
	if err != nil {
		return nil, "", err
	}
	if request.Message.ContextID != "" || request.Message.TaskID != "" {
		return ctx, request.Message.ContextID, nil
	}
	session, err := h.Agents.StartSession(ctx, ref, request.Message.ID, "", workspaces.Grant{})
	if err != nil {
		return nil, "", h.refusal(ctx, "start session", err)
	}
	return ctx, session.ID, nil
}

func sendHistoryLength(request *a2a.SendMessageRequest) *int {
	if request.Config == nil {
		return nil
	}
	return request.Config.HistoryLength
}

// limitHistory never mutates a downstream task or its backing history slice.
func limitHistory(task *a2a.Task, historyLength *int) *a2a.Task {
	if task == nil || historyLength == nil || len(task.History) <= *historyLength {
		return task
	}
	limited := *task
	limited.History = task.History[len(task.History)-*historyLength:]
	return &limited
}

// GetTask answers a task of the agent.
func (h *Handler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	if req.HistoryLength != nil && *req.HistoryLength < 0 {
		return nil, fmt.Errorf("%w: historyLength must be nonnegative", a2a.ErrInvalidParams)
	}
	ctx, _, err := h.agentContext(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	task, err := h.Agents.GetTaskWithHistory(ctx, req.ID, req.HistoryLength)
	if err != nil {
		return nil, h.refusal(ctx, "get task", err)
	}
	return limitHistory(task, req.HistoryLength), nil
}

// CancelTask cancels a task of the agent.
func (h *Handler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	ctx, _, err := h.agentContext(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	task, err := h.Agents.CancelTask(ctx, req.ID)
	if err != nil {
		return nil, h.refusal(ctx, "cancel task", err)
	}
	return task, nil
}

// SubscribeToTask streams the events of a running task.
func (h *Handler) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		ctx, _, err := h.agentContext(ctx, req.Tenant)
		if err != nil {
			yield(nil, err)
			return
		}
		for event, err := range h.Agents.Subscribe(ctx, req.ID) {
			if err != nil {
				yield(nil, h.refusal(ctx, "subscribe", err))
				return
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

// GetExtendedAgentCard answers the card of the agent the request names.
func (h *Handler) GetExtendedAgentCard(ctx context.Context, request *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	if request == nil || request.Tenant == "" {
		return nil, a2a.ErrExtendedCardNotConfigured
	}
	return h.agentCard(ctx, request.Tenant)
}

// ListTasks is not offered.
func (h *Handler) ListTasks(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return nil, a2a.ErrUnsupportedOperation
}

// GetTaskPushConfig is not offered.
func (h *Handler) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

// ListTaskPushConfigs is not offered.
func (h *Handler) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

// CreateTaskPushConfig is not offered.
func (h *Handler) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, a2a.ErrPushNotificationNotSupported
}

// DeleteTaskPushConfig is not offered.
func (h *Handler) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return a2a.ErrPushNotificationNotSupported
}

// refusal maps a platform error to an A2A error that names no component
// behind the gateway; the cause is logged.
func (h *Handler) refusal(ctx context.Context, op string, err error) error {
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "a2a server: request refused", "op", op, "agent", pkga2a.AgentRefFromContext(ctx), "error", err)
	var connect *pkga2a.ConnectRequiredError
	var refused *pkga2a.TokenRefusedError
	switch {
	case errors.As(err, &connect) && connect.SignIn != nil:
		return actionRequired(connect.SignIn)
	case errors.As(err, &refused):
		return a2a.NewError(a2a.ErrUnauthenticated, "the platform could not authenticate the forwarded token").
			WithErrorInfoMeta(map[string]string{"reason": refused.Reason})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, pkga2a.ErrNoIdentity):
		return a2a.ErrUnauthenticated
	case errors.Is(err, pkga2a.ErrAgentNotPermitted), errors.Is(err, pkga2a.ErrAgentUnavailable):
		return fmt.Errorf("%w: the agent is not available to this caller", a2a.ErrUnauthorized)
	case pkga2a.IsNotFound(err), errors.Is(err, a2a.ErrTaskNotFound):
		return a2a.ErrTaskNotFound
	case errors.Is(err, pkga2a.ErrSessionBusy):
		return fmt.Errorf("%w: the conversation already has a running task", a2a.ErrUnsupportedOperation)
	}
	for _, protocolError := range []error{
		a2a.ErrParseError, a2a.ErrInvalidRequest, a2a.ErrMethodNotFound, a2a.ErrInvalidParams,
		a2a.ErrInternalError, a2a.ErrServerError, a2a.ErrTaskNotCancelable,
		a2a.ErrPushNotificationNotSupported, a2a.ErrUnsupportedOperation, a2a.ErrUnsupportedContentType,
		a2a.ErrInvalidAgentResponse, a2a.ErrExtendedCardNotConfigured, a2a.ErrExtensionSupportRequired,
		a2a.ErrVersionNotSupported, a2a.ErrUnauthenticated, a2a.ErrUnauthorized,
	} {
		if errors.Is(err, protocolError) {
			return protocolError
		}
	}
	return fmt.Errorf("%w: the request could not be completed", a2a.ErrInternalError)
}

// actionRequired asks the caller to act at signIn's URL, a sign-in or an
// approval muster asks for before the agent can start, then to send the same
// message again: agent-manager starts a Session once per message id. The URL
// and message travel as metadata of the error's ErrorInfo, named as in an
// MCP URL elicitation.
func actionRequired(signIn *muster.SignIn) error {
	return a2a.NewError(a2a.ErrUnauthorized, "the caller must complete the step at the URL, then send the same message again").
		WithErrorInfoMeta(map[string]string{"url": signIn.URL, "message": signIn.Message})
}
