package a2aserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const (
	// cardTTL is how long an agent's card is reused. Whether the caller may
	// use the agent is checked on every request regardless.
	cardTTL = time.Minute
	// maxCachedCards bounds the cards kept in memory.
	maxCachedCards = 1024
)

// agentURL is the JSON-RPC endpoint of the agent tenant names under the
// server's endpoint base.
func agentURL(base, tenant string) string {
	return strings.TrimSuffix(base, "/") + "/" + tenant
}

// NewAgentPaths serves each agent under prefix (the A2A server's endpoint
// path with a trailing slash), addressed as <organization>/<agent>:
//
//   - POST <prefix><organization>/<agent>: A2A JSON-RPC for that agent. A
//     request's tenant, when set, must name the same agent.
//   - GET <prefix><organization>/<agent>/.well-known/agent-card.json: the
//     agent's own Agent Card.
//
// An agent the caller may not use is not found, the same as one that does
// not exist. Callers wrap the handler in the Authenticator.
func NewAgentPaths(handler *Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 2 || (len(parts) == 3 && parts[2] == ""):
			tenant, ok := agentTenant(parts[0], parts[1])
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			a2asrv.NewJSONRPCHandler(newRequestHandler(handler, &pathTenant{tenant: tenant})).ServeHTTP(w, r)
		case len(parts) == 4 && "/"+parts[2]+"/"+parts[3] == a2asrv.WellKnownAgentCardPath:
			tenant, ok := agentTenant(parts[0], parts[1])
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			handler.serveAgentCard(w, r, tenant)
		default:
			http.NotFound(w, r)
		}
	})
}

func agentTenant(organization, agent string) (string, bool) {
	if organization == "" || agent == "" {
		return "", false
	}
	return organization + "/" + agent, true
}

// pathTenant makes the agent the URL names the request's tenant, refusing a
// request whose own tenant names another.
type pathTenant struct {
	a2asrv.PassthroughCallInterceptor
	tenant string
}

func (p *pathTenant) Before(ctx context.Context, _ *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	var field *string
	switch payload := req.Payload.(type) {
	case *a2a.SendMessageRequest:
		field = &payload.Tenant
	case *a2a.GetTaskRequest:
		field = &payload.Tenant
	case *a2a.CancelTaskRequest:
		field = &payload.Tenant
	case *a2a.SubscribeToTaskRequest:
		field = &payload.Tenant
	case *a2a.GetExtendedAgentCardRequest:
		field = &payload.Tenant
	case *a2a.ListTasksRequest:
		field = &payload.Tenant
	case *a2a.GetTaskPushConfigRequest:
		field = &payload.Tenant
	case *a2a.ListTaskPushConfigRequest:
		field = &payload.Tenant
	case *a2a.DeleteTaskPushConfigRequest:
		field = &payload.Tenant
	case *a2a.PushConfig:
		field = &payload.Tenant
	default:
		return ctx, nil, nil
	}
	if *field != "" && *field != p.tenant {
		return ctx, nil, a2a.NewError(a2a.ErrInvalidParams, "tenant does not match the agent URL")
	}
	*field = p.tenant
	return a2a.AttachTenant(ctx, p.tenant), nil, nil
}

// serveAgentCard answers the Agent Card of the agent tenant names, as the
// caller.
func (h *Handler) serveAgentCard(w http.ResponseWriter, r *http.Request, tenant string) {
	w.Header().Set("Cache-Control", "private, no-store")
	card, err := h.agentCard(r.Context(), tenant)
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, a2a.ErrUnauthenticated):
			status = http.StatusUnauthorized
		case errors.Is(err, a2a.ErrUnauthorized), errors.Is(err, a2a.ErrInvalidParams):
			status = http.StatusNotFound
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	body, err := json.Marshal(card)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// agentCard is the card of the agent tenant names among the agents the caller
// may use: the agent's own card from the controller, served at the agent's
// endpoint with the server's security schemes.
func (h *Handler) agentCard(ctx context.Context, tenant string) (*a2a.AgentCard, error) {
	if h.Card == nil || len(h.Card.SupportedInterfaces) == 0 {
		return nil, a2a.ErrExtendedCardNotConfigured
	}
	ctx, ref, err := h.agentContext(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if card, ok := h.cards.get(ref, time.Now()); ok {
		return card, nil
	}
	own, err := h.Agents.AgentCard(ctx)
	if err != nil {
		return nil, h.refusal(ctx, "agent card", err)
	}
	card := h.publicAgentCard(own, tenant)
	h.cards.put(ref, card, time.Now())
	return card, nil
}

// publicAgentCard is own as an A2A client sees it: the agent's identity,
// skills and capabilities (its extensions included) unchanged, its only
// interface the agent's endpoint on this server and its security the
// server's. Nothing names the controller, its namespace or its URLs.
func (h *Handler) publicAgentCard(own *a2a.AgentCard, tenant string) *a2a.AgentCard {
	base := h.Card.SupportedInterfaces[0]
	return &a2a.AgentCard{
		Name:        own.Name,
		Description: own.Description,
		Version:     own.Version,
		SupportedInterfaces: []*a2a.AgentInterface{{
			URL: agentURL(base.URL, tenant), ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version,
		}},
		Capabilities:         own.Capabilities,
		DefaultInputModes:    own.DefaultInputModes,
		DefaultOutputModes:   own.DefaultOutputModes,
		Skills:               own.Skills,
		SecuritySchemes:      h.Card.SecuritySchemes,
		SecurityRequirements: h.Card.SecurityRequirements,
	}
}

// cardCache keeps built cards by agent reference for cardTTL. The zero value
// is ready to use.
type cardCache struct {
	mu      sync.Mutex
	entries map[string]cachedCard
}

type cachedCard struct {
	card    *a2a.AgentCard
	expires time.Time
}

func (c *cardCache) get(ref string, now time.Time) (*a2a.AgentCard, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[ref]
	if !ok {
		return nil, false
	}
	if !now.Before(entry.expires) {
		delete(c.entries, ref)
		return nil, false
	}
	return entry.card, true
}

// put keeps card for cardTTL; nothing new is kept while the cache is full of
// entries that are still fresh.
func (c *cardCache) put(ref string, card *a2a.AgentCard, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]cachedCard)
	}
	if _, ok := c.entries[ref]; !ok && len(c.entries) >= maxCachedCards {
		for k, entry := range c.entries {
			if !now.Before(entry.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxCachedCards {
			return
		}
	}
	c.entries[ref] = cachedCard{card: card, expires: now.Add(cardTTL)}
}
