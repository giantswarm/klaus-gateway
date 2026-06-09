// Package a2a implements the A2A server surface for the gateway.
//
// The gateway acts as the single A2A waypoint in front of Klaus pods.
// The inbound A2A JSON-RPC and agent-card endpoints are mounted here;
// the ForwardingExecutor resolves the target pod and streams the response back.
package a2a

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"

	"github.com/giantswarm/klaus-gateway/pkg/kagentapi"
)

// a2aMethodCompat maps pre-v2 A2A JSON-RPC method names (used by kagent ≤ 0.9.x
// via trpc-a2a-go) to the names expected by a2a-go/v2. Requests using new-style
// names pass through unchanged.
var a2aMethodCompat = map[string]string{
	"message/send":                       "SendMessage",
	"message/stream":                     "SendStreamingMessage",
	"tasks/get":                          "GetTask",
	"tasks/cancel":                       "CancelTask",
	"tasks/resubscribe":                  "SubscribeToTask",
	"tasks/pushNotificationConfig/set":   "CreateTaskPushNotificationConfig",
	"tasks/pushNotificationConfig/get":   "GetTaskPushNotificationConfig",
	"agent/getAuthenticatedExtendedCard": "GetExtendedAgentCard",
}

// a2aCompatMiddleware rewrites legacy A2A JSON-RPC method names to their v2
// equivalents before the request reaches the handler. It is a no-op for
// requests that already use v2 names or for non-JSON bodies.
func a2aCompatMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		// Decode into a raw map so we can replace only the method field
		// without touching any other fields (preserves params, id, etc.).
		var raw map[string]json.RawMessage
		if json.Unmarshal(body, &raw) == nil {
			var method string
			if json.Unmarshal(raw["method"], &method) == nil {
				if newMethod, ok := a2aMethodCompat[method]; ok {
					if repl, merr := json.Marshal(newMethod); merr == nil {
						raw["method"] = repl
						if reencoded, rerr := json.Marshal(raw); rerr == nil {
							body = reencoded
						}
					}
				}
			}
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, r)
	})
}

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
func Mount(r chi.Router, card *a2a.AgentCard, executor a2asrv.AgentExecutor) {
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithPushNotifications(newTTLPushStore(), push.NewHTTPPushSender(nil)),
	)
	jsonrpcHandler := a2asrv.NewJSONRPCHandler(handler)
	cardHandler := a2asrv.NewStaticAgentCardHandler(card)

	r.Handle("/.well-known/agent-card.json", cardHandler)
	r.Handle("/.well-known/agent.json", cardHandler)

	translated := a2aCompatMiddleware(jsonrpcHandler)
	authed := extractAuthMiddleware(translated)
	// kagent posts to the service root (POST /) rather than the /a2a path
	// advertised in the agent card. This route should be removed once kagent
	// honours the SupportedInterfaces URL from the card.
	r.Post("/", authed.ServeHTTP)
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
