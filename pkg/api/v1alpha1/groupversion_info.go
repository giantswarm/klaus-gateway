package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group + version for ChannelRoute.
	GroupVersion = schema.GroupVersion{Group: "routing.giantswarm.io", Version: "v1alpha1"}

	// SchemeBuilder builds the runtime scheme for this group.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme registers the types in this package with a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &ChannelRoute{}, &ChannelRouteList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
