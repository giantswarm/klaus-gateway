package configmap_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	configmapstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/configmap"
)

// configMapKey is what the API server accepts as a ConfigMap data key. The
// fake clientset does not validate it, so the test does: the routing key's
// own serialised form ("slack|C1||1700.0001|sre") is rejected by a real
// cluster, which made every Put on the store fail.
var configMapKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

func TestStore_RoundTripWithValidDataKeys(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	s := configmapstore.New(client, configmapstore.Options{Namespace: "agent-platform"})

	slack := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "sre-agent"}
	web := store.Key{Channel: "web", ChannelID: "lab", UserID: "admin@lab.local", ThreadID: "t|1"}
	require.NoError(t, s.Put(ctx, slack, store.Entry{
		AgentInstanceID: "inst-1", TaskID: "task-7",
		Resume:    map[string]string{"slack_user": "U1"},
		CreatedAt: time.Now(), LastSeen: time.Now(),
	}))
	require.NoError(t, s.Put(ctx, web, store.Entry{Instance: "klaus-1", CreatedAt: time.Now(), LastSeen: time.Now()}))

	cm, err := client.CoreV1().ConfigMaps("agent-platform").Get(ctx, configmapstore.DefaultConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, cm.Data, 2)
	for k := range cm.Data {
		require.Regexp(t, configMapKey, k, "a ConfigMap data key must stay within the API server's charset")
	}

	got, ok, err := s.Get(ctx, slack)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "inst-1", got.AgentInstanceID)
	require.Equal(t, "task-7", got.TaskID)
	require.Equal(t, map[string]string{"slack_user": "U1"}, got.Resume)

	all, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	keys := map[string]string{}
	for _, ke := range all {
		keys[ke.Key.String()] = ke.Entry.Instance + ke.Entry.AgentInstanceID
	}
	require.Equal(t, map[string]string{slack.String(): "inst-1", web.String(): "klaus-1"}, keys, "List decodes the stored keys back")

	require.NoError(t, s.Delete(ctx, slack))
	_, ok, err = s.Get(ctx, slack)
	require.NoError(t, err)
	require.False(t, ok)
	all, err = s.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
}
