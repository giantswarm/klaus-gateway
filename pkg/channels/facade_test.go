package channels_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/storetest"
)

// fakeAgent is the kagent client at the facade's seam: it records the message
// and session every turn is sent with, hands out sessions keyed by request
// id like the controller's idempotent create, and plays back a fixed event
// sequence.
type fakeAgent struct {
	mu sync.Mutex

	events    []a2apkg.Event
	streamErr error // yielded as the stream's first item
	tailErr   error // yielded after events
	hold      chan struct{}
	// attempts, when set, script the Stream calls one by one: the nth call
	// plays attempts[n] instead of events and streamErr.
	attempts []streamAttempt

	sessions  map[string]pkga2a.Session
	byRequest map[string]string
	createErr error
	getErr    error
	deleteErr error
	tasks     map[a2apkg.TaskID]*a2apkg.Task

	// subscribeEvents are played back by Subscribe; subscribeErr is yielded as
	// its first item instead.
	subscribeEvents []a2apkg.Event
	subscribeErr    error
	subscribed      []a2apkg.TaskID
	subscribedOn    []string

	streamCtx       context.Context
	streamed        []*a2apkg.Message
	streamedOn      []string
	createRequests  []string
	createNames     []string
	canceled        []a2apkg.TaskID
	canceledOn      []string
	deleted         []string
	gotTasks        []a2apkg.TaskID
	created, gotIns int

	// createdAs is the bearer each CreateSession ran under; sharedAs the
	// bearer of each CreateShare, shares the ids minted, revoked the revokes.
	createdAs []string
	sharedAs  []string
	sharedTTL []time.Duration
	shareExp  func(ttl time.Duration) time.Time
	shareErr  error
	shares    []string
	revoked   []string
	// onShare, when set, runs after CreateShare minted a share and before it
	// returns, standing in for what another turn does meanwhile.
	onShare func()
}

// streamAttempt is what one Stream call of the fake plays: its events, or err
// as the stream's first item.
type streamAttempt struct {
	events []a2apkg.Event
	err    error
}

func newFakeAgent(events ...a2apkg.Event) *fakeAgent {
	return &fakeAgent{
		events:    events,
		sessions:  map[string]pkga2a.Session{},
		byRequest: map[string]string{},
		tasks:     map[a2apkg.TaskID]*a2apkg.Task{},
	}
}

func (a *fakeAgent) Stream(ctx context.Context, sessionID string, msg *a2apkg.Message) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		a.mu.Lock()
		a.streamCtx = ctx
		a.streamed = append(a.streamed, msg)
		a.streamedOn = append(a.streamedOn, sessionID)
		events, streamErr, tailErr, hold := a.events, a.streamErr, a.tailErr, a.hold
		if n := len(a.streamed) - 1; n < len(a.attempts) {
			events, streamErr = a.attempts[n].events, a.attempts[n].err
		}
		a.mu.Unlock()
		if streamErr != nil {
			yield(nil, streamErr)
			return
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
		if tailErr != nil {
			yield(nil, tailErr)
		}
	}
}

func (a *fakeAgent) Subscribe(ctx context.Context, taskID a2apkg.TaskID) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		a.mu.Lock()
		a.streamCtx = ctx
		a.subscribed = append(a.subscribed, taskID)
		a.subscribedOn = append(a.subscribedOn, pkga2a.AgentRefFromContext(ctx))
		events, streamErr, hold := a.subscribeEvents, a.subscribeErr, a.hold
		a.mu.Unlock()
		if streamErr != nil {
			yield(nil, streamErr)
			return
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				yield(nil, ctx.Err())
			}
		}
	}
}

func (a *fakeAgent) GetTask(_ context.Context, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gotTasks = append(a.gotTasks, taskID)
	task, ok := a.tasks[taskID]
	if !ok {
		return nil, a2apkg.ErrTaskNotFound
	}
	return task, nil
}

func (a *fakeAgent) CancelTask(ctx context.Context, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.canceled = append(a.canceled, taskID)
	a.canceledOn = append(a.canceledOn, pkga2a.AgentRefFromContext(ctx))
	return &a2apkg.Task{ID: taskID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCanceled}}, nil
}

func (a *fakeAgent) CreateSession(ctx context.Context, agentRef, requestID, name string) (pkga2a.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.createdAs = append(a.createdAs, pkga2a.ForwardedTokenFromContext(ctx))
	a.createRequests = append(a.createRequests, requestID)
	a.createNames = append(a.createNames, name)
	if a.createErr != nil {
		return pkga2a.Session{}, a.createErr
	}
	if id, ok := a.byRequest[requestID]; ok {
		return a.sessions[id], nil
	}
	a.created++
	inst := pkga2a.Session{ID: fmt.Sprintf("inst-%s-%d", agentRef, a.created), State: "RUNTIME_STATE_READY"}
	a.sessions[inst.ID] = inst
	a.byRequest[requestID] = inst.ID
	return inst, nil
}

func (a *fakeAgent) GetSession(_ context.Context, id string) (pkga2a.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gotIns++
	if a.getErr != nil {
		return pkga2a.Session{}, a.getErr
	}
	inst, ok := a.sessions[id]
	if !ok {
		return pkga2a.Session{}, fmt.Errorf("%w: %s", pkga2a.ErrSessionNotFound, id)
	}
	return inst, nil
}

func (a *fakeAgent) DeleteSession(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleted = append(a.deleted, id)
	if a.deleteErr != nil {
		return a.deleteErr
	}
	delete(a.sessions, id)
	return nil
}

func (a *fakeAgent) CreateShare(ctx context.Context, sessionID string, ttl time.Duration) (pkga2a.Share, error) {
	a.mu.Lock()
	a.sharedAs = append(a.sharedAs, pkga2a.ForwardedTokenFromContext(ctx))
	a.sharedTTL = append(a.sharedTTL, ttl)
	if a.shareErr != nil {
		a.mu.Unlock()
		return pkga2a.Share{}, a.shareErr
	}
	id := fmt.Sprintf("share-%d", len(a.shares)+1)
	a.shares = append(a.shares, id)
	share := pkga2a.Share{ID: id, Token: "token-" + id + "-" + sessionID}
	if a.shareExp != nil {
		share.ExpiresAt = a.shareExp(ttl)
	}
	hook := a.onShare
	a.onShare = nil
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return share, nil
}

func (a *fakeAgent) RevokeShare(_ context.Context, shareID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revoked = append(a.revoked, shareID)
	return nil
}

func (a *fakeAgent) lastStreamed() *a2apkg.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streamed[len(a.streamed)-1]
}

// newA2AFacade wires a facade on the fake with a memory store.
func newA2AFacade(agent *fakeAgent) (*channels.Facade, store.Store) {
	s := memory.New()
	return &channels.Facade{Agent: agent, Routes: s, ThreadTTL: channels.DefaultThreadTTL}, s
}

// taskInfo is the identity of the fake turn's task, as the controller's events
// would carry it.
var taskInfo = a2apkg.TaskInfo{TaskID: "task-1", ContextID: "ctx-1"}

func slackMsg(text string) channels.InboundMessage {
	return channels.InboundMessage{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", AgentRef: "kagent/worker", Text: text, BearerToken: "user-jwt"}
}

// drain collects every delta of a turn.
func drain(t *testing.T, ch <-chan channels.OutboundDelta) []channels.OutboundDelta {
	t.Helper()
	var deltas []channels.OutboundDelta
	for d := range ch {
		deltas = append(deltas, d)
	}
	return deltas
}

func TestFacade_SendCompletionViaA2A_FirstTurnCreatesTheSession(t *testing.T) {
	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart("hello world")),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	)
	f, routes := newA2AFacade(agent)
	msg := slackMsg("hi")
	msg.SenderID, msg.Subject = "U1", "sub-1"

	ch, err := f.SendCompletion(t.Context(), msg)
	require.NoError(t, err)
	var content strings.Builder
	var done bool
	for _, d := range drain(t, ch) {
		require.NoError(t, d.Err)
		if d.Done {
			done = true
		}
		content.WriteString(d.Content)
	}
	require.Equal(t, "hello world", content.String())
	require.True(t, done)

	// The session was created with the synthesized context id as the
	// idempotency key (thread-scoped: empty user slot), the turn ran on it,
	// and the binding is persisted for the next turn and the next process.
	// The id is fixed: every live thread is bound by it, so a change to its
	// encoding hands each one a new, empty instance (docs/invariants.md).
	// printf '5:slack|2:C1|0:|9:1700.0001|13:kagent/worker|' | shasum -a 256
	const wantRequest = "84393e8104d7c8645e2f4e69d95afc4613c1c8ebe488e0d64a4c382c0c9a654c"
	require.Equal(t, []string{wantRequest}, agent.createRequests)
	require.Equal(t, []string{"hi"}, agent.createNames, "the conversation is named after the message that opened it")
	require.Equal(t, []string{"inst-kagent/worker-1"}, agent.streamedOn)
	entry, ok, err := routes.Get(t.Context(), store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "inst-kagent/worker-1", entry.AgentInstanceID)
	require.Equal(t, "kagent/worker", entry.AgentRef, "the row names the agent the thread is bound to")
	require.Equal(t, 2*channels.DefaultThreadTTL, entry.TTL, "the binding slides with the thread's lifetime, and the row outlives it by as much again")

	sent := agent.lastStreamed()
	require.Empty(t, sent.TaskID, "a fresh turn lets the controller assign the task id")
	require.Empty(t, sent.ContextID, "the controller owns the conversation's context id")
	require.Equal(t, a2apkg.MessageRoleUser, sent.Role)
	require.Equal(t, "hi", sent.Parts[0].Text())
}

// The conversation is named after the message that opened it, so kagent lists
// the thread under the line it started with instead of an id. An adapter that
// renders its own title (Slack drops the mention and the command a user typed
// to address the bot) is taken at its word, and a turn with nothing to name it
// after creates the conversation unnamed rather than inventing a name.
func TestFacade_SendCompletionViaA2A_NamesTheConversation(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  channels.InboundMessage
		want string
	}{
		{
			name: "an adapter title wins over the raw text",
			msg:  withTitle(slackMsg(`/agent "sre" why is kong down?`), "why is kong down?"),
			want: "why is kong down?",
		},
		{
			name: "a pasted question is rendered on one line",
			msg:  slackMsg("why is this failing?\n\n  kubectl get pods\n"),
			want: "why is this failing? kubectl get pods",
		},
		{
			name: "an upload with no caption stays unnamed",
			msg:  withAttachment(slackMsg("")),
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
			f, _ := newA2AFacade(agent)

			ch, err := f.SendCompletion(t.Context(), tc.msg)
			require.NoError(t, err)
			drain(t, ch)
			require.Equal(t, []string{tc.want}, agent.createNames)
		})
	}
}

func withTitle(msg channels.InboundMessage, title string) channels.InboundMessage {
	msg.Title = title
	return msg
}

func withAttachment(msg channels.InboundMessage) channels.InboundMessage {
	msg.Attachments = []channels.Attachment{{Filename: "graph.png", ContentType: "image/png", Bytes: []byte("PNG")}}
	return msg
}

func TestFacade_SendCompletionViaA2A_LaterTurnsReuseTheBinding(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}
	require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{AgentRef: "kagent/worker", AgentInstanceID: "inst-from-before-the-restart"}))

	ch, err := f.SendCompletion(t.Context(), slackMsg("and the nodes?"))
	require.NoError(t, err)
	drain(t, ch)

	require.Empty(t, agent.createRequests, "a bound thread creates no session")
	require.Equal(t, []string{"inst-from-before-the-restart"}, agent.streamedOn)
}

// A retried first turn (the binding was not written, or the process died in
// between) reaches the controller with the same request id and gets the same
// session back instead of a second one.
func TestFacade_SendCompletionViaA2A_RetriedFirstTurnIsIdempotent(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}

	for range 2 {
		ch, err := f.SendCompletion(t.Context(), slackMsg("hi"))
		require.NoError(t, err)
		drain(t, ch)
		require.NoError(t, storetest.Expire(t.Context(), routes, key))
	}
	require.Len(t, agent.createRequests, 2)
	require.Equal(t, agent.createRequests[0], agent.createRequests[1])
	require.Equal(t, 1, agent.created, "the same request id yields the same session")
	require.Equal(t, []string{"inst-kagent/worker-1", "inst-kagent/worker-1"}, agent.streamedOn)
}

// The issue's turn: three rounds of "narrate, then call tools", then the final
// answer, which kagent sends twice — as a text-only working event and as the
// artifact. Every narration must arrive exactly once, and the answer must not be
// duplicated (klaus-gateway#197).
func TestFacade_SendCompletionViaA2A_NarrationDeliveredOnceWithFinalAnswer(t *testing.T) {
	const (
		narration1 = "Let me pull the HelmRelease from both clusters simultaneously."
		narration2 = "Both HelmReleases share the same chart version — the differences will be in the ConfigMaps."
		answer     = "Here is the focused diff on the klausGateway section:"
	)
	working := func(parts ...*a2apkg.Part) a2apkg.Event {
		msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, parts...)
		return a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, msg)
	}
	call := func(id string) *a2apkg.Part { return kagentPart("function_call", "kubectl_get", id) }
	resp := func(id string) *a2apkg.Part { return kagentPart("function_response", "kubectl_get", id) }
	user := a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("compare both clusters"))

	agent := newFakeAgent(
		// The controller yields the stored submitted task first, then echoes the
		// user's own message as the submitted event.
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateSubmitted, user),
		working(a2apkg.NewTextPart(narration1), call("c1"), call("c2")),
		working(resp("c1"), resp("c2")),
		working(a2apkg.NewTextPart(narration2), call("c3")),
		working(resp("c3")),
		working(a2apkg.NewTextPart(answer)), // the mirror of the final answer
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart(answer)),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	)
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), slackMsg("compare both clusters"))
	require.NoError(t, err)

	var narrations, texts []string
	var tools int
	var done bool
	for _, d := range drain(t, ch) {
		require.NoError(t, d.Err)
		switch {
		case d.Done:
			done = true
		case d.Kind == channels.DeltaNarration:
			narrations = append(narrations, d.Content)
		case d.Kind == channels.DeltaToolActivity:
			tools++
		case d.Content != "":
			texts = append(texts, d.Content)
		}
	}

	require.True(t, done)
	require.Equal(t, []string{narration1, narration2}, narrations)
	require.Equal(t, []string{answer}, texts, "the answer arrives once, from the artifact")
	require.Equal(t, 6, tools)
}

// kagentPart builds a kagent function_call/function_response DataPart the way
// the wire format spells it.
func kagentPart(kagentType, name, id string) *a2apkg.Part {
	p := a2apkg.NewDataPart(map[string]any{"name": name, "id": id})
	p.Metadata = map[string]any{"kagent_type": kagentType}
	return p
}

func TestFacade_SendCompletionViaA2A_ForwardsIdentity(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, _ := newA2AFacade(agent)

	msg := slackMsg("hi")
	msg.Channel = "slack"
	ch, err := f.SendCompletion(t.Context(), msg)
	require.NoError(t, err)
	drain(t, ch)

	require.Equal(t, "user-jwt", pkga2a.ForwardedTokenFromContext(agent.streamCtx))
}

// A refusal before the first event — the controller rejecting the turn — is
// returned synchronously so channels render it as the turn's outcome.
func TestFacade_SendCompletionViaA2A_RefusalIsSynchronous(t *testing.T) {
	agent := newFakeAgent()
	agent.streamErr = fmt.Errorf("%w: Session x already has an active task", pkga2a.ErrSessionBusy)
	f, _ := newA2AFacade(agent)

	_, err := f.SendCompletion(t.Context(), slackMsg("hi"))
	require.ErrorIs(t, err, pkga2a.ErrSessionBusy)

	agent = newFakeAgent()
	agent.createErr = fmt.Errorf("%w: kagent/worker: no Harness admits this AgentTemplate", pkga2a.ErrAgentUnavailable)
	f, _ = newA2AFacade(agent)
	_, err = f.SendCompletion(t.Context(), slackMsg("hi"))
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
	require.Empty(t, agent.streamed, "a refused session never gets a turn")
}

func TestFacade_SendCompletionViaA2A_MidStreamErrorPropagated(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil))
	agent.tailErr = errors.New("executor boom")
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), slackMsg("hi"))
	require.NoError(t, err)
	var gotErr error
	for _, d := range drain(t, ch) {
		if d.Err != nil {
			gotErr = d.Err
		}
	}
	require.ErrorContains(t, gotErr, "executor boom")
}

func TestFacade_SendCompletionViaA2A_InputRequired_EmitsPromptDelta(t *testing.T) {
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("approve the tool call?"))
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateInputRequired, msg))
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), slackMsg("hi"))
	require.NoError(t, err)
	deltas := drain(t, ch)
	require.Len(t, deltas, 1)
	require.Equal(t, channels.DeltaPrompt, deltas[0].Kind)
	require.Equal(t, "approve the tool call?", deltas[0].Content)
	require.Equal(t, string(taskInfo.TaskID), deltas[0].TaskID)
	require.Empty(t, agent.canceled, "a paused task is left paused, not canceled")
}

func TestFacade_SendCompletionViaA2A_AuthRequired_EmitsPromptDelta(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateAuthRequired, nil))
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), slackMsg("hi"))
	require.NoError(t, err)
	deltas := drain(t, ch)
	require.Len(t, deltas, 1)
	require.Equal(t, channels.DeltaPrompt, deltas[0].Kind)
}

// A HITL decision resumes the paused task in place: the message carries the
// paused task's id, and the typed response is built against the request that
// task is paused on (the defect of #215 cannot recur on this transport).
func TestFacade_HitlResumeCarriesThePausedTaskAndTypedResponse(t *testing.T) {
	prompt := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("Delete the pod?"))
	require.NoError(t, pkga2a.AttachHITL(prompt, pkga2a.ToolApprovalRequest{
		Type:  pkga2a.HITLTypeToolApprovalRequest,
		Hint:  "Delete the pod?",
		Tools: []pkga2a.HITLTool{{ID: "approval-1", CallID: "toolu_1", Name: "kubectl_delete"}},
	}))
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	agent.tasks[taskInfo.TaskID] = &a2apkg.Task{
		ID: taskInfo.TaskID, ContextID: taskInfo.ContextID,
		Status: a2apkg.TaskStatus{State: a2apkg.TaskStateInputRequired, Message: prompt},
	}
	f, routes := newA2AFacade(agent)
	require.NoError(t, storetest.Put(t.Context(), routes, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"},
		store.Entry{AgentRef: "kagent/worker", AgentInstanceID: "inst-1"}))

	msg := slackMsg("approve")
	msg.TaskID = string(taskInfo.TaskID)
	msg.Decision = &channels.HitlDecision{Type: channels.DecisionApprove}
	ch, err := f.SendCompletion(t.Context(), msg)
	require.NoError(t, err)
	drain(t, ch)

	require.Equal(t, []a2apkg.TaskID{taskInfo.TaskID}, agent.gotTasks, "the paused task is read for its request")
	sent := agent.lastStreamed()
	require.Equal(t, taskInfo.TaskID, sent.TaskID, "the decision message carries the paused task id")
	require.Contains(t, sent.Extensions, pkga2a.HITLExtensionURI)
	payload, ok := sent.Metadata[pkga2a.HITLExtensionURI].(map[string]any)
	require.True(t, ok)
	require.Equal(t, pkga2a.HITLTypeToolApprovalResponse, payload["type"])
	approvals, ok := payload["approvals"].([]any)
	require.True(t, ok)
	require.Len(t, approvals, 1)
	require.Equal(t, map[string]any{"id": "approval-1", "approved": true}, approvals[0])
	require.Equal(t, "approve", sent.Parts[0].Text(), "the decision keeps a readable label in the history")
}

func TestFacade_HitlResumeRefusedWhenTheTaskIsNotPaused(t *testing.T) {
	agent := newFakeAgent()
	agent.tasks[taskInfo.TaskID] = &a2apkg.Task{ID: taskInfo.TaskID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted}}
	f, routes := newA2AFacade(agent)
	require.NoError(t, storetest.Put(t.Context(), routes, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"},
		store.Entry{AgentRef: "kagent/worker", AgentInstanceID: "inst-1"}))

	msg := slackMsg("approve")
	msg.TaskID = string(taskInfo.TaskID)
	msg.Decision = &channels.HitlDecision{Type: channels.DecisionApprove}
	_, err := f.SendCompletion(t.Context(), msg)
	require.ErrorIs(t, err, channels.ErrNoPendingPrompt)
	require.Empty(t, agent.streamed)
}

// Stopping a turn cancels the task at the controller, not only the client
// stream, so the agent stops working.
func TestFacade_StoppedTurnCancelsTheTaskServerSide(t *testing.T) {
	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil),
	)
	agent.hold = make(chan struct{})
	f, _ := newA2AFacade(agent)

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := f.SendCompletion(ctx, slackMsg("long task"))
	require.NoError(t, err)
	cancel()
	drain(t, ch)

	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return len(agent.canceled) == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, []a2apkg.TaskID{taskInfo.TaskID}, agent.canceled)
	require.Equal(t, []string{"kagent/worker"}, agent.canceledOn, "the cancel is addressed to the thread's agent")
	require.NotNil(t, agent.streamCtx)
}

func TestFacade_SessionResumable(t *testing.T) {
	msg := slackMsg("still there?")
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}

	t.Run("no binding: starting fresh", func(t *testing.T) {
		f, _ := newA2AFacade(newFakeAgent())
		exists, checked := f.SessionResumable(t.Context(), msg)
		require.True(t, checked)
		require.False(t, exists)
	})

	t.Run("bound to a live session", func(t *testing.T) {
		agent := newFakeAgent()
		agent.sessions["inst-1"] = pkga2a.Session{ID: "inst-1", State: "RUNTIME_STATE_SUSPENDED"}
		f, routes := newA2AFacade(agent)
		require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{AgentRef: "kagent/worker", AgentInstanceID: "inst-1"}))
		exists, checked := f.SessionResumable(t.Context(), msg)
		require.True(t, checked)
		require.True(t, exists)
	})

	t.Run("bound to a deleted session clears the binding and keeps the thread", func(t *testing.T) {
		f, routes := newA2AFacade(newFakeAgent())
		require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{AgentRef: "kagent/worker", AgentInstanceID: "inst-gone", Initiator: "U1"}))
		exists, checked := f.SessionResumable(t.Context(), msg)
		require.True(t, checked)
		require.False(t, exists)
		entry, ok, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.True(t, ok)
		require.Empty(t, entry.AgentInstanceID, "the next turn must create a fresh session")
		require.Equal(t, "kagent/worker", entry.AgentRef, "the thread keeps its agent")
		require.Equal(t, "U1", entry.Initiator, "and its initiator")
	})

	t.Run("lookup error is indeterminate", func(t *testing.T) {
		agent := newFakeAgent()
		agent.getErr = errors.New("boom")
		f, routes := newA2AFacade(agent)
		require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{AgentRef: "kagent/worker", AgentInstanceID: "inst-1"}))
		_, checked := f.SessionResumable(t.Context(), msg)
		require.False(t, checked)
	})

	t.Run("no agent client configured", func(t *testing.T) {
		_, checked := (&channels.Facade{}).SessionResumable(t.Context(), msg)
		require.False(t, checked)
	})
}

func TestFacade_ResetSession(t *testing.T) {
	msg := slackMsg("resend")
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}

	t.Run("deletes the session and clears the binding", func(t *testing.T) {
		agent := newFakeAgent()
		agent.sessions["inst-1"] = pkga2a.Session{ID: "inst-1"}
		f, routes := newA2AFacade(agent)
		require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{
			AgentRef: "kagent/worker", AgentInstanceID: "inst-1", Initiator: "U1",
			Workspace: &store.WorkspaceChoice{Namespace: "kagent", Name: "klaus-dev"},
		}))
		reset, err := f.ResetSession(t.Context(), msg)
		require.NoError(t, err)
		require.True(t, reset)
		require.Equal(t, []string{"inst-1"}, agent.deleted)
		entry, ok, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.True(t, ok)
		require.Empty(t, entry.AgentInstanceID)
		require.Nil(t, entry.Workspace, "the workspace choice goes with the binding: the thread is asked again")
		require.Equal(t, "U1", entry.Initiator, "the thread keeps its initiator")
	})

	t.Run("delete failure is reported", func(t *testing.T) {
		agent := newFakeAgent()
		agent.deleteErr = errors.New("boom")
		f, routes := newA2AFacade(agent)
		require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{AgentRef: "kagent/worker", AgentInstanceID: "inst-1"}))
		reset, err := f.ResetSession(t.Context(), msg)
		require.Error(t, err)
		require.False(t, reset)
	})

	t.Run("no binding: nothing to reset", func(t *testing.T) {
		f, _ := newA2AFacade(newFakeAgent())
		reset, err := f.ResetSession(t.Context(), msg)
		require.NoError(t, err)
		require.False(t, reset)
	})

	t.Run("no agent client configured", func(t *testing.T) {
		reset, err := (&channels.Facade{}).ResetSession(t.Context(), msg)
		require.NoError(t, err)
		require.False(t, reset)
	})
}

// The Go ADK's wire shape for two text runs around a tool call: every run is
// streamed as appended artifact chunks and then re-sent whole on the same
// artifact (klaus-gateway#242). Each run reaches the channel once, the runs
// are separated by a paragraph, and the tool activity is unaffected.
func TestFacade_SendCompletionViaA2A_StreamedRunsRenderOnce(t *testing.T) {
	const (
		run1a, run1b = "I'll look for the right tools. ", "Let me first discover what's available."
		run2         = "7 nodes, all Ready, v1.35.8 on every node."
	)
	stream := func(id a2apkg.ArtifactID, first bool, text string) a2apkg.Event {
		if first {
			ev := a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart(text))
			ev.Artifact.ID = id
			return ev
		}
		return a2apkg.NewArtifactUpdateEvent(taskInfo, id, a2apkg.NewTextPart(text))
	}
	finished := func(id a2apkg.ArtifactID, parts ...*a2apkg.Part) a2apkg.Event {
		ev := a2apkg.NewArtifactUpdateEvent(taskInfo, id, parts...)
		ev.Append, ev.LastChunk = false, true
		return ev
	}
	toolArtifact := func(part *a2apkg.Part) a2apkg.Event {
		ev := a2apkg.NewArtifactEvent(taskInfo, part)
		ev.LastChunk = true
		return ev
	}

	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil),
		stream("run-1", true, run1a),
		stream("run-1", false, run1b),
		finished("run-1", a2apkg.NewTextPart(run1a+run1b), kagentPart("function_call", "x_kubernetes_cluster_health", "c1")),
		toolArtifact(kagentPart("function_response", "x_kubernetes_cluster_health", "c1")),
		stream("run-2", true, run2),
		finished("run-2", a2apkg.NewTextPart(run2)),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	)
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), slackMsg("how many nodes?"))
	require.NoError(t, err)

	var text strings.Builder
	var tools int
	var done bool
	for _, d := range drain(t, ch) {
		require.NoError(t, d.Err)
		switch {
		case d.Done:
			done = true
		case d.Kind == channels.DeltaToolActivity:
			tools++
		case d.Kind == channels.DeltaText:
			text.WriteString(d.Content)
		}
	}
	require.True(t, done)
	require.Equal(t, run1a+run1b+"\n\n"+run2, text.String(), "each run once, runs separated by a paragraph")
	require.Equal(t, 2, tools)
	require.Empty(t, agent.canceled, "a completed turn is never cancelled")
}

// A channel that stops listening after the task already completed must not
// have the task cancelled: the agent is done, and a cancel would record the
// finished turn as canceled (klaus-gateway#242). The channel's buffer is
// filled so the completed event is seen by the producer while the channel is
// already gone.
func TestFacade_CompletedTurnIsNotCancelledWhenTheChannelLeavesLate(t *testing.T) {
	events := []a2apkg.Event{
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
	}
	// Exactly as many text deltas as the channel buffers, so the producer sees
	// the completed event and blocks on delivering it.
	for i := range 16 {
		events = append(events, a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart(fmt.Sprintf("chunk %d ", i))))
	}
	events = append(events, a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	agent := newFakeAgent(events...)
	f, _ := newA2AFacade(agent)

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := f.SendCompletion(ctx, slackMsg("long answer"))
	require.NoError(t, err)
	// Nothing is read: the producer fills the channel's buffer, then blocks on
	// the completed event until the channel goes away.
	time.Sleep(50 * time.Millisecond)
	cancel()
	drain(t, ch)

	require.Empty(t, agent.canceled, "the task had completed before the channel left")
}

// A shutdown mid-turn (context cause channels.ErrShutdown) leaves the task
// running at the controller and its record in the routing store, for the next
// process to resubscribe to; a /stop (a plain cancellation) cancels the task
// and drops the record.
func TestFacade_ShutdownLeavesTheTaskRunningAndRecorded(t *testing.T) {
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}
	// run starts a turn, waits for its record, ends the turn context with
	// cause, and drains the stream so the pump's bookkeeping has completed.
	run := func(t *testing.T, cause error) (*fakeAgent, store.Store) {
		agent := newFakeAgent(
			&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
			a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil),
		)
		agent.hold = make(chan struct{})
		f, routes := newA2AFacade(agent)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		msg := slackMsg("long task")
		msg.Resume = map[string]string{"slack_user": "U1", "message_ts": "1700.0001"}
		ch, err := f.SendCompletion(ctx, msg)
		require.NoError(t, err)
		// The record is written once the controller has named the task.
		require.Eventually(t, func() bool {
			entry, ok, err := routes.Get(t.Context(), key)
			return err == nil && ok && entry.TaskID == string(taskInfo.TaskID)
		}, 2*time.Second, 10*time.Millisecond, "the in-flight task is recorded on the thread's binding")
		cancel(cause)
		drain(t, ch)
		return agent, routes
	}

	t.Run("shutdown", func(t *testing.T) {
		agent, routes := run(t, channels.ErrShutdown)

		agent.mu.Lock()
		defer agent.mu.Unlock()
		require.Empty(t, agent.canceled, "a shutdown leaves the task running")
		entry, ok, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, string(taskInfo.TaskID), entry.TaskID, "the record survives for the next process")
		require.Equal(t, "U1", entry.Resume["slack_user"], "with the channel's resume data")
	})

	t.Run("stop", func(t *testing.T) {
		agent, routes := run(t, nil)

		agent.mu.Lock()
		defer agent.mu.Unlock()
		require.Equal(t, []a2apkg.TaskID{taskInfo.TaskID}, agent.canceled, "a /stop cancels the task at the controller")
		entry, ok, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.True(t, ok, "the thread's binding stays")
		require.Empty(t, entry.TaskID, "the in-flight record is dropped")
		require.Nil(t, entry.Resume)
	})
}

// A fresh turn on a thread whose row still carries what an earlier turn
// delivered starts its record from nothing: the old delivery is no measure of
// the new answer, and a restart during the new turn must not cut it.
func TestFacade_FreshTurnDropsTheStaleDeliveryRecord(t *testing.T) {
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}
	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil),
	)
	agent.hold = make(chan struct{})
	f, routes := newA2AFacade(agent)
	require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{
		AgentRef: "kagent/worker", AgentInstanceID: "inst-1",
		Delivered: store.Delivered{TextLen: 99},
		CreatedAt: time.Now(), LastSeen: time.Now(),
	}))
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	ch, err := f.SendCompletion(ctx, slackMsg("again"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		entry, ok, err := routes.Get(t.Context(), key)
		return err == nil && ok && entry.TaskID == string(taskInfo.TaskID)
	}, 2*time.Second, 10*time.Millisecond, "the new task is recorded")
	entry, _, err := routes.Get(t.Context(), key)
	require.NoError(t, err)
	require.True(t, entry.Delivered.IsZero(), "the stale delivery record went with the new task's record: %+v", entry.Delivered)
	cancel(channels.ErrShutdown)
	drain(t, ch)
}

// A turn that ran to completion clears its record; a context cancelled after
// the terminal delta (the channel tearing the turn down) does not cancel the
// completed task.
func TestFacade_CompletedTurnClearsTheRecordAndIsNotCanceled(t *testing.T) {
	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart("done")),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	)
	f, routes := newA2AFacade(agent)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := f.SendCompletion(ctx, slackMsg("quick"))
	require.NoError(t, err)
	var deltas []channels.OutboundDelta
	for d := range ch {
		deltas = append(deltas, d)
		if d.Done {
			cancel() // the channel adapter's turn teardown
		}
	}
	require.True(t, deltas[len(deltas)-1].Done)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	require.Empty(t, agent.canceled, "a completed task is never cancelled by the teardown")
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}
	entry, ok, err := routes.Get(t.Context(), key)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, entry.TaskID)
}

// ResumeTurn resubscribes to a turn a previous process left running and
// delivers its result: a task that finished meanwhile arrives whole and its
// artifacts are rendered as the answer; the record is cleared afterwards.
func TestFacade_ResumeTurnDeliversAFinishedTask(t *testing.T) {
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}
	delivered := store.Delivered{TextLen: 11}
	seed := func(t *testing.T, agent *fakeAgent) (*channels.Facade, store.Store) {
		f, routes := newA2AFacade(agent)
		require.NoError(t, storetest.Put(t.Context(), routes, key, store.Entry{
			AgentRef: "kagent/worker", AgentInstanceID: "inst-1", TaskID: "task-7",
			Resume:    map[string]string{"slack_user": "U1"},
			Delivered: delivered,
			CreatedAt: time.Now(), LastSeen: time.Now(),
		}))
		return f, routes
	}

	t.Run("artifacts", func(t *testing.T) {
		agent := newFakeAgent()
		agent.subscribeEvents = []a2apkg.Event{&a2apkg.Task{
			ID: "task-7", ContextID: "ctx-1",
			Status:    a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
			Artifacts: []*a2apkg.Artifact{{ID: "a1", Parts: a2apkg.ContentParts{a2apkg.NewTextPart("the answer")}}},
		}}
		f, routes := seed(t, agent)

		turns, err := f.InFlightTurns(t.Context(), "slack")
		require.NoError(t, err)
		require.Len(t, turns, 1)
		require.Equal(t, "task-7", turns[0].TaskID)
		require.Equal(t, "U1", turns[0].Msg.Resume["slack_user"])
		require.Equal(t, "1700.0001", turns[0].Msg.ThreadID)
		require.Equal(t, "kagent/worker", turns[0].Msg.AgentRef)
		require.Equal(t, delivered, turns[0].Delivered, "what the previous process posted travels with the turn")
		none, err := f.InFlightTurns(t.Context(), "other")
		require.NoError(t, err)
		require.Empty(t, none, "other channels' bindings are not listed")

		turn, ok, err := f.InFlightTurn(t.Context(), slackMsg("and then?"))
		require.NoError(t, err)
		require.True(t, ok)
		msg := turn.Msg
		msg.BearerToken = "user-jwt"
		ch, err := f.ResumeTurn(t.Context(), msg, turn.TaskID)
		require.NoError(t, err)
		deltas := drain(t, ch)
		require.Equal(t, []channels.OutboundDelta{{Content: "the answer"}, {Done: true}}, deltas)

		agent.mu.Lock()
		require.Equal(t, []a2apkg.TaskID{"task-7"}, agent.subscribed)
		require.Equal(t, []string{"kagent/worker"}, agent.subscribedOn, "the resubscription is addressed to the thread's agent")
		agent.mu.Unlock()
		entry, ok, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "inst-1", entry.AgentInstanceID, "the binding stays")
		require.True(t, entry.Delivered.IsZero(), "the delivery record goes with the task")
		require.Empty(t, entry.TaskID, "the delivered turn is no longer in flight")
		_, ok, err = f.InFlightTurn(t.Context(), slackMsg("again?"))
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("history fallback", func(t *testing.T) {
		agent := newFakeAgent()
		agent.subscribeEvents = []a2apkg.Event{&a2apkg.Task{
			ID: "task-7", ContextID: "ctx-1",
			Status:  a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
			History: []*a2apkg.Message{a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("q")), a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("from history"))},
		}}
		f, _ := seed(t, agent)
		msg := slackMsg("")
		ch, err := f.ResumeTurn(t.Context(), msg, "task-7")
		require.NoError(t, err)
		require.Equal(t, []channels.OutboundDelta{{Content: "from history"}, {Done: true}}, drain(t, ch))
	})

	// The answer read back at completion is rendered as the stream renders it:
	// a paragraph break between two artifacts, as the live stream puts one
	// before an artifact that follows rendered text. A channel that cuts off
	// the prefix it posted while the stream was live therefore cuts at the
	// right byte, and the continued text does not run two sentences together.
	t.Run("still running, two artifacts", func(t *testing.T) {
		agent := newFakeAgent()
		info := a2apkg.TaskInfo{TaskID: "task-7", ContextID: "ctx-1"}
		agent.subscribeEvents = []a2apkg.Event{
			&a2apkg.Task{ID: "task-7", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking}},
			a2apkg.NewStatusUpdateEvent(info, a2apkg.TaskStateCompleted, nil),
		}
		agent.tasks["task-7"] = &a2apkg.Task{
			ID: "task-7", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
			Artifacts: []*a2apkg.Artifact{
				{ID: "a1", Parts: a2apkg.ContentParts{a2apkg.NewTextPart("Last page remaining: valkey, zot.")}},
				{ID: "a2", Parts: a2apkg.ContentParts{a2apkg.NewTextPart("Here's the full picture.")}},
			},
		}
		f, _ := seed(t, agent)
		ch, err := f.ResumeTurn(t.Context(), slackMsg(""), "task-7")
		require.NoError(t, err)
		require.Equal(t, []channels.OutboundDelta{{Content: "Last page remaining: valkey, zot.\n\nHere's the full picture."}, {Done: true}}, drain(t, ch))
	})

	// A task still running when the resubscription attaches: the text chunks
	// streamed from here on are not the whole answer (what the agent wrote in
	// between is not replayed), so the answer is read back whole at completion;
	// tool activity still streams.
	t.Run("still running", func(t *testing.T) {
		agent := newFakeAgent()
		info := a2apkg.TaskInfo{TaskID: "task-7", ContextID: "ctx-1"}
		call := a2apkg.NewDataPart(map[string]any{"id": "c1", "name": "kubectl_get", "args": map[string]any{"kind": "pods"}})
		call.Metadata = map[string]any{"kagent_type": "function_call"}
		agent.subscribeEvents = []a2apkg.Event{
			&a2apkg.Task{ID: "task-7", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking}},
			a2apkg.NewStatusUpdateEvent(info, a2apkg.TaskStateWorking, a2apkg.NewMessage(a2apkg.MessageRoleAgent, call)),
			a2apkg.NewArtifactEvent(info, a2apkg.NewTextPart("ucky by the superstitious.")),
			a2apkg.NewStatusUpdateEvent(info, a2apkg.TaskStateCompleted, nil),
		}
		agent.tasks["task-7"] = &a2apkg.Task{
			ID: "task-7", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
			Artifacts: []*a2apkg.Artifact{{ID: "a1", Parts: a2apkg.ContentParts{a2apkg.NewTextPart("13 is considered unlucky by the superstitious.")}}},
		}
		f, _ := seed(t, agent)
		ch, err := f.ResumeTurn(t.Context(), slackMsg(""), "task-7")
		require.NoError(t, err)
		deltas := drain(t, ch)
		var texts []string
		tools := 0
		for _, d := range deltas {
			if d.Kind == channels.DeltaToolActivity {
				tools++
			}
			if d.Kind == channels.DeltaText && d.Content != "" {
				texts = append(texts, d.Content)
			}
		}
		require.Equal(t, 1, tools, "tool activity streams live")
		require.Equal(t, []string{"13 is considered unlucky by the superstitious."}, texts, "the answer is the completed task's, whole, not the tail that streamed")
		require.True(t, deltas[len(deltas)-1].Done)
		agent.mu.Lock()
		defer agent.mu.Unlock()
		require.Equal(t, []a2apkg.TaskID{"task-7"}, agent.gotTasks, "the answer is read back once the task completed")
	})

	// The calls made before the resubscription are in the snapshot only; the
	// task read back at completion counts them, and the call that streamed
	// counts once.
	t.Run("still running, usage", func(t *testing.T) {
		usage := func(prompt, completion int) map[string]any {
			return map[string]any{"kagent.dev/a2a/usage": map[string]any{
				"promptTokenCount": float64(prompt), "candidatesTokenCount": float64(completion), "totalTokenCount": float64(prompt + completion),
			}}
		}
		before := &a2apkg.Artifact{ID: "a1", Metadata: usage(100, 20), Parts: a2apkg.ContentParts{a2apkg.NewTextPart("Let me check.")}}
		streamed := &a2apkg.Artifact{ID: "a2", Metadata: usage(200, 40), Parts: a2apkg.ContentParts{a2apkg.NewTextPart("7 nodes.")}}
		agent := newFakeAgent()
		info := a2apkg.TaskInfo{TaskID: "task-7", ContextID: "ctx-1"}
		agent.subscribeEvents = []a2apkg.Event{
			&a2apkg.Task{ID: "task-7", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking}, Artifacts: []*a2apkg.Artifact{before}},
			&a2apkg.TaskArtifactUpdateEvent{TaskID: "task-7", ContextID: "ctx-1", Artifact: streamed},
			a2apkg.NewStatusUpdateEvent(info, a2apkg.TaskStateCompleted, nil),
		}
		agent.tasks["task-7"] = &a2apkg.Task{
			ID: "task-7", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
			Artifacts: []*a2apkg.Artifact{before, streamed},
		}
		f, _ := seed(t, agent)
		ch, err := f.ResumeTurn(t.Context(), slackMsg(""), "task-7")
		require.NoError(t, err)
		deltas := drain(t, ch)
		var usages []channels.TurnUsage
		for _, d := range deltas {
			if d.Usage != nil {
				usages = append(usages, *d.Usage)
			}
		}
		require.ElementsMatch(t, []channels.TurnUsage{
			{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
			{InputTokens: 200, OutputTokens: 40, TotalTokens: 240},
		}, usages)
		require.True(t, deltas[len(deltas)-1].Done)
	})

	t.Run("gone at the controller", func(t *testing.T) {
		agent := newFakeAgent()
		agent.subscribeErr = a2apkg.ErrTaskNotFound
		f, routes := seed(t, agent)
		_, err := f.ResumeTurn(t.Context(), slackMsg(""), "task-7")
		require.ErrorIs(t, err, a2apkg.ErrTaskNotFound)
		entry, _, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.Empty(t, entry.TaskID, "nothing is left to deliver, so the record goes")
		require.Equal(t, "inst-1", entry.AgentInstanceID)
	})

	t.Run("transient failure keeps the record", func(t *testing.T) {
		agent := newFakeAgent()
		agent.subscribeErr = errors.New("controller unavailable")
		f, routes := seed(t, agent)
		_, err := f.ResumeTurn(t.Context(), slackMsg(""), "task-7")
		require.Error(t, err)
		entry, _, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.Equal(t, "task-7", entry.TaskID, "a later attempt can still deliver it")
	})
}

func TestFacade_ResumesTurns(t *testing.T) {
	f, _ := newA2AFacade(newFakeAgent())
	require.False(t, f.ResumesTurns(), "a store that dies with the process cannot deliver after a restart")
	f.Durable = true
	require.True(t, f.ResumesTurns())
	require.False(t, (&channels.Facade{Durable: true}).ResumesTurns(), "no kagent client, no resubscription")
}

// A thread has one row: the channel's own facts, the agent it is bound to and
// its Session live side by side, and no writer erases another's fields.
func TestSessionFor_OneRowPerThread(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}

	// The channel records the thread's initiator before the first turn.
	require.NoError(t, f.UpdateThreadRecord(t.Context(), "slack", "C1", "1700.0001", func(e *store.Entry, _ bool) bool {
		e.Initiator = "U1"
		return true
	}))

	ch, err := f.SendCompletion(t.Context(), slackMsg("hi"))
	require.NoError(t, err)
	drain(t, ch)

	entry, ok, err := routes.Get(t.Context(), key)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "U1", entry.Initiator, "the binding write keeps the channel's fields")
	require.Equal(t, "kagent/worker", entry.AgentRef)
	require.Equal(t, "inst-kagent/worker-1", entry.AgentInstanceID)

	// A second turn on the same agent reuses the binding and slides the row.
	require.NoError(t, routes.Update(t.Context(), key, func(e *store.Entry, _ bool) bool {
		e.LastSeen = time.Now().Add(-time.Hour)
		return true
	}))
	ch, err = f.SendCompletion(t.Context(), slackMsg("and now?"))
	require.NoError(t, err)
	drain(t, ch)

	require.Len(t, agent.createRequests, 1, "a bound thread creates no second session")
	entry, _, err = routes.Get(t.Context(), key)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), entry.LastSeen, time.Minute, "the turn refreshed the row")

	// A turn naming another agent rebinds the thread, keeping what is the
	// channel's.
	other := slackMsg("you then")
	other.AgentRef = "kagent/other"
	ch, err = f.SendCompletion(t.Context(), other)
	require.NoError(t, err)
	drain(t, ch)

	entry, _, err = routes.Get(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "kagent/other", entry.AgentRef)
	require.Equal(t, "inst-kagent/other-2", entry.AgentInstanceID)
	require.Equal(t, "U1", entry.Initiator)
}
