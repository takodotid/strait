package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/links"
)

const ns = "strait"

func testSite() *straitv1.Site {
	return &straitv1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       straitv1.SiteSpec{WireGuard: &straitv1.SiteWireGuard{PrivateKeySecretRef: straitv1.SecretKeyRef{Name: "wg"}}},
	}
}

func keySecret(key string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "wg"}, Data: map[string][]byte{"privateKey": []byte(key + "\n")}}
}

// newAgent returns an agent on a fake client and a fake kernel. Patches counts status patches.
func newAgent(t *testing.T, objs ...client.Object) (a *Agent, k *fakeKernel, patches *atomic.Int32) {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	patches = &atomic.Int32{}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&straitv1.Site{}, &straitv1.SiteLink{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				patches.Add(1)
				return c.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		}).Build()
	k = newFakeKernel()
	a = &Agent{Client: c, Kernel: k, Namespace: ns, Identity: "gw-0", HTTP: http.DefaultClient, Resolve: resolve, Log: logr.Discard()}
	return a, k, patches
}

func TestMissingSecret(t *testing.T) {
	l := wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
	p := peer("b", newKey(t).PublicKey().String())
	a, k, _ := newAgent(t, testSite(), &l, &p)
	ctx := context.Background()

	_, in, err := a.inputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in.Key != nil || in.NoKey != "Secret strait/wg not found" {
		t.Fatalf("key set %v, NoKey %q", in.Key != nil, in.NoKey)
	}
	a.syncWireGuard(ctx)
	if len(k.devs) != 0 {
		t.Fatalf("interfaces without a key: %d", len(k.devs))
	}
	if _, problems := a.links(); problems["to-b"] != "Secret strait/wg not found" {
		t.Fatalf("problems = %v", problems)
	}
}

func TestPublishKey(t *testing.T) {
	key := newKey(t)
	l := wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
	p := peer("b", newKey(t).PublicKey().String())
	a, k, patches := newAgent(t, testSite(), keySecret(key.String()), &l, &p)
	ctx := context.Background()

	a.syncWireGuard(ctx)
	var site straitv1.Site
	if err := a.Client.Get(ctx, client.ObjectKey{Name: "a"}, &site); err != nil {
		t.Fatal(err)
	}
	if site.Status.WireGuardPublicKey != key.PublicKey().String() {
		t.Fatal("the public key is not published")
	}
	if held, _ := a.links(); held["to-b"] != links.Interface("to-b") || len(k.devs) != 1 {
		t.Fatalf("held %v, %d interfaces", held, len(k.devs))
	}
	k.takeCalls()
	a.syncWireGuard(ctx)
	if n := patches.Load(); n != 1 {
		t.Errorf("%d status patches, want 1", n)
	}
	if calls := k.takeCalls(); len(calls) != 0 {
		t.Errorf("second sync changed %v", calls)
	}
}

func envoy(t *testing.T, up *atomic.Bool) string {
	t.Helper()
	body, err := os.ReadFile("testdata/clusters.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/clusters":
			_, _ = w.Write(body)
		case "/ready":
			_, _ = w.Write([]byte("LIVE\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestReportHealthWritesOnChange(t *testing.T) {
	var objs []client.Object
	for _, l := range fixtureLinks() {
		objs = append(objs, &l)
	}
	a, _, patches := newAgent(t, objs...)
	up := &atomic.Bool{}
	up.Store(true)
	a.EnvoyAdmin = envoy(t, up)
	a.setLinks(map[string]string{"b-wg": links.Interface("b-wg")}, nil)
	ctx := context.Background()

	get := func(name string) straitv1.SiteLink {
		var l straitv1.SiteLink
		if err := a.Client.Get(ctx, client.ObjectKey{Name: name}, &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	a.reportHealth(ctx)
	if n := patches.Load(); n != 5 {
		t.Fatalf("%d patches on the first pass, want 5", n)
	}
	wg := get("b-wg")
	if !wg.Status.Healthy || !wg.Status.Active || wg.Status.Interface != links.Interface("b-wg") || wg.Status.LastTransitionTime == nil {
		t.Fatalf("b-wg status = %+v", wg.Status)
	}
	if d := get("b-direct"); d.Status.Healthy || d.Status.Active || d.Status.Interface != "" {
		t.Fatalf("b-direct status = %+v", d.Status)
	}
	rv := wg.ResourceVersion

	a.reportHealth(ctx)
	if n := patches.Load(); n != 5 {
		t.Fatalf("%d patches after an unchanged pass, want 5", n)
	}
	if got := get("b-wg").ResourceVersion; got != rv {
		t.Fatalf("resourceVersion moved from %s to %s", rv, got)
	}

	up.Store(false)
	a.reportHealth(ctx)
	wg = get("b-wg")
	c := meta.FindStatusCondition(wg.Status.Conditions, ConditionHealthy)
	if wg.Status.Healthy || wg.Status.Active || c == nil || c.Reason != "EnvoyUnreachable" {
		t.Fatalf("b-wg with Envoy down = %+v", wg.Status)
	}
}

func TestStatusPatchCarriesFalse(t *testing.T) {
	old := directLink("lan", "b", 10, "198.51.100.2")
	l := old.DeepCopy()
	l.Status.Interface = "x"
	patch, err := statusPatch(&old, l)
	if err != nil {
		t.Fatal(err)
	}
	data, err := patch.Data(l)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status["healthy"] != false || p.Status["active"] != false || p.Status["interface"] != "x" {
		t.Fatalf("patch = %s", data)
	}
}

func TestProbes(t *testing.T) {
	up := &atomic.Bool{}
	h := Probes(envoy(t, up), http.DefaultClient)
	code := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if code("/healthz") != 200 || code("/readyz") != 503 {
		t.Fatalf("Envoy down: healthz %d, readyz %d", code("/healthz"), code("/readyz"))
	}
	up.Store(true)
	if code("/readyz") != 200 {
		t.Fatalf("Envoy up: readyz %d", code("/readyz"))
	}
}

func leadWithInterface(t *testing.T) (*Agent, *fakeKernel) {
	t.Helper()
	key := newKey(t)
	l := wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
	p := peer("b", newKey(t).PublicKey().String())
	a, k, _ := newAgent(t, testSite(), keySecret(key.String()), &l, &p)
	a.EnvoyAdmin = envoy(t, &atomic.Bool{})
	return a, k
}

func waitForInterfaces(t *testing.T, k *fakeKernel, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if devs, _ := k.Devices(); len(devs) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not %d interfaces in time", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReleaseWaitsForInterfaces(t *testing.T) {
	a, k := leadWithInterface(t)
	released := false
	l := &lease{Interface: &recordLock{update: func(r resourcelock.LeaderElectionRecord) {
		devs, _ := k.Devices()
		released = r.HolderIdentity == "" && len(devs) == 0
	}}, before: a.stopLeading}
	if err := l.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: "gw-0"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Go(func() { a.lead(context.Background(), l, func() {}) })
	waitForInterfaces(t, k, 1)
	if err := l.Update(context.Background(), resourcelock.LeaderElectionRecord{}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !released {
		t.Fatal("the Lease was released while an interface was still in place")
	}
	if held, _ := a.links(); len(held) != 0 {
		t.Fatalf("held after release: %v", held)
	}
}

func TestStepDownWhenNotRenewed(t *testing.T) {
	a, k := leadWithInterface(t)
	l := &lease{Interface: &recordLock{update: func(resourcelock.LeaderElectionRecord) {}}, before: a.stopLeading}
	_ = l.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: "gw-0"})
	var stepped atomic.Bool
	start := time.Now()
	a.lead(context.Background(), l, func() { stepped.Store(true) })
	if !stepped.Load() || time.Since(start) < RenewDeadline {
		t.Fatalf("stepped down %v after %s", stepped.Load(), time.Since(start))
	}
	waitForInterfaces(t, k, 0)
}

// recordLock is a resourcelock.Interface that only records updates.
type recordLock struct {
	resourcelock.Interface
	update func(resourcelock.LeaderElectionRecord)
}

func (l *recordLock) Update(_ context.Context, r resourcelock.LeaderElectionRecord) error {
	l.update(r)
	return nil
}
