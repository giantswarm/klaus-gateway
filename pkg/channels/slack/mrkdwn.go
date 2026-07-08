package slack

import (
	"strings"
	"unicode/utf8"
)

// splitMarkdown splits text into chunks of at most maxLen bytes for Block Kit
// markdown blocks, breaking on line boundaries and never inside a fenced code
// block: when a chunk boundary (or the streaming tail) falls inside an open
// fence, the fence is closed at the chunk's end and reopened at the next chunk's
// start, so every chunk is self-contained, balanced Markdown. A single line longer
// than maxLen is hard-split via splitAtLines.
func splitMarkdown(text string, maxLen int) []string {
	var chunks []string
	var b strings.Builder
	inFence := false
	fenceOpen := "" // the opening fence line (info string + newline) to reopen with

	emit := func() {
		s := b.String()
		b.Reset()
		if inFence {
			if !strings.HasSuffix(s, "\n") {
				s += "\n"
			}
			s += "```"
			b.WriteString(fenceOpen) // reopen for the continuation chunk
		}
		chunks = append(chunks, s)
	}

	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" { // trailing element from SplitAfter
			continue
		}
		if b.Len() > 0 && b.Len()+len(line) > maxLen {
			emit()
		}
		if len(line) > maxLen {
			if b.Len() > 0 {
				emit()
			}
			chunks = append(chunks, splitAtLines(line, maxLen)...)
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			if inFence {
				inFence, fenceOpen = false, ""
			} else {
				inFence, fenceOpen = true, line
			}
		}
		b.WriteString(line)
	}

	s := b.String()
	if inFence {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += "```"
	}
	if s != "" || len(chunks) == 0 {
		chunks = append(chunks, s)
	}
	return chunks
}

// splitAtLines splits text into chunks of at most maxLen bytes at line boundaries,
// staying within Slack's 40 000-character message limit.
//
// ponytail: a code fence longer than maxLen is split like any other text, so the
// closing ``` lands in a later chunk and both halves render unfenced. Acceptable
// until replies routinely carry single fences over ~39 KB; the upgrade path is to
// split fenced regions on their own and re-open the fence in the continuation.
func splitAtLines(text string, maxLen int) []string {
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		cut := strings.LastIndex(text[:maxLen], "\n")
		if cut > 0 {
			cut++ // keep the newline with the preceding chunk
		} else {
			// No line boundary in range: hard-cut, backing off to a rune
			// boundary so a multi-byte glyph is never split (Slack rejects
			// invalid UTF-8). At most 3 bytes of back-off for a 4-byte rune.
			cut = maxLen
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			if cut == 0 { // pathological: maxLen smaller than a single rune
				cut = maxLen
			}
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}
