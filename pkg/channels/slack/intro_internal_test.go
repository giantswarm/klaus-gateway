package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

func TestIntroBlocks(t *testing.T) {
	roster := &rosterErrThenAgents{agents: []pkga2a.AgentInfo{{Name: "sre-agent", DisplayName: "SRE Agent"}}}
	text := func(b any) string {
		m := b.(map[string]any)
		if m[bkType] == bkContext {
			return m[bkElements].([]any)[0].(map[string]any)[bkText].(string)
		}
		return m[bkText].(map[string]any)[bkText].(string)
	}
	for name, tc := range map[string]struct {
		dflt, body, hints string
		want, wantHints   string
	}{
		"intro names the default agent": {
			"sre-agent", channelIntroText, channelIntroContext,
			"Swarmgeist connects this channel to Giant Swarm's agents. Mention <@UBOT> to start a thread with *SRE Agent*, or mention it with `/agent` to pick another agent. Agents work with your permissions and ask before making changes.",
			"<@UBOT> `/help` lists the commands",
		},
		"greeting names the default agent": {
			"sre-agent", assistantGreetingText, assistantGreetingContext,
			"Ask about a cluster, an alert or a deployment. Swarmgeist routes each conversation to an agent, *SRE Agent* by default. Agents work with your permissions and ask before making changes.",
			"<@UBOT> `/help` lists the commands · <@UBOT> `/agent` lists the agents",
		},
		"greeting on an agent off the roster uses its name": {
			"kagent/swarmgeist", assistantGreetingText, assistantGreetingContext,
			"Ask about a cluster, an alert or a deployment. Swarmgeist routes each conversation to an agent, *swarmgeist* by default. Agents work with your permissions and ask before making changes.",
			"<@UBOT> `/help` lists the commands · <@UBOT> `/agent` lists the agents",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := testAdapterWithRoster(roster)
			a.DefaultAgent = tc.dflt
			a.botUserID, a.botResolved = "UBOT", true
			fallback, blocks := a.introBlocks(context.Background(), tc.body, tc.hints)
			require.Len(t, blocks, 2)
			require.Equal(t, tc.want, text(blocks[0]))
			require.Equal(t, tc.wantHints, text(blocks[1]))
			require.Equal(t, tc.want, fallback)
			require.NotContains(t, fallback, "%!", "no stray format verb")
		})
	}
}

// Without the bot's ID the hints cannot say what to type, so they are left off.
func TestIntroBlocks_NoBotIDDropsTheHints(t *testing.T) {
	a := testAdapterWithRoster(&rosterErrThenAgents{agents: []pkga2a.AgentInfo{{Name: "sre-agent", DisplayName: "SRE Agent"}}})
	a.DefaultAgent = "sre-agent"
	a.botResolved = true
	_, blocks := a.introBlocks(context.Background(), assistantGreetingText, assistantGreetingContext)
	require.Len(t, blocks, 1, "only the section")
	require.Equal(t, bkSection, blocks[0].(map[string]any)[bkType])
}
