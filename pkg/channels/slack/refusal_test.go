package slack_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func goModulesRefusal(at time.Time) channels.EgressRefusal {
	return channels.EgressRefusal{
		Namespace: "org-otter", Agent: "otter-coder", DisplayName: "Otter Coder",
		Creator: "badger@example.com", Runtime: "kagent",
		Scheme: "https", Host: "proxy.golang.org", Port: 443,
		Count: 3, Sessions: []string{"s1"}, At: at,
		AgentURL: "https://portal.example.com/agents/org-otter/otter-coder",
	}
}

func refusalHarness(t *testing.T) (*fakeSlackAPI, func(channels.EgressRefusal) error) {
	t.Helper()
	a, _, fake := teamReviewHarness(t, &recordingTools{})
	fake.setResponder("users.lookupByEmail", func(p map[string]any) string {
		if p["email"] == "badger@example.com" {
			return `{"ok":true,"user":{"id":"UBADGER"}}`
		}
		return `{"ok":false,"error":"users_not_found"}`
	})
	posted := 0
	fake.setResponder("chat.postMessage", func(map[string]any) string {
		posted++
		return fmt.Sprintf(`{"ok":true,"channel":"DBADGER","ts":"1700000200.00000%d"}`, posted)
	})
	a.RefusalWindow = time.Hour
	return fake, func(r channels.EgressRefusal) error { return a.NoteRefusal(context.Background(), r) }
}

func messageText(blocks []map[string]any) string {
	var b strings.Builder
	for _, block := range blocks {
		fmt.Fprintf(&b, "%v\n", block)
	}
	return b.String()
}

func TestRefusal_OneNoticePerAgentAndHostWithinTheWindow(t *testing.T) {
	fake, note := refusalHarness(t)
	at := time.Date(2026, 10, 19, 10, 42, 0, 0, time.UTC)

	require.NoError(t, note(goModulesRefusal(at)))
	posts := fake.pathCalls("chat.postMessage")
	require.Len(t, posts, 1)
	require.Equal(t, "UBADGER", posts[0].params["channel"], "the creator gets a direct message")
	text := messageText(blocksOf(posts[0]))
	require.Contains(t, text, "Outgoing connection to proxy.golang.org blocked")
	require.Contains(t, text, "Otter Coder is not allowed to reach this address · 3 attempts in 1 Session since Oct 19 10:42 UTC")
	require.Contains(t, text, "You can allow it: add `https://proxy.golang.org` to Otter Coder’s Extra egress origins.")
	require.Contains(t, text, "Technical details: destination `proxy.golang.org:443 (https)`, origin `https://proxy.golang.org`")
	require.Contains(t, text, "the next notice for it comes after 1 hour")
	require.Equal(t, []string{"refusal_open_agent"}, actionIDs(blocksOf(posts[0])))
	require.Equal(t, "Edit Otter Coder", buttonLabel(blocksOf(posts[0])))

	second := goModulesRefusal(at.Add(5 * time.Minute))
	second.Count, second.Sessions = 2, []string{"s1", "s2"}
	require.NoError(t, note(second))
	require.Len(t, fake.pathCalls("chat.postMessage"), 1, "a refusal of the same host inside the window posts nothing new")
	updated := latestUpdateText(fake, "1700000200.000001")
	require.Contains(t, updated, "5 attempts in 2 Sessions since Oct 19 10:42 UTC, last at Oct 19 10:47 UTC")
	require.Equal(t, "DBADGER", fake.pathCalls("chat.update")[0].params["channel"], "the notice is rewritten in its conversation")

	other := goModulesRefusal(at.Add(6 * time.Minute))
	other.Host = "sum.golang.org"
	require.NoError(t, note(other))
	require.Len(t, fake.pathCalls("chat.postMessage"), 2, "a second host is a second notice")
}

func TestRefusal_DestinationIsNotLinkified(t *testing.T) {
	fake, note := refusalHarness(t)
	r := goModulesRefusal(time.Now())
	r.Host = "api.anthropic.com"

	require.NoError(t, note(r))
	blocks := blocksOf(fake.pathCalls("chat.postMessage")[0])
	text := messageText(blocks)
	require.Contains(t, text, "destination `api.anthropic.com:443 (https)`")
	require.NotContains(t, text, "<http", "the notice carries no link markup")
	for _, block := range blocks {
		switch block["type"] {
		case "section":
			require.Equal(t, true, block["text"].(map[string]any)["verbatim"],
				"Slack must not turn api.anthropic.com:443 into an http://…:443 link")
		case "context":
			for _, el := range block["elements"].([]any) {
				require.Equal(t, true, el.(map[string]any)["verbatim"])
			}
		}
	}
}

func TestRefusal_CodingAgentIsThePlatformTeams(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, harnessTitle, says string
	}{
		{"claude", "claude", "", "Otter Coder can only reach the addresses the platform allows for Claude Code. Ask your platform team to allow registry.npmjs.org."},
		{"codex", "codex", "", "allows for Codex."},
		{"byo", "byo", "", "allows for Custom image."},
		{"display name", "claude", "Claude Code with Go", "allows for Claude Code with Go."},
		{"unknown coding runtime", "omp", "", "allows for omp."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, note := refusalHarness(t)
			r := goModulesRefusal(time.Now())
			r.Host, r.Runtime, r.HarnessTitle = "registry.npmjs.org", tc.runtime, tc.harnessTitle

			require.NoError(t, note(r))
			blocks := blocksOf(fake.pathCalls("chat.postMessage")[0])
			text := messageText(blocks)
			require.Contains(t, text, tc.says)
			require.NotContains(t, text, "Extra egress origins", "a coding agent's egress is the platform's: no editor can add an origin")
			require.Empty(t, actionIDs(blocks), "the edit page cannot allow it")
		})
	}
}

func TestRefusal_UnknownRuntimePromisesNothing(t *testing.T) {
	fake, note := refusalHarness(t)
	r := goModulesRefusal(time.Now())
	r.Runtime = ""

	require.NoError(t, note(r))
	blocks := blocksOf(fake.pathCalls("chat.postMessage")[0])
	require.Contains(t, messageText(blocks), "If Otter Coder needs proxy.golang.org, ask your platform team.")
	require.Empty(t, actionIDs(blocks))
}

func TestRefusal_LaterRefusalUpdatesTheRemedy(t *testing.T) {
	fake, note := refusalHarness(t)
	at := time.Now()
	r := goModulesRefusal(at)
	r.Runtime = ""
	require.NoError(t, note(r))

	r = goModulesRefusal(at.Add(time.Minute))
	r.Runtime = "claude"
	require.NoError(t, note(r))
	require.Contains(t, latestUpdateText(fake, "1700000200.000001"), "allows for Claude Code. Ask your platform team")
}

func TestRefusal_RawTCP(t *testing.T) {
	fake, note := refusalHarness(t)
	r := goModulesRefusal(time.Now())
	r.Scheme, r.Host, r.Port = "tcp", "github.com", 22

	require.NoError(t, note(r))
	blocks := blocksOf(fake.pathCalls("chat.postMessage")[0])
	text := messageText(blocks)
	require.Contains(t, text, "Raw TCP connections are not available to agents · 3 attempts")
	require.Contains(t, text, "Otter Coder’s egress settings list the HTTP and HTTPS origins it may reach.")
	require.Contains(t, text, "Technical details: destination `github.com:22 (tcp)`")
	require.NotContains(t, text, "origin `")
	require.Equal(t, []string{"refusal_open_agent"}, actionIDs(blocks))

	fake, note = refusalHarness(t)
	r.AgentURL = ""
	require.NoError(t, note(r))
	require.Empty(t, actionIDs(blocksOf(fake.pathCalls("chat.postMessage")[0])), "no link without an agent URL")
}

func TestRefusal_UnknownCreatorIsAddresseeNotFound(t *testing.T) {
	fake, note := refusalHarness(t)
	r := goModulesRefusal(time.Now())
	r.Creator = "nobody@example.com"

	require.ErrorIs(t, note(r), channels.ErrAddresseeNotFound)
	require.Empty(t, fake.pathCalls("chat.postMessage"))
}

func buttonLabel(blocks []map[string]any) string {
	for _, block := range blocks {
		if block["type"] != "actions" {
			continue
		}
		for _, el := range block["elements"].([]any) {
			return el.(map[string]any)["text"].(map[string]any)["text"].(string)
		}
	}
	return ""
}
