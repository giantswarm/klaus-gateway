package a2aserver

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// mcpMetadataKey is the MCP _meta key syntax: an optional prefix of
// dot-separated labels and a slash, then a name.
var mcpMetadataKey = regexp.MustCompile(`^([A-Za-z]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z]([A-Za-z0-9-]*[A-Za-z0-9])?)*/)?([A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)?$`)

// transportMetadataKeys are unprefixed _meta keys the MCP and W3C trace
// context transports set themselves.
var transportMetadataKeys = map[string]bool{"progressToken": true, "traceparent": true, "tracestate": true, "baggage": true}

type mcpRequestMetadataInterceptor struct {
	a2asrv.PassthroughCallInterceptor
}

// Before validates the metadata of every request that carries it: the server
// propagator forwards it, whichever method the request is for.
func (*mcpRequestMetadataInterceptor) Before(ctx context.Context, _ *a2asrv.CallContext, request *a2asrv.Request) (context.Context, any, error) {
	var metadata map[string]any
	switch payload := request.Payload.(type) {
	case *a2a.SendMessageRequest:
		if payload != nil {
			metadata = payload.Metadata
		}
	case *a2a.CancelTaskRequest:
		if payload != nil {
			metadata = payload.Metadata
		}
	}
	return ctx, nil, validateMCPRequestMetadata(metadata)
}

func validateMCPRequestMetadata(values map[string]any) error {
	for key := range values {
		switch {
		case key == "" || !mcpMetadataKey.MatchString(key):
			return fmt.Errorf("%w: invalid MCP metadata key %q", a2a.ErrInvalidParams, key)
		case transportMetadataKeys[key], reservedMCPPrefix(key):
			return fmt.Errorf("%w: MCP metadata key %q is reserved for MCP and its transport", a2a.ErrInvalidParams, key)
		}
	}
	return nil
}

// reservedMCPPrefix reports a key whose prefix has modelcontextprotocol or
// mcp as its second label, which MCP reserves (basic, "_meta").
func reservedMCPPrefix(key string) bool {
	prefix, _, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	labels := strings.Split(prefix, ".")
	return len(labels) > 1 && (labels[1] == "modelcontextprotocol" || labels[1] == "mcp")
}

// NewRequestHandler validates and propagates ordinary request metadata.
func NewRequestHandler(handler *Handler) a2asrv.RequestHandler {
	return &a2asrv.InterceptedHandler{
		Handler: handler,
		Interceptors: []a2asrv.CallInterceptor{&mcpRequestMetadataInterceptor{}, a2aext.NewServerPropagator(&a2aext.ServerPropagatorConfig{
			MetadataPredicate: func(context.Context, string) bool { return true },
			HeaderPredicate:   func(context.Context, string) bool { return false },
		})},
		Logger: handler.Logger,
	}
}
