// Package muster calls one muster tool as a person. Each call is its own MCP
// exchange against muster's /mcp endpoint, authenticated with the person's dex
// id_token, so muster sees the person and not the gateway and the tool runs
// under the person's own grants.
//
// The client declares URL mode elicitation. A call muster cannot run until
// the person does something at a link (sign in somewhere, approve the call)
// is answered with that URL elicitation (SignIn) instead of a tool result;
// the caller shows the link, and once the person is done there retries the
// call with Accept. muster asks on protocol 2026-07-28 with an input_required
// result, on 2025-11-25 with the JSON-RPC error -32042; both are read.
package muster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	defaultTimeout = 60 * time.Second
	maxResponse    = 1 << 20
	clientName     = "klaus-gateway"
)

// AcceptTimeout bounds Accept: muster waits up to five minutes for the
// sign-in before it runs the call.
const AcceptTimeout = 6 * time.Minute

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

// SignIn is a URL mode elicitation muster answered a call with: a sign-in, a
// tool approval or any later one, which the client does not tell apart. The
// person opens URL, exactly as given, after they agreed to; Accept then
// retries the call that asked for it.
type SignIn struct {
	// Message is muster's own words on what is asked, to show attributed to
	// muster.
	Message string
	// URL is the page the person acts on, as muster gave it; the caller adds
	// its return (ReturnLink).
	URL string
	// ElicitationID is muster's id of the elicitation, which muster appends
	// to the return after an approval; empty when muster gave none.
	ElicitationID string

	tool         string
	args         map[string]any
	inputKey     string
	requestState string
	// required is set for an elicitation muster asked with -32042: its retry
	// carries no answer, and a decline sends nothing.
	required bool
	// idempotent is set for an elicitation CallIdempotent got: the answer of
	// its accepted retry is handed to the next identical CallIdempotent.
	idempotent bool
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

	handoverMu sync.Mutex
	handovers  map[string]handover
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

// HandoverFor is how long the answer of an accepted retry of a CallIdempotent
// waits for the identical call it is handed to.
const HandoverFor = 2 * time.Minute

// handover is the answer of an accepted retry, waiting for the identical
// CallIdempotent.
type handover struct {
	res     Result
	expires time.Time
}

// CallIdempotent is CallTool for a tool whose call repeated with the same
// arguments answers the same (agent-manager's start_session, idempotent per
// request id), and that its caller makes again after an accepted retry ran
// it (a held message replayed after Done). The answer of that retry is handed,
// once and within HandoverFor, to the next CallIdempotent with the same bearer,
// tool and arguments, which does not call muster again: muster used the
// approval up when the retry ran, so the same call would ask a second time.
func (c *Client) CallIdempotent(ctx context.Context, bearer, tool string, args map[string]any) (Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	if res, ok := c.takeHandover(bearer, tool, args); ok {
		return res, nil
	}
	res, err := c.CallTool(ctx, bearer, tool, args)
	if res.SignIn != nil {
		res.SignIn.idempotent = true
	}
	return res, err
}

func handoverKey(bearer, tool string, args map[string]any) (string, bool) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(bearer + "\x00" + tool + "\x00" + string(encoded)))
	return hex.EncodeToString(sum[:]), true
}

func (c *Client) putHandover(bearer, tool string, args map[string]any, res Result) {
	key, ok := handoverKey(bearer, tool, args)
	if !ok {
		return
	}
	now := time.Now()
	c.handoverMu.Lock()
	defer c.handoverMu.Unlock()
	if c.handovers == nil {
		c.handovers = map[string]handover{}
	}
	for k, h := range c.handovers {
		if now.After(h.expires) {
			delete(c.handovers, k)
		}
	}
	c.handovers[key] = handover{res: res, expires: now.Add(HandoverFor)}
}

func (c *Client) takeHandover(bearer, tool string, args map[string]any) (Result, bool) {
	key, ok := handoverKey(bearer, tool, args)
	if !ok {
		return Result{}, false
	}
	c.handoverMu.Lock()
	defer c.handoverMu.Unlock()
	h, found := c.handovers[key]
	delete(c.handovers, key)
	if !found || time.Now().After(h.expires) {
		return Result{}, false
	}
	return h.res, true
}

// Accept retries the call signIn was asked for with the person's accept: they
// agreed to open the link. muster waits for the sign-in to complete before it
// runs the call; a sign-in that did not complete in time is asked again, as a
// Result with a new SignIn.
func (c *Client) Accept(ctx context.Context, bearer string, signIn *SignIn) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, AcceptTimeout)
	defer cancel()
	return c.answer(ctx, bearer, signIn, mcp.ElicitationResponseActionAccept)
}

// Decline answers the call signIn was asked for with the person's decline:
// they will not open the link. muster runs nothing and answers with a tool
// error, which is the expected outcome and not returned as an error.
func (c *Client) Decline(ctx context.Context, bearer string, signIn *SignIn) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	_, err := c.answer(ctx, bearer, signIn, mcp.ElicitationResponseActionDecline)
	return err
}

func (c *Client) answer(ctx context.Context, bearer string, signIn *SignIn, action mcp.ElicitationResponseAction) (Result, error) {
	if signIn == nil || signIn.tool == "" {
		return Result{}, errors.New("muster: no sign-in to answer")
	}
	res, err := c.retry(ctx, bearer, signIn, action)
	if err == nil && action == mcp.ElicitationResponseActionAccept && signIn.idempotent {
		switch {
		case res.SignIn != nil:
			res.SignIn.idempotent = true
		case res.Dispatched && !res.IsError:
			c.putHandover(bearer, signIn.tool, signIn.args, res)
		}
	}
	return res, err
}

// retry calls the call signIn was asked for again with the person's action.
func (c *Client) retry(ctx context.Context, bearer string, signIn *SignIn, action mcp.ElicitationResponseAction) (Result, error) {
	if signIn.required {
		// -32042 has no answer to send: the retry is the call again, which
		// muster matches server-side, and a decline is not calling again.
		if action != mcp.ElicitationResponseActionAccept {
			return Result{}, nil
		}
		return c.call(ctx, bearer, signIn.tool, signIn.args, mcp.MultiRoundTripParams{})
	}
	answer := mcp.NewElicitationInputResponse(mcp.ElicitationResult{
		ElicitationResponse: mcp.ElicitationResponse{Action: action},
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
	if required, ok := asURLElicitationRequired(err); ok {
		signIn, err := signInOfRequired(required, tool, args)
		if err != nil {
			return Result{}, fmt.Errorf("muster: call %s: %w", tool, err)
		}
		return Result{SignIn: signIn}, nil
	}
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
	bounded := *httpClient
	bounded.Transport = musterTransport{next: httpClient.Transport}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/mcp"
	t, err := transport.NewStreamableHTTP(endpoint,
		transport.WithHTTPBasicClient(&bounded),
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + bearer}),
	)
	if err != nil {
		return nil, fmt.Errorf("muster: transport: %w", err)
	}
	// mcp-go reads a server/discover slower than its probe bound as a server
	// older than 2026-07-28 and falls back to it, where a sign-in is no URL
	// elicitation this client reads; the call's own deadline bounds it.
	session := mcpclient.NewClient(t,
		mcpclient.WithDiscoverTimeout(-1),
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
	_, err = session.Initialize(ctx, initialize)
	if legacyOnly(err) {
		initialize.Params.ProtocolVersion = mcp.LATEST_LEGACY_PROTOCOL_VERSION
		_, err = session.Initialize(ctx, initialize)
	}
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("muster: initialize: %w", err)
	}
	return session, nil
}

// legacyOnly reports a server/discover refusal whose supported versions end
// at the newest revision the initialize handshake negotiates. mcp-go stops
// there instead of falling back to initialize, so the caller pins that
// revision and initializes again.
func legacyOnly(err error) bool {
	var unsupported mcp.UnsupportedProtocolVersionError
	return errors.As(err, &unsupported) && slices.Contains(unsupported.Supported, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
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

// asURLElicitationRequired finds the -32042 error muster answers a 2025-11-25
// session with.
func asURLElicitationRequired(err error) (mcp.URLElicitationRequiredError, bool) {
	var required mcp.URLElicitationRequiredError
	if err != nil && errors.As(err, &required) {
		return required, true
	}
	return required, false
}

// signInOfRequired reads the one URL mode elicitation a -32042 error carries.
func signInOfRequired(required mcp.URLElicitationRequiredError, tool string, args map[string]any) (*SignIn, error) {
	if len(required.Elicitations) != 1 {
		return nil, fmt.Errorf("%w: %d elicitations", ErrUnusableSignIn, len(required.Elicitations))
	}
	e := required.Elicitations[0]
	if e.Mode != mcp.ElicitationModeURL {
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
		required:      true,
	}, nil
}

// ReturnLinkParam is the query parameter of muster's link that names where
// muster sends the browser after the person acted there, checked against
// muster's oauth.mcpClient.postLoginRedirectAllowlist (a prefix match that
// leaves the query free). muster keeps the redirect's query and appends to it
// the elicitation's elicitationId after an approval, the server's name after
// a sign-in; never what was decided.
const ReturnLinkParam = "redirect"

// ReturnLink sets returnURL as the redirect of link when link is on the
// origin of musterURL (scheme, host and port, a default port written or
// not). A redirect a link on muster's origin already names is never kept,
// since it was not the caller's: it is replaced, or dropped when returnURL is
// empty. Any other link is returned as given.
func ReturnLink(link, musterURL, returnURL string) string {
	if musterURL == "" {
		return link
	}
	m, err := url.Parse(musterURL)
	if err != nil || m.Host == "" {
		return link
	}
	u, err := url.Parse(link)
	if err != nil || !sameOrigin(u, m) {
		return link
	}
	q := u.Query()
	if !q.Has(ReturnLinkParam) && returnURL == "" {
		return link
	}
	q.Del(ReturnLinkParam)
	if returnURL != "" {
		q.Set(ReturnLinkParam, returnURL)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// sameOrigin reports whether a and b share scheme, host and port, a scheme's
// default port written or left out alike.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		originPort(a) == originPort(b)
}

// originPort is u's port, its scheme's default when none is written.
func originPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// SignInURLMaxBytes bounds a sign-in link as a client writes it in markup,
// each & as &amp;, so that the client can always show it whole next to the
// server's message.
const SignInURLMaxBytes = 2000

// ValidateSignInURL accepts a link the person may be asked to open: absolute
// https, or http on a loopback host, without credentials in it, at most
// SignInURLMaxBytes long written in markup, and nothing a client could not
// show exactly as written: spaces, control and format characters
// (bidirectional overrides and zero-width characters reorder or hide what is
// shown), backticks, and the < and > that RFC 3986 leaves out of a URL.
func ValidateSignInURL(link string) error {
	if len(link)+len("amp;")*strings.Count(link, "&") > SignInURLMaxBytes {
		return fmt.Errorf("%w: the URL is longer than %d bytes", ErrUnusableSignIn, SignInURLMaxBytes)
	}
	if strings.ContainsFunc(link, unshowable) {
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

func unshowable(r rune) bool {
	switch r {
	case '`', '<', '>':
		return true
	}
	return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError
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

// musterTransport turns a 401 or 403 answer into an HTTPError, so a refused
// token is told apart from any other failure, and reads at most maxResponse
// bytes of any other answer: a longer one fails to decode.
type musterTransport struct {
	next http.RoundTripper
}

func (t musterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		resp.Body = limitedBody{Reader: io.LimitReader(resp.Body, maxResponse), Closer: resp.Body}
		return resp, nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
}

type limitedBody struct {
	io.Reader
	io.Closer
}
