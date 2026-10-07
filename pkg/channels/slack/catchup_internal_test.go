package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Thread 100.000, muted at 100.010, unmuted by the message at 100.900.

func TestRenderCatchUp_KeepsOnlyTheMutedPeriod(t *testing.T) {
	got := renderCatchUp(fullRead([]threadMessage{
		{TS: "100.000", User: "u1", Text: "the root"},
		{TS: "100.005", User: "u1", Text: "before the mute"},
		{TS: "100.010", User: "u1", Text: "mute"},
		{TS: "100.020", User: "UBOT", Text: "Muted."},
		{TS: "100.030", User: "u2", Text: "meanwhile"},
		{TS: "100.900", User: "u1", Text: "the mention"},
		{TS: "100.950", User: "u2", Text: "after the mention"},
	}), "100.000", "100.010", "100.900", "UBOT", upper)

	require.Equal(t, strings.Join([]string{
		"[messages written in this thread while the agent was muted: 1 message, oldest first]",
		"1970-01-01 00:01 NAME-U2: meanwhile",
	}, "\n"), got)
}

// The root is dropped by its ts even when it sorts after the mute, which no
// real thread does: the check does not lean on the order.
func TestRenderCatchUp_DropsTheRootByItsTS(t *testing.T) {
	got := renderCatchUp(fullRead([]threadMessage{
		{TS: "100.500", User: "u1", Text: "the root"},
		{TS: "100.600", User: "u2", Text: "meanwhile"},
	}), "100.500", "100.010", "100.900", "UBOT", upper)

	require.NotContains(t, got, "the root")
	require.Contains(t, got, "meanwhile")
}

// Nothing pins the oldest line: the cap keeps the newest lines that fit.
func TestRenderCatchUp_CapKeepsTheNewest(t *testing.T) {
	long := strings.Repeat("x", 4000)
	var msgs []threadMessage
	for i := range 5 {
		msgs = append(msgs, threadMessage{TS: fmt.Sprintf("100.%03d", 100+i), User: "u1", Text: fmt.Sprintf("m%d ", i) + long})
	}
	got := renderCatchUp(fullRead(msgs), "100.000", "100.010", "100.900", "UBOT", upper)

	require.Contains(t, got, "muted: 5 messages, the most recent 12,000 characters shown]")
	require.NotContains(t, got, "m0 ", "the oldest line is not pinned")
	require.NotContains(t, got, "m2 ", "three lines of about 4,030 characters do not fit")
	require.Contains(t, got, "m3 ")
	require.Contains(t, got, "m4 ")
	_, body, _ := strings.Cut(got, "\n")
	require.LessOrEqual(t, len([]rune(body)), threadContextMaxChars)
}

// A newest line over the cap on its own is cut, never dropped.
func TestRenderCatchUp_OversizedNewestLineIsCut(t *testing.T) {
	got := renderCatchUp(fullRead([]threadMessage{
		{TS: "100.100", User: "u1", Text: "older"},
		{TS: "100.200", User: "u1", Text: strings.Repeat("y", 20000)},
	}), "100.000", "100.010", "100.900", "UBOT", upper)

	require.Contains(t, got, "muted: 2 messages, the most recent 12,000 characters shown]")
	require.Contains(t, got, "NAME-U1: yyy")
	require.NotContains(t, got, "older")
	_, body, _ := strings.Cut(got, "\n")
	require.Len(t, []rune(body), threadContextMaxChars)
}

func TestRenderCatchUp_PartialReadSaysSo(t *testing.T) {
	got := renderCatchUp(threadRead{Messages: []threadMessage{
		{TS: "100.100", User: "u1", Text: "one"},
		{TS: "100.200", User: "u2", Text: "two"},
	}}, "100.000", "100.010", "100.900", "UBOT", upper)

	require.Contains(t, got, "[messages written in this thread while the agent was muted: 2 messages (the read stopped early), oldest first]")
}

func TestRenderCatchUp_NothingWrittenRendersNothing(t *testing.T) {
	require.Empty(t, renderCatchUp(fullRead([]threadMessage{
		{TS: "100.000", User: "u1", Text: "the root"},
		{TS: "100.010", User: "u1", Text: "mute"},
		{TS: "100.020", User: "UBOT", Text: "Muted."},
		{TS: "100.900", User: "u1", Text: "the mention"},
	}), "100.000", "100.010", "100.900", "UBOT", upper))
}
