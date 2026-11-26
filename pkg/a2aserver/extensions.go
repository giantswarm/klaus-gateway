package a2aserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// ToolCallMetadataExtensionURI is kagent's A2A extension for request-scoped MCP
// tool-call metadata. Its request metadata entry is an object keyed by
// metadata key; the agent runtime sends the payload in tool calls to servers
// declaring kagent's MCP extension dev.kagent/tool-call-metadata.
const ToolCallMetadataExtensionURI = "https://kagent.dev/extensions/mcp-tool-call-metadata/v1"

var toolCallMetadataExtension = &a2a.AgentExtension{URI: ToolCallMetadataExtensionURI}

func toolCallMetadataCardExtension() a2a.AgentExtension {
	return a2a.AgentExtension{
		URI:         ToolCallMetadataExtensionURI,
		Description: "Request-scoped tool-call metadata for MCP servers that opt in",
	}
}

type toolCallMetadataInterceptor struct {
	a2asrv.PassthroughCallInterceptor
}

func (*toolCallMetadataInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	send, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || send == nil || !callCtx.Extensions().Requested(toolCallMetadataExtension) {
		return ctx, nil, nil
	}
	if raw, present := send.Metadata[ToolCallMetadataExtensionURI]; present && raw != nil {
		if _, ok := raw.(map[string]any); !ok {
			return ctx, nil, fmt.Errorf("%w: metadata %s must be an object", a2a.ErrInvalidParams, ToolCallMetadataExtensionURI)
		}
	}
	activate(ctx, toolCallMetadataExtension)
	return ctx, nil, nil
}

// toolCallMetadataPropagator passes the MCP tool-call metadata extension on to the
// agent runtime when the interceptor activated it: its request metadata entry
// and its A2A-Extensions activation (a2a-go's propagation, read by the kagent
// client's propagator). No other request metadata key or extension is passed
// on.
func toolCallMetadataPropagator() a2asrv.CallInterceptor {
	return a2aext.NewServerPropagator(&a2aext.ServerPropagatorConfig{
		MetadataPredicate: func(ctx context.Context, key string) bool {
			return key == ToolCallMetadataExtensionURI && toolCallMetadataActive(ctx)
		},
		HeaderPredicate: func(ctx context.Context, key string) bool {
			return strings.EqualFold(key, a2a.SvcParamExtensions) && toolCallMetadataActive(ctx)
		},
	})
}

func toolCallMetadataActive(ctx context.Context) bool {
	extensions, ok := a2asrv.ExtensionsFrom(ctx)
	return ok && extensions.Active(toolCallMetadataExtension)
}

type activationKey struct{}

// activation is the call context whose activated extensions a response
// names in its A2A-Extensions header.
type activation struct {
	mu      sync.Mutex
	callCtx *a2asrv.CallContext
}

// activate marks extension activated in ctx's call and records the call for
// EchoExtensions.
func activate(ctx context.Context, extension *a2a.AgentExtension) {
	callCtx, ok := a2asrv.CallContextFrom(ctx)
	if !ok {
		return
	}
	held, _ := ctx.Value(activationKey{}).(*activation)
	if held == nil {
		callCtx.Extensions().Activate(extension)
		return
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	callCtx.Extensions().Activate(extension)
	held.callCtx = callCtx
}

func (a *activation) uris() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.callCtx == nil {
		return nil
	}
	return a.callCtx.Extensions().ActivatedURIs()
}

// EchoExtensions names the extensions the call activated in the response's
// A2A-Extensions header, before the response's first byte. The a2asrv
// JSON-RPC transport does not; its gRPC transport sends them as a trailer.
func EchoExtensions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held := &activation{}
		next.ServeHTTP(&echoWriter{ResponseWriter: w, held: held}, r.WithContext(context.WithValue(r.Context(), activationKey{}, held)))
	})
}

type echoWriter struct {
	http.ResponseWriter
	held    *activation
	written bool
}

func (w *echoWriter) echo() {
	if w.written {
		return
	}
	w.written = true
	if uris := w.held.uris(); len(uris) > 0 {
		w.Header().Set(a2a.SvcParamExtensions, strings.Join(uris, ", "))
	}
}

func (w *echoWriter) WriteHeader(code int) {
	w.echo()
	w.ResponseWriter.WriteHeader(code)
}

func (w *echoWriter) Write(b []byte) (int, error) {
	w.echo()
	return w.ResponseWriter.Write(b)
}

// Flush lets the streaming transport flush each event.
func (w *echoWriter) Flush() {
	w.echo()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap gives http.ResponseController the underlying writer.
func (w *echoWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// NewRequestHandler serves h with the A2A server's call interceptors: the MCP
// tool-call metadata payload is checked and the extension is activated before
// the propagator reads its activation.
func NewRequestHandler(h *Handler) a2asrv.RequestHandler {
	return &a2asrv.InterceptedHandler{
		Handler:      h,
		Interceptors: []a2asrv.CallInterceptor{&toolCallMetadataInterceptor{}, toolCallMetadataPropagator()},
		Logger:       h.Logger,
	}
}
