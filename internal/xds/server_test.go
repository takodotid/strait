package xds

import (
	"context"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// freeAddress returns host:port of a port that was free a moment ago.
func freeAddress(t *testing.T, host string) string {
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func gatewayPod(name, ip string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: objectMeta("strait", name)}
	p.Status.PodIP = ip
	return p
}

// startServer runs a Server until the test ends.
func startServer(t *testing.T, c client.Client, addr string) *Server {
	s := NewServer(c, "strait", addr, "controller.example:8080")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return s
}

type adsStream struct {
	t *testing.T
	discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient
	cancel context.CancelFunc
}

func dial(t *testing.T, addr, node string) adsStream {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var stream discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if stream, err = discovery.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	a := adsStream{t: t, AggregatedDiscoveryService_StreamAggregatedResourcesClient: stream, cancel: cancel}
	a.request(node, "", "")
	return a
}

func (a adsStream) request(node, version, nonce string) {
	a.t.Helper()
	if err := a.Send(&discovery.DiscoveryRequest{
		Node: &core.Node{Id: node, Cluster: NodeCluster}, TypeUrl: resource.ClusterType,
		VersionInfo: version, ResponseNonce: nonce,
	}); err != nil {
		a.t.Fatal(err)
	}
}

// recv returns the next response, and fails when none comes within the timeout.
func (a adsStream) recv(timeout time.Duration) *discovery.DiscoveryResponse {
	a.t.Helper()
	type result struct {
		r   *discovery.DiscoveryResponse
		err error
	}
	c := make(chan result, 1)
	go func() {
		r, err := a.Recv()
		c <- result{r, err}
	}()
	select {
	case r := <-c:
		if r.err != nil {
			a.t.Fatal(r.err)
		}
		return r.r
	case <-time.After(timeout):
		a.t.Fatal("no response")
		return nil
	}
}

func TestServer(t *testing.T) {
	c := fixture(t)
	if err := c.Create(context.Background(), gatewayPod("strait-gateway-0", podIP)); err != nil {
		t.Fatal(err)
	}
	addr := freeAddress(t, "127.0.0.1")
	s := startServer(t, c, addr)

	// The first request is answered from a snapshot built when the stream opened.
	a := dial(t, addr, "strait-gateway-0")
	r := a.recv(time.Second)
	if len(r.GetResources()) != 6 || r.GetVersionInfo() == "" {
		t.Fatalf("got %d clusters, version %q", len(r.GetResources()), r.GetVersionInfo())
	}
	a.request("strait-gateway-0", r.GetVersionInfo(), r.GetNonce())

	// A change and a trigger give a new version within the debounce.
	if err := c.Delete(context.Background(), &straitv1.Export{ObjectMeta: objectMeta("app", "redis")}); err != nil {
		t.Fatal(err)
	}
	s.Trigger()
	s.Trigger()
	r2 := a.recv(time.Second)
	if len(r2.GetResources()) != 5 || r2.GetVersionInfo() == r.GetVersionInfo() {
		t.Fatalf("got %d clusters, version %q", len(r2.GetResources()), r2.GetVersionInfo())
	}

	// A node whose pod does not exist gets nothing.
	dial(t, addr, "strait-gateway-9")
	time.Sleep(100 * time.Millisecond)
	if _, err := s.cache.GetSnapshot("strait-gateway-9"); err == nil {
		t.Error("a node with no pod has a snapshot")
	}

	// A node is forgotten when its last stream closes.
	a.cancel()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := s.cache.GetSnapshot("strait-gateway-0"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the snapshot of a closed node is kept")
		}
	}
}

func TestBootstrapFile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/envoy-bootstrap.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := Bootstrap("XDS_ADDRESS", Port); string(b) != want {
		t.Fatalf("deploy/envoy-bootstrap.yaml differs from Bootstrap(\"XDS_ADDRESS\", Port):\n%s", want)
	}
}

// A request with an error detail rejects the last configuration of its type. A node keeps the rejection until it
// accepts a configuration of that type, or its last stream closes. After a rejection, the server sends that type again
// only once the configuration changes: an answer at once would be the same configuration, rejected again.
func TestRejections(t *testing.T) {
	c := fixture(t)
	if err := c.Create(context.Background(), gatewayPod("strait-gateway-0", podIP)); err != nil {
		t.Fatal(err)
	}
	addr := freeAddress(t, "127.0.0.1")
	s := startServer(t, c, addr)
	changes := make(chan struct{}, 10)
	s.Rejected = func() { changes <- struct{}{} }
	changed := func(want bool) {
		t.Helper()
		select {
		case <-changes:
			if !want {
				t.Fatal("Rejected was called")
			}
		case <-time.After(300 * time.Millisecond):
			if want {
				t.Fatal("Rejected was not called")
			}
		}
	}
	before := testutil.ToFloat64(RejectedTotal.WithLabelValues("Cluster"))

	a := dial(t, addr, "strait-gateway-0")
	responses := make(chan *discovery.DiscoveryResponse, 100)
	go func() {
		for {
			r, err := a.Recv()
			if err != nil {
				close(responses)
				return
			}
			responses <- r
		}
	}()
	next := func(want bool) *discovery.DiscoveryResponse {
		t.Helper()
		select {
		case r, ok := <-responses:
			if want && ok {
				return r
			}
			if !want && ok {
				t.Fatalf("a response came: version %s", r.GetVersionInfo())
			}
		case <-time.After(300 * time.Millisecond):
		}
		if want {
			t.Fatal("no response")
		}
		return nil
	}
	r := next(true)
	nack := func(nonce string) {
		t.Helper()
		if err := a.Send(&discovery.DiscoveryRequest{
			Node: &core.Node{Id: "strait-gateway-0", Cluster: NodeCluster}, TypeUrl: resource.ClusterType,
			ResponseNonce: nonce, ErrorDetail: &status.Status{Code: 3, Message: "Proto constraint validation failed"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	nack(r.GetNonce())
	changed(true)
	want := []string{"pod strait-gateway-0 rejected Cluster: Proto constraint validation failed"}
	if got := s.Rejections(); !slices.Equal(got, want) {
		t.Fatalf("Rejections() = %q, want %q", got, want)
	}
	if got := testutil.ToFloat64(RejectedTotal.WithLabelValues("Cluster")) - before; got != 1 {
		t.Errorf("strait_xds_rejected_total{type=Cluster} grew by %v, want 1", got)
	}
	next(false)

	// A rejection with an older nonce is no answer to the last response.
	nack("stale")
	changed(false)
	if got := testutil.ToFloat64(RejectedTotal.WithLabelValues("Cluster")) - before; got != 1 {
		t.Errorf("strait_xds_rejected_total{type=Cluster} grew by %v, want 1", got)
	}

	// A change of the configuration is sent, and its acceptance clears the rejection.
	if err := c.Delete(context.Background(), &straitv1.Export{ObjectMeta: objectMeta("app", "redis")}); err != nil {
		t.Fatal(err)
	}
	s.Trigger()
	r = next(true)
	a.request("strait-gateway-0", r.GetVersionInfo(), r.GetNonce())
	changed(true)
	if got := s.Rejections(); len(got) != 0 {
		t.Fatalf("after an acceptance, Rejections() = %q", got)
	}

	// The node's last stream closing clears it too.
	nack(r.GetNonce())
	changed(true)
	a.cancel()
	changed(true)
	if got := s.Rejections(); len(got) != 0 {
		t.Fatalf("after the stream closed, Rejections() = %q", got)
	}
}
