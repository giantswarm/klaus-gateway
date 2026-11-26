package a2aserver

import (
	"context"
	"fmt"
	"regexp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/mark3labs/mcp-go/mcp"
)

var mcpMetadataKey = regexp.MustCompile(`^([A-Za-z]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z]([A-Za-z0-9-]*[A-Za-z0-9])?)*/)?([A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?)?$`)

type mcpRequestMetadataInterceptor struct {
	a2asrv.PassthroughCallInterceptor
}

func (*mcpRequestMetadataInterceptor) Before(ctx context.Context, _ *a2asrv.CallContext, request *a2asrv.Request) (context.Context, any, error) {
	if send, ok := request.Payload.(*a2a.SendMessageRequest); ok && send != nil {
		if err := validateMCPRequestMetadata(send.Metadata); err != nil {
			return ctx, nil, err
		}
	}
	return ctx, nil, nil
}

func validateMCPRequestMetadata(values map[string]any) error {
	for key := range values {
		if !mcpMetadataKey.MatchString(key) {
			return fmt.Errorf("%w: invalid MCP metadata key %q", a2a.ErrInvalidParams, key)
		}
		switch key {
		case mcp.MetaKeyProtocolVersion, mcp.MetaKeyClientInfo, mcp.MetaKeyClientCapabilities,
			mcp.MetaKeyLogLevel, mcp.MetaKeyServerInfo, mcp.MetaKeySubscriptionID,
			"progressToken", "traceparent", "tracestate", "baggage":
			return fmt.Errorf("%w: MCP metadata key %q is owned by the transport", a2a.ErrInvalidParams, key)
		}
	}
	return nil
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
