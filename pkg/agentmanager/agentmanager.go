// Package agentmanager starts kagent Sessions through agent-manager's
// start_session tool, called through muster as the person behind the turn.
// agent-manager creates the Session as that person, after the checks every
// surface shares (a workspace grant names its caller), so the gateway never
// creates a Session at the controller itself.
//
// The tool is reached by its aggregated name, the configured prefix joined
// with the tool's own name (x_agent-manager_start_session).
//
// An organization's agent is started by the organization's name where the
// installation's agent-manager takes it: the Starter's Target learns the
// organization from agent-manager's list_organizations, as the person, once
// for each start.
package agentmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// agent-manager's tools, by their own names.
const (
	toolStartSession      = "start_session"
	toolListOrganizations = "list_organizations"
)

// ToolCaller calls a muster tool as the person whose bearer token it is
// given. *muster.Client satisfies it.
type ToolCaller interface {
	CallTool(ctx context.Context, bearer, tool string, args map[string]any) (muster.Result, error)
}

// Starter starts Sessions through agent-manager's start_session. It satisfies
// pkga2a.SessionStarter.
type Starter struct {
	tools             ToolCaller
	tool              string
	listOrganizations string
	platform          []string
	logger            *slog.Logger
}

var _ pkga2a.SessionStarter = (*Starter)(nil)

// Options are a Starter's settings beyond its tools.
type Options struct {
	// PlatformNamespaces are the installation's own platform namespaces: an
	// Agent there is no organization's, so its Session starts by namespace
	// without asking agent-manager for the person's organizations.
	PlatformNamespaces []string
	// Logger logs a lookup of the organizations that failed; nil is
	// slog.Default().
	Logger *slog.Logger
}

// New returns a Starter that calls prefix + start_session through tools.
func New(tools ToolCaller, prefix string, opts Options) *Starter {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Starter{
		tools:             tools,
		tool:              prefix + toolStartSession,
		listOrganizations: prefix + toolListOrganizations,
		platform:          slices.Clone(opts.PlatformNamespaces),
		logger:            logger,
	}
}

// Target is how a start names the Agent namespace/name, asked as the person
// bearer belongs to: the Agent of an organization by the organization's name
// where agent-manager takes the organization (organizationOf), any other
// Agent by its namespace. Its refusals are those of organizationOf.
func (s *Starter) Target(ctx context.Context, bearer, namespace, name string) (pkga2a.SessionTarget, error) {
	organization, err := s.organizationOf(ctx, bearer, namespace)
	if err != nil {
		return pkga2a.SessionTarget{}, err
	}
	return pkga2a.SessionTarget{Namespace: namespace, Name: name, Organization: organization}, nil
}

// StartSession starts the Session of a conversation with the Agent target
// names as the person bearer belongs to, keyed by requestID and
// named name (empty: unnamed), in the workspace a non-zero grant admits the
// person to (start_session's workspaceGrant), else in none (noWorkspace). It sends no message: the gateway
// sends the turn itself. agent-manager's refusals map onto the a2a errors a channel
// renders: an Agent that cannot run onto pkga2a.AgentUnavailableError, a
// caller muster or the controller does not know onto pkga2a.ErrNoIdentity, an
// unknown Agent onto pkga2a.ErrAgentUnknown, an Agent the caller may not use
// onto pkga2a.ErrAgentNotPermitted, a refused name onto
// pkga2a.ErrInvalidSessionName. The sign-in muster asks for first (the person
// has not connected a server the start goes through) is a
// pkga2a.ConnectRequiredError.
//
// A target with an organization names the Agent by the organization's name
// and its own (start_session's organization and name); any other by namespace
// and name.
func (s *Starter) StartSession(ctx context.Context, bearer string, target pkga2a.SessionTarget, requestID, sessionName string, grant workspaces.Grant) (pkga2a.Session, error) {
	ref := target.Namespace + "/" + target.Name
	organization := target.Organization
	args := map[string]any{"name": target.Name, "requestId": requestID}
	if organization != "" {
		args["organization"] = organization
	} else {
		args["namespace"] = target.Namespace
	}
	if sessionName != "" {
		args["sessionName"] = sessionName
	}
	// A start without a grant says so: agent-manager otherwise starts it in
	// the Agent's default workspace, which only a grant the gateway asked for
	// as the person may do.
	if grant.IsZero() {
		args["noWorkspace"] = true
	} else {
		args["workspaceGrant"] = grant.Token()
	}
	res, err := s.tools.CallTool(ctx, bearer, s.tool, args)
	if err != nil {
		if tokenRefused(err) {
			return pkga2a.Session{}, fmt.Errorf("%w: muster refused the caller's token for %s: %v", pkga2a.ErrNoIdentity, s.tool, err)
		}
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: %w", ref, err)
	}
	if res.SignIn != nil {
		return pkga2a.Session{}, &pkga2a.ConnectRequiredError{SignIn: res.SignIn}
	}
	if !res.Dispatched {
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: muster did not run %s: %s", ref, s.tool, res.Text)
	}
	if res.IsError {
		return pkga2a.Session{}, refusal(ref, res.Text)
	}
	var out struct {
		// Organization is the organization agent-manager started the
		// Session in, when it started it by one.
		Organization string `json:"organization"`
		Session      *struct {
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
	// An agent-manager that does not take organization (another replica
	// mid-rollout, a rollback since the lookup) drops it and starts the Agent
	// of that name in its default namespace: the answer then names no
	// organization, and the turn must not go to that Session.
	if organization != "" && out.Organization != organization {
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: %s started Session %s not in organization %s but in %q; nothing is sent to it", ref, s.tool, out.Session.ID, organization, out.Organization)
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

// organizationOf is the name of the organization whose Agent Platform
// namespace namespace is, when the installation's agent-manager starts its
// Sessions by the organization's name; "" starts the Session by namespace.
//
// The organizations are agent-manager's list_organizations, asked as the
// person, never derived from the namespace's name. Its entries also tell the
// two contracts apart: the agent-manager whose start_session takes
// organization lists an organization by name and namespace alone, while the
// one before it (AgentPlatformOrganizations, start_session by namespace)
// lists each with its parent namespace. Nothing is cached: an organization
// is looked up again for every Session start (the gateway's unnamed retry of
// a start takes the start's Target), so a namespace that changed hands never
// sends a start to another organization.
//
// A platform namespace asks nothing. A lookup that does not answer starts by
// namespace: that is the earlier contract's start, and the later one refuses
// an organization's namespace given as namespace, so it never reaches another
// Agent. Only a refused token and a sign-in muster asks for are returned,
// since the start would meet them too.
func (s *Starter) organizationOf(ctx context.Context, bearer, namespace string) (string, error) {
	if namespace == "" || slices.Contains(s.platform, namespace) {
		return "", nil
	}
	res, err := s.tools.CallTool(ctx, bearer, s.listOrganizations, nil)
	switch {
	case err != nil && tokenRefused(err):
		return "", fmt.Errorf("%w: muster refused the caller's token for %s: %v", pkga2a.ErrNoIdentity, s.listOrganizations, err)
	case err != nil:
		s.logger.Warn("agentmanager: organizations unavailable, starting by namespace", "namespace", namespace, "error", err)
		return "", nil
	case res.SignIn != nil:
		return "", &pkga2a.ConnectRequiredError{SignIn: res.SignIn}
	case !res.Dispatched || res.IsError:
		s.logger.Warn("agentmanager: organizations unavailable, starting by namespace", "namespace", namespace, "answer", res.Text)
		return "", nil
	}
	var listing struct {
		Organizations []map[string]json.RawMessage `json:"organizations"`
	}
	if err := json.Unmarshal([]byte(res.Text), &listing); err != nil {
		s.logger.Warn("agentmanager: organizations unreadable, starting by namespace", "namespace", namespace, "error", err)
		return "", nil
	}
	for _, entry := range listing.Organizations {
		var name, ns string
		_ = json.Unmarshal(entry["name"], &name)
		_ = json.Unmarshal(entry["namespace"], &ns)
		if ns != namespace || name == "" {
			continue
		}
		if _, earlier := entry["parent"]; earlier {
			return "", nil
		}
		return name, nil
	}
	return "", nil
}

// tokenRefused reports whether muster refused the caller's token.
func tokenRefused(err error) bool {
	var httpErr *muster.HTTPError
	return errors.As(err, &httpErr) && (httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden)
}

// refusal maps start_session's error answer, "<code>: <reason>", onto the a2a
// errors. The codes are agent-manager's: conflict is the controller's
// FailedPrecondition (the Agent has no ready revision) or an organization
// that cannot take Sessions (its Agent Platform is not enabled, or not ready
// yet), not_found an unknown Agent or organization, forbidden its
// PermissionDenied (the caller may not use the Agent), unauthenticated and
// auth_required a caller it cannot act as.
func refusal(ref, text string) error {
	code, reason, _ := strings.Cut(text, ": ")
	switch code {
	case "conflict":
		return &pkga2a.AgentUnavailableError{Ref: ref, Reason: controllerReason(reason)}
	case "unauthenticated", "auth_required":
		return fmt.Errorf("%w: agent-manager: %s", pkga2a.ErrNoIdentity, reason)
	case "not_found":
		return &pkga2a.AgentNotFoundError{Ref: ref, Reason: reason}
	case "forbidden":
		return fmt.Errorf("%w: agent-manager: %s", pkga2a.ErrAgentNotPermitted, reason)
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
