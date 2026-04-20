// Package v1alpha1 contains API types for the routing.giantswarm.io group.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ChannelRouteSpec defines the routing rule: it maps a conversation key
// (channel, channelID, userID, threadID) to a named Klaus instance.
type ChannelRouteSpec struct {
	// Channel identifies the adapter type (web, slack, cli).
	Channel string `json:"channel"`
	// ChannelID is the workspace / team / server identifier.
	ChannelID string `json:"channelID"`
	// UserID is the per-channel user identifier.
	UserID string `json:"userID"`
	// ThreadID scopes the route to a specific thread; empty means per-user.
	ThreadID string `json:"threadID"`
	// Instance is the name of the Klaus instance that owns this conversation.
	Instance string `json:"instance"`
	// CreatedAt is when the route was first written.
	CreatedAt metav1.Time `json:"createdAt"`
	// LastSeen is refreshed on every message routed through this entry.
	LastSeen metav1.Time `json:"lastSeen"`
	// TTLSeconds is the idle TTL in seconds. Zero means never expire.
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
}

// ChannelRouteStatus contains observed state reported by the embedded controller.
type ChannelRouteStatus struct {
	// Conditions holds standard Kubernetes conditions for this ChannelRoute.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cr
// +kubebuilder:printcolumn:name="Channel",type=string,JSONPath=".spec.channel"
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=".spec.instance"
// +kubebuilder:printcolumn:name="LastSeen",type=date,JSONPath=".spec.lastSeen"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// ChannelRoute maps a conversation key to the Klaus instance that owns it.
// One CR per active conversation; the embedded controller reconciles instance
// liveness and updates status conditions.
type ChannelRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ChannelRouteSpec   `json:"spec,omitempty"`
	Status ChannelRouteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ChannelRouteList contains a list of ChannelRoute objects.
type ChannelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ChannelRoute `json:"items"`
}
