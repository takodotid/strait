package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

const gatewayNamespace = "strait-system"

// activeGatewayPod is a ready gateway pod on node-1 that holds the gateway lease.
func activeGatewayPod() []client.Object {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: GatewayLease},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("gateway-0"),
			LeaseDurationSeconds: ptr.To[int32](15),
			RenewTime:            &metav1.MicroTime{Time: time.Now()},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: "gateway-0"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			PodIP:      "10.0.9.9",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	return []client.Object{lease, pod}
}

func gatewaySite(direct straitv1.ServiceExposure) *straitv1.Site {
	s := site("a", "")
	s.Spec.Gateway.ControlPort = 30900
	s.Spec.Gateway.DirectService = direct
	return s
}

func runGateway(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	c := fakeClient(t, objs...)
	r := &GatewayReconciler{Client: c, Reader: c, Namespace: gatewayNamespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	return c
}

func directService(t *testing.T, c client.Client) corev1.Service {
	t.Helper()
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: gatewayNamespace, Name: GatewayDirectService}, &svc); err != nil {
		t.Fatal(err)
	}
	return svc
}

// kube-proxy serves externalTrafficPolicy Local only from endpoints whose node is its own.
func TestGatewayEndpointNamesItsNode(t *testing.T) {
	c := runGateway(t, append(activeGatewayPod(), gatewaySite(straitv1.ServiceExposure{Type: "NodePort", ExternalTrafficPolicy: "Local"}))...)
	var es discoveryv1.EndpointSlice
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: gatewayNamespace, Name: GatewayDirectService}, &es); err != nil {
		t.Fatal(err)
	}
	if len(es.Endpoints) != 1 || es.Endpoints[0].Addresses[0] != "10.0.9.9" {
		t.Fatalf("endpoints = %+v", es.Endpoints)
	}
	if node := ptr.Deref(es.Endpoints[0].NodeName, ""); node != "node-1" {
		t.Fatalf("endpoint node = %q, want node-1", node)
	}
	if svc := directService(t, c); svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Fatalf("externalTrafficPolicy = %q", svc.Spec.ExternalTrafficPolicy)
	}
}

func TestGatewayServiceRestricted(t *testing.T) {
	direct := straitv1.ServiceExposure{
		Type:                          "LoadBalancer",
		LoadBalancerSourceRanges:      []string{"192.0.2.21/32"},
		AllocateLoadBalancerNodePorts: ptr.To(false),
		ExternalTrafficPolicy:         "Local",
	}
	svc := directService(t, runGateway(t, append(activeGatewayPod(), gatewaySite(direct))...))
	s := svc.Spec
	if !slices.Equal(s.LoadBalancerSourceRanges, []string{"192.0.2.21/32"}) || ptr.Deref(s.AllocateLoadBalancerNodePorts, true) ||
		s.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Fatalf("loadBalancerSourceRanges = %v, allocateLoadBalancerNodePorts = %v, externalTrafficPolicy = %q; want [192.0.2.21/32], false, Local",
			s.LoadBalancerSourceRanges, ptr.Deref(s.AllocateLoadBalancerNodePorts, true), s.ExternalTrafficPolicy)
	}
	// A node port that is set is allocated even with allocateLoadBalancerNodePorts false.
	for _, p := range svc.Spec.Ports {
		if p.NodePort != 0 {
			t.Fatalf("port %s has node port %d, want none", p.Name, p.NodePort)
		}
	}
}

// Unset fields leave the Service as the API server defaults it, so a field set before does not stay,
// and a field of LoadBalancer does not stay on a NodePort, which the API server would reject.
func TestGatewayServiceUnrestricted(t *testing.T) {
	old := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: GatewayDirectService},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			Ports:                         []corev1.ServicePort{tcpPort("control", 30900)},
			LoadBalancerSourceRanges:      []string{"192.0.2.21/32"},
			AllocateLoadBalancerNodePorts: ptr.To(false),
			ExternalTrafficPolicy:         corev1.ServiceExternalTrafficPolicyLocal,
			HealthCheckNodePort:           32000,
		},
	}
	svc := directService(t, runGateway(t, append(activeGatewayPod(), gatewaySite(straitv1.ServiceExposure{Type: "NodePort"}), old)...))
	s := svc.Spec
	if s.Type != corev1.ServiceTypeNodePort || s.LoadBalancerSourceRanges != nil || s.AllocateLoadBalancerNodePorts != nil ||
		s.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster || s.HealthCheckNodePort != 0 {
		t.Fatalf("type = %s, loadBalancerSourceRanges = %v, allocateLoadBalancerNodePorts set = %t, externalTrafficPolicy = %q, healthCheckNodePort = %d; "+
			"want NodePort, none, false, Cluster, 0",
			s.Type, s.LoadBalancerSourceRanges, s.AllocateLoadBalancerNodePorts != nil, s.ExternalTrafficPolicy, s.HealthCheckNodePort)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].NodePort != 30900 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}

	// A new Service with every field unset is the one that v0.1.1 writes.
	for _, typ := range []corev1.ServiceType{corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer} {
		svc := directService(t, runGateway(t, append(activeGatewayPod(), gatewaySite(straitv1.ServiceExposure{Type: string(typ)}))...))
		want := corev1.ServiceSpec{Type: typ, Ports: []corev1.ServicePort{tcpPort("control", 30900)}}
		if typ != corev1.ServiceTypeClusterIP {
			want.Ports[0].NodePort = 30900
		}
		if !equalStatus(&svc.Spec, &want) {
			t.Errorf("%s: spec = %+v, want %+v", typ, svc.Spec, want)
		}
	}
}

// The elected controller writes its version into the Site's status, and a new version replaces the old one.
func TestGatewayControllerVersion(t *testing.T) {
	c := fakeClient(t, &straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}})
	r := &GatewayReconciler{Client: c, Reader: c, Namespace: gatewayNamespace}

	for _, v := range []string{"v1.0.0", "v1.0.1"} {
		r.Version = v
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatal(err)
		}
		var site straitv1.Site
		if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &site); err != nil {
			t.Fatal(err)
		}
		if site.Status.ControllerVersion != v {
			t.Errorf("controllerVersion: got %q, want %q", site.Status.ControllerVersion, v)
		}
	}
}
