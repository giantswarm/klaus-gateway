package channels

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// TitleMax is the cap kagent puts on a conversation's display name, in runes.
const TitleMax = 200

// TitleFrom renders text as a one-line title of at most max runes: control
// characters become spaces, whitespace runs collapse, and an over-long title is
// cut on a word boundary and marked with an ellipsis. Returns "" when nothing
// survives, which callers pass on as "unnamed" rather than inventing a title.
//
// The result is a name the kagent controller accepts as it stands: its display
// name rejects control characters anywhere and a leading or trailing separator,
// so normalising here is what keeps a create from being refused.
func TitleFrom(text string, max int) string {
	if max <= 0 {
		return ""
	}
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	// Cut a rune short of the cap so the ellipsis fits inside it. Whitespace is
	// collapsed by now, so the last space in that budget is the last word
	// boundary; a single word longer than the budget has none and takes the
	// hard cut.
	head := string([]rune(s)[:max-1])
	if i := strings.LastIndexByte(head, ' '); i > 0 {
		head = head[:i]
	}
	return head + "…"
}

// instanceName is the display name the conversation msg opens is created with:
// what the adapter made of the message where it renders a title itself, the
// message's own text everywhere else. Normalised either way, so an adapter
// cannot hand the controller a name it refuses.
func instanceName(msg InboundMessage) string {
	text := msg.Title
	if text == "" {
		text = msg.Text
	}
	return TitleFrom(text, TitleMax)
}
