package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/exchange"
	"github.com/takodotid/strait/internal/observe"
	"github.com/takodotid/strait/internal/plugin"
)

var now = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

// peers is a fixed view of the exchange.
type peers struct {
	fresh map[string]bool
	view  map[string]observe.Report
	// read holds the peers read since started, stale or not. A fresh peer has been read too.
	read    map[string]bool
	started time.Time
	// polled is true once the first round of polls has ended.
	polled bool
}

func (p peers) Fresh(peer string, _ time.Time, _ time.Duration) bool { return p.fresh[peer] }

func (p peers) View(_, _ string, _ []straitv1.Member, _ time.Time, _ time.Duration) map[string]observe.Report {
	return p.view
}

func (p peers) Snapshot(peer string) (exchange.Document, exchange.PeerState) {
	if p.read[peer] || p.fresh[peer] {
		return exchange.Document{}, exchange.PeerState{LastSuccess: now}
	}
	return exchange.Document{}, exchange.PeerState{}
}

// readB is an exchange where peer b was read since polling began, and is stale now.
var readB = peers{read: map[string]bool{"b": true}}

func (p peers) Started() time.Time { return p.started }

func (p peers) Polled() bool { return p.polled }

func site(name, gateway string) *straitv1.Site {
	s := &straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
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

func webService(active string) *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web", UID: "uid-web"},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "http", Port: 8080},
			Plugin: straitv1.PluginSpec{Declared: &straitv1.DeclaredPlugin{Active: active}},
			Members: []straitv1.Member{
				{Name: "web-a", Site: "a", Service: &straitv1.ServicePortRef{Name: "web-a", Port: 8080}},
				{Name: "web-b", Site: "b", Service: &straitv1.ServicePortRef{Name: "web-b", Port: 8080}},
			},
			Policy: straitv1.Policy{Multiple: straitv1.RouteNowhere, UnknownHold: metav1.Duration{Duration: 10 * time.Second}},
		},
	}
}

// fakeClient is a client of an API server that holds objs.
func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&straitv1.SiteService{}, &straitv1.Export{}, &straitv1.Import{}, &straitv1.Site{}).
		WithIndex(&straitv1.Export{}, IndexExportService, exportService).
		WithIndex(&straitv1.Export{}, IndexGatewayPort, gatewayPortIndex).
		WithIndex(&straitv1.Import{}, IndexGatewayPort, gatewayPortIndex).
		Build()
}

func reconciler(t *testing.T, p peers, objs ...client.Object) (*SiteServiceReconciler, client.Client) {
	t.Helper()
	c := fakeClient(t, objs...)
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

func status(t *testing.T, c client.Client) straitv1.SiteServiceStatus {
	t.Helper()
	var ss straitv1.SiteService
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &ss); err != nil {
		t.Fatal(err)
	}
	return ss.Status
}

func TestLocalActiveMember(t *testing.T) {
	objs := append(appService("web-a", "10.0.0.1", "10.0.0.2"), site("a", "10.0.9.9"), webService("web-a"))
	r, c := reconciler(t, readB, objs...)
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
	if st.Route.Reason != straitv1.ReasonActive || len(st.Route.Members) != 1 || st.Route.Members[0] != "web-a" || st.Route.Endpoints != 2 {
		t.Fatalf("route = %+v", st.Route)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, straitv1.ConditionRouted) {
		t.Fatalf("conditions = %+v", st.Conditions)
	}
	if st.Members[0].Path != straitv1.PathLocal || st.Members[1].Path != "" {
		t.Fatalf("members = %+v", st.Members)
	}
	var exp straitv1.Export
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-a"}, &exp); err != nil {
		t.Fatalf("export: %v", err)
	}
	var imp straitv1.Import
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-b"}, &imp); err != nil || imp.Spec.Site != "b" || imp.Spec.Export != "web-web-b" {
		t.Fatalf("import: %v %+v", err, imp.Spec)
	}
}

func TestRemoteActiveMemberThroughGateway(t *testing.T) {
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	p := peers{
		fresh: map[string]bool{"b": true},
		view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
			"web-b": {Member: "web-b", Site: "b", Role: straitv1.RolePrimary, Eligible: true, State: straitv1.StateStable, ObservedAt: now},
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
	if st := status(t, c); st.Route.Reason != straitv1.ReasonActive || st.Route.Members[0] != "web-b" || st.Members[1].Path != straitv1.PathGateway {
		t.Fatalf("status = %+v", st)
	}
}

// A gateway handover leaves a member reached through the gateway with no endpoint, from when the active replica stops
// being active until a standby holds the Lease and is ready: the Site has no gateway address (RFC0005, requirement
// 11, and its failover sequence). The route and its reason stay, since the decision has not changed, and the Routed
// condition is False with reason NoEndpoints, since the Service has none (RFC0002, requirement 27). Then the endpoint
// is the new replica's address.
func TestGatewayHandoverLeavesNoEndpoint(t *testing.T) {
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	p := peers{
		fresh: map[string]bool{"b": true},
		view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
			"web-b": {Member: "web-b", Site: "b", Role: straitv1.RolePrimary, Eligible: true, State: straitv1.StateStable, ObservedAt: now},
		}}},
	}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-b"), imp)
	r, c := reconciler(t, p, objs...)
	gateway := func(address string) {
		t.Helper()
		var s straitv1.Site
		if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &s); err != nil {
			t.Fatal(err)
		}
		s.Status.Gateway.Address = address
		if err := c.Status().Update(context.Background(), &s); err != nil {
			t.Fatal(err)
		}
		run(t, r)
	}
	check := func(when, address string, routed metav1.ConditionStatus, reason string) {
		t.Helper()
		var addresses []string
		for _, e := range endpointSlices(t, c)["web-b"].Endpoints {
			addresses = append(addresses, e.Addresses...)
		}
		st := status(t, c)
		cond := meta.FindStatusCondition(st.Conditions, straitv1.ConditionRouted)
		if address == "" && len(addresses) > 0 || address != "" && !slices.Equal(addresses, []string{address}) {
			t.Errorf("%s: endpoints %v, want %q", when, addresses, address)
		}
		if !slices.Equal(st.Route.Members, []string{"web-b"}) || st.Route.Reason != straitv1.ReasonActive {
			t.Errorf("%s: route %v %s, want [web-b] Active", when, st.Route.Members, st.Route.Reason)
		}
		if cond == nil || cond.Status != routed || cond.Reason != reason {
			t.Errorf("%s: Routed %+v, want %s %s", when, cond, routed, reason)
		}
	}

	run(t, r)
	check("before", "10.0.9.9", metav1.ConditionTrue, straitv1.ReasonActive)
	gateway("")
	check("no active replica", "", metav1.ConditionFalse, "NoEndpoints")
	gateway("10.0.9.10")
	check("a standby took the Lease", "10.0.9.10", metav1.ConditionTrue, straitv1.ReasonActive)
}

func TestRemoteActiveMemberEndpointDirect(t *testing.T) {
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	link := &straitv1.SiteLink{
		ObjectMeta: metav1.ObjectMeta{Name: "b-direct"},
		Spec:       straitv1.SiteLinkSpec{Peer: "b", Transport: straitv1.TransportDirect, Direct: &straitv1.DirectLink{Address: "172.31.0.22", EndpointDirect: true}},
	}
	link.Status.Active, link.Status.Healthy = true, true
	p := peers{
		fresh: map[string]bool{"b": true},
		view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
			"web-b": {Member: "web-b", Site: "b", Role: straitv1.RolePrimary, Eligible: true, State: straitv1.StateStable, ObservedAt: now,
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
	if st := status(t, c); st.Members[1].Path != straitv1.PathEndpointDirect {
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
	if st.Route.Reason != straitv1.ReasonDeclarationsDisagree || len(st.Route.Members) != 0 ||
		!meta.IsStatusConditionTrue(st.Conditions, straitv1.ConditionDeclarationsDisagree) ||
		meta.IsStatusConditionTrue(st.Conditions, straitv1.ConditionRouted) {
		t.Fatalf("status = %+v", st)
	}
}

func TestUnknownHeldThenNowhere(t *testing.T) {
	ss := webService("web-b")
	ss.Status.Route.Members = []string{"web-b"}
	ss.Status.Members = []straitv1.MemberStatus{{Name: "web-b", Site: "b", LastConfirmedTime: &metav1.Time{Time: now.Add(-5 * time.Second)}}}
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	// The slice that the previous controller wrote for the route.
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b", Labels: map[string]string{
			discoveryv1.LabelServiceName: "web", LabelManagedBy: ManagedBy, LabelSiteService: "web", LabelMember: "web-b",
		}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.9.9"}}},
	}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss, imp, es)
	r, c := reconciler(t, readB, objs...)
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonHeldUnknown || len(endpointSlices(t, c)) != 1 {
		t.Fatalf("route = %+v", st.Route)
	}

	r.Now = func() time.Time { return now.Add(6 * time.Second) }
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonUnknownTimeout || len(endpointSlices(t, c)) != 0 {
		t.Fatalf("route = %+v", st.Route)
	}
}

// After a restart, a hold counts from the last confirmed time in status, which the previous controller wrote on its own
// node's clock. A time later than now counts as now, so the hold lasts at most unknownHold (RFC0002, requirement 17).
// The status here was written by a v1.2 controller whose clock ran an hour ahead.
func TestHoldAfterRestartWithAClockAhead(t *testing.T) {
	ss := webService("web-b")
	ss.Status.Route.Members = []string{"web-b"}
	ss.Status.Members = []straitv1.MemberStatus{{Name: "web-b", Site: "b", LastConfirmedTime: &metav1.Time{Time: now.Add(time.Hour)}}}
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b", Labels: map[string]string{
			discoveryv1.LabelServiceName: "web", LabelManagedBy: ManagedBy, LabelSiteService: "web", LabelMember: "web-b",
		}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.9.9"}}},
	}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss, imp, es)
	r, c := reconciler(t, readB, objs...)
	run(t, r)
	st := status(t, c)
	if st.Route.Reason != straitv1.ReasonHeldUnknown {
		t.Fatalf("route = %+v, want HeldUnknown", st.Route)
	}
	if got := st.Members[1].LastConfirmedTime; got == nil || !got.Time.Equal(now) {
		t.Errorf("lastConfirmedTime = %v, want now, %v", got, now)
	}

	// unknownHold is 10s.
	r.Now = func() time.Time { return now.Add(10 * time.Second) }
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonUnknownTimeout || len(endpointSlices(t, c)) != 0 {
		t.Fatalf("route = %+v, want UnknownTimeout and no slice, 10s after the restart", st.Route)
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
	cond := meta.FindStatusCondition(status(t, c).Conditions, straitv1.ConditionRouted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServiceConflict" {
		t.Fatalf("routed = %+v", cond)
	}
}

// When another owner takes the name of a Service that Strait wrote, the slices that Strait wrote earlier stop feeding
// it. Once that Service is gone, Strait writes its own again (issue #33).
func TestServiceConflictDeletesSlices(t *testing.T) {
	r, c := reconciler(t, readB, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))...)
	run(t, r)
	if es := endpointSlices(t, c); len(es) != 1 {
		t.Fatalf("before the conflict: slices = %v", es)
	}
	// The Service loses its owner, as when it is deleted and another owner creates one of the same name.
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &svc); err != nil {
		t.Fatal(err)
	}
	svc.OwnerReferences = nil
	if err := c.Update(context.Background(), &svc); err != nil {
		t.Fatal(err)
	}
	run(t, r)
	if m := sliceMembers(context.Background(), t, c); len(m) != 0 {
		t.Fatalf("slices of %v, want none on another owner's Service", m)
	}
	// The route shows the decision, as before: on a conflict, no slice is to be written.
	st := status(t, c)
	if cond := meta.FindStatusCondition(st.Conditions, straitv1.ConditionRouted); cond == nil || cond.Reason != "ServiceConflict" || st.Route.Endpoints != 0 ||
		!slices.Equal(st.Route.Members, []string{"web-a"}) {
		t.Fatalf("route = %+v, routed = %+v", st.Route, cond)
	}

	if err := c.Delete(context.Background(), &svc); err != nil {
		t.Fatal(err)
	}
	run(t, r)
	if es := endpointSlices(t, c); len(es) != 1 || !meta.IsStatusConditionTrue(status(t, c).Conditions, straitv1.ConditionRouted) {
		t.Fatalf("after the other Service is gone: slices = %v, conditions = %+v", es, status(t, c).Conditions)
	}
}

// With a startup hold, a local member is routed only once the peer has been read since polling began, or once the
// hold ends.
func TestStartupHold(t *testing.T) {
	ss := webService("web-a")
	ss.Spec.Policy.StartupHold = metav1.Duration{Duration: time.Minute}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss)
	r, c := reconciler(t, peers{started: now.Add(-5 * time.Second)}, objs...)
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonAwaitingPeers || len(st.Route.Members) != 0 || len(endpointSlices(t, c)) != 0 {
		t.Fatalf("before b is read: route = %+v", st.Route)
	}

	r.Peers = peers{
		started: now.Add(-5 * time.Second),
		read:    map[string]bool{"b": true},
		fresh:   map[string]bool{"b": true},
		view:    map[string]observe.Report{"b": {Declared: "web-a", Members: map[string]observe.Observation{}}},
	}
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonActive || len(endpointSlices(t, c)) != 1 {
		t.Fatalf("once b is read: route = %+v", st.Route)
	}

	// The hold also ends the status at startup, even before the first poll of every peer has ended.
	r, c = reconciler(t, peers{started: now.Add(-time.Minute)}, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss.DeepCopy())...)
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonActive || len(endpointSlices(t, c)) != 1 {
		t.Fatalf("after the hold: route = %+v", st.Route)
	}
}

// After a restart, until the first poll of every peer has ended, the status shows AwaitingPeers in place of the
// decision's reason. Then it shows the decision's reason, also when the poll of a cut-off peer failed. The route and
// the endpoints are the decision's throughout (issue #19).
func TestStatusAtStartup(t *testing.T) {
	ss := webService("web-a")
	// The status from before the restart.
	ss.Status.Route = straitv1.Route{Members: []string{"web-a"}, Reason: straitv1.ReasonActive, LastChangeTime: &metav1.Time{Time: now.Add(-time.Hour)}, Endpoints: 1}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss)
	r, c := reconciler(t, peers{started: now}, objs...)

	for _, after := range []time.Duration{0, 5 * time.Second, 40 * time.Second} {
		r.Now = func() time.Time { return now.Add(after) }
		run(t, r)
		st := status(t, c)
		routed := meta.FindStatusCondition(st.Conditions, straitv1.ConditionRouted)
		if st.Route.Reason != straitv1.ReasonAwaitingPeers || !strings.Contains(st.Route.Message, "site b") || !strings.Contains(st.Route.Message, straitv1.ReasonActive) ||
			!slices.Equal(st.Route.Members, []string{"web-a"}) || st.Route.Endpoints != 1 || len(endpointSlices(t, c)) != 1 ||
			routed == nil || routed.Status != metav1.ConditionTrue || routed.Reason != straitv1.ReasonAwaitingPeers {
			t.Fatalf("%s after the start, first poll not ended: route = %+v, routed = %+v", after, st.Route, routed)
		}
	}

	// The first poll of b failed: b is cut off. The status shows the decision, with web-b Unknown.
	r.Peers = peers{started: now, polled: true}
	r.Now = func() time.Time { return now.Add(41 * time.Second) }
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonActive || !slices.Equal(st.Route.Members, []string{"web-a"}) || st.Members[1].State != straitv1.StateUnknown {
		t.Fatalf("after a failed first poll of b: route = %+v, members = %+v", st.Route, st.Members)
	}

	r.Peers = peers{
		started: now,
		polled:  true,
		fresh:   map[string]bool{"b": true},
		view:    map[string]observe.Report{"b": {Declared: "web-a", Members: map[string]observe.Observation{}}},
	}
	r.Now = func() time.Time { return now.Add(42 * time.Second) }
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonActive || !slices.Equal(st.Route.Members, []string{"web-a"}) {
		t.Fatalf("once b is read: route = %+v", st.Route)
	}
}

func routeChangedEvents(r *SiteServiceReconciler) int {
	n := 0
	for {
		select {
		case e := <-r.Recorder.(*events.FakeRecorder).Events:
			if strings.Contains(e, "RouteChanged") {
				n++
			}
		default:
			return n
		}
	}
}

// A cache that has not yet seen the controller's last status write does not make the next write conflict, and does
// not repeat the route change (issue #18).
func TestStatusWriteOnLaggingCache(t *testing.T) {
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))
	r, c := reconciler(t, readB, objs...)
	cache := &laggingCache{Client: c}
	r.Client = cache
	run(t, r)
	cache.freeze(t, &straitv1.SiteService{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}})

	// The member loses its endpoint. The route moves to nowhere, in a write that the cache never shows.
	var es discoveryv1.EndpointSlice
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-a-x"}, &es); err != nil {
		t.Fatal(err)
	}
	es.Endpoints = nil
	if err := c.Update(context.Background(), &es); err != nil {
		t.Fatal(err)
	}
	run(t, r)
	// The second reconcile is due to refresh the timestamps, so it writes again.
	for _, after := range []time.Duration{time.Second, statusRefresh + time.Second, statusRefresh + 2*time.Second} {
		r.Now = func() time.Time { return now.Add(after) }
		run(t, r)
	}
	st := status(t, c)
	if len(st.Route.Members) != 0 || !st.Route.LastChangeTime.Equal(&metav1.Time{Time: now}) || !st.Members[0].ObservedTime.Equal(&metav1.Time{Time: now.Add(statusRefresh + time.Second)}) {
		t.Fatalf("status = %+v", st)
	}
	if n := routeChangedEvents(r); n != 1 {
		t.Fatalf("RouteChanged events = %d, want 1", n)
	}

	cache.thaw()
	r.Now = func() time.Time { return now.Add(statusRefresh + 3*time.Second) }
	run(t, r)
	if len(r.written.last) != 0 {
		t.Fatalf("written = %+v, want it dropped once the cache shows the last write", r.written.last)
	}
}

// A status write that conflicts with another writer's change is left to the next reconcile, not returned as an error.
func TestStatusWriteConflict(t *testing.T) {
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))
	r, c := reconciler(t, readB, objs...)
	cache := &laggingCache{Client: c}
	r.Client = cache
	cache.freeze(t, &straitv1.SiteService{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}})
	var ss straitv1.SiteService
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &ss); err != nil {
		t.Fatal(err)
	}
	ss.Labels = map[string]string{"edited": "true"}
	if err := c.Update(context.Background(), &ss); err != nil {
		t.Fatal(err)
	}
	run(t, r)
	if st := status(t, c); st.Route.Reason != "" {
		t.Fatalf("written over another writer's change: route = %+v", st.Route)
	}
	if n := routeChangedEvents(r); n != 0 {
		t.Fatalf("RouteChanged events = %d before the status is written", n)
	}

	cache.thaw()
	run(t, r)
	if st := status(t, c); st.Route.Reason != straitv1.ReasonActive {
		t.Fatalf("next reconcile: route = %+v", st.Route)
	}
}

// directPeer is site a with a healthy, active direct link to b with endpointDirect, and b's member web-b at addresses.
func directPeer(t *testing.T, link func(*straitv1.SiteLink), addresses ...string) client.Client {
	t.Helper()
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	l := &straitv1.SiteLink{
		ObjectMeta: metav1.ObjectMeta{Name: "b-direct"},
		Spec:       straitv1.SiteLinkSpec{Peer: "b", Transport: straitv1.TransportDirect, Direct: &straitv1.DirectLink{Address: "172.31.0.22", EndpointDirect: true}},
	}
	l.Status.Active, l.Status.Healthy = true, true
	if link != nil {
		link(l)
	}
	p := peers{
		fresh: map[string]bool{"b": true},
		view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
			"web-b": {Member: "web-b", Site: "b", Role: straitv1.RolePrimary, Eligible: true, State: straitv1.StateStable, ObservedAt: now,
				Endpoint: observe.Endpoint{Addresses: addresses, Port: 9090}},
		}}},
	}
	r, c := reconciler(t, p, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-b"), imp, l)...)
	run(t, r)
	return c
}

func sliceAddresses(es discoveryv1.EndpointSlice) []string {
	var out []string
	for _, e := range es.Endpoints {
		out = append(out, e.Addresses...)
	}
	return out
}

// Endpoint-direct writes only IP addresses from a peer's document, and none that is unspecified, loopback, link-local
// or multicast. Status names what it dropped (issue #36).
func TestEndpointDirectDropsAddresses(t *testing.T) {
	c := directPeer(t, nil, "10.1.0.7", "127.0.0.1", "not-an-ip", "169.254.169.254", "224.0.0.1", "0.0.0.0", "::ffff:10.1.0.8")
	es := endpointSlices(t, c)["web-b"]
	if got := sliceAddresses(es); !slices.Equal(got, []string{"10.1.0.7", "10.1.0.8"}) || *es.Ports[0].Port != 9090 {
		t.Fatalf("addresses = %v, port %d; want [10.1.0.7 10.1.0.8], 9090", got, *es.Ports[0].Port)
	}
	routed := meta.FindStatusCondition(status(t, c).Conditions, straitv1.ConditionRouted)
	for _, want := range []string{"endpoint-direct dropped from web-b", "127.0.0.1", `"not-an-ip"`, "169.254.169.254", "224.0.0.1", "0.0.0.0"} {
		if routed == nil || !strings.Contains(routed.Message, want) {
			t.Fatalf("routed = %+v, want %q in its message", routed, want)
		}
	}
}

// A peer cannot make the Routed message too long for the API server, which takes up to 32768 bytes (issue #36).
func TestEndpointDirectBoundsTheMessage(t *testing.T) {
	addresses := []string{"10.1.0.7", strings.Repeat("x", 100_000)}
	for i := range 20 {
		addresses = append(addresses, fmt.Sprintf("127.0.0.%d", i+1))
	}
	c := directPeer(t, nil, addresses...)
	routed := meta.FindStatusCondition(status(t, c).Conditions, straitv1.ConditionRouted)
	if routed == nil || len(routed.Message) > 1024 || !strings.Contains(routed.Message, "and 16 more") {
		t.Fatalf("routed message of %d bytes: %.300s", len(routed.Message), routed.Message)
	}
}

// A member whose document gives no address that endpoint-direct accepts takes the gateway, as with no address
// (issue #36).
func TestEndpointDirectFallsBackToGateway(t *testing.T) {
	c := directPeer(t, nil, "127.0.0.1")
	es := endpointSlices(t, c)["web-b"]
	if got := sliceAddresses(es); !slices.Equal(got, []string{"10.0.9.9"}) || *es.Ports[0].Port != 15001 {
		t.Fatalf("addresses = %v, port %d; want the gateway, 10.0.9.9:15001", got, *es.Ports[0].Port)
	}
	if st := status(t, c); st.Members[1].Path != straitv1.PathGateway {
		t.Fatalf("members = %+v", st.Members)
	}
}

// With endpointCIDRs, endpoint-direct writes only addresses in those ranges (issue #36).
func TestEndpointDirectCIDRs(t *testing.T) {
	c := directPeer(t, func(l *straitv1.SiteLink) { l.Spec.Direct.EndpointCIDRs = []string{"10.1.0.0/16", "fd00:1::/64"} },
		"10.1.0.7", "10.2.0.9", "192.0.2.1")
	if got := sliceAddresses(endpointSlices(t, c)["web-b"]); !slices.Equal(got, []string{"10.1.0.7"}) {
		t.Fatalf("addresses = %v, want [10.1.0.7]", got)
	}
	routed := meta.FindStatusCondition(status(t, c).Conditions, straitv1.ConditionRouted)
	if routed == nil || !strings.Contains(routed.Message, "10.2.0.9 (outside endpointCIDRs)") || !strings.Contains(routed.Message, "192.0.2.1 (outside endpointCIDRs)") {
		t.Fatalf("routed = %+v", routed)
	}
}

// spyPlugin records the reader that the controller hands it.
type spyPlugin struct{ reader client.Reader }

func (s *spyPlugin) Observe(_ context.Context, r client.Reader, _ *straitv1.SiteService, _ string, _ time.Time) observe.Report {
	s.reader = r
	return observe.Report{Members: map[string]observe.Observation{}}
}

// The plugin gets a reader with no write methods, though the controller holds a full client (RFC#0003, requirement 4).
func TestPluginReaderIsReadOnly(t *testing.T) {
	r, _ := reconciler(t, readB, site("a", "10.0.9.9"), webService("web-a"))
	spy := &spyPlugin{}
	r.pluginFor = func(*straitv1.SiteService) (plugin.Plugin, error) { return spy, nil }
	run(t, r)
	if spy.reader == nil {
		t.Fatal("the plugin was not called")
	}
	var v any = spy.reader
	if _, ok := v.(client.Writer); ok {
		t.Error("the plugin's reader can write")
	}
	if _, ok := v.(client.StatusClient); ok {
		t.Error("the plugin's reader can write status")
	}
	if _, ok := v.(client.SubResourceClientConstructor); ok {
		t.Error("the plugin's reader can write a subresource")
	}
}

// cnpgCluster is a CloudNativePG Cluster in namespace app whose spec names primary, and whose current primary
// instance reports writable or not.
func cnpgCluster(name, primary string, writable bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"replica": map[string]any{"primary": primary, "source": "pg-b"}},
		"status": map[string]any{
			"phase":                  "Cluster in healthy state",
			"currentPrimary":         name + "-1",
			"targetPrimary":          name + "-1",
			"instancesReportedState": map[string]any{name + "-1": map[string]any{"isPrimary": writable}},
		},
	}}
	u.SetGroupVersionKind(CNPGCluster)
	u.SetNamespace("app")
	u.SetName(name)
	return u
}

// Two local Clusters that name different primaries route nowhere with DeclarationsDisagree, though one of them is
// writable. The site still declares none to its peers.
func TestLocalClustersDisagree(t *testing.T) {
	svc := &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web", UID: "uid-web"},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "postgres", Port: 5432},
			Plugin: straitv1.PluginSpec{CNPG: &straitv1.CNPGPlugin{Role: straitv1.CNPGRolePrimary}},
			Members: []straitv1.Member{
				{Name: "pg-a", Site: "a", CNPG: &straitv1.CNPGMember{Cluster: "pg-a"}},
				{Name: "pg-a2", Site: "a", CNPG: &straitv1.CNPGMember{Cluster: "pg-a2"}},
				{Name: "pg-b", Site: "b", CNPG: &straitv1.CNPGMember{Cluster: "pg-b"}},
			},
			Policy: straitv1.Policy{Multiple: straitv1.RouteNowhere, UnknownHold: metav1.Duration{Duration: 10 * time.Second}},
		},
	}
	rw := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "pg-a-rw"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "postgres", Port: 5432}}}}
	rwSlice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "app", Name: "pg-a-rw-x", Labels: map[string]string{discoveryv1.LabelServiceName: "pg-a-rw"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("postgres"), Port: ptr.To[int32](5432)}},
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
	}
	local := &exchange.Local{}
	r, c := reconciler(t, readB, site("a", "10.0.9.9"), svc, rw, rwSlice, cnpgCluster("pg-a", "pg-a", true), cnpgCluster("pg-a2", "pg-b", false))
	r.Local = local
	run(t, r)

	st := status(t, c)
	if st.Route.Reason != straitv1.ReasonDeclarationsDisagree || len(st.Route.Members) != 0 || st.Route.Endpoints != 0 {
		t.Fatalf("route = %+v", st.Route)
	}
	if !strings.Contains(st.Route.Message, "the workloads of site a declare pg-a, pg-b") {
		t.Errorf("message = %q", st.Route.Message)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, straitv1.ConditionDeclarationsDisagree) {
		t.Errorf("conditions = %+v", st.Conditions)
	}
	if len(endpointSlices(t, c)) != 0 {
		t.Errorf("slices = %v", endpointSlices(t, c))
	}
	doc := local.Document("a", now, 0)
	if len(doc.Services) != 1 || doc.Services[0].DeclaredPrimary != "" {
		t.Errorf("document = %+v", doc.Services)
	}
}

// The Routed condition: True with the decision's reason while an endpoint is written; False with NoEndpoints while the
// route names a member that has no endpoint; False with the decision's reason when the route is nowhere (RFC#0002,
// requirement 27). ServiceConflict is TestServiceConflict.
func TestRoutedCondition(t *testing.T) {
	b := func(eligible bool) peers {
		return peers{
			fresh: map[string]bool{"b": true},
			view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
				"web-b": {Member: "web-b", Site: "b", Role: straitv1.RolePrimary, Eligible: eligible, State: straitv1.StateStable, ObservedAt: now},
			}}},
		}
	}
	tests := []struct {
		name   string
		peers  peers
		port   int32
		status metav1.ConditionStatus
		reason string
	}{
		{"an endpoint", b(true), 15001, metav1.ConditionTrue, straitv1.ReasonActive},
		{"routed, no endpoint", b(true), 0, metav1.ConditionFalse, "NoEndpoints"},
		{"nowhere", b(false), 15001, metav1.ConditionFalse, straitv1.ReasonNoEligibleMember},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
			imp.Status.GatewayPort = tt.port
			r, c := reconciler(t, tt.peers, site("a", "10.0.9.9"), webService("web-b"), imp)
			run(t, r)
			cond := meta.FindStatusCondition(status(t, c).Conditions, straitv1.ConditionRouted)
			if cond == nil || cond.Status != tt.status || cond.Reason != tt.reason {
				t.Fatalf("Routed = %+v, want %s %s", cond, tt.status, tt.reason)
			}
		})
	}
}

// writableB is a fresh peer b that declares web-a, and whose member web-b is writable too.
var writableB = peers{
	fresh: map[string]bool{"b": true},
	view: map[string]observe.Report{"b": {Declared: "web-a", Members: map[string]observe.Observation{
		"web-b": {Member: "web-b", Site: "b", Role: straitv1.RolePrimary, Eligible: true, State: straitv1.StateStable, ObservedAt: now},
	}}},
}

// warnings returns the Warning events recorded so far, with their reasons.
func warnings(r *SiteServiceReconciler) []string {
	var out []string
	for {
		select {
		case e := <-r.Recorder.(*events.FakeRecorder).Events:
			if strings.HasPrefix(e, corev1.EventTypeWarning+" ") {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// Two writable members raise a Warning event and set the Multiple condition, whatever the policy: also when
// PreferDeclaredIfConsistent routes to the declared one (RFC#0002, requirement 22).
func TestMultipleWritableWarns(t *testing.T) {
	for _, tc := range []struct {
		policy straitv1.MultiplePolicy
		route  []string
		reason string
	}{
		{straitv1.RouteNowhere, nil, straitv1.ReasonMultipleWritable},
		{straitv1.PreferDeclaredIfConsistent, []string{"web-a"}, straitv1.ReasonPreferDeclared},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
			imp.Status.GatewayPort = 15001
			ss := webService("web-a")
			ss.Spec.Policy.Multiple = tc.policy
			r, c := reconciler(t, writableB, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss, imp)...)
			run(t, r)
			st := status(t, c)
			if !slices.Equal(st.Route.Members, tc.route) || st.Route.Reason != tc.reason {
				t.Fatalf("route = %+v, want %v with %s", st.Route, tc.route, tc.reason)
			}
			multiple := meta.FindStatusCondition(st.Conditions, straitv1.ConditionMultiple)
			if multiple == nil || multiple.Status != metav1.ConditionTrue || multiple.Reason != straitv1.ReasonMultipleWritable {
				t.Fatalf("Multiple condition = %+v, want True with %s", multiple, straitv1.ReasonMultipleWritable)
			}
			w := warnings(r)
			if !slices.ContainsFunc(w, func(e string) bool { return strings.HasPrefix(e, "Warning "+straitv1.ReasonMultipleWritable+" ") }) {
				t.Fatalf("Warning events %v, want one with reason %s", w, straitv1.ReasonMultipleWritable)
			}
		})
	}
}

// status.declarations shows the declaration of each known site: the local site and every peer in the exchange's view,
// also one that declares none. The view holds the fresh peers only (TestView), so peer c, read once and stale now, is
// not shown (RFC#0002, requirement 26).
func TestDeclarations(t *testing.T) {
	ss := webService("web-a")
	ss.Spec.Members = append(ss.Spec.Members,
		straitv1.Member{Name: "web-c", Site: "c", Service: &straitv1.ServicePortRef{Name: "web-c", Port: 8080}},
		straitv1.Member{Name: "web-d", Site: "d", Service: &straitv1.ServicePortRef{Name: "web-d", Port: 8080}},
	)
	p := peers{
		fresh: map[string]bool{"b": true, "d": true},
		read:  map[string]bool{"c": true},
		view: map[string]observe.Report{
			"b": {Declared: "web-a", Members: map[string]observe.Observation{
				"web-b": {Member: "web-b", Site: "b", Role: straitv1.RoleReplica, Eligible: true, State: straitv1.StateStable, ObservedAt: now},
			}},
			"d": {Members: map[string]observe.Observation{}},
		},
	}
	r, c := reconciler(t, p, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss)...)
	run(t, r)
	want := []straitv1.Declaration{{Site: "a", Primary: "web-a"}, {Site: "b", Primary: "web-a"}, {Site: "d"}}
	if got := status(t, c).Declarations; !slices.Equal(got, want) {
		t.Fatalf("declarations = %+v, want %+v", got, want)
	}
}

// status.observedGeneration is the metadata.generation that the status was computed from (RFC#0002,
// requirement 28).
func TestObservedGeneration(t *testing.T) {
	ss := webService("web-a")
	ss.Generation = 3
	r, c := reconciler(t, peers{}, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), ss)...)
	run(t, r)
	if st := status(t, c); st.ObservedGeneration != 3 {
		t.Fatalf("observedGeneration = %d, want 3", st.ObservedGeneration)
	}

	var cur straitv1.SiteService
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &cur); err != nil {
		t.Fatal(err)
	}
	// The fake API server does not count generations, so the test sets the one that a spec change gives.
	cur.Spec.Policy.UnknownHold = metav1.Duration{Duration: 20 * time.Second}
	cur.Generation = 4
	if err := c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	run(t, r)
	st := status(t, c)
	if st.ObservedGeneration != 4 {
		t.Fatalf("after a spec change: observedGeneration = %d, want 4", st.ObservedGeneration)
	}
}

// Endpoint-direct applies only to a direct link with endpointDirect, only while it is the active, healthy link to the
// member's peer. Otherwise the member takes the gateway (RFC#0005, requirement 19).
func TestEndpointDirectOnlyOnTheActiveHealthyLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(*straitv1.SiteLink)
	}{
		{"endpointDirect false", func(l *straitv1.SiteLink) { l.Spec.Direct.EndpointDirect = false }},
		{"not active", func(l *straitv1.SiteLink) { l.Status.Active = false }},
		{"not healthy", func(l *straitv1.SiteLink) { l.Status.Healthy = false }},
		{"a wireguard link", func(l *straitv1.SiteLink) {
			l.Spec.Transport = straitv1.TransportWireGuard
			l.Spec.WireGuard = &straitv1.WireGuardLink{Address: "100.64.0.1/30", PeerAddress: "100.64.0.2"}
		}},
		{"a direct link with no direct block", func(l *straitv1.SiteLink) { l.Spec.Direct = nil }},
		{"a link to another peer", func(l *straitv1.SiteLink) { l.Spec.Peer = "c" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := directPeer(t, tc.link, "10.1.0.7")
			es := endpointSlices(t, c)["web-b"]
			if got := sliceAddresses(es); !slices.Equal(got, []string{"10.0.9.9"}) || *es.Ports[0].Port != 15001 {
				t.Fatalf("addresses = %v, port %d; want the gateway, 10.0.9.9:15001", got, *es.Ports[0].Port)
			}
			if st := status(t, c); st.Members[1].Path != straitv1.PathGateway {
				t.Fatalf("members = %+v", st.Members)
			}
		})
	}
	// The same link, active and healthy, takes endpoint-direct.
	c := directPeer(t, nil, "10.1.0.7")
	if got := sliceAddresses(endpointSlices(t, c)["web-b"]); !slices.Equal(got, []string{"10.1.0.7"}) {
		t.Fatalf("control: addresses = %v, want [10.1.0.7]", got)
	}
}

// With the cnpg plugin off, the controller reads no CloudNativePG kind. A cached read of a kind that RBAC forbids
// would wait for the reconcile's whole time limit. The local member is Unknown with reason PluginDisabled (RFC#0003,
// requirement 13).
func TestCNPGOffReadsNothing(t *testing.T) {
	svc := &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web", UID: "uid-web"},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "postgres", Port: 5432},
			Plugin: straitv1.PluginSpec{CNPG: &straitv1.CNPGPlugin{Role: straitv1.CNPGRolePrimary}},
			Members: []straitv1.Member{
				{Name: "pg-a", Site: "a", CNPG: &straitv1.CNPGMember{Cluster: "pg-a"}},
				{Name: "pg-b", Site: "b", CNPG: &straitv1.CNPGMember{Cluster: "pg-b"}},
			},
			Policy: straitv1.Policy{Multiple: straitv1.RouteNowhere, UnknownHold: metav1.Duration{Duration: 10 * time.Second}},
		},
	}
	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprintf("off=%v", off), func(t *testing.T) {
			r, c := reconciler(t, readB, site("a", "10.0.9.9"), svc.DeepCopy(), cnpgCluster("pg-a", "pg-a", true))
			var reads []string
			r.Reader = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind().Group == CNPGCluster.Group {
						reads = append(reads, key.String())
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			r.Plugins = plugin.Options{CNPGOff: off}
			run(t, r)
			m := status(t, c).Members[0]
			switch {
			case off && (len(reads) != 0 || m.State != straitv1.StateUnknown || m.Reason != plugin.ReasonPluginDisabled):
				t.Errorf("read %v; member %+v; want no read, Unknown, PluginDisabled", reads, m)
			case !off && (len(reads) == 0 || m.Reason == plugin.ReasonPluginDisabled):
				t.Errorf("read %v; member %+v; want the Cluster read", reads, m)
			}
		})
	}
}

// A member whose site is a stale peer shows SiteUnknown in status, as the decision counts it, and a member that a
// fresh peer does not list shows NotObserved (RFC#0002, requirement 16).
func TestMemberReasonOfAStalePeer(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    peers
		want string
	}{
		{"stale", readB, "SiteUnknown"},
		{"fresh, not listed", peers{fresh: map[string]bool{"b": true}, view: map[string]observe.Report{"b": {Members: map[string]observe.Observation{}}}}, "NotObserved"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, c := reconciler(t, tt.p, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))...)
			run(t, r)
			m := status(t, c).Members[1]
			if m.Name != "web-b" || m.State != straitv1.StateUnknown || m.Reason != tt.want {
				t.Errorf("member = %+v, want web-b Unknown, %s", m, tt.want)
			}
		})
	}
}

// Each decision records its time for the metric strait_siteservice_last_decision_timestamp_seconds, and a deleted
// SiteService's time goes with it.
func TestDecisionsRecordTheirTime(t *testing.T) {
	r, c := reconciler(t, peers{}, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))...)
	key := types.NamespacedName{Namespace: "app", Name: "web"}
	if got := r.Decisions(); len(got) != 0 {
		t.Fatalf("before a decision: %v", got)
	}
	run(t, r)
	if got := r.Decisions(); !got[key].Equal(r.Now()) || len(got) != 1 {
		t.Fatalf("after a decision: %v, want %s at %v", got, key, r.Now())
	}
	if err := c.Delete(context.Background(), &straitv1.SiteService{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}); err != nil {
		t.Fatal(err)
	}
	run(t, r)
	if got := r.Decisions(); len(got) != 0 {
		t.Fatalf("after the delete: %v", got)
	}
}
