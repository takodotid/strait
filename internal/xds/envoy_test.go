//go:build envoy

// These tests run the configuration in a real Envoy, in docker: go test -tags envoy ./internal/xds/
package xds

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/takodotid/strait/api/v1alpha1"
)

const (
	envoyImage = "envoyproxy/envoy:distroless-v1.40.2"
	probeImage = "busybox:1.37"
)

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// containerName returns a unique name and removes the container when the test ends.
func containerName(t *testing.T, role string) string {
	name := fmt.Sprintf("strait-xds-%s-%d-%d", role, os.Getpid(), time.Now().UnixNano()%1e6)
	t.Cleanup(func() { docker("rm", "-f", name) })
	return name
}

// configDir writes files into a directory that Envoy's non-root user can read.
func configDir(t *testing.T, files map[string][]byte) string {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func validate(t *testing.T, dir, file string, args ...string) (string, error) {
	return docker(append([]string{"run", "--rm", "--name", containerName(t, "validate"), "--network", "none",
		"-v", dir + ":/cfg:ro", envoyImage, "--mode", "validate", "-c", "/cfg/" + file}, args...)...)
}

func TestEnvoyValidate(t *testing.T) {
	cfg, err := staticConfig(build(t, collect(t)))
	if err != nil {
		t.Fatal(err)
	}
	dir := configDir(t, map[string][]byte{
		"envoy.yaml":     cfg,
		"bootstrap.yaml": []byte(Bootstrap("strait-controller.strait-system.svc", Port)),
	})
	if out, err := validate(t, dir, "envoy.yaml"); err != nil {
		t.Fatalf("static configuration: %v\n%s", err, out)
	}
	if out, err := validate(t, dir, "bootstrap.yaml", "--service-node", "strait-gateway-0"); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}
}

// Envoy takes only IP addresses as endpoints, which is why Collect leaves out a link with a host name.
func TestEnvoyRejectsHostName(t *testing.T) {
	m := collect(t)
	m.Peers[0].Links[0].Address = "gateway-b.example"
	cfg, err := staticConfig(build(t, m))
	if err != nil {
		t.Fatal(err)
	}
	out, err := validate(t, configDir(t, map[string][]byte{"envoy.yaml": cfg}), "envoy.yaml")
	if err == nil {
		t.Fatal("Envoy accepted a host name as an endpoint")
	}
	t.Logf("Envoy rejects a host name endpoint: %s", lastLine(out))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// loopback is a gateway whose peer b is the gateway itself, so one Envoy with no network carries the whole path:
// import -> link -> export -> Envoy's admin, and the peer's health checks on the control port.
// Link lan, the first, points at an address that cannot be reached, so traffic must take link wg.
func loopback() Model {
	return Model{
		ControlPort: 30900, PeerPort: 14999, Controller: "127.0.0.1:9901",
		Exports: []Export{
			{Namespace: "app", Name: "admin", Port: 30901, Addresses: []string{"127.0.0.1"}, TargetPort: AdminPort},
			{Namespace: "app", Name: "empty", Port: 30902},
		},
		Imports: []Import{
			{Namespace: "app", Name: "admin-b", Port: 15000, Peer: "b", Export: "admin", RemotePort: 30901},
			{Namespace: "app", Name: "empty-b", Port: 15001, Peer: "b", Export: "empty", RemotePort: 30902},
		},
		Peers: []Peer{{Name: "b", ControlPort: 30900, Links: []Link{
			{Name: "lan", Address: "192.0.2.1", Priority: 0},
			{Name: "wg", Address: "127.0.0.1", Priority: 1},
		}}},
	}
}

type probe struct {
	t    *testing.T
	name string
}

var statusLine = regexp.MustCompile(`HTTP/1\.[01] (\d{3})`)

// request returns the status, body and headers of a request made from inside the Envoy container's network.
// The body is empty unless the status is 2xx.
func (p probe) request(method, url string) (int, string, string) {
	args := []string{"exec", p.name, "wget", "-q", "-S", "-T", "3", "-O", "-"}
	if method == "POST" {
		args = append(args, "--post-data", "x")
	}
	cmd := exec.Command("docker", append(args, url)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	m := statusLine.FindAllStringSubmatch(stderr.String(), -1)
	if len(m) == 0 {
		return 0, "", stderr.String()
	}
	var code int
	fmt.Sscan(m[len(m)-1][1], &code)
	return code, stdout.String(), stderr.String()
}

func (p probe) get(url string) (int, string) {
	code, body, _ := p.request("GET", url)
	return code, body
}

// eventually polls f until it returns "" or the time is up, and fails with f's last message.
func (p probe) eventually(what string, f func() string) {
	p.t.Helper()
	var msg string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if msg = f(); msg == "" {
			return
		}
	}
	p.t.Fatalf("%s: %s", what, msg)
}

// runEnvoy starts Envoy with the configuration in dir and a probe container in its network namespace.
func runEnvoy(t *testing.T, dir string, network string, args ...string) (envoy string, p probe) {
	envoy = containerName(t, "envoy")
	runArgs := append([]string{"run", "-d", "--name", envoy, "--network", network, "-v", dir + ":/cfg:ro", envoyImage, "-c", "/cfg/envoy.yaml"}, args...)
	if out, err := docker(runArgs...); err != nil {
		t.Fatalf("docker run envoy: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := docker("logs", "--tail", "60", envoy)
			t.Logf("envoy logs:\n%s", out)
		}
	})
	p = probe{t: t, name: containerName(t, "probe")}
	if out, err := docker("run", "-d", "--name", p.name, "--network", "container:"+envoy, probeImage, "sleep", "300"); err != nil {
		t.Fatalf("docker run probe: %v\n%s", err, out)
	}
	return envoy, p
}

// ready waits until Envoy has its initial configuration.
func (p probe) ready() {
	p.t.Helper()
	p.eventually("Envoy is not ready", func() string {
		if code, body := p.get("http://127.0.0.1:9901/ready"); code != 200 {
			return fmt.Sprintf("%d %s", code, body)
		}
		return ""
	})
}

func expect(t *testing.T, p probe, method, url string, code int, body string) {
	t.Helper()
	gotCode, gotBody, _ := p.request(method, url)
	if gotCode != code || (body != "" && strings.TrimSpace(gotBody) != body) {
		t.Errorf("%s %s = %d %q, want %d %q", method, url, gotCode, gotBody, code, body)
	}
}

// stat returns the value of one counter from Envoy's admin.
func (p probe) stat(name string) string {
	_, body := p.get("http://127.0.0.1:9901/stats?filter=^" + name + "$")
	_, v, _ := strings.Cut(strings.TrimSpace(body), ": ")
	return v
}

// healthFlags returns the health flags of each host of a cluster from Envoy's admin.
func healthFlags(p probe, cluster string) map[string]string {
	_, body := p.get("http://127.0.0.1:9901/clusters")
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		parts := strings.Split(line, "::")
		if len(parts) == 4 && parts[0] == cluster && parts[2] == "health_flags" {
			out[parts[1]] = parts[3]
		}
	}
	return out
}

// waitHealthy waits until each host's health flags start with the wanted ones.
func waitHealthy(p probe, cluster string, want map[string]string) {
	p.t.Helper()
	p.eventually(cluster+" health", func() string {
		got := healthFlags(p, cluster)
		for host, flags := range want {
			if !strings.HasPrefix(got[host], flags) {
				return fmt.Sprintf("%v, want %v", got, want)
			}
		}
		return ""
	})
}

func TestEnvoyRun(t *testing.T) {
	snap, err := Build(loopback(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := staticConfig(snap)
	if err != nil {
		t.Fatal(err)
	}
	_, p := runEnvoy(t, configDir(t, map[string][]byte{"envoy.yaml": cfg}), "none")
	p.ready()

	// The control port.
	expect(t, p, "GET", "http://127.0.0.1:30900/healthz", 200, "ok")
	expect(t, p, "GET", "http://127.0.0.1:30900/healthz/exports/app/admin", 200, "")
	expect(t, p, "GET", "http://127.0.0.1:30900/healthz/exports/app/empty", 503, "")
	expect(t, p, "GET", "http://127.0.0.1:30900/healthz/exports/app/unknown", 404, "")
	expect(t, p, "GET", "http://127.0.0.1:30900/stats", 404, "")
	expect(t, p, "POST", "http://127.0.0.1:30900/healthz", 404, "")
	expect(t, p, "POST", "http://127.0.0.1:30900/healthz/exports/app/admin", 404, "")
	expect(t, p, "POST", "http://127.0.0.1:30900/v1alpha1/observations", 404, "")
	// The controller here is Envoy's admin, which answers 404 for the path. Only that request reaches the cluster.
	if code, _ := p.get("http://127.0.0.1:30900/v1alpha1/observations"); code != 404 {
		t.Errorf("GET /v1alpha1/observations = %d, want the controller's 404", code)
	}
	if n := p.stat("cluster.controller.upstream_rq_total"); n != "1" {
		t.Errorf("the controller cluster took %s requests, want 1", n)
	}

	// Link lan cannot be reached, so priority 0 has no healthy host and traffic takes link wg.
	unreachable := "/failed_active_hc"
	waitHealthy(p, "peer/b", map[string]string{"192.0.2.1:30900": unreachable, "127.0.0.1:30900": "healthy"})
	waitHealthy(p, "import/app/admin-b", map[string]string{"192.0.2.1:30901": unreachable, "127.0.0.1:30901": "healthy"})
	// The peer's export has no endpoint, so the import has no healthy host on any link.
	waitHealthy(p, "import/app/empty-b", map[string]string{"192.0.2.1:30902": unreachable, "127.0.0.1:30902": unreachable})

	expect(t, p, "GET", "http://127.0.0.1:14999/peers/b/healthz", 200, "ok")
	expect(t, p, "GET", "http://127.0.0.1:14999/peers/c/healthz", 404, "")
	expect(t, p, "GET", "http://127.0.0.1:15000/ready", 200, "LIVE")
	expect(t, p, "GET", "http://127.0.0.1:30901/ready", 200, "LIVE")
	if code, _ := p.get("http://127.0.0.1:15001/ready"); code != 0 {
		t.Errorf("import with no healthy host answered %d", code)
	}
}

// TestEnvoyADS runs the Server and an Envoy started from Bootstrap, on the default docker network.
// The model is the loopback one, read from objects.
func TestEnvoyADS(t *testing.T) {
	out, err := docker("network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		t.Fatalf("docker network inspect: %v\n%s", err, out)
	}
	ctx := context.Background()
	slice := func(service string) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Namespace: "app", Name: service + "-1", Labels: map[string]string{discoveryv1.LabelServiceName: service}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("admin"), Port: ptr.To[int32](AdminPort)}},
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"127.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
		}
	}
	var objects []client.Object
	for i, name := range []string{"admin", "empty"} {
		objects = append(objects,
			&v1alpha1.Export{
				ObjectMeta: objectMeta("app", name),
				Spec:       v1alpha1.ExportSpec{Service: v1alpha1.ServicePortRef{Name: name, Port: AdminPort}},
				Status:     v1alpha1.ExportStatus{GatewayPort: 30901 + int32(i)},
			},
			&corev1.Service{ObjectMeta: objectMeta("app", name), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "admin", Port: AdminPort}}}},
			&v1alpha1.Import{
				ObjectMeta: objectMeta("app", name+"-b"),
				Spec:       v1alpha1.ImportSpec{Site: "b", Export: name},
				Status:     v1alpha1.ImportStatus{GatewayPort: 15000 + int32(i), RemotePort: 30901 + int32(i)},
			})
	}
	objects = append(objects,
		&v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: v1alpha1.SiteSpec{Gateway: v1alpha1.GatewaySpec{ControlPort: 30900, PeerPort: 14999}}},
		slice("admin"),
		&v1alpha1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.SitePeerSpec{ControlPort: 30900}},
		&v1alpha1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: v1alpha1.SiteLinkSpec{
			Peer: "b", Transport: v1alpha1.TransportDirect, Cost: 10, Encryption: v1alpha1.EncryptionNone,
			Direct: &v1alpha1.DirectLink{Address: "192.0.2.1"},
		}},
		&v1alpha1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: "wg"}, Spec: v1alpha1.SiteLinkSpec{
			Peer: "b", Transport: v1alpha1.TransportWireGuard, Cost: 100,
			WireGuard: &v1alpha1.WireGuardLink{Address: "127.0.0.2/32", PeerAddress: "127.0.0.1"},
		}},
	)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objects...).Build()

	addr := freeAddress(t, strings.TrimSpace(out))
	s := startServer(t, c, addr)
	host, port, _ := net.SplitHostPort(addr)
	xdsPort, _ := strconv.Atoi(port)
	node := "strait-gateway-ads"
	envoy, p := runEnvoy(t, configDir(t, map[string][]byte{"envoy.yaml": []byte(Bootstrap(host, xdsPort))}), "bridge", "--service-node", node)

	// The pod's address is known once the container runs. Until the Pod exists, the node gets no configuration.
	out, err = docker("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", envoy)
	if err != nil {
		t.Fatalf("docker inspect: %v\n%s", err, out)
	}
	ip := strings.TrimSpace(out)
	if err := c.Create(ctx, gatewayPod(node, ip)); err != nil {
		t.Fatal(err)
	}
	s.Trigger()
	p.ready()

	expect(t, p, "GET", "http://127.0.0.1:30900/healthz", 200, "ok")
	expect(t, p, "GET", "http://127.0.0.1:30900/healthz/exports/app/admin", 200, "")
	expect(t, p, "GET", "http://127.0.0.1:30900/healthz/exports/app/empty", 503, "")
	waitHealthy(p, "import/app/admin-b", map[string]string{"192.0.2.1:30901": "/failed_active_hc", "127.0.0.1:30901": "healthy"})
	expect(t, p, "GET", "http://"+ip+":15000/ready", 200, "LIVE")
	expect(t, p, "GET", "http://"+ip+":14999/peers/b/healthz", 200, "ok")
	// Import and peer listeners bind to the pod address only.
	if code, _ := p.get("http://127.0.0.1:15000/ready"); code != 0 {
		t.Errorf("the import listener answered %d on 127.0.0.1", code)
	}

	// An endpoint change reaches Envoy over EDS, with no listener or cluster change.
	added := p.stat("listener_manager.listener_added")
	if err := c.Create(ctx, slice("empty")); err != nil {
		t.Fatal(err)
	}
	s.Trigger()
	p.eventually("export app/empty has no endpoint", func() string {
		if code, _ := p.get("http://127.0.0.1:30900/healthz/exports/app/empty"); code != 200 {
			return fmt.Sprint(code)
		}
		return ""
	})
	waitHealthy(p, "import/app/empty-b", map[string]string{"127.0.0.1:30902": "healthy"})
	for name, want := range map[string]string{
		"listener_manager.listener_added":    added,
		"listener_manager.listener_modified": "0",
		"listener_manager.listener_removed":  "0",
		"cluster_manager.cluster_modified":   "0",
		"cluster_manager.cluster_removed":    "0",
	} {
		if got := p.stat(name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}
