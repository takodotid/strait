package controller

import (
	"context"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/takodotid/strait/api/v1alpha1"
)

// XDSTrigger asks the xDS server to rebuild when an input of the gateway's configuration changes.
type XDSTrigger struct {
	Trigger   func()
	Namespace string
}

// Reconcile implements reconcile.Reconciler.
func (t *XDSTrigger) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	t.Trigger()
	return ctrl.Result{}, nil
}

// SetupWithManager registers the trigger.
func (t *XDSTrigger) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "xds"}}}
	})
	inNamespace := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == t.Namespace
	}))
	return ctrl.NewControllerManagedBy(mgr).
		Named("xds").
		Watches(&v1alpha1.Site{}, one).
		Watches(&v1alpha1.SitePeer{}, one).
		Watches(&v1alpha1.SiteLink{}, one).
		Watches(&v1alpha1.Export{}, one).
		Watches(&v1alpha1.Import{}, one).
		Watches(&discoveryv1.EndpointSlice{}, one).
		Watches(&corev1.Pod{}, one, inNamespace).
		Watches(&coordinationv1.Lease{}, one, inNamespace).
		Complete(t)
}
