package a2a

import (
	"context"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/grpc"
)

// agentSkillTag tags the skill a card without skills gets.
const agentSkillTag = "agent"

// skillFillingClient is the controller's A2A service with one change: an
// Agent's card without skills gets one skill named after the Agent before
// a2a-go converts it. The controller gives an Agent without A2A skills an
// empty skill list, and a2a-go refuses a card without skills.
type skillFillingClient struct {
	a2apb.A2AServiceClient
}

func (c skillFillingClient) GetExtendedAgentCard(ctx context.Context, in *a2apb.GetExtendedAgentCardRequest, opts ...grpc.CallOption) (*a2apb.AgentCard, error) {
	card, err := c.A2AServiceClient.GetExtendedAgentCard(ctx, in, opts...)
	if err != nil || card == nil || len(card.GetSkills()) > 0 {
		return card, err
	}
	card.Skills = []*a2apb.AgentSkill{{
		Id:          in.GetTenant(),
		Name:        card.GetName(),
		Description: card.GetDescription(),
		Tags:        []string{agentSkillTag},
	}}
	return card, nil
}
