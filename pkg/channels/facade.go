package channels

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// AgentClient is the slice of pkg/a2a.Client the Facade needs to run a
// conversation on a kagent API v2 controller: the A2A v1 calls on an
// AgentInstance, the instance lifecycle, and the AgentTemplate roster.
type AgentClient interface {
	// Stream sends msg to the instance and yields the task's events.
	Stream(ctx context.Context, instanceID string, msg *a2apkg.Message) iter.Seq2[a2apkg.Event, error]
	// Subscribe attaches to a task already running on the instance and yields
	// its events; a task that has quiesced arrives whole, as the only event.
	Subscribe(ctx context.Context, instanceID string, taskID a2apkg.TaskID) iter.Seq2[a2apkg.Event, error]
	// GetTask returns one task of the instance.
	GetTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) (*a2apkg.Task, error)
	// CancelTask cancels a running task server-side.
	CancelTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) (*a2apkg.Task, error)
	// CreateInstance creates (idempotently per requestID) the instance for a
	// conversation with agentRef, named name, and returns it once it is ready.
	// An empty name leaves the conversation unnamed.
	CreateInstance(ctx context.Context, agentRef, requestID, name string) (pkga2a.Instance, error)
	// GetInstance returns an instance; pkga2a.ErrInstanceNotFound when gone.
	GetInstance(ctx context.Context, id string) (pkga2a.Instance, error)
	// DeleteInstance removes an instance; a missing one is not an error.
	DeleteInstance(ctx context.Context, id string) error
	// ListAgents lists the selectable agents of the served namespace.
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
}

// Facade wires the kagent client and the routing store together into the
// Gateway surface used by channel adapters.
type Facade struct {
	// Agent runs a channel turn on the thread's kagent AgentInstance.
	Agent AgentClient
	// Routes persists the thread -> AgentInstance binding across restarts.
	// Required when Agent is set.
	Routes store.Store
	// Durable reports whether Routes outlives the process. A turn a shutdown
	// cuts short is delivered afterwards only when its record survives; when it
	// cannot, channels tell the user where the result is instead of promising
	// to post it.
	Durable bool
	// ThreadTTL is the sliding lifetime of a thread's record and of its
	// AgentInstance binding: every turn refreshes it, and after it the
	// conversation has ended — routing, the binding and the grants read the
	// row as absent and the next mention starts the thread over. The row
	// itself stays for twice as long (storeTTL) so a reply in a thread that
	// ended can be told so; after that the store has forgotten the thread.
	// 0 never expires. main.go sets it from --thread-ttl.
	ThreadTTL time.Duration

	// now is the clock the facade stamps rows with; nil means time.Now.
	now func() time.Time
}

// SetNowFunc is a test hook: the clock the facade stamps rows with, so a test
// that ages a store's rows moves the facade's clock along with the store's.
func (f *Facade) SetNowFunc(fn func() time.Time) { f.now = fn }

func (f *Facade) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// ListAgents lists the agents a channel may select. Unavailable when no
// kagent client is configured.
func (f *Facade) ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	if f == nil || f.Agent == nil {
		return nil, errors.New("channels: no agent client configured")
	}
	return f.Agent.ListAgents(ctx)
}

// SessionResumable reports whether msg's thread is bound to an AgentInstance
// the controller still knows, so a reply resumes it rather than starting
// fresh. checked is false when no kagent client is configured or the lookup
// errored; exists is then meaningless and the caller should stay silent. A
// binding whose instance the controller no longer has is cleared — the thread
// keeps its agent, its initiator and its grants — so the next turn creates a
// fresh instance. The lookup is bounded by a short timeout: it sits before the
// turn, so a slow controller must not stall the first reply.
func (f *Facade) SessionResumable(ctx context.Context, msg InboundMessage) (exists, checked bool) {
	if f == nil || f.Agent == nil || f.Routes == nil {
		return false, false
	}
	ctx = withChannelAuth(ctx, msg)
	ctx, cancel := context.WithTimeout(ctx, sessionCheckTimeout)
	defer cancel()
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return false, false
	}
	entry, ok = f.liveEntry(entry, ok)
	if !ok || entry.AgentInstanceID == "" {
		return false, true
	}
	if _, err := f.Agent.GetInstance(ctx, entry.AgentInstanceID); err != nil {
		if !pkga2a.IsNotFound(err) {
			return false, false
		}
		_ = f.Routes.Update(ctx, key, clearBinding)
		return false, true
	}
	return true, true
}

// sessionCheckTimeout bounds the resume existence-check, which runs on the
// turn's critical path.
const sessionCheckTimeout = 3 * time.Second

// ResetSession deletes the AgentInstance bound to msg's thread and clears the
// binding — the thread keeps its agent, its initiator and its grants — so the
// next turn starts a fresh instance. Used when the conversation's history has
// become unusable (the model API rejects it on every turn). Returns false when
// no kagent client is configured or the thread has no binding.
func (f *Facade) ResetSession(ctx context.Context, msg InboundMessage) (bool, error) {
	if f == nil || f.Agent == nil || f.Routes == nil {
		return false, nil
	}
	ctx = withChannelAuth(ctx, msg)
	ctx, cancel := context.WithTimeout(ctx, sessionCheckTimeout)
	defer cancel()
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return false, err
	}
	if !ok || entry.AgentInstanceID == "" {
		return false, nil
	}
	if err := f.Agent.DeleteInstance(ctx, entry.AgentInstanceID); err != nil {
		return false, err
	}
	if err := f.Routes.Update(ctx, key, clearBinding); err != nil {
		return false, err
	}
	return true, nil
}

// SendCompletion streams a completion for msg: the turn runs on the
// AgentInstance msg's thread is bound to. A fresh turn that fails before it
// showed anything, on a failure a second attempt may get past
// (FailureClass.Retryable), is sent once more on the same instance and the
// channel sees only the second attempt: the runtime sets up its tool set and
// its MCP sessions again for every run, and a new instance would start the
// conversation over. A resume is sent once: the paused task it answers is
// gone once it failed.
//
// The caller must receive from the returned channel until it closes.
func (f *Facade) SendCompletion(ctx context.Context, msg InboundMessage) (<-chan OutboundDelta, error) {
	if f == nil || f.Agent == nil {
		return nil, errors.New("channels: no agent client configured")
	}
	deltas, err := f.sendViaA2A(ctx, msg)
	if msg.TaskID != "" {
		return deltas, err
	}
	if err != nil {
		if ctx.Err() != nil || !ClassifyFailure(err).Retryable() {
			return nil, err
		}
		noteRetry(ctx, msg, err)
		return f.sendViaA2A(ctx, msg)
	}
	return f.retryUnshown(ctx, msg, deltas), nil
}

// retryUnshown forwards a turn's deltas. When the task failed before any of it
// reached the channel — no text, narration, tool activity or prompt — on a
// retryable failure, the message is sent once more and that attempt's deltas
// are forwarded instead. Only a task the controller reports failed is sent
// again: it is over, where a stream that broke may leave a task running that
// a second message would run beside. Should the second send be refused, the
// first failure is what the turn reports: it names what broke.
func (f *Facade) retryUnshown(ctx context.Context, msg InboundMessage, deltas <-chan OutboundDelta) <-chan OutboundDelta {
	out := make(chan OutboundDelta, cap(deltas))
	go func() {
		defer close(out)
		retried, shown := false, false
		for d := range deltas {
			if !shown && !retried && ctx.Err() == nil && isTaskFailure(d.Err) && ClassifyFailure(d.Err).Retryable() {
				retried = true
				drainDeltas(deltas)
				noteRetry(ctx, msg, d.Err)
				if d.Usage != nil && !f.emit(ctx, out, OutboundDelta{Usage: d.Usage}) {
					return
				}
				next, err := f.sendViaA2A(ctx, msg)
				if err == nil {
					f.forward(ctx, out, next)
					return
				}
				slog.Warn("channels: the turn's second attempt was refused", "channel", msg.Channel, "thread", msg.ThreadID, "error", err)
			}
			shown = shown || d.shows()
			if !f.emit(ctx, out, d) {
				drainDeltas(deltas)
				return
			}
		}
	}()
	return out
}

// forward copies deltas to out until deltas closes, draining the rest once
// out's reader is gone.
func (f *Facade) forward(ctx context.Context, out chan<- OutboundDelta, deltas <-chan OutboundDelta) {
	for d := range deltas {
		if !f.emit(ctx, out, d) {
			drainDeltas(deltas)
			return
		}
	}
}

// taskEnded is a task's terminal state other than completed, with the
// runtime's (or the controller's) account of it as the error text.
type taskEnded struct {
	state  a2apkg.TaskState
	reason string
}

func (e *taskEnded) Error() string { return e.reason }

// isTaskFailure reports whether err is a task that ended failed.
func isTaskFailure(err error) bool {
	var ended *taskEnded
	return errors.As(err, &ended) && ended.state == a2apkg.TaskStateFailed
}

// drainDeltas receives from deltas until it closes, so its producer finishes.
func drainDeltas(deltas <-chan OutboundDelta) {
	for range deltas {
	}
}

// noteRetry records that the turn on ctx is sent a second time after err.
func noteRetry(ctx context.Context, msg InboundMessage, err error) {
	TurnTimerFromContext(ctx).Retry()
	slog.Warn("channels: turn failed before it showed anything, sending it once more", "record", RecordTurnRetry,
		"channel", msg.Channel, "channel_id", msg.ChannelID, "thread", msg.ThreadID, "agent", msg.AgentRef,
		"failure_class", ClassifyFailure(err), "error", err)
}

// clearBinding drops a thread's AgentInstance binding and anything that only
// makes sense with it, keeping the rest of the thread's row.
func clearBinding(e *store.Entry, found bool) bool {
	if !found {
		return false
	}
	e.AgentInstanceID, e.TaskID, e.Resume = "", "", nil
	return true
}

// instanceFor returns the AgentInstance id msg's thread is bound to, creating
// the instance on the thread's first turn, named after the message that opens
// it. A thread binds one agent; a turn that names another one rebinds it, and
// the task in flight on the old instance goes with it — the conversation the
// rebind creates is named after the message that asked for it, that being its
// own first message. The create is keyed by the synthesized context id, so
// a retried first turn does not create a second instance. The binding slides
// with the thread's lifetime: every turn refreshes it, the conversation ends
// after ThreadTTL of silence, and the next mention asks the controller for an
// instance again — the idempotent create hands the same person the earlier one
// back while the controller still holds it.
func (f *Facade) instanceFor(ctx context.Context, msg InboundMessage) (string, error) {
	if f.Routes == nil {
		return "", errors.New("channels: no routing store for the agent instance binding")
	}
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	now := f.clock()
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("channels: read instance binding: %w", err)
	}
	if f.threadClosed(entry, now) {
		// The conversation ended: the row is still there but its binding is
		// not the thread's any more, so this turn creates a fresh instance.
		entry, ok = store.Entry{}, false
	}
	if ok && entry.AgentInstanceID != "" && entry.AgentRef == msg.AgentRef {
		if err := f.Routes.Update(ctx, key, func(e *store.Entry, found bool) bool {
			if !found {
				return false
			}
			e.LastSeen = now
			// A row written before the thread lifetime existed carries no TTL;
			// it adopts the row lifetime on its next turn.
			if e.TTL <= 0 && f.ThreadTTL > 0 {
				e.TTL = f.storeTTL()
			}
			return true
		}); err != nil {
			return "", fmt.Errorf("channels: refresh instance binding: %w", err)
		}
		return entry.AgentInstanceID, nil
	}
	// The empty user slot is load-bearing, and not a leftover of the field
	// InboundMessage no longer has: this hash is the create's idempotency key,
	// and it has been computed with an empty user slot on every release. Fill
	// it, or drop the parameter, and every hash changes — every live thread
	// then asks for an instance the controller does not have and starts its
	// conversation over, empty.
	requestID := SynthesizeContextID(msg.Channel, msg.ChannelID, "", msg.ThreadID, msg.AgentRef)
	created := TurnTimerFromContext(ctx).Span(PhaseCreateInstance)
	inst, err := f.Agent.CreateInstance(ctx, msg.AgentRef, requestID, instanceName(msg))
	created()
	if err != nil {
		return "", err
	}
	if err := f.Routes.Update(ctx, key, func(e *store.Entry, found bool) bool {
		// Only the side effect counts here — a closed row emptied, so this
		// turn writes a fresh binding rather than merging into the ended
		// conversation's. The reopened found state has no reader below.
		_ = f.reopen(e, found, now)
		if e.AgentInstanceID != "" && e.AgentInstanceID != inst.ID {
			// A rebind: nothing of the previous instance's turn is deliverable.
			e.TaskID, e.Resume, e.Delivered = "", nil, store.Delivered{}
		}
		e.AgentRef, e.AgentInstanceID, e.LastSeen = msg.AgentRef, inst.ID, now
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		if e.TTL <= 0 {
			e.TTL = f.storeTTL()
		}
		return true
	}); err != nil {
		return "", fmt.Errorf("channels: store instance binding: %w", err)
	}
	slog.Info("channels: thread bound to agent instance", "record", "instance_bound",
		"channel", msg.Channel, "channel_id", msg.ChannelID, "thread", msg.ThreadID, "agent", msg.AgentRef, "instance", inst.ID)
	return inst.ID, nil
}

// cancelTimeout bounds the server-side cancel of a task whose channel turn
// was stopped.
const cancelTimeout = 10 * time.Second

// bindingWriteTimeout bounds the routing-store writes that track a thread's
// in-flight task. They run detached from the turn's cancellation, so a stopped
// turn still clears its record.
const bindingWriteTimeout = 5 * time.Second

// sendViaA2A runs the turn on the thread's AgentInstance and maps the task's
// events to OutboundDeltas. The controller's refusal of a turn (an unknown or
// unavailable agent, an instance still working on a previous message, a
// resume that names no paused prompt) is returned synchronously so channels
// can render it; the stream's own failures arrive as error deltas.
func (f *Facade) sendViaA2A(ctx context.Context, msg InboundMessage) (<-chan OutboundDelta, error) {
	ctx = withChannelAuth(ctx, msg)

	instanceID, err := f.instanceFor(ctx, msg)
	if err != nil {
		return nil, err
	}
	message, err := f.outboundMessage(ctx, instanceID, msg)
	if err != nil {
		return nil, err
	}
	// The task id is learned from the first event, also on a HITL resume: the
	// paused task's record was dropped with the prompt, so the resumed segment
	// is recorded afresh.
	return f.streamTask(ctx, threadKey(msg.Channel, msg.ChannelID, msg.ThreadID), instanceID, "", msg.Resume, f.Agent.Stream(ctx, instanceID, message))
}

// ResumesTurns reports whether a turn this gateway leaves running at its
// shutdown is delivered into its thread after the restart, which takes the
// kagent client and a routing store that outlives the process.
func (f *Facade) ResumesTurns() bool {
	return f != nil && f.Agent != nil && f.Routes != nil && f.Durable
}

// InFlightTurns lists the turns a previous process left running for channel:
// every thread of that channel whose row still records a task. A channel
// adapter calls it once at start to resubscribe to each and deliver the result.
func (f *Facade) InFlightTurns(ctx context.Context, channel string) ([]InFlightTurn, error) {
	if f == nil || f.Agent == nil || f.Routes == nil {
		return nil, nil
	}
	entries, err := f.Routes.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("channels: list in-flight turns: %w", err)
	}
	now := f.clock()
	var turns []InFlightTurn
	for _, ke := range entries {
		if ke.Key.Channel != channel || ke.Entry.TaskID == "" || ke.Entry.AgentInstanceID == "" {
			continue
		}
		// A row whose conversation has ended is absent for the binding too:
		// its task is nobody's turn any more.
		if f.threadClosed(ke.Entry, now) {
			continue
		}
		turns = append(turns, inFlightTurn(ke.Key, ke.Entry))
	}
	return turns, nil
}

// InFlightTurn returns the turn a previous process left running on msg's
// thread, when the binding records one. It lets a reply into the thread
// deliver the result before it is handled, for a turn the resubscription at
// start could not reach (no token for its user at the time, the controller
// unreachable).
func (f *Facade) InFlightTurn(ctx context.Context, msg InboundMessage) (InFlightTurn, bool, error) {
	if f == nil || f.Agent == nil || f.Routes == nil {
		return InFlightTurn{}, false, nil
	}
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return InFlightTurn{}, false, fmt.Errorf("channels: read in-flight turn: %w", err)
	}
	entry, ok = f.liveEntry(entry, ok)
	if !ok || entry.TaskID == "" || entry.AgentInstanceID == "" {
		return InFlightTurn{}, false, nil
	}
	return inFlightTurn(key, entry), true, nil
}

func inFlightTurn(key store.Key, entry store.Entry) InFlightTurn {
	return InFlightTurn{
		Msg:       InboundMessage{Channel: key.Channel, ChannelID: key.ChannelID, ThreadID: key.ThreadID, AgentRef: entry.AgentRef, Resume: entry.Resume},
		TaskID:    entry.TaskID,
		Delivered: entry.Delivered,
	}
}

// ResumeTurn resubscribes to taskID on msg's thread and streams what is left
// of it — for a task that finished meanwhile, its result — as OutboundDeltas
// under msg's identity. The record of the in-flight turn goes when the task
// quiesces, and at once when the controller no longer knows the task or the
// instance (nothing is left to deliver); any other failure keeps it for a
// later attempt.
func (f *Facade) ResumeTurn(ctx context.Context, msg InboundMessage, taskID string) (<-chan OutboundDelta, error) {
	if f == nil || f.Agent == nil || f.Routes == nil {
		return nil, errors.New("channels: no agent client configured")
	}
	ctx = withChannelAuth(ctx, msg)
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("channels: read instance binding: %w", err)
	}
	if !ok || entry.AgentInstanceID == "" {
		return nil, fmt.Errorf("channels: thread %s has no agent instance to resume task %s on", msg.ThreadID, taskID)
	}
	id := a2apkg.TaskID(taskID)
	out, err := f.streamTask(ctx, key, entry.AgentInstanceID, id, nil, f.Agent.Subscribe(ctx, entry.AgentInstanceID, id))
	if err != nil {
		if errors.Is(err, a2apkg.ErrTaskNotFound) || pkga2a.IsNotFound(err) {
			f.forgetTask(ctx, key)
		}
		return nil, err
	}
	return out, nil
}

// streamTask drives a task's events into OutboundDeltas. The first event is
// pulled synchronously, so the controller's refusal of the call is the
// returned error; the rest is pumped on a goroutine that also keeps the
// thread's record of its in-flight task: written once the controller has
// named the task (a fresh turn learns the id from its first event; known is
// the id a resubscription already has, whose record exists), cleared when the
// task quiesces or the turn is stopped. A resubscription joins the stream
// wherever it is, and whatever the agent wrote between the previous process's
// end and this one's subscription is not replayed — so its answer text is not
// streamed but posted whole when the task completes, read back from the
// controller, and the channel adapter cuts off the part the previous process
// had posted (InFlightTurn.Delivered); tool activity and narration still
// stream live. A consumer that goes away before a
// terminal delta stopped the turn: the gateway's shutdown (context cause
// ErrShutdown) leaves the task running and its record in place for the next
// process to resubscribe to, a plain cancellation (/stop) cancels the task at
// the controller. A context cancelled after the terminal delta is the turn
// being torn down and touches the task not at all.
func (f *Facade) streamTask(ctx context.Context, key store.Key, instanceID string, known a2apkg.TaskID, resume map[string]string, events iter.Seq2[a2apkg.Event, error]) (<-chan OutboundDelta, error) {
	next, stop := iter.Pull2(events)
	first, err, ok := next()
	if err != nil {
		stop()
		return nil, err
	}
	if !ok {
		stop()
		return nil, errors.New("a2a: stream ended without events")
	}
	timer := TurnTimerFromContext(ctx)
	timer.Mark(PhaseFirstEvent)

	out := make(chan OutboundDelta, 16)
	go func() {
		defer close(out)
		defer stop()
		defer timer.Mark(PhaseStreamEnd)
		taskID := known
		recorded := known != ""
		resumed := known != ""
		mapper := newEventMapper()
		// terminal is set once the task reached a terminal or waiting state: the
		// agent is not working any more, whether or not the channel is still
		// listening. delivered is set once the channel received that state or
		// the stream's failure.
		terminal, delivered := false, false
		event, streamErr := first, error(nil)
		for {
			if streamErr != nil {
				delivered = f.emit(ctx, out, OutboundDelta{Err: streamErr})
				break
			}
			if info, ok := event.(a2apkg.TaskInfoProvider); ok && info.TaskInfo().TaskID != "" {
				taskID = info.TaskInfo().TaskID
			}
			if !recorded && taskID != "" {
				recorded = true
				timer.SetTaskID(string(taskID))
				f.rememberTask(ctx, key, taskID, resume)
			}
			_, whole := event.(*a2apkg.Task) // a quiesced task arriving whole already carries its full answer
			for _, delta := range mapper.deltas(event) {
				if delta.isZero() {
					continue
				}
				if resumed && !whole && delta.Kind == DeltaText && delta.Content != "" {
					continue // the tail that streams is not the answer; it is read back at completion
				}
				if delta.Err != nil || delta.Done || delta.Kind == DeltaPrompt {
					terminal = true
					timer.Mark(PhaseTaskDone)
				}
				if resumed && !whole && delta.Done {
					if text := f.finalText(ctx, instanceID, taskID); text != "" && !f.emit(ctx, out, OutboundDelta{Content: text}) {
						break
					}
				}
				if !f.emit(ctx, out, delta) {
					break
				}
				if terminal {
					delivered = true
					break
				}
			}
			if terminal || ctx.Err() != nil {
				break
			}
			var more bool
			event, streamErr, more = next()
			if !more {
				break
			}
		}
		if ctx.Err() != nil {
			// The channel stopped listening (/stop, a closed web stream, the
			// gateway's shutdown). A task still running is cancelled server-side
			// so the agent does not work on unobserved — unless the shutdown is
			// what stopped the channel: then the task runs on and its record stays
			// for the next process to resubscribe to. One that already finished or
			// paused on a prompt is left alone — cancelling it would record a
			// completed turn as canceled (klaus-gateway#242).
			if taskID != "" && !terminal {
				if errors.Is(context.Cause(ctx), ErrShutdown) {
					slog.Info("a2a: task left running through the shutdown", "record", "task_left_running",
						"instance", instanceID, "task", taskID, "channel", key.Channel, "thread", key.ThreadID)
					return
				}
				f.forgetTask(ctx, key)
				f.cancelTask(ctx, instanceID, taskID)
				return
			}
			if recorded {
				f.forgetTask(ctx, key)
			}
			return
		}
		if recorded {
			f.forgetTask(ctx, key)
		}
		if !delivered {
			f.emit(ctx, out, OutboundDelta{Err: errors.New("a2a: stream ended without terminal status")})
		}
	}()
	return out, nil
}

// finalText reads a completed task back from the controller and returns its
// answer, for a resubscribed turn whose streamed text is not trusted to be
// whole. A failed read is logged and yields nothing: the terminal delta still
// closes the turn.
func (f *Facade) finalText(ctx context.Context, instanceID string, taskID a2apkg.TaskID) string {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindingWriteTimeout)
	defer cancel()
	task, err := f.Agent.GetTask(rctx, instanceID, taskID)
	if err != nil {
		slog.Warn("a2a: read the completed task's answer failed", "instance", instanceID, "task", taskID, "error", err)
		return ""
	}
	return taskResultText(task)
}

// rememberTask records taskID as the task in flight on key's thread, with the
// channel's resume data, so a restart can resubscribe to it. Nothing of the
// new task has been delivered yet, so a stale record of a previous turn's
// delivery is dropped with it.
func (f *Facade) rememberTask(ctx context.Context, key store.Key, taskID a2apkg.TaskID, resume map[string]string) {
	f.updateBinding(ctx, key, func(e *store.Entry) bool {
		e.TaskID, e.Resume, e.Delivered = string(taskID), resume, store.Delivered{}
		return true
	})
}

// forgetTask clears the thread's in-flight task record, and with it what the
// channel delivered of the turn.
func (f *Facade) forgetTask(ctx context.Context, key store.Key) {
	f.updateBinding(ctx, key, func(e *store.Entry) bool {
		if e.TaskID == "" && e.Resume == nil && e.Delivered.IsZero() {
			return false
		}
		e.TaskID, e.Resume, e.Delivered = "", nil, store.Delivered{}
		return true
	})
}

// updateBinding applies mutate to the thread's row when it exists and writes it
// back when mutate reports a change. It goes through the store's per-key
// update, so a grant or a binding written during the turn is not lost. Best
// effort and detached from the turn's cancellation: the row itself is never at
// stake here, only the bookkeeping of its in-flight task.
func (f *Facade) updateBinding(ctx context.Context, key store.Key, mutate func(*store.Entry) bool) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindingWriteTimeout)
	defer cancel()
	if err := f.Routes.Update(wctx, key, func(e *store.Entry, found bool) bool {
		if !found {
			return false
		}
		return mutate(e)
	}); err != nil {
		slog.Warn("channels: write in-flight task record failed", "thread", key.ThreadID, "error", err)
	}
}

// emit delivers delta unless ctx is done; it reports whether the delta was
// delivered.
func (f *Facade) emit(ctx context.Context, out chan<- OutboundDelta, delta OutboundDelta) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- delta:
		return true
	}
}

// cancelTask cancels the task server-side after the channel stopped the turn,
// so the agent stops working instead of running on unobserved. The cancel
// keeps the turn's identity but not its cancellation.
func (f *Facade) cancelTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	if _, err := f.Agent.CancelTask(cctx, instanceID, taskID); err != nil {
		slog.Warn("a2a: cancel task after stopped turn failed", "instance", instanceID, "task", taskID, "error", err)
	}
}

// outboundMessage builds the A2A message of a turn. A fresh turn carries no
// task id (the controller assigns one) and no context id (the controller owns
// it). A HITL decision resumes the paused task: the message carries that task's
// id, and the typed response — built against the request the task is paused
// on — rides as the HITL extension payload.
func (f *Facade) outboundMessage(ctx context.Context, instanceID string, msg InboundMessage) (*a2apkg.Message, error) {
	message := a2apkg.NewMessage(a2apkg.MessageRoleUser, buildInboundParts(msg)...)
	if msg.TaskID == "" {
		return message, nil
	}
	message.TaskID = a2apkg.TaskID(msg.TaskID)
	if msg.Decision == nil {
		return message, nil
	}
	task, err := f.Agent.GetTask(ctx, instanceID, message.TaskID)
	if err != nil {
		return nil, fmt.Errorf("a2a: load the paused task %s: %w", msg.TaskID, err)
	}
	if task.Status.State != a2apkg.TaskStateInputRequired {
		return nil, fmt.Errorf("%w: task %s is %s", ErrNoPendingPrompt, msg.TaskID, task.Status.State)
	}
	request, err := pkga2a.ParseHITLRequest(task.Status.Message)
	if err != nil {
		return nil, fmt.Errorf("a2a: read the paused task's prompt: %w", err)
	}
	if request == nil {
		return nil, fmt.Errorf("%w: task %s carries no HITL request", ErrNoPendingPrompt, msg.TaskID)
	}
	if err := pkga2a.AttachHITL(message, hitlResponse(request, msg.Decision)); err != nil {
		return nil, err
	}
	return message, nil
}

// ErrNoPendingPrompt is returned when a HITL decision arrives for a task that
// is not paused on a prompt any more.
var ErrNoPendingPrompt = errors.New("a2a: the task is not waiting for a decision")

// withChannelAuth seeds ctx with the target agent ref, the originating channel
// name, and the caller's forwarded bearer token for the A2A client. The channel
// name lets the token source enforce per-channel credential policy (e.g. no
// service-account fallback for Slack when account linking is enabled).
func withChannelAuth(ctx context.Context, msg InboundMessage) context.Context {
	ctx = pkga2a.WithAgentRef(ctx, msg.AgentRef)
	ctx = pkga2a.WithChannel(ctx, msg.Channel)
	ctx = pkga2a.WithForwardedToken(ctx, msg.BearerToken)
	return ctx
}

// eventMapper converts a task's A2A streaming events to OutboundDeltas. It
// keeps the per-stream state the conversion needs: the text each artifact has
// delivered so far, so an artifact update renders as what it adds.
type eventMapper struct {
	artifacts artifactText
}

func newEventMapper() *eventMapper {
	return &eventMapper{artifacts: newArtifactText()}
}

// deltas converts a single A2A streaming event to zero or more OutboundDeltas.
// A single event may carry both assistant text and tool-call DataParts, so it
// can expand to several deltas. Non-completed terminal states (failed,
// rejected, canceled) map to an error delta so channels surface them rather
// than silently closing.
//
// kagent attaches token usage to the event/message metadata (not to a part),
// as per-LLM-call deltas on interim working events; the terminal completed
// event carries no usage, so consumers sum the interim deltas. Partial
// (streaming) events mirror their call's usage and are skipped to avoid
// counting one call several times. Tool activity rides on
// function_call/function_response DataParts. A paused task's prompt rides on
// the input-required status message as the HITL extension payload.
func (m *eventMapper) deltas(event a2apkg.Event) []OutboundDelta {
	switch ev := event.(type) {
	case *a2apkg.TaskArtifactUpdateEvent:
		if ev.Artifact == nil {
			return nil
		}
		return append(textDeltaOf(m.artifacts.delta(ev)), toolActivityDeltas(ev.Artifact.Parts)...)
	case *a2apkg.TaskStatusUpdateEvent:
		var usage *TurnUsage
		if !isPartialStatusUpdate(ev) {
			usage = parseTurnUsage(ev.Metadata)
			if usage == nil && ev.Status.Message != nil {
				usage = parseTurnUsage(ev.Status.Message.Metadata)
			}
		}
		return mapTaskStatus(ev.TaskID, ev.Status, usage, func() []OutboundDelta {
			// Interim working event: surface the narration the agent wrote for the
			// tool calls the same message carries, the tool activity itself, and a
			// usage-only delta when the event reports usage.
			parts := messageParts(ev.Status.Message)
			deltas := narrationDeltas(ev, parts)
			deltas = append(deltas, toolActivityDeltas(parts)...)
			if usage != nil {
				deltas = append(deltas, OutboundDelta{Usage: usage})
			}
			return deltas
		})
	case *a2apkg.Task:
		// A whole task arrives as the first event of a stream (the submitted
		// task the controller stored), as the reply to a message the controller
		// answered from its store, and as the only event of a resubscription to
		// a task that quiesced meanwhile. Only a quiescent state carries
		// something to render; the submitted snapshot is the turn starting. A
		// completed task carries its whole answer, rendered ahead of the
		// terminal delta so a result produced while no gateway was listening
		// still reaches the thread.
		if ev.Status.State == a2apkg.TaskStateSubmitted || ev.Status.State == a2apkg.TaskStateWorking {
			return nil
		}
		deltas := mapTaskStatus(ev.ID, ev.Status, parseTurnUsage(ev.Metadata), func() []OutboundDelta { return nil })
		if ev.Status.State == a2apkg.TaskStateCompleted {
			deltas = append(m.taskResultDeltas(ev), deltas...)
		}
		return deltas
	case *a2apkg.Message:
		// A bare agent message is a complete reply without a task wrapper.
		return append(textDelta(ev.Parts), OutboundDelta{Done: true})
	}
	return nil
}

// mapTaskStatus maps a task state to deltas: completed closes the turn,
// input-required/auth-required pauses it on a prompt, any other terminal state
// fails it, and a working state is left to interim.
func mapTaskStatus(taskID a2apkg.TaskID, status a2apkg.TaskStatus, usage *TurnUsage, interim func() []OutboundDelta) []OutboundDelta {
	switch status.State {
	case a2apkg.TaskStateCompleted:
		return []OutboundDelta{{Done: true, Usage: usage}}
	case a2apkg.TaskStateInputRequired, a2apkg.TaskStateAuthRequired:
		var hitl *HitlPrompt
		text := ""
		if status.Message != nil {
			hitl = parseHitlPrompt(status.Message)
			text = extractTextFromA2AParts(status.Message.Parts)
			if hitl != nil {
				hitl.StatusText = text
			}
		}
		// The status message's text is the agent's hint; when it carries none,
		// fall back to the structured prompt's summary for plain-text renderers.
		if text == "" && hitl != nil {
			text = hitl.summary()
		}
		return []OutboundDelta{{Kind: DeltaPrompt, Content: text, TaskID: string(taskID), Prompt: hitl, Usage: usage}}
	default:
		if status.State.Terminal() {
			msg := fmt.Sprintf("a2a: task ended with state %s", status.State)
			if status.Message != nil {
				if text := extractTextFromA2AParts(status.Message.Parts); text != "" {
					msg = text
				}
			}
			return []OutboundDelta{{Err: &taskEnded{state: status.State, reason: msg}, Usage: usage}}
		}
		return interim()
	}
}

// taskResultDeltas renders a completed task's answer as what the stream has
// not delivered yet: each artifact goes through the tracker as a full replace,
// so a task arriving whole after its chunks streamed adds nothing, and one
// arriving as the only event (a resubscription to a task that finished in
// between) renders in full. A runtime that records the reply in the history
// alone yields the last agent message.
func (m *eventMapper) taskResultDeltas(task *a2apkg.Task) []OutboundDelta {
	var deltas []OutboundDelta
	for _, artifact := range task.Artifacts {
		if artifact != nil {
			deltas = append(deltas, textDeltaOf(m.artifacts.delta(&a2apkg.TaskArtifactUpdateEvent{Artifact: artifact}))...)
		}
	}
	if len(task.Artifacts) > 0 {
		return deltas
	}
	return textDeltaOf(lastAgentText(task.History))
}

// taskResultText is the whole answer of a completed task, rendered as the
// stream renders it: each artifact's text with a paragraph break where a new
// artifact follows rendered text, or — for a runtime that records the reply
// in the history only — the last agent message. Byte for byte what a
// listener would have received had it heard the whole stream, so a channel
// that cuts off the prefix it had already posted cuts at the right place.
func taskResultText(task *a2apkg.Task) string {
	var sb strings.Builder
	for _, delta := range newEventMapper().taskResultDeltas(task) {
		sb.WriteString(delta.Content)
	}
	return sb.String()
}

// lastAgentText is the text of the last agent message in history.
func lastAgentText(history []*a2apkg.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if m := history[i]; m != nil && m.Role == a2apkg.MessageRoleAgent {
			return extractTextFromA2AParts(m.Parts)
		}
	}
	return ""
}

// textDelta returns a single-element slice with the concatenated text of parts,
// or nil when there is no text.
func textDelta(parts a2apkg.ContentParts) []OutboundDelta {
	return textDeltaOf(extractTextFromA2AParts(parts))
}

// textDeltaOf wraps text in a single-element slice, or nil when it is empty.
func textDeltaOf(text string) []OutboundDelta {
	if text == "" {
		return nil
	}
	return []OutboundDelta{{Content: text}}
}

// artifactText reconciles a task's artifact updates into the text a channel
// renders, honouring the A2A update semantics: Append marks parts that extend
// the artifact sent earlier under the same ID, its absence parts that are the
// artifact's whole content. The Go ADK streams every text run that way — each
// chunk appended, then the finished run re-sent whole (Append false, LastChunk
// true) on the same artifact — so rendering every update as an append showed
// each run twice (klaus-gateway#242). A replace renders as what lies past the
// text the artifact already delivered; should the replacement diverge from
// what was streamed, the part past the common prefix is rendered so no text is
// lost. A new artifact opens a paragraph: producers start a fresh artifact for
// each text run between tool calls, and the runs would otherwise run into one
// another.
type artifactText struct {
	delivered map[a2apkg.ArtifactID]string
	// last is the artifact the most recent rendered text came from; tail is how
	// that text ends, for the paragraph break.
	last a2apkg.ArtifactID
	tail string
}

func newArtifactText() artifactText {
	return artifactText{delivered: map[a2apkg.ArtifactID]string{}}
}

// delta returns the text ev adds to what the channel has rendered.
func (a *artifactText) delta(ev *a2apkg.TaskArtifactUpdateEvent) string {
	text := extractTextFromA2AParts(ev.Artifact.Parts)
	id := ev.Artifact.ID
	if id == "" {
		// Not addressable: nothing to reconcile against, render as sent.
		return a.render(id, text)
	}
	previous := a.delivered[id]
	if ev.Append {
		a.delivered[id] = previous + text
		return a.render(id, text)
	}
	a.delivered[id] = text
	return a.render(id, text[commonPrefixLen(previous, text):])
}

// render prefixes a paragraph break when text opens a new artifact after
// rendered text, and remembers how the rendered stream ends.
func (a *artifactText) render(id a2apkg.ArtifactID, text string) string {
	if text == "" {
		return ""
	}
	if a.tail != "" && id != a.last {
		text = paragraphBreak(a.tail) + text
	}
	a.last, a.tail = id, text[max(0, len(text)-2):]
	return text
}

// paragraphBreak returns the newlines that separate a paragraph from rendered
// text ending in tail.
func paragraphBreak(tail string) string {
	switch {
	case strings.HasSuffix(tail, "\n\n"):
		return ""
	case strings.HasSuffix(tail, "\n"):
		return "\n"
	}
	return "\n\n"
}

// commonPrefixLen returns the length in bytes of the longest common prefix of
// a and b, backed off to a rune boundary of b.
func commonPrefixLen(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	for i > 0 && i < len(b) && !utf8.RuneStart(b[i]) {
		i--
	}
	return i
}

// narrationDeltas returns the agent's interim narration for a working status
// message: the text it wrote just before the tool calls the same message
// carries. Requiring a function_call part is what separates narration from
// kagent's other text-bearing working events — the text-only mirror of the final
// answer (rendered from the artifact, so this would duplicate it), the echo of
// the user's own message, and partial streaming chunks. It also keeps narration
// off function_response messages, whose payload records the login URLs a channel
// scrubs out of prose: narration is emitted first, so a message mixing a call
// with a response would post an unscrubbed link. Only ADK emitting tool results
// as their own data-only events rules that shape out — preserve the exclusion if
// widening this gate. A request for confirmation never reaches this path: the
// runtime pauses the task at input-required with the prompt instead.
func narrationDeltas(ev *a2apkg.TaskStatusUpdateEvent, parts a2apkg.ContentParts) []OutboundDelta {
	if !hasFunctionCallPart(parts) || isPartialStatusUpdate(ev) {
		return nil
	}
	text := extractTextFromA2AParts(parts)
	if text == "" {
		return nil
	}
	return []OutboundDelta{{Kind: DeltaNarration, Content: text}}
}

// isPartialStatusUpdate reports whether a status update is a streaming chunk.
// kagent stamps the flag on the event, on its status message, or on both,
// depending on the emitting runtime.
func isPartialStatusUpdate(ev *a2apkg.TaskStatusUpdateEvent) bool {
	if isPartialMeta(ev.Metadata) {
		return true
	}
	return ev.Status.Message != nil && isPartialMeta(ev.Status.Message.Metadata)
}

// toolActivityDeltas maps each function_call/function_response DataPart to a
// DeltaToolActivity, skipping parts that are not tool activity.
func toolActivityDeltas(parts a2apkg.ContentParts) []OutboundDelta {
	var deltas []OutboundDelta
	for _, p := range parts {
		if d := toolActivityDelta(p); !d.isZero() {
			deltas = append(deltas, d)
		}
	}
	return deltas
}

func messageParts(msg *a2apkg.Message) a2apkg.ContentParts {
	if msg == nil {
		return nil
	}
	return msg.Parts
}

// extractTextFromA2AParts concatenates text from A2A content parts.
func extractTextFromA2AParts(parts a2apkg.ContentParts) string {
	var sb bytes.Buffer
	for _, p := range parts {
		if p != nil {
			sb.WriteString(p.Text())
		}
	}
	return sb.String()
}
