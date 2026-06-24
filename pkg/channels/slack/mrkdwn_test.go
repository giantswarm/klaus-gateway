package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarkdownToMrkdwn(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "heading h1",
			in:   "# Heading",
			want: "*Heading*",
		},
		{
			name: "heading h3",
			in:   "### Deep heading",
			want: "*Deep heading*",
		},
		{
			name: "bold",
			in:   "**bold**",
			want: "*bold*",
		},
		{
			name: "italic passes through unchanged",
			in:   "_italic_",
			want: "_italic_",
		},
		{
			name: "link",
			in:   "[text](https://example.com)",
			want: "<https://example.com|text>",
		},
		{
			name: "unordered list dash",
			in:   "- item",
			want: "• item",
		},
		{
			name: "unordered list star",
			in:   "* item",
			want: "• item",
		},
		{
			name: "strikethrough",
			in:   "~~strike~~",
			want: "~strike~",
		},
		{
			name: "code fence preserved",
			in:   "```go\nfmt.Println(\"hello\")\n```",
			want: "```go\nfmt.Println(\"hello\")\n```",
		},
		{
			name: "code fence content not transformed",
			in:   "```go\n**not bold** _not italic_\n```",
			want: "```go\n**not bold** _not italic_\n```",
		},
		{
			name: "mixed content",
			in:   "# Title\n**bold** and _italic_ with [link](https://x.com)\n- item1\n- item2",
			want: "*Title*\n*bold* and _italic_ with <https://x.com|link>\n• item1\n• item2",
		},
		{
			name: "no transformation needed",
			in:   "plain text",
			want: "plain text",
		},
		{
			name: "unterminated trailing fence is preserved",
			in:   "text **bold**\n```go\nfmt.Println",
			want: "text *bold*\n```go\nfmt.Println",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, markdownToMrkdwn(tc.in))
		})
	}
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
}
