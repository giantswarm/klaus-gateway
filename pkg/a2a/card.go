package a2a

import (
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/giantswarm/klaus-gateway/internal/config"
)

// AgentCard builds the gateway's agent card from configuration.
// The card is served at /.well-known/agent-card.json and used by kagent
// to discover the agent's URL and capabilities.
func AgentCard(cfg config.A2AConfig) *a2a.AgentCard {
	skills := make([]a2a.AgentSkill, 0, len(cfg.Skills))
	for _, s := range cfg.Skills {
		skills = append(skills, a2a.AgentSkill{
			ID:          s.ID,
			Name:        s.Name,
			Description: s.Description,
			Tags:        s.Tags,
			Examples:    s.Examples,
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		})
	}
	// Provide a minimal default skill set when none are configured.
	if len(skills) == 0 {
		skills = defaultSkills()
	}

	return &a2a.AgentCard{
		Name:            cfg.CardName,
		Description:     cfg.CardDescription,
		Version:         cfg.CardVersion,
		URL:             cfg.CardURL,
		ProtocolVersion: "0.3.15",
		Capabilities: a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
		},
		Skills:             skills,
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	}
}

// defaultSkills returns skills that describe Klaus's core capabilities.
// Used when no skills are provided via configuration.
func defaultSkills() []a2a.AgentSkill {
	return []a2a.AgentSkill{
		{
			ID:          "general-coding",
			Name:        "Software development",
			Description: "Write, review, and refactor code in any language. Applies best practices for testing, documentation, and architecture.",
			Tags:        []string{"code", "development", "testing", "review"},
			Examples: []string{
				"Write a Go HTTP handler that validates a JWT",
				"Review this PR diff for security issues",
				"Add unit tests to this function",
			},
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		},
		{
			ID:          "platform-ops",
			Name:        "Platform operations",
			Description: "Operate Kubernetes clusters: manage workloads, inspect resources, troubleshoot failures, write and apply YAML manifests.",
			Tags:        []string{"kubernetes", "platform", "ops", "yaml"},
			Examples: []string{
				"List all failing pods in the kagent namespace",
				"Write a Helm values file for this service",
				"Debug why this Deployment rollout is stuck",
			},
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		},
		{
			ID:          "observability",
			Name:        "Observability and monitoring",
			Description: "Query metrics, logs, and traces. Write PromQL/LogQL, interpret dashboards, and diagnose production incidents.",
			Tags:        []string{"prometheus", "grafana", "loki", "tracing", "observability"},
			Examples: []string{
				"Show me the p99 latency for this service over the last hour",
				"Write a recording rule for request rate by namespace",
				"Find log lines correlated with this error spike",
			},
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		},
		{
			ID:          "mcp-tools",
			Name:        "MCP tool use",
			Description: "Execute MCP tools (muster, mcp-prometheus, custom servers) to interact with external systems.",
			Tags:        []string{"mcp", "tools", "automation"},
			Examples: []string{
				"List available MCP servers via muster",
				"Query Prometheus for current cluster CPU usage",
			},
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		},
	}
}
