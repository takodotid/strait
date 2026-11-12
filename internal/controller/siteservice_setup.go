package controller

import (
	"context"
	"slices"

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

// IndexMemberService indexes SiteServices by the name of each Service that their members' plugin reads the endpoints
// of: a declared member's Service, and a cnpg member's Cluster Services and Pooler.
const IndexMemberService = "strait.tako.id/memberService"

// memberServices is the value of IndexMemberService. A cnpg member reads one of its Cluster's Services by its role and
// whether the Cluster is a replica cluster, which can change at any time, so all three count.
func memberServices(o client.Object) []string {
	s, ok := o.(*straitv1.SiteService)
	if !ok {
		return nil
	}
	var out []string
	for _, m := range s.Spec.Members {
		switch {
		case m.Service != nil:
			out = append(out, m.Service.Name)
		case m.CNPG != nil:
			out = append(out, m.CNPG.Cluster+"-rw", m.CNPG.Cluster+"-ro", m.CNPG.Cluster+"-r")
			if m.CNPG.Pooler != "" {
				out = append(out, m.CNPG.Pooler)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SetupWithManager registers the reconciler. Changes is a channel of peer changes; any value requeues every SiteService.
// WatchCNPG adds watches on CloudNativePG clusters and poolers; set it only when their CRDs exist.
func (r *SiteServiceReconciler) SetupWithManager(mgr ctrl.Manager, changes <-chan event.GenericEvent, watchCNPG bool) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &straitv1.SiteService{}, IndexMemberService, memberServices); err != nil {
		return err
	}
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
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.usersOfSlice)).
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

// usersOfSlice returns the SiteServices whose members' plugin reads the endpoints of the EndpointSlice's Service. A
// slice that names no Service, or Strait's own slice of an application name, requeues none: the decision that comes
// every 2s repairs a change to it.
func (r *SiteServiceReconciler) usersOfSlice(ctx context.Context, o client.Object) []reconcile.Request {
	name := o.GetLabels()[discoveryv1.LabelServiceName]
	if name == "" {
		return nil
	}
	var list straitv1.SiteServiceList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{IndexMemberService: name}); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, s := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
	}
	return out
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
