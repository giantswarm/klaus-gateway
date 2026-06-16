package slack

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reMrkdwnHeading       = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reMrkdwnBold          = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMrkdwnItalic        = regexp.MustCompile(`_(.+?)_`)
	reMrkdwnLink          = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reMrkdwnListDash      = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)
	reMrkdwnStrikethrough = regexp.MustCompile(`~~(.+?)~~`)
	reMrkdwnCodeFence     = regexp.MustCompile("(?s)```[a-z]*\\n?(.*?)```")
)

// markdownToMrkdwn converts a Markdown string to Slack mrkdwn format.
// Code fences are preserved unchanged. Headings become bold lines. Bold,
// italic, links, lists, and strikethrough are converted to mrkdwn equivalents.
func markdownToMrkdwn(text string) string {
	// Preserve code fences first (protect their content from other transforms).
	type fence struct{ placeholder, original string }
	var fences []fence
	text = reMrkdwnCodeFence.ReplaceAllStringFunc(text, func(s string) string {
		ph := fmt.Sprintf("\x00fence%d\x00", len(fences))
		fences = append(fences, fence{ph, s})
		return ph
	})

	// Headings: # Foo → *Foo*
	text = reMrkdwnHeading.ReplaceAllString(text, "*$1*")
	// Bold: **x** → *x*
	text = reMrkdwnBold.ReplaceAllString(text, "*$1*")
	// Italic: _x_ → _x_ (mrkdwn uses same syntax — ensure well-formed)
	text = reMrkdwnItalic.ReplaceAllString(text, "_${1}_")
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
