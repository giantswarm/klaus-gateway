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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

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

func kubeAgents(objects ...runtime.Object) *refusals.KubeAgents {
	scheme := runtime.NewScheme()
	return &refusals.KubeAgents{Client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		refusals.AgentsGVR:    "AgentList",
		refusals.HarnessesGVR: "HarnessList",
	}, objects...)}
}

func agentObject(harness string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "Agent",
		"metadata": map[string]any{
			"namespace": "org-otter", "name": "otter-coder",
			"annotations": map[string]any{
				refusals.CreatedByAnnotation:      "badger@example.com",
				refusals.DisplayNameAnnotation:    "Otter Coder",
				refusals.NotifyRefusalsAnnotation: "true",
			},
		},
		"spec": map[string]any{"harnessRef": map[string]any{"name": harness}},
	}}
}

func harnessObject(name string, spec map[string]any, annotations map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "Harness",
		"metadata": map[string]any{"namespace": "org-otter", "name": name, "annotations": annotations},
		"spec":     spec,
	}}
}

func TestKubeAgents_ReadsTheRuntimeOfTheAgentsHarness(t *testing.T) {
	substrate := map[string]any{"profile": "default"}
	for _, tc := range []struct {
		name         string
		spec         map[string]any
		annotations  map[string]any
		runtime      string
		harnessTitle string
	}{
		{"claude", map[string]any{"claude": map[string]any{}, "substrate": substrate, "workload": map[string]any{"image": "x"}}, nil, "claude", ""},
		{"kagent", map[string]any{"kagent": map[string]any{}, "substrate": substrate, "env": []any{}}, nil, "kagent", ""},
		{"display name", map[string]any{"codex": map[string]any{}, "substrate": substrate}, map[string]any{refusals.DisplayNameAnnotation: "Codex with Go"}, "codex", "Codex with Go"},
		{"no runtime", map[string]any{"substrate": substrate}, nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, err := kubeAgents(agentObject("coding"), harnessObject("coding", tc.spec, tc.annotations)).Agent(context.Background(), "org-otter", "otter-coder")
			require.NoError(t, err)
			require.Equal(t, refusals.Agent{
				DisplayName: "Otter Coder", Creator: "badger@example.com",
				NotifyRefusals: true,
				Runtime:        tc.runtime, HarnessTitle: tc.harnessTitle,
			}, agent)
		})
	}
}

func TestKubeAgents_NoCreatorReadsNoHarness(t *testing.T) {
	obj := agentObject("coding")
	obj.SetAnnotations(nil)
	agents := kubeAgents(obj)
	agents.Client.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "harnesses", func(k8stesting.Action) (bool, runtime.Object, error) {
		t.Fatal("an Agent nobody is told about needs no Harness")
		return true, nil, nil
	})
	agent, err := agents.Agent(context.Background(), "org-otter", "otter-coder")
	require.NoError(t, err)
	require.Empty(t, agent.Creator)
}

func TestKubeAgents_NotificationOptIn(t *testing.T) {
	for _, value := range []string{"", "false", "True", "true"} {
		t.Run(value, func(t *testing.T) {
			object := agentObject("")
			annotations := object.GetAnnotations()
			annotations[refusals.NotifyRefusalsAnnotation] = value
			object.SetAnnotations(annotations)
			agent, err := kubeAgents(object).Agent(t.Context(), "org-otter", "otter-coder")
			require.NoError(t, err)
			require.Equal(t, value == "true", agent.NotifyRefusals)
		})
	}
}

func TestKubeAgents_MissingHarnessLeavesTheRuntimeUnknown(t *testing.T) {
	agent, err := kubeAgents(agentObject("gone")).Agent(context.Background(), "org-otter", "otter-coder")
	require.NoError(t, err)
	require.Equal(t, "badger@example.com", agent.Creator)
	require.Empty(t, agent.Runtime)
}

func TestKubeAgents_ForbiddenHarnessLeavesTheRuntimeUnknown(t *testing.T) {
	agents := kubeAgents(agentObject("coding"))
	agents.Client.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "harnesses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(refusals.HarnessesGVR.GroupResource(), "coding", errors.New("no RBAC"))
	})
	agent, err := agents.Agent(context.Background(), "org-otter", "otter-coder")
	require.NoError(t, err, "a ClusterRole without harnesses still notes the refusal")
	require.Empty(t, agent.Runtime)
}

func TestKubeAgents_HarnessReadFailureIsRetried(t *testing.T) {
	agents := kubeAgents(agentObject("coding"))
	agents.Client.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "harnesses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver restarting")
	})
	_, err := agents.Agent(context.Background(), "org-otter", "otter-coder")
	require.Error(t, err)
}
