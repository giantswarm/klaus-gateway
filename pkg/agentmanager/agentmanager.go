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
	"regexp"
	"strings"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

// toolStartSession is agent-manager's tool, by its own name.
const toolStartSession = "start_session"

// What muster's sign-in challenge says in its text when its structured
// content is not there to read: the server on its "Server:" line or in the
// first sentence, and the sign-in link on a line of its own.
var (
	challengeServerRe = regexp.MustCompile(`(?m)^Server:\s*(\S+)|server '([^']+)'`)
	challengeURLRe    = regexp.MustCompile(`https://\S+`)
)

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
	// server is the muster server the tool is of, as the prefix names it
	// (x_<server>_): what a sign-in challenge that names none is for.
	server string
}

var _ pkga2a.SessionStarter = (*Starter)(nil)

// New returns a Starter that calls prefix + start_session through tools.
func New(tools ToolCaller, prefix string) *Starter {
	return &Starter{tools: tools, tool: prefix + toolStartSession, server: strings.TrimSuffix(strings.TrimPrefix(prefix, "x_"), "_")}
}

// StartSession starts the Session of a conversation with the Agent
// namespace/name as the person bearer belongs to, keyed by requestID and
// named name (empty: unnamed), in the workspace a non-zero grant admits the
// person to (start_session's workspaceGrant), else in none (noWorkspace). It sends no message: the gateway
// sends the turn itself. agent-manager's refusals map onto the a2a errors a channel
// renders: an Agent that cannot run onto pkga2a.AgentUnavailableError, a
// caller muster or the controller does not know onto pkga2a.ErrNoIdentity, an
// unknown Agent onto pkga2a.ErrAgentUnknown, an Agent the caller may not use
// onto pkga2a.ErrAgentNotPermitted, a refused name onto
// pkga2a.ErrInvalidSessionName. muster's own sign-in challenge for the server
// (the person has not connected it; the link says where) is a
// pkga2a.ConnectRequiredError.
func (s *Starter) StartSession(ctx context.Context, bearer, namespace, name, requestID, sessionName string, grant workspaces.Grant) (pkga2a.Session, error) {
	ref := namespace + "/" + name
	args := map[string]any{"namespace": namespace, "name": name, "requestId": requestID}
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
		var httpErr *muster.HTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden) {
			return pkga2a.Session{}, fmt.Errorf("%w: muster refused the caller's token for %s: %v", pkga2a.ErrNoIdentity, s.tool, err)
		}
		return pkga2a.Session{}, fmt.Errorf("agentmanager: start a Session of %s: %w", ref, err)
	}
	if challenge := signInChallenge(res, s.server); challenge != nil {
		return pkga2a.Session{}, challenge
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

// signInChallenge reads muster's sign-in challenge out of the answer: an
// error result whose structured content says auth_required and names the
// server and the link, the text saying the same (and alone, when the answer
// is muster refusing the call before any envelope). A challenge that names no
// server is for the tool's own. A challenge that carries no link is not one
// the person can act on from a prompt: nil, so the answer is mapped as a
// refusal.
func signInChallenge(res muster.Result, server string) *pkga2a.ConnectRequiredError {
	if !res.IsError {
		return nil
	}
	var challenge struct {
		Status  string `json:"status"`
		Server  string `json:"server"`
		AuthURL string `json:"authUrl"`
	}
	structured := len(res.Structured) > 0 && json.Unmarshal(res.Structured, &challenge) == nil && challenge.Status == "auth_required"
	if !structured && !strings.HasPrefix(res.Text, "auth_required") {
		return nil
	}
	if challenge.Server == "" {
		if m := challengeServerRe.FindStringSubmatch(res.Text); m != nil {
			challenge.Server = m[1] + m[2]
		}
	}
	if challenge.Server == "" {
		challenge.Server = server
	}
	if challenge.AuthURL == "" {
		// The link ends its line in muster's prose; a stop after it is prose too.
		challenge.AuthURL = strings.TrimRight(challengeURLRe.FindString(res.Text), ".,;:)")
	}
	if challenge.AuthURL == "" {
		return nil
	}
	return &pkga2a.ConnectRequiredError{Server: challenge.Server, LoginURL: challenge.AuthURL}
}

// refusal maps start_session's error answer, "<code>: <reason>", onto the a2a
// errors. The codes are agent-manager's: conflict is the controller's
// FailedPrecondition (the Agent has no ready revision), forbidden its
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
		return fmt.Errorf("%w: agent-manager: %s", pkga2a.ErrAgentUnknown, reason)
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
