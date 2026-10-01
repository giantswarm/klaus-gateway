package slack

import (
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

func TestAgentInfoRef_FollowsDeploymentRefShape(t *testing.T) {
	ag := pkga2a.AgentInfo{Name: "sre-agent", Namespace: "kagent"}
	bare := &Adapter{DefaultAgent: "sre-agent", Namespace: "kagent"}
	require.Equal(t, "sre-agent", bare.agentInfoRef(ag), "bare default")
	qualified := &Adapter{DefaultAgent: "kagent/sre-agent", Namespace: "kagent"}
	require.Equal(t, "kagent/sre-agent", qualified.agentInfoRef(ag), "qualified default")
}
