package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// leaseAPI serves the gateway Lease of namespace ns over HTTP, as the API server does for coordinationv1client: get,
// create and update, with resource versions. hang tells, for each request, whether the server never answers it: the
// request then ends only when the client gives up, or the test ends.
type leaseAPI struct {
	mu    sync.Mutex
	lease *coordinationv1.Lease
	rv    int
	hang  func(method string) bool
	stop  chan struct{}
}

func newLeaseAPI(t *testing.T, lease *coordinationv1.Lease, hang func(method string) bool) (*leaseAPI, coordinationv1client.LeasesGetter) {
	t.Helper()
	s := &leaseAPI{lease: lease, hang: hang, stop: make(chan struct{})}
	if lease != nil {
		s.rv = 1
		lease.ResourceVersion = "1"
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(s.stop) })
	// The server reads JSON. client-go sends built-in kinds as protobuf unless told otherwise.
	c, err := coordinationv1client.NewForConfig(&rest.Config{Host: srv.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func (s *leaseAPI) holder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease == nil || s.lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *s.lease.Spec.HolderIdentity
}

func (s *leaseAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.hang(r.Method) {
		select {
		case <-r.Context().Done():
		case <-s.stop:
		}
		return
	}
	prefix := "/apis/coordination.k8s.io/v1/namespaces/" + ns + "/leases"
	name, _ := strings.CutPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if !strings.HasPrefix(r.URL.Path, prefix) || (name != "" && name != "strait-gateway") {
		status(w, http.StatusNotFound, metav1.StatusReasonNotFound, r.URL.Path+" not found")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if s.lease == nil {
			status(w, http.StatusNotFound, metav1.StatusReasonNotFound, "the Lease does not exist")
			return
		}
		write(w, http.StatusOK, s.lease)
	case http.MethodPost, http.MethodPut:
		var in coordinationv1.Lease
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			status(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
			return
		}
		switch {
		case r.Method == http.MethodPost && s.lease != nil:
			status(w, http.StatusConflict, metav1.StatusReasonAlreadyExists, "the Lease exists")
			return
		case r.Method == http.MethodPut && (s.lease == nil || in.ResourceVersion != s.lease.ResourceVersion):
			status(w, http.StatusConflict, metav1.StatusReasonConflict, "the Lease has changed")
			return
		}
		s.rv++
		in.Namespace, in.Name, in.ResourceVersion = ns, "strait-gateway", strconv.Itoa(s.rv)
		s.lease = &in
		code := http.StatusOK
		if r.Method == http.MethodPost {
			code = http.StatusCreated
		}
		write(w, code, s.lease)
	default:
		status(w, http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, r.Method)
	}
}

func write(w http.ResponseWriter, code int, l *coordinationv1.Lease) {
	out := l.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(out)
}

func status(w http.ResponseWriter, code int, reason metav1.StatusReason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: reason, Code: int32(code), Message: msg,
	})
}

// A request on the gateway Lease that the API server never answers does not hold a standby's takeover: the request ends
// at its timeout, half the renew deadline, and the standby tries again. Another replica, gw-1, holds the Lease and has
// stopped renewing it. The standby takes it within lease.duration + 4.4 x lease.retryPeriod + that timeout of when it
// joins the election (RFC0005, requirements 49 and 50; issue #61). The cases hang the standby's first read of the
// Lease, and its first write that takes it.
func TestStandbyTakesTheLeaseWhileARequestHangs(t *testing.T) {
	timing := straitv1.GatewayLease{
		Duration:      metav1.Duration{Duration: 2 * time.Second},
		RenewDeadline: metav1.Duration{Duration: time.Second},
		RetryPeriod:   metav1.Duration{Duration: 200 * time.Millisecond},
	}
	timeout := timing.RenewDeadline.Duration / 2
	bound := timing.Duration.Duration + 44*timing.RetryPeriod.Duration/10 + timeout
	// A request to the test's server and the agent's start take milliseconds. The rest is room for a busy runner.
	const slack = time.Second
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			site := testSite()
			site.Spec.Gateway.Lease = timing
			a, _, _ := newAgent(t, site)
			up := &atomic.Bool{}
			up.Store(true)
			a.EnvoyAdmin = envoy(t, up)
			now := metav1.NewMicroTime(time.Now())
			var hung atomic.Int32
			api, leases := newLeaseAPI(t, &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "strait-gateway"},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("gw-1"), LeaseDurationSeconds: ptr.To[int32](2),
					AcquireTime: &now, RenewTime: &now},
			}, func(m string) bool { return m == method && hung.Add(1) == 1 })
			a.Leases = leases

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- a.Run(ctx) }()
			defer func() {
				cancel()
				<-done
			}()
			for api.holder() != "gw-0" {
				if time.Since(start) > bound+slack {
					t.Fatalf("the Lease is held by %q %s after the standby joined, want gw-0 within %s; requests hung: %d",
						api.holder(), time.Since(start).Round(time.Millisecond), bound, min(hung.Load(), 1))
				}
				time.Sleep(20 * time.Millisecond)
			}
			if hung.Load() == 0 {
				t.Fatalf("no %s request was hung", method)
			}
			t.Logf("took the Lease %s after joining, bound %s", time.Since(start).Round(time.Millisecond), bound)
		})
	}
}
