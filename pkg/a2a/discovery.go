package a2a

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// HarnessRuntimeClaude is the runtime of a Harness with spec.claude: the
// Claude Code harness of coding agents.
const HarnessRuntimeClaude = "claude"

// harnessRuntimes are the runtime adapters a Harness spec selects exactly one
// of, under the key of the same name.
var harnessRuntimes = []string{"kagent", "codex", HarnessRuntimeClaude, "byo"}

// AgentInfo describes one Agent of the served namespace.
type AgentInfo struct {
	Name      string
	Namespace string
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
	// Runtime is the runtime of the Harness the Agent embeds (spec.harness):
	// kagent, codex, claude or byo. Empty for a referenced Harness, whose
	// runtime HarnessRuntime reads.
	Runtime string
	// Unavailable is empty for a selectable agent. Otherwise it says why the
	// Agent cannot start a conversation: its Ready condition is not True.
	Unavailable string
}

// Ref is the agent ref ("namespace/name") channels route a conversation with.
func (a AgentInfo) Ref() string {
	return a.Namespace + "/" + a.Name
}

// RefusesCollaborators reports whether only the person whose Session it is may
// instruct agentRef's agent: whether its Harness has the claude runtime. A
// claude Harness runs code in the Session's workspace, so a git hook or a
// background process one sender's turn leaves would run under the next
// sender's credential: its Sessions are never shared. Every other runtime
// keeps the thread's collaborators. The error is HarnessRuntime's.
func (c *Client) RefusesCollaborators(ctx context.Context, agentRef string) (bool, error) {
	runtime, err := c.HarnessRuntime(ctx, agentRef)
	if err != nil {
		return false, err
	}
	return runtime == HarnessRuntimeClaude, nil
}

// HarnessRuntime returns the runtime of agentRef's Harness: the embedded
// one's, or the referenced one's as kagent's HarnessService lists it, read as
// the caller. The roster does not carry it, so a controller route that does
// not serve HarnessService costs this lookup and nothing else. A Harness the
// Agent names and the namespace does not hold is an error.
func (c *Client) HarnessRuntime(ctx context.Context, agentRef string) (string, error) {
	info, err := c.Agent(ctx, agentRef)
	if err != nil {
		return "", err
	}
	if info.Harness == "" {
		if info.Runtime == "" {
			return "", fmt.Errorf("a2a: Agent %s names no Harness", info.Ref())
		}
		return info.Runtime, nil
	}
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return "", err
	}
	listed, err := c.harnesses.ListHarnesses(callCtx, &apiv1alpha1.ListHarnessesRequest{Namespace: info.Namespace})
	if err != nil {
		return "", fmt.Errorf("a2a: list Harnesses in %s: %w", info.Namespace, err)
	}
	for _, h := range listed.GetHarnesses() {
		if h.GetRef().GetName() == info.Harness {
			return h.GetRuntime(), nil
		}
	}
	return "", fmt.Errorf("a2a: Agent %s names Harness %s, which %s does not hold", info.Ref(), info.Harness, info.Namespace)
}

// ListAgents returns the selectable Agents of the served namespace: the ones
// whose Ready condition is True. Agents that are not ready are left out;
// selecting one by name is refused with the reason (CardInfo). The list is
// fetched as the caller when the context carries a token and served from the
// roster cache otherwise.
func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	agents, err := c.rosterFor(ctx)
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

// CardInfo validates an agent selection: the Agent must exist in the served
// namespace and be selectable. The error names the reason so a channel can
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

// Agent returns the Agent agentRef names, whether or not it is selectable. A
// bare ref is resolved in the served namespace.
func (c *Client) Agent(ctx context.Context, agentRef string) (AgentInfo, error) {
	namespace, name, err := c.splitRef(agentRef)
	if err != nil {
		return AgentInfo{}, err
	}
	agents, err := c.rosterFor(ctx)
	if err != nil {
		return AgentInfo{}, err
	}
	for _, a := range agents {
		if a.Namespace == namespace && a.Name == name {
			return a, nil
		}
	}
	return AgentInfo{}, fmt.Errorf("%w: no Agent %s/%s", ErrAgentUnknown, namespace, name)
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

// rosterFor returns the roster for a call: fetched as the caller when ctx
// carries a token and the cache is stale, the cached roster otherwise. Without
// a token and without a cache there is nothing to serve.
func (c *Client) rosterFor(ctx context.Context) ([]AgentInfo, error) {
	cached, ok, fresh := c.cachedRoster()
	if ok && fresh {
		return cached, nil
	}
	agents, err := c.fetchRoster(ctx)
	if err == nil {
		return agents, nil
	}
	if ok && errors.Is(err, ErrNoIdentity) {
		return cached, nil
	}
	return nil, err
}

// fetchRoster lists the served namespace's Agents and AgentTemplates as the
// caller and refreshes the roster cache. The templates supply what an Agent
// takes from the one it references: the description, the ModelConfig, and
// the UI annotations the Agent itself does not carry.
func (c *Client) fetchRoster(ctx context.Context) ([]AgentInfo, error) {
	callCtx, err := c.serviceCtx(ctx)
	if err != nil {
		return nil, err
	}
	listed, err := c.agents.ListAgents(callCtx, &apiv1alpha1.ListAgentsRequest{Namespace: c.namespace})
	if err != nil {
		return nil, fmt.Errorf("a2a: list Agents in %s: %w", c.namespace, err)
	}
	templates, err := c.templates.ListAgentTemplates(callCtx, &apiv1alpha1.ListAgentTemplatesRequest{Namespace: c.namespace})
	if err != nil {
		return nil, fmt.Errorf("a2a: list AgentTemplates in %s: %w", c.namespace, err)
	}
	byName := make(map[string]*apiv1alpha1.AgentTemplate, len(templates.GetAgentTemplates()))
	for _, t := range templates.GetAgentTemplates() {
		byName[t.GetRef().GetName()] = t
	}
	agents := make([]AgentInfo, 0, len(listed.GetAgents()))
	for _, a := range listed.GetAgents() {
		agents = append(agents, c.agentInfo(a, byName))
	}
	c.storeRoster(agents)
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
		DisplayName: stringAt(annotations, DisplayNameAnnotation),
		IconURL:     stringAt(annotations, IconURLAnnotation),
	}
	if inline := nested(spec, "template"); inline != nil {
		info.Description = stringAt(inline, "description")
		info.ModelConfig = stringAt(nested(inline, "modelConfig"), "name")
	}
	if t := templates[stringAt(nested(spec, "templateRef"), "name")]; t != nil {
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
	if info.Harness == "" {
		info.Runtime = inlineRuntime(nested(spec, "harness"))
	}
	if info.IconURL == "" {
		info.IconURL = c.fallbackIcon(info.Name)
	}
	conditions, _ := nested(resource, "status")["conditions"].([]any)
	if ready, reason := readyCondition(conditions); !ready {
		info.Unavailable = fmt.Sprintf("Agent %s is not ready: %s", info.Name, reason)
	}
	return info
}

// inlineRuntime is the runtime of an embedded Harness spec: the one runtime
// key it sets. "" for no spec.
func inlineRuntime(harness map[string]any) string {
	for _, runtime := range harnessRuntimes {
		if nested(harness, runtime) != nil {
			return runtime
		}
	}
	return ""
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
