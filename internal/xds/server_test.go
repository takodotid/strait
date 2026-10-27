package xds

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/strait/api/v1alpha1"
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
	if err := c.Delete(context.Background(), &v1alpha1.Export{ObjectMeta: objectMeta("app", "redis")}); err != nil {
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
