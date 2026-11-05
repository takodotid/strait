package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// IndexExportService indexes Exports by the name of the Service they forward to.
const IndexExportService = "spec.service.name"

// exportService is the value of IndexExportService.
func exportService(o client.Object) []string {
	e, ok := o.(*straitv1.Export)
	if !ok || e.Spec.Service.Name == "" {
		return nil
	}
	return []string{e.Spec.Service.Name}
}

// XDSTrigger asks the xDS server to rebuild when an input of the gateway's configuration changes. The configuration
// is built from the Site, the SitePeers, the SiteLinks, the Exports with the EndpointSlices of their Services, the
// Imports, and the gateway pods' addresses (xds.Collect). A change of anything else waits for the server's own
// rebuild, every 2s.
type XDSTrigger struct {
	Trigger   func()
	Namespace string
	// Reader reads the Exports, with IndexExportService. Nil is the manager's client.
	Reader client.Reader
}

// Reconcile implements reconcile.Reconciler.
func (t *XDSTrigger) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	t.Trigger()
	return ctrl.Result{}, nil
}

// SetupWithManager registers the trigger and its index.
func (t *XDSTrigger) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &straitv1.Export{}, IndexExportService, exportService); err != nil {
		return err
	}
	if t.Reader == nil {
		t.Reader = mgr.GetClient()
	}
	one := []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "xds"}}}
	always := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request { return one })
	exported := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		if !t.exported(ctx, o) {
			return nil
		}
		return one
	})
	inNamespace := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == t.Namespace
	}))
	return ctrl.NewControllerManagedBy(mgr).
		Named("xds").
		Watches(&straitv1.Site{}, always).
		Watches(&straitv1.SitePeer{}, always).
		Watches(&straitv1.SiteLink{}, always).
		Watches(&straitv1.Export{}, always).
		Watches(&straitv1.Import{}, always).
		Watches(&discoveryv1.EndpointSlice{}, exported).
		Watches(&corev1.Pod{}, always, inNamespace).
		Complete(t)
}

// exported reports whether an EndpointSlice belongs to a Service that an Export of its namespace forwards to. When the
// Exports cannot be read, it reports true.
func (t *XDSTrigger) exported(ctx context.Context, o client.Object) bool {
	service := o.GetLabels()[discoveryv1.LabelServiceName]
	if service == "" {
		return false
	}
	var exports straitv1.ExportList
	if err := t.Reader.List(ctx, &exports, client.InNamespace(o.GetNamespace()), client.MatchingFields{IndexExportService: service}); err != nil {
		return true
	}
	return len(exports.Items) > 0
}
