package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group + version for ChannelRoute.
	GroupVersion = schema.GroupVersion{Group: "routing.giantswarm.io", Version: "v1alpha1"}

	// SchemeBuilder builds the runtime scheme for this group.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registers the types in this package with a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&ChannelRoute{}, &ChannelRouteList{})
}
