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
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
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

// StartSession starts the Session for a conversation with the Agent agentRef
// names, keyed by requestID, through the configured SessionStarter
// (agent-manager's start_session, as the caller). The start is idempotent per
// (caller, requestID): a retried first turn returns the session the first
// attempt started instead of a second one. name is the conversation's display
// name; an empty one leaves it unnamed, identified by its id. An Agent that is
// not selectable is refused with ErrAgentUnavailable and the reason before
// anything is started. A non-zero grant starts the session in the workspace it
// admits the caller to. The call returns once the session is READY.
func (c *Client) StartSession(ctx context.Context, agentRef, requestID, name string, grant workspaces.Grant) (Session, error) {
	if c.starter == nil {
		return Session{}, ErrNoStarter
	}
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return Session{}, err
	}
	if info.Unavailable != "" {
		return Session{}, &AgentUnavailableError{Ref: agentRef, Reason: info.Unavailable}
	}
	token, err := c.bearer(ctx)
	if err != nil {
		return Session{}, err
	}
	session, err := c.starter.StartSession(ctx, token, info.Namespace, info.Name, requestID, name, grant)
	if err != nil && name != "" && errors.Is(err, ErrInvalidSessionName) {
		// A name the controller will not take costs the name, never the
		// conversation, so a refused start goes again unnamed. The request id
		// is reused: a refused start reserved nothing. The retry is the whole
		// of what is known here: the refusal may just as well be about
		// something else, in which case it comes back the same way and is
		// returned below.
		c.logger.Warn("a2a: session start refused, retrying it unnamed", "agent", agentRef, "error", err)
		session, err = c.starter.StartSession(ctx, token, info.Namespace, info.Name, requestID, "", grant)
	}
	if err != nil {
		var unavailable *AgentUnavailableError
		if errors.As(err, &unavailable) {
			unavailable.Ref = agentRef
		}
		var notPermitted *AgentNotPermittedError
		if errors.Is(err, ErrAgentNotPermitted) && !errors.As(err, &notPermitted) {
			return Session{}, c.notPermitted(agentRef, err)
		}
		return Session{}, err
	}
	return c.awaitReady(ctx, session)
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

// ShareRefusedError is the controller's refusal to share a Session at all,
// whoever asks: its agent's Harness may run code a turn leaves behind, or the
// agent is gone. Reason is the controller's own text, written for the person
// the refusal is shown to.
type ShareRefusedError struct {
	SessionID string
	Reason    string
}

func (e *ShareRefusedError) Error() string {
	return fmt.Sprintf("a2a: share Session %s refused: %s", e.SessionID, e.Reason)
}

// ShareRefusal returns the controller's reason when err is its refusal to
// share a Session.
func ShareRefusal(err error) (string, bool) {
	var refused *ShareRefusedError
	if errors.As(err, &refused) {
		return refused.Reason, true
	}
	return "", false
}

// CreateShare mints a read-write share of the session, which lets a caller
// other than its creator send and cancel turns on it as themselves. Only the
// session's creator may create one. A positive ttl asks the controller to
// expire the share that long after its creation; without one the share lasts
// until it is revoked or the session deleted. A session the controller
// refuses to share is a *ShareRefusedError.
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
		switch status.Code(err) {
		case codes.NotFound:
			return Share{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
		case codes.FailedPrecondition:
			return Share{}, &ShareRefusedError{SessionID: sessionID, Reason: status.Convert(err).Message()}
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
