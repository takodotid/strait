package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// CNPG kinds the cnpg plugin reads.
var (
	CNPGCluster = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	CNPGPooler  = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"}
)

// SetupWithManager registers the reconciler. Changes is a channel of peer changes; any value requeues every SiteService.
// WatchCNPG adds watches on CloudNativePG clusters and poolers; set it only when their CRDs exist.
func (r *SiteServiceReconciler) SetupWithManager(mgr ctrl.Manager, changes <-chan event.GenericEvent, watchCNPG bool) error {
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return r.all(ctx, "")
	})
	inNamespace := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		return r.all(ctx, o.GetNamespace())
	})
	b := ctrl.NewControllerManagedBy(mgr).
		For(&straitv1.SiteService{}).
		Owns(&corev1.Service{}).
		Owns(&straitv1.Export{}).
		Owns(&straitv1.Import{}).
		Watches(&discoveryv1.EndpointSlice{}, inNamespace).
		Watches(&straitv1.Site{}, all).
		Watches(&straitv1.SiteLink{}, all).
		WatchesRawSource(source.Channel(changes, all)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1})
	if watchCNPG {
		for _, gvk := range []schema.GroupVersionKind{CNPGCluster, CNPGPooler} {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk)
			b = b.Watches(u, inNamespace)
		}
	}
	return b.Complete(r)
}

func (r *SiteServiceReconciler) all(ctx context.Context, namespace string) []reconcile.Request {
	var list straitv1.SiteServiceList
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.List(ctx, &list, opts...); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, s := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
	}
	return out
}
