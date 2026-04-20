// Package upstream wires klaus-gateway to either talk directly to klaus
// instances or to route through an agentgateway upstream.
package upstream

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// InstanceHeader is the header agentgateway inspects to pick an upstream backend.
const InstanceHeader = "X-Klaus-Instance"

// Agentgateway rewrites outgoing instance requests to go through an
// agentgateway deployment. The base URL is the agentgateway's public LLM
// entrypoint (e.g. http://agentgateway:8080). Routing by instance name is
// handled on the agentgateway side via this header.
type Agentgateway struct {
	BaseURL *url.URL
}

// Parse returns an Agentgateway for raw. Returns nil, nil when raw is empty
// (direct mode).
func Parse(raw string) (*Agentgateway, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &Agentgateway{BaseURL: u}, nil
}

// Apply rewrites req to point at the agentgateway and injects the instance
// header. The original URL's path is preserved.
func (a *Agentgateway) Apply(req *http.Request, ref lifecycle.InstanceRef) {
	if a == nil || a.BaseURL == nil {
		return
	}
	req.URL.Scheme = a.BaseURL.Scheme
	req.URL.Host = a.BaseURL.Host
	basePath := strings.TrimRight(a.BaseURL.Path, "/")
	if basePath != "" {
		req.URL.Path = basePath + req.URL.Path
	}
	req.Host = a.BaseURL.Host
	req.Header.Set(InstanceHeader, ref.Name)
}

// URL returns the configured base URL as a string, or "" in direct mode.
func (a *Agentgateway) URL() string {
	if a == nil || a.BaseURL == nil {
		return ""
	}
	return a.BaseURL.String()
}
