package controller

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// TestXDSTriggerFollowsExportedSlices: the gateway's configuration reads the EndpointSlices of exported Services only,
// so a change of any other EndpointSlice of the cluster does not rebuild it (issue #46, row 18).
func TestXDSTriggerFollowsExportedSlices(t *testing.T) {
	exp := &straitv1.Export{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "pg-pg-a"},
		Spec:       straitv1.ExportSpec{Service: straitv1.ServicePortRef{Name: "pg-a-rw", Port: 5432}},
	}
	tr := &XDSTrigger{Trigger: func() {}, Namespace: "strait-system", Reader: fakeClient(t, exp)}
	slice := func(namespace, service string) *discoveryv1.EndpointSlice {
		es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: service + "-x1"}}
		if service != "" {
			es.Labels = map[string]string{discoveryv1.LabelServiceName: service}
		}
		return es
	}
	for _, c := range []struct {
		name  string
		slice *discoveryv1.EndpointSlice
		want  bool
	}{
		{"exported Service", slice("app", "pg-a-rw"), true},
		{"other Service", slice("app", "pg-a-ro"), false},
		{"same name in another namespace", slice("web", "pg-a-rw"), false},
		{"no Service", slice("app", ""), false},
	} {
		if got := tr.exported(context.Background(), c.slice); got != c.want {
			t.Errorf("%s: exported = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestXDSTriggerEvents: a renewal of a Lease and a change of an EndpointSlice that no Export reads do not rebuild the
// gateway's configuration. A change of an exported Service's EndpointSlice does, and so does the update that writes a
// gateway pod's address: a pod's Envoy can connect before it, and its configuration waits for it.
func TestXDSTriggerEvents(t *testing.T) {
	exp := &straitv1.Export{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "pg-pg-a"},
		Spec:       straitv1.ExportSpec{Service: straitv1.ServicePortRef{Name: "pg-a-rw", Port: 5432}},
	}
	c := fakeClient(t, exp)
	var triggers atomic.Int32
	tr := &XDSTrigger{Trigger: func() { triggers.Add(1) }, Namespace: "strait-system", Reader: c}

	informers := &informertest.FakeInformers{Scheme: c.Scheme()}
	kinds := []client.Object{&straitv1.Site{}, &straitv1.SitePeer{}, &straitv1.SiteLink{}, &straitv1.Export{}, &straitv1.Import{},
		&discoveryv1.EndpointSlice{}, &corev1.Pod{}, &coordinationv1.Lease{}}
	for _, o := range kinds {
		if _, err := informers.FakeInformerFor(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	mapper := meta.NewDefaultRESTMapper(nil)
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
	if err := tr.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	// The controller has started once a Site event triggers.
	sites, _ := informers.FakeInformerFor(ctx, &straitv1.Site{})
	waitTriggers := func(what string, want int32) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); triggers.Load() < want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d triggers, want %d", what, triggers.Load(), want)
			}
		}
	}
	for deadline := time.Now().Add(5 * time.Second); triggers.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the trigger did not start")
		}
		sites.Add(site("a", ""))
	}
	time.Sleep(200 * time.Millisecond)
	base := triggers.Load()

	leases, _ := informers.FakeInformerFor(ctx, &coordinationv1.Lease{})
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "strait-system", Name: "strait-gateway"}}
	renewed := lease.DeepCopy()
	renewed.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
	leases.Update(lease, renewed)
	slices, _ := informers.FakeInformerFor(ctx, &discoveryv1.EndpointSlice{})
	other := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "pg-a-ro-x1",
		Labels: map[string]string{discoveryv1.LabelServiceName: "pg-a-ro"}}}
	slices.Add(other)
	time.Sleep(time.Second)
	if got := triggers.Load(); got != base {
		t.Fatalf("a Lease renewal and an EndpointSlice that no Export reads: %d triggers, want none", got-base)
	}
	exported := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "pg-a-rw-x1",
		Labels: map[string]string{discoveryv1.LabelServiceName: "pg-a-rw"}}}
	slices.Add(exported)
	waitTriggers("an exported Service's EndpointSlice", base+1)

	pods, _ := informers.FakeInformerFor(ctx, &corev1.Pod{})
	pending := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "strait-system", Name: "strait-gateway-0"}}
	addressed := pending.DeepCopy()
	addressed.Status.PodIP = "10.244.1.7"
	pods.Update(pending, addressed)
	waitTriggers("a gateway pod's address", base+2)
}

// The Export reconciler and the xDS trigger both read IndexExportService. On one manager, the cache gets it once:
// a cache refuses an index it holds already (issue #64).
func TestExportIndexOnOneManager(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		straitv1.GroupVersion.WithKind("Export"), straitv1.GroupVersion.WithKind("Site"), straitv1.GroupVersion.WithKind("SitePeer"),
		straitv1.GroupVersion.WithKind("SiteLink"), straitv1.GroupVersion.WithKind("Import"),
		discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"), corev1.SchemeGroupVersion.WithKind("Pod"),
		coordinationv1.SchemeGroupVersion.WithKind("Lease"),
	} {
		scope := meta.RESTScopeNamespace
		if gvk.Kind == "Site" || gvk.Kind == "SitePeer" || gvk.Kind == "SiteLink" {
			scope = meta.RESTScopeRoot
		}
		mapper.Add(gvk, scope)
	}
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		MapperProvider:         func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&ExportReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatalf("Export reconciler: %v", err)
	}
	if err := (&XDSTrigger{Trigger: func() {}}).SetupWithManager(mgr); err != nil {
		t.Fatalf("xDS trigger on the same manager: %v", err)
	}
}
