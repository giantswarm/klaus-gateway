// Package muster calls one muster tool as a person. Each call is its own MCP
// exchange against muster's /mcp endpoint, authenticated with the person's dex
// id_token, so muster sees the person and not the gateway and the tool runs
// under the person's own grants.
//
// The client declares URL mode elicitation. A call muster cannot run until
// the person signs in somewhere is answered with that sign-in as a URL
// elicitation (SignIn) instead of a tool result; the caller shows the link,
// and once the person agreed to open it retries the call with Accept.
package muster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	defaultTimeout = 60 * time.Second
	// acceptTimeout bounds a retry carrying the person's accept: muster waits
	// up to five minutes for the sign-in before it runs the call.
	acceptTimeout = 6 * time.Minute
	clientName    = "klaus-gateway"
)

// Result is a tool's answer: its text content joined, its structured content
// as the tool sent it, and whether the tool reported it as an error (a refusal
// is an error result, not a transport failure). Dispatched tells the target
// tool's own answer from call_tool's: false is muster refusing the call itself
// (an unknown tool, one outside the caller's toolset), the tool never ran.
//
// SignIn, when set, is the sign-in muster needs before it runs the call: the
// call did not run, and the other fields are empty.
type Result struct {
	Text       string
	Structured json.RawMessage
	IsError    bool
	Dispatched bool
	SignIn     *SignIn
}

// SignIn is a URL mode elicitation muster answered a call with. The person
// opens URL, exactly as given, after they agreed to; Accept then retries the
// call that asked for it.
type SignIn struct {
	// Message is muster's own words on why, to show attributed to muster.
	Message string
	// URL is the page the person signs in on.
	URL string
	// ElicitationID names the elicitation; the same sign-in asked again keeps
	// it.
	ElicitationID string

	tool         string
	args         map[string]any
	inputKey     string
	requestState string
}

// ErrUnusableSignIn is a sign-in muster asked for that the gateway will not
// show: not one URL mode elicitation, or a link that is not https (http only
// on a loopback host) or that carries credentials.
var ErrUnusableSignIn = errors.New("muster: unusable sign-in elicitation")

// Client calls muster tools. One client is safe to share.
type Client struct {
	// BaseURL is muster's base URL; the MCP endpoint is BaseURL + /mcp.
	BaseURL string
	// HTTP carries the requests; each call bounds itself with its context.
	HTTP *http.Client
}

// NewClient returns a Client for the muster at baseURL.
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{}}
}

// CallTool calls tool with args as the person whose bearer token is given.
func (c *Client) CallTool(ctx context.Context, bearer, tool string, args map[string]any) (Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	return c.call(ctx, bearer, tool, args, mcp.MultiRoundTripParams{})
}

// Accept retries the call signIn was asked for with the person's accept: they
// agreed to open the link. muster waits for the sign-in to complete before it
// runs the call; a sign-in that did not complete in time is asked again, as a
// Result with a new SignIn.
func (c *Client) Accept(ctx context.Context, bearer string, signIn *SignIn) (Result, error) {
	if signIn == nil || signIn.tool == "" {
		return Result{}, errors.New("muster: no sign-in to accept")
	}
	ctx, cancel := context.WithTimeout(ctx, acceptTimeout)
	defer cancel()
	answer := mcp.NewElicitationInputResponse(mcp.ElicitationResult{
		ElicitationResponse: mcp.ElicitationResponse{Action: mcp.ElicitationResponseActionAccept},
	})
	return c.call(ctx, bearer, signIn.tool, signIn.args, mcp.MultiRoundTripParams{
		InputResponses: mcp.InputResponses{signIn.inputKey: answer},
		RequestState:   signIn.requestState,
	})
}

func (c *Client) call(ctx context.Context, bearer, tool string, args map[string]any, roundTrip mcp.MultiRoundTripParams) (Result, error) {
	if bearer == "" {
		return Result{}, errors.New("muster: no bearer token for the tool call")
	}
	session, err := c.connect(ctx, bearer)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = session.Close() }()

	// muster exposes every aggregated server tool (x_<server>_<tool>) and every
	// core tool through its call_tool meta-tool, not under its own name: the
	// target's result comes back serialised in the meta-tool's text content,
	// its isError verdict inside.
	request := mcp.CallToolRequest{}
	request.Params.Name = metaCallTool
	request.Params.Arguments = map[string]any{nameKey: tool, argumentsKey: args}
	request.Params.MultiRoundTripParams = roundTrip
	res, err := session.CallTool(ctx, request)
	if err != nil {
		return Result{}, fmt.Errorf("muster: call %s: %w", tool, err)
	}
	if res.NeedsInput() {
		signIn, err := signInOf(res, tool, args)
		if err != nil {
			return Result{}, fmt.Errorf("muster: call %s: %w", tool, err)
		}
		return Result{SignIn: signIn}, nil
	}
	outer := flatten(res.Content, res.IsError)
	if inner, ok := unwrapEnvelope(outer); ok {
		inner.Dispatched = true
		return inner, nil
	}
	// call_tool's own refusal (an unknown tool, one outside the caller's
	// toolset) is plain text with the meta-tool's isError set; a result that
	// is no envelope is handed back as it is.
	return outer, nil
}

// connect opens an MCP client to muster as the person, declaring URL mode
// elicitation and answering input requests by hand.
func (c *Client) connect(ctx context.Context, bearer string) (*mcpclient.Client, error) {
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	refusing := *httpClient
	refusing.Transport = refusedTokenTransport{next: httpClient.Transport}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/mcp"
	t, err := transport.NewStreamableHTTP(endpoint,
		transport.WithHTTPBasicClient(&refusing),
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + bearer}),
	)
	if err != nil {
		return nil, fmt.Errorf("muster: transport: %w", err)
	}
	session := mcpclient.NewClient(t,
		mcpclient.WithManualInputRoundTrips(),
		mcpclient.WithClientCapabilities(mcp.ClientCapabilities{
			Elicitation: &mcp.ElicitationCapability{URL: &struct{}{}},
		}),
	)
	if err := session.Start(ctx); err != nil {
		return nil, fmt.Errorf("muster: start: %w", err)
	}
	initialize := mcp.InitializeRequest{}
	initialize.Params.ClientInfo = mcp.Implementation{Name: clientName}
	if _, err := session.Initialize(ctx, initialize); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("muster: initialize: %w", err)
	}
	return session, nil
}

// signInOf reads the one URL mode elicitation an input_required answer
// carries.
func signInOf(res *mcp.CallToolResult, tool string, args map[string]any) (*SignIn, error) {
	if len(res.InputRequests) != 1 {
		return nil, fmt.Errorf("%w: %d input requests", ErrUnusableSignIn, len(res.InputRequests))
	}
	for key, request := range res.InputRequests {
		e := request.Elicitation
		if request.Method != mcp.MethodElicitationCreate || e == nil || e.Mode != mcp.ElicitationModeURL {
			return nil, fmt.Errorf("%w: not a URL elicitation", ErrUnusableSignIn)
		}
		if err := ValidateSignInURL(e.URL); err != nil {
			return nil, err
		}
		return &SignIn{
			Message:       e.Message,
			URL:           e.URL,
			ElicitationID: e.ElicitationID,
			tool:          tool,
			args:          args,
			inputKey:      key,
			requestState:  res.RequestState,
		}, nil
	}
	return nil, ErrUnusableSignIn
}

// ValidateSignInURL accepts a link the person may be asked to open: absolute
// https, or http on a loopback host, without credentials in it, and nothing a
// client could not show exactly as written (spaces, control characters,
// backticks).
func ValidateSignInURL(link string) error {
	if strings.ContainsFunc(link, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '`' }) {
		return fmt.Errorf("%w: the URL cannot be shown as written", ErrUnusableSignIn)
	}
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: not an absolute URL", ErrUnusableSignIn)
	}
	if u.User != nil {
		return fmt.Errorf("%w: the URL carries credentials", ErrUnusableSignIn)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return fmt.Errorf("%w: scheme %q", ErrUnusableSignIn, u.Scheme)
}

// metaCallTool is muster's meta-tool that runs any aggregated or core tool;
// nameKey and argumentsKey are the tools/call parameter names it mirrors.
const (
	metaCallTool = "call_tool"
	nameKey      = "name"
	argumentsKey = "arguments"
)

// envelope is the target tool's result as call_tool serialises it.
type envelope struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

// unwrapEnvelope reads the target tool's own result out of call_tool's text.
func unwrapEnvelope(outer Result) (Result, bool) {
	var env envelope
	if err := json.Unmarshal([]byte(outer.Text), &env); err != nil || len(env.Content) == 0 {
		return Result{}, false
	}
	var texts []string
	for _, part := range env.Content {
		if part.Type == "text" && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	res := Result{Text: strings.Join(texts, "\n"), IsError: env.IsError}
	if len(env.StructuredContent) > 0 && string(env.StructuredContent) != "null" {
		res.Structured = env.StructuredContent
	}
	return res, true
}

func flatten(content []mcp.Content, isError bool) Result {
	var texts []string
	for _, part := range content {
		if text, ok := mcp.AsTextContent(part); ok && text.Text != "" {
			texts = append(texts, text.Text)
		}
	}
	return Result{Text: strings.Join(texts, "\n"), IsError: isError}
}

// HTTPError is muster refusing the person's token at its MCP endpoint (401 or
// 403).
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("status %d", e.Status)
	}
	return fmt.Sprintf("status %d: %s", e.Status, e.Body)
}

// refusedTokenTransport turns a 401 or 403 answer into an HTTPError, so a
// refused token is told apart from any other failure.
type refusedTokenTransport struct {
	next http.RoundTripper
}

func (t refusedTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
}
