package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// Names of the gateway's objects in the gateway's namespace.
const (
	GatewayLease            = "strait-gateway"
	GatewayDirectService    = "strait-gateway-direct"
	GatewayWireGuardService = "strait-gateway-wireguard"
	LabelComponent          = "app.kubernetes.io/component"
	ComponentGateway        = "gateway"
	// LabelName names the application of the chart's pods.
	LabelName = "app.kubernetes.io/name"
	// AnnotationDeletionCost ranks the pods of a ReplicaSet for deletion: a ReplicaSet deletes a pod of lower cost
	// first, among pods that are equally ready. The controller sets it on the active gateway pod only.
	AnnotationDeletionCost = "controller.kubernetes.io/pod-deletion-cost"
	// activeDeletionCost is the deletion cost of the active gateway pod. Every other gateway pod has none, which
	// counts as 0.
	activeDeletionCost = "1"
)

// GatewayReconciler finds the active gateway pod and keeps the Services that peers reach the gateway on.
type GatewayReconciler struct {
	client.Client
	Reader client.Reader
	// Namespace is the gateway's namespace.
	Namespace string
	// Version is the controller's version, written into the Site's status.
	Version string
	// Now is the controller's clock. Nil means time.Now.
	Now func() time.Time
	// ConfigRejections returns one line for each gateway pod and resource type whose last configuration the pod's
	// Envoy rejected. Nil reports none.
	ConfigRejections func() []string

	// written keeps what the last status write of the Site returned, until the cache shows it.
	written written[straitv1.Site, *straitv1.Site]

	mu sync.Mutex
	// lease is the gateway Lease's spec as the controller last saw it, and seen is when the controller saw it change.
	lease coordinationv1.LeaseSpec
	seen  time.Time
}

// Reconcile implements reconcile.Reconciler.
func (r *GatewayReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	site, err := LocalSite(ctx, r.Reader)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, err
	}
	r.written.latest(site)
	pod, addr, node, left, err := r.activeGateway(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	var exports straitv1.ExportList
	if err := r.List(ctx, &exports); err != nil {
		return ctrl.Result{}, err
	}
	// Each port once: the API server refuses a Service that lists a port twice. Of two Exports that hold one port, the
	// first by namespace, then name, keeps it, as the gateway serves it (RFC#0005, requirement 31).
	slices.SortFunc(exports.Items, func(a, b straitv1.Export) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	direct := []corev1.ServicePort{tcpPort("control", site.Spec.Gateway.ControlPort)}
	for _, e := range exports.Items {
		p := e.Status.GatewayPort
		if p <= 0 || slices.ContainsFunc(direct, func(sp corev1.ServicePort) bool { return sp.Port == p }) {
			continue
		}
		direct = append(direct, tcpPort(fmt.Sprintf("x-%d", p), p))
	}
	var links straitv1.SiteLinkList
	if err := r.List(ctx, &links); err != nil {
		return ctrl.Result{}, err
	}
	var wg []corev1.ServicePort
	for _, l := range links.Items {
		if l.Spec.Transport == straitv1.TransportWireGuard && l.Spec.WireGuard != nil {
			p := l.Spec.WireGuard.ListenPort
			if !slices.ContainsFunc(wg, func(sp corev1.ServicePort) bool { return sp.Port == p }) {
				wg = append(wg, corev1.ServicePort{Name: fmt.Sprintf("wg-%d", p), Port: p, TargetPort: intstr.FromInt32(p), Protocol: corev1.ProtocolUDP})
			}
		}
	}
	sortPorts(direct)
	sortPorts(wg)

	cond := metav1.Condition{Type: "GatewayServices", Status: metav1.ConditionTrue, Reason: "Synced", ObservedGeneration: site.Generation}
	if err := r.syncService(ctx, GatewayDirectService, site.Spec.Gateway.DirectService, corev1.ServiceTypeClusterIP, direct, addr, node); err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "DirectServiceFailed", err.Error()
	}
	if err := r.syncService(ctx, GatewayWireGuardService, site.Spec.Gateway.WireGuardService, corev1.ServiceTypeNodePort, wg, addr, node); err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "WireGuardServiceFailed", err.Error()
	}

	old := site.Status.DeepCopy()
	site.Status.Gateway = straitv1.GatewayStatus{ActivePod: pod, Address: addr}
	site.Status.ControllerVersion = r.Version
	meta.SetStatusCondition(&site.Status.Conditions, cond)
	active := metav1.Condition{Type: "GatewayActive", Status: metav1.ConditionTrue, Reason: "LeaseHeld", ObservedGeneration: site.Generation}
	if addr == "" {
		active.Status, active.Reason, active.Message = metav1.ConditionFalse, "NoActiveGateway", "no ready gateway pod holds the gateway lease"
	}
	meta.SetStatusCondition(&site.Status.Conditions, active)
	meta.SetStatusCondition(&site.Status.Conditions, r.configCondition(site.Generation))
	if !equalStatus(old, &site.Status) {
		if _, err := r.written.updateStatus(ctx, r.Client, site); err != nil {
			return ctrl.Result{}, err
		}
	}
	// A failure here does not end the reconcile: the requeue below must stay, since no event marks the Lease's expiry.
	if err := r.syncDeletionCost(ctx, pod); err != nil {
		log.FromContext(ctx).Error(err, "set the deletion cost of the gateway pods")
	}
	// The Lease expires on the controller's clock, and no event marks it, so look again when it would.
	next := 10 * time.Second
	if left > 0 && left < next {
		next = left + 10*time.Millisecond
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// ConditionGatewayConfig is the Site condition that says whether every gateway Envoy accepted its configuration.
const ConditionGatewayConfig = "GatewayConfig"

// maxRejections is how many rejections the GatewayConfig condition names, each cut to maxRejection bytes.
const (
	maxRejections = 5
	maxRejection  = 256
)

// configCondition is False while a gateway pod's Envoy rejected its last configuration of a type, and True otherwise.
// Envoy keeps the last configuration that it accepted, so the gateway may not follow the latest routes (RFC#0005).
func (r *GatewayReconciler) configCondition(generation int64) metav1.Condition {
	c := metav1.Condition{Type: ConditionGatewayConfig, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: generation}
	var rejected []string
	if r.ConfigRejections != nil {
		rejected = r.ConfigRejections()
	}
	if len(rejected) == 0 {
		return c
	}
	shown := rejected[:min(len(rejected), maxRejections)]
	lines := make([]string, len(shown))
	for i, l := range shown {
		lines[i] = cut(l, maxRejection)
	}
	msg := strings.Join(lines, "; ")
	if more := len(rejected) - len(shown); more > 0 {
		msg += fmt.Sprintf("; and %d more", more)
	}
	c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Rejected", msg
	return c
}

// activeGateway returns the pod that holds the gateway lease, and its address and node when it is running and ready.
// Left is how long the lease stays valid unless it is renewed, or 0 when that is not known.
func (r *GatewayReconciler) activeGateway(ctx context.Context) (name, addr, node string, left time.Duration, err error) {
	var lease coordinationv1.Lease
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: GatewayLease}, &lease); err != nil {
		return "", "", "", 0, client.IgnoreNotFound(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return "", "", "", 0, nil
	}
	left = r.leaseLeft(lease.Spec)
	if left < 0 {
		return "", "", "", 0, nil
	}
	name = *lease.Spec.HolderIdentity
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return name, "", "", left, nil
		}
		return "", "", "", 0, err
	}
	if pod.Status.PodIP == "" || !pod.DeletionTimestamp.IsZero() || !podReady(&pod) {
		return name, "", "", left, nil
	}
	return name, pod.Status.PodIP, pod.Spec.NodeName, left, nil
}

// syncDeletionCost sets the deletion cost of the gateway pod that holds the gateway Lease, and removes it from every
// other gateway pod, so that a rollout or a scale-down of the gateway deletes the standbys first, and moves the Lease
// once (RFC#0005, requirement 39). It changes nothing else on a pod. A pod being deleted is left alone.
func (r *GatewayReconciler) syncDeletionCost(ctx context.Context, holder string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Namespace), client.MatchingLabels{LabelName: "strait", LabelComponent: ComponentGateway}); err != nil {
		return err
	}
	var errs []error
	for i := range pods.Items {
		p := &pods.Items[i]
		cost, has := p.Annotations[AnnotationDeletionCost]
		var value any
		switch {
		case !p.DeletionTimestamp.IsZero():
			continue
		case p.Name == holder && cost != activeDeletionCost:
			value = activeDeletionCost
		case p.Name != holder && has:
			value = nil
		default:
			continue
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationDeletionCost: value}}})
		if err != nil {
			return err
		}
		if err := r.Patch(ctx, p, client.RawPatch(types.MergePatchType, patch)); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// leaseLeft returns how long the gateway lease stays valid unless its spec changes: its duration, counted on the
// controller's clock from when the controller saw the spec change, as client-go's leader election counts it. The
// holder sets renewTime on its own node's clock, so renewTime is never compared with the controller's clock.
// It returns a negative duration once the lease has expired, and 0 when the lease names no duration.
func (r *GatewayReconciler) leaseLeft(spec coordinationv1.LeaseSpec) time.Duration {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen.IsZero() || !equality.Semantic.DeepEqual(r.lease, spec) {
		r.lease, r.seen = *spec.DeepCopy(), now
	}
	if spec.LeaseDurationSeconds == nil {
		return 0
	}
	left := r.seen.Add(time.Duration(*spec.LeaseDurationSeconds) * time.Second).Sub(now)
	if left == 0 {
		left = -1
	}
	return left
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func tcpPort(name string, port int32) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP}
}

func sortPorts(ports []corev1.ServicePort) {
	slices.SortFunc(ports, func(a, b corev1.ServicePort) int { return int(a.Port - b.Port) })
}

// syncService keeps a selector-less gateway Service and its EndpointSlice, which holds the active gateway pod only.
// A Service with no ports is deleted. An exposure with no type gets def.
// The endpoint names the pod's node: kube-proxy serves externalTrafficPolicy Local only from endpoints on its own node.
func (r *GatewayReconciler) syncService(ctx context.Context, name string, exp straitv1.ServiceExposure, def corev1.ServiceType, ports []corev1.ServicePort, addr, node string) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace}}
	es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace}}
	if len(ports) == 0 {
		for _, o := range []client.Object{es, svc} {
			if err := r.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		return nil
	}
	typ := corev1.ServiceType(exp.Type)
	if typ == "" {
		typ = def
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = map[string]string{LabelComponent: ComponentGateway}
		svc.Annotations = exp.Annotations
		svc.Spec.Selector = nil
		svc.Spec.Type = typ
		setExposure(&svc.Spec, exp)
		ps := slices.Clone(ports)
		// An explicit node port is allocated even with allocateLoadBalancerNodePorts false.
		if typ != corev1.ServiceTypeClusterIP && ptr.Deref(svc.Spec.AllocateLoadBalancerNodePorts, true) {
			for i := range ps {
				ps[i].NodePort = ps[i].Port
			}
		}
		svc.Spec.Ports = ps
		return nil
	}); err != nil {
		return err
	}
	// An EndpointSlice's address type cannot change. A slice of the other family than the active pod's address is
	// deleted, and written again (RFC#0005, requirement 24). The precondition keeps a delete from reaching a slice
	// already written again, while the cache still shows the one before.
	at := addressType([]string{addr})
	if addr != "" {
		var cur discoveryv1.EndpointSlice
		err := r.Get(ctx, client.ObjectKeyFromObject(es), &cur)
		switch {
		case err == nil && cur.AddressType != at:
			if err := r.Delete(ctx, &cur, client.Preconditions{UID: &cur.UID}); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("replace the %s EndpointSlice of %s: %w", cur.AddressType, name, err)
			}
			replaced := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace}, AddressType: at}
			setGatewaySlice(replaced, name, ports, addr, node)
			return r.Create(ctx, replaced)
		case err != nil && !apierrors.IsNotFound(err):
			return err
		}
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		if es.AddressType == "" {
			es.AddressType = at
		}
		setGatewaySlice(es, name, ports, addr, node)
		return nil
	})
	return err
}

// setGatewaySlice sets a gateway Service's EndpointSlice: its ports, and the active pod's address on its node, when
// the address has the slice's address type.
func setGatewaySlice(es *discoveryv1.EndpointSlice, name string, ports []corev1.ServicePort, addr, node string) {
	es.Labels = map[string]string{discoveryv1.LabelServiceName: name, LabelManagedBy: ManagedBy, LabelComponent: ComponentGateway}
	es.Ports = nil
	for _, p := range ports {
		es.Ports = append(es.Ports, discoveryv1.EndpointPort{Name: ptr.To(p.Name), Port: ptr.To(p.Port), Protocol: ptr.To(p.Protocol)})
	}
	es.Endpoints = nil
	if addr != "" && addressType([]string{addr}) == es.AddressType {
		ep := discoveryv1.Endpoint{Addresses: []string{addr}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}
		if node != "" {
			ep.NodeName = ptr.To(node)
		}
		es.Endpoints = []discoveryv1.Endpoint{ep}
	}
}

// setExposure sets the fields of a Service's spec that depend on its type, from exp.
// A field that exp leaves unset gets the API server's default, so a field set once and then unset does not stay.
// A field that does not apply to the type is cleared, since the API server would reject or keep it.
func setExposure(spec *corev1.ServiceSpec, exp straitv1.ServiceExposure) {
	lb := spec.Type == corev1.ServiceTypeLoadBalancer
	spec.LoadBalancerSourceRanges = nil
	if lb {
		spec.LoadBalancerSourceRanges = slices.Clone(exp.LoadBalancerSourceRanges)
	}
	switch {
	case !lb:
		spec.AllocateLoadBalancerNodePorts = nil
	case exp.AllocateLoadBalancerNodePorts != nil:
		spec.AllocateLoadBalancerNodePorts = ptr.To(*exp.AllocateLoadBalancerNodePorts)
	case spec.AllocateLoadBalancerNodePorts != nil:
		spec.AllocateLoadBalancerNodePorts = ptr.To(true)
	}
	switch {
	case spec.Type == corev1.ServiceTypeClusterIP:
		spec.ExternalTrafficPolicy = ""
	case exp.ExternalTrafficPolicy != "":
		spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicy(exp.ExternalTrafficPolicy)
	case spec.ExternalTrafficPolicy != "":
		spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	}
	if !lb || spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		spec.HealthCheckNodePort = 0
	}
}

// SetupWithManager registers the reconciler. Every event maps to one request, since the reconciler handles the one Site.
// Any value on changes, such as a change of ConfigRejections, also requeues it. Changes may be nil.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager, changes <-chan event.GenericEvent) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "gateway"}}}
	})
	inNamespace := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == r.Namespace
	}))
	b := ctrl.NewControllerManagedBy(mgr).
		Named("gateway").
		Watches(&straitv1.Site{}, one).
		Watches(&straitv1.Export{}, one).
		Watches(&straitv1.SiteLink{}, one).
		Watches(&coordinationv1.Lease{}, one, inNamespace).
		Watches(&corev1.Pod{}, one, inNamespace).
		Watches(&corev1.Service{}, one, inNamespace).
		Watches(&discoveryv1.EndpointSlice{}, one, inNamespace)
	if changes != nil {
		b = b.WatchesRawSource(source.Channel(changes, one))
	}
	return b.Complete(r)
}

// cut returns s cut to at most n bytes on a rune boundary, with "..." after a cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
