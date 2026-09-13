package a2a

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// Annotations the Generic agent chart writes on an AgentTemplate for the UIs.
const (
	// DisplayNameAnnotation carries the human-facing name of an agent.
	DisplayNameAnnotation = "ui.giantswarm.io/display-name"
	// IconURLAnnotation carries the URL of the agent's icon.
	IconURLAnnotation = "ui.giantswarm.io/icon-url"
)

// AgentInfo describes one AgentTemplate of the served namespace.
type AgentInfo struct {
	Name      string
	Namespace string
	// DisplayName is the ui.giantswarm.io/display-name annotation; empty when
	// the template carries none.
	DisplayName string
	// IconURL is the ui.giantswarm.io/icon-url annotation, or the configured
	// fallback template rendered for the agent; empty when neither is set.
	IconURL     string
	Description string
	// Harness is the admitting Harness an AgentInstance of this template is
	// created with: the one whose compiled revision is Ready, else the first
	// admitting one. Empty when no Harness admits the template.
	Harness string
	// ModelConfig is the name of the template's ModelConfig (same namespace).
	ModelConfig string
	// Unavailable is empty for a selectable template. Otherwise it says why the
	// template cannot start a conversation: no Harness admits it, or the
	// admitting Harness has not compiled a ready revision.
	Unavailable string
}

// Ref is the agent ref ("namespace/name") channels route a conversation with.
func (a AgentInfo) Ref() string {
	return a.Namespace + "/" + a.Name
}

// ListAgents returns the selectable AgentTemplates of the served namespace:
// the ones an admitting Harness has compiled a ready revision for. Templates
// nobody admits, or whose revision is not ready, are left out; selecting one by
// name is refused with the reason (CardInfo). The list is fetched as the
// caller when the context carries a token and served from the roster cache
// otherwise.
func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	agents, err := c.templatesFor(ctx)
	if err != nil {
		return nil, err
	}
	selectable := make([]AgentInfo, 0, len(agents))
	for _, a := range agents {
		if a.Unavailable == "" {
			selectable = append(selectable, a)
		}
	}
	return selectable, nil
}

// CardIdentity returns the agent's display name and icon URL for branding a
// reply. Branding never blocks or fails a turn: an unknown agent yields empty
// values (the icon still falls back to the configured template).
func (c *Client) CardIdentity(ctx context.Context, agentRef string) (username, iconURL string) {
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return "", c.fallbackIcon(agentRef)
	}
	return info.DisplayName, info.IconURL
}

// CardInfo validates an agent selection: the template must exist in the
// served namespace and be selectable. The error names the reason so a channel
// can refuse the selection loudly instead of substituting an agent.
func (c *Client) CardInfo(ctx context.Context, agentRef string) (name, description string, err error) {
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return "", "", err
	}
	if info.Unavailable != "" {
		return "", "", fmt.Errorf("%w: %s: %s", ErrAgentUnavailable, agentRef, info.Unavailable)
	}
	name = info.DisplayName
	if name == "" {
		name = info.Name
	}
	return name, info.Description, nil
}

// AgentModel resolves the model id and provider behind an agent from its
// ModelConfig. Empty strings with a nil error mean the template names no
// ModelConfig.
func (c *Client) AgentModel(ctx context.Context, agentRef string) (model, provider string, err error) {
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return "", "", err
	}
	if info.ModelConfig == "" {
		return "", "", nil
	}
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return "", "", err
	}
	resp, err := c.models.GetModelConfig(callCtx, &apiv1alpha1.GetModelConfigRequest{
		Ref: &apiv1alpha1.ResourceReference{Namespace: info.Namespace, Name: info.ModelConfig},
	})
	if err != nil {
		return "", "", fmt.Errorf("a2a: get ModelConfig %s/%s: %w", info.Namespace, info.ModelConfig, err)
	}
	spec := nested(resp.GetModelConfig().GetResource().GetValue().AsMap(), "spec")
	return stringAt(spec, "model"), stringAt(spec, "provider"), nil
}

// Agent returns the template agentRef names, whether or not it is selectable.
// A bare ref is resolved in the served namespace.
func (c *Client) Agent(ctx context.Context, agentRef string) (AgentInfo, error) {
	namespace, name, err := c.splitRef(agentRef)
	if err != nil {
		return AgentInfo{}, err
	}
	agents, err := c.templatesFor(ctx)
	if err != nil {
		return AgentInfo{}, err
	}
	for _, a := range agents {
		if a.Namespace == namespace && a.Name == name {
			return a, nil
		}
	}
	return AgentInfo{}, fmt.Errorf("%w: no AgentTemplate %s/%s", ErrAgentUnknown, namespace, name)
}

// splitRef resolves "name" or "namespace/name" against the served namespace.
func (c *Client) splitRef(agentRef string) (namespace, name string, err error) {
	namespace, name = c.namespace, agentRef
	if ns, n, ok := strings.Cut(agentRef, "/"); ok {
		namespace, name = ns, n
	}
	if name == "" || namespace == "" {
		return "", "", fmt.Errorf("%w: agent ref %q is empty", ErrAgentUnknown, agentRef)
	}
	if namespace != c.namespace {
		return "", "", fmt.Errorf("%w: %s is not in the served namespace %s", ErrAgentUnknown, agentRef, c.namespace)
	}
	return namespace, name, nil
}

func (c *Client) fallbackIcon(agentRef string) string {
	if c.iconTemplate == "" {
		return ""
	}
	name := agentRef
	if i := strings.LastIndex(agentRef, "/"); i >= 0 {
		name = agentRef[i+1:]
	}
	return strings.ReplaceAll(c.iconTemplate, "{agent}", name)
}

// templatesFor returns the roster for a call: fetched as the caller when ctx
// carries a token and the cache is stale, the cached roster otherwise. Without
// a token and without a cache there is nothing to serve.
func (c *Client) templatesFor(ctx context.Context) ([]AgentInfo, error) {
	cached, ok, fresh := c.cachedRoster()
	if ok && fresh {
		return cached, nil
	}
	agents, err := c.fetchTemplates(ctx)
	if err == nil {
		return agents, nil
	}
	if ok && errors.Is(err, ErrNoIdentity) {
		return cached, nil
	}
	return nil, err
}

// fetchTemplates lists the served namespace's AgentTemplates as the caller and
// refreshes the roster cache.
func (c *Client) fetchTemplates(ctx context.Context) ([]AgentInfo, error) {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.templates.ListAgentTemplates(callCtx, &apiv1alpha1.ListAgentTemplatesRequest{Namespace: c.namespace})
	if err != nil {
		return nil, fmt.Errorf("a2a: list AgentTemplates in %s: %w", c.namespace, err)
	}
	agents := make([]AgentInfo, 0, len(resp.GetAgentTemplates()))
	for _, t := range resp.GetAgentTemplates() {
		agents = append(agents, c.agentInfo(t))
	}
	c.storeRoster(agents)
	return agents, nil
}

// agentInfo derives the roster entry from a template: the annotations from
// its metadata, the readiness from status.harnesses[] of the admitting
// Harnesses the controller reports.
func (c *Client) agentInfo(t *apiv1alpha1.AgentTemplate) AgentInfo {
	resource := t.GetResource().GetValue().AsMap()
	annotations := nested(nested(resource, "metadata"), "annotations")
	info := AgentInfo{
		Name:        t.GetRef().GetName(),
		Namespace:   t.GetRef().GetNamespace(),
		DisplayName: stringAt(annotations, DisplayNameAnnotation),
		IconURL:     stringAt(annotations, IconURLAnnotation),
		Description: t.GetDescription(),
		ModelConfig: t.GetModelConfigRef().GetName(),
	}
	if info.IconURL == "" {
		info.IconURL = c.fallbackIcon(info.Name)
	}
	info.Harness, info.Unavailable = harnessReadiness(t.GetAdmittingHarnesses(), nested(resource, "status"))
	return info
}

// harnessReadiness picks the Harness a conversation is created with and
// explains an unusable template. Readiness is the Ready condition the
// controller writes per admitting Harness under status.harnesses[].
func harnessReadiness(admitting []string, statusMap map[string]any) (harness, unavailable string) {
	if len(admitting) == 0 {
		return "", "no Harness admits this AgentTemplate (it carries no admission label a platform Harness selects)"
	}
	entries, _ := statusMap["harnesses"].([]any)
	var firstReason string
	for _, name := range admitting {
		ready, reason := readyCondition(entries, name)
		if ready {
			return name, ""
		}
		if firstReason == "" {
			firstReason = reason
		}
	}
	return admitting[0], fmt.Sprintf("Harness %s has not compiled a ready revision: %s", admitting[0], firstReason)
}

// readyCondition reads the Ready condition of one Harness entry.
func readyCondition(entries []any, harness string) (bool, string) {
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok || stringAt(entry, "harness") != harness {
			continue
		}
		conditions, _ := entry["conditions"].([]any)
		for _, cond := range conditions {
			cm, ok := cond.(map[string]any)
			if !ok || stringAt(cm, "type") != "Ready" {
				continue
			}
			if stringAt(cm, "status") == "True" {
				return true, ""
			}
			reason := stringAt(cm, "message")
			if reason == "" {
				reason = stringAt(cm, "reason")
			}
			if reason == "" {
				reason = "Ready=" + stringAt(cm, "status")
			}
			return false, reason
		}
		return false, "no Ready condition reported yet"
	}
	return false, "no status reported yet"
}

func nested(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func stringAt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
