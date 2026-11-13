package orgnamespaces_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/klaus-gateway/pkg/orgnamespaces"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// An organization namespace is named by its organization label; a namespace
// without the label, with an empty one, or not known at all is not one.
func TestDirectory_Organization(t *testing.T) {
	client := fake.NewClientset(
		namespace("agents-payments-x7k2p", map[string]string{orgnamespaces.OrganizationLabel: "payments", orgnamespaces.ParentLabel: "org-acme"}),
		namespace("agents-orphan-d4e5f", map[string]string{orgnamespaces.OrganizationLabel: "orphan"}),
		namespace("agents-empty-a1b2c", map[string]string{orgnamespaces.OrganizationLabel: ""}),
		namespace("org-acme", map[string]string{"giantswarm.io/organization": "acme"}),
		namespace("other", nil),
	)
	dir := orgnamespaces.New(client)
	dir.Start(t.Context())
	require.True(t, dir.WaitForSync(t.Context()))

	name, parent, ok := dir.Organization("agents-payments-x7k2p")
	require.True(t, ok)
	require.Equal(t, "payments", name)
	require.Equal(t, "org-acme", parent)

	name, parent, ok = dir.Organization("agents-orphan-d4e5f")
	require.True(t, ok, "a missing parent label still names the organization")
	require.Equal(t, "orphan", name)
	require.Empty(t, parent)

	for _, ns := range []string{"agents-empty-a1b2c", "org-acme", "other", "missing"} {
		_, _, ok := dir.Organization(ns)
		require.False(t, ok, ns)
	}
}

// Before the first list the directory knows no organization, and says so
// instead of waiting.
func TestDirectory_OrganizationBeforeStart(t *testing.T) {
	client := fake.NewClientset(namespace("agents-payments-x7k2p", map[string]string{orgnamespaces.OrganizationLabel: "payments"}))
	dir := orgnamespaces.New(client)

	_, _, ok := dir.Organization("agents-payments-x7k2p")
	require.False(t, ok)
}
