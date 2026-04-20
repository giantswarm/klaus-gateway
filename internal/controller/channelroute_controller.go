// Package controller contains the embedded controller-runtime reconciler for
// ChannelRoute CRs. It runs in-process inside the gateway when --controller=true
// and updates status conditions to reflect whether the referenced Klaus instance
// is reachable.
package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/giantswarm/klaus-gateway/pkg/api/v1alpha1"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

const (
	conditionReady   = "Ready"
	requeueInterval  = 5 * time.Minute
)

// ChannelRouteReconciler reconciles ChannelRoute resources and keeps their
// status conditions in sync with the liveness of the referenced Klaus instance.
//
// +kubebuilder:rbac:groups=routing.giantswarm.io,resources=channelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=routing.giantswarm.io,resources=channelroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=routing.giantswarm.io,resources=channelroutes/finalizers,verbs=update
type ChannelRouteReconciler struct {
	client.Client
	Lifecycle lifecycle.Manager
}

// Reconcile is called by controller-runtime whenever a ChannelRoute is created,
// updated, or re-queued. It checks whether the referenced instance is alive and
// writes the result into status.conditions.
func (r *ChannelRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cr v1alpha1.ChannelRoute
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	condStatus, reason, message := r.instanceCondition(ctx, cr.Spec.Instance)
	if condStatus == metav1.ConditionFalse {
		logger.Info("instance unreachable for ChannelRoute",
			"route", req.NamespacedName, "instance", cr.Spec.Instance)
	}

	cond := metav1.Condition{
		Type:               conditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cr.Generation,
	}
	meta.SetStatusCondition(&cr.Status.Conditions, cond)

	if err := r.Status().Update(ctx, &cr); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// SetupWithManager registers this reconciler with mgr.
func (r *ChannelRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ChannelRoute{}).
		Complete(r)
}

func (r *ChannelRouteReconciler) instanceCondition(ctx context.Context, name string) (metav1.ConditionStatus, string, string) {
	ref, err := r.Lifecycle.Get(ctx, name)
	if err != nil {
		return metav1.ConditionFalse, "InstanceNotFound", err.Error()
	}
	return metav1.ConditionTrue, "InstanceReady", ref.BaseURL
}
