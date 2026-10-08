package a2a

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// Session is the slice of a kagent Session the gateway acts on.
type Session struct {
	// ID is the controller-assigned UUID. It is also the A2A context id every
	// message of the conversation carries.
	ID string
	// State is the controller's runtime state (READY, SUSPENDED, ...).
	State string
	// Failure explains a FAILED session.
	Failure string
}

// Ready reports whether the session can take a turn. A SUSPENDED session is
// as good as READY: Stream resumes it before the send.
func (s Session) Ready() bool {
	return s.State == apiv1alpha1.RuntimeState_RUNTIME_STATE_READY.String() ||
		s.State == apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED.String()
}

// sessionReadyTimeout bounds the wait for a freshly created or resumed
// session to reach READY: the controller converges it synchronously, so the
// poll only covers a create that was interrupted and retried.
const sessionReadyTimeout = 90 * time.Second

// sessionPollInterval is the GetSession cadence while waiting for READY.
const sessionPollInterval = 2 * time.Second

// CreateSession creates the Session for a conversation with the Agent
// agentRef names, keyed by requestID. The create is idempotent per (caller,
// requestID): a retried first turn returns the session the first attempt
// created instead of a second one. name is the conversation's display name;
// an empty one leaves it unnamed, identified by its id. An Agent that is not
// selectable is refused with ErrAgentUnavailable and the reason. The call
// returns once the session is READY.
func (c *Client) CreateSession(ctx context.Context, agentRef, requestID, name string) (Session, error) {
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return Session{}, err
	}
	if info.Unavailable != "" {
		return Session{}, &AgentUnavailableError{Ref: agentRef, Reason: info.Unavailable}
	}
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return Session{}, err
	}
	req := &apiv1alpha1.CreateSessionRequest{
		Agent:     &apiv1alpha1.ResourceReference{Namespace: info.Namespace, Name: info.Name},
		RequestId: requestID,
		Name:      name,
	}
	resp, err := c.sessions.CreateSession(callCtx, req)
	if err != nil && req.GetName() != "" && status.Code(err) == codes.InvalidArgument {
		// A name the controller will not take costs the name, never the
		// conversation, so a rejected create goes again unnamed. The request id
		// is reused because the controller validates the request in an
		// interceptor, ahead of the handler that reserves it: a rejected
		// create reserved nothing. The retry is the whole of what is known
		// here: the rejection may just as well be about something else, in
		// which case it comes back the same way and is returned below.
		c.logger.Warn("a2a: create refused, retrying it unnamed", "agent", agentRef, "error", err)
		req.Name = ""
		resp, err = c.sessions.CreateSession(callCtx, req)
	}
	if err != nil {
		switch status.Code(err) {
		case codes.FailedPrecondition:
			return Session{}, &AgentUnavailableError{Ref: agentRef, Reason: status.Convert(err).Message()}
		case codes.AlreadyExists:
			return Session{}, fmt.Errorf("a2a: create Session for %s: request id %s was already used for a different conversation: %w", agentRef, requestID, err)
		}
		return Session{}, fmt.Errorf("a2a: create Session for %s: %w", agentRef, err)
	}
	session := resp.GetSession()
	// The routing store keeps the session id alone and every later turn
	// names it as the message's context id, so the two must be the same.
	if contextID := session.GetContextId(); contextID != "" && contextID != session.GetId() {
		return Session{}, fmt.Errorf("a2a: create Session for %s: the controller returned context id %s for session %s", agentRef, contextID, session.GetId())
	}
	c.refreshRosterInBackground(ctx)
	return c.awaitReady(ctx, fromProtoSession(session))
}

// awaitReady polls the session until it is READY, FAILED, or the deadline
// passes. The common case returns immediately.
func (c *Client) awaitReady(ctx context.Context, session Session) (Session, error) {
	deadline := time.Now().Add(sessionReadyTimeout)
	for {
		switch session.State {
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_READY.String(), apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED.String():
			return session, nil
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_FAILED.String():
			return session, fmt.Errorf("a2a: Session %s failed: %s", session.ID, session.Failure)
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETING.String(), apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED.String():
			return session, fmt.Errorf("%w: %s is %s", ErrSessionNotFound, session.ID, session.State)
		}
		if time.Now().After(deadline) {
			return session, fmt.Errorf("a2a: Session %s is still %s after %s", session.ID, session.State, sessionReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return session, ctx.Err()
		case <-time.After(sessionPollInterval):
		}
		next, err := c.GetSession(ctx, session.ID)
		if err != nil {
			return session, err
		}
		session = next
	}
}

// GetSession returns the session, or ErrSessionNotFound when the controller
// no longer knows it (deleted, or created by someone else).
func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return Session{}, err
	}
	resp, err := c.sessions.GetSession(callCtx, &apiv1alpha1.GetSessionRequest{SessionId: id})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		return Session{}, fmt.Errorf("a2a: get Session %s: %w", id, err)
	}
	return fromProtoSession(resp.GetSession()), nil
}

// ResumeSession resumes a SUSPENDED session and returns once it is READY. A
// suspended session accepts no task until then.
func (c *Client) ResumeSession(ctx context.Context, id string) (Session, error) {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return Session{}, err
	}
	resp, err := c.sessions.ResumeSession(callCtx, &apiv1alpha1.ResumeSessionRequest{SessionId: id})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		return Session{}, fmt.Errorf("a2a: resume Session %s: %w", id, err)
	}
	session := fromProtoSession(resp.GetSession())
	deadline := time.Now().Add(sessionReadyTimeout)
	for session.State != apiv1alpha1.RuntimeState_RUNTIME_STATE_READY.String() {
		if session.State == apiv1alpha1.RuntimeState_RUNTIME_STATE_FAILED.String() {
			return session, fmt.Errorf("a2a: Session %s failed: %s", id, session.Failure)
		}
		if time.Now().After(deadline) {
			return session, fmt.Errorf("a2a: Session %s is still %s after %s", id, session.State, sessionReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return session, ctx.Err()
		case <-time.After(sessionPollInterval):
		}
		session, err = c.GetSession(ctx, id)
		if err != nil {
			return session, err
		}
	}
	return session, nil
}

// DeleteSession removes the session so the conversation can start over. A
// missing session is success: it is gone either way.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return err
	}
	_, err = c.sessions.DeleteSession(callCtx, &apiv1alpha1.DeleteSessionRequest{SessionId: id})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("a2a: delete Session %s: %w", id, err)
	}
	return nil
}

// Share is a Session share: ID names it for a revoke, and Token is the secret
// a call presents to act on the session. The controller returns the token
// only when the share is created and keeps just its hash. ExpiresAt is when
// the token stops granting access; zero means never.
type Share struct {
	ID        string
	Token     string
	ExpiresAt time.Time
}

// CreateShare mints a read-write share of the session, which lets a caller
// other than its creator send and cancel turns on it as themselves. Only the
// session's creator may create one. A positive ttl asks the controller to
// expire the share that long after its creation; without one the share lasts
// until it is revoked or the session deleted.
func (c *Client) CreateShare(ctx context.Context, sessionID string, ttl time.Duration) (Share, error) {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return Share{}, err
	}
	req := &apiv1alpha1.CreateSessionShareRequest{
		SessionId:  sessionID,
		Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE,
	}
	if ttl > 0 {
		req.Ttl = durationpb.New(ttl)
	}
	resp, err := c.sessions.CreateSessionShare(callCtx, req)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Share{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
		}
		return Share{}, fmt.Errorf("a2a: share Session %s: %w", sessionID, err)
	}
	if resp.GetToken() == "" || resp.GetShare().GetId() == "" {
		return Share{}, fmt.Errorf("a2a: share Session %s: the controller returned no share token", sessionID)
	}
	share := Share{ID: resp.GetShare().GetId(), Token: resp.GetToken()}
	if at := resp.GetShare().GetExpiresAt(); at != nil {
		share.ExpiresAt = at.AsTime()
	}
	return share, nil
}

// RevokeShare revokes a share, so its token no longer grants anything. A share
// that is already gone is success.
func (c *Client) RevokeShare(ctx context.Context, shareID string) error {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return err
	}
	_, err = c.sessions.RevokeSessionShare(callCtx, &apiv1alpha1.RevokeSessionShareRequest{ShareId: shareID})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("a2a: revoke Session share %s: %w", shareID, err)
	}
	return nil
}

// IsNotFound reports whether err says the session is gone.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrSessionNotFound)
}

func fromProtoSession(pb *apiv1alpha1.Session) Session {
	session := Session{ID: pb.GetId(), State: pb.GetState().String()}
	if f := pb.GetFailure(); f != nil {
		session.Failure = f.GetMessage()
		if session.Failure == "" {
			session.Failure = f.GetReason()
		}
	}
	return session
}
