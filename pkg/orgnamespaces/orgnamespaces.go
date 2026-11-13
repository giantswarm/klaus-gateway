// Package orgnamespaces names Agent Platform organization namespaces by their
// organization. agent-platform-operator creates each AgentPlatformOrganization's
// namespace (agents-<name>-<random>) and labels it with the organization's
// name and its parent namespace; the directory reads those labels, never the
// namespace's name.
package orgnamespaces

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	// OrganizationLabel is on an organization namespace: the name of its
	// AgentPlatformOrganization.
	OrganizationLabel = "agent-platform.giantswarm.io/organization"
	// ParentLabel is on an organization namespace: the parent namespace its
	// AgentPlatformOrganization lives in.
	ParentLabel = "agent-platform.giantswarm.io/parent"
)

// Directory watches the namespaces labelled OrganizationLabel with the
// gateway's ServiceAccount (list and watch on namespaces). It decides nothing
// about access: it only names a namespace a person's roster already lists.
type Directory struct {
	factory informers.SharedInformerFactory
	lister  corelisters.NamespaceLister
	synced  cache.InformerSynced
}

// New builds a Directory on client; Start begins the watch.
func New(client kubernetes.Interface) *Directory {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = OrganizationLabel }))
	namespaces := factory.Core().V1().Namespaces()
	return &Directory{factory: factory, lister: namespaces.Lister(), synced: namespaces.Informer().HasSynced}
}

// Start lists and watches the organization namespaces until ctx ends.
func (d *Directory) Start(ctx context.Context) {
	d.factory.Start(ctx.Done())
}

// WaitForSync waits for the first list; false when ctx ends first. The
// gateway does not wait: Organization answers ok false until then. It is for
// a caller that must, such as a test.
func (d *Directory) WaitForSync(ctx context.Context) bool {
	return cache.WaitForCacheSync(ctx.Done(), d.synced)
}

// Organization is the name of the organization whose namespace namespace is,
// and its parent namespace (ParentLabel; empty when the label is missing).
// ok is false for a namespace without a non-empty OrganizationLabel, and for
// every namespace until the first list has completed.
func (d *Directory) Organization(namespace string) (name, parent string, ok bool) {
	if !d.synced() {
		return "", "", false
	}
	ns, err := d.lister.Get(namespace)
	if err != nil {
		return "", "", false
	}
	name = ns.Labels[OrganizationLabel]
	if name == "" {
		return "", "", false
	}
	return name, ns.Labels[ParentLabel], true
}
