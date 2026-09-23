package slack

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// Continuation of a turn across a gateway restart. The process that
// resubscribes to a turn left running is handed the whole answer when the
// task completes and none of the tool calls it missed, so on its own it would
// post the opening the previous process had already streamed a second time
// and number its steps from one again — reusing ids that are already on the
// reply. The writer therefore records, on the thread's routing-store row and
// as the turn streams, how much answer text has landed, which message it is
// landing in and how many step ids it has handed out (store.Delivered), and a
// writer that continues the turn is seeded with that record: it drops that
// many bytes of the answer and numbers its steps on from the recorded count.

// deliveredWriteTimeout bounds one write of the delivery record. The write
// runs on the writer's goroutine, after the Slack call it follows, so a store
// that does not answer slows the stream but cannot wedge the turn.
const deliveredWriteTimeout = 2 * time.Second

// continuedNothingNote closes a continued turn whose whole answer had already
// been posted before the restart, so the restart notice's promise of a post is
// kept without repeating the reply.
const continuedNothingNote = "_(done — the reply above is complete)_"

// continueFrom seeds the writer with what a previous process delivered of the
// turn it continues: the first TextLen bytes of answer text are dropped, the
// step ids count on from the recorded ones, and a stream the previous process
// left open is adopted, so the reply continues in the same message instead of
// a second one.
//
// A step the record names as still running is closed on the adopted stream
// straight away — queued here, not on this writer's first chunk — because a
// turn whose answer had all landed before the restart adds nothing to the queue
// and would otherwise close the message with the step spinning. A record that
// names none leaves the reply's steps exactly as they are.
func (w *batchedWriter) continueFrom(d store.Delivered) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.carried = d
	w.skipText = d.TextLen
	w.streamTS, w.streamed, w.streamAdopted = d.StreamTS, d.StreamLen, d.StreamTS != ""
	if d.StreamTS != "" {
		// The adopted message is one of the turn's streamed messages, so a
		// connector prompt taking the turn over retracts it like the rest.
		w.streamMessages = append(w.streamMessages, d.StreamTS)
	}
	w.stepsIssued = d.ToolSteps
	if d.StreamTS != "" && d.OpenStepID != "" {
		w.queue = append(w.queue, queuedChunk{step: &taskUpdate{
			id: d.OpenStepID, title: d.OpenStepTitle, status: stepComplete,
		}})
	}
}

// continued reports whether the writer continues a turn of which answer text
// had already been posted before it.
func (w *batchedWriter) continued() bool {
	return w.carried.TextLen > 0
}

// skipDelivered trims from content the answer text a previous process
// posted, as long as any is left to skip. The recorded length is a length of
// whole deltas, so the cut falls on a rune boundary. The whitespace the cut
// leaves at the front of what follows — the paragraph break before the
// agent's next sentence, usually — is dropped too, so the continued reply
// does not open with blank lines; it is counted with the delivered text so a
// second restart cuts at the right place.
func (w *batchedWriter) skipDelivered(content string) string {
	if w.skipText > 0 {
		n := min(w.skipText, len(content))
		w.skipText -= n
		content = content[n:]
		w.trimLead = w.skipText == 0
	}
	if w.trimLead {
		trimmed := strings.TrimLeftFunc(content, unicode.IsSpace)
		w.mu.Lock()
		w.leadTrimmed += len(content) - len(trimmed)
		w.mu.Unlock()
		content = trimmed
		w.trimLead = content == ""
	}
	return content
}

// noteDelivered records what the turn has delivered so far — the answer text
// that landed (what the previous process posted plus what this one appended),
// the stream it is landing in, and the step ids handed out — through the
// writer's sink, when it has one. Narration is deliberately absent from the
// text count: a continuing process replays the answer, never the narration, so
// counting it would cut the answer in the wrong place.
func (w *batchedWriter) noteDelivered(ctx context.Context) {
	if w.onDelivered == nil {
		return
	}
	w.mu.Lock()
	d := store.Delivered{
		TextLen:   w.carried.TextLen + w.leadTrimmed + w.appendedLen,
		StreamTS:  w.streamTS,
		StreamLen: w.streamed,
		ToolSteps: w.stepsIssued,
	}
	// The most recent step still running is the one a process continuing the
	// turn has to close; a turn with nothing running names none, so a step that
	// already finished is left alone.
	if n := len(w.openSteps); n > 0 {
		d.OpenStepID, d.OpenStepTitle = w.openSteps[n-1].id, w.openSteps[n-1].title
	}
	w.mu.Unlock()
	w.onDelivered(ctx, d)
}

// recordDelivered writes d onto the thread's row as what the turn in flight
// has delivered. Only a row that records a task takes it: without one there is
// nothing a later process could resubscribe to, and the facade drops the
// record with the task. Best effort, bounded, and detached from the turn's
// cancellation: the shutdown's last flush is the write that matters most.
func (a *Adapter) recordDelivered(ctx context.Context, slackChannel, threadID string, d store.Delivered) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveredWriteTimeout)
	defer cancel()
	err := a.records().UpdateThreadRecord(wctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		if !found || e.TaskID == "" {
			return false
		}
		e.Delivered = d
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: record of the turn's delivered reply not written", "thread", threadID, "error", err)
	}
}
