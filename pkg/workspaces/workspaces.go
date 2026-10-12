// Package workspaces asks the workspace-manager, through muster and as the
// person, which workspaces they may pick and whether they may start a Session
// on one. The workspace-manager is an MCP server behind muster; its tools are
// reached by their aggregated names, the configured prefix joined with the
// tool's own name (x_workspace-manager_list_workspaces).
//
// A Client without a prefix is off: every method answers ErrDisabled without
// calling muster, so a gateway that does not configure workspaces never asks.
package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// The workspace-manager's tools, by their own names.
const (
	toolListWorkspaces     = "list_workspaces"
	toolAuthorizeWorkspace = "authorize_workspace"
	toolListProviders      = "list_providers"
)

// ToolCaller calls a muster tool as the person whose bearer token it is
// given. *muster.Client satisfies it.
type ToolCaller interface {
	CallTool(ctx context.Context, bearer, tool string, args map[string]any) (muster.Result, error)
}

// Client calls the workspace-manager's tools. A nil Client, or one without a
// prefix, is off.
type Client struct {
	tools  ToolCaller
	prefix string
}

// New returns a Client that calls the workspace-manager's tools through tools,
// named prefix + tool. An empty prefix returns nil: workspaces are off.
func New(tools ToolCaller, prefix string) *Client {
	if prefix == "" || tools == nil {
		return nil
	}
	return &Client{tools: tools, prefix: prefix}
}

// Enabled reports whether workspaces are configured.
func (c *Client) Enabled() bool {
	return c != nil
}

// Workspace is one workspace the person may pick.
type Workspace struct {
	Name         string
	Organization string
	// SyncedAt is when the last sync cycle that brought the volume up to date
	// ended (the workspace-manager's Succeeded or NoChange). Zero when no cycle
	// has, and when the last cycle failed: the listing reports the last cycle
	// only.
	SyncedAt time.Time
	// Providers are the provider instances the workspace's sources are on, the
	// ones the person has to be connected to; sorted, each once.
	Providers []string
	// Deleting is true once the workspace is being deleted: no Session starts
	// on it.
	Deleting bool
}

// List returns the workspaces of every Organization the person belongs to.
func (c *Client) List(ctx context.Context, bearer string) ([]Workspace, error) {
	var out struct {
		Workspaces *[]struct {
			Name         string `json:"name"`
			Organization string `json:"organization"`
			Sources      []struct {
				Provider string `json:"provider"`
			} `json:"sources"`
			Deleting bool `json:"deleting"`
			LastSync *struct {
				Time   time.Time `json:"time"`
				Result string    `json:"result"`
			} `json:"lastSync"`
		} `json:"workspaces"`
	}
	if err := c.call(ctx, bearer, toolListWorkspaces, nil, &out); err != nil {
		return nil, err
	}
	if out.Workspaces == nil {
		return nil, &MalformedError{Tool: c.prefix + toolListWorkspaces, Reason: "no workspaces field"}
	}
	list := make([]Workspace, 0, len(*out.Workspaces))
	for _, w := range *out.Workspaces {
		if w.Name == "" {
			return nil, &MalformedError{Tool: c.prefix + toolListWorkspaces, Reason: "a workspace without a name"}
		}
		ws := Workspace{Name: w.Name, Organization: w.Organization, Deleting: w.Deleting}
		if ls := w.LastSync; ls != nil && (ls.Result == "Succeeded" || ls.Result == "NoChange") {
			ws.SyncedAt = ls.Time
		}
		for _, s := range w.Sources {
			if s.Provider != "" && !slices.Contains(ws.Providers, s.Provider) {
				ws.Providers = append(ws.Providers, s.Provider)
			}
		}
		slices.Sort(ws.Providers)
		list = append(list, ws)
	}
	return list, nil
}

// Provider is one of the installation's workspace providers and the person's
// connection to it.
type Provider struct {
	Name      string
	Kind      string
	Connected bool
	// ConnectURL is the page where the person connects it.
	ConnectURL string
}

// Providers lists the installation's workspace providers, each with whether
// the person is connected to it.
func (c *Client) Providers(ctx context.Context, bearer string) ([]Provider, error) {
	var out struct {
		Providers *[]struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			Connected  bool   `json:"connected"`
			ConnectURL string `json:"connectURL"`
		} `json:"providers"`
	}
	if err := c.call(ctx, bearer, toolListProviders, nil, &out); err != nil {
		return nil, err
	}
	if out.Providers == nil {
		return nil, &MalformedError{Tool: c.prefix + toolListProviders, Reason: "no providers field"}
	}
	list := make([]Provider, 0, len(*out.Providers))
	for _, p := range *out.Providers {
		if p.Name == "" {
			return nil, &MalformedError{Tool: c.prefix + toolListProviders, Reason: "a provider without a name"}
		}
		list = append(list, Provider{Name: p.Name, Kind: p.Kind, Connected: p.Connected, ConnectURL: p.ConnectURL})
	}
	return list, nil
}

// Outcome is how an authorization ended.
type Outcome int

const (
	// Granted: the person reads every repository of the workspace;
	// Authorization.Grant is for start_session.
	Granted Outcome = iota + 1
	// Refused: the person cannot read the repositories in
	// Authorization.Unreadable.
	Refused
	// ConnectRequired: the person has not connected the providers in
	// Authorization.Connect yet.
	ConnectRequired
	// Unavailable: no Session can start on the workspace yet, whoever asks:
	// it has not synced, it is being deleted, its volume is not ready.
	// Authorization.Reason says which.
	Unavailable
)

func (o Outcome) String() string {
	switch o {
	case Granted:
		return "granted"
	case Refused:
		return "refused"
	case ConnectRequired:
		return "connect_required"
	case Unavailable:
		return "unavailable"
	}
	return "unknown"
}

// Connect is a provider the person connects before the workspace is theirs.
type Connect struct {
	Provider string
	URL      string
}

// Authorization is the workspace-manager's answer whether the person may start
// a Session on a workspace.
type Authorization struct {
	Workspace string
	Outcome   Outcome
	// Reason says why in a sentence, for every outcome but Granted.
	Reason string
	// Grant is the short-lived grant start_session takes; set when Granted.
	Grant Grant
	// ExpiresAt is when the grant expires.
	ExpiresAt time.Time
	// Repositories are the workspace's repositories, `<owner>/<name>`, and
	// Changed those pushed to since its last sync; set when Granted.
	Repositories []string
	Changed      []string
	// Unreadable are the repositories the person cannot read; set when
	// Refused.
	Unreadable []string
	// Connect are the providers to connect first; set when ConnectRequired.
	Connect []Connect
}

// Authorize asks whether the person may start a Session on the workspace.
// Every answer about the person's access is an Authorization; an error is a
// failure to ask (the workspace unknown or not the person's Organization's,
// muster's sign-in challenge, an unreachable or malformed tool).
func (c *Client) Authorize(ctx context.Context, bearer, workspace string) (*Authorization, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if workspace == "" {
		return nil, errors.New("workspaces: authorize: no workspace")
	}
	var out struct {
		Workspace    string     `json:"workspace"`
		Outcome      string     `json:"outcome"`
		Reason       string     `json:"reason"`
		Grant        string     `json:"grant"`
		ExpiresAt    *time.Time `json:"expiresAt"`
		Repositories []string   `json:"repositories"`
		Changed      []string   `json:"changed"`
		Unreadable   []string   `json:"unreadable"`
		Connect      []struct {
			Provider string `json:"provider"`
			URL      string `json:"url"`
		} `json:"connect"`
	}
	if err := c.call(ctx, bearer, toolAuthorizeWorkspace, map[string]any{"workspace": workspace}, &out); err != nil {
		return nil, err
	}
	tool := c.prefix + toolAuthorizeWorkspace
	a := &Authorization{Workspace: workspace, Reason: out.Reason}
	switch out.Outcome {
	case "granted":
		if out.Grant == "" || out.ExpiresAt == nil {
			return nil, &MalformedError{Tool: tool, Reason: "granted without a grant or its expiry"}
		}
		a.Outcome, a.Grant, a.ExpiresAt = Granted, Grant{token: out.Grant}, *out.ExpiresAt
		a.Repositories, a.Changed = out.Repositories, out.Changed
	case "connect_required":
		if len(out.Connect) == 0 {
			return nil, &MalformedError{Tool: tool, Reason: "connect_required without a provider"}
		}
		a.Outcome = ConnectRequired
		for _, p := range out.Connect {
			a.Connect = append(a.Connect, Connect{Provider: p.Provider, URL: p.URL})
		}
	case "refused":
		// A refusal names the unreadable repositories, or none when the
		// workspace itself cannot be started on yet.
		a.Outcome, a.Unreadable = Refused, out.Unreadable
		if len(out.Unreadable) == 0 {
			a.Outcome = Unavailable
		}
	default:
		return nil, &MalformedError{Tool: tool, Reason: fmt.Sprintf("unknown outcome %q", out.Outcome)}
	}
	return a, nil
}

// Grant is a workspace grant: opaque, short-lived, for start_session alone. It
// never reaches a log line, an error, a message or a store: it prints, logs
// and marshals as a placeholder, and JSON refuses it.
type Grant struct {
	token string
}

const grantRedacted = "[workspace grant]"

// Token is the grant itself, for start_session's argument.
func (g Grant) Token() string { return g.token }

// IsZero reports whether there is no grant.
func (g Grant) IsZero() bool { return g.token == "" }

func (g Grant) String() string   { return grantRedacted }
func (g Grant) GoString() string { return grantRedacted }

// LogValue keeps the grant out of every slog record.
func (g Grant) LogValue() slog.Value { return slog.StringValue(grantRedacted) }

// MarshalJSON refuses: a grant is never stored, so a row that carries one
// fails to write instead of persisting it.
func (g Grant) MarshalJSON() ([]byte, error) {
	return nil, errors.New("workspaces: a grant is never serialised")
}

// MarshalText refuses like MarshalJSON.
func (g Grant) MarshalText() ([]byte, error) {
	return nil, errors.New("workspaces: a grant is never serialised")
}

// ErrDisabled is every answer of a Client that has no tools prefix.
var ErrDisabled = errors.New("workspaces: not configured")

// ToolUnavailableError is muster refusing the call itself: no such tool, or
// one outside the person's toolset. The workspace-manager is not behind
// muster under the configured prefix.
type ToolUnavailableError struct {
	Tool    string
	Message string
}

func (e *ToolUnavailableError) Error() string {
	return fmt.Sprintf("workspaces: tool %s unavailable: %s", e.Tool, e.Message)
}

// ToolError is the workspace-manager refusing the call: the workspace unknown
// or not the person's Organization's, a failure on its side. Message is the
// tool's own.
type ToolError struct {
	Tool    string
	Message string
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("workspaces: %s: %s", e.Tool, e.Message)
}

// MalformedError is an answer that is not the tool's documented result.
type MalformedError struct {
	Tool   string
	Reason string
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("workspaces: %s answered a malformed result: %s", e.Tool, e.Reason)
}

// AuthRequiredError is muster's sign-in challenge: it holds no grant for the
// person at the workspace-manager yet. LoginURL is where the person signs in;
// the call succeeds once they have.
type AuthRequiredError struct {
	Server   string
	LoginURL string
}

func (e *AuthRequiredError) Error() string {
	return fmt.Sprintf("workspaces: sign-in to %s required", e.Server)
}

// call runs the tool as the person and decodes its structured result into
// out. Its errors never carry the result's content, which may hold a grant.
func (c *Client) call(ctx context.Context, bearer, tool string, args map[string]any, out any) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	name := c.prefix + tool
	res, err := c.tools.CallTool(ctx, bearer, name, args)
	if err != nil {
		return fmt.Errorf("workspaces: %s: %w", name, err)
	}
	if challenge := authChallengeOf(res); challenge != nil {
		return challenge
	}
	if res.IsError {
		if !res.Dispatched {
			return &ToolUnavailableError{Tool: name, Message: res.Text}
		}
		return &ToolError{Tool: name, Message: res.Text}
	}
	if len(res.Structured) == 0 {
		return &MalformedError{Tool: name, Reason: "no structured content"}
	}
	if err := json.Unmarshal(res.Structured, out); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return &MalformedError{Tool: name, Reason: fmt.Sprintf("field %s is not a %s", typeErr.Field, typeErr.Type)}
		}
		return &MalformedError{Tool: name, Reason: "not a JSON object"}
	}
	return nil
}

// authChallengeOf reads muster's auth_required answer: an error result whose
// structured content names the server and the sign-in link (muster puts the
// link both there and in the text, which the Slack adapter's tool calls
// parse).
func authChallengeOf(res muster.Result) *AuthRequiredError {
	if !res.IsError {
		return nil
	}
	var challenge struct {
		Status  string `json:"status"`
		Server  string `json:"server"`
		AuthURL string `json:"authUrl"`
	}
	if len(res.Structured) > 0 && json.Unmarshal(res.Structured, &challenge) == nil && challenge.Status == "auth_required" {
		return &AuthRequiredError{Server: challenge.Server, LoginURL: challenge.AuthURL}
	}
	if strings.HasPrefix(res.Text, "auth_required") {
		return &AuthRequiredError{}
	}
	return nil
}
