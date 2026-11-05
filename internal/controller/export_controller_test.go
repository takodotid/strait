package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/exchange"
)

var webPort = &straitv1.ServicePort{Name: "http", Port: 8080}

// webImport is a hand-written Import of site b's export web-b, on gateway port 15001.
func webImport(port *straitv1.ServicePort) *straitv1.Import {
	imp := &straitv1.Import{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-b", UID: "uid-import"},
		Spec:       straitv1.ImportSpec{Site: "b", Export: "web-b", Port: port},
	}
	imp.Status.GatewayPort = 15001
	return imp
}

func importSite(gateway string) *straitv1.Site {
	s := site("a", gateway)
	s.Spec.Gateway.ImportPorts = straitv1.PortRange{First: 15000, Last: 15999}
	return s
}

func importReconciler(t *testing.T, objs ...client.Object) (*ImportReconciler, client.Client) {
	t.Helper()
	c := fakeClient(t, objs...)
	return &ImportReconciler{Client: c, Reader: c, Peers: peers{}, Now: func() time.Time { return now }}, c
}

func runImport(t *testing.T, r *ImportReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web-b"}}); err != nil {
		t.Fatal(err)
	}
}

func importStatus(t *testing.T, c client.Client) straitv1.ImportStatus {
	t.Helper()
	var imp straitv1.Import
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-b"}, &imp); err != nil {
		t.Fatal(err)
	}
	return imp.Status
}

// importSlice returns the EndpointSlice named after the Import, or nil.
func importSlice(t *testing.T, c client.Client) *discoveryv1.EndpointSlice {
	t.Helper()
	var es discoveryv1.EndpointSlice
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-b"}, &es)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &es
}

// addresses returns the slice's addresses. A missing slice has none.
func addresses(es *discoveryv1.EndpointSlice) []string {
	if es == nil {
		return nil
	}
	var out []string
	for _, e := range es.Endpoints {
		out = append(out, e.Addresses...)
	}
	return out
}

func TestImportService(t *testing.T) {
	r, c := importReconciler(t, importSite("10.0.9.9"), webImport(webPort))
	runImport(t, r)

	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-b"}, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Selector != nil || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != "http" || svc.Spec.Ports[0].Port != 8080 ||
		svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP || !metav1.IsControlledBy(&svc, webImport(nil)) {
		t.Fatalf("service = %+v %+v", svc.ObjectMeta, svc.Spec)
	}
	es := importSlice(t, c)
	if es == nil || es.Labels[discoveryv1.LabelServiceName] != "web-b" || es.Labels[LabelManagedBy] != ManagedBy || !metav1.IsControlledBy(es, webImport(nil)) {
		t.Fatalf("slice = %+v", es)
	}
	if got := addresses(es); len(got) != 1 || got[0] != "10.0.9.9" || len(es.Ports) != 1 || *es.Ports[0].Name != "http" || *es.Ports[0].Port != 15001 {
		t.Fatalf("slice = %+v", es)
	}
	if !meta.IsStatusConditionTrue(importStatus(t, c).Conditions, ConditionService) {
		t.Fatalf("conditions = %+v", importStatus(t, c).Conditions)
	}

	// No active gateway: the Service stays, with no endpoint.
	var s straitv1.Site
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &s); err != nil {
		t.Fatal(err)
	}
	s.Status.Gateway.Address = ""
	if err := c.Status().Update(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	runImport(t, r)
	if es := importSlice(t, c); es == nil || len(es.Endpoints) != 0 {
		t.Fatalf("slice = %+v", es)
	}
}

func TestImportServiceConflict(t *testing.T) {
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-b"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	r, c := importReconciler(t, importSite("10.0.9.9"), webImport(webPort), other)
	runImport(t, r)

	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-b"}, &svc); err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || len(svc.OwnerReferences) != 0 {
		t.Fatalf("service changed: %+v %+v", svc.ObjectMeta, svc.Spec)
	}
	if es := importSlice(t, c); es != nil {
		t.Fatalf("slice = %+v", es)
	}
	cond := meta.FindStatusCondition(importStatus(t, c).Conditions, ConditionService)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServiceConflict" {
		t.Fatalf("service condition = %+v", cond)
	}
}

func TestImportPortRemoved(t *testing.T) {
	// The Service, the slice and the condition that the Import had while it set port.
	imp := webImport(nil)
	imp.Status.Conditions = []metav1.Condition{{Type: ConditionService, Status: metav1.ConditionTrue, Reason: "Synced", LastTransitionTime: metav1.Time{Time: now}}}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(imp, straitv1.GroupVersion.WithKind("Import"))}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-b", OwnerReferences: owner},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "app", Name: "web-b", OwnerReferences: owner, Labels: map[string]string{discoveryv1.LabelServiceName: "web-b"}},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	r, c := importReconciler(t, importSite("10.0.9.9"), imp, svc, slice)
	runImport(t, r)

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-b"}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("service: got %v, want not found", err)
	}
	if es := importSlice(t, c); es != nil {
		t.Fatalf("slice = %+v, want none", es)
	}
	if cond := meta.FindStatusCondition(importStatus(t, c).Conditions, ConditionService); cond != nil {
		t.Fatalf("service condition = %+v", cond)
	}
}

// Without port, the Import leaves a Service and a slice of its name alone: a SiteService's slice is named like its Imports.
func TestImportWithoutPortLeavesOthers(t *testing.T) {
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-b"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "app", Name: "web-b", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	r, c := importReconciler(t, importSite("10.0.9.9"), webImport(nil), other, slice)
	runImport(t, r)

	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-b"}, &svc); err != nil {
		t.Fatalf("service: %v", err)
	}
	if es := importSlice(t, c); es == nil || es.Labels[discoveryv1.LabelServiceName] != "web" {
		t.Fatalf("slice = %+v", es)
	}
	if cond := meta.FindStatusCondition(importStatus(t, c).Conditions, ConditionService); cond != nil {
		t.Fatalf("service condition = %+v", cond)
	}
}

// The Service of an Import follows the active gateway when the Site changes, not only at the next resync.
func TestImportServiceFollowsSite(t *testing.T) {
	r, c := importReconciler(t, importSite("10.0.9.9"), webImport(webPort))
	runImport(t, r)
	var reconciles atomic.Int32
	r.Now = func() time.Time {
		reconciles.Add(1)
		return now
	}

	// The fake informers are not safe for concurrent use: each one exists before the controller starts,
	// and gets events only once the controller has reconciled, so after it has added its handlers.
	informers := &informertest.FakeInformers{Scheme: c.Scheme()}
	for _, o := range []client.Object{&straitv1.Import{}, &corev1.Service{}, &discoveryv1.EndpointSlice{}, &straitv1.Site{}} {
		if _, err := informers.FakeInformerFor(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(straitv1.GroupVersion.WithKind("Import"), meta.RESTScopeNamespace)
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme:                 c.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		NewCache:               func(*rest.Config, cache.Options) (cache.Cache, error) { return informers, nil },
		MapperProvider:         func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	changes := make(chan event.GenericEvent)
	if err := r.SetupWithManager(mgr, changes); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	changes <- event.GenericEvent{Object: webImport(nil)}
	for deadline := time.Now().Add(5 * time.Second); reconciles.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the controller did not start")
		}
	}

	var before straitv1.Site
	if err := c.Get(ctx, types.NamespacedName{Name: "a"}, &before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Status.Gateway.Address = "10.0.9.10"
	if err := c.Status().Update(ctx, after); err != nil {
		t.Fatal(err)
	}
	sites, err := informers.FakeInformerFor(ctx, &straitv1.Site{})
	if err != nil {
		t.Fatal(err)
	}
	sites.Update(&before, after)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got := addresses(importSlice(t, c))
		if len(got) == 1 && got[0] == "10.0.9.10" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("5s after the Site changed, the Import's slice holds %v, want [10.0.9.10]", got)
		}
	}
}

// laggingExport is an export reconciler of app/web-a, whose cache shows the Export as it was before the first write.
func laggingExport(t *testing.T) (*ExportReconciler, *laggingCache, client.Client) {
	t.Helper()
	s := site("a", "")
	s.Spec.Gateway.ExportPorts = straitv1.PortRange{First: 16000, Last: 16999}
	exp := &straitv1.Export{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-a", UID: "uid-export"},
		Spec:       straitv1.ExportSpec{Service: straitv1.ServicePortRef{Name: "web-a", Port: 8080}},
	}
	c := fakeClient(t, append(appService("web-a", "10.0.0.1"), s, exp)...)
	cache := &laggingCache{Client: c}
	cache.freeze(t, &straitv1.Export{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-a"}})
	return &ExportReconciler{Client: cache, Reader: cache, Local: &exchange.Local{}}, cache, c
}

func runExport(t *testing.T, r *ExportReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web-a"}}); err != nil {
		t.Fatal(err)
	}
}

func exportStatus(t *testing.T, c client.Client) straitv1.ExportStatus {
	t.Helper()
	var exp straitv1.Export
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-a"}, &exp); err != nil {
		t.Fatal(err)
	}
	return exp.Status
}

// A cache that has not yet seen the controller's last write of an Export's status does not make the next write
// conflict (issue #23).
func TestExportStatusWriteOnLaggingCache(t *testing.T) {
	r, cache, c := laggingExport(t)
	// The first reconcile allocates the port. The second has nothing new to write.
	runExport(t, r)
	runExport(t, r)
	// The third writes over the first.
	var es discoveryv1.EndpointSlice
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-a-x"}, &es); err != nil {
		t.Fatal(err)
	}
	es.Endpoints = nil
	if err := c.Update(context.Background(), &es); err != nil {
		t.Fatal(err)
	}
	runExport(t, r)
	if st := exportStatus(t, c); st.GatewayPort != 16000 || st.ReadyEndpoints != 0 || !meta.IsStatusConditionFalse(st.Conditions, ConditionReady) {
		t.Fatalf("status = %+v", st)
	}

	cache.thaw()
	runExport(t, r)
	if len(r.written.last) != 0 {
		t.Fatalf("written = %+v, want it dropped once the cache shows the last write", r.written.last)
	}
}

// A cache that has not yet seen the controller's last write of an Import's status does not make the next write
// conflict (issue #23).
func TestImportStatusWriteOnLaggingCache(t *testing.T) {
	r, c := importReconciler(t, importSite("10.0.9.9"), webImport(nil))
	cache := &laggingCache{Client: c}
	cache.freeze(t, &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-b"}})
	r.Client, r.Reader = cache, cache
	// The first reconcile writes the conditions. The second has nothing new to write.
	runImport(t, r)
	runImport(t, r)
	// The third writes over the first: peer b is read, and does not list the export.
	r.Peers = peers{fresh: map[string]bool{"b": true}}
	runImport(t, r)
	if c := meta.FindStatusCondition(importStatus(t, c).Conditions, ConditionReady); c == nil || c.Reason != "ExportNotFound" {
		t.Fatalf("Ready = %+v", c)
	}

	cache.thaw()
	runImport(t, r)
	if len(r.written.last) != 0 {
		t.Fatalf("written = %+v, want it dropped once the cache shows the last write", r.written.last)
	}
}

// portSite is site a with export ports 16000-16999 and import ports 15000-15999.
func portSite() *straitv1.Site {
	s := site("a", "10.0.9.9")
	s.Spec.Gateway.ExportPorts = straitv1.PortRange{First: 16000, Last: 16999}
	s.Spec.Gateway.ImportPorts = straitv1.PortRange{First: 15000, Last: 15999}
	return s
}

// newExport is an Export in namespace app, of Service web, that holds port, or none.
func newExport(name string, port int32) *straitv1.Export {
	e := &straitv1.Export{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name, UID: types.UID("uid-" + name)},
		Spec:       straitv1.ExportSpec{Service: straitv1.ServicePortRef{Name: "web", Port: 8080}},
	}
	e.Status.GatewayPort = port
	return e
}

// newImport is an Import in namespace app, of site b's export of the same name, that holds port, or none.
func newImport(name string, port int32) *straitv1.Import {
	i := &straitv1.Import{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name, UID: types.UID("uid-" + name)},
		Spec:       straitv1.ImportSpec{Site: "b", Export: name},
	}
	i.Status.GatewayPort = port
	return i
}

// portsExport is an export reconciler whose cache can lag behind the API server.
func portsExport(t *testing.T, objs ...client.Object) (*ExportReconciler, *laggingCache, client.Client) {
	t.Helper()
	c := fakeClient(t, append(appService("web", "10.0.0.1"), append(objs, portSite())...)...)
	cache := &laggingCache{Client: c}
	return &ExportReconciler{Client: cache, Reader: cache, Local: &exchange.Local{}}, cache, c
}

// portsImport is an import reconciler whose cache can lag behind the API server.
func portsImport(t *testing.T, objs ...client.Object) (*ImportReconciler, *laggingCache, client.Client) {
	t.Helper()
	r, c := importReconciler(t, append(objs, portSite())...)
	cache := &laggingCache{Client: c}
	r.Client, r.Reader = cache, cache
	return r, cache, c
}

func reconcileNamed(t *testing.T, r reconcile.Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: name}}); err != nil {
		t.Fatal(err)
	}
}

// exportPorts returns the gateway port of each named Export, as the API server holds it.
func exportPorts(t *testing.T, c client.Client, names ...string) []int32 {
	t.Helper()
	var out []int32
	for _, n := range names {
		var e straitv1.Export
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: n}, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Status.GatewayPort)
	}
	return out
}

// importPorts returns the gateway port of each named Import, as the API server holds it.
func importPorts(t *testing.T, c client.Client, names ...string) []int32 {
	t.Helper()
	var out []int32
	for _, n := range names {
		var i straitv1.Import
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: n}, &i); err != nil {
			t.Fatal(err)
		}
		out = append(out, i.Status.GatewayPort)
	}
	return out
}

// Two Exports created together get different ports, though the second is reconciled before the cache shows the
// first one's port (issue #26).
func TestExportPortsCreatedTogether(t *testing.T) {
	r, cache, c := portsExport(t, newExport("one", 0), newExport("two", 0))
	cache.freeze(t, &straitv1.Export{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "one"}})
	reconcileNamed(t, r, "one")
	reconcileNamed(t, r, "two")
	if got := exportPorts(t, c, "one", "two"); !slices.Equal(got, []int32{16000, 16001}) {
		t.Fatalf("ports = %v, want [16000 16001]", got)
	}
}

// Two Imports created together get different ports, though the second is reconciled before the cache shows the
// first one's port (issue #26).
func TestImportPortsCreatedTogether(t *testing.T) {
	r, cache, c := portsImport(t, newImport("one", 0), newImport("two", 0))
	cache.freeze(t, &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "one"}})
	reconcileNamed(t, r, "one")
	reconcileNamed(t, r, "two")
	if got := importPorts(t, c, "one", "two"); !slices.Equal(got, []int32{15000, 15001}) {
		t.Fatalf("ports = %v, want [15000 15001]", got)
	}
}

// Of two Exports that hold one port, as an earlier release could leave them, the one that comes second by namespace
// and name moves to a free port, whichever is reconciled first. The first keeps it (issue #26).
func TestExportDuplicatePortRepaired(t *testing.T) {
	for _, order := range [][]string{{"a-first", "b-second"}, {"b-second", "a-first"}} {
		r, _, c := portsExport(t, newExport("a-first", 16000), newExport("b-second", 16000), newExport("c-other", 16001))
		for _, n := range order {
			reconcileNamed(t, r, n)
		}
		if got := exportPorts(t, c, "a-first", "b-second", "c-other"); !slices.Equal(got, []int32{16000, 16002, 16001}) {
			t.Errorf("reconciled in order %v: ports = %v, want [16000 16002 16001]", order, got)
		}
	}
}

// Of two Imports that hold one port, the one that comes second moves to a free port. The first keeps it (issue #26).
func TestImportDuplicatePortRepaired(t *testing.T) {
	for _, order := range [][]string{{"a-first", "b-second"}, {"b-second", "a-first"}} {
		r, _, c := portsImport(t, newImport("a-first", 15000), newImport("b-second", 15000))
		for _, n := range order {
			reconcileNamed(t, r, n)
		}
		if got := importPorts(t, c, "a-first", "b-second"); !slices.Equal(got, []int32{15000, 15001}) {
			t.Errorf("reconciled in order %v: ports = %v, want [15000 15001]", order, got)
		}
	}
}

// A cache that has not yet seen a port move moves nothing else: the moved object keeps its new port, and its old and
// new ports both count as they are on the API server.
func TestExportPortMoveOnLaggingCache(t *testing.T) {
	r, cache, c := portsExport(t, newExport("a-first", 16000), newExport("b-second", 16000), newExport("c-new", 0))
	cache.freeze(t, &straitv1.Export{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "b-second"}})
	reconcileNamed(t, r, "b-second")
	// The cache still shows b-second on 16000.
	reconcileNamed(t, r, "a-first")
	reconcileNamed(t, r, "b-second")
	reconcileNamed(t, r, "c-new")
	if got := exportPorts(t, c, "a-first", "b-second", "c-new"); !slices.Equal(got, []int32{16000, 16001, 16002}) {
		t.Fatalf("ports = %v, want [16000 16001 16002]", got)
	}
}

// With no free port left, the Export that comes second holds none, and says so, rather than share a port.
func TestExportDuplicatePortRangeFull(t *testing.T) {
	r, _, c := portsExport(t, newExport("a-first", 16000), newExport("b-second", 16000))
	var s straitv1.Site
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &s); err != nil {
		t.Fatal(err)
	}
	s.Spec.Gateway.ExportPorts.Last = 16000
	if err := c.Update(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	reconcileNamed(t, r, "b-second")
	var e straitv1.Export
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "b-second"}, &e); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(e.Status.Conditions, ConditionAllocated); e.Status.GatewayPort != 0 || cond == nil || cond.Reason != "RangeFull" {
		t.Fatalf("port %d, Allocated = %+v; want no port, RangeFull", e.Status.GatewayPort, cond)
	}
}

// listCounter counts the lists of Exports and Imports that read every object of the kind: those with no field
// selector.
type listCounter struct {
	client.Client
	full int
}

func (l *listCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	switch list.(type) {
	case *straitv1.ExportList, *straitv1.ImportList:
		if o.FieldSelector == nil {
			l.full++
		}
	}
	return l.Client.List(ctx, list, opts...)
}

// A reconcile of an Export or an Import that keeps its port lists no object of its kind but those that hold that
// port. Only one that needs a port lists them all (issue #46, row 37).
func TestPortKeptWithoutFullList(t *testing.T) {
	var exports []client.Object
	var imports []client.Object
	for i := range 50 {
		exports = append(exports, newExport(fmt.Sprintf("e%02d", i), int32(16000+i)))
		imports = append(imports, newImport(fmt.Sprintf("i%02d", i), int32(15000+i)))
	}
	exports = append(exports, newExport("new", 0))
	imports = append(imports, newImport("new", 0))

	er, _, ec := portsExport(t, exports...)
	counter := &listCounter{Client: er.Client}
	er.Client, er.Reader = counter, counter
	reconcileNamed(t, er, "e07")
	if counter.full != 0 {
		t.Errorf("an Export that keeps its port: %d lists of every Export, want none", counter.full)
	}
	reconcileNamed(t, er, "new")
	if counter.full != 1 {
		t.Errorf("an Export that needs a port: %d lists of every Export, want 1", counter.full)
	}
	if got := exportPorts(t, ec, "e07", "new"); !slices.Equal(got, []int32{16007, 16050}) {
		t.Errorf("export ports = %v, want [16007 16050]", got)
	}

	ir, _, ic := portsImport(t, imports...)
	counter = &listCounter{Client: ir.Client}
	ir.Client, ir.Reader = counter, counter
	reconcileNamed(t, ir, "i07")
	if counter.full != 0 {
		t.Errorf("an Import that keeps its port: %d lists of every Import, want none", counter.full)
	}
	reconcileNamed(t, ir, "new")
	if counter.full != 1 {
		t.Errorf("an Import that needs a port: %d lists of every Import, want 1", counter.full)
	}
	if got := importPorts(t, ic, "i07", "new"); !slices.Equal(got, []int32{15007, 15050}) {
		t.Errorf("import ports = %v, want [15007 15050]", got)
	}
}

// An Export that holds a port in the cache moves when the controller has just given that port to one that comes
// first, though the cache does not show that write yet: as when an earlier leader gave the port twice.
func TestPortKeptSeesTheLastWrite(t *testing.T) {
	r, cache, c := portsExport(t, newExport("a-first", 0), newExport("b-second", 0))
	cache.freeze(t, &straitv1.Export{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "a-first"}})
	reconcileNamed(t, r, "a-first")
	// Another leader's write gives b-second the same port.
	var b straitv1.Export
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "b-second"}, &b); err != nil {
		t.Fatal(err)
	}
	b.Status.GatewayPort = 16000
	if err := c.Status().Update(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	reconcileNamed(t, r, "b-second")
	if got := exportPorts(t, c, "a-first", "b-second"); !slices.Equal(got, []int32{16000, 16001}) {
		t.Fatalf("ports = %v, want [16000 16001]", got)
	}
}
