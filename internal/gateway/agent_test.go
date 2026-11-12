package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
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
	h := Probes(envoy(t, up), http.DefaultClient, nil)
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
	up := &atomic.Bool{}
	up.Store(true)
	a.EnvoyAdmin = envoy(t, up)
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

// renewing renews l every 500ms until the test ends, as the elector does.
func renewing(t *testing.T, l *lease) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			_ = l.Update(ctx, resourcelock.LeaderElectionRecord{HolderIdentity: "gw-0"})
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
}

// The active replica steps down once Envoy has not been ready for EnvoyNotReady, so that a ready standby can take the
// Lease, and not while Envoy is ready (issue #35).
func TestStepDownWhenEnvoyNotReady(t *testing.T) {
	key := newKey(t)
	wl := wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
	p := peer("b", newKey(t).PublicKey().String())
	a, k, _ := newAgent(t, testSite(), keySecret(key.String()), &wl, &p)
	up := &atomic.Bool{}
	up.Store(true)
	a.EnvoyAdmin = envoy(t, up)
	l := &lease{Interface: &recordLock{update: func(resourcelock.LeaderElectionRecord) {}}, before: a.stopLeading}
	renewing(t, l)

	var stepped atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.lead(context.Background(), l, func() { stepped.Store(true) })
	}()
	waitForInterfaces(t, k, 1)
	time.Sleep(EnvoyNotReady + time.Second)
	if stepped.Load() {
		t.Fatal("stepped down while Envoy is ready")
	}

	up.Store(false)
	start := time.Now()
	select {
	case <-done:
	case <-time.After(EnvoyNotReady + 5*time.Second):
		t.Fatalf("still leading %s after Envoy stopped being ready", time.Since(start))
	}
	if !stepped.Load() || time.Since(start) < EnvoyNotReady {
		t.Fatalf("stepped down %v after %s, want after %s", stepped.Load(), time.Since(start), EnvoyNotReady)
	}
	waitForInterfaces(t, k, 0)
}

// A replica joins the gateway election only while its Envoy is ready (issue #35).
func TestJoinOnlyWhenEnvoyReady(t *testing.T) {
	a, _, _ := newAgent(t, testSite())
	up := &atomic.Bool{}
	a.EnvoyAdmin = envoy(t, up)
	leases := k8sfake.NewClientset().CoordinationV1()
	a.Leases = leases
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	holder := func() string {
		l, err := leases.Leases(ns).Get(context.Background(), "strait-gateway", metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil {
			return ""
		}
		return *l.Spec.HolderIdentity
	}
	time.Sleep(2*RetryPeriod + RetryPeriod/2)
	if h := holder(); h != "" {
		t.Fatalf("Envoy not ready: the Lease is held by %q", h)
	}
	up.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for holder() != "gw-0" {
		if time.Now().After(deadline) {
			t.Fatalf("Envoy ready: the Lease is held by %q, want gw-0", holder())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A standby that joined the election while its Envoy was ready does not take the Lease once Envoy is not ready, and
// takes it once Envoy is ready again (RFC0005, requirement 33; issue #38).
func TestNoTakeoverWhileEnvoyNotReady(t *testing.T) {
	a, _, _ := newAgent(t, testSite())
	up := &atomic.Bool{}
	up.Store(true)
	a.EnvoyAdmin = envoy(t, up)
	// Another replica holds the Lease, for 1s from when this one first reads it.
	other := "gw-1"
	leases := k8sfake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "strait-gateway"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &other, LeaseDurationSeconds: ptr.To[int32](1)},
	}).CoordinationV1()
	a.Leases = leases
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	holder := func() string {
		l, err := leases.Leases(ns).Get(context.Background(), "strait-gateway", metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil {
			return ""
		}
		return *l.Spec.HolderIdentity
	}

	// Joined: wait for the first read of the Lease, then Envoy stops being ready.
	time.Sleep(RetryPeriod / 2)
	up.Store(false)
	time.Sleep(3 * RetryPeriod)
	if h := holder(); h != other {
		t.Fatalf("Envoy not ready, the other replica's Lease expired: the Lease is held by %q, want %q", h, other)
	}
	up.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for holder() != "gw-0" {
		if time.Now().After(deadline) {
			t.Fatalf("Envoy ready again: the Lease is held by %q, want gw-0", holder())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// memLock is a resourcelock.Interface in memory. Get shows holder.
type memLock struct {
	holder string
	writes int
}

func (l *memLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	return &resourcelock.LeaderElectionRecord{HolderIdentity: l.holder}, nil, nil
}

func (l *memLock) Create(_ context.Context, r resourcelock.LeaderElectionRecord) error {
	l.writes++
	l.holder = r.HolderIdentity
	return nil
}

func (l *memLock) Update(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	return l.Create(ctx, r)
}

func (*memLock) RecordEvent(string) {}
func (*memLock) Identity() string   { return "gw-0" }
func (*memLock) Describe() string   { return "memory/strait-gateway" }

// Readiness is checked before a write that takes the Lease, and never before a renewal: an active replica whose Envoy
// is not ready steps down after EnvoyNotReady, not at the renew deadline (RFC0005, requirements 33 and 34).
func TestLeaseChecksReadinessOnlyToTake(t *testing.T) {
	inner := &memLock{holder: "gw-1"}
	var ready atomic.Bool
	l := &lease{Interface: inner, before: func() {}, ready: func(context.Context) error {
		if !ready.Load() {
			return errors.New("503")
		}
		return nil
	}}
	ctx := context.Background()
	self := resourcelock.LeaderElectionRecord{HolderIdentity: "gw-0"}
	step := func(what string, write func() error, wantErr bool, wantWrites int) {
		t.Helper()
		err := write()
		if (err != nil) != wantErr || inner.writes != wantWrites {
			t.Fatalf("%s: error %v, %d writes; want error %v, %d writes", what, err, inner.writes, wantErr, wantWrites)
		}
	}
	if _, _, err := l.Get(ctx); err != nil {
		t.Fatal(err)
	}
	step("create while not ready", func() error { return l.Create(ctx, self) }, true, 0)
	step("take while not ready", func() error { return l.Update(ctx, self) }, true, 0)
	ready.Store(true)
	step("take while ready", func() error { return l.Update(ctx, self) }, false, 1)
	ready.Store(false)
	step("renew while not ready", func() error { return l.Update(ctx, self) }, false, 2)
	if _, _, err := l.Get(ctx); err != nil {
		t.Fatal(err)
	}
	step("renew after a read that shows this replica", func() error { return l.Update(ctx, self) }, false, 3)
	inner.holder = "gw-1"
	if _, _, err := l.Get(ctx); err != nil {
		t.Fatal(err)
	}
	step("take after a read that shows another holder", func() error { return l.Update(ctx, self) }, true, 3)
	ready.Store(true)
	step("take again while ready", func() error { return l.Update(ctx, self) }, false, 4)
	step("release", func() error { return l.Update(ctx, resourcelock.LeaderElectionRecord{}) }, false, 5)
	ready.Store(false)
	step("take after a release", func() error { return l.Update(ctx, self) }, true, 5)
	ready.Store(true)
	step("take in a new round", func() error { return l.Update(ctx, self) }, false, 6)
	l.reset()
	ready.Store(false)
	step("take after a new round starts", func() error { return l.Update(ctx, self) }, true, 6)
}

// A stall of the API server longer than the renew deadline hands the gateway over: the active replica removes its
// interfaces. Site.spec.gateway.lease sets a longer renew deadline, and the same stall then moves nothing (RFC0005,
// requirement 38). The stall here fails every request on the gateway Lease for 6s.
func TestLeaseTimingRidesOutAStall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lease    straitv1.GatewayLease
		handover bool
	}{
		{"defaults", straitv1.GatewayLease{}, true},
		{"renewDeadline 8s", straitv1.GatewayLease{
			Duration: metav1.Duration{Duration: 10 * time.Second}, RenewDeadline: metav1.Duration{Duration: 8 * time.Second},
			RetryPeriod: metav1.Duration{Duration: time.Second},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := newKey(t)
			wl := wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
			p := peer("b", newKey(t).PublicKey().String())
			site := testSite()
			site.Spec.Gateway.Lease = tc.lease
			a, k, _ := newAgent(t, site, keySecret(key.String()), &wl, &p)
			up := &atomic.Bool{}
			up.Store(true)
			a.EnvoyAdmin = envoy(t, up)
			cs := k8sfake.NewClientset()
			var stall atomic.Bool
			cs.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
				if stall.Load() {
					return true, nil, errors.New("injected: the API server does not answer")
				}
				return false, nil, nil
			})
			a.Leases = cs.CoordinationV1()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()
			defer func() {
				cancel()
				<-done
			}()
			waitForInterfaces(t, k, 1)

			stall.Store(true)
			removed := false
			for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
				if devs, _ := k.Devices(); len(devs) == 0 {
					removed = true
				}
			}
			stall.Store(false)
			if removed != tc.handover {
				t.Fatalf("a 6s stall removed the interfaces: %v, want %v", removed, tc.handover)
			}
			waitForInterfaces(t, k, 1)
		})
	}
}

// Each field of Site.spec.gateway.lease that is unset takes its default, as a Site written before 1.3.0 leaves them.
func TestLeaseTimingDefaults(t *testing.T) {
	if got, want := LeaseTiming(nil), (Timing{6 * time.Second, 4 * time.Second, time.Second}); got != want {
		t.Errorf("no Site: %+v, want %+v", got, want)
	}
	site := testSite()
	site.Spec.Gateway.Lease.RenewDeadline = metav1.Duration{Duration: 3 * time.Second}
	if got, want := LeaseTiming(site), (Timing{6 * time.Second, 3 * time.Second, time.Second}); got != want {
		t.Errorf("renewDeadline set: %+v, want %+v", got, want)
	}
}

// leadingAgent leads with one WireGuard interface in place. Once blockWrites is set, every write to the API server
// hangs until the test ends, and Envoy's /clusters hangs too, while its /ready answers.
func leadingAgent(t *testing.T) (a *Agent, k *fakeKernel, blockWrites *atomic.Bool) {
	t.Helper()
	SessionGrace = 300 * time.Millisecond
	t.Cleanup(func() { SessionGrace = 30 * time.Second })
	hung := make(chan struct{})
	blockWrites = &atomic.Bool{}
	block := func(ctx context.Context) error {
		if !blockWrites.Load() {
			return nil
		}
		select {
		case <-hung:
		case <-ctx.Done():
		}
		return context.DeadlineExceeded
	}
	key := newKey(t)
	wl := wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
	p := peer("b", newKey(t).PublicKey().String())
	a, k, _ = newAgent(t, testSite(), keySecret(key.String()), &wl, &p)
	inner := a.Client
	a.Client = interceptor.NewClient(inner.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := block(ctx); err != nil {
				return err
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			_, _ = io.WriteString(w, "LIVE\n")
			return
		}
		select {
		case <-hung:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(admin.Close)
	t.Cleanup(func() { close(hung) })
	a.EnvoyAdmin = admin.URL

	l := &lease{Interface: &recordLock{update: func(resourcelock.LeaderElectionRecord) {}}, before: a.stopLeading}
	renewing(t, l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.lead(ctx, l, func() {})
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForInterfaces(t, k, 1)
	return a, k, blockWrites
}

// The agent's liveness check passes while the API server and Envoy's admin hang: its session loop waits for neither.
// The test shortens the grace from 30s to 300ms: with that grace, a loop that missed one turn would fail it.
func TestAgentAliveWhileAPIServerAndEnvoyHang(t *testing.T) {
	a, _, blockWrites := leadingAgent(t)
	blockWrites.Store(true)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !a.Leading() {
			t.Fatal("the agent stopped leading")
		}
		if err := a.Alive(time.Now()); err != nil {
			t.Fatalf("liveness failed: %v", err)
		}
	}
}

// The agent's liveness check fails while a change of the interfaces hangs, and passes once it ends. A standby passes.
func TestAgentAliveFailsWhenTheKernelHangs(t *testing.T) {
	standby, _, _ := newAgent(t, testSite())
	if err := standby.Alive(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("a standby: %v", err)
	}
	a, _, _ := leadingAgent(t)
	a.kernel.Lock()
	time.Sleep(SessionGrace + 2*SessionInterval)
	err := a.Alive(time.Now())
	a.kernel.Unlock()
	if err == nil {
		t.Fatal("liveness passed while the kernel hung")
	}
	t.Logf("liveness: %v", err)
	time.Sleep(2 * SessionInterval)
	if err := a.Alive(time.Now()); err != nil {
		t.Fatalf("after the kernel answered: %v", err)
	}
	h := Probes(a.EnvoyAdmin, http.DefaultClient, func(time.Time) error { return errors.New("stuck") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz = %d while stuck, want 503", rec.Code)
	}
}

// A link whose health or active state changes is logged at the info level. Another change of its status is logged
// at V(1) only.
func TestLinkChangesLogAtInfo(t *testing.T) {
	var objs []client.Object
	for _, l := range fixtureLinks() {
		objs = append(objs, &l)
	}
	a, _, _ := newAgent(t, objs...)
	var mu sync.Mutex
	var lines []string
	a.Log = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{Verbosity: 0})
	logged := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := lines
		lines = nil
		return out
	}
	up := &atomic.Bool{}
	a.EnvoyAdmin = envoy(t, up)
	a.setLinks(map[string]string{"b-wg": links.Interface("b-wg")}, nil)
	ctx := context.Background()

	// Envoy is down: each link's status is written for the first time, unhealthy as before.
	a.reportHealth(ctx)
	if got := logged(); len(got) != 0 {
		t.Fatalf("logged at info with no change of health: %q", got)
	}
	up.Store(true)
	a.reportHealth(ctx)
	got := strings.Join(logged(), "\n")
	if !strings.Contains(got, `"link"="b-wg"`) || !strings.Contains(got, `"healthy"=true`) || !strings.Contains(got, `"active"=true`) {
		t.Fatalf("link b-wg turned healthy and active, logged at info: %q", got)
	}
}

// Each request of the agent to the API server ends within APITimeout, so one that the API server never answers does not
// hold its loop: the WireGuard sync's read of the Secret and its write of the Site's status, and the health report's
// write of each SiteLink's status. The test shortens APITimeout from 5s to 200ms (RFC0005, requirement 49; issue #61).
func TestAgentRequestsEnd(t *testing.T) {
	APITimeout = 200 * time.Millisecond
	t.Cleanup(func() { APITimeout = 5 * time.Second })
	// hangs is a request that the API server never answers: it ends when the client gives up.
	hangs := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, tc := range []struct {
		name string
		hang interceptor.Funcs
		run  func(*Agent, context.Context)
		// requests is the number of requests that hang in one run.
		requests int
	}{
		{"Secret read", interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return hangs(ctx)
			}
			return c.Get(ctx, key, obj, opts...)
		}}, (*Agent).syncWireGuard, 1},
		{"Site status", interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			return hangs(ctx)
		}}, (*Agent).syncWireGuard, 1},
		{"SiteLink status", interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			return hangs(ctx)
		}}, (*Agent).reportHealth, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := newKey(t)
			l1 := wgLink("to-b", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
			l2 := directLink("to-b-direct", "b", 10, "192.0.2.1")
			p := peer("b", newKey(t).PublicKey().String())
			a, _, _ := newAgent(t, testSite(), keySecret(key.String()), &l1, &l2, &p)
			a.Client = interceptor.NewClient(a.Client.(client.WithWatch), tc.hang)
			a.EnvoyAdmin = "http://127.0.0.1:1"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(done)
				tc.run(a, ctx)
			}()
			want := time.Duration(tc.requests) * APITimeout
			select {
			case <-done:
			case <-time.After(want + 2*time.Second):
				t.Fatalf("still waiting %s after the start, want done within %s", time.Since(start).Round(time.Millisecond), want)
			}
			if d := time.Since(start); d < want {
				t.Errorf("done after %s: no request hung", d)
			}
		})
	}
}
