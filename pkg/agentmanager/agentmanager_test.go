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

	session, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "restart kong")
	require.NoError(t, err)
	require.Equal(t, pkga2a.Session{ID: sessionID, State: "RUNTIME_STATE_READY"}, session)
	require.True(t, session.Ready())

	calls := srv.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, tool, calls[0].Tool)
	require.Equal(t, "Bearer "+bearer, calls[0].Bearer)
	require.Equal(t, map[string]any{"namespace": "kagent", "name": "sre-agent", "requestId": "req-1", "sessionName": "restart kong"}, calls[0].Args)
}

func TestStartSession_CarriesAFailedSessionsReason(t *testing.T) {
	starter, _ := starterAnswering(t, mustertest.Answer{
		Text: `{"session":{"id":"` + sessionID + `","state":"RUNTIME_STATE_FAILED","failure":{"reason":"ImagePullBackOff"}}}`,
	})

	session, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "")
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
			_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "")
			require.ErrorIs(t, err, tc.is)
		})
	}

	starter, _ := starterAnswering(t, mustertest.Answer{IsError: true, Text: "conflict: conflict: kagent: a session of agent kagent/sre-agent: Agent has no ready revision"})
	_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "")
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

	_, err := agentmanager.New(muster.NewClient(srv.URL), prefix).StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "")
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
			_, err := starter.StartSession(context.Background(), bearer, "kagent", "sre-agent", "req-1", "")
			require.Error(t, err)
			require.NotErrorIs(t, err, pkga2a.ErrAgentUnavailable)
		})
	}
}
