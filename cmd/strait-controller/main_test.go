package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/controller"
	"github.com/takodotid/strait/internal/exchange"
	"github.com/takodotid/strait/internal/observe"
	"github.com/takodotid/strait/internal/xds"
)

// memoryLock is a leader election lock held in memory.
type memoryLock struct {
	mu     sync.Mutex
	id     string
	record *resourcelock.LeaderElectionRecord
}

func (l *memoryLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.record == nil {
		return nil, nil, apierrors.NewNotFound(coordinationv1.Resource("leases"), leaderElectionID)
	}
	r := *l.record
	raw, err := json.Marshal(r)
	return &r, raw, err
}

func (l *memoryLock) Create(_ context.Context, r resourcelock.LeaderElectionRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record = &r
	return nil
}

func (l *memoryLock) Update(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	return l.Create(ctx, r)
}

func (l *memoryLock) RecordEvent(string) {}
func (l *memoryLock) Identity() string   { return l.id }
func (l *memoryLock) Describe() string   { return "memory/" + leaderElectionID }
func (l *memoryLock) holder() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.record == nil {
		return ""
	}
	return l.record.HolderIdentity
}

// A controller that stops releases its Lease, so the next one takes it at once instead of after the lease duration.
// It releases it only after its leader-only runnables have stopped (issue #27).
func TestStopReleasesLease(t *testing.T) {
	lock := &memoryLock{id: "old"}
	scheme := runtime.NewScheme()
	opts := managerOptions(scheme, lock, "strait-system", "0", "0", true)
	// A shutdown timeout would release the Lease while a runnable that has not stopped still runs.
	if d := opts.GracefulShutdownTimeout; d == nil || *d >= 0 {
		t.Fatalf("GracefulShutdownTimeout = %v, want a negative duration: wait for every runnable", d)
	}
	opts.NewCache = func(*rest.Config, cache.Options) (cache.Cache, error) {
		return &informertest.FakeInformers{Scheme: scheme}, nil
	}
	opts.MapperProvider = func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return meta.NewDefaultRESTMapper(nil), nil }
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	// A leader-only runnable that takes a while to stop, and notes who holds the Lease once it has.
	heldAtStop := make(chan string, 1)
	if err := mgr.Add(leaderOnly(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond)
		heldAtStop <- lock.holder()
		return nil
	}))); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- start(ctx, mgr, lock, true) }()
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("the manager stopped before it was elected: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not elected within 10s")
	}
	if h := lock.holder(); h != "old" {
		t.Fatalf("elected, and the Lease is held by %q, want old", h)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the manager did not stop within 10s")
	}
	if h := <-heldAtStop; h != "old" {
		t.Errorf("when the leader-only runnable stopped, the Lease was held by %q, want old: released too early", h)
	}
	if h := lock.holder(); h != "" {
		t.Errorf("after stop, the Lease is held by %q, want released", h)
	}
}

// renewFails is a lock whose renewals fail once fail is set. A release, an update with no holder, still succeeds.
type renewFails struct {
	*memoryLock
	fail atomic.Bool
}

func (l *renewFails) Update(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	if r.HolderIdentity != "" && l.fail.Load() {
		return errors.New("injected: the renewal fails")
	}
	return l.memoryLock.Update(ctx, r)
}

// A controller that cannot renew the Lease stops leading and leaves the Lease held: the manager does not wait for its
// leader-only runnables on that path, so a release would let the next controller start while they still run
// (RFC0004, requirement 16).
func TestLostLeaseNotReleased(t *testing.T) {
	lock := &renewFails{memoryLock: &memoryLock{id: "old"}}
	scheme := runtime.NewScheme()
	opts := managerOptions(scheme, lock, "strait-system", "0", "0", true)
	opts.LeaseDuration, opts.RenewDeadline, opts.RetryPeriod = ptr.To(time.Second), ptr.To(400*time.Millisecond), ptr.To(100*time.Millisecond)
	opts.NewCache = func(*rest.Config, cache.Options) (cache.Cache, error) {
		return &informertest.FakeInformers{Scheme: scheme}, nil
	}
	opts.MapperProvider = func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return meta.NewDefaultRESTMapper(nil), nil }
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	// A leader-only runnable that is still running when the manager gives up.
	if err := mgr.Add(leaderOnly(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(time.Hour)
		return nil
	}))); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- start(context.Background(), mgr, lock, true) }()
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("the manager stopped before it was elected: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not elected within 10s")
	}
	lock.fail.Store(true)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the manager stopped with no error after the Lease was lost")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the manager did not stop within 10s of the first failed renewal")
	}
	if h := lock.holder(); h != "old" {
		t.Errorf("after the Lease was lost, it is held by %q, want old: released while a leader-only runnable still ran", h)
	}
}

// The observation server returns only once a request in flight has ended, so it answers nothing after the Lease is
// released.
func TestServeWaitsForRequests(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inFlight, release := make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inFlight)
		<-release
		_, _ = w.Write([]byte("ok"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, lis, handler) }()

	answered := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + lis.Addr().String())
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	<-inFlight
	cancel()
	select {
	case <-done:
		t.Fatal("serve returned while a request was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if code := <-answered; code != http.StatusOK {
		t.Errorf("the request in flight got %d, want 200", code)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return once the request ended")
	}
}

// The document server, the poller, the SitePeer status writer and the xDS server run on the elected controller only
// (RFC0004, requirement 12).
func TestRunnablesNeedLeaderElection(t *testing.T) {
	poller := &exchange.Poller{}
	list := runnables("127.0.0.1:0", func() exchange.Document { return exchange.Document{} }, poller, nil, xds.NewServer(nil, "strait-system", "127.0.0.1:0", ""))
	if len(list) != 4 {
		t.Fatalf("%d runnables, want the document server, the poller, the status writer and the xDS server", len(list))
	}
	for i, r := range list {
		l, ok := r.(manager.LeaderElectionRunnable)
		if !ok || !l.NeedLeaderElection() {
			t.Errorf("runnable %d (%T) runs without the Lease", i, r)
		}
	}
}

// The controller polls a peer through the active gateway's peer listener, so the poll takes the link that the gateway
// picks for traffic. It has no URL while no gateway is active (RFC0004, requirement 10). That the peer listener and
// each peer's cluster follow the link order of traffic is TestBuildPeers and TestBuildClusters in internal/xds.
func TestPeerURL(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		gateway string
		want    string
	}{
		{"10.0.9.9", "http://10.0.9.9:14999/peers/b"},
		{"fd00::9", "http://[fd00::9]:14999/peers/b"},
		{"", ""},
	} {
		site := &straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
		site.Spec.Gateway.PeerPort = 14999
		site.Status.Gateway.Address = tc.gateway
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(site).Build()
		got, err := peerURL(context.Background(), c, "b")
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("gateway %q: URL %q, want an error", tc.gateway, got)
		case tc.want != "" && (err != nil || got != tc.want):
			t.Errorf("gateway %q: URL %q, error %v; want %q", tc.gateway, got, err, tc.want)
		}
	}
}

// Each reconcile has a time limit, so a request that hangs does not hold the one worker of its kind, and a site whose
// reconciles hang ages its observations at its peers instead of serving them forever (RFC0004, requirement 15).
func TestReconcileTimeout(t *testing.T) {
	opts := managerOptions(runtime.NewScheme(), &memoryLock{id: "a"}, "strait-system", "0", "0", true)
	if d := opts.Controller.ReconciliationTimeout; d != 10*time.Second {
		t.Errorf("ReconciliationTimeout = %v, want 10s", d)
	}
}

// The document ages each member by the local Site's peerTimeout (RFC0004, requirement 15).
func TestLocalDocumentAgesByPeerTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	site := &straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	site.Spec.Exchange.PeerTimeout = metav1.Duration{Duration: 6 * time.Second}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(site).Build()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	local := &exchange.Local{}
	local.SetService("app", "pg", "pg-a", []observe.Observation{{
		Member: "pg-a", Site: "a", Role: straitv1.RolePrimary, Eligible: true, State: straitv1.StateStable, ObservedAt: at,
	}})
	for _, tc := range []struct {
		after time.Duration
		want  straitv1.State
	}{
		{6 * time.Second, straitv1.StateStable},
		{7 * time.Second, straitv1.StateUnknown},
	} {
		d := localDocument(c, local, func() time.Time { return at.Add(tc.after) })()
		if d.Site != "a" || len(d.Services) != 1 || d.Services[0].Members[0].State != tc.want {
			t.Errorf("%v after the observation: %+v, want the member %s", tc.after, d, tc.want)
		}
	}
}

// The controller watches CloudNativePG's kinds only while the cnpg plugin is on and their CRDs exist (RFC0003,
// requirement 13).
func TestCNPGWatched(t *testing.T) {
	withCRDs := meta.NewDefaultRESTMapper(nil)
	withCRDs.Add(controller.CNPGCluster, meta.RESTScopeNamespace)
	for _, tt := range []struct {
		name   string
		on     bool
		mapper meta.RESTMapper
		want   bool
	}{
		{"on, CRDs", true, withCRDs, true},
		{"on, no CRDs", true, meta.NewDefaultRESTMapper(nil), false},
		{"off, CRDs", false, withCRDs, false},
		{"off, no CRDs", false, meta.NewDefaultRESTMapper(nil), false},
	} {
		if got := cnpgWatched(tt.on, tt.mapper); got != tt.want {
			t.Errorf("%s: watched %v, want %v", tt.name, got, tt.want)
		}
	}
}

// --namespaces lists the namespaces of the writes, and the gateway's is added. Empty writes in every namespace (RFC0007,
// requirement 16).
func TestWriteNamespaces(t *testing.T) {
	for _, tt := range []struct {
		list string
		want []string
	}{
		{"", nil},
		{" , ", nil},
		{"db,web", []string{"db", "web", "strait-system"}},
		{" db , ", []string{"db", "strait-system"}},
	} {
		if got := writeNamespaces(tt.list, "strait-system"); !slices.Equal(got, tt.want) {
			t.Errorf("%q: %v, want %v", tt.list, got, tt.want)
		}
	}
}

// The liveness check passes while every write to the API server hangs, and while the peer accepts connections but
// never answers, as over a link that is down. Its loops read only the cache and time out each poll, so they keep their
// pace. The test shortens the grace from 30s to 300ms: with that grace, a loop that missed one turn would fail it.
func TestLivenessWhileAPIServerAndLinksHang(t *testing.T) {
	exchange.LoopGrace, xds.LoopGrace = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { exchange.LoopGrace, xds.LoopGrace = 30*time.Second, 30*time.Second })

	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	block := func(ctx context.Context) error {
		select {
		case <-hung:
		case <-ctx.Done():
		}
		return context.DeadlineExceeded
	}
	// A peer that accepts and never answers.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	peerPort, _ := strconv.Atoi(port)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	site := &straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	site.Spec.Gateway.PeerPort = int32(peerPort)
	site.Spec.Exchange = straitv1.ExchangeSpec{PollInterval: metav1.Duration{Duration: 100 * time.Millisecond}, PeerTimeout: metav1.Duration{Duration: time.Second}}
	site.Status.Gateway.Address = "127.0.0.1"
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(site, &straitv1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "b"}}).
		WithStatusSubresource(&straitv1.Site{}, &straitv1.SitePeer{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return block(ctx)
			},
			Update: func(ctx context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return block(ctx)
			},
			Patch: func(ctx context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return block(ctx)
			},
			SubResourcePatch: func(ctx context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return block(ctx)
			},
			SubResourceUpdate: func(ctx context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return block(ctx)
			},
		}).Build()

	xdsServer := xds.NewServer(c, "strait-system", "127.0.0.1:0", "controller.example:8082")
	poller := newPoller(c, func(string) { xdsServer.Trigger() })
	ctx, cancel := context.WithCancel(context.Background())
	// The loops read LoopGrace, so they stop before the cleanup above sets it back.
	var loops sync.WaitGroup
	t.Cleanup(func() { cancel(); loops.Wait() })
	for _, r := range runnables("127.0.0.1:0", func() exchange.Document { return exchange.Document{} }, poller, c, xdsServer) {
		loops.Go(func() { _ = r.Start(ctx) })
	}
	check := loopsAlive(poller, xdsServer)
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if err := check(nil); err != nil {
			t.Fatalf("liveness failed: %v", err)
		}
	}
	if _, st := poller.Snapshot("b"); st.LastError == "" || !st.LastSuccess.IsZero() {
		t.Fatalf("the peer was read, or no poll failed: %+v", st)
	}
}

// The liveness check fails once a loop stops: here the poller, stuck in a read.
func TestLivenessFailsWhenALoopStops(t *testing.T) {
	exchange.LoopGrace = 100 * time.Millisecond
	t.Cleanup(func() { exchange.LoopGrace = 30 * time.Second })
	stuck := make(chan struct{})
	calls := atomic.Int32{}
	p := &exchange.Poller{
		Peers: func() []string { return nil },
		URL:   func(string) (string, error) { return "", errors.New("none") },
		Exchange: func() straitv1.ExchangeSpec {
			if calls.Add(1) > 1 {
				<-stuck
			}
			return straitv1.ExchangeSpec{PollInterval: metav1.Duration{Duration: 50 * time.Millisecond}}
		},
	}
	check := loopsAlive(p)
	if err := check(nil); err != nil {
		t.Fatalf("before Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// The poller reads LoopGrace, so it stops before the cleanup above sets it back.
	stopped := make(chan struct{})
	t.Cleanup(func() { cancel(); close(stuck); <-stopped })
	go func() { defer close(stopped); _ = p.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)
	if err := check(nil); err != nil {
		t.Fatalf("while on time: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := check(nil); err == nil {
		t.Fatal("liveness passed while the poller was stuck")
	} else {
		t.Logf("liveness: %v", err)
	}
}
