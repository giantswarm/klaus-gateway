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
		Creator: "badger@example.com",
		Scheme:  "https", Host: "proxy.golang.org", Port: 443,
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
	require.Contains(t, text, "Egress refused for Otter Coder")
	require.Contains(t, text, "`proxy.golang.org:443` is not one of the hosts *Otter Coder* may reach")
	require.Contains(t, text, "Declare `https://proxy.golang.org`")
	require.Contains(t, text, "3 requests in 1 Session since Oct 19 10:42 UTC")
	require.Contains(t, text, "the next notice for it comes after 1 hour")
	require.Equal(t, []string{"refusal_open_agent"}, actionIDs(blocksOf(posts[0])))

	second := goModulesRefusal(at.Add(5 * time.Minute))
	second.Count, second.Sessions = 2, []string{"s1", "s2"}
	require.NoError(t, note(second))
	require.Len(t, fake.pathCalls("chat.postMessage"), 1, "a refusal of the same host inside the window posts nothing new")
	updated := latestUpdateText(fake, "1700000200.000001")
	require.Contains(t, updated, "5 requests in 2 Sessions since Oct 19 10:42 UTC, last at Oct 19 10:47 UTC")
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
	require.Contains(t, text, "`api.anthropic.com:443` is not one of the hosts")
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

func TestRefusal_RawTCPSaysHTTPSOnly(t *testing.T) {
	fake, note := refusalHarness(t)
	r := goModulesRefusal(time.Now())
	r.Scheme, r.Host, r.Port, r.AgentURL = "tcp", "github.com", 22, ""

	require.NoError(t, note(r))
	blocks := blocksOf(fake.pathCalls("chat.postMessage")[0])
	require.Contains(t, messageText(blocks), "opened a raw TCP connection to `github.com:22`; agents reach hosts over HTTPS only")
	require.Empty(t, actionIDs(blocks), "no link without an agent URL")
}

func TestRefusal_UnknownCreatorIsAddresseeNotFound(t *testing.T) {
	fake, note := refusalHarness(t)
	r := goModulesRefusal(time.Now())
	r.Creator = "nobody@example.com"

	require.ErrorIs(t, note(r), channels.ErrAddresseeNotFound)
	require.Empty(t, fake.pathCalls("chat.postMessage"))
}
