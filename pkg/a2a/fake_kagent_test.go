package a2a_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// fakeKagent is an in-process kagent API v2 controller: the A2A v1 service
// and the Agent, AgentTemplate, Harness, Session and Model services, served over gRPC
// on a bufconn. It records the metadata of every call so tests can assert the
// wire contract, and implements the controller's idempotent create, its
// tenant and context-id routing, its one-active-task refusal and its error
// shapes.
type fakeKagent struct {
	a2apb.UnimplementedA2AServiceServer
	apiv1alpha1.UnimplementedAgentServiceServer
	apiv1alpha1.UnimplementedAgentTemplateServiceServer
	apiv1alpha1.UnimplementedHarnessServiceServer
	apiv1alpha1.UnimplementedSessionServiceServer
	apiv1alpha1.UnimplementedModelServiceServer

	mu sync.Mutex

	agents       []*apiv1alpha1.Agent
	templates    []*apiv1alpha1.AgentTemplate
	harnesses    []*apiv1alpha1.Harness
	harnessesErr error                     // returned by ListHarnesses
	modelConfigs map[string]map[string]any // name -> spec
	sessions     map[string]*apiv1alpha1.Session
	byRequest    map[string]string // creator|request_id -> session id
	tasks        map[string]*a2apkg.Task
	events       []a2apkg.Event // played back by SendStreamingMessage
	busy         bool           // refuse a new task: one is active
	refuse       error          // returned by SendStreamingMessage and SubscribeToTask
	createState  apiv1alpha1.RuntimeState

	serverOpts []grpc.ServerOption // the controller's gRPC options (size limits)
	clientOpts []grpc.DialOption   // the client connection's extra options

	calls     map[string][]metadata.MD // method -> incoming metadata per call
	sent      []*a2apkg.Message
	tenants   []string // the tenant of every A2A call, in order
	created   []*apiv1alpha1.CreateSessionRequest
	canceled  []string
	deleted   []string
	resumed   []string
	shares    map[string]string // share id -> session id
	tokens    map[string]string // share token -> session id
	sharedAs  []apiv1alpha1.SessionSharePermission
	sharedTTL []*durationpb.Duration
	revoked   []string
}

func newFakeKagent() *fakeKagent {
	return &fakeKagent{
		modelConfigs: map[string]map[string]any{},
		sessions:     map[string]*apiv1alpha1.Session{},
		byRequest:    map[string]string{},
		tasks:        map[string]*a2apkg.Task{},
		shares:       map[string]string{},
		tokens:       map[string]string{},
		calls:        map[string][]metadata.MD{},
		createState:  apiv1alpha1.RuntimeState_RUNTIME_STATE_READY,
	}
}

// serve starts the fake on a bufconn and returns a client dialled to it.
func (f *fakeKagent) serve(t *testing.T, cfg pkga2a.Config) *pkga2a.Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	f.serveOn(t, lis)

	// The stats handler is the one Dial installs: the wire the tests assert is
	// the wire an installation sees, trace context included.
	conn, err := grpc.NewClient("passthrough:///bufnet", append([]grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	}, f.clientOpts...)...)
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

// serveOn serves the fake's services on lis until the test ends.
func (f *fakeKagent) serveOn(t *testing.T, lis net.Listener) {
	t.Helper()
	srv := grpc.NewServer(f.serverOpts...)
	a2apb.RegisterA2AServiceServer(srv, f)
	apiv1alpha1.RegisterAgentServiceServer(srv, f)
	apiv1alpha1.RegisterAgentTemplateServiceServer(srv, f)
	apiv1alpha1.RegisterHarnessServiceServer(srv, f)
	apiv1alpha1.RegisterSessionServiceServer(srv, f)
	apiv1alpha1.RegisterModelServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
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

// reaches mirrors the controller's ownership check: a session is reachable
// by its creator, and by anyone presenting one of its share tokens. The
// caller holds f.mu.
func (f *fakeKagent) reaches(ctx context.Context, session *apiv1alpha1.Session) bool {
	who, err := creator(ctx)
	if err != nil {
		return false
	}
	if session.GetCreator() == who {
		return true
	}
	for _, token := range metadata.ValueFromIncomingContext(ctx, pkga2a.ShareTokenHeader) {
		if f.tokens[token] == session.GetId() {
			return true
		}
	}
	return false
}

// routedSession mirrors the A2A gateway's routing: the tenant names the
// Agent, the message's context id (or its task) the session, which must be
// the Agent's and READY.
func (f *fakeKagent) routedSession(ctx context.Context, tenant, contextID, taskID string) (*apiv1alpha1.Session, error) {
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = append(f.tenants, tenant)
	namespace, name, ok := strings.Cut(tenant, "/")
	if !ok || namespace == "" || name == "" {
		return nil, invalidRequest("Agent tenant must be namespace/name")
	}
	if contextID == "" && taskID != "" {
		task, ok := f.tasks[taskID]
		if !ok {
			return nil, status.Error(codes.NotFound, "task not found")
		}
		contextID = string(task.ContextID)
	}
	session, ok := f.sessions[contextID]
	if !ok || !f.reaches(ctx, session) {
		return nil, status.Error(codes.PermissionDenied, "not authorized")
	}
	if session.GetAgent().GetNamespace() != namespace || session.GetAgent().GetName() != name {
		return nil, status.Error(codes.PermissionDenied, "not authorized")
	}
	return session, nil
}

// --- lf.a2a.v1.A2AService ---

func (f *fakeKagent) SendStreamingMessage(req *a2apb.SendMessageRequest, stream grpc.ServerStreamingServer[a2apb.StreamResponse]) error {
	ctx := stream.Context()
	f.record(ctx, "SendStreamingMessage")
	msg, err := pbconv.FromProtoMessage(req.GetMessage())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	session, err := f.routedSession(ctx, req.GetTenant(), string(msg.ContextID), string(msg.TaskID))
	if err != nil {
		return err
	}
	if msg.ContextID != "" && msg.ContextID != session.GetContextId() {
		return invalidRequest("message context does not match task")
	}
	if session.GetState() != apiv1alpha1.RuntimeState_RUNTIME_STATE_READY {
		return unsupportedOperation("Session cannot accept work during a lifecycle operation")
	}
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	busy, events, refuse := f.busy, f.events, f.refuse
	f.mu.Unlock()
	if refuse != nil {
		return refuse
	}
	if busy && msg.TaskID == "" {
		return unsupportedOperation(fmt.Sprintf("session %s already has an active task: conflict", session.GetId()))
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
	return protocolError(codes.FailedPrecondition, "UNSUPPORTED_OPERATION", msg)
}

// invalidRequest mirrors the a2a-go gRPC error shape of
// a2a.ErrInvalidRequest.
func invalidRequest(msg string) error {
	return protocolError(codes.InvalidArgument, "INVALID_REQUEST", msg)
}

func protocolError(code codes.Code, reason, msg string) error {
	st, err := status.New(code, msg).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: a2apkg.ProtocolDomain})
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return st.Err()
}

func (f *fakeKagent) GetTask(ctx context.Context, req *a2apb.GetTaskRequest) (*a2apb.Task, error) {
	f.record(ctx, "GetTask")
	if _, err := f.routedSession(ctx, req.GetTenant(), "", req.GetId()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	task := f.tasks[req.GetId()]
	f.mu.Unlock()
	return pbconv.ToProtoTask(task)
}

// SubscribeToTask serves a stored quiescent task whole, as the controller does,
// and plays the configured events for one still running.
func (f *fakeKagent) SubscribeToTask(req *a2apb.SubscribeToTaskRequest, stream grpc.ServerStreamingServer[a2apb.StreamResponse]) error {
	ctx := stream.Context()
	f.record(ctx, "SubscribeToTask")
	if _, err := f.routedSession(ctx, req.GetTenant(), "", req.GetId()); err != nil {
		return err
	}
	f.mu.Lock()
	task, ok := f.tasks[req.GetId()]
	events, refuse := f.events, f.refuse
	f.mu.Unlock()
	if refuse != nil {
		return refuse
	}
	if !ok {
		return status.Error(codes.NotFound, "task not found")
	}
	if task.Status.State.Terminal() || task.Status.State == a2apkg.TaskStateInputRequired {
		resp, err := pbconv.ToProtoStreamResponse(task)
		if err != nil {
			return err
		}
		return stream.Send(resp)
	}
	for _, ev := range events {
		resp, err := pbconv.ToProtoStreamResponse(ev)
		if err != nil {
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeKagent) CancelTask(ctx context.Context, req *a2apb.CancelTaskRequest) (*a2apb.Task, error) {
	f.record(ctx, "CancelTask")
	if _, err := f.routedSession(ctx, req.GetTenant(), "", req.GetId()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	task := f.tasks[req.GetId()]
	f.canceled = append(f.canceled, req.GetId())
	canceled := *task
	canceled.Status = a2apkg.TaskStatus{State: a2apkg.TaskStateCanceled}
	f.tasks[req.GetId()] = &canceled
	return pbconv.ToProtoTask(&canceled)
}

// --- kagent.api.v1alpha1.AgentService ---

func (f *fakeKagent) ListAgents(ctx context.Context, req *apiv1alpha1.ListAgentsRequest) (*apiv1alpha1.ListAgentsResponse, error) {
	f.record(ctx, "ListAgents")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*apiv1alpha1.Agent
	for _, a := range f.agents {
		if a.GetRef().GetNamespace() == req.GetNamespace() {
			out = append(out, a)
		}
	}
	return &apiv1alpha1.ListAgentsResponse{Agents: out}, nil
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

// --- kagent.api.v1alpha1.HarnessService ---

func (f *fakeKagent) ListHarnesses(ctx context.Context, req *apiv1alpha1.ListHarnessesRequest) (*apiv1alpha1.ListHarnessesResponse, error) {
	f.record(ctx, "ListHarnesses")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	if f.harnessesErr != nil {
		return nil, f.harnessesErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*apiv1alpha1.Harness
	for _, h := range f.harnesses {
		if h.GetRef().GetNamespace() == req.GetNamespace() {
			out = append(out, h)
		}
	}
	return &apiv1alpha1.ListHarnessesResponse{Harnesses: out}, nil
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

// --- kagent.api.v1alpha1.SessionService ---

func (f *fakeKagent) CreateSession(ctx context.Context, req *apiv1alpha1.CreateSessionRequest) (*apiv1alpha1.CreateSessionResponse, error) {
	f.record(ctx, "CreateSession")
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
	// The controller's display-name contract, as protovalidate enforces it:
	// at most 200 characters, no control characters, no leading or trailing
	// whitespace. Checked after the attempt is recorded so a test can see both
	// the refused create and what the client sent instead.
	if name := req.GetName(); name != "" {
		if utf8.RuneCountInString(name) > 200 || strings.TrimSpace(name) != name || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, status.Error(codes.InvalidArgument, "name must be at most 200 characters and hold no control characters")
		}
	}
	key := who + "|" + req.GetRequestId()
	if id, ok := f.byRequest[key]; ok {
		existing := f.sessions[id]
		if existing.GetAgent().GetName() != req.GetAgent().GetName() {
			return nil, status.Error(codes.AlreadyExists, "request_id was already used for a different Session")
		}
		return &apiv1alpha1.CreateSessionResponse{Session: existing}, nil
	}
	if req.GetAgent().GetName() == "racy" {
		return nil, status.Error(codes.FailedPrecondition, "Agent has no successful revision")
	}
	id := fmt.Sprintf("0192f1c2-7d1e-7a3b-9c4d-%012d", len(f.sessions)+1)
	session := &apiv1alpha1.Session{
		Id:        id,
		Creator:   who,
		Agent:     req.GetAgent(),
		State:     f.createState,
		Operation: apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE,
		Name:      req.GetName(),
		ContextId: id,
	}
	f.sessions[id] = session
	f.byRequest[key] = id
	return &apiv1alpha1.CreateSessionResponse{Session: session}, nil
}

// ownedSession mirrors the controller's owner-scoped lookup: the creator, or
// a share holder, sees the session; anyone else gets NotFound. The caller
// holds f.mu.
func (f *fakeKagent) ownedSession(ctx context.Context, id string) (*apiv1alpha1.Session, error) {
	session, ok := f.sessions[id]
	if !ok || !f.reaches(ctx, session) {
		return nil, status.Error(codes.NotFound, "Session not found")
	}
	return session, nil
}

func (f *fakeKagent) GetSession(ctx context.Context, req *apiv1alpha1.GetSessionRequest) (*apiv1alpha1.GetSessionResponse, error) {
	f.record(ctx, "GetSession")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, err := f.ownedSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	// A CREATING session converges on the next look.
	if session.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_CREATING {
		session.State = apiv1alpha1.RuntimeState_RUNTIME_STATE_READY
	}
	return &apiv1alpha1.GetSessionResponse{Session: session}, nil
}

func (f *fakeKagent) ResumeSession(ctx context.Context, req *apiv1alpha1.ResumeSessionRequest) (*apiv1alpha1.ResumeSessionResponse, error) {
	f.record(ctx, "ResumeSession")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, err := f.ownedSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	f.resumed = append(f.resumed, session.GetId())
	session.State = apiv1alpha1.RuntimeState_RUNTIME_STATE_READY
	return &apiv1alpha1.ResumeSessionResponse{Session: session}, nil
}

func (f *fakeKagent) DeleteSession(ctx context.Context, req *apiv1alpha1.DeleteSessionRequest) (*apiv1alpha1.DeleteSessionResponse, error) {
	f.record(ctx, "DeleteSession")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, err := f.ownedSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	f.deleted = append(f.deleted, req.GetSessionId())
	delete(f.sessions, req.GetSessionId())
	return &apiv1alpha1.DeleteSessionResponse{Session: session}, nil
}

// CreateSessionShare mirrors the controller's owner-scoped insert: only the
// session's creator may share it, and anyone else gets NotFound.
func (f *fakeKagent) CreateSessionShare(ctx context.Context, req *apiv1alpha1.CreateSessionShareRequest) (*apiv1alpha1.CreateSessionShareResponse, error) {
	f.record(ctx, "CreateSessionShare")
	who, err := creator(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[req.GetSessionId()]
	if !ok || session.GetCreator() != who {
		return nil, status.Error(codes.NotFound, "Session not found")
	}
	id := fmt.Sprintf("share-%d", len(f.shares)+1)
	f.shares[id] = session.GetId()
	f.tokens["token-"+id] = session.GetId()
	f.sharedAs = append(f.sharedAs, req.GetPermission())
	f.sharedTTL = append(f.sharedTTL, req.GetTtl())
	share := &apiv1alpha1.SessionShare{Id: id, SessionId: session.GetId(), Permission: req.GetPermission()}
	if req.GetTtl() != nil {
		share.ExpiresAt = timestamppb.New(time.Now().Add(req.GetTtl().AsDuration()))
	}
	return &apiv1alpha1.CreateSessionShareResponse{Share: share, Token: "token-" + id}, nil
}

func (f *fakeKagent) RevokeSessionShare(ctx context.Context, req *apiv1alpha1.RevokeSessionShareRequest) (*apiv1alpha1.RevokeSessionShareResponse, error) {
	f.record(ctx, "RevokeSessionShare")
	if _, err := creator(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.shares[req.GetShareId()]; !ok {
		return nil, status.Error(codes.NotFound, "Session share not found")
	}
	delete(f.tokens, "token-"+req.GetShareId())
	delete(f.shares, req.GetShareId())
	f.revoked = append(f.revoked, req.GetShareId())
	return &apiv1alpha1.RevokeSessionShareResponse{}, nil
}

// agent builds an Agent the way the controller serves it: the whole CR as a
// StructuredObject. It references the AgentTemplate template and the Harness
// harness by name, and reports the Ready condition given.
func agent(t *testing.T, name string, annotations map[string]any, template, harness string, conditions []any) *apiv1alpha1.Agent {
	t.Helper()
	metadata := map[string]any{"name": name, "namespace": "kagent"}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	resource := map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3",
		"kind":       "Agent",
		"metadata":   metadata,
		"spec":       map[string]any{"templateRef": map[string]any{"name": template}, "harnessRef": map[string]any{"name": harness}},
		"status":     map[string]any{"observedGeneration": 1},
	}
	if conditions != nil {
		resource["status"].(map[string]any)["conditions"] = conditions
	}
	value, err := structpb.NewStruct(resource)
	require.NoError(t, err)
	return &apiv1alpha1.Agent{
		Ref:      &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: name},
		Resource: &apiv1alpha1.StructuredObject{ApiVersion: "api.kagent.dev/v1alpha3", Kind: "Agent", Value: value},
	}
}

// readyCondition is the Ready condition of an Agent's status as the
// controller writes it.
func readyCondition(ready bool, reason, message string) []any {
	st := "False"
	if ready {
		st = "True"
	}
	return []any{map[string]any{"type": "Ready", "status": st, "reason": reason, "message": message}}
}

// template builds an AgentTemplate the way the controller serves it: the
// whole CR as a StructuredObject plus the denormalised fields.
func template(t *testing.T, name string, annotations map[string]any, modelConfig string) *apiv1alpha1.AgentTemplate {
	t.Helper()
	metadata := map[string]any{"name": name, "namespace": "kagent"}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	resource := map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3",
		"kind":       "AgentTemplate",
		"metadata":   metadata,
		"spec":       map[string]any{"description": "Investigates " + name, "modelConfig": map[string]any{"name": modelConfig}},
		"status":     map[string]any{"observedGeneration": 1},
	}
	value, err := structpb.NewStruct(resource)
	require.NoError(t, err)
	var modelRef *apiv1alpha1.ResourceReference
	if modelConfig != "" {
		modelRef = &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: modelConfig}
	}
	return &apiv1alpha1.AgentTemplate{
		Ref:            &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: name},
		Resource:       &apiv1alpha1.StructuredObject{ApiVersion: "api.kagent.dev/v1alpha3", Kind: "AgentTemplate", Value: value},
		ModelConfigRef: modelRef,
		Description:    "Investigates " + name,
	}
}

// harness builds a Harness the way the controller lists it: its ref and the
// denormalised runtime the spec selects.
func harness(name, runtime string) *apiv1alpha1.Harness {
	return &apiv1alpha1.Harness{
		Ref:     &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: name},
		Runtime: runtime,
		Ready:   true,
	}
}

// asUser is a context carrying a forwarded person token, addressed to agent.
func asUser(ctx context.Context, token string) context.Context {
	return asUserOf(ctx, token, "sre-agent")
}

// asUserOf is a context carrying a forwarded person token, addressed to
// agentRef.
func asUserOf(ctx context.Context, token, agentRef string) context.Context {
	return pkga2a.WithAgentRef(pkga2a.WithForwardedToken(ctx, token), agentRef)
}

// structValue converts m to a protobuf struct value.
func structValue(t *testing.T, m map[string]any) *structpb.Value {
	t.Helper()
	value, err := structpb.NewValue(m)
	require.NoError(t, err)
	return value
}
