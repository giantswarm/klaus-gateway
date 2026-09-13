package a2a

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// Instance is the slice of a kagent AgentInstance the gateway acts on.
type Instance struct {
	// ID is the controller-assigned UUID every A2A call on the conversation is
	// routed with.
	ID string
	// State is the controller's lifecycle state (READY, SUSPENDED, ...).
	State string
	// Failure explains a FAILED instance.
	Failure string
}

// Ready reports whether the instance can take a turn. A conversation gives its
// worker back between turns, so SUSPENDED is as good as READY: the next send
// resumes it.
func (i Instance) Ready() bool {
	return i.State == apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY.String() ||
		i.State == apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED.String()
}

// instanceReadyTimeout bounds the wait for a freshly created instance to reach
// READY: the controller converges it synchronously, so the poll only covers a
// create that was interrupted and retried.
const instanceReadyTimeout = 90 * time.Second

// instancePollInterval is the GetAgentInstance cadence while waiting for READY.
const instancePollInterval = 2 * time.Second

// CreateInstance creates the AgentInstance for a conversation with the agent
// agentRef names: the template and the Harness admitting it, keyed by
// requestID. The create is idempotent per (caller, requestID): a retried first
// turn returns the instance the first attempt created instead of a second one.
// A template that is not selectable is refused with ErrAgentUnavailable and the
// reason. The call returns once the instance is READY.
func (c *Client) CreateInstance(ctx context.Context, agentRef, requestID string) (Instance, error) {
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return Instance{}, err
	}
	if info.Unavailable != "" {
		return Instance{}, fmt.Errorf("%w: %s: %s", ErrAgentUnavailable, agentRef, info.Unavailable)
	}
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return Instance{}, err
	}
	resp, err := c.instances.CreateAgentInstance(callCtx, &apiv1alpha1.CreateAgentInstanceRequest{
		Harness:       &apiv1alpha1.ResourceReference{Namespace: info.Namespace, Name: info.Harness},
		AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: info.Namespace, Name: info.Name},
		RequestId:     requestID,
	})
	if err != nil {
		switch status.Code(err) {
		case codes.FailedPrecondition:
			return Instance{}, fmt.Errorf("%w: %s: %s", ErrAgentUnavailable, agentRef, status.Convert(err).Message())
		case codes.AlreadyExists:
			return Instance{}, fmt.Errorf("a2a: create AgentInstance for %s: request id %s was already used for a different conversation: %w", agentRef, requestID, err)
		}
		return Instance{}, fmt.Errorf("a2a: create AgentInstance for %s: %w", agentRef, err)
	}
	c.refreshRosterInBackground(ctx)
	return c.awaitReady(ctx, fromProtoInstance(resp.GetAgentInstance()))
}

// awaitReady polls the instance until it is READY, FAILED, or the deadline
// passes. The common case returns immediately.
func (c *Client) awaitReady(ctx context.Context, inst Instance) (Instance, error) {
	deadline := time.Now().Add(instanceReadyTimeout)
	for {
		switch inst.State {
		case apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY.String(), apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED.String():
			return inst, nil
		case apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_FAILED.String():
			return inst, fmt.Errorf("a2a: AgentInstance %s failed: %s", inst.ID, inst.Failure)
		case apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETING.String(), apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETED.String():
			return inst, fmt.Errorf("%w: %s is %s", ErrInstanceNotFound, inst.ID, inst.State)
		}
		if time.Now().After(deadline) {
			return inst, fmt.Errorf("a2a: AgentInstance %s is still %s after %s", inst.ID, inst.State, instanceReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return inst, ctx.Err()
		case <-time.After(instancePollInterval):
		}
		next, err := c.GetInstance(ctx, inst.ID)
		if err != nil {
			return inst, err
		}
		inst = next
	}
}

// GetInstance returns the instance, or ErrInstanceNotFound when the controller
// no longer knows it (deleted, or created by someone else).
func (c *Client) GetInstance(ctx context.Context, id string) (Instance, error) {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return Instance{}, err
	}
	resp, err := c.instances.GetAgentInstance(callCtx, &apiv1alpha1.GetAgentInstanceRequest{AgentInstanceId: id})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Instance{}, fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
		}
		return Instance{}, fmt.Errorf("a2a: get AgentInstance %s: %w", id, err)
	}
	return fromProtoInstance(resp.GetAgentInstance()), nil
}

// DeleteInstance removes the instance so the conversation can start over. A
// missing instance is success: it is gone either way.
func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return err
	}
	_, err = c.instances.DeleteAgentInstance(callCtx, &apiv1alpha1.DeleteAgentInstanceRequest{AgentInstanceId: id})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("a2a: delete AgentInstance %s: %w", id, err)
	}
	return nil
}

// IsNotFound reports whether err says the instance is gone.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrInstanceNotFound)
}

func fromProtoInstance(pb *apiv1alpha1.AgentInstance) Instance {
	inst := Instance{ID: pb.GetId(), State: pb.GetState().String()}
	if f := pb.GetFailure(); f != nil {
		inst.Failure = f.GetMessage()
		if inst.Failure == "" {
			inst.Failure = f.GetReason()
		}
	}
	return inst
}
