// Package configmap provides a Kubernetes ConfigMap-backed routing store.
// Every entry lives as one key in a single ConfigMap ("klaus-gateway-routes"
// by default) within a single namespace. Writes use optimistic concurrency
// via resourceVersion; on conflict the caller is expected to retry.
//
// This is a placeholder for the cluster mode until the ChannelRoute CRD lands.
// It suits low-write workloads; heavy producers should move to a CRD store.
package configmap

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// DefaultConfigMapName is the ConfigMap used when no name is specified.
const DefaultConfigMapName = "klaus-gateway-routes"

// Store persists routes in a single ConfigMap.
type Store struct {
	client    kubernetes.Interface
	namespace string
	name      string

	// retryOnConflict bounds optimistic-concurrency retries.
	retries int

	mu sync.Mutex
}

// Options configure the ConfigMap-backed store.
type Options struct {
	Namespace string
	Name      string
	Retries   int
}

// New returns a Store backed by the given Kubernetes client. Callers typically
// supply a real clientset from kubernetes.NewForConfig; tests pass fake.NewSimpleClientset.
func New(client kubernetes.Interface, opts Options) *Store {
	if opts.Name == "" {
		opts.Name = DefaultConfigMapName
	}
	if opts.Retries <= 0 {
		opts.Retries = 5
	}
	return &Store{
		client:    client,
		namespace: opts.Namespace,
		name:      opts.Name,
		retries:   opts.Retries,
	}
}

// Get returns the entry for k.
func (s *Store) Get(ctx context.Context, k store.Key) (store.Entry, bool, error) {
	cm, err := s.get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return store.Entry{}, false, nil
		}
		return store.Entry{}, false, err
	}
	raw, ok := cm.Data[k.String()]
	if !ok {
		return store.Entry{}, false, nil
	}
	var e store.Entry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return store.Entry{}, false, err
	}
	if e.Expired(time.Now()) {
		return store.Entry{}, false, nil
	}
	return e, true, nil
}

// Put upserts the entry, retrying on resourceVersion conflicts.
func (s *Store) Put(ctx context.Context, k store.Key, e store.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(cm *corev1.ConfigMap) {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[k.String()] = string(buf)
	})
}

// Delete removes the entry, retrying on resourceVersion conflicts.
func (s *Store) Delete(ctx context.Context, k store.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(ctx, func(cm *corev1.ConfigMap) {
		delete(cm.Data, k.String())
	})
}

// List returns every entry in the ConfigMap.
func (s *Store) List(ctx context.Context) ([]store.KeyEntry, error) {
	cm, err := s.get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]store.KeyEntry, 0, len(cm.Data))
	now := time.Now()
	for ks, raw := range cm.Data {
		k, err := store.ParseKey(ks)
		if err != nil {
			return nil, err
		}
		var e store.Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		if e.Expired(now) {
			continue
		}
		out = append(out, store.KeyEntry{Key: k, Entry: e})
	}
	return out, nil
}

// Close is a no-op; the client is owned by the caller.
func (s *Store) Close() error { return nil }

func (s *Store) get(ctx context.Context) (*corev1.ConfigMap, error) {
	return s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
}

// mutate applies fn to the current ConfigMap (creating it if missing) and
// writes it back. It retries up to retries times on conflict.
func (s *Store) mutate(ctx context.Context, fn func(*corev1.ConfigMap)) error {
	for i := 0; i < s.retries; i++ {
		cm, err := s.get(ctx)
		switch {
		case apierrors.IsNotFound(err):
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace},
				Data:       map[string]string{},
			}
			fn(cm)
			_, err = s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
			if err == nil {
				return nil
			}
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
			continue
		case err != nil:
			return err
		}
		fn(cm)
		_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("configmap %s/%s: conflict retry limit exceeded", s.namespace, s.name)
}
