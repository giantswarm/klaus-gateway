package a2a_test

import (
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

const (
	instanceID = "0192f1c2-7d1e-7a3b-9c4d-000000000001"
	userToken  = "user-jwt"
)

// readyFake is a fake with one ready template ("sre-agent") and one instance
// of it, the shape most transport tests start from.
func readyFake(t *testing.T) *fakeKagent {
	t.Helper()
	f := newFakeKagent()
	f.templates = []*apiv1alpha1.AgentTemplate{
		template(t, "sre-agent", map[string]any{
			pkga2a.DisplayNameAnnotation: "SRE Agent",
			pkga2a.IconURLAnnotation:     "https://icons.example/sre.png",
		}, []string{"kagent"}, []any{harnessStatus("kagent", true, "")}, "default-model-config"),
	}
	f.instances[instanceID] = &apiv1alpha1.AgentInstance{
		Id: instanceID, Creator: "Bearer " + userToken, ContextId: "ctx-1",
		Harness:       &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "kagent"},
		AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "sre-agent"},
		State:         apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
	}
	return f
}

func TestParseTarget(t *testing.T) {
	host, tls, err := pkga2a.ParseTarget("grpc://agentgateway.agent-platform.svc.cluster.local:8080")
	require.NoError(t, err)
	require.Equal(t, "agentgateway.agent-platform.svc.cluster.local:8080", host)
	require.False(t, tls)

	host, tls, err = pkga2a.ParseTarget("grpcs://kagent.example.com:443")
	require.NoError(t, err)
	require.Equal(t, "kagent.example.com:443", host)
	require.True(t, tls)

	for _, bad := range []string{
		"http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent", // the 0.x REST shape
		"grpc://kagent.example.com",             // no port
		"grpc://kagent.example.com:8080/kagent", // a path
		"kagent.example.com:8080",               // no scheme
	} {
		_, _, err := pkga2a.ParseTarget(bad)
		require.Error(t, err, bad)
	}
}

// Every A2A call rides the caller's bearer, exactly one instance route, and
// the HITL extension request as gRPC metadata; the message keeps no context id
// of its own and the events come back as the SDK types the channels map.
func TestClient_Stream_WireContractAndEvents(t *testing.T) {
	f := readyFake(t)
	info := a2apkg.TaskInfo{TaskID: "task-1", ContextID: "ctx-1"}
	f.events = []a2apkg.Event{
		&a2apkg.Task{ID: info.TaskID, ContextID: info.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(info, a2apkg.NewTextPart("pong")),
		a2apkg.NewStatusUpdateEvent(info, a2apkg.TaskStateCompleted, nil),
	}
	client := f.serve(t, pkga2a.Config{})

	msg := a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("ping"))
	var events []a2apkg.Event
	for ev, err := range client.Stream(asUser(t.Context(), userToken), instanceID, msg) {
		require.NoError(t, err)
		events = append(events, ev)
	}
	require.Len(t, events, 3)
	_, isTask := events[0].(*a2apkg.Task)
	require.True(t, isTask, "the stored submitted task opens the stream")
	artifact, isArtifact := events[1].(*a2apkg.TaskArtifactUpdateEvent)
	require.True(t, isArtifact)
	require.Equal(t, "pong", artifact.Artifact.Parts[0].Text())
	final, isStatus := events[2].(*a2apkg.TaskStatusUpdateEvent)
	require.True(t, isStatus)
	require.Equal(t, a2apkg.TaskStateCompleted, final.Status.State)

	md := f.lastMD("SendStreamingMessage")
	require.Equal(t, []string{"Bearer " + userToken}, md.Get("authorization"))
	require.Equal(t, []string{instanceID}, md.Get(pkga2a.InstanceIDHeader), "exactly one instance route")
	require.Contains(t, md.Get("a2a-extensions"), pkga2a.HITLExtensionURI, "the HITL extension is requested on every turn")
	require.Equal(t, []string{"1.0"}, md.Get("a2a-version"))

	require.Len(t, f.sent, 1)
	require.Empty(t, f.sent[0].ContextID, "the controller owns the context id")
	require.Equal(t, "ping", f.sent[0].Parts[0].Text())
}

func TestClient_Stream_RequiresTheCallerIdentity(t *testing.T) {
	client := readyFake(t).serve(t, pkga2a.Config{})
	for _, err := range client.Stream(t.Context(), instanceID, a2apkg.NewMessage(a2apkg.MessageRoleUser)) {
		require.ErrorIs(t, err, pkga2a.ErrNoIdentity)
	}
	_, err := client.GetTask(t.Context(), instanceID, "task-1")
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity)
	_, err = client.ListAgents(t.Context())
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity, "no cache and no token: nothing to serve")
}

func TestClient_Stream_OneActiveTaskRefusalIsErrInstanceBusy(t *testing.T) {
	f := readyFake(t)
	f.busy = true
	client := f.serve(t, pkga2a.Config{})

	var got error
	for _, err := range client.Stream(asUser(t.Context(), userToken), instanceID, a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("again"))) {
		got = err
	}
	require.ErrorIs(t, got, pkga2a.ErrInstanceBusy)
}

func TestClient_GetAndCancelTask(t *testing.T) {
	f := readyFake(t)
	prompt := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("Delete the pod?"))
	require.NoError(t, pkga2a.AttachHITL(prompt, pkga2a.ToolApprovalRequest{Type: pkga2a.HITLTypeToolApprovalRequest, Tools: []pkga2a.HITLTool{{ID: "approval-1", Name: "kubectl_delete"}}}))
	f.tasks["task-1"] = &a2apkg.Task{ID: "task-1", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateInputRequired, Message: prompt}}
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)

	task, err := client.GetTask(ctx, instanceID, "task-1")
	require.NoError(t, err)
	require.Equal(t, a2apkg.TaskStateInputRequired, task.Status.State)
	request, err := pkga2a.ParseHITLRequest(task.Status.Message)
	require.NoError(t, err)
	require.NotNil(t, request.ToolApproval, "the HITL payload survives the proto round trip")
	require.Equal(t, "approval-1", request.ToolApproval.Tools[0].ID)
	require.Equal(t, []string{instanceID}, f.lastMD("GetTask").Get(pkga2a.InstanceIDHeader))

	canceled, err := client.CancelTask(ctx, instanceID, "task-1")
	require.NoError(t, err)
	require.Equal(t, a2apkg.TaskStateCanceled, canceled.Status.State)
	require.Equal(t, []string{"task-1"}, f.canceled)
	require.Equal(t, []string{"Bearer " + userToken}, f.lastMD("CancelTask").Get("authorization"))

	_, err = client.GetTask(ctx, instanceID, "no-such-task")
	require.ErrorIs(t, err, a2apkg.ErrTaskNotFound)
}

func TestClient_ListAgents_ReadinessAndAnnotations(t *testing.T) {
	f := newFakeKagent()
	f.templates = []*apiv1alpha1.AgentTemplate{
		template(t, "sre-agent", map[string]any{pkga2a.DisplayNameAnnotation: "SRE Agent", pkga2a.IconURLAnnotation: "https://icons.example/sre.png"},
			[]string{"kagent"}, []any{harnessStatus("kagent", true, "")}, "default-model-config"),
		template(t, "compiling", nil, []string{"kagent"}, []any{harnessStatus("kagent", false, "golden snapshot not taken yet")}, "default-model-config"),
		template(t, "orphan", nil, nil, nil, ""),
		template(t, "no-status-yet", nil, []string{"kagent"}, nil, "default-model-config"),
	}
	client := f.serve(t, pkga2a.Config{FallbackIconURLTemplate: "https://avatars.example/{agent}.png"})
	ctx := asUser(t.Context(), userToken)

	agents, err := client.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, agents, 1, "only a template a Harness has compiled a ready revision for is offered")
	require.Equal(t, pkga2a.AgentInfo{
		Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent", IconURL: "https://icons.example/sre.png",
		Description: "Investigates sre-agent", Harness: "kagent", ModelConfig: "default-model-config",
	}, agents[0])
	require.Equal(t, "kagent/sre-agent", agents[0].Ref())
	require.Equal(t, []string{"Bearer " + userToken}, f.lastMD("ListAgentTemplates").Get("authorization"))

	// Selection by name is refused with the reason for anything not offered.
	_, _, err = client.CardInfo(ctx, "compiling")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
	require.ErrorContains(t, err, "Harness kagent has not compiled a ready revision: golden snapshot not taken yet")
	_, _, err = client.CardInfo(ctx, "kagent/orphan")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
	require.ErrorContains(t, err, "no Harness admits")
	_, _, err = client.CardInfo(ctx, "no-status-yet")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
	require.ErrorContains(t, err, "no status reported yet")
	_, _, err = client.CardInfo(ctx, "nobody")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnknown)
	_, _, err = client.CardInfo(ctx, "other-namespace/sre-agent")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnknown, "only the served namespace is offered")

	name, description, err := client.CardInfo(ctx, "sre-agent")
	require.NoError(t, err)
	require.Equal(t, "SRE Agent", name)
	require.Equal(t, "Investigates sre-agent", description)

	// Branding: the annotation's icon, or the fallback template for a template
	// without one — and the fallback even for an agent nobody knows.
	username, icon := client.CardIdentity(ctx, "kagent/sre-agent")
	require.Equal(t, "SRE Agent", username)
	require.Equal(t, "https://icons.example/sre.png", icon)
	_, icon = client.CardIdentity(ctx, "compiling")
	require.Equal(t, "https://avatars.example/compiling.png", icon)
	_, icon = client.CardIdentity(ctx, "kagent/nobody")
	require.Equal(t, "https://avatars.example/nobody.png", icon)
}

// The roster is fetched as the caller and cached; a call without a token —
// branding a reply, recovering a thread's agent — is served from the cache.
func TestClient_ListAgents_CacheServesCallsWithoutIdentity(t *testing.T) {
	f := readyFake(t)
	client := f.serve(t, pkga2a.Config{})

	_, err := client.ListAgents(t.Context())
	require.ErrorIs(t, err, pkga2a.ErrNoIdentity)

	agents, err := client.ListAgents(asUser(t.Context(), userToken))
	require.NoError(t, err)
	require.Len(t, agents, 1)

	agents, err = client.ListAgents(t.Context())
	require.NoError(t, err, "the cached roster serves an identity-less call")
	require.Len(t, agents, 1)
	username, _ := client.CardIdentity(t.Context(), "sre-agent")
	require.Equal(t, "SRE Agent", username)
	require.Equal(t, 1, f.callCount("ListAgentTemplates"), "a fresh cache is not re-fetched")
}

func TestClient_AgentModel(t *testing.T) {
	f := readyFake(t)
	f.modelConfigs["default-model-config"] = map[string]any{"model": "claude-sonnet-4-6", "provider": "Anthropic"}
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)

	model, provider, err := client.AgentModel(ctx, "kagent/sre-agent")
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-6", model)
	require.Equal(t, "Anthropic", provider)
	require.Equal(t, []string{"Bearer " + userToken}, f.lastMD("GetModelConfig").Get("authorization"))

	_, _, err = client.AgentModel(ctx, "nobody")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnknown)
}

func TestClient_CreateInstance_IdempotentPerRequestID(t *testing.T) {
	f := readyFake(t)
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)
	requestID := "1d2c7a1e5e1a4b7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d"

	first, err := client.CreateInstance(ctx, "sre-agent", requestID)
	require.NoError(t, err)
	require.True(t, first.Ready())
	again, err := client.CreateInstance(ctx, "sre-agent", requestID)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID, "a retried create returns the same instance")
	require.Len(t, f.created, 2)
	require.Equal(t, "kagent", f.created[0].GetHarness().GetName(), "the admitting Harness from the template's status")
	require.Equal(t, "sre-agent", f.created[0].GetAgentTemplate().GetName())
	require.Equal(t, requestID, f.created[0].GetRequestId())

	// A different creator with the same request id is a different conversation.
	other, err := client.CreateInstance(asUser(t.Context(), "someone-else"), "sre-agent", requestID)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, other.ID)
}

func TestClient_CreateInstance_WaitsForReady(t *testing.T) {
	f := readyFake(t)
	f.createState = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING
	client := f.serve(t, pkga2a.Config{})

	inst, err := client.CreateInstance(asUser(t.Context(), userToken), "sre-agent", "req-1")
	require.NoError(t, err)
	require.True(t, inst.Ready())
	require.GreaterOrEqual(t, f.callCount("GetAgentInstance"), 1, "a CREATING instance is polled until READY")
}

func TestClient_CreateInstance_RefusesAnUnavailableAgent(t *testing.T) {
	f := readyFake(t)
	f.templates = append(f.templates,
		template(t, "compiling", nil, []string{"kagent"}, []any{harnessStatus("kagent", false, "not yet")}, "m"),
		template(t, "racy", nil, []string{"unready"}, []any{harnessStatus("unready", true, "")}, "m"),
	)
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)

	_, err := client.CreateInstance(ctx, "compiling", "req-1")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
	require.Empty(t, f.created, "an unavailable template is refused before the controller is asked")

	_, err = client.CreateInstance(ctx, "nobody", "req-1")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnknown)

	// The controller's own refusal (the revision went away between the roster
	// read and the create) maps the same way.
	_, err = client.CreateInstance(ctx, "racy", "req-2")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
}

func TestClient_GetAndDeleteInstance(t *testing.T) {
	f := readyFake(t)
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)

	inst, err := client.GetInstance(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, instanceID, inst.ID)
	require.True(t, inst.Ready())

	require.NoError(t, client.DeleteInstance(ctx, instanceID))
	require.Equal(t, []string{instanceID}, f.deleted)
	_, err = client.GetInstance(ctx, instanceID)
	require.ErrorIs(t, err, pkga2a.ErrInstanceNotFound)
	require.True(t, pkga2a.IsNotFound(err))
	require.NoError(t, client.DeleteInstance(ctx, instanceID), "a missing instance is gone either way")
}

func TestAttachAndParseHITL_RoundTrip(t *testing.T) {
	msg := a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("approve"))
	require.NoError(t, pkga2a.AttachHITL(msg, pkga2a.ToolApprovalResponse{
		Type:      pkga2a.HITLTypeToolApprovalResponse,
		Approvals: []pkga2a.ToolApproval{{ID: "approval-1", Approved: true}},
	}))
	require.Equal(t, []string{pkga2a.HITLExtensionURI}, msg.Extensions)
	payload, ok := msg.Metadata[pkga2a.HITLExtensionURI].(map[string]any)
	require.True(t, ok)
	require.Equal(t, pkga2a.HITLTypeToolApprovalResponse, payload["type"])

	// A response is not a request.
	request, err := pkga2a.ParseHITLRequest(msg)
	require.NoError(t, err)
	require.Nil(t, request)

	ask := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("Which one?"))
	require.NoError(t, pkga2a.AttachHITL(ask, pkga2a.AskUserRequest{Type: pkga2a.HITLTypeAskUserRequest, ID: "q-1", Questions: []pkga2a.HITLQuestion{{Question: "Which one?", Choices: []string{"a", "b"}}}}))
	request, err = pkga2a.ParseHITLRequest(ask)
	require.NoError(t, err)
	require.NotNil(t, request.AskUser)
	require.Equal(t, "q-1", request.AskUser.ResponseID())

	nested := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("child asks"))
	require.NoError(t, pkga2a.AttachHITL(nested, pkga2a.AskUserRequest{Type: pkga2a.HITLTypeAskUserRequest, ID: "parent-1",
		Nested: &pkga2a.NestedHITLRequest{SubagentName: "child", TaskID: "ct", Tools: []pkga2a.HITLTool{{ID: "child-q-1", Name: "ask_user"}}}}))
	request, err = pkga2a.ParseHITLRequest(nested)
	require.NoError(t, err)
	require.Equal(t, "child-q-1", request.AskUser.ResponseID(), "a propagated question is answered under the child's id")
}
