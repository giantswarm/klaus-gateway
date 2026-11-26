package a2a_test

import (
	"context"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

func TestClientNativeSendPreservesConfigurationAndMetadata(t *testing.T) {
	for _, immediately := range []bool{false, true} {
		fake := readyFake(t)
		client := fake.serve(t, pkga2a.Config{})
		ctx, callContext := a2asrv.NewCallContext(asUser(t.Context(), userToken), a2asrv.NewServiceParams(nil))
		ctx, _, err := a2aext.NewServerPropagator(&a2aext.ServerPropagatorConfig{MetadataPredicate: func(context.Context, string) bool { return true }}).Before(ctx, callContext, &a2asrv.Request{Payload: &a2a.SendMessageRequest{
			Message: a2a.NewMessage(a2a.MessageRoleUser), Metadata: map[string]any{"com.example/session": "ref"},
		}})
		require.NoError(t, err)
		message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
		configuration := &a2a.SendMessageConfig{ReturnImmediately: immediately, HistoryLength: new(0), AcceptedOutputModes: []string{"text/plain"}}
		result, err := client.SendMessage(ctx, sessionID, message, configuration)
		require.NoError(t, err)
		require.Equal(t, a2a.TaskStateSubmitted, result.(*a2a.Task).Status.State)
		require.Equal(t, sreTenant, fake.sendRequest.GetTenant())
		require.Equal(t, sessionID, fake.sendRequest.GetMessage().GetContextId())
		require.Equal(t, immediately, fake.sendRequest.GetConfiguration().GetReturnImmediately())
		require.NotNil(t, fake.sendRequest.GetConfiguration().HistoryLength)
		require.Zero(t, fake.sendRequest.GetConfiguration().GetHistoryLength())
		require.Equal(t, []string{"text/plain"}, fake.sendRequest.GetConfiguration().GetAcceptedOutputModes())
		require.Equal(t, map[string]any{"com.example/session": "ref"}, fake.requestMetadata)
		require.Contains(t, fake.calls, "SendMessage")
		require.NotContains(t, fake.calls, "SendStreamingMessage")
		require.Empty(t, message.ContextID)
		_, err = client.SendMessage(asUser(t.Context(), userToken), sessionID, a2a.NewMessage(a2a.MessageRoleUser), nil)
		require.NoError(t, err)
		require.Empty(t, fake.requestMetadata)
	}
}

func TestClientNativeSendResumesSuspendedSession(t *testing.T) {
	fake := readyFake(t)
	fake.sessions[sessionID].State = apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED
	client := fake.serve(t, pkga2a.Config{})
	_, err := client.SendMessage(asUser(t.Context(), userToken), sessionID, a2a.NewMessage(a2a.MessageRoleUser), &a2a.SendMessageConfig{ReturnImmediately: true})
	require.NoError(t, err)
	require.Equal(t, []string{sessionID}, fake.resumed)
	require.Equal(t, 2, fake.callCount("SendMessage"))
	fake.sessions[sessionID].State = apiv1alpha1.RuntimeState_RUNTIME_STATE_FAILED
	_, err = client.SendMessage(asUser(t.Context(), userToken), sessionID, a2a.NewMessage(a2a.MessageRoleUser), nil)
	require.ErrorIs(t, err, a2a.ErrUnsupportedOperation)
	require.Len(t, fake.resumed, 1)
}

func TestClientStreamAndGetForwardHistoryLimit(t *testing.T) {
	fake := readyFake(t)
	fake.tasks["task-1"] = &a2a.Task{ID: "task-1", ContextID: sessionID}
	client := fake.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)
	for _, err := range client.StreamWithConfig(ctx, sessionID, a2a.NewMessage(a2a.MessageRoleUser), &a2a.SendMessageConfig{HistoryLength: new(0)}) {
		require.NoError(t, err)
	}
	require.NotNil(t, fake.sendRequest.GetConfiguration().HistoryLength)
	_, err := client.GetTaskWithHistory(ctx, "task-1", new(0))
	require.NoError(t, err)
	require.NotNil(t, fake.taskRequest.HistoryLength)
	require.Zero(t, fake.taskRequest.GetHistoryLength())
}
