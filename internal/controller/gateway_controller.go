package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// Names of the gateway's objects in the gateway's namespace.
const (
	GatewayLease            = "strait-gateway"
	GatewayDirectService    = "strait-gateway-direct"
	GatewayWireGuardService = "strait-gateway-wireguard"
	LabelComponent          = "app.kubernetes.io/component"
	ComponentGateway        = "gateway"
)

// GatewayReconciler finds the active gateway pod and keeps the Services that peers reach the gateway on.
type GatewayReconciler struct {
	client.Client
	Reader client.Reader
	// Namespace is the gateway's namespace.
	Namespace string
}

// Reconcile implements reconcile.Reconciler.
func (r *GatewayReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	site, err := LocalSite(ctx, r.Reader)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, err
	}
	pod, addr, err := r.activeGateway(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	var exports straitv1.ExportList
	if err := r.List(ctx, &exports); err != nil {
		return ctrl.Result{}, err
	}
	direct := []corev1.ServicePort{tcpPort("control", site.Spec.Gateway.ControlPort)}
	for _, e := range exports.Items {
		if p := e.Status.GatewayPort; p > 0 {
			direct = append(direct, tcpPort(fmt.Sprintf("x-%d", p), p))
		}
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
	if err := r.syncService(ctx, GatewayDirectService, site.Spec.Gateway.DirectService, corev1.ServiceTypeClusterIP, direct, addr); err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "DirectServiceFailed", err.Error()
	}
	if err := r.syncService(ctx, GatewayWireGuardService, site.Spec.Gateway.WireGuardService, corev1.ServiceTypeNodePort, wg, addr); err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "WireGuardServiceFailed", err.Error()
	}

	old := site.Status.DeepCopy()
	site.Status.Gateway = straitv1.GatewayStatus{ActivePod: pod, Address: addr}
	meta.SetStatusCondition(&site.Status.Conditions, cond)
	active := metav1.Condition{Type: "GatewayActive", Status: metav1.ConditionTrue, Reason: "LeaseHeld", ObservedGeneration: site.Generation}
	if addr == "" {
		active.Status, active.Reason, active.Message = metav1.ConditionFalse, "NoActiveGateway", "no ready gateway pod holds the gateway lease"
	}
	meta.SetStatusCondition(&site.Status.Conditions, active)
	if !equalStatus(old, &site.Status) {
		if err := r.Status().Update(ctx, site); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// activeGateway returns the pod that holds the gateway lease and its address, when it is running and ready.
func (r *GatewayReconciler) activeGateway(ctx context.Context) (string, string, error) {
	var lease coordinationv1.Lease
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: GatewayLease}, &lease); err != nil {
		return "", "", client.IgnoreNotFound(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return "", "", nil
	}
	if lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil &&
		time.Since(lease.Spec.RenewTime.Time) > time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second {
		return "", "", nil
	}
	name := *lease.Spec.HolderIdentity
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return name, "", nil
		}
		return "", "", err
	}
	if pod.Status.PodIP == "" || !pod.DeletionTimestamp.IsZero() || !podReady(&pod) {
		return name, "", nil
	}
	return name, pod.Status.PodIP, nil
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
func (r *GatewayReconciler) syncService(ctx context.Context, name string, exp straitv1.ServiceExposure, def corev1.ServiceType, ports []corev1.ServicePort, addr string) error {
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
		ps := slices.Clone(ports)
		for i := range ps {
			if typ != corev1.ServiceTypeClusterIP {
				ps[i].NodePort = ps[i].Port
			}
		}
		svc.Spec.Ports = ps
		return nil
	}); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Labels = map[string]string{discoveryv1.LabelServiceName: name, LabelManagedBy: ManagedBy, LabelComponent: ComponentGateway}
		es.AddressType = discoveryv1.AddressTypeIPv4
		es.Ports = nil
		for _, p := range ports {
			es.Ports = append(es.Ports, discoveryv1.EndpointPort{Name: ptr.To(p.Name), Port: ptr.To(p.Port), Protocol: ptr.To(p.Protocol)})
		}
		es.Endpoints = nil
		if addr != "" {
			es.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{addr}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}
		}
		return nil
	})
	return err
}

// SetupWithManager registers the reconciler. Every event maps to one request, since the reconciler handles the one Site.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "gateway"}}}
	})
	inNamespace := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == r.Namespace
	}))
	return ctrl.NewControllerManagedBy(mgr).
		Named("gateway").
		Watches(&straitv1.Site{}, one).
		Watches(&straitv1.Export{}, one).
		Watches(&straitv1.SiteLink{}, one).
		Watches(&coordinationv1.Lease{}, one, inNamespace).
		Watches(&corev1.Pod{}, one, inNamespace).
		Watches(&corev1.Service{}, one, inNamespace).
		Watches(&discoveryv1.EndpointSlice{}, one, inNamespace).
		Complete(r)
}
