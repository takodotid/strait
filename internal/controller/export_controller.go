package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/plugin"
)

// Condition types of Exports and Imports.
const (
	ConditionReady     = "Ready"
	ConditionAllocated = "Allocated"
)

// ExportReconciler allocates each Export's gateway port and reports its readiness.
type ExportReconciler struct {
	client.Client
	Reader client.Reader
	Local  Local
}

// Reconcile implements reconcile.Reconciler.
func (r *ExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var exp v1alpha1.Export
	if err := r.Get(ctx, req.NamespacedName, &exp); err != nil {
		if apierrors.IsNotFound(err) {
			r.Local.DeleteExport(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !exp.DeletionTimestamp.IsZero() {
		r.Local.DeleteExport(req.Namespace, req.Name)
		return ctrl.Result{}, nil
	}
	site, err := LocalSite(ctx, r.Reader)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, err
	}
	old := exp.Status.DeepCopy()

	rng := site.Spec.Gateway.ExportPorts
	if p := exp.Status.GatewayPort; p < rng.First || p > rng.Last {
		var list v1alpha1.ExportList
		if err := r.List(ctx, &list); err != nil {
			return ctrl.Result{}, err
		}
		used := map[int32]bool{}
		for _, e := range list.Items {
			if e.UID != exp.UID {
				used[e.Status.GatewayPort] = true
			}
		}
		exp.Status.GatewayPort = allocate(rng, used)
	}
	alloc := metav1.Condition{Type: ConditionAllocated, Status: metav1.ConditionTrue, Reason: "Allocated", ObservedGeneration: exp.Generation}
	if exp.Status.GatewayPort == 0 {
		alloc.Status, alloc.Reason, alloc.Message = metav1.ConditionFalse, "RangeFull", fmt.Sprintf("no free port in %d-%d", rng.First, rng.Last)
	}
	meta.SetStatusCondition(&exp.Status.Conditions, alloc)

	ep, err := plugin.ReadyEndpoint(ctx, r.Reader, exp.Namespace, exp.Spec.Service.Name, exp.Spec.Service.Port)
	ready := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "EndpointsReady", ObservedGeneration: exp.Generation}
	switch {
	case errors.Is(err, plugin.ErrNoService):
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "ServiceNotFound", fmt.Sprintf("service %s not found", exp.Spec.Service.Name)
	case err != nil:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "ReadFailed", err.Error()
	case len(ep.Addresses) == 0:
		ready.Status, ready.Reason = metav1.ConditionFalse, "NoReadyEndpoint"
	}
	exp.Status.ReadyEndpoints = int32(len(ep.Addresses))
	meta.SetStatusCondition(&exp.Status.Conditions, ready)

	r.Local.SetExport(exp.Namespace, exp.Name, exp.Status.GatewayPort, exp.Status.ReadyEndpoints > 0)
	if !equalStatus(old, &exp.Status) {
		if err := r.Status().Update(ctx, &exp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// allocate returns the lowest port of rng that is not used, or 0.
func allocate(rng v1alpha1.PortRange, used map[int32]bool) int32 {
	for p := rng.First; p <= rng.Last; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

func equalStatus[T any](a, b *T) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// SetupWithManager registers the reconciler.
func (r *ExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inNamespace := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var list v1alpha1.ExportList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, e := range list.Items {
			if e.Spec.Service.Name == o.GetLabels()[discoveryv1.LabelServiceName] {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: e.Namespace, Name: e.Name}})
			}
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("export").
		For(&v1alpha1.Export{}).
		Watches(&discoveryv1.EndpointSlice{}, inNamespace).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// ImportReconciler allocates each Import's gateway port and reads the peer's export port.
type ImportReconciler struct {
	client.Client
	Reader client.Reader
	Peers  Peers
	Now    func() time.Time
}

// Reconcile implements reconcile.Reconciler.
func (r *ImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var imp v1alpha1.Import
	if err := r.Get(ctx, req.NamespacedName, &imp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	site, err := LocalSite(ctx, r.Reader)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, err
	}
	old := imp.Status.DeepCopy()

	rng := site.Spec.Gateway.ImportPorts
	if p := imp.Status.GatewayPort; p < rng.First || p > rng.Last {
		var list v1alpha1.ImportList
		if err := r.List(ctx, &list); err != nil {
			return ctrl.Result{}, err
		}
		used := map[int32]bool{}
		for _, e := range list.Items {
			if e.UID != imp.UID {
				used[e.Status.GatewayPort] = true
			}
		}
		imp.Status.GatewayPort = allocate(rng, used)
	}
	alloc := metav1.Condition{Type: ConditionAllocated, Status: metav1.ConditionTrue, Reason: "Allocated", ObservedGeneration: imp.Generation}
	if imp.Status.GatewayPort == 0 {
		alloc.Status, alloc.Reason, alloc.Message = metav1.ConditionFalse, "RangeFull", fmt.Sprintf("no free port in %d-%d", rng.First, rng.Last)
	}
	meta.SetStatusCondition(&imp.Status.Conditions, alloc)

	// The remote port is kept from the last document while the peer is stale, so the gateway keeps its listener.
	ready := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: "PeerUnknown", ObservedGeneration: imp.Generation}
	if port, ok, found := remoteExport(r.Peers, imp.Spec.Site, imp.Namespace, imp.Spec.Export, r.Now(), site.Spec.Exchange.PeerTimeout.Duration); found {
		imp.Status.RemotePort = port
		imp.Status.RemoteReady = ok
		ready.Status, ready.Reason = metav1.ConditionTrue, "RemoteReady"
		if !ok {
			ready.Status, ready.Reason = metav1.ConditionFalse, "RemoteNotReady"
		}
	} else {
		imp.Status.RemoteReady = false
		if r.Peers.Fresh(imp.Spec.Site, r.Now(), site.Spec.Exchange.PeerTimeout.Duration) {
			ready.Reason, ready.Message = "ExportNotFound", fmt.Sprintf("site %s does not list export %s", imp.Spec.Site, imp.Spec.Export)
		}
	}
	meta.SetStatusCondition(&imp.Status.Conditions, ready)

	if !equalStatus(old, &imp.Status) {
		if err := r.Status().Update(ctx, &imp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// SetupWithManager registers the reconciler. Any value on changes requeues every Import.
func (r *ImportReconciler) SetupWithManager(mgr ctrl.Manager, changes <-chan event.GenericEvent) error {
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list v1alpha1.ImportList
		if err := r.List(ctx, &list); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: i.Namespace, Name: i.Name}})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("import").
		For(&v1alpha1.Import{}).
		WatchesRawSource(source.Channel(changes, all)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// remoteExport returns an export's port from the peer's last document, and its readiness while that document is fresh.
func remoteExport(p Peers, peer, namespace, name string, now time.Time, timeout time.Duration) (port int32, ready bool, found bool) {
	doc, state := p.Snapshot(peer)
	for _, e := range doc.Exports {
		if e.Namespace == namespace && e.Name == name {
			return e.Port, e.Ready && state.Fresh(now, timeout), true
		}
	}
	return 0, false, false
}
