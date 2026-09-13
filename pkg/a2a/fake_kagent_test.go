package a2a_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// fakeKagent is an in-process kagent API v2 controller: the A2A v1 service
// and the AgentTemplate, AgentInstance and Model services, served over gRPC
// on a bufconn. It records the metadata of every call so tests can assert the
// wire contract, and implements the controller's idempotent create, its
// one-active-task refusal and its error shapes.
type fakeKagent struct {
	a2apb.UnimplementedA2AServiceServer
	apiv1alpha1.UnimplementedAgentTemplateServiceServer
	apiv1alpha1.UnimplementedAgentInstanceServiceServer
	apiv1alpha1.UnimplementedModelServiceServer

	mu sync.Mutex

	templates    []*apiv1alpha1.AgentTemplate
	modelConfigs map[string]map[string]any // name -> spec
	instances    map[string]*apiv1alpha1.AgentInstance
	byRequest    map[string]string // creator|request_id -> instance id
	tasks        map[string]*a2apkg.Task
	events       []a2apkg.Event // played back by SendStreamingMessage
	busy         bool           // refuse a new task: one is active
	createState  apiv1alpha1.AgentInstanceState

	calls    map[string][]metadata.MD // method -> incoming metadata per call
	sent     []*a2apkg.Message
	created  []*apiv1alpha1.CreateAgentInstanceRequest
	canceled []string
	deleted  []string
}

func newFakeKagent() *fakeKagent {
	return &fakeKagent{
		modelConfigs: map[string]map[string]any{},
		instances:    map[string]*apiv1alpha1.AgentInstance{},
		byRequest:    map[string]string{},
		tasks:        map[string]*a2apkg.Task{},
		calls:        map[string][]metadata.MD{},
		createState:  apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
	}
}

// serve starts the fake on a bufconn and returns a client dialled to it.
func (f *fakeKagent) serve(t *testing.T, cfg pkga2a.Config) *pkga2a.Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	a2apb.RegisterA2AServiceServer(srv, f)
	apiv1alpha1.RegisterAgentTemplateServiceServer(srv, f)
	apiv1alpha1.RegisterAgentInstanceServiceServer(srv, f)
	apiv1alpha1.RegisterModelServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	if cfg.Target == "" {
		cfg.Target = "grpc://bufnet:8080"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "kagent"
	}
	client, err := pkga2a.NewClient(conn, cfg)
	require.NoError(t, err)
	return client
}

func (f *fakeKagent) record(ctx context.Context, method string) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method] = append(f.calls[method], md)
}

// lastMD returns the metadata of the last call of method.
func (f *fakeKagent) lastMD(method string) metadata.MD {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls[method]
	if len(calls) == 0 {
		return nil
	}
	return calls[len(calls)-1]
}

func (f *fakeKagent) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls[method])
}

// creator mirrors the controller: the caller is whoever the bearer says.
func creator(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	auth := md.Get("authorization")
	if len(auth) != 1 || auth[0] == "" {
		return "", status.Error(codes.Unauthenticated, "invalid credentials")
	}
	return auth[0], nil
}

// routedInstance mirrors the A2A gateway's route(): exactly one instance id.
func (f *fakeKagent) routedInstance(ctx context.Context) (*apiv1alpha1.AgentInstance, error) {
	ids := metadata.ValueFromIncomingContext(ctx, pkga2a.InstanceIDHeader)
	if len(ids) != 1 {
		return nil, status.Errorf(codes.InvalidArgument, "exactly one %s header is required", pkga2a.InstanceIDHeader)
	}
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[ids[0]]
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "not authorized")
	}
	return inst, nil
}

// --- lf.a2a.v1.A2AService ---

func (f *fakeKagent) SendStreamingMessage(req *a2apb.SendMessageRequest, stream grpc.ServerStreamingServer[a2apb.StreamResponse]) error {
	ctx := stream.Context()
	f.record(ctx, "SendStreamingMessage")
	inst, err := f.routedInstance(ctx)
	if err != nil {
		return err
	}
	msg, err := pbconv.FromProtoMessage(req.GetMessage())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if msg.ContextID != "" && msg.ContextID != inst.GetContextId() {
		return status.Error(codes.InvalidArgument, "message context does not match AgentInstance")
	}
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	busy, events := f.busy, f.events
	f.mu.Unlock()
	if busy && msg.TaskID == "" {
		return unsupportedOperation(fmt.Sprintf("AgentInstance %s already has an active task: conflict", inst.GetId()))
	}
	for _, ev := range events {
		pb, err := pbconv.ToProtoStreamResponse(ev)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if err := stream.Send(pb); err != nil {
			return err
		}
	}
	return nil
}

// unsupportedOperation mirrors the a2a-go gRPC error shape of
// a2a.ErrUnsupportedOperation: FailedPrecondition with the protocol's
// ErrorInfo reason.
func unsupportedOperation(msg string) error {
	st, err := status.New(codes.FailedPrecondition, msg).WithDetails(&errdetails.ErrorInfo{Reason: "UNSUPPORTED_OPERATION", Domain: a2apkg.ProtocolDomain})
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return st.Err()
}

func (f *fakeKagent) GetTask(ctx context.Context, req *a2apb.GetTaskRequest) (*a2apb.Task, error) {
	f.record(ctx, "GetTask")
	if _, err := f.routedInstance(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	task, ok := f.tasks[req.GetId()]
	f.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return pbconv.ToProtoTask(task)
}

func (f *fakeKagent) CancelTask(ctx context.Context, req *a2apb.CancelTaskRequest) (*a2apb.Task, error) {
	f.record(ctx, "CancelTask")
	if _, err := f.routedInstance(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	f.canceled = append(f.canceled, req.GetId())
	canceled := *task
	canceled.Status = a2apkg.TaskStatus{State: a2apkg.TaskStateCanceled}
	f.tasks[req.GetId()] = &canceled
	return pbconv.ToProtoTask(&canceled)
}

// --- kagent.api.v1alpha1.AgentTemplateService ---

func (f *fakeKagent) ListAgentTemplates(ctx context.Context, req *apiv1alpha1.ListAgentTemplatesRequest) (*apiv1alpha1.ListAgentTemplatesResponse, error) {
	f.record(ctx, "ListAgentTemplates")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*apiv1alpha1.AgentTemplate
	for _, t := range f.templates {
		if t.GetRef().GetNamespace() == req.GetNamespace() {
			out = append(out, t)
		}
	}
	return &apiv1alpha1.ListAgentTemplatesResponse{AgentTemplates: out}, nil
}

// --- kagent.api.v1alpha1.ModelService ---

func (f *fakeKagent) GetModelConfig(ctx context.Context, req *apiv1alpha1.GetModelConfigRequest) (*apiv1alpha1.GetModelConfigResponse, error) {
	f.record(ctx, "GetModelConfig")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	spec, ok := f.modelConfigs[req.GetRef().GetName()]
	f.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "ModelConfig not found")
	}
	value, err := structpb.NewStruct(map[string]any{"spec": spec})
	if err != nil {
		return nil, err
	}
	return &apiv1alpha1.GetModelConfigResponse{ModelConfig: &apiv1alpha1.ModelConfig{
		Ref:      req.GetRef(),
		Resource: &apiv1alpha1.StructuredObject{ApiVersion: "kagent.dev/v1alpha3", Kind: "ModelConfig", Value: value},
	}}, nil
}

// --- kagent.api.v1alpha1.AgentInstanceService ---

func (f *fakeKagent) CreateAgentInstance(ctx context.Context, req *apiv1alpha1.CreateAgentInstanceRequest) (*apiv1alpha1.CreateAgentInstanceResponse, error) {
	f.record(ctx, "CreateAgentInstance")
	who, err := creator(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.GetRequestId()) == 0 || len(req.GetRequestId()) > 128 {
		return nil, status.Error(codes.InvalidArgument, "request_id must be 1-128 characters")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	key := who + "|" + req.GetRequestId()
	if id, ok := f.byRequest[key]; ok {
		existing := f.instances[id]
		if existing.GetHarness().GetName() != req.GetHarness().GetName() || existing.GetAgentTemplate().GetName() != req.GetAgentTemplate().GetName() {
			return nil, status.Error(codes.AlreadyExists, "request_id was already used for a different AgentInstance")
		}
		return &apiv1alpha1.CreateAgentInstanceResponse{AgentInstance: existing}, nil
	}
	if req.GetHarness().GetName() == "unready" {
		return nil, status.Error(codes.FailedPrecondition, "AgentTemplate and Harness do not have a ready prepared revision")
	}
	inst := &apiv1alpha1.AgentInstance{
		Id:            fmt.Sprintf("0192f1c2-7d1e-7a3b-9c4d-%012d", len(f.instances)+1),
		Creator:       who,
		Harness:       req.GetHarness(),
		AgentTemplate: req.GetAgentTemplate(),
		State:         f.createState,
		ContextId:     fmt.Sprintf("ctx-%d", len(f.instances)+1),
	}
	f.instances[inst.Id] = inst
	f.byRequest[key] = inst.Id
	return &apiv1alpha1.CreateAgentInstanceResponse{AgentInstance: inst}, nil
}

func (f *fakeKagent) GetAgentInstance(ctx context.Context, req *apiv1alpha1.GetAgentInstanceRequest) (*apiv1alpha1.GetAgentInstanceResponse, error) {
	f.record(ctx, "GetAgentInstance")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[req.GetAgentInstanceId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "AgentInstance not found")
	}
	// A CREATING instance converges on the next look.
	if inst.GetState() == apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING {
		inst.State = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY
	}
	return &apiv1alpha1.GetAgentInstanceResponse{AgentInstance: inst}, nil
}

func (f *fakeKagent) DeleteAgentInstance(ctx context.Context, req *apiv1alpha1.DeleteAgentInstanceRequest) (*apiv1alpha1.DeleteAgentInstanceResponse, error) {
	f.record(ctx, "DeleteAgentInstance")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[req.GetAgentInstanceId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "AgentInstance not found")
	}
	f.deleted = append(f.deleted, req.GetAgentInstanceId())
	delete(f.instances, req.GetAgentInstanceId())
	return &apiv1alpha1.DeleteAgentInstanceResponse{AgentInstance: inst}, nil
}

// template builds an AgentTemplate the way the controller serves it: the
// whole CR as a StructuredObject plus the denormalised fields.
func template(t *testing.T, name string, annotations map[string]any, admitting []string, harnessStatus []any, modelConfig string) *apiv1alpha1.AgentTemplate {
	t.Helper()
	metadata := map[string]any{"name": name, "namespace": "kagent"}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	resource := map[string]any{
		"apiVersion": "kagent.dev/v1alpha3",
		"kind":       "AgentTemplate",
		"metadata":   metadata,
		"spec":       map[string]any{"description": "Investigates " + name, "modelConfig": map[string]any{"name": modelConfig}},
		"status":     map[string]any{"observedGeneration": 1, "harnesses": harnessStatus},
	}
	value, err := structpb.NewStruct(resource)
	require.NoError(t, err)
	var modelRef *apiv1alpha1.ResourceReference
	if modelConfig != "" {
		modelRef = &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: modelConfig}
	}
	return &apiv1alpha1.AgentTemplate{
		Ref:                &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: name},
		Resource:           &apiv1alpha1.StructuredObject{ApiVersion: "kagent.dev/v1alpha3", Kind: "AgentTemplate", Value: value},
		ModelConfigRef:     modelRef,
		Description:        "Investigates " + name,
		AdmittingHarnesses: admitting,
	}
}

func harnessStatus(harness string, ready bool, message string) map[string]any {
	st := "False"
	if ready {
		st = "True"
	}
	return map[string]any{
		"harness":         harness,
		"desiredRevision": "rev-1",
		"conditions":      []any{map[string]any{"type": "Ready", "status": st, "reason": "Compiled", "message": message}},
	}
}

// asUser is a context carrying a forwarded person token.
func asUser(ctx context.Context, token string) context.Context {
	return pkga2a.WithForwardedToken(ctx, token)
}
