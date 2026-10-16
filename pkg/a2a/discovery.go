package a2a

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// Annotations the Generic agent chart writes on an Agent, or on the
// AgentTemplate it references, for the UIs.
const (
	// DisplayNameAnnotation carries the human-facing name of an agent.
	DisplayNameAnnotation = "ui.giantswarm.io/display-name"
	// IconURLAnnotation carries the URL of the agent's icon.
	IconURLAnnotation = "ui.giantswarm.io/icon-url"
)

// AgentInfo describes one Agent of the caller's roster.
type AgentInfo struct {
	Name      string
	Namespace string
	// Tenant is the tenant of the Agent's namespace (Client.Tenant).
	Tenant string
	// DisplayName is the ui.giantswarm.io/display-name annotation of the
	// Agent, or of its AgentTemplate; empty when neither carries one.
	DisplayName string
	// IconURL is the ui.giantswarm.io/icon-url annotation of the Agent, or of
	// its AgentTemplate, or the configured fallback template rendered for the
	// agent; empty when none is set.
	IconURL string
	// Description is the template's description: the referenced
	// AgentTemplate's, or the inline template's.
	Description string
	// ModelConfig is the name of the template's ModelConfig (same namespace).
	ModelConfig string
	// Harness is the name of the Harness spec.harnessRef names; empty when the
	// Agent embeds its Harness.
	Harness string
	// Unavailable is empty for a selectable agent. Otherwise it says why the
	// Agent cannot start a conversation: its Ready condition is not True.
	Unavailable string
}

// Ref is the agent ref ("namespace/name") channels route a conversation with.
func (a AgentInfo) Ref() string {
	return a.Namespace + "/" + a.Name
}

// ListAgents returns the selectable Agents of the caller's roster: the ones
// whose Ready condition is True, in every namespace the controller lets the
// caller list. Agents that are not ready are left out; selecting one by name
// is refused with the reason (CardInfo). The roster is fetched as the caller
// on every call: the controller answers each person with their own Agents, so
// no answer is kept for another call.
func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	agents, err := c.fetchRoster(ctx)
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
// reply. Branding never blocks or fails a turn and needs no caller: it is how
// the agent looked in the last roster any caller fetched, or, before one has,
// the caller's own roster when ctx carries a token. An unknown agent yields
// an empty name and the configured fallback icon.
func (c *Client) CardIdentity(ctx context.Context, agentRef string) (username, iconURL string) {
	namespace, name, err := c.splitRef(agentRef)
	if err != nil {
		return "", c.fallbackIcon(agentRef)
	}
	if look, ok := c.looks.get(namespace, name); ok {
		return look.displayName, look.iconURL
	}
	fctx, cancel := context.WithTimeout(ctx, cardIdentityTimeout)
	defer cancel()
	if _, err := c.fetchRoster(fctx); err == nil {
		if look, ok := c.looks.get(namespace, name); ok {
			return look.displayName, look.iconURL
		}
	}
	return "", c.fallbackIcon(agentRef)
}

// cardIdentityTimeout bounds the roster read behind a look nobody's roster
// listed yet: branding must not hold a turn up.
const cardIdentityTimeout = 5 * time.Second

// CardInfo validates an agent selection as the caller: the Agent must be in
// their roster and be selectable. The error names the reason so a channel can
// refuse the selection loudly instead of substituting an agent.
func (c *Client) CardInfo(ctx context.Context, agentRef string) (name, description string, err error) {
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return "", "", err
	}
	if info.Unavailable != "" {
		return "", "", &AgentUnavailableError{Ref: agentRef, Reason: info.Unavailable}
	}
	name = info.DisplayName
	if name == "" {
		name = info.Name
	}
	return name, info.Description, nil
}

// AgentModel resolves the model id and provider behind an agent from its
// ModelConfig. Empty strings with a nil error mean the agent's template names
// no ModelConfig.
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

// Agent returns the Agent agentRef names from the caller's roster, whether or
// not it is selectable. A bare ref is resolved in the platform tenant's
// namespace. An Agent the roster does not list is an AgentNotPermittedError:
// the controller lists every Agent the caller may use, so one it leaves out
// either does not exist or is not the caller's to use, and the two are not
// told apart.
func (c *Client) Agent(ctx context.Context, agentRef string) (AgentInfo, error) {
	namespace, name, err := c.splitRef(agentRef)
	if err != nil {
		return AgentInfo{}, err
	}
	agents, err := c.fetchRoster(ctx)
	if err != nil {
		return AgentInfo{}, err
	}
	for _, a := range agents {
		if a.Namespace == namespace && a.Name == name {
			return a, nil
		}
	}
	return AgentInfo{}, c.notPermitted(agentRef, nil)
}

// notPermitted is the refusal of agentRef for the caller, cause the
// controller's or agent-manager's own when there is one.
func (c *Client) notPermitted(agentRef string, cause error) *AgentNotPermittedError {
	e := &AgentNotPermittedError{Ref: agentRef, Name: agentRef, Cause: cause}
	if namespace, name, err := c.splitRef(agentRef); err == nil {
		e.Name, e.Tenant = name, c.Tenant(namespace)
		// The controller's refusal names an Agent the caller already reached;
		// a roster miss shows nothing another caller's roster listed, so it
		// does not tell an Agent that exists from one that does not.
		if look, ok := c.looks.get(namespace, name); ok && cause != nil {
			e.DisplayName = look.displayName
		}
	}
	return e
}

// splitRef resolves "name" or "namespace/name"; a bare name is in the
// platform tenant's namespace.
func (c *Client) splitRef(agentRef string) (namespace, name string, err error) {
	namespace, name = c.namespace, agentRef
	if ns, n, ok := strings.Cut(agentRef, "/"); ok {
		namespace, name = ns, n
	}
	if name == "" || namespace == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%w: agent ref %q is not name or namespace/name", ErrAgentUnknown, agentRef)
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

// fetchRoster lists the Agents and AgentTemplates of every namespace the
// controller lets the caller list, as the caller, and records how each Agent
// looks (agentLooks). The templates supply what an Agent takes from the one it
// references, in its own namespace: the description, the ModelConfig, and the
// UI annotations the Agent itself does not carry.
func (c *Client) fetchRoster(ctx context.Context) ([]AgentInfo, error) {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return nil, err
	}
	listed, err := c.agents.ListAgents(callCtx, &apiv1alpha1.ListAgentsRequest{})
	if err != nil {
		return nil, fmt.Errorf("a2a: list Agents: %w", err)
	}
	templates, err := c.templates.ListAgentTemplates(callCtx, &apiv1alpha1.ListAgentTemplatesRequest{})
	if err != nil {
		return nil, fmt.Errorf("a2a: list AgentTemplates: %w", err)
	}
	byRef := make(map[string]*apiv1alpha1.AgentTemplate, len(templates.GetAgentTemplates()))
	for _, t := range templates.GetAgentTemplates() {
		byRef[t.GetRef().GetNamespace()+"/"+t.GetRef().GetName()] = t
	}
	agents := make([]AgentInfo, 0, len(listed.GetAgents()))
	for _, a := range listed.GetAgents() {
		info := c.agentInfo(a, byRef)
		c.looks.put(info)
		agents = append(agents, info)
	}
	return agents, nil
}

// agentInfo derives the roster entry from an Agent: the annotations from its
// metadata (its AgentTemplate's when it carries none), the description and
// ModelConfig from the template it references or embeds, the Harness it
// references or the runtime of the one it embeds, the readiness from
// status.conditions.
func (c *Client) agentInfo(a *apiv1alpha1.Agent, templates map[string]*apiv1alpha1.AgentTemplate) AgentInfo {
	resource := a.GetResource().GetValue().AsMap()
	spec := nested(resource, "spec")
	annotations := nested(nested(resource, "metadata"), "annotations")
	info := AgentInfo{
		Name:        a.GetRef().GetName(),
		Namespace:   a.GetRef().GetNamespace(),
		Tenant:      c.Tenant(a.GetRef().GetNamespace()),
		DisplayName: stringAt(annotations, DisplayNameAnnotation),
		IconURL:     stringAt(annotations, IconURLAnnotation),
	}
	if inline := nested(spec, "template"); inline != nil {
		info.Description = stringAt(inline, "description")
		info.ModelConfig = stringAt(nested(inline, "modelConfig"), "name")
	}
	if t := templates[info.Namespace+"/"+stringAt(nested(spec, "templateRef"), "name")]; t != nil {
		info.Description = t.GetDescription()
		info.ModelConfig = t.GetModelConfigRef().GetName()
		templateAnnotations := nested(nested(t.GetResource().GetValue().AsMap(), "metadata"), "annotations")
		if info.DisplayName == "" {
			info.DisplayName = stringAt(templateAnnotations, DisplayNameAnnotation)
		}
		if info.IconURL == "" {
			info.IconURL = stringAt(templateAnnotations, IconURLAnnotation)
		}
	}
	info.Harness = stringAt(nested(spec, "harnessRef"), "name")
	if info.IconURL == "" {
		info.IconURL = c.fallbackIcon(info.Name)
	}
	conditions, _ := nested(resource, "status")["conditions"].([]any)
	if ready, reason := readyCondition(conditions); !ready {
		info.Unavailable = fmt.Sprintf("Agent %s is not ready: %s", info.Name, reason)
	}
	return info
}

// readyCondition reads the Ready condition of an Agent's status.conditions:
// true, or false with the reason.
func readyCondition(conditions []any) (bool, string) {
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

// agentLook is how an Agent looks on a channel: its display name and icon.
type agentLook struct {
	displayName, iconURL string
}

// agentLooks remembers how each Agent looked in the last roster any caller
// fetched, by namespace and name. It labels replies and notices where no
// caller's token is at hand; it never says which Agents a caller may see or
// use, which only the caller's own roster does. An Agent leaves it only when
// the process restarts: a stale entry costs a label, never access.
type agentLooks struct {
	mu    sync.Mutex
	looks map[string]agentLook
}

func (l *agentLooks) put(info AgentInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.looks == nil {
		l.looks = map[string]agentLook{}
	}
	l.looks[info.Namespace+"/"+info.Name] = agentLook{displayName: info.DisplayName, iconURL: info.IconURL}
}

func (l *agentLooks) get(namespace, name string) (agentLook, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	look, ok := l.looks[namespace+"/"+name]
	return look, ok
}
