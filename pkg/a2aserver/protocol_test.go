package a2aserver_test

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
)

type protocolAgents struct {
	fakeAgents
	task              *a2a.Task
	sendConfiguration *a2a.SendMessageConfig
	getHistoryLength  *int
	refusal           error
	unary             bool
}

func (agents *protocolAgents) SendMessage(_ context.Context, _ string, _ *a2a.Message, configuration *a2a.SendMessageConfig) (a2a.SendMessageResult, error) {
	agents.unary = true
	agents.sendConfiguration = configuration
	return agents.task, agents.refusal
}

func (agents *protocolAgents) StreamWithConfig(_ context.Context, _ string, _ *a2a.Message, configuration *a2a.SendMessageConfig) iter.Seq2[a2a.Event, error] {
	agents.sendConfiguration = configuration
	return func(yield func(a2a.Event, error) bool) { yield(agents.task, agents.refusal) }
}

func (agents *protocolAgents) GetTaskWithHistory(_ context.Context, _ a2a.TaskID, historyLength *int) (*a2a.Task, error) {
	agents.getHistoryLength = historyLength
	return agents.task, agents.refusal
}

func TestSendUsesNativeTaskExecution(t *testing.T) {
	for _, immediately := range []bool{false, true} {
		t.Run(fmt.Sprint(immediately), func(t *testing.T) {
			agents := &protocolAgents{task: &a2a.Task{ID: "task-1", ContextID: newSessionID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}}
			_, card := serve(t, agents)
			configuration := &a2a.SendMessageConfig{ReturnImmediately: immediately, HistoryLength: new(0), AcceptedOutputModes: []string{"text/plain"}}
			result, err := client(t, card, clientToken).SendMessage(t.Context(), &a2a.SendMessageRequest{
				Tenant: tenant, Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")), Config: configuration,
			})
			require.NoError(t, err)
			require.True(t, agents.unary)
			require.Equal(t, configuration, agents.sendConfiguration)
			require.Equal(t, a2a.TaskStateSubmitted, result.(*a2a.Task).Status.State)
		})
	}
}

func TestHistoryLimitsOnEveryTaskResponse(t *testing.T) {
	for _, limit := range []*int{nil, new(0), new(1), new(3), new(10)} {
		for _, operation := range []string{"send", "stream", "get"} {
			t.Run(fmt.Sprintf("%s/%v", operation, limit), func(t *testing.T) {
				history := []*a2a.Message{
					a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("one")),
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("two")),
					a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("three")),
				}
				agents := &protocolAgents{task: &a2a.Task{ID: "task-1", ContextID: newSessionID, History: history}}
				_, card := serve(t, agents)
				caller := client(t, card, clientToken)
				request := &a2a.SendMessageRequest{Tenant: tenant, Message: a2a.NewMessage(a2a.MessageRoleUser), Config: &a2a.SendMessageConfig{HistoryLength: limit}}
				var task *a2a.Task
				switch operation {
				case "send":
					result, err := caller.SendMessage(t.Context(), request)
					require.NoError(t, err)
					task = result.(*a2a.Task)
				case "stream":
					for event, err := range caller.SendStreamingMessage(t.Context(), request) {
						require.NoError(t, err)
						task = event.(*a2a.Task)
					}
				case "get":
					var err error
					task, err = caller.GetTask(t.Context(), &a2a.GetTaskRequest{Tenant: tenant, ID: "task-1", HistoryLength: limit})
					require.NoError(t, err)
					require.Equal(t, limit, agents.getHistoryLength)
				}
				length := len(history)
				if limit != nil {
					length = min(length, *limit)
				}
				require.Len(t, task.History, length)
				if length > 0 {
					require.Equal(t, history[len(history)-length:], task.History)
				}
				require.Equal(t, history, agents.task.History)
			})
		}
	}
}

func TestRecognizedErrorsKeepCodesAndHideBackendText(t *testing.T) {
	for _, sentinel := range []error{
		a2a.ErrUnsupportedOperation, a2a.ErrTaskNotCancelable, a2a.ErrUnsupportedContentType,
		a2a.ErrInvalidRequest, a2a.ErrInvalidParams, a2a.ErrUnauthenticated, a2a.ErrUnauthorized,
		a2a.ErrTaskNotFound, a2a.ErrInternalError, a2a.ErrPushNotificationNotSupported,
		a2a.ErrMethodNotFound, a2a.ErrVersionNotSupported, a2a.ErrInvalidAgentResponse,
		a2a.ErrExtensionSupportRequired, a2a.ErrExtendedCardNotConfigured,
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			agents := &protocolAgents{refusal: fmt.Errorf("%w: private backend details", sentinel)}
			_, card := serve(t, agents)
			_, err := client(t, card, clientToken).GetTask(t.Context(), &a2a.GetTaskRequest{Tenant: tenant, ID: "task-1"})
			require.ErrorIs(t, err, sentinel)
			require.NotContains(t, err.Error(), "private backend details")
		})
	}
}

func TestNegativeHistoryLengthRefusedBeforeSessionCreation(t *testing.T) {
	agents := &protocolAgents{}
	handler := &a2aserver.Handler{Agents: agents, Organizations: organizations{}}
	request := &a2a.SendMessageRequest{Tenant: tenant, Message: a2a.NewMessage(a2a.MessageRoleUser), Config: &a2a.SendMessageConfig{HistoryLength: new(-1)}}
	_, err := handler.SendMessage(t.Context(), request)
	require.ErrorIs(t, err, a2a.ErrInvalidParams)
	for _, err := range handler.SendStreamingMessage(t.Context(), request) {
		require.ErrorIs(t, err, a2a.ErrInvalidParams)
	}
	_, err = handler.GetTask(t.Context(), &a2a.GetTaskRequest{Tenant: tenant, ID: "task-1", HistoryLength: new(-1)})
	require.ErrorIs(t, err, a2a.ErrInvalidParams)
	require.Empty(t, agents.started)
}

func TestTaskIDWithoutContextDoesNotStartSession(t *testing.T) {
	agents := &protocolAgents{task: &a2a.Task{ID: "task-1", ContextID: newSessionID}}
	handler := &a2aserver.Handler{Agents: agents, Organizations: organizations{}}
	message := a2a.NewMessage(a2a.MessageRoleUser)
	message.TaskID = "task-1"
	_, err := handler.SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: tenant, Message: message})
	require.NoError(t, err)
	require.Empty(t, agents.started)
}
