package refusals_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/klaus-gateway/pkg/refusals"
)

// fakeController answers SessionService/GetSession from sessions; a missing
// id answers NotFound, as the controller does for a Session it never had.
type fakeController struct {
	sessions map[string]*apiv1alpha1.Session
	calls    int
}

func (f *fakeController) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	if method != apiv1alpha1.SessionService_GetSession_FullMethodName {
		return status.Errorf(codes.Unimplemented, "%s", method)
	}
	f.calls++
	session, ok := f.sessions[args.(*apiv1alpha1.GetSessionRequest).GetSessionId()]
	if !ok {
		return status.Error(codes.NotFound, "Session not found")
	}
	proto.Merge(reply.(*apiv1alpha1.GetSessionResponse), &apiv1alpha1.GetSessionResponse{Session: session})
	return nil
}

func (f *fakeController) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("no streams")
}

func kagentSessions(t *testing.T, controller *fakeController) *refusals.KagentSessions {
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token\n"), 0o600))
	return &refusals.KagentSessions{Conn: controller, TokenFile: token}
}

func TestKagentSessions_DeletedSessionAnswersWithItsTombstone(t *testing.T) {
	controller := &fakeController{sessions: map[string]*apiv1alpha1.Session{
		"short-lived": {
			Id:    "short-lived",
			State: apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED,
			Agent: &apiv1alpha1.ResourceReference{Namespace: "org-otter", Name: "refusal-probe"},
		},
	}}
	sessions := kagentSessions(t, controller)

	namespace, agent, err := sessions.SessionAgent(context.Background(), "short-lived")
	require.NoError(t, err, "a Session deleted before the read is still attributed to its Agent")
	require.Equal(t, "org-otter", namespace)
	require.Equal(t, "refusal-probe", agent)

	_, _, err = sessions.SessionAgent(context.Background(), "short-lived")
	require.NoError(t, err)
	require.Equal(t, 1, controller.calls, "the answer is remembered")
}

func TestKagentSessions_UnknownSessionIsNotASession(t *testing.T) {
	_, _, err := kagentSessions(t, &fakeController{}).SessionAgent(context.Background(), "never-was")
	require.ErrorIs(t, err, refusals.ErrNotSession)
}
