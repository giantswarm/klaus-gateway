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
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// Agents is the platform side of the channel: the Sessions and turns of the
// agents the caller may use, as the caller.
type Agents interface {
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
	StartSession(ctx context.Context, agentRef, requestID, name string, grant workspaces.Grant) (pkga2a.Session, error)
	Stream(ctx context.Context, sessionID string, msg *a2a.Message) iter.Seq2[a2a.Event, error]
	Subscribe(ctx context.Context, taskID a2a.TaskID) iter.Seq2[a2a.Event, error]
	GetTask(ctx context.Context, taskID a2a.TaskID) (*a2a.Task, error)
	CancelTask(ctx context.Context, taskID a2a.TaskID) (*a2a.Task, error)
}

// Handler serves the A2A protocol methods of the channel. Push notification
// configs and task listing are not offered.
type Handler struct {
	Agents Agents
	// Card is the public Agent Card; the extended card adds one interface per
	// agent the caller may use.
	Card *a2a.AgentCard
	// Organizations names the organization of an agent's namespace. An
	// A2A client addresses an agent as <organization>/<agent>; agents
	// outside an organization, or of an organization whose name is not
	// unique, are not offered.
	Organizations pkga2a.OrganizationNames
	Logger        *slog.Logger
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
		if req.Message == nil {
			yield(nil, fmt.Errorf("%w: message is required", a2a.ErrInvalidParams))
			return
		}
		ctx, ref, err := h.agentContext(ctx, req.Tenant)
		if err != nil {
			yield(nil, err)
			return
		}
		sessionID := req.Message.ContextID
		if sessionID == "" {
			session, err := h.Agents.StartSession(ctx, ref, req.Message.ID, "", workspaces.Grant{})
			if err != nil {
				yield(nil, h.refusal(ctx, "start session", err))
				return
			}
			sessionID = session.ID
		}
		for event, err := range h.Agents.Stream(ctx, sessionID, req.Message) {
			if err != nil {
				yield(nil, h.refusal(ctx, "stream", err))
				return
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

// SendMessage runs the turn to its end and answers the task, or the message
// the agent answered without one.
func (h *Handler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	var taskID a2a.TaskID
	var last a2a.Event
	for event, err := range h.SendStreamingMessage(ctx, req) {
		if err != nil {
			return nil, err
		}
		last = event
		if id := event.TaskInfo().TaskID; id != "" {
			taskID = id
		}
	}
	switch result := last.(type) {
	case *a2a.Message:
		return result, nil
	case *a2a.Task:
		return result, nil
	}
	if taskID == "" {
		return nil, fmt.Errorf("%w: the turn ended without a task", a2a.ErrInvalidAgentResponse)
	}
	return h.GetTask(ctx, &a2a.GetTaskRequest{Tenant: req.Tenant, ID: taskID})
}

// GetTask answers a task of the agent.
func (h *Handler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	ctx, _, err := h.agentContext(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	task, err := h.Agents.GetTask(ctx, req.ID)
	if err != nil {
		return nil, h.refusal(ctx, "get task", err)
	}
	return task, nil
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

// GetExtendedAgentCard answers the public card with one JSON-RPC interface
// per agent the caller may use, its tenant naming the agent.
func (h *Handler) GetExtendedAgentCard(ctx context.Context, _ *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	if h.Card == nil || len(h.Card.SupportedInterfaces) == 0 {
		return nil, a2a.ErrExtendedCardNotConfigured
	}
	agents, err := h.Agents.ListAgents(ctx)
	if err != nil {
		return nil, h.refusal(ctx, "list agents", err)
	}
	card := *h.Card
	base := h.Card.SupportedInterfaces[0]
	card.SupportedInterfaces = make([]*a2a.AgentInterface, 0, len(agents))
	card.Skills = make([]a2a.AgentSkill, 0, len(agents))
	for _, agent := range agents {
		tenant, ok := h.tenant(agent)
		if !ok {
			continue
		}
		card.SupportedInterfaces = append(card.SupportedInterfaces, &a2a.AgentInterface{
			URL: base.URL, ProtocolBinding: base.ProtocolBinding, ProtocolVersion: base.ProtocolVersion, Tenant: tenant,
		})
		name := agent.DisplayName
		if name == "" {
			name = agent.Name
		}
		card.Skills = append(card.Skills, a2a.AgentSkill{ID: tenant, Name: name, Description: agent.Description, Tags: []string{"agent"}})
	}
	return &card, nil
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
	switch {
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
	case errors.Is(err, a2a.ErrInvalidParams):
		return err
	}
	return fmt.Errorf("%w: the request could not be completed", a2a.ErrInternalError)
}
