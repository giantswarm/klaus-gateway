package channels

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestTitleFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"plain", "restart the kong pods", "restart the kong pods"},
		{"trimmed", "   restart the kong pods  ", "restart the kong pods"},
		{"newlines_collapse", "why is this failing?\n\n  kubectl get pods\n", "why is this failing? kubectl get pods"},
		{"tabs_collapse", "one\ttwo\t\tthree", "one two three"},
		{"control_characters_do_not_survive", "bell\a and \x00null", "bell and null"},
		{"empty", "", ""},
		{"whitespace_only", " \n\t ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, TitleFrom(tc.text, TitleMax))
		})
	}
}

func TestTitleFrom_Truncation(t *testing.T) {
	t.Run("cuts_on_a_word_boundary", func(t *testing.T) {
		// The cut lands a rune short of the cap and then backs up to the last
		// space, so the title never ends mid-word.
		got := TitleFrom(strings.Repeat("word ", 60), 20)
		require.Equal(t, "word word word…", got)
		require.LessOrEqual(t, utf8.RuneCountInString(got), 20)
	})

	t.Run("one_long_word_takes_the_hard_cut", func(t *testing.T) {
		got := TitleFrom(strings.Repeat("x", 40), 10)
		require.Equal(t, "xxxxxxxxx…", got)
		require.Equal(t, 10, utf8.RuneCountInString(got))
	})

	t.Run("multi_byte_runes_are_not_split", func(t *testing.T) {
		got := TitleFrom(strings.Repeat("ü", 40), 10)
		require.True(t, utf8.ValidString(got))
		require.Equal(t, 10, utf8.RuneCountInString(got))
	})

	t.Run("no_cut_below_the_cap", func(t *testing.T) {
		require.Equal(t, "short", TitleFrom("short", TitleMax))
	})

	t.Run("no_budget", func(t *testing.T) {
		require.Empty(t, TitleFrom("anything", 0))
	})
}

// A name the controller would refuse costs the conversation its create, so
// whatever comes out of here must satisfy its contract: at most TitleMax runes,
// no control characters, nothing to trim.
func TestTitleFrom_SatisfiesTheControllerContract(t *testing.T) {
	for _, text := range []string{
		"\x00\x01 leading control characters",
		"trailing control characters \a\b",
		" non-breaking space around ",
		strings.Repeat("überlange ", 400),
		strings.Repeat("x", 500),
	} {
		got := TitleFrom(text, TitleMax)
		require.LessOrEqual(t, utf8.RuneCountInString(got), TitleMax, "over the cap: %q", got)
		require.False(t, strings.ContainsFunc(got, unicode.IsControl), "holds a control character: %q", got)
		require.Equal(t, strings.TrimSpace(got), got, "not trimmed: %q", got)
	}
}

func TestInstanceName(t *testing.T) {
	t.Run("an_adapter_title_wins", func(t *testing.T) {
		msg := InboundMessage{Text: "/agent \"sre\" why is kong down?", Title: "why is kong down?"}
		require.Equal(t, "why is kong down?", instanceName(msg))
	})

	t.Run("falls_back_to_the_text", func(t *testing.T) {
		require.Equal(t, "why is kong down?", instanceName(InboundMessage{Text: "why is kong down?\n"}))
	})

	t.Run("an_adapter_title_is_normalised_too", func(t *testing.T) {
		require.Equal(t, "two lines", instanceName(InboundMessage{Title: " two\nlines "}))
	})

	t.Run("nothing_to_name_it_after", func(t *testing.T) {
		require.Empty(t, instanceName(InboundMessage{Attachments: []Attachment{{Filename: "graph.png"}}}))
	})
}
