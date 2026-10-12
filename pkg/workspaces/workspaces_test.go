package workspaces

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/muster/mustertest"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

const (
	prefix = "x_workspace-manager_"
	bearer = "id-token-alice"
	// grantToken stands for the workspace-manager's signed grant.
	grantToken = "grant-eyJhbGciOiJFUzI1NiJ9.c2VjcmV0.c2ln"
)

var expires = time.Date(2026, 10, 12, 14, 5, 0, 0, time.UTC)

// The answers below are the workspace-manager's results as v0.4.1 shapes
// them: list_workspaces' WorkspacesResult, authorize_workspace's
// authorize.Result, list_providers' ProvidersResult.

func listing(map[string]any) mustertest.Answer {
	return mustertest.Answer{Text: "3 workspaces", Structured: map[string]any{"workspaces": []any{
		map[string]any{
			"name": "platform", "organization": "giantswarm",
			"sources": []any{
				map[string]any{"provider": "github", "owner": "giantswarm", "repositories": []any{"klaus-gateway"}},
				map[string]any{"provider": "gitlab", "owner": "infra", "filters": map[string]any{"topics": []any{"go"}}},
				map[string]any{"provider": "github", "owner": "giantswarm", "repositories": []any{"muster"}},
			},
			"sync":     map[string]any{"schedule": "nightly"},
			"lastSync": map[string]any{"time": "2026-10-12T03:00:00Z", "result": "Succeeded", "trigger": "schedule"},
			"nextSync": "2026-10-13T03:00:00Z", "repositories": 2, "sessions": 1, "resourceVersion": "7", "createdAt": "2026-10-01T09:00:00Z",
		},
		map[string]any{
			"name": "docs", "organization": "giantswarm",
			"sources":  []any{map[string]any{"provider": "github", "owner": "giantswarm"}},
			"lastSync": map[string]any{"time": "2026-10-12T03:10:00Z", "result": "Failed", "message": "listing failed"},
		},
		map[string]any{
			"name": "shop", "organization": "acme",
			"sources": []any{map[string]any{"provider": "github", "owner": "acme"}},
		},
	}}}
}

func providers(map[string]any) mustertest.Answer {
	return mustertest.Answer{Text: "2 providers", Structured: map[string]any{"providers": []any{
		map[string]any{"name": "github", "kind": "github", "connected": true, "connectURL": "https://wm.example/connect/github"},
		map[string]any{"name": "gitlab", "kind": "gitlab", "connected": false, "connectURL": "https://wm.example/connect/gitlab"},
	}}}
}

// authorize answers authorize_workspace by the workspace's name, one per
// outcome.
func authorize(args map[string]any) mustertest.Answer {
	name, _ := args["workspace"].(string)
	switch name {
	case "platform":
		return mustertest.Answer{Text: "Granted for workspace platform until 14:05:00 UTC: 2 repositories, 1 changed since the last sync.",
			Structured: map[string]any{"workspace": name, "outcome": "granted", "grant": grantToken, "expiresAt": expires.Format(time.RFC3339),
				"repositories": []any{"giantswarm/klaus-gateway", "giantswarm/muster"}, "changed": []any{"giantswarm/muster"}}}
	case "secret":
		return mustertest.Answer{Text: "you cannot read 1 repository of workspace secret: giantswarm/vault",
			Structured: map[string]any{"workspace": name, "outcome": "refused", "reason": "you cannot read 1 repository of workspace secret: giantswarm/vault",
				"repositories": []any{"giantswarm/klaus-gateway", "giantswarm/vault"}, "unreadable": []any{"giantswarm/vault"}}}
	case "mixed":
		return mustertest.Answer{Text: "connect gitlab to use workspace mixed: the Session works as you there",
			Structured: map[string]any{"workspace": name, "outcome": "connect_required", "reason": "connect gitlab to use workspace mixed: the Session works as you there",
				"repositories": []any{"infra/tools"}, "connect": []any{map[string]any{"provider": "gitlab", "url": "https://wm.example/connect/gitlab"}}}}
	case "fresh":
		return mustertest.Answer{Text: "workspace fresh has not synced yet: its repositories are not on its volume",
			Structured: map[string]any{"workspace": name, "outcome": "refused", "reason": "workspace fresh has not synced yet: its repositories are not on its volume"}}
	}
	return mustertest.Answer{IsError: true, Text: fmt.Sprintf("authorize workspace: workspaces.workspace-manager.giantswarm.io %q not found", name)}
}

func fakeMuster(t *testing.T) *mustertest.Server {
	t.Helper()
	srv := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		prefix + "list_workspaces":     listing,
		prefix + "list_providers":      providers,
		prefix + "authorize_workspace": authorize,
	})
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, srv *mustertest.Server) *Client {
	t.Helper()
	c := New(muster.NewClient(srv.URL), prefix)
	require.True(t, c.Enabled())
	return c
}

func TestList_AcrossTwoOrganizations(t *testing.T) {
	srv := fakeMuster(t)
	list, err := newClient(t, srv).List(context.Background(), bearer)
	require.NoError(t, err)
	require.Equal(t, []Workspace{
		{Name: "platform", Organization: "giantswarm", SyncedAt: time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC), Providers: []string{"github", "gitlab"}},
		{Name: "docs", Organization: "giantswarm", Providers: []string{"github"}},
		{Name: "shop", Organization: "acme", Providers: []string{"github"}},
	}, list, "a failed last cycle and no cycle yet both leave SyncedAt zero")

	calls := srv.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, mustertest.Call{Bearer: "Bearer " + bearer, Tool: prefix + "list_workspaces", Args: map[string]any{}}, calls[0], "the call runs as the person")
}

func TestProviders(t *testing.T) {
	list, err := newClient(t, fakeMuster(t)).Providers(context.Background(), bearer)
	require.NoError(t, err)
	require.Equal(t, []Provider{
		{Name: "github", Kind: "github", Connected: true, ConnectURL: "https://wm.example/connect/github"},
		{Name: "gitlab", Kind: "gitlab", ConnectURL: "https://wm.example/connect/gitlab"},
	}, list)
}

func TestAuthorize_TheFourAnswers(t *testing.T) {
	srv := fakeMuster(t)
	c := newClient(t, srv)
	ctx := context.Background()

	granted, err := c.Authorize(ctx, bearer, "platform")
	require.NoError(t, err)
	require.Equal(t, Granted, granted.Outcome)
	require.Equal(t, grantToken, granted.Grant.Token())
	require.Equal(t, expires, granted.ExpiresAt)
	require.Equal(t, []string{"giantswarm/klaus-gateway", "giantswarm/muster"}, granted.Repositories)
	require.Equal(t, []string{"giantswarm/muster"}, granted.Changed)

	refused, err := c.Authorize(ctx, bearer, "secret")
	require.NoError(t, err)
	require.Equal(t, Refused, refused.Outcome)
	require.Equal(t, []string{"giantswarm/vault"}, refused.Unreadable)
	require.True(t, refused.Grant.IsZero())

	connect, err := c.Authorize(ctx, bearer, "mixed")
	require.NoError(t, err)
	require.Equal(t, ConnectRequired, connect.Outcome)
	require.Equal(t, []Connect{{Provider: "gitlab", URL: "https://wm.example/connect/gitlab"}}, connect.Connect)

	notSynced, err := c.Authorize(ctx, bearer, "fresh")
	require.NoError(t, err)
	require.Equal(t, Unavailable, notSynced.Outcome)
	require.Contains(t, notSynced.Reason, "has not synced yet")

	_, err = c.Authorize(ctx, bearer, "elsewhere")
	var toolErr *ToolError
	require.ErrorAs(t, err, &toolErr, "a workspace the person may not see is the tool's refusal")
	require.Contains(t, toolErr.Message, "not found")

	for _, call := range srv.Calls() {
		require.Equal(t, "Bearer "+bearer, call.Bearer)
		require.Equal(t, prefix+"authorize_workspace", call.Tool)
	}
}

// The grant never reaches a log line, an error or the thread row.
func TestAuthorize_GrantNeverLeaves(t *testing.T) {
	var logs bytes.Buffer
	jsonLog := slog.New(slog.NewJSONHandler(&logs, nil))
	textLog := slog.New(slog.NewTextHandler(&logs, nil))

	c := newClient(t, fakeMuster(t))
	a, err := c.Authorize(context.Background(), bearer, "platform")
	require.NoError(t, err)

	for _, l := range []*slog.Logger{jsonLog, textLog} {
		l.Info("workspace authorized", "authorization", a, "grant", a.Grant)
		l.Info("workspace authorized", "authorization", *a)
	}
	fmt.Fprintf(&logs, "%v %+v %#v %s", a, *a, *a, a.Grant)
	require.NotContains(t, logs.String(), grantToken)
	require.Contains(t, logs.String(), grantRedacted)

	_, err = json.Marshal(a)
	require.Error(t, err, "an authorization carrying a grant cannot be serialised into a row")
	row, err := json.Marshal(store.Entry{Workspace: &store.WorkspaceChoice{Name: a.Workspace}})
	require.NoError(t, err)
	require.NotContains(t, string(row), grantToken, "the row records the choice, never the grant")

	// A malformed answer around a grant: the error names what is wrong, not
	// the answer.
	srv := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		prefix + "authorize_workspace": func(map[string]any) mustertest.Answer {
			return mustertest.Answer{Structured: map[string]any{"workspace": "platform", "outcome": "granted", "grant": grantToken}}
		},
	})
	defer srv.Close()
	_, err = New(muster.NewClient(srv.URL), prefix).Authorize(context.Background(), bearer, "platform")
	var malformed *MalformedError
	require.ErrorAs(t, err, &malformed)
	require.NotContains(t, err.Error(), grantToken)
}

func TestNoPrefix_NoCall(t *testing.T) {
	srv := fakeMuster(t)
	c := New(muster.NewClient(srv.URL), "")
	require.False(t, c.Enabled())

	_, err := c.List(context.Background(), bearer)
	require.ErrorIs(t, err, ErrDisabled)
	_, err = c.Providers(context.Background(), bearer)
	require.ErrorIs(t, err, ErrDisabled)
	_, err = c.Authorize(context.Background(), bearer, "platform")
	require.ErrorIs(t, err, ErrDisabled)
	require.Empty(t, srv.Calls(), "no workspace call is made")
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("missing tool", func(t *testing.T) {
		srv := mustertest.New(nil)
		defer srv.Close()
		_, err := New(muster.NewClient(srv.URL), prefix).List(ctx, bearer)
		var unavailable *ToolUnavailableError
		require.ErrorAs(t, err, &unavailable)
		require.Equal(t, prefix+"list_workspaces", unavailable.Tool)
	})
	t.Run("auth challenge", func(t *testing.T) {
		srv := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
			prefix + "list_providers": func(map[string]any) mustertest.Answer {
				return mustertest.Answer{IsError: true,
					Text:       "auth_required: server 'workspace-manager' requires authentication before its tools can be called (not connected).\n\nAuthentication Required\n\nServer: workspace-manager",
					Structured: map[string]any{"status": "auth_required", "server": "workspace-manager", "authUrl": "https://muster.example/login?s=1"}}
			},
		})
		defer srv.Close()
		_, err := New(muster.NewClient(srv.URL), prefix).Providers(ctx, bearer)
		var challenge *AuthRequiredError
		require.ErrorAs(t, err, &challenge)
		require.Equal(t, &AuthRequiredError{Server: "workspace-manager", LoginURL: "https://muster.example/login?s=1"}, challenge)
	})
	t.Run("malformed", func(t *testing.T) {
		for name, answer := range map[string]mustertest.Answer{
			"no structured content": {Text: "3 workspaces"},
			"wrong field type":      {Structured: map[string]any{"workspaces": "three"}},
			"no workspaces field":   {Structured: map[string]any{}},
			"nameless workspace":    {Structured: map[string]any{"workspaces": []any{map[string]any{"organization": "acme"}}}},
		} {
			srv := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
				prefix + "list_workspaces": func(map[string]any) mustertest.Answer { return answer },
			})
			_, err := New(muster.NewClient(srv.URL), prefix).List(ctx, bearer)
			srv.Close()
			var malformed *MalformedError
			require.ErrorAs(t, err, &malformed, name)
		}
	})
	t.Run("unknown outcome", func(t *testing.T) {
		srv := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
			prefix + "authorize_workspace": func(map[string]any) mustertest.Answer {
				return mustertest.Answer{Structured: map[string]any{"workspace": "w", "outcome": "maybe"}}
			},
		})
		defer srv.Close()
		_, err := New(muster.NewClient(srv.URL), prefix).Authorize(ctx, bearer, "w")
		var malformed *MalformedError
		require.ErrorAs(t, err, &malformed)
	})
	t.Run("muster unreachable", func(t *testing.T) {
		_, err := New(muster.NewClient("http://127.0.0.1:1"), prefix).List(ctx, bearer)
		require.Error(t, err)
	})
}
