package slack

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// knownLooks is an AgentCardResolver that knows how some agents look, the
// way the a2a client does once any caller's roster listed them.
type knownLooks map[string]string

func (k knownLooks) CardIdentity(_ context.Context, agentRef string) (string, string) {
	return k[agentRef], ""
}

// introAdapter is an adapter whose card resolver knows the SRE agent's look.
func introAdapter() *Adapter {
	a := &Adapter{AgentCards: knownLooks{"sre-agent": "SRE Agent"}, Logger: slog.New(slog.DiscardHandler)}
	a.gw = newMemoryRecorder()
	return a
}

// The intro and the greeting run where no person's token is at hand; the
// default agent is named by how it last looked.
func TestIntroBlocks(t *testing.T) {
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
			"Swarmgeist connects this channel to Giant Swarm's agents. Mention <@UBOT> to start a thread with *SRE Agent*, or mention it with `agents` to pick another agent. Agents work with your permissions and ask before making changes.",
			"<@UBOT> `help` lists the commands",
		},
		"greeting names the default agent": {
			"sre-agent", assistantGreetingText, assistantGreetingContext,
			"Ask about a cluster, an alert or a deployment. Swarmgeist routes each conversation to an agent, *SRE Agent* by default. Agents work with your permissions and ask before making changes.",
			"<@UBOT> `help` lists the commands · <@UBOT> `agents` lists the agents",
		},
		"greeting on an agent off the roster uses its name": {
			"kagent/swarmgeist", assistantGreetingText, assistantGreetingContext,
			"Ask about a cluster, an alert or a deployment. Swarmgeist routes each conversation to an agent, *swarmgeist* by default. Agents work with your permissions and ask before making changes.",
			"<@UBOT> `help` lists the commands · <@UBOT> `agents` lists the agents",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := introAdapter()
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
	a := introAdapter()
	a.DefaultAgent = "sre-agent"
	a.botResolved = true
	_, blocks := a.introBlocks(context.Background(), assistantGreetingText, assistantGreetingContext)
	require.Len(t, blocks, 1, "only the section")
	require.Equal(t, bkSection, blocks[0].(map[string]any)[bkType])
}
