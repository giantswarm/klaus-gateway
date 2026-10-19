package refusals

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	apiv1alpha1 "github.com/giantswarm/klaus-gateway/pkg/kagent/gen/kagent/api/v1alpha1"
)

// sessionCacheMax bounds the Sessions KagentSessions remembers. A Session
// runs one Agent for its whole life, so an answer never goes stale.
const sessionCacheMax = 4096

// KagentSessions asks the kagent controller which Agent a Session runs, as
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

// The Agent annotations a notice reads: the creator agent-manager records,
// and the display name the agent chart renders.
const (
	CreatedByAnnotation   = "agent-platform.giantswarm.io/created-by"
	DisplayNameAnnotation = "ui.giantswarm.io/display-name"
)

// AgentsGVR is the kagent Agent resource.
var AgentsGVR = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}

// KubeAgents reads Agents from the Kubernetes API.
type KubeAgents struct {
	Client dynamic.Interface
}

// Agent implements Agents.
func (k *KubeAgents) Agent(ctx context.Context, namespace, name string) (Agent, error) {
	obj, err := k.Client.Resource(AgentsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Agent{}, fmt.Errorf("refusals: get Agent %s/%s: %w", namespace, name, err)
	}
	annotations := obj.GetAnnotations()
	return Agent{DisplayName: annotations[DisplayNameAnnotation], Creator: annotations[CreatedByAnnotation]}, nil
}
