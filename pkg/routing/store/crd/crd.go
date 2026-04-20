// Package crd provides a ChannelRoute CRD-backed implementation of routing.Store.
// Each routing entry is persisted as a ChannelRoute CR in a Kubernetes cluster,
// giving per-entry status conditions, RBAC-gated access, and cluster-wide
// visibility. Intended for production deployments where the configmap store's
// single-object write contention becomes a bottleneck.
package crd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/giantswarm/klaus-gateway/pkg/api/v1alpha1"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// Store persists routing entries as ChannelRoute custom resources.
type Store struct {
	client    client.Client
	namespace string
}

// New returns a Store that reads and writes ChannelRoute CRs in namespace.
func New(c client.Client, namespace string) *Store {
	return &Store{client: c, namespace: namespace}
}

// Get returns the entry for k, or (_, false, nil) if absent or expired.
func (s *Store) Get(ctx context.Context, k store.Key) (store.Entry, bool, error) {
	var cr v1alpha1.ChannelRoute
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: crName(k)}, &cr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return store.Entry{}, false, nil
		}
		return store.Entry{}, false, err
	}
	e := entryFromCR(&cr)
	if e.Expired(time.Now()) {
		return store.Entry{}, false, nil
	}
	return e, true, nil
}

// Put creates or updates the ChannelRoute CR for k.
func (s *Store) Put(ctx context.Context, k store.Key, e store.Entry) error {
	name := crName(k)
	var existing v1alpha1.ChannelRoute
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, &existing)
	if apierrors.IsNotFound(err) {
		cr := buildCR(s.namespace, name, k, e)
		return s.client.Create(ctx, &cr)
	}
	if err != nil {
		return err
	}
	existing.Spec = specFromKeyEntry(k, e)
	return s.client.Update(ctx, &existing)
}

// Delete removes the ChannelRoute CR for k. A missing CR is not an error.
func (s *Store) Delete(ctx context.Context, k store.Key) error {
	cr := &v1alpha1.ChannelRoute{}
	cr.Name = crName(k)
	cr.Namespace = s.namespace
	err := s.client.Delete(ctx, cr)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// List returns every live (non-expired) ChannelRoute CR in the namespace.
func (s *Store) List(ctx context.Context) ([]store.KeyEntry, error) {
	var list v1alpha1.ChannelRouteList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]store.KeyEntry, 0, len(list.Items))
	for i := range list.Items {
		cr := &list.Items[i]
		e := entryFromCR(cr)
		if e.Expired(now) {
			continue
		}
		out = append(out, store.KeyEntry{Key: keyFromCR(cr), Entry: e})
	}
	return out, nil
}

// Close is a no-op; the client lifecycle is managed by the caller.
func (s *Store) Close() error { return nil }

// crName returns a deterministic, DNS-valid CR name derived from the routing key.
// Uses the first 40 hex digits of SHA-256(key.String()) to stay well under the
// 253-character limit while being collision-resistant in practice.
func crName(k store.Key) string {
	h := sha256.Sum256([]byte(k.String()))
	return fmt.Sprintf("route-%x", h[:20])
}

func buildCR(namespace, name string, k store.Key, e store.Entry) v1alpha1.ChannelRoute {
	return v1alpha1.ChannelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"routing.giantswarm.io/channel": labelSafe(k.Channel),
			},
		},
		Spec: specFromKeyEntry(k, e),
	}
}

func specFromKeyEntry(k store.Key, e store.Entry) v1alpha1.ChannelRouteSpec {
	return v1alpha1.ChannelRouteSpec{
		Channel:    k.Channel,
		ChannelID:  k.ChannelID,
		UserID:     k.UserID,
		ThreadID:   k.ThreadID,
		Instance:   e.Instance,
		CreatedAt:  metav1.NewTime(e.CreatedAt),
		LastSeen:   metav1.NewTime(e.LastSeen),
		TTLSeconds: int64(e.TTL.Seconds()),
	}
}

func entryFromCR(cr *v1alpha1.ChannelRoute) store.Entry {
	return store.Entry{
		Instance:  cr.Spec.Instance,
		CreatedAt: cr.Spec.CreatedAt.Time,
		LastSeen:  cr.Spec.LastSeen.Time,
		TTL:       time.Duration(cr.Spec.TTLSeconds) * time.Second,
	}
}

func keyFromCR(cr *v1alpha1.ChannelRoute) store.Key {
	return store.Key{
		Channel:   cr.Spec.Channel,
		ChannelID: cr.Spec.ChannelID,
		UserID:    cr.Spec.UserID,
		ThreadID:  cr.Spec.ThreadID,
	}
}

// labelSafe truncates s to 63 characters, the maximum for a Kubernetes label value.
func labelSafe(s string) string {
	if len(s) > 63 {
		return s[:63]
	}
	return s
}
