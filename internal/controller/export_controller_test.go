package controller

import (
	"context"
	"net/http"
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

	straitv1 "github.com/takodotid/strait/api/v1"
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
