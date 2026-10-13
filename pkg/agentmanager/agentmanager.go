// Package agentmanager starts kagent Sessions through agent-manager's
// start_session tool, called through muster as the person behind the turn.
// agent-manager creates the Session as that person, after the checks every
// surface shares (a workspace grant names its caller), so the gateway never
// creates a Session at the controller itself.
//
// The tool is reached by its aggregated name, the configured prefix joined
// with the tool's own name (x_agent-manager_start_session).
package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// toolStartSession is agent-manager's tool, by its own name.
const toolStartSession = "start_session"

// ToolCaller calls a muster tool as the person whose bearer token it is
// given. *muster.Client satisfies it.
type ToolCaller interface {
	CallTool(ctx context.Context, bearer, tool string, args map[string]any) (muster.Result, error)
}

// Starter starts Sessions through agent-manager's start_session. It satisfies
// pkga2a.SessionStarter.
type Starter struct {
	tools ToolCaller
	tool  string
}

var _ pkga2a.SessionStarter = (*Starter)(nil)

// New returns a Starter that calls prefix + start_session through tools.
func New(tools ToolCaller, prefix string) *Starter {
	return &Starter{tools: tools, tool: prefix + toolStartSession}
}

// StartSession starts the Session of a conversation with the Agent
// namespace/name as the person bearer belongs to, keyed by requestID and
// named name (empty: unnamed), in the workspace a non-zero grant admits the
// person to (start_session's workspaceGrant). It sends no message: the gateway
// sends the turn itself. agent-manager's refusals map onto the a2a errors a channel
// renders: an Agent that cannot run onto pkga2a.AgentUnavailableError, a
// caller muster or the controller does not know onto pkga2a.ErrNoIdentity, an
// unknown Agent onto pkga2a.ErrAgentUnknown, a refused name onto
// pkga2a.ErrInvalidSessionName.
func (s *Starter) StartSession(ctx context.Context, bearer, namespace, name, requestID, sessionName string, grant workspaces.Grant) (pkga2a.Session, error) {
	ref := namespace + "/" + name
	args := map[string]any{"namespace": namespace, "name": name, "requestId": requestID}
	if sessionName != "" {
		args["sessionName"] = sessionName
	}
	if !grant.IsZero() {
		args["workspaceGrant"] = grant.Token()
	}
	res, err := s.tools.CallTool(ctx, bearer, s.tool, args)
	if err != nil {
		var httpErr *muster.HTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden) {
			return pkga2a.Session{}, fmt.Errorf("%w: muster refused the caller's token for %s: %v", pkga2a.ErrNoIdentity, s.tool, err)
		}
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: %w", ref, err)
	}
	if !res.Dispatched {
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: muster did not run %s: %s", ref, s.tool, res.Text)
	}
	if res.IsError {
		return pkga2a.Session{}, refusal(ref, res.Text)
	}
	var out struct {
		Session *struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			ContextID string `json:"contextId"`
			Failure   *struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"failure"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: decode the answer of %s: %w", ref, s.tool, err)
	}
	if out.Session == nil || out.Session.ID == "" {
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: %s answered no session id", ref, s.tool)
	}
	// The routing store keeps the session id alone and every later turn names
	// it as the message's context id, so the two must be the same.
	if c := out.Session.ContextID; c != "" && c != out.Session.ID {
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: the controller returned context id %s for session %s", ref, c, out.Session.ID)
	}
	session := pkga2a.Session{ID: out.Session.ID, State: out.Session.State}
	if f := out.Session.Failure; f != nil {
		session.Failure = f.Message
		if session.Failure == "" {
			session.Failure = f.Reason
		}
	}
	return session, nil
}

// refusal maps start_session's error answer, "<code>: <reason>", onto the a2a
// errors. The codes are agent-manager's: conflict is the controller's
// FailedPrecondition (the Agent has no ready revision), unauthenticated and
// auth_required a caller it cannot act as.
func refusal(ref, text string) error {
	code, reason, _ := strings.Cut(text, ": ")
	switch code {
	case "conflict":
		return &pkga2a.AgentUnavailableError{Ref: ref, Reason: controllerReason(reason)}
	case "unauthenticated", "auth_required":
		return fmt.Errorf("%w: agent-manager: %s", pkga2a.ErrNoIdentity, reason)
	case "not_found":
		return fmt.Errorf("%w: agent-manager: %s", pkga2a.ErrAgentUnknown, reason)
	case "invalid_request":
		return fmt.Errorf("%w: agent-manager: %s", pkga2a.ErrInvalidSessionName, reason)
	}
	return fmt.Errorf("agentmanager: start a Session of %s: %s", ref, text)
}

// controllerReason is the controller's own sentence out of agent-manager's
// "conflict: kagent: a session of agent <ref>: <sentence>".
func controllerReason(reason string) string {
	if i := strings.LastIndex(reason, ": "); i >= 0 && strings.Contains(reason[:i], "kagent: ") {
		return reason[i+2:]
	}
	return reason
}
