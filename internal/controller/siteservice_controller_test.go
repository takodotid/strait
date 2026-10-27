package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/exchange"
	"github.com/takodotid/strait/internal/observe"
)

var now = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

// peers is a fixed view of the exchange.
type peers struct {
	fresh map[string]bool
	view  map[string]observe.Report
}

func (p peers) Fresh(peer string, _ time.Time, _ time.Duration) bool { return p.fresh[peer] }

func (p peers) View(_, _ string, _ []v1alpha1.Member, _ time.Time, _ time.Duration) map[string]observe.Report {
	return p.view
}

func (p peers) Snapshot(string) (exchange.Document, exchange.PeerState) {
	return exchange.Document{}, exchange.PeerState{}
}

func site(name, gateway string) *v1alpha1.Site {
	s := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
	s.Spec.Exchange.PeerTimeout = metav1.Duration{Duration: 6 * time.Second}
	s.Status.Gateway.Address = gateway
	return s
}

func appService(name string, addresses ...string) []client.Object {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
	es := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "app", Name: name + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: name}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To[int32](9090)}},
	}
	for _, a := range addresses {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{a}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	}
	return []client.Object{svc, es}
}

func webService(active string) *v1alpha1.SiteService {
	return &v1alpha1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web", UID: "uid-web"},
		Spec: v1alpha1.SiteServiceSpec{
			Mode:   v1alpha1.ModeSingle,
			Port:   v1alpha1.ServicePort{Name: "http", Port: 8080},
			Plugin: v1alpha1.PluginSpec{Declared: &v1alpha1.DeclaredPlugin{Active: active}},
			Members: []v1alpha1.Member{
				{Name: "web-a", Site: "a", Service: &v1alpha1.ServicePortRef{Name: "web-a", Port: 8080}},
				{Name: "web-b", Site: "b", Service: &v1alpha1.ServicePortRef{Name: "web-b", Port: 8080}},
			},
			Policy: v1alpha1.Policy{Multiple: v1alpha1.RouteNowhere, UnknownHold: metav1.Duration{Duration: 10 * time.Second}},
		},
	}
}

func reconciler(t *testing.T, p peers, objs ...client.Object) (*SiteServiceReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.SiteService{}, &v1alpha1.Import{}).Build()
	return &SiteServiceReconciler{
		Client:   c,
		Reader:   c,
		Recorder: events.NewFakeRecorder(100),
		Peers:    p,
		Local:    &exchange.Local{},
		Now:      func() time.Time { return now },
	}, c
}

func run(t *testing.T, r *SiteServiceReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web"}}); err != nil {
		t.Fatal(err)
	}
}

func endpointSlices(t *testing.T, c client.Client) map[string]discoveryv1.EndpointSlice {
	t.Helper()
	var list discoveryv1.EndpointSliceList
	if err := c.List(context.Background(), &list, client.InNamespace("app"), client.MatchingLabels{LabelSiteService: "web"}); err != nil {
		t.Fatal(err)
	}
	out := map[string]discoveryv1.EndpointSlice{}
	for _, s := range list.Items {
		out[s.Labels[LabelMember]] = s
	}
	return out
}

func status(t *testing.T, c client.Client) v1alpha1.SiteServiceStatus {
	t.Helper()
	var ss v1alpha1.SiteService
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &ss); err != nil {
		t.Fatal(err)
	}
	return ss.Status
}

func TestLocalActiveMember(t *testing.T) {
	objs := append(appService("web-a", "10.0.0.1", "10.0.0.2"), site("a", "10.0.9.9"), webService("web-a"))
	r, c := reconciler(t, peers{}, objs...)
	run(t, r)

	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Selector != nil || svc.Spec.Ports[0].Port != 8080 || !metav1.IsControlledBy(&svc, webService("")) {
		t.Fatalf("service = %+v", svc.Spec)
	}
	es := endpointSlices(t, c)["web-a"]
	if len(es.Endpoints) != 2 || *es.Ports[0].Port != 9090 || es.Labels[discoveryv1.LabelServiceName] != "web" || es.Labels[LabelManagedBy] != ManagedBy {
		t.Fatalf("slice = %+v", es)
	}
	st := status(t, c)
	if st.Route.Reason != v1alpha1.ReasonActive || len(st.Route.Members) != 1 || st.Route.Members[0] != "web-a" || st.Route.Endpoints != 2 {
		t.Fatalf("route = %+v", st.Route)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionRouted) {
		t.Fatalf("conditions = %+v", st.Conditions)
	}
	if st.Members[0].Path != v1alpha1.PathLocal || st.Members[1].Path != "" {
		t.Fatalf("members = %+v", st.Members)
	}
	var exp v1alpha1.Export
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-a"}, &exp); err != nil {
		t.Fatalf("export: %v", err)
	}
	var imp v1alpha1.Import
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-b"}, &imp); err != nil || imp.Spec.Site != "b" || imp.Spec.Export != "web-web-b" {
		t.Fatalf("import: %v %+v", err, imp.Spec)
	}
}

func TestRemoteActiveMemberThroughGateway(t *testing.T) {
	imp := &v1alpha1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: v1alpha1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	p := peers{
		fresh: map[string]bool{"b": true},
		view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
			"web-b": {Member: "web-b", Site: "b", Role: v1alpha1.RolePrimary, Eligible: true, State: v1alpha1.StateStable, ObservedAt: now},
		}}},
	}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-b"), imp)
	r, c := reconciler(t, p, objs...)
	run(t, r)

	es := endpointSlices(t, c)
	if len(es) != 1 {
		t.Fatalf("slices = %v", es)
	}
	got := es["web-b"]
	if len(got.Endpoints) != 1 || got.Endpoints[0].Addresses[0] != "10.0.9.9" || *got.Ports[0].Port != 15001 {
		t.Fatalf("slice = %+v", got)
	}
	if st := status(t, c); st.Route.Reason != v1alpha1.ReasonActive || st.Route.Members[0] != "web-b" || st.Members[1].Path != v1alpha1.PathGateway {
		t.Fatalf("status = %+v", st)
	}
}

func TestRemoteActiveMemberEndpointDirect(t *testing.T) {
	imp := &v1alpha1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: v1alpha1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	link := &v1alpha1.SiteLink{
		ObjectMeta: metav1.ObjectMeta{Name: "b-direct"},
		Spec:       v1alpha1.SiteLinkSpec{Peer: "b", Transport: v1alpha1.TransportDirect, Direct: &v1alpha1.DirectLink{Address: "172.31.0.22", EndpointDirect: true}},
	}
	link.Status.Active, link.Status.Healthy = true, true
	p := peers{
		fresh: map[string]bool{"b": true},
		view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
			"web-b": {Member: "web-b", Site: "b", Role: v1alpha1.RolePrimary, Eligible: true, State: v1alpha1.StateStable, ObservedAt: now,
				Endpoint: observe.Endpoint{Addresses: []string{"10.1.0.7"}, Port: 9090}},
		}}},
	}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-b"), imp, link)
	r, c := reconciler(t, p, objs...)
	run(t, r)

	got := endpointSlices(t, c)["web-b"]
	if len(got.Endpoints) != 1 || got.Endpoints[0].Addresses[0] != "10.1.0.7" || *got.Ports[0].Port != 9090 {
		t.Fatalf("slice = %+v", got)
	}
	if st := status(t, c); st.Members[1].Path != v1alpha1.PathEndpointDirect {
		t.Fatalf("members = %+v", st.Members)
	}
}

func TestDisagreeRoutesNowhere(t *testing.T) {
	p := peers{
		fresh: map[string]bool{"b": true},
		view:  map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{}}},
	}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))
	r, c := reconciler(t, p, objs...)
	run(t, r)
	if es := endpointSlices(t, c); len(es) != 0 {
		t.Fatalf("slices = %v", es)
	}
	st := status(t, c)
	if st.Route.Reason != v1alpha1.ReasonDeclarationsDisagree || len(st.Route.Members) != 0 ||
		!meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionDeclarationsDisagree) ||
		meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionRouted) {
		t.Fatalf("status = %+v", st)
	}
}

func TestUnknownHeldThenNowhere(t *testing.T) {
	ss := webService("web-b")
	ss.Status.Route.Members = []string{"web-b"}
	ss.Status.Members = []v1alpha1.MemberStatus{{Name: "web-b", Site: "b", LastConfirmedTime: &metav1.Time{Time: now.Add(-5 * time.Second)}}}
	imp := &v1alpha1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: v1alpha1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss, imp)
	r, c := reconciler(t, peers{}, objs...)
	run(t, r)
	if st := status(t, c); st.Route.Reason != v1alpha1.ReasonHeldUnknown || len(endpointSlices(t, c)) != 1 {
		t.Fatalf("route = %+v", st.Route)
	}

	r.Now = func() time.Time { return now.Add(6 * time.Second) }
	run(t, r)
	if st := status(t, c); st.Route.Reason != v1alpha1.ReasonUnknownTimeout || len(endpointSlices(t, c)) != 0 {
		t.Fatalf("route = %+v", st.Route)
	}
}

func TestServiceConflict(t *testing.T) {
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"), other)
	r, c := reconciler(t, peers{}, objs...)
	run(t, r)
	if es := endpointSlices(t, c); len(es) != 0 {
		t.Fatalf("slices = %v", es)
	}
	cond := meta.FindStatusCondition(status(t, c).Conditions, v1alpha1.ConditionRouted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServiceConflict" {
		t.Fatalf("routed = %+v", cond)
	}
}
