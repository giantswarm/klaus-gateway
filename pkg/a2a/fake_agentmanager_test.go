package a2a_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/giantswarm/klaus-gateway/pkg/agentmanager"
	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/muster/mustertest"
)

// agent-manager's start_session and list_organizations as muster names them.
const (
	startSessionTool      = "x_agent-manager_start_session"
	listOrganizationsTool = "x_agent-manager_list_organizations"
)

// agentManager starts a fake muster whose agent-manager start_session does
// what agent-manager's does without a message: it creates the Session at the
// fake controller as the caller and answers {session}, mapping the
// controller's refusals onto agent-manager's "<code>: <reason>" error answers.
// The calls it served are f.musterCalls.
func (f *fakeKagent) agentManager(t *testing.T) *agentmanager.Starter {
	t.Helper()
	srv := mustertest.New(nil)
	srv.CallTools = map[string]func(mustertest.Call) mustertest.Answer{
		startSessionTool: f.startSession,
	}
	if f.organizations != nil {
		srv.CallTools[listOrganizationsTool] = f.listOrganizations
	}
	t.Cleanup(srv.Close)
	f.muster = srv
	return agentmanager.New(muster.NewClient(srv.URL), "x_agent-manager_", agentmanager.Options{PlatformNamespaces: []string{"kagent"}})
}

func (f *fakeKagent) startSession(call mustertest.Call) mustertest.Answer {
	str := func(key string) string { s, _ := call.Args[key].(string); return s }
	namespace, name, organization := str("namespace"), str("name"), str("organization")
	if organization != "" {
		ns, ok := f.organizations[organization]
		if !ok {
			return mustertest.Answer{IsError: true, Text: "not_found: organization " + organization + " not found; list_organizations lists yours"}
		}
		namespace = ns
	}
	if sessionName := str("sessionName"); utf8.RuneCountInString(sessionName) > 200 {
		return mustertest.Answer{IsError: true, Text: "invalid_request: invalid request: sessionName is longer than 200 characters"}
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", call.Bearer))
	resp, err := f.CreateSession(ctx, &apiv1alpha1.CreateSessionRequest{
		Agent:     &apiv1alpha1.ResourceReference{Namespace: namespace, Name: name},
		RequestId: str("requestId"),
		Name:      str("sessionName"),
	})
	if err != nil {
		code := map[codes.Code]string{
			codes.FailedPrecondition: "conflict",
			codes.AlreadyExists:      "conflict",
			codes.Unauthenticated:    "unauthenticated",
			codes.PermissionDenied:   "forbidden",
			codes.NotFound:           "not_found",
			codes.InvalidArgument:    "invalid_request",
		}[status.Code(err)]
		if code == "" {
			code = "backend_error"
		}
		return mustertest.Answer{IsError: true, Text: fmt.Sprintf("%s: %s: kagent: a session of agent %s/%s: %s", code, code, namespace, name, status.Convert(err).Message())}
	}
	session, err := protojson.Marshal(resp.GetSession())
	if err != nil {
		return mustertest.Answer{IsError: true, Text: "backend_error: " + err.Error()}
	}
	answer := map[string]json.RawMessage{"session": session}
	if organization != "" {
		answer["organization"], _ = json.Marshal(organization)
	}
	out, _ := json.Marshal(answer)
	return mustertest.Answer{Text: string(out)}
}

// listOrganizations is the list_organizations of the agent-manager that takes
// an organization's Agent by the organization's name: each organization by
// name and namespace.
func (f *fakeKagent) listOrganizations(mustertest.Call) mustertest.Answer {
	entries := []map[string]string{}
	for name, namespace := range f.organizations {
		entries = append(entries, map[string]string{"name": name, "namespace": namespace})
	}
	out, _ := json.Marshal(map[string]any{"organizations": entries})
	return mustertest.Answer{Text: string(out)}
}
