package slack

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The notice names the lifetime an operator configured, never a shorter one:
// spellDuration counts the largest unit the duration is a whole multiple of,
// so 36 hours is not "1 day" and 90 minutes is not "1 hour".
func TestSpellDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{90 * 24 * time.Hour, "90 days"},
		{24 * time.Hour, "1 day"},
		{48 * time.Hour, "2 days"},
		{36 * time.Hour, "36 hours"},
		{12 * time.Hour, "12 hours"},
		{time.Hour, "1 hour"},
		{90 * time.Minute, "90 minutes"},
		{30 * time.Minute, "30 minutes"},
		{time.Minute, "1 minute"},
		{45 * time.Second, "45 seconds"},
		{time.Second, "1 second"},
		{1500 * time.Millisecond, "1.5s"},
	} {
		require.Equal(t, tc.want, spellDuration(tc.d), "%s", tc.d)
	}
}

func TestCountOf(t *testing.T) {
	require.Equal(t, "1 day", countOf(1, "day"))
	require.Equal(t, "2 days", countOf(2, "day"))
	require.Equal(t, "1 hour", countOf(1, "hour"))
	require.Equal(t, "36 hours", countOf(36, "hour"))
	require.Equal(t, "1 minute", countOf(1, "minute"))
	require.Equal(t, "90 minutes", countOf(90, "minute"))
	require.Equal(t, "1 second", countOf(1, "second"))
	require.Equal(t, "45 seconds", countOf(45, "second"))
	require.Equal(t, "0 days", countOf(0, "day"))
}

// The notice reads as one sentence with the configured lifetime in it.
func TestThreadClosedNotice(t *testing.T) {
	require.Equal(t,
		"This conversation ended after 90 days without messages. Mention the bot to start a new one.",
		threadClosedNotice(90*24*time.Hour))
	require.Contains(t, threadClosedNotice(36*time.Hour), "after 36 hours without messages")
}

// The dismissed-connector note names the real cooldown, so a change to it
// cannot leave the note wrong.
func TestConnectorDismissedNotice(t *testing.T) {
	require.Equal(t, "Not asked again for 1 hour.", connectorDismissedNotice())
	require.Contains(t, connectorDismissedNotice(), spellDuration(connectorPromptCooldown))
}

// A terminal note is mrkdwn: Slack's error text in it is escaped.
func TestRenderFailedNote_EscapesTheReason(t *testing.T) {
	require.Equal(t, "The agent finished, but Slack refused the rest of the reply: a &lt;b&gt; &amp; c.",
		renderFailedNote(errors.New("a <b> & c")))
}

// In text mode a /stop before any answer posts the command's note and
// replaces the placeholder: the two must not be the same line.
func TestStopNotesDiffer(t *testing.T) {
	require.NotEqual(t, stoppedNote, stopStoppedNotice)
}

// A turn that failed after its reply started says it is over and names what
// broke by its class only, never by the error's text.
func TestInterruptedFailureNote(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("stream terminated by RST_STREAM with error code: INTERNAL_ERROR"), "a platform error"},
		{errors.New(`anthropic API error: 529 {"type":"overloaded_error"}`), "a model error"},
		{errors.New("failed to list MCP tools: connection reset by peer"), "a tool connection error"},
		{errors.New("403 authorization failed"), "a policy refusal"},
		{errors.New("boom"), "an error"},
	} {
		note := interruptedFailureNote(tc.err)
		require.Contains(t, note, "The turn ended with "+tc.want+" before the agent finished", "%v", tc.err)
		require.Contains(t, note, "Dev Portal")
		require.NotContains(t, note, tc.err.Error(), "the error's text stays out of the thread")
	}
}
