package agentmanager_test

import (
	"context"
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

func starterAnswering(t *testing.T, answer mustertest.Answer) (*agentmanager.Starter, *mustertest.Server) {
	t.Helper()
	srv := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		tool: func(map[string]any) mustertest.Answer { return answer },
	})
	t.Cleanup(srv.Close)
	return agentmanager.New(muster.NewClient(srv.URL), prefix), srv
}

// The answer is agent-manager's SessionStart as start_session encodes it
// without a message: the session as proto3 JSON, no turn.
func TestStartSession_CallsStartSessionAsThePerson(t *testing.T) {
	starter, srv := starterAnswering(t, mustertest.Answer{
		Text: `{"session":{"id":"` + sessionID + `","contextId":"` + sessionID + `","state":"RUNTIME_STATE_READY","agent":{"namespace":"kagent","name":"sre-agent"}}}`,
	})

	session, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "restart kong", workspaces.Grant{})
	require.NoError(t, err)
	require.Equal(t, pkga2a.Session{ID: sessionID, State: "RUNTIME_STATE_READY"}, session)
	require.True(t, session.Ready())

	calls := srv.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, tool, calls[0].Tool)
	require.Equal(t, "Bearer "+bearer, calls[0].Bearer)
	require.Equal(t, map[string]any{"namespace": "kagent", "name": "sre-agent", "requestId": "req-1", "sessionName": "restart kong"}, calls[0].Args)
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

	_, err = starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", auth.Grant)
	require.NoError(t, err)
	calls := srv.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, map[string]any{"namespace": "kagent", "name": "sre-agent", "requestId": "req-1", "workspaceGrant": "signed-grant"}, calls[0].Args)
}

func TestStartSession_CarriesAFailedSessionsReason(t *testing.T) {
	starter, _ := starterAnswering(t, mustertest.Answer{
		Text: `{"session":{"id":"` + sessionID + `","state":"RUNTIME_STATE_FAILED","failure":{"reason":"ImagePullBackOff"}}}`,
	})

	session, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
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
		"name refused": {
			answer: mustertest.Answer{IsError: true, Text: "invalid_request: invalid request: sessionName is longer than 200 characters"},
			is:     pkga2a.ErrInvalidSessionName,
		},
	} {
		t.Run(name, func(t *testing.T) {
			starter, _ := starterAnswering(t, tc.answer)
			_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
			require.ErrorIs(t, err, tc.is)
		})
	}

	starter, _ := starterAnswering(t, mustertest.Answer{IsError: true, Text: "conflict: conflict: kagent: a session of agent kagent/sre-agent: Agent has no ready revision"})
	_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
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

	_, err := agentmanager.New(muster.NewClient(srv.URL), prefix).StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity)
}

// muster's own sign-in challenge for the server (the person is signed in but
// has not connected it) carries the link: the channel's Connect prompt, not
// the sign-in prompt. The link is read from the structured content, or from
// the text when the answer is muster refusing the call before any envelope.
func TestStartSession_MusterSignInChallengeIsConnectRequired(t *testing.T) {
	const link = "https://muster.example/oauth/proxy/start?state=abc"
	text := "auth_required: server 'agent-manager' requires authentication before its tools can be called (this session is not authenticated to it).\n\n" +
		"Authentication Required\n\nServer: agent-manager\nStatus: Authentication required for agent-manager. Please visit the link below to authenticate.\n\n" +
		"Please sign in to connect to this server:\n\n" + link + "\n\nAfter signing in, run this tool again to complete the connection."
	for name, answer := range map[string]mustertest.Answer{
		"structured":   {IsError: true, Text: text, Structured: map[string]any{"status": "auth_required", "server": "agent-manager", "authUrl": link}},
		"text only":    {IsError: true, Text: text},
		"undispatched": {IsError: true, Undispatched: true, Text: text},
	} {
		t.Run(name, func(t *testing.T) {
			starter, _ := starterAnswering(t, answer)
			_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
			var connect *pkga2a.ConnectRequiredError
			require.ErrorAs(t, err, &connect)
			require.Equal(t, "agent-manager", connect.Server)
			require.Equal(t, link, connect.LoginURL)
			require.NotErrorIs(t, err, pkga2a.ErrNoIdentity, "a consent the person can give is not a missing identity")
		})
	}

	// A challenge that names no server is for the tool's own, the one the
	// prefix names; a stop after the link is prose, not the link.
	starter, _ := starterAnswering(t, mustertest.Answer{IsError: true, Undispatched: true, Text: "auth_required: sign in first: " + link + "."})
	_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
	var connect *pkga2a.ConnectRequiredError
	require.ErrorAs(t, err, &connect)
	require.Equal(t, &pkga2a.ConnectRequiredError{Server: "agent-manager", LoginURL: link}, connect)

	// agent-manager's own auth_required, without a link, is a caller the
	// gateway cannot act for: no identity, as before.
	starter, _ = starterAnswering(t, mustertest.Answer{IsError: true, Text: "auth_required: auth required: a commit without the caller's GitHub authorization"})
	_, err = starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity)
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
			_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "", workspaces.Grant{})
			require.Error(t, err)
			require.NotErrorIs(t, err, pkga2a.ErrAgentUnavailable)
		})
	}
}
