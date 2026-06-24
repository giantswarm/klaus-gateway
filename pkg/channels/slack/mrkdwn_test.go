package slack

import (
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
