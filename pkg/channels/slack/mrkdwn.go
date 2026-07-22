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
	// bufHasOpenFence tracks whether the current buffer contains an unclosed
	// fence opening (original or reopened). When the reopen overhead does not fit
	// maxLen the continuation is emitted as a plain split with no opening, and
	// emit must not append a close to it (a stray ``` would open a fence when
	// rendered).
	bufHasOpenFence := false

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
		hadOpenFence := bufHasOpenFence
		b.Reset()
		bufHasOpenFence = false
		if inFence && s == fenceOpen {
			return
		}
		if inFence && hadOpenFence {
			s = closeFence(s)
		}
		chunks = append(chunks, s)
	}

	for line := range strings.Lines(text) {
		// Inside a fence a chunk carries invisible overhead: the auto-close
		// appended by emit, plus the reopened fence line when the chunk starts
		// fresh. Both count against maxLen so no emitted chunk ever exceeds it.
		// When the reopen itself does not fit (pathologically long fence info
		// string) the continuation degrades to a plain split: no reopen, no
		// auto-close, full budget. Losing code formatting beats emitting a chunk
		// Slack rejects outright.
		reopenViable := !inFence || len(fenceOpen)+len("\n```") < maxLen
		budget := maxLen
		if inFence && reopenViable {
			budget = maxLen - len(fenceOpen) - len("\n```")
		}
		isFenceLine := strings.HasPrefix(strings.TrimLeft(line, " \t"), "```")
		// A single line over the budget cannot share a chunk: flush what we have,
		// then hard-split it. While inside a fence each piece is wrapped in its
		// own fenced block so code formatting survives. The fence toggle below
		// still applies to a hard-split fence line, so fence state stays in sync
		// with the input even though the line never enters the buffer.
		if len(line) > budget {
			emit()
			for _, piece := range splitAtLines(line, budget) {
				if inFence && reopenViable {
					chunks = append(chunks, closeFence(fenceOpen+piece))
				} else {
					chunks = append(chunks, piece)
				}
			}
			if isFenceLine {
				if inFence {
					inFence, fenceOpen = false, ""
				} else {
					inFence, fenceOpen = true, line
				}
			}
			continue
		}
		overhead := 0
		switch {
		case inFence && bufHasOpenFence:
			overhead = len("\n```")
		case inFence && b.Len() == 0 && reopenViable:
			overhead = len(fenceOpen) + len("\n```")
		case !inFence && isFenceLine:
			// This line opens a fence on the current buffer: once appended the
			// buffer holds an open fence, so emit auto-closes it. Budget that
			// close now, else a near-full buffer plus the close bytes overshoots
			// maxLen and Slack rejects the block.
			overhead = len("\n```")
		}
		if b.Len() > 0 && b.Len()+len(line)+overhead > maxLen {
			emit()
		}
		if inFence && b.Len() == 0 && reopenViable {
			b.WriteString(fenceOpen) // reopen for the continuation chunk
			bufHasOpenFence = true
		}
		if isFenceLine {
			if inFence {
				inFence, fenceOpen = false, ""
				bufHasOpenFence = false
			} else {
				inFence, fenceOpen = true, line
				bufHasOpenFence = true
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

// escapeMrkdwn escapes the three characters Slack's mrkdwn parser treats as
// control sequences (&, <, >), per the Slack formatting rules. Agent-rendered
// text must pass through this before entering an mrkdwn context (section
// blocks, plain chat.postMessage text): otherwise content the agent quotes,
// such as a log line containing <!channel> or <@U...>, triggers real
// notifications inside bot-branded messages. Block Kit markdown blocks do not
// parse these sequences and must not be escaped.
func escapeMrkdwn(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// codeSpanSafe makes untrusted text safe to embed in a `code span`: backticks
// would terminate the span and newlines would carry injected markdown onto
// their own line, so both are replaced.
func codeSpanSafe(s string) string {
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
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
