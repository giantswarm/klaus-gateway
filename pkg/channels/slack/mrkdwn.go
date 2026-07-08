package slack

import (
	"strings"
	"unicode/utf8"
)

// splitMarkdown splits text into chunks of at most maxLen bytes for Block Kit
// markdown blocks, breaking on line boundaries and never inside a fenced code
// block: when a chunk boundary (or the streaming tail) falls inside an open
// fence, the fence is closed at the chunk's end and reopened at the next chunk's
// start, so every chunk is self-contained, balanced Markdown. A single line
// longer than maxLen is hard-split via splitAtLines, with each piece wrapped in
// the fence while inside one.
//
// Packing is greedy left-to-right, so every non-final chunk boundary is stable
// as text accumulates across flushes: the streamed tail messages (tailTS) keep
// their content and are never rewritten with shifted text. Preserve this if
// editing.
func splitMarkdown(text string, maxLen int) []string {
	var chunks []string
	var b strings.Builder
	inFence := false
	fenceOpen := "" // the opening fence line (info string + newline) to reopen with

	// closeFence appends the closing ``` to s when a chunk ends mid-fence.
	closeFence := func(s string) string {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		return s + "```"
	}

	// emit finalizes the current buffer as a chunk, closing an open fence.
	// A buffer holding only the reopened fence line (no content) is dropped
	// rather than emitted as an empty code block; the reopen happens again on the
	// next content line.
	emit := func() {
		if b.Len() == 0 {
			return
		}
		s := b.String()
		b.Reset()
		if inFence && s == fenceOpen {
			return
		}
		if inFence {
			s = closeFence(s)
		}
		chunks = append(chunks, s)
	}

	for line := range strings.Lines(text) {
		// A single line over the cap cannot share a chunk: flush what we have,
		// then hard-split it. While inside a fence each piece is wrapped in its
		// own fenced block so code formatting survives.
		if len(line) > maxLen {
			emit()
			budget := maxLen
			if inFence {
				if r := maxLen - len(fenceOpen) - len("\n```"); r > 0 {
					budget = r
				}
			}
			for _, piece := range splitAtLines(line, budget) {
				if inFence {
					chunks = append(chunks, closeFence(fenceOpen+piece))
				} else {
					chunks = append(chunks, piece)
				}
			}
			continue
		}
		if b.Len() > 0 && b.Len()+len(line) > maxLen {
			emit()
		}
		if inFence && b.Len() == 0 {
			b.WriteString(fenceOpen) // reopen for the continuation chunk
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

	emit()
	if len(chunks) == 0 {
		chunks = append(chunks, "")
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
