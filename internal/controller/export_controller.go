package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/plugin"
)

// Condition types of Exports and Imports. An Import has ConditionService only while it sets spec.port.
const (
	ConditionReady     = "Ready"
	ConditionAllocated = "Allocated"
	ConditionService   = "Service"
)

// ExportReconciler allocates each Export's gateway port and reports its readiness.
type ExportReconciler struct {
	client.Client
	Reader client.Reader
	Local  Local

	// written keeps what the last status write of each Export returned, until the cache shows it.
	written written[straitv1.Export, *straitv1.Export]
}

// Reconcile implements reconcile.Reconciler.
func (r *ExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var exp straitv1.Export
	if err := r.Get(ctx, req.NamespacedName, &exp); err != nil {
		if apierrors.IsNotFound(err) {
			r.Local.DeleteExport(req.Namespace, req.Name)
			r.written.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.written.latest(&exp)
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
	exp.Status.GatewayPort, err = nextPort(ctx, r.Client, rng, &exp, &r.written, exportPort, func(opts ...client.ListOption) ([]straitv1.Export, error) {
		var list straitv1.ExportList
		err := r.List(ctx, &list, opts...)
		return list.Items, err
	})
	if err != nil {
		return ctrl.Result{}, err
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
		if _, err := r.written.updateStatus(ctx, r.Client, &exp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// portHolder is another Export, or another Import, and the gateway port it holds.
type portHolder struct {
	key  types.NamespacedName
	port int32
}

func exportPort(e *straitv1.Export) int32 { return e.Status.GatewayPort }
func importPort(i *straitv1.Import) int32 { return i.Status.GatewayPort }

// otherPorts returns the gateway port of each listed object but self: as the cache shows it or, while the cache
// predates this controller's last status write of that object, as that write set it. So a port that this controller
// allocated counts as taken before the cache shows it. That holds because the reconciles of one kind run one at a time.
func otherPorts[T any, P interface {
	*T
	client.Object
}](items []T, self types.UID, w *written[T, P], port func(P) int32) []portHolder {
	out := make([]portHolder, 0, len(items))
	for i := range items {
		o := P(&items[i])
		if o.GetUID() == self {
			continue
		}
		w.latest(o)
		out = append(out, portHolder{key: client.ObjectKeyFromObject(o), port: port(o)})
	}
	return out
}

// IndexGatewayPort indexes Exports and Imports by the gateway port they hold. An object that holds none has no entry.
const IndexGatewayPort = "status.gatewayPort"

// gatewayPortIndex is the value of IndexGatewayPort.
func gatewayPortIndex(o client.Object) []string {
	var port int32
	switch v := o.(type) {
	case *straitv1.Export:
		port = v.Status.GatewayPort
	case *straitv1.Import:
		port = v.Status.GatewayPort
	}
	if port == 0 {
		return nil
	}
	return []string{strconv.Itoa(int(port))}
}

// nextPort returns the port that obj holds next, as gatewayPort does. While obj's port is in rng, it first reads only
// the other objects that hold that port: through the cache's index, and, while the cache predates this controller's
// last status write of an object, as that write set it. Only when obj does not keep its port does it list every
// object of its kind, so that a reconcile that keeps a port costs no list of the kind (issue #46, row 37).
func nextPort[T any, P interface {
	*T
	client.Object
}](ctx context.Context, r client.Reader, rng straitv1.PortRange, obj P, w *written[T, P], port func(P) int32, list func(...client.ListOption) ([]T, error)) (int32, error) {
	current := port(obj)
	if current >= rng.First && current <= rng.Last {
		at, err := list(client.MatchingFields{IndexGatewayPort: strconv.Itoa(int(current))})
		if err != nil {
			return 0, err
		}
		holders := otherPorts(at, obj.GetUID(), w, port)
		for _, key := range w.keys(func(o P) bool { return port(o) == current }) {
			if key == client.ObjectKeyFromObject(obj) || slices.ContainsFunc(holders, func(h portHolder) bool { return h.key == key }) {
				continue
			}
			o := P(new(T))
			if err := r.Get(ctx, key, o); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return 0, err
			}
			w.latest(o)
			holders = append(holders, portHolder{key: key, port: port(o)})
		}
		first := slices.ContainsFunc(holders, func(h portHolder) bool {
			return h.port == current && comesFirst(h.key, client.ObjectKeyFromObject(obj))
		})
		if !first {
			return current, nil
		}
	}
	all, err := list()
	if err != nil {
		return 0, err
	}
	return gatewayPort(ctx, rng, obj, current, otherPorts(all, obj.GetUID(), w, port)), nil
}

// comesFirst orders two holders of one port as the gateway does: the lower namespace, then the lower name, keeps it.
func comesFirst(a, b types.NamespacedName) bool {
	return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name)) < 0
}

// gatewayPort returns the port that obj holds next. Obj keeps port while it is in rng and no other object that comes
// first holds it. Otherwise obj gets the lowest port of rng that no other object holds, or 0 when there is none.
func gatewayPort(ctx context.Context, rng straitv1.PortRange, obj client.Object, port int32, others []portHolder) int32 {
	key := client.ObjectKeyFromObject(obj)
	used := map[int32]bool{}
	var holder *types.NamespacedName
	for _, o := range others {
		used[o.port] = true
		if o.port == port && comesFirst(o.key, key) {
			holder = &o.key
		}
	}
	inRange := port >= rng.First && port <= rng.Last
	if inRange && holder == nil {
		return port
	}
	next := allocate(rng, used)
	if inRange {
		log.FromContext(ctx).Info("gateway port moved: another object holds it and comes first", "from", port, "to", next, "holder", holder.String())
	}
	return next
}

// allocate returns the lowest port of rng that is not used, or 0.
func allocate(rng straitv1.PortRange, used map[int32]bool) int32 {
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

// SetupWithManager registers the reconciler and its index.
func (r *ExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &straitv1.Export{}, IndexGatewayPort, gatewayPortIndex); err != nil {
		return err
	}
	inNamespace := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var list straitv1.ExportList
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
		For(&straitv1.Export{}).
		Watches(&discoveryv1.EndpointSlice{}, inNamespace).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// ImportReconciler allocates each Import's gateway port, reads the peer's export port,
// and keeps the Service of an Import that sets spec.port.
type ImportReconciler struct {
	client.Client
	Reader client.Reader
	Peers  Peers
	Now    func() time.Time
	// Retry is the time between two tries to write the Service and the EndpointSlice of an Import whose last try
	// failed: resync when zero (RFC#0005, requirement 51).
	Retry time.Duration

	// written keeps what the last status write of each Import returned, until the cache shows it.
	written written[straitv1.Import, *straitv1.Import]
	// failed keeps the last sync error of each Import, until a sync succeeds or the Import is gone.
	failed lastErrors
}

// lastErrors keeps the last error of each object, so that a retry that fails the same way logs at V(1).
type lastErrors struct {
	mu   sync.Mutex
	last map[types.NamespacedName]string
}

// changed records err as the last error of key, and reports whether it differs from the one before: true on the
// first failure after a success.
func (l *lastErrors) changed(key types.NamespacedName, err error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last[key] == err.Error() {
		return false
	}
	if l.last == nil {
		l.last = map[types.NamespacedName]string{}
	}
	l.last[key] = err.Error()
	return true
}

// forget drops the last error of key.
func (l *lastErrors) forget(key types.NamespacedName) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, key)
}

// Reconcile implements reconcile.Reconciler.
func (r *ImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var imp straitv1.Import
	if err := r.Get(ctx, req.NamespacedName, &imp); err != nil {
		if apierrors.IsNotFound(err) {
			r.written.forget(req.NamespacedName)
			r.failed.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.written.latest(&imp)
	site, err := LocalSite(ctx, r.Reader)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, err
	}
	old := imp.Status.DeepCopy()

	rng := site.Spec.Gateway.ImportPorts
	imp.Status.GatewayPort, err = nextPort(ctx, r.Client, rng, &imp, &r.written, importPort, func(opts ...client.ListOption) ([]straitv1.Import, error) {
		var list straitv1.ImportList
		err := r.List(ctx, &list, opts...)
		return list.Items, err
	})
	if err != nil {
		return ctrl.Result{}, err
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

	var syncErr error
	if imp.Spec.Port == nil {
		meta.RemoveStatusCondition(&imp.Status.Conditions, ConditionService)
		syncErr = r.deleteOwned(ctx, &imp, &discoveryv1.EndpointSlice{}, &corev1.Service{})
	} else {
		cond := metav1.Condition{Type: ConditionService, Status: metav1.ConditionTrue, Reason: "Synced", ObservedGeneration: imp.Generation}
		var conflict bool
		conflict, syncErr = r.syncService(ctx, &imp, site.Status.Gateway.Address)
		switch {
		case syncErr != nil:
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "SyncFailed", syncErr.Error()
		case conflict:
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "ServiceConflict", fmt.Sprintf("a Service named %s exists and Strait does not own it", imp.Name)
		}
		meta.SetStatusCondition(&imp.Status.Conditions, cond)
	}

	if !equalStatus(old, &imp.Status) {
		if _, err := r.written.updateStatus(ctx, r.Client, &imp); err != nil {
			return ctrl.Result{}, err
		}
	}
	if syncErr != nil {
		// Not returned: the controller's backoff would wait up to about 16 minutes between tries.
		retry := cmp.Or(r.Retry, resync)
		// An error only when it is new: a permanent one would log every retry. The SyncFailed condition holds it.
		if r.failed.changed(req.NamespacedName, syncErr) {
			log.FromContext(ctx).Error(syncErr, "Import Service not synced", "retry", retry.String())
		} else {
			log.FromContext(ctx).V(1).Info("Import Service not synced", "error", syncErr, "retry", retry.String())
		}
		return ctrl.Result{RequeueAfter: retry}, nil
	}
	r.failed.forget(req.NamespacedName)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// syncService keeps the Service named after an Import that sets spec.port, and its EndpointSlice:
// the active gateway's address on the import's gateway port. It reports a conflict when another owner holds the name.
func (r *ImportReconciler) syncService(ctx context.Context, imp *straitv1.Import, gateway string) (bool, error) {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(imp), &existing)
	switch {
	case err == nil && !metav1.IsControlledBy(&existing, imp):
		// A slice left from before would add the gateway to the other owner's Service.
		return true, r.deleteOwned(ctx, imp, &discoveryv1.EndpointSlice{})
	case err != nil && !apierrors.IsNotFound(err):
		return false, err
	}
	port := *imp.Spec.Port
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: imp.Name, Namespace: imp.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		// Only local pods reach an import.
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = nil
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       port.Name,
			Port:       port.Port,
			TargetPort: intstr.FromInt32(port.Port),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(imp, svc, r.Scheme())
	}); err != nil {
		return false, fmt.Errorf("service %s: %w", imp.Name, err)
	}
	es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: imp.Name, Namespace: imp.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Labels = map[string]string{discoveryv1.LabelServiceName: imp.Name, LabelManagedBy: ManagedBy}
		if es.AddressType == "" {
			es.AddressType = addressType([]string{gateway})
		}
		es.Ports, es.Endpoints = nil, nil
		if p := imp.Status.GatewayPort; p > 0 {
			es.Ports = []discoveryv1.EndpointPort{{Name: ptr.To(port.Name), Port: ptr.To(p), Protocol: ptr.To(corev1.ProtocolTCP)}}
			if gateway != "" && addressType([]string{gateway}) == es.AddressType {
				es.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{gateway}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}
			}
		}
		return controllerutil.SetControllerReference(imp, es, r.Scheme())
	}); err != nil {
		return false, fmt.Errorf("endpointslice %s: %w", imp.Name, err)
	}
	return false, nil
}

// deleteOwned deletes the objects named after the Import that the Import owns, and leaves the others. In a namespace
// where the controller reads no Services and EndpointSlices, it writes none either, so there is none to delete.
func (r *ImportReconciler) deleteOwned(ctx context.Context, imp *straitv1.Import, objs ...client.Object) error {
	for _, o := range objs {
		if err := r.Get(ctx, client.ObjectKeyFromObject(imp), o); err != nil {
			if apierrors.IsNotFound(err) || errors.Is(err, ErrNotRead) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(o, imp) {
			continue
		}
		if err := r.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// SetupWithManager registers the reconciler. Any value on changes, and any change of the Site, requeues every Import.
// The Site's status names the active gateway, which an Import's Service points at.
func (r *ImportReconciler) SetupWithManager(mgr ctrl.Manager, changes <-chan event.GenericEvent) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &straitv1.Import{}, IndexGatewayPort, gatewayPortIndex); err != nil {
		return err
	}
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list straitv1.ImportList
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
		For(&straitv1.Import{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Watches(&straitv1.Site{}, all).
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
