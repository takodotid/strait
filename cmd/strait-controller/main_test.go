package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
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
	go func() { done <- mgr.Start(ctx) }()
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
