package slack

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestSplitMarkdown(t *testing.T) {
	t.Run("fits in one chunk", func(t *testing.T) {
		require.Equal(t, []string{"hello world"}, splitMarkdown("hello world", 100))
	})

	t.Run("rolls over on line boundary", func(t *testing.T) {
		text := "line one\nline two\nline three\n"
		chunks := splitMarkdown(text, 12)
		require.Greater(t, len(chunks), 1)
		require.Equal(t, text, strings.Join(chunks, ""))
	})

	t.Run("closes and reopens a fence across a boundary", func(t *testing.T) {
		// A code fence spanning a chunk boundary must be closed at the end of the
		// first chunk and reopened at the start of the next, so each chunk is
		// balanced Markdown.
		text := "```go\n" + strings.Repeat("x := 1\n", 10) + "```\n"
		chunks := splitMarkdown(text, 24)
		require.Greater(t, len(chunks), 1)
		for i, c := range chunks {
			require.Equal(t, 0, countFences(c)%2, "chunk %d has an unbalanced fence: %q", i, c)
		}
		require.True(t, strings.HasPrefix(chunks[1], "```go"), "continuation reopens the fence: %q", chunks[1])
	})

	t.Run("auto-closes an unterminated trailing fence (mid-stream)", func(t *testing.T) {
		// During streaming the closing ``` has not arrived yet; the rendered chunk
		// must still be balanced.
		chunks := splitMarkdown("text\n```go\nfmt.Println", 100)
		require.Len(t, chunks, 1)
		require.Equal(t, 2, countFences(chunks[0]), "trailing fence auto-closed: %q", chunks[0])
		require.True(t, strings.HasSuffix(chunks[0], "```"))
	})

	t.Run("empty input", func(t *testing.T) {
		require.Equal(t, []string{""}, splitMarkdown("", 100))
	})

	t.Run("long line inside a fence stays fenced and balanced", func(t *testing.T) {
		// A single line longer than the cap, inside a code fence (e.g. a minified
		// JSON blob or long log line from a tool). Every chunk must be balanced
		// and content must stay inside a fence, not leak out as plain text.
		long := strings.Repeat("A", 60)
		chunks := splitMarkdown("```go\n"+long+"\n```\n", 24)

		total := 0
		for i, c := range chunks {
			require.Equal(t, 0, countFences(c)%2, "chunk %d unbalanced: %q", i, c)
			if n := strings.Count(c, "A"); n > 0 {
				require.GreaterOrEqual(t, countFences(c), 2, "content chunk not fenced: %q", c)
				total += n
			}
		}
		require.Equal(t, 60, total, "all content preserved")
	})
}

func countFences(s string) int {
	n := 0
	for line := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			n++
		}
	}
	return n
}

func TestSplitAtLines(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		require.Empty(t, splitAtLines("", 10))
	})

	t.Run("within limit", func(t *testing.T) {
		require.Equal(t, []string{"hello"}, splitAtLines("hello", 10))
	})

	t.Run("exactly limit", func(t *testing.T) {
		require.Equal(t, []string{"hello"}, splitAtLines("hello", 5))
	})

	t.Run("over limit splits at newline", func(t *testing.T) {
		text := "line one\nline two\nline three"
		chunks := splitAtLines(text, 9)
		require.Equal(t, "line one\n", chunks[0])
		require.Greater(t, len(chunks), 1)
		require.Equal(t, text, strings.Join(chunks, ""))
	})

	t.Run("no newline in chunk causes hard cut", func(t *testing.T) {
		text := "abcdefghij"
		chunks := splitAtLines(text, 4)
		require.Equal(t, "abcd", chunks[0])
		require.Equal(t, text, strings.Join(chunks, ""))
	})

	t.Run("reassembles to original", func(t *testing.T) {
		text := "alpha\nbeta\ngamma\ndelta\nepsilon"
		require.Equal(t, text, strings.Join(splitAtLines(text, 12), ""))
	})

	t.Run("hard cut never splits a multi-byte rune", func(t *testing.T) {
		// Four 3-byte runes, no newline; every cut window lands mid-rune.
		text := "日本語東"
		for maxLen := 4; maxLen <= len(text); maxLen++ {
			chunks := splitAtLines(text, maxLen)
			require.Equal(t, text, strings.Join(chunks, ""))
			for _, c := range chunks {
				require.True(t, utf8.ValidString(c), "chunk %q is not valid UTF-8 (maxLen=%d)", c, maxLen)
			}
		}
	})
}
