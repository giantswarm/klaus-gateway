// Package a2a implements the A2A server surface for the gateway.
//
// The gateway acts as the single A2A waypoint in front of Klaus pods.
// The inbound A2A JSON-RPC and agent-card endpoints are mounted here;
// the ForwardingExecutor resolves the target pod and streams the response back.
package a2a

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	a2apkg "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/push"

	"github.com/giantswarm/klaus-gateway/pkg/kagentapi"
)

// Mount registers the A2A endpoints on r:
//
//	/.well-known/agent-card.json  public agent card (unauthenticated discovery)
//	/.well-known/agent.json       alias for agent-card.json
//	/a2a                          A2A JSON-RPC endpoint (streaming + non-streaming)
//	/a2a/                         same with trailing slash
//
// The identity middleware reads the caller's bearer token and X-User-Id header
// from each request and stores them in the context so ForwardingExecutor can
// pass them to kagent without re-parsing tokens.
func Mount(r chi.Router, card *a2apkg.AgentCard, executor a2asrv.AgentExecutor) {
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithPushNotifications(newTTLPushStore(), push.NewHTTPPushSender(nil)),
	)
	jsonrpcHandler := a2asrv.NewJSONRPCHandler(handler)
	cardHandler := a2asrv.NewStaticAgentCardHandler(card)

	r.Handle("/.well-known/agent-card.json", cardHandler)
	r.Handle("/.well-known/agent.json", cardHandler)

	authed := extractAuthMiddleware(jsonrpcHandler)
	r.Post("/", authed.ServeHTTP) // kagent ignores the /a2a path in the card URL and posts to service root
	r.Handle("/a2a", authed)
	r.Handle("/a2a/*", authed)
}

// extractAuthMiddleware reads the agentgateway-forwarded identity headers and
// stores them as AuthInfo in the request context. No JWT parsing is performed;
// the token's validity is established upstream by agentgateway.
func extractAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		userSub := r.Header.Get("X-User-Id")
		ctx := WithAuthInfo(r.Context(), kagentapi.AuthInfo{
			BearerToken: bearer,
			UserSub:     userSub,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
