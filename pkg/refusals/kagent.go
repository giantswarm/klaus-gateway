package refusals

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// sessionCacheMax bounds the Sessions KagentSessions remembers. A Session
// runs one Agent for its whole life, so an answer never goes stale.
const sessionCacheMax = 4096

// KagentSessions asks the kagent controller which Agent a Session runs, or
// ran: a deleted Session answers with its tombstone (RUNTIME_STATE_DELETED),
// which still names the Agent. It asks as
// the gateway's own ServiceAccount: the controller admits that one account
// to read any creator's Session (its session reader), with the projected
// token at TokenFile as the bearer.
type KagentSessions struct {
	Conn      grpc.ClientConnInterface
	TokenFile string

	mu    sync.Mutex
	cache map[string][2]string
}

// SessionAgent implements Sessions.
func (k *KagentSessions) SessionAgent(ctx context.Context, sessionID string) (string, string, error) {
	k.mu.Lock()
	if hit, ok := k.cache[sessionID]; ok {
		k.mu.Unlock()
		return hit[0], hit[1], nil
	}
	k.mu.Unlock()

	token, err := os.ReadFile(k.TokenFile)
	if err != nil {
		return "", "", fmt.Errorf("refusals: read the kagent token: %w", err)
	}
	callCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := apiv1alpha1.NewSessionServiceClient(k.Conn).GetSession(callCtx, &apiv1alpha1.GetSessionRequest{SessionId: sessionID})
	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound, codes.InvalidArgument:
		return "", "", fmt.Errorf("%w: %s", ErrNotSession, sessionID)
	default:
		return "", "", fmt.Errorf("refusals: get Session %s: %w", sessionID, err)
	}
	agent := resp.GetSession().GetAgent()
	if agent.GetName() == "" {
		return "", "", fmt.Errorf("%w: %s names no Agent", ErrNotSession, sessionID)
	}
	k.mu.Lock()
	if k.cache == nil || len(k.cache) >= sessionCacheMax {
		k.cache = map[string][2]string{}
	}
	k.cache[sessionID] = [2]string{agent.GetNamespace(), agent.GetName()}
	k.mu.Unlock()
	return agent.GetNamespace(), agent.GetName(), nil
}

// The annotations a notice reads: the creator agent-manager records on an
// Agent, and the display name the agent chart renders on an Agent and an
// admin may set on a Harness.
const (
	CreatedByAnnotation      = "agent-platform.giantswarm.io/created-by"
	DisplayNameAnnotation    = "ui.giantswarm.io/display-name"
	NotifyRefusalsAnnotation = "klaus.giantswarm.io/notify-egress-refusals"
)

// AgentsGVR is the kagent Agent resource and HarnessesGVR the Harness an
// Agent names in spec.harnessRef, in its own namespace.
var (
	AgentsGVR    = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}
	HarnessesGVR = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "harnesses"}
)

// KubeAgents reads Agents and their Harnesses from the Kubernetes API.
type KubeAgents struct {
	Client dynamic.Interface
	Logger *slog.Logger
}

// Agent implements Agents. The Harness is read only for an Agent with a
// creator, the one a notice goes to. A Harness that is not found, or that the
// gateway may not read, leaves the runtime unknown; any other failure to read
// it is an error, so the refusal is noted on a later read rather than with a
// remedy that may be wrong.
func (k *KubeAgents) Agent(ctx context.Context, namespace, name string) (Agent, error) {
	obj, err := k.Client.Resource(AgentsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Agent{}, fmt.Errorf("refusals: get Agent %s/%s: %w", namespace, name, err)
	}
	annotations := obj.GetAnnotations()
	out := Agent{DisplayName: annotations[DisplayNameAnnotation], Creator: annotations[CreatedByAnnotation]}
	out.NotifyRefusals = annotations[NotifyRefusalsAnnotation] == "true"
	harness, _, _ := unstructured.NestedString(obj.Object, "spec", "harnessRef", "name")
	if out.Creator == "" || !out.NotifyRefusals || harness == "" {
		return out, nil
	}
	h, err := k.Client.Resource(HarnessesGVR).Namespace(namespace).Get(ctx, harness, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return out, nil
	case apierrors.IsForbidden(err):
		k.logger().Warn("refusals: the gateway may not read the Agent's Harness, so the notice names no remedy: grant get on api.kagent.dev harnesses",
			"agent", namespace+"/"+name, "harness", harness, "error", err)
		return out, nil
	case err != nil:
		return Agent{}, fmt.Errorf("refusals: get Harness %s/%s of Agent %s: %w", namespace, harness, name, err)
	}
	out.Runtime = harnessRuntime(h)
	out.HarnessTitle = h.GetAnnotations()[DisplayNameAnnotation]
	return out, nil
}

// harnessSpecFields are the Harness spec fields that select no runtime.
var harnessSpecFields = map[string]bool{"workload": true, "env": true, "substrate": true}

// harnessRuntime is the runtime a Harness selects: the one key of its spec
// besides harnessSpecFields (kagent, claude, codex, byo), as agent-manager
// reads it; "" for none.
func harnessRuntime(h *unstructured.Unstructured) string {
	spec, _, _ := unstructured.NestedMap(h.Object, "spec")
	keys := make([]string, 0, 1)
	for key := range spec {
		if !harnessSpecFields[key] {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	slices.Sort(keys)
	return keys[0]
}

func (k *KubeAgents) logger() *slog.Logger {
	if k.Logger != nil {
		return k.Logger
	}
	return slog.Default()
}
