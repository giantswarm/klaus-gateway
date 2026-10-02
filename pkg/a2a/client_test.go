package a2a_test

import (
	"net"
	"strings"
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

	// A TLS target without a port is the edge on 443, as an https URL would be
	// (agentlab's klaus-gateway proof passes the edge that way).
	host, tls, err = pkga2a.ParseTarget("grpcs://agentgateway.127.0.0.1.nip.io")
	require.NoError(t, err)
	require.Equal(t, "agentgateway.127.0.0.1.nip.io:443", host)
	require.True(t, tls)

	for _, bad := range []string{
		"http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent", // the 0.x REST shape
		"grpc://kagent.example.com",             // no port: plaintext gRPC has no conventional one
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

// streamErr runs one turn and returns the events before the error, and the
// error.
func streamErr(t *testing.T, client *pkga2a.Client, text string) ([]a2apkg.Event, error) {
	t.Helper()
	var events []a2apkg.Event
	for ev, err := range client.Stream(asUser(t.Context(), userToken), instanceID, a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart(text))) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// The controller refuses a request over its gRPC size limit on the stream's
// first read: the person's message was too large.
func TestClient_Stream_OversizeRequestIsErrPayloadTooLarge(t *testing.T) {
	f := readyFake(t)
	f.serverOpts = []grpc.ServerOption{grpc.MaxRecvMsgSize(1 << 10)}
	client := f.serve(t, pkga2a.Config{})

	_, err := streamErr(t, client, strings.Repeat("x", 4<<10))
	require.ErrorIs(t, err, pkga2a.ErrPayloadTooLarge)
}

// Only gRPC's size limit is about size: a quota or rate-limit refusal with
// the same code reaches the channel as the error it is.
func TestClient_ResourceExhaustedWithoutTheSizeLimitIsNotTooLarge(t *testing.T) {
	f := readyFake(t)
	f.tasks["task-1"] = &a2apkg.Task{ID: "task-1", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking}}
	f.refuse = status.Error(codes.ResourceExhausted, "quota exceeded for the caller")
	client := f.serve(t, pkga2a.Config{})

	_, err := streamErr(t, client, "hi")
	require.ErrorContains(t, err, "quota exceeded for the caller", "the refusal reaches the channel as it was")
	require.NotErrorIs(t, err, pkga2a.ErrPayloadTooLarge)

	// A subscribe sends no message of the person's, so not even gRPC's size
	// text makes it "too large".
	f.refuse = status.Error(codes.ResourceExhausted, "grpc: received message larger than max (5 vs. 4)")
	var subErr error
	for _, err := range client.Subscribe(asUser(t.Context(), userToken), instanceID, "task-1") {
		subErr = err
	}
	require.ErrorContains(t, subErr, "larger than max")
	require.NotErrorIs(t, subErr, pkga2a.ErrPayloadTooLarge)
}

// The controller can fail to send its own first event: a resumed task's
// snapshot carries its whole history. The person's message was accepted, so
// that is not "too large".
func TestClient_Stream_ControllerSendRefusalIsNotTooLarge(t *testing.T) {
	f := readyFake(t)
	info := a2apkg.TaskInfo{TaskID: "task-1", ContextID: "ctx-1"}
	history := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart(strings.Repeat("x", 4<<10)))
	f.events = []a2apkg.Event{
		&a2apkg.Task{ID: info.TaskID, ContextID: info.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking}, History: []*a2apkg.Message{history}},
	}
	f.serverOpts = []grpc.ServerOption{grpc.MaxSendMsgSize(1 << 10)}
	client := f.serve(t, pkga2a.Config{})

	_, err := streamErr(t, client, "approve")
	require.ErrorContains(t, err, "trying to send message larger than max")
	require.NotErrorIs(t, err, pkga2a.ErrPayloadTooLarge)
}

// An event over this client's receive limit fails in the middle of the turn.
// That is the agent's reply, not the person's message.
func TestClient_Stream_OversizeEventMidStreamIsNotTooLarge(t *testing.T) {
	f := readyFake(t)
	info := a2apkg.TaskInfo{TaskID: "task-1", ContextID: "ctx-1"}
	f.events = []a2apkg.Event{
		&a2apkg.Task{ID: info.TaskID, ContextID: info.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(info, a2apkg.NewTextPart(strings.Repeat("x", 4<<10))),
	}
	f.clientOpts = []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1 << 10))}
	client := f.serve(t, pkga2a.Config{})

	events, err := streamErr(t, client, "hi")
	require.Len(t, events, 1, "the submitted task arrived")
	require.ErrorContains(t, err, "larger than max", "gRPC refused the event on this side")
	require.NotErrorIs(t, err, pkga2a.ErrPayloadTooLarge)
}

// Dial receives an event up to the controller's own limit (16 MiB), past
// gRPC's default of 4 MiB, and no larger.
func TestDial_ReceivesEventsUpToTheControllerLimit(t *testing.T) {
	f := readyFake(t)
	info := a2apkg.TaskInfo{TaskID: "task-1", ContextID: "ctx-1"}
	f.events = []a2apkg.Event{
		&a2apkg.Task{ID: info.TaskID, ContextID: info.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(info, a2apkg.NewTextPart(strings.Repeat("x", 5<<20))),
		a2apkg.NewArtifactEvent(info, a2apkg.NewTextPart(strings.Repeat("x", 17<<20))),
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.serveOn(t, lis)
	client, err := pkga2a.Dial(pkga2a.Config{Target: "grpc://" + lis.Addr().String(), Namespace: "kagent"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	events, err := streamErr(t, client, "hi")
	require.Len(t, events, 2, "the 5 MiB event arrived")
	require.ErrorContains(t, err, "grpc: received message larger than max", "the 17 MiB event is over the limit")
	require.NotErrorIs(t, err, pkga2a.ErrPayloadTooLarge)
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
	// The reason is readable without parsing the message, so a channel can
	// render it instead of calling the agent unknown.
	var unavailable *pkga2a.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, "kagent/orphan", unavailable.Ref)
	require.Contains(t, unavailable.Reason, "no Harness admits this AgentTemplate")
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

// The controller dedupes a create by its request id; the client's part is to
// send it, with the template, the Harness that admits it and the name.
func TestClient_CreateInstance_SendsTheTemplateHarnessRequestIDAndName(t *testing.T) {
	f := readyFake(t)
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)
	requestID := "1d2c7a1e5e1a4b7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d"

	inst, err := client.CreateInstance(ctx, "sre-agent", requestID, "restart the kong pods on gazelle")
	require.NoError(t, err)
	require.True(t, inst.Ready())
	require.Len(t, f.created, 1)
	require.Equal(t, "kagent", f.created[0].GetHarness().GetName(), "the admitting Harness from the template's status")
	require.Equal(t, "sre-agent", f.created[0].GetAgentTemplate().GetName())
	require.Equal(t, requestID, f.created[0].GetRequestId())
	require.Equal(t, "restart the kong pods on gazelle", f.created[0].GetName(), "the conversation is named after the message that opened it")
}

func TestClient_CreateInstance_WaitsForReady(t *testing.T) {
	f := readyFake(t)
	f.createState = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING
	client := f.serve(t, pkga2a.Config{})

	inst, err := client.CreateInstance(asUser(t.Context(), userToken), "sre-agent", "req-1", "")
	require.NoError(t, err)
	require.True(t, inst.Ready())
	require.GreaterOrEqual(t, f.callCount("GetAgentInstance"), 1, "a CREATING instance is polled until READY")
}

// A name the controller will not take costs the name, never the conversation:
// the refused create reserved nothing, so the same request id goes again
// unnamed.
func TestClient_CreateInstance_FallsBackWhenTheNameIsRefused(t *testing.T) {
	f := readyFake(t)
	client := f.serve(t, pkga2a.Config{})
	tooLong := strings.Repeat("x", 201)

	inst, err := client.CreateInstance(asUser(t.Context(), userToken), "sre-agent", "req-1", tooLong)
	require.NoError(t, err)
	require.True(t, inst.Ready())
	require.Len(t, f.created, 2)
	require.Equal(t, tooLong, f.created[0].GetName())
	require.Equal(t, "req-1", f.created[1].GetRequestId(), "the retry reuses the request id, so no second conversation")
	require.Empty(t, f.created[1].GetName())
}

func TestClient_CreateInstance_RefusesAnUnavailableAgent(t *testing.T) {
	f := readyFake(t)
	f.templates = append(f.templates,
		template(t, "compiling", nil, []string{"kagent"}, []any{harnessStatus("kagent", false, "not yet")}, "m"),
		template(t, "racy", nil, []string{"unready"}, []any{harnessStatus("unready", true, "")}, "m"),
	)
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)

	_, err := client.CreateInstance(ctx, "compiling", "req-1", "")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
	require.Empty(t, f.created, "an unavailable template is refused before the controller is asked")

	_, err = client.CreateInstance(ctx, "nobody", "req-1", "")
	require.ErrorIs(t, err, pkga2a.ErrAgentUnknown)

	// The controller's own refusal (the revision went away between the roster
	// read and the create) maps the same way.
	_, err = client.CreateInstance(ctx, "racy", "req-2", "")
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

func TestClient_CreateAndRevokeShare(t *testing.T) {
	f := readyFake(t)
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)

	share, err := client.CreateShare(ctx, instanceID)
	require.NoError(t, err)
	require.NotEmpty(t, share.ID)
	require.NotEmpty(t, share.Token)
	require.Equal(t, []apiv1alpha1.AgentInstanceSharePermission{apiv1alpha1.AgentInstanceSharePermission_AGENT_INSTANCE_SHARE_PERMISSION_READ_WRITE}, f.sharedAs,
		"a collaborator sends and cancels turns, which a read-only share refuses")
	require.Equal(t, []string{"Bearer " + userToken}, f.lastMD("CreateAgentInstanceShare").Get("authorization"))

	_, err = client.CreateShare(asUser(t.Context(), "someone-else"), instanceID)
	require.ErrorIs(t, err, pkga2a.ErrInstanceNotFound, "only the instance's creator may share it")

	require.NoError(t, client.RevokeShare(ctx, share.ID))
	require.Equal(t, []string{share.ID}, f.revoked)
	require.NoError(t, client.RevokeShare(ctx, share.ID), "a share already gone is revoked either way")
}

// A share token in the context rides on every call as x-share-token, next to
// the caller's own bearer; without one no such entry is sent.
func TestClient_ShareTokenRidesEveryCall(t *testing.T) {
	f := readyFake(t)
	f.tasks["task-1"] = &a2apkg.Task{ID: "task-1", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking}}
	f.events = []a2apkg.Event{&a2apkg.Task{ID: "task-1", ContextID: "ctx-1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted}}}
	client := f.serve(t, pkga2a.Config{})

	plain := asUser(t.Context(), userToken)
	for _, err := range client.Stream(plain, instanceID, a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("hi"))) {
		require.NoError(t, err)
	}
	require.Empty(t, f.lastMD("SendStreamingMessage").Get(pkga2a.ShareTokenHeader))

	shared := pkga2a.WithShareToken(asUser(t.Context(), "collaborator-jwt"), "share-secret")
	for _, err := range client.Stream(shared, instanceID, a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("hi"))) {
		require.NoError(t, err)
	}
	_, err := client.GetTask(shared, instanceID, "task-1")
	require.NoError(t, err)
	_, err = client.CancelTask(shared, instanceID, "task-1")
	require.NoError(t, err)
	_, err = client.GetInstance(shared, instanceID)
	require.NoError(t, err)

	for _, method := range []string{"SendStreamingMessage", "GetTask", "CancelTask", "GetAgentInstance"} {
		md := f.lastMD(method)
		require.Equal(t, []string{"Bearer collaborator-jwt"}, md.Get("authorization"), method)
		require.Equal(t, []string{"share-secret"}, md.Get(pkga2a.ShareTokenHeader), method)
	}
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

// Subscribe resubscribes to a task on the instance: the controller serves a
// task that has quiesced whole, as the only event, and the call rides the same
// instance route and caller identity as every other A2A call.
func TestClient_Subscribe_ServesAQuiescentTaskWhole(t *testing.T) {
	f := readyFake(t)
	f.tasks["task-1"] = &a2apkg.Task{
		ID: "task-1", ContextID: "ctx-1",
		Status:    a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
		Artifacts: []*a2apkg.Artifact{{ID: "a1", Parts: a2apkg.ContentParts{a2apkg.NewTextPart("done")}}},
	}
	client := f.serve(t, pkga2a.Config{})
	ctx := asUser(t.Context(), userToken)

	var events []a2apkg.Event
	for event, err := range client.Subscribe(ctx, instanceID, "task-1") {
		require.NoError(t, err)
		events = append(events, event)
	}
	require.Len(t, events, 1)
	task, ok := events[0].(*a2apkg.Task)
	require.True(t, ok, "a quiescent task arrives whole")
	require.Equal(t, a2apkg.TaskStateCompleted, task.Status.State)
	require.Len(t, task.Artifacts, 1)
	require.Equal(t, "done", task.Artifacts[0].Parts[0].Text(), "the artifacts survive the proto round trip")
	require.Equal(t, []string{instanceID}, f.lastMD("SubscribeToTask").Get(pkga2a.InstanceIDHeader))
	require.Equal(t, []string{"Bearer " + userToken}, f.lastMD("SubscribeToTask").Get("authorization"))

	for _, err := range client.Subscribe(ctx, instanceID, "no-such-task") {
		require.ErrorIs(t, err, a2apkg.ErrTaskNotFound)
	}
}
