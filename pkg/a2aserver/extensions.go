package a2aserver

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// ToolCallMetadataExtensionURI is kagent's A2A extension for request-scoped MCP
// tool-call metadata. Its request metadata entry is an object keyed by
// metadata key; the agent runtime sends the values a server accepts in that
// server's tool calls, under kagent's MCP extension dev.kagent/tool-call-metadata.
// The card's params name the keys the gateway accepts.
const ToolCallMetadataExtensionURI = "https://kagent.dev/extensions/mcp-tool-call-metadata/v1"

// toolCallMetadataParamKeys is the params key listing the accepted metadata
// keys.
const toolCallMetadataParamKeys = "keys"

var toolCallMetadataExtension = &a2a.AgentExtension{URI: ToolCallMetadataExtensionURI}

// toolCallMetadataCardExtension declares the MCP tool-call metadata extension,
// accepting keys.
func toolCallMetadataCardExtension(ids []string) a2a.AgentExtension {
	return a2a.AgentExtension{
		URI:         ToolCallMetadataExtensionURI,
		Description: "Values for MCP servers, keyed by metadata key. Only the keys in params.keys are accepted.",
		Params:      map[string]any{toolCallMetadataParamKeys: slices.Clone(ids)},
	}
}

// toolCallMetadataInterceptor activates the MCP tool-call metadata extension on a
// message whose caller requested it, when the gateway accepts every key the
// message's entry names. An entry that is not an object, or an id not
// accepted, refuses the message with invalid params before anything runs.
// With no accepted key the extension is not offered and never activated.
func newToolCallMetadataInterceptor(accepted []string) a2asrv.CallInterceptor {
	return &toolCallMetadataInterceptor{accepted: slices.Clone(accepted)}
}

type toolCallMetadataInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	accepted []string
}

func (i *toolCallMetadataInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	send, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || send == nil || len(i.accepted) == 0 || !callCtx.Extensions().Requested(toolCallMetadataExtension) {
		return ctx, nil, nil
	}
	if raw, present := send.Metadata[ToolCallMetadataExtensionURI]; present {
		entries, ok := raw.(map[string]any)
		if !ok {
			return ctx, nil, fmt.Errorf("%w: metadata %s must be an object keyed by metadata key", a2a.ErrInvalidParams, ToolCallMetadataExtensionURI)
		}
		for _, key := range slices.Sorted(maps.Keys(entries)) {
			if !slices.Contains(i.accepted, key) {
				return ctx, nil, fmt.Errorf("%w: metadata key %q is not accepted", a2a.ErrInvalidParams, key)
			}
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
// tool-call metadata extension, accepting toolCallMetadataKeys, is checked and
// activated before the propagator reads its activation.
func NewRequestHandler(h *Handler, toolCallMetadataKeys []string) a2asrv.RequestHandler {
	return &a2asrv.InterceptedHandler{
		Handler:      h,
		Interceptors: []a2asrv.CallInterceptor{newToolCallMetadataInterceptor(toolCallMetadataKeys), toolCallMetadataPropagator()},
		Logger:       h.Logger,
	}
}
