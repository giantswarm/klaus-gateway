package slack

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reMrkdwnHeading       = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reMrkdwnBold          = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMrkdwnLink          = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reMrkdwnListDash      = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)
	reMrkdwnStrikethrough = regexp.MustCompile(`~~(.+?)~~`)
	reMrkdwnCodeFence     = regexp.MustCompile("(?s)```[a-z]*\\n?.*?```")
)

// markdownToMrkdwn converts a Markdown string to Slack mrkdwn format.
// Code fences are preserved unchanged. Headings become bold lines. Bold,
// links, lists, and strikethrough are converted to mrkdwn equivalents.
// Italic (`_x_`) already shares Slack's syntax, so it passes through untouched.
func markdownToMrkdwn(text string) string {
	// Preserve code fences first (protect their content from other transforms).
	type fence struct{ placeholder, original string }
	var fences []fence
	text = reMrkdwnCodeFence.ReplaceAllStringFunc(text, func(s string) string {
		ph := fmt.Sprintf("\x00fence%d\x00", len(fences))
		fences = append(fences, fence{ph, s})
		return ph
	})

	// Protect a trailing, still-unterminated fence too. During streaming the
	// closing ``` may not have arrived yet; without this its body would be
	// mangled by the bold/italic/link transforms below until it closes.
	if idx := strings.Index(text, "```"); idx >= 0 {
		ph := fmt.Sprintf("\x00fence%d\x00", len(fences))
		fences = append(fences, fence{ph, text[idx:]})
		text = text[:idx] + ph
	}

	// Headings: # Foo → *Foo*
	text = reMrkdwnHeading.ReplaceAllString(text, "*$1*")
	// Bold: **x** → *x*
	text = reMrkdwnBold.ReplaceAllString(text, "*$1*")
	// Links: [text](url) → <url|text>
	text = reMrkdwnLink.ReplaceAllString(text, "<$2|$1>")
	// Unordered lists: - item / * item → • item
	text = reMrkdwnListDash.ReplaceAllString(text, "${1}• ")
	// Strikethrough: ~~x~~ → ~x~
	text = reMrkdwnStrikethrough.ReplaceAllString(text, "~$1~")

	// Restore code fences.
	for _, f := range fences {
		text = strings.ReplaceAll(text, f.placeholder, f.original)
	}
	return text
}

// splitAtLines splits text into chunks of at most maxLen bytes at line boundaries,
// staying within Slack's 40 000-character message limit.
func splitAtLines(text string, maxLen int) []string {
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		cut := strings.LastIndex(text[:maxLen], "\n")
		if cut <= 0 {
			cut = maxLen
		} else {
			cut++
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}
