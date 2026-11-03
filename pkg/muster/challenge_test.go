package muster

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResult_SignInChallenge(t *testing.T) {
	const link = "https://muster.example/oauth/proxy/start?state=abc"
	// muster's call_tool answer for a server that holds no grant for the
	// person yet, as seen on a live install.
	const text = "auth_required: server 'agent-manager' requires authentication before its tools can be called (this session is not authenticated to it).\n\n" +
		"Authentication Required\n\nServer: agent-manager\nStatus: Authentication required for agent-manager. Please visit the link below to authenticate.\n\n" +
		"Please sign in to connect to this server:\n\n" + link + "\n\nAfter signing in, run this tool again to complete the connection."

	for name, tc := range map[string]struct {
		res              Result
		server, loginURL string
		challenged       bool
	}{
		"structured": {
			res:    Result{IsError: true, Text: text, Structured: []byte(`{"status":"auth_required","server":"agent-manager","authUrl":"` + link + `"}`)},
			server: "agent-manager", loginURL: link, challenged: true,
		},
		"text only": {
			res:    Result{IsError: true, Text: text},
			server: "agent-manager", loginURL: link, challenged: true,
		},
		"no server named, a stop after the link": {
			res:    Result{IsError: true, Text: "auth_required: sign in first: " + link + "."},
			server: "default-server", loginURL: link, challenged: true,
		},
		"link in an undecoded JSON document": {
			res:    Result{IsError: true, Text: `{"text":"Authentication Required\nhttps://pro.example/auth?a=1\u0026b=2\nthen run it again"}`},
			server: "default-server", loginURL: "https://pro.example/auth?a=1&b=2", challenged: true,
		},
		"agent-manager's own auth_required without a link": {
			res: Result{IsError: true, Text: "auth_required: auth required: a commit without the caller's GitHub authorization"},
		},
		"a link that is not https": {
			res: Result{IsError: true, Text: "Authentication Required\n\nServer: pro\n\nSign in: http://pro.example.com/authorize"},
		},
		"a refusal that is no challenge": {
			res: Result{IsError: true, Text: "not a member of team-bumblebee, see https://docs.example/teams"},
		},
		"not an error": {
			res: Result{Text: text},
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, loginURL, ok := tc.res.SignInChallenge("default-server")
			require.Equal(t, tc.challenged, ok)
			require.Equal(t, tc.server, server)
			require.Equal(t, tc.loginURL, loginURL)
		})
	}
}
