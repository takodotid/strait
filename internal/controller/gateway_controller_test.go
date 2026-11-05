package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
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

// laggingGateway is a gateway reconciler of version v1.1.2 whose cache shows site a as it was before the first write.
func laggingGateway(t *testing.T) (*GatewayReconciler, *laggingCache, client.Client) {
	t.Helper()
	s := gatewaySite(straitv1.ServiceExposure{})
	s.Status.ControllerVersion = "v1.1.1"
	c := fakeClient(t, append(activeGatewayPod(), s)...)
	cache := &laggingCache{Client: c}
	cache.freeze(t, &straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}})
	return &GatewayReconciler{Client: cache, Reader: cache, Namespace: gatewayNamespace, Version: "v1.1.2"}, cache, c
}

func reconcileGateway(t *testing.T, r *GatewayReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
}

func siteStatus(t *testing.T, c client.Client) straitv1.SiteStatus {
	t.Helper()
	var s straitv1.Site
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &s); err != nil {
		t.Fatal(err)
	}
	return s.Status
}

// gatewayNotReady makes the gateway pod not ready.
func gatewayNotReady(t *testing.T, c client.Client) {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: gatewayNamespace, Name: "gateway-0"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
}

// A cache that has not yet seen the controller's last write of the Site's status does not make the next write
// conflict (issue #23).
func TestSiteStatusWriteOnLaggingCache(t *testing.T) {
	r, cache, c := laggingGateway(t)
	// The first reconcile after a start writes the version. The second has nothing new to write.
	reconcileGateway(t, r)
	reconcileGateway(t, r)
	// The third writes over the first.
	gatewayNotReady(t, c)
	reconcileGateway(t, r)
	st := siteStatus(t, c)
	if st.ControllerVersion != "v1.1.2" || st.Gateway.Address != "" || !meta.IsStatusConditionFalse(st.Conditions, "GatewayActive") {
		t.Fatalf("status = %+v", st)
	}

	cache.thaw()
	reconcileGateway(t, r)
	if len(r.written.last) != 0 {
		t.Fatalf("written = %+v, want it dropped once the cache shows the last write", r.written.last)
	}
}

// A status write that conflicts with another writer's change of the Site is left to the next reconcile, not returned
// as an error, and keeps that change. The gateway agent writes the Site's public key that way.
func TestSiteStatusWriteConflict(t *testing.T) {
	r, cache, c := laggingGateway(t)
	reconcileGateway(t, r)

	// The gateway agent publishes the public key with a merge patch.
	var s straitv1.Site
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &s); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(s.DeepCopy())
	s.Status.WireGuardPublicKey = "key-a"
	if err := c.Status().Patch(context.Background(), &s, patch); err != nil {
		t.Fatal(err)
	}

	gatewayNotReady(t, c)
	reconcileGateway(t, r)
	if st := siteStatus(t, c); st.WireGuardPublicKey != "key-a" || st.Gateway.Address != "10.0.9.9" {
		t.Fatalf("written over another writer's change: status = %+v", st)
	}

	cache.thaw()
	reconcileGateway(t, r)
	if st := siteStatus(t, c); st.WireGuardPublicKey != "key-a" || st.Gateway.Address != "" {
		t.Fatalf("next reconcile: status = %+v", st)
	}
}

// skewedLease is the gateway Lease of a holder whose clock is off by skew, with a 6s duration.
func skewedLease(t *testing.T, skew time.Duration) (*GatewayReconciler, client.Client, *time.Time) {
	t.Helper()
	objs := activeGatewayPod()
	lease := objs[0].(*coordinationv1.Lease)
	lease.Spec.LeaseDurationSeconds = ptr.To[int32](6)
	lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().Add(skew)}
	c := fakeClient(t, append(objs, gatewaySite(straitv1.ServiceExposure{}))...)
	clock := time.Now()
	return &GatewayReconciler{Client: c, Reader: c, Namespace: gatewayNamespace, Now: func() time.Time { return clock }}, c, &clock
}

// renew renews the gateway Lease, on the holder's clock.
func renew(t *testing.T, c client.Client, by time.Duration) {
	t.Helper()
	var lease coordinationv1.Lease
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: gatewayNamespace, Name: GatewayLease}, &lease); err != nil {
		t.Fatal(err)
	}
	lease.Spec.RenewTime = &metav1.MicroTime{Time: lease.Spec.RenewTime.Add(by)}
	if err := c.Update(context.Background(), &lease); err != nil {
		t.Fatal(err)
	}
}

func gatewayAddress(t *testing.T, r *GatewayReconciler, c client.Client) (string, ctrl.Result) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return siteStatus(t, c).Gateway.Address, res
}

// The gateway Lease expires by the controller's clock, counted from when the controller saw it renewed. A holder whose
// clock is behind keeps the Lease while it renews, and one whose clock is ahead loses it once it stops (issue #34).
func TestGatewayLeaseOnTheControllersClock(t *testing.T) {
	t.Run("holder an hour behind", func(t *testing.T) {
		r, c, clock := skewedLease(t, -time.Hour)
		if addr, res := gatewayAddress(t, r, c); addr != "10.0.9.9" || res.RequeueAfter > 6*time.Second+time.Second/10 {
			t.Fatalf("just renewed: address %q, requeue after %s; want 10.0.9.9 within 6s", addr, res.RequeueAfter)
		}
		// Each renewal is an event that the controller reconciles.
		*clock = clock.Add(5 * time.Second)
		renew(t, c, 5*time.Second)
		if addr, _ := gatewayAddress(t, r, c); addr != "10.0.9.9" {
			t.Fatalf("at a renewal: address %q, want 10.0.9.9", addr)
		}
		*clock = clock.Add(5 * time.Second)
		if addr, _ := gatewayAddress(t, r, c); addr != "10.0.9.9" {
			t.Fatalf("5s after a renewal: address %q, want 10.0.9.9", addr)
		}
		*clock = clock.Add(2 * time.Second)
		if addr, _ := gatewayAddress(t, r, c); addr != "" || !meta.IsStatusConditionFalse(siteStatus(t, c).Conditions, "GatewayActive") {
			t.Fatalf("7s after the last renewal: address %q, want none", addr)
		}
	})
	t.Run("holder an hour ahead", func(t *testing.T) {
		r, c, clock := skewedLease(t, time.Hour)
		if addr, _ := gatewayAddress(t, r, c); addr != "10.0.9.9" {
			t.Fatalf("just renewed: address %q, want 10.0.9.9", addr)
		}
		*clock = clock.Add(7 * time.Second)
		if addr, _ := gatewayAddress(t, r, c); addr != "" {
			t.Fatalf("7s with no renewal: address %q, want none", addr)
		}
	})
}

// The direct Service lists each port once, also when two Exports hold one port, as a hand edit or a deposed leader can
// leave them, and when an Export holds the control port. The API server refuses a Service that lists a port twice, and
// the Service would then take no new export (issue #30, row 35).
func TestDirectServiceListsEachPortOnce(t *testing.T) {
	export := func(ns, name string, port int32) *straitv1.Export {
		e := &straitv1.Export{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		e.Status.GatewayPort = port
		return e
	}
	objs := append(activeGatewayPod(), gatewaySite(straitv1.ServiceExposure{Type: "NodePort"}),
		export("app", "b", 30901), export("app", "a", 30901), export("web", "c", 30902), export("web", "d", 30900))
	c := runGateway(t, objs...)
	var ports []int32
	for _, p := range directService(t, c).Spec.Ports {
		ports = append(ports, p.Port)
	}
	if !slices.Equal(ports, []int32{30900, 30901, 30902}) {
		t.Errorf("Service ports = %v, want [30900 30901 30902]", ports)
	}
	var es discoveryv1.EndpointSlice
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: gatewayNamespace, Name: GatewayDirectService}, &es); err != nil {
		t.Fatal(err)
	}
	if len(es.Ports) != 3 {
		t.Errorf("EndpointSlice ports = %d, want 3", len(es.Ports))
	}
}

// gatewayPod is a ready pod of the chart's gateway, with the deletion cost given, or none when cost is "".
func gatewayPod(name, cost string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNamespace, Name: name,
			Labels: map[string]string{LabelName: "strait", LabelComponent: ComponentGateway, "pod-template-hash": "abc"}},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "agent", Image: "strait:1"}}},
		Status: corev1.PodStatus{
			PodIP:      "10.0.9.9",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	if cost != "" {
		p.Annotations = map[string]string{AnnotationDeletionCost: cost, "other": "x"}
	}
	return p
}

// The active gateway pod, and only it, has a deletion cost, so a rollout deletes the standbys first and moves the
// gateway Lease once. Nothing else on a pod changes, and no pod but the gateway's is touched (RFC#0005, requirement
// 39).
func TestDeletionCostOnTheActivePod(t *testing.T) {
	controllerPod := gatewayPod("strait-controller-0", "5")
	controllerPod.Labels[LabelComponent] = "controller"
	lease := activeGatewayPod()[0].(*coordinationv1.Lease)
	c := fakeClient(t, gatewaySite(straitv1.ServiceExposure{}), lease, gatewayPod("gateway-0", ""), gatewayPod("gateway-1", "1"), controllerPod)
	r := &GatewayReconciler{Client: c, Reader: c, Namespace: gatewayNamespace}
	pods := func() map[string]corev1.Pod {
		var list corev1.PodList
		if err := c.List(context.Background(), &list); err != nil {
			t.Fatal(err)
		}
		out := map[string]corev1.Pod{}
		for _, p := range list.Items {
			out[p.Name] = p
		}
		return out
	}
	before := pods()
	check := func(active string) {
		t.Helper()
		got := pods()
		for name, p := range got {
			want := ""
			switch name {
			case active:
				want = "1"
			case controllerPod.Name:
				want = "5"
			}
			if cost := p.Annotations[AnnotationDeletionCost]; cost != want {
				t.Errorf("active %s: pod %s has deletion cost %q, want %q", active, name, cost, want)
			}
			b := before[name]
			if p.Annotations["other"] != b.Annotations["other"] || !equality.Semantic.DeepEqual(p.Labels, b.Labels) ||
				!equality.Semantic.DeepEqual(p.Spec, b.Spec) || !equality.Semantic.DeepEqual(p.Status, b.Status) {
				t.Errorf("pod %s changed beyond its deletion cost: %+v", name, p)
			}
		}
	}
	reconcileGateway(t, r)
	check("gateway-0")

	lease.Spec.HolderIdentity = ptr.To("gateway-1")
	if err := c.Update(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	reconcileGateway(t, r)
	check("gateway-1")

	lease.Spec.HolderIdentity = ptr.To("")
	if err := c.Update(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	reconcileGateway(t, r)
	check("")
}

// The gateway Services' EndpointSlice has the address type of the active pod's address. An EndpointSlice's address
// type cannot change, so a slice of the other family is replaced (issue #45).
func TestGatewaySliceFollowsTheAddressFamily(t *testing.T) {
	objs := activeGatewayPod()
	pod := objs[1].(*corev1.Pod)
	pod.Status.PodIP = "fd00::9"
	c := fakeClient(t, append(objs, gatewaySite(straitv1.ServiceExposure{}))...)
	r := &GatewayReconciler{Client: c, Reader: c, Namespace: gatewayNamespace}
	check := func(at discoveryv1.AddressType, addr string) {
		t.Helper()
		for _, name := range []string{GatewayDirectService} {
			var es discoveryv1.EndpointSlice
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: gatewayNamespace, Name: name}, &es); err != nil {
				t.Fatal(err)
			}
			if es.AddressType != at || len(es.Endpoints) != 1 || es.Endpoints[0].Addresses[0] != addr {
				t.Errorf("%s: %s slice with endpoints %+v, want %s with %s", name, es.AddressType, es.Endpoints, at, addr)
			}
		}
		if cond := meta.FindStatusCondition(siteStatus(t, c).Conditions, "GatewayServices"); cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("GatewayServices = %+v", cond)
		}
	}
	reconcileGateway(t, r)
	check(discoveryv1.AddressTypeIPv6, "fd00::9")

	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.PodIP = "10.0.9.9"
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconcileGateway(t, r)
	check(discoveryv1.AddressTypeIPv4, "10.0.9.9")
}
