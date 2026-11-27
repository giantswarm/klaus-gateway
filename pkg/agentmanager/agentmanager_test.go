package agentmanager_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/agentmanager"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/muster/mustertest"
	"github.com/giantswarm/klaus-gateway/pkg/workspaces"
)

const (
	prefix    = "x_agent-manager_"
	tool      = prefix + "start_session"
	bearer    = "id-token-alice"
	sessionID = "0192f1c2-7d1e-7a3b-9c4d-000000000001"
)

// options name kagent the installation's platform namespace, as the gateway
// does with its own.
var options = agentmanager.Options{PlatformNamespaces: []string{"kagent"}}

func starterAnswering(t *testing.T, answer mustertest.Answer) (*agentmanager.Starter, *mustertest.Server) {
	t.Helper()
	return starterWith(t, map[string]func(map[string]any) mustertest.Answer{
		tool: func(map[string]any) mustertest.Answer { return answer },
	})
}

func starterWith(t *testing.T, tools map[string]func(map[string]any) mustertest.Answer) (*agentmanager.Starter, *mustertest.Server) {
	t.Helper()
	srv := mustertest.New(tools)
	t.Cleanup(srv.Close)
	return agentmanager.New(muster.NewClient(srv.URL), prefix, options), srv
}

// start starts a Session of the Agent namespace/name as the gateway does, as
// bearer with request id req-1: its target, then the start by that target.
func start(starter *agentmanager.Starter, namespace, name, sessionName string, grant workspaces.Grant) (pkga2a.Session, error) {
	ctx := context.Background()
	target, err := starter.Target(ctx, bearer, namespace, name)
	if err != nil {
		return pkga2a.Session{}, err
	}
	return starter.StartSession(ctx, bearer, target, "req-1", sessionName, grant)
}

// The two contracts of agent-manager: the one whose start_session takes an
// organization's agent by the organization's name (list_organizations lists
// an organization by name and namespace), and the one before it, whose
// start_session takes the namespace (list_organizations lists each with its
// parent). Whatever the person's organizations, an Agent outside them starts
// by namespace, and a platform namespace asks for none.
func TestStartSession_NamesTheAgentAsTheInstallationsAgentManagerTakesIt(t *testing.T) {
	byOrganization := map[string]any{"organization": "acme", "name": "billing", "requestId": "req-1", "noWorkspace": true}
	byNamespace := func(ns string) map[string]any {
		return map[string]any{"namespace": ns, "name": "billing", "requestId": "req-1", "noWorkspace": true}
	}
	for name, tc := range map[string]struct {
		namespace     string
		organizations *mustertest.Answer
		want          map[string]any
		calls         int
	}{
		"an organization's agent, agent-manager takes the organization": {
			namespace: "agents-acme",
			organizations: &mustertest.Answer{Text: `{"organizations":[` +
				`{"name":"payments","namespace":"agents-payments","permissions":{"manage":false,"member":true}},` +
				`{"name":"acme","namespace":"agents-acme","permissions":{"manage":true,"member":true}}]}`},
			want: byOrganization, calls: 2,
		},
		"an organization's agent, agent-manager takes the namespace": {
			namespace: "agents-acme-x7k2p",
			organizations: &mustertest.Answer{Text: `{"organizations":[` +
				`{"name":"acme","parent":"org-giantswarm","namespace":"agents-acme-x7k2p","permissions":{"manage":true,"member":true}}]}`},
			want: byNamespace("agents-acme-x7k2p"), calls: 2,
		},
		"a namespace of none of the person's organizations": {
			namespace:     "team-a",
			organizations: &mustertest.Answer{Text: `{"organizations":[],"none":"you manage and belong to no organization"}`},
			want:          byNamespace("team-a"), calls: 2,
		},
		"an agent-manager without organizations": {
			namespace:     "team-a",
			organizations: &mustertest.Answer{IsError: true, Text: "unsupported: organization management is not configured"},
			want:          byNamespace("team-a"), calls: 2,
		},
		"list_organizations not behind muster": {
			namespace: "team-a",
			want:      byNamespace("team-a"), calls: 2,
		},
		"the installation's platform namespace": {
			namespace:     "kagent",
			organizations: &mustertest.Answer{Text: `{"organizations":[{"name":"acme","namespace":"kagent"}]}`},
			want:          byNamespace("kagent"), calls: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			tools := map[string]func(map[string]any) mustertest.Answer{
				tool: startedAsAgentManagerAnswers,
			}
			if tc.organizations != nil {
				tools[prefix+"list_organizations"] = func(map[string]any) mustertest.Answer { return *tc.organizations }
			}
			starter, srv := starterWith(t, tools)

			_, err := start(starter, tc.namespace, "billing", "", workspaces.Grant{})
			require.NoError(t, err)
			calls := srv.Calls()
			require.Len(t, calls, tc.calls)
			last := calls[len(calls)-1]
			require.Equal(t, tool, last.Tool)
			require.Equal(t, tc.want, last.Args)
			for _, call := range calls {
				require.Equal(t, "Bearer "+bearer, call.Bearer, "every call is the person's")
			}
		})
	}
}

// startedAsAgentManagerAnswers is start_session's answer: a ready Session,
// naming the organization it was started in when it was started by one.
func startedAsAgentManagerAnswers(args map[string]any) mustertest.Answer {
	answer := map[string]any{"session": map[string]any{"id": sessionID, "state": "RUNTIME_STATE_READY"}}
	if org, ok := args["organization"]; ok {
		answer["organization"] = org
	}
	text, _ := json.Marshal(answer)
	return mustertest.Answer{Text: string(text)}
}

// A start by organization that an agent-manager without organization
// answered (another replica mid-rollout: it drops the argument and starts the
// Agent of that name in its default namespace) is no Session of the
// organization's Agent.
func TestStartSession_RefusesASessionOutsideTheOrganization(t *testing.T) {
	starter, _ := starterWith(t, map[string]func(map[string]any) mustertest.Answer{
		prefix + "list_organizations": func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Text: `{"organizations":[{"name":"acme","namespace":"agents-acme"}]}`}
		},
		tool: func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Text: `{"session":{"id":"` + sessionID + `","state":"RUNTIME_STATE_READY","agent":{"namespace":"kagent","name":"billing"}}}`}
		},
	})
	_, err := start(starter, "agents-acme", "billing", "", workspaces.Grant{})
	require.ErrorContains(t, err, "not in organization acme")
}

// An organization agent-manager cannot start a Session in is refused in
// agent-manager's own words, which the channel shows the person.
func TestStartSession_CarriesTheOrganizationsRefusal(t *testing.T) {
	const listed = `{"organizations":[{"name":"acme","namespace":"agents-acme"}]}`
	for name, tc := range map[string]struct {
		answer string
		is     error
		reason string
	}{
		"not found": {
			answer: "not_found: organization acme not found; list_organizations lists yours",
			is:     pkga2a.ErrAgentUnknown,
			reason: "organization acme not found; list_organizations lists yours",
		},
		"not enabled": {
			answer: "conflict: the Agent Platform is not enabled for organization acme (its Organization's spec.agentPlatform.enabled is not true); nothing was written",
			is:     pkga2a.ErrAgentUnavailable,
			reason: "the Agent Platform is not enabled for organization acme (its Organization's spec.agentPlatform.enabled is not true); nothing was written",
		},
		"not ready": {
			answer: "conflict: organization acme is not ready yet: agent-platform-operator has not reported its namespace (status.agentPlatform.namespace); nothing was written, try again shortly",
			is:     pkga2a.ErrAgentUnavailable,
			reason: "organization acme is not ready yet: agent-platform-operator has not reported its namespace (status.agentPlatform.namespace); nothing was written, try again shortly",
		},
	} {
		t.Run(name, func(t *testing.T) {
			starter, _ := starterWith(t, map[string]func(map[string]any) mustertest.Answer{
				prefix + "list_organizations": func(map[string]any) mustertest.Answer { return mustertest.Answer{Text: listed} },
				tool:                          func(map[string]any) mustertest.Answer { return mustertest.Answer{IsError: true, Text: tc.answer} },
			})
			_, err := start(starter, "agents-acme", "billing", "", workspaces.Grant{})
			require.ErrorIs(t, err, tc.is)
			var unavailable *pkga2a.AgentUnavailableError
			var notFound *pkga2a.AgentNotFoundError
			switch {
			case errors.As(err, &unavailable):
				require.Equal(t, tc.reason, unavailable.Reason)
			case errors.As(err, &notFound):
				require.Equal(t, tc.reason, notFound.Reason)
			default:
				t.Fatalf("%v carries no reason", err)
			}
		})
	}
}

// The answer is agent-manager's SessionStart as start_session encodes it
// without a message: the session as proto3 JSON, no turn.
func TestStartSession_CallsStartSessionAsThePerson(t *testing.T) {
	starter, srv := starterAnswering(t, mustertest.Answer{
		Text: `{"session":{"id":"` + sessionID + `","contextId":"` + sessionID + `","state":"RUNTIME_STATE_READY","agent":{"namespace":"kagent","name":"sre-agent"}}}`,
	})

	session, err := start(starter, "kagent", "sre-agent", "restart kong", workspaces.Grant{})
	require.NoError(t, err)
	require.Equal(t, pkga2a.Session{ID: sessionID, State: "RUNTIME_STATE_READY"}, session)
	require.True(t, session.Ready())

	calls := srv.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, tool, calls[0].Tool)
	require.Equal(t, "Bearer "+bearer, calls[0].Bearer)
	require.Equal(t, map[string]any{"namespace": "kagent", "name": "sre-agent", "requestId": "req-1", "sessionName": "restart kong", "noWorkspace": true}, calls[0].Args,
		"a start without a grant says so: agent-manager would otherwise start it in the agent's default workspace")
}

// A Session started in a workspace carries the grant authorize_workspace
// answered as workspaceGrant, and nothing else changes.
func TestStartSession_PassesTheWorkspaceGrant(t *testing.T) {
	starter, srv := starterAnswering(t, mustertest.Answer{
		Text: `{"session":{"id":"` + sessionID + `","contextId":"` + sessionID + `","state":"RUNTIME_STATE_READY"}}`,
	})
	wm := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		"x_workspace-manager_authorize_workspace": func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Structured: map[string]any{"workspace": "platform", "outcome": "granted", "grant": "signed-grant", "expiresAt": "2026-10-13T12:00:00Z"}}
		},
	})
	t.Cleanup(wm.Close)
	auth, err := workspaces.New(muster.NewClient(wm.URL), "x_workspace-manager_").Authorize(context.Background(), bearer, "platform")
	require.NoError(t, err)

	_, err = start(starter, "kagent", "sre-agent", "", auth.Grant)
	require.NoError(t, err)
	calls := srv.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, map[string]any{"namespace": "kagent", "name": "sre-agent", "requestId": "req-1", "workspaceGrant": "signed-grant"}, calls[0].Args)
}

func TestStartSession_CarriesAFailedSessionsReason(t *testing.T) {
	starter, _ := starterAnswering(t, mustertest.Answer{
		Text: `{"session":{"id":"` + sessionID + `","state":"RUNTIME_STATE_FAILED","failure":{"reason":"ImagePullBackOff"}}}`,
	})

	session, err := start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
	require.NoError(t, err)
	require.Equal(t, "ImagePullBackOff", session.Failure)
}

func TestStartSession_MapsRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		answer mustertest.Answer
		is     error
	}{
		"agent not runnable": {
			answer: mustertest.Answer{IsError: true, Text: "conflict: conflict: kagent: a session of agent kagent/sre-agent: Agent has no ready revision"},
			is:     pkga2a.ErrAgentUnavailable,
		},
		"caller unknown to the controller": {
			answer: mustertest.Answer{IsError: true, Text: "unauthenticated: unauthenticated: kagent: a session of agent kagent/sre-agent: invalid credentials"},
			is:     pkga2a.ErrNoIdentity,
		},
		"no such agent": {
			answer: mustertest.Answer{IsError: true, Text: "not_found: not found: kagent: a session of agent kagent/gone: agent not found"},
			is:     pkga2a.ErrAgentUnknown,
		},
		"agent not the caller's": {
			answer: mustertest.Answer{IsError: true, Text: "forbidden: forbidden: kagent: a session of agent org-acme/billing: not authorized"},
			is:     pkga2a.ErrAgentNotPermitted,
		},
		"name refused": {
			answer: mustertest.Answer{IsError: true, Text: "invalid_request: invalid request: sessionName is longer than 200 characters"},
			is:     pkga2a.ErrInvalidSessionName,
		},
	} {
		t.Run(name, func(t *testing.T) {
			starter, _ := starterAnswering(t, tc.answer)
			_, err := start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
			require.ErrorIs(t, err, tc.is)
		})
	}

	starter, _ := starterAnswering(t, mustertest.Answer{IsError: true, Text: "conflict: conflict: kagent: a session of agent kagent/sre-agent: Agent has no ready revision"})
	_, err := start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
	var unavailable *pkga2a.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, "Agent has no ready revision", unavailable.Reason, "the controller's own sentence, for the agent-not-runnable notice")
}

// muster refusing the person's token is the same as no identity: the
// channel's sign-in prompt.
func TestStartSession_MusterRefusingTheTokenIsNoIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	_, err := start(agentmanager.New(muster.NewClient(srv.URL), prefix, options), "kagent", "sre-agent", "", workspaces.Grant{})
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity)
}

// muster answers a call it cannot run as the caller's token (it lacks an
// audience muster needs, or the Kubernetes API rejects it) with its re-login
// error instead of dispatching the tool: a refused token, with muster's reason.
func TestStartSession_MusterReloginIsTokenRefused(t *testing.T) {
	const reason = `Tool execution failed: tool not found: ` + tool + `: Your session token does not carry the "kubernetes" audience the Kubernetes API needs to tell which MCP servers you may see. A re-login is required: sign in again to get a fresh session token, then retry.`
	starter, _ := starterAnswering(t, mustertest.Answer{Undispatched: true, IsError: true, Text: reason})
	_, err := start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
	var refused *pkga2a.TokenRefusedError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, reason, refused.Reason)
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity)

	// The lookup of the caller's organizations meets the same refusal, and
	// nothing is started after it.
	starter, srv := starterWith(t, map[string]func(map[string]any) mustertest.Answer{
		prefix + "list_organizations": func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Undispatched: true, IsError: true, Text: reason}
		},
		tool: func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Text: `{"session":{"id":"` + sessionID + `"}}`}
		},
	})
	_, err = start(starter, "agents-acme", "billing", "", workspaces.Grant{})
	require.ErrorAs(t, err, &refused)
	require.Len(t, srv.Calls(), 1)
}

// The sign-in muster asks for (the person is signed in but has not connected
// a server the start goes through) is a URL elicitation: the channel's
// Connect prompt, not the sign-in prompt.
func TestStartSession_MusterSignInIsConnectRequired(t *testing.T) {
	const link = "https://muster.example/oauth/proxy/start?state=abc"
	starter, _ := starterAnswering(t, mustertest.Answer{SignIn: &mustertest.SignIn{Message: "Sign in to agent-manager to continue.", URL: link}})
	_, err := start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
	var connect *pkga2a.ConnectRequiredError
	require.ErrorAs(t, err, &connect)
	require.Equal(t, link, connect.SignIn.URL)
	require.Equal(t, "Sign in to agent-manager to continue.", connect.SignIn.Message)
	require.NotErrorIs(t, err, pkga2a.ErrNoIdentity, "a consent the person can give is not a missing identity")

	// The sign-in asked for the lookup of the person's organizations is the
	// same Connect prompt, and nothing is started before it.
	starter, srv := starterWith(t, map[string]func(map[string]any) mustertest.Answer{
		prefix + "list_organizations": func(map[string]any) mustertest.Answer {
			return mustertest.Answer{SignIn: &mustertest.SignIn{Message: "Sign in to agent-manager to continue.", URL: link}}
		},
		tool: func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Text: `{"session":{"id":"` + sessionID + `"}}`}
		},
	})
	_, err = start(starter, "agents-acme", "billing", "", workspaces.Grant{})
	require.ErrorAs(t, err, &connect)
	require.Equal(t, link, connect.SignIn.URL)
	require.Len(t, srv.Calls(), 1)

	// agent-manager's own auth_required, without a link, is a caller the
	// gateway cannot act for: no identity, as before.
	starter, _ = starterAnswering(t, mustertest.Answer{IsError: true, Text: "auth_required: auth required: a commit without the caller's GitHub authorization"})
	_, err = start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity)

	// A refusal that names a server and carries a link, without muster's
	// sign-in markers, is no challenge: the person gets no Connect prompt.
	starter, _ = starterAnswering(t, mustertest.Answer{IsError: true, Text: "rate limited by server 'agent-manager', see https://docs.example/limits"})
	_, err = start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
	require.Error(t, err)
	require.NotErrorAs(t, err, &connect)
}

func TestStartSession_RefusesWhatIsNoSession(t *testing.T) {
	for name, answer := range map[string]mustertest.Answer{
		"tool not behind muster": {Undispatched: true, IsError: true, Text: "Tool execution failed: tool not found: " + tool},
		"no session id":          {Text: `{"session":{}}`},
		"not json":               {Text: "started"},
		"context id differs":     {Text: `{"session":{"id":"` + sessionID + `","contextId":"another"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			starter, _ := starterAnswering(t, answer)
			_, err := start(starter, "kagent", "sre-agent", "", workspaces.Grant{})
			require.Error(t, err)
			require.NotErrorIs(t, err, pkga2a.ErrAgentUnavailable)
		})
	}
}
