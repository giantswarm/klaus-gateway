package muster_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/muster/mustertest"
)

// personToken is the person's dex id_token the tests call as.
const personToken = "id-token-alice"

func TestCallTool_RunsAsThePersonThroughCallTool(t *testing.T) {
	fake := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		"x_repo_approve_change": func(map[string]any) mustertest.Answer { return mustertest.Answer{Text: "review submitted"} },
	})
	defer fake.Close()

	res, err := muster.NewClient(fake.URL+"/").CallTool(t.Context(), personToken, "x_repo_approve_change", map[string]any{"pr": 7})
	require.NoError(t, err)
	require.Equal(t, muster.Result{Text: "review submitted", Dispatched: true}, res)
	calls := fake.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, "Bearer "+personToken, calls[0].Bearer, "muster sees the person")
	require.Equal(t, "x_repo_approve_change", calls[0].Tool, "the tool is reached through call_tool")
	require.Equal(t, map[string]any{"pr": float64(7)}, calls[0].Args)
}

func TestCallTool_ToolRefusalIsAResultNotAnError(t *testing.T) {
	fake := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		"x_repo_approve_change": func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Text: "alice is not a member of team-bumblebee", IsError: true}
		},
	})
	defer fake.Close()

	res, err := muster.NewClient(fake.URL).CallTool(t.Context(), personToken, "x_repo_approve_change", nil)
	require.NoError(t, err)
	require.Equal(t, muster.Result{Text: "alice is not a member of team-bumblebee", IsError: true, Dispatched: true}, res)
}

func TestCallTool_StructuredContentOfTheTool(t *testing.T) {
	fake := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		"x_ws_list_workspaces": func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Text: "2 workspaces", Structured: map[string]any{"workspaces": []any{map[string]any{"name": "a"}}}}
		},
	})
	defer fake.Close()

	res, err := muster.NewClient(fake.URL).CallTool(t.Context(), personToken, "x_ws_list_workspaces", nil)
	require.NoError(t, err)
	require.Equal(t, "2 workspaces", res.Text)
	require.JSONEq(t, `{"workspaces":[{"name":"a"}]}`, string(res.Structured))
	require.True(t, res.Dispatched)
}

// muster's own refusal (an unknown tool, one outside the caller's toolset)
// is call_tool's plain-text error result, no inner envelope: still a result.
func TestCallTool_MetaToolRefusalIsAResultNotAnError(t *testing.T) {
	fake := mustertest.New(nil)
	defer fake.Close()

	res, err := muster.NewClient(fake.URL).CallTool(t.Context(), personToken, "x_repo_approve_change", nil)
	require.NoError(t, err)
	require.Equal(t, muster.Result{Text: "Tool execution failed: tool not found: x_repo_approve_change", IsError: true}, res)
}

func TestCallTool_RefusedTokenIsAnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := muster.NewClient(srv.URL).CallTool(t.Context(), "stale", "x_repo_approve_change", nil)
	var httpErr *muster.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusUnauthorized, httpErr.Status)
}

func TestCallTool_NoBearerNoCall(t *testing.T) {
	fake := mustertest.New(nil)
	defer fake.Close()

	_, err := muster.NewClient(fake.URL).CallTool(t.Context(), "", "x", nil)
	require.Error(t, err)
	require.Empty(t, fake.Calls())
}

// The client declares URL elicitation, so a call that needs a sign-in is
// answered with the link to open, not run; Accept retries it with the
// person's accept, echoing muster's request state.
func TestCallTool_SignInThenAccept(t *testing.T) {
	fake := mustertest.New(nil)
	defer fake.Close()
	fake.CallTools = map[string]func(mustertest.Call) mustertest.Answer{
		"x_agent-manager_start_session": func(call mustertest.Call) mustertest.Answer {
			if !call.Accepted {
				return mustertest.Answer{SignIn: &mustertest.SignIn{Message: "Sign in to github to continue.", URL: "https://muster.example/oauth/proxy/start?s=1"}}
			}
			return mustertest.Answer{Text: `{"session":{"id":"s1"}}`}
		},
	}
	client := muster.NewClient(fake.URL)

	res, err := client.CallTool(t.Context(), personToken, "x_agent-manager_start_session", map[string]any{"requestId": "r1"})
	require.NoError(t, err)
	require.NotNil(t, res.SignIn)
	require.Equal(t, "Sign in to github to continue.", res.SignIn.Message)
	require.Equal(t, "https://muster.example/oauth/proxy/start?s=1", res.SignIn.URL)
	require.False(t, res.Dispatched)

	res, err = client.Accept(t.Context(), personToken, res.SignIn)
	require.NoError(t, err)
	require.Nil(t, res.SignIn)
	require.Equal(t, muster.Result{Text: `{"session":{"id":"s1"}}`, Dispatched: true}, res)

	calls := fake.Calls()
	require.Len(t, calls, 2)
	require.False(t, calls[0].Accepted)
	require.True(t, calls[1].Accepted, "the retry carries the person's accept")
	require.Equal(t, "Bearer "+personToken, calls[1].Bearer)
	require.Equal(t, "x_agent-manager_start_session", calls[1].Tool, "the same call again")
	require.Equal(t, map[string]any{"requestId": "r1"}, calls[1].Args)
}

func TestCallTool_UnusableSignInLinkIsAnError(t *testing.T) {
	for name, link := range map[string]string{
		"plain http": "http://muster.example/start",
		"userinfo":   "https://alice@muster.example/start",
		"relative":   "/oauth/proxy/start",
		"javascript": "javascript:alert(1)",
	} {
		t.Run(name, func(t *testing.T) {
			fake := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
				"x_tool": func(map[string]any) mustertest.Answer {
					return mustertest.Answer{SignIn: &mustertest.SignIn{Message: "Sign in.", URL: link}}
				},
			})
			defer fake.Close()

			_, err := muster.NewClient(fake.URL).CallTool(t.Context(), personToken, "x_tool", nil)
			require.ErrorIs(t, err, muster.ErrUnusableSignIn)
		})
	}
}

func TestValidateSignInURL(t *testing.T) {
	for link, ok := range map[string]bool{
		"https://muster.example/start?s=1":    true,
		"http://127.0.0.1:8080/start":         true,
		"http://localhost/start":              true,
		"http://muster.example/start":         false,
		"https://a:b@muster.example/":         false,
		"https:///nohost":                     false,
		"ftp://muster.example/":               false,
		"https://muster.example/a b":          false,
		"https://muster.example/`x`":          false,
		"https://muster.example/\nx":          false,
		"https://muster.example/\u202eetadpu": false,
		"https://muster.example/a\u200bb":     false,
		"https://muster.example/a\u00a0b":     false,
		"https://muster.example/<x>":          false,
		"https://muster.example/\xff":         false,
		"https://müster.example/start":        true,
		"https://muster.example/" + strings.Repeat("a", muster.SignInURLMaxBytes-len("https://muster.example/")):   true,
		"https://muster.example/" + strings.Repeat("a", muster.SignInURLMaxBytes-len("https://muster.example/")+1): false,
	} {
		err := muster.ValidateSignInURL(link)
		if ok {
			require.NoError(t, err, link)
		} else {
			require.ErrorIs(t, err, muster.ErrUnusableSignIn, link)
		}
	}
}

func TestAccept_NoSignIn(t *testing.T) {
	_, err := muster.NewClient("http://unused").Accept(t.Context(), personToken, nil)
	require.Error(t, err)
}

func TestDecline_AnswersTheSignInAndRunsNothing(t *testing.T) {
	fake := mustertest.New(nil)
	defer fake.Close()
	fake.CallTools = map[string]func(mustertest.Call) mustertest.Answer{
		"x_tool": func(mustertest.Call) mustertest.Answer {
			return mustertest.Answer{SignIn: &mustertest.SignIn{Message: "Sign in.", URL: "https://muster.example/s"}}
		},
	}
	client := muster.NewClient(fake.URL)
	res, err := client.CallTool(t.Context(), personToken, "x_tool", map[string]any{"a": "b"})
	require.NoError(t, err)
	require.NotNil(t, res.SignIn)

	require.NoError(t, client.Decline(t.Context(), personToken, res.SignIn))
	calls := fake.Calls()
	require.Len(t, calls, 2)
	require.True(t, calls[1].Declined, "the retry carries the person's decline")
	require.Equal(t, map[string]any{"a": "b"}, calls[1].Args)

	require.Error(t, client.Decline(t.Context(), personToken, nil))
}
