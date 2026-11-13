//go:build envoy

// These tests run the configuration in a real Envoy, in docker: make test-envoy.
package xds

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/gateway"
)

// probeImage runs the probes next to Envoy, pinned by the digest of its multi-platform index.
const probeImage = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// envoyImage is the Envoy image that the chart runs, by digest.
var envoyImage = func() string {
	data, err := os.ReadFile("../../deploy/helm/strait/values.yaml")
	if err != nil {
		panic(err)
	}
	var v struct {
		Envoy struct {
			Image struct{ Repository, Tag string }
		}
	}
	if err := yaml.Unmarshal(data, &v); err != nil {
		panic(err)
	}
	if v.Envoy.Image.Repository == "" || !strings.Contains(v.Envoy.Image.Tag, "@sha256:") {
		panic("values.yaml: envoy.image needs a repository and a tag with a digest")
	}
	return v.Envoy.Image.Repository + ":" + v.Envoy.Image.Tag
}()

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

// adsEnvoy runs the Server and an Envoy started from Bootstrap, on the default docker network, and waits until Envoy
// is ready. The model is the loopback one, read from objects. It returns the server, its objects, the probe, Envoy's
// node ID and the address of the Envoy container. args are more Envoy flags.
func adsEnvoy(t *testing.T, args ...string) (*Server, client.Client, probe, string, string) {
	out, err := docker("network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		t.Fatalf("docker network inspect: %v\n%s", err, out)
	}
	ctx := context.Background()
	var objects []client.Object
	for i, name := range []string{"admin", "empty"} {
		objects = append(objects,
			&straitv1.Export{
				ObjectMeta: objectMeta("app", name),
				Spec:       straitv1.ExportSpec{Service: straitv1.ServicePortRef{Name: name, Port: AdminPort}},
				Status:     straitv1.ExportStatus{GatewayPort: 30901 + int32(i)},
			},
			&corev1.Service{ObjectMeta: objectMeta("app", name), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "admin", Port: AdminPort}}}},
			&straitv1.Import{
				ObjectMeta: objectMeta("app", name+"-b"),
				Spec:       straitv1.ImportSpec{Site: "b", Export: name},
				Status:     straitv1.ImportStatus{GatewayPort: 15000 + int32(i), RemotePort: 30901 + int32(i)},
			})
	}
	objects = append(objects,
		&straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: straitv1.SiteSpec{Gateway: straitv1.GatewaySpec{ControlPort: 30900, PeerPort: 14999}}},
		slice("admin"),
		&straitv1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: straitv1.SitePeerSpec{ControlPort: 30900}},
		&straitv1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: straitv1.SiteLinkSpec{
			Peer: "b", Transport: straitv1.TransportDirect, Cost: 10, Encryption: straitv1.EncryptionNone,
			Direct: &straitv1.DirectLink{Address: "192.0.2.1"},
		}},
		&straitv1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: "wg"}, Spec: straitv1.SiteLinkSpec{
			Peer: "b", Transport: straitv1.TransportWireGuard, Cost: 100,
			WireGuard: &straitv1.WireGuardLink{Address: "127.0.0.2/32", PeerAddress: "127.0.0.1"},
		}},
	)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objects...).Build()

	addr := freeAddress(t, strings.TrimSpace(out))
	s := startServer(t, c, addr)
	host, port, _ := net.SplitHostPort(addr)
	xdsPort, _ := strconv.Atoi(port)
	node := "strait-gateway-ads"
	envoy, p := runEnvoy(t, configDir(t, map[string][]byte{"envoy.yaml": []byte(Bootstrap(host, xdsPort))}), "bridge", append([]string{"--service-node", node}, args...)...)

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
	return s, c, p, node, ip
}

// slice is an EndpointSlice of a Service of the loopback model, with Envoy's admin as its one endpoint.
func slice(service string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "app", Name: service + "-1", Labels: map[string]string{discoveryv1.LabelServiceName: service}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("admin"), Port: ptr.To[int32](AdminPort)}},
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"127.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
	}
}

// TestEnvoyADS runs the Server and an Envoy over ADS.
func TestEnvoyADS(t *testing.T) {
	ctx := context.Background()
	s, c, p, _, ip := adsEnvoy(t)

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

// A configuration that Envoy rejects shows in Rejections and in the counter, with Envoy's error. Envoy keeps serving
// the configuration it accepted last. The next configuration that it accepts clears the rejection.
func TestEnvoyRejection(t *testing.T) {
	s, _, p, node, _ := adsEnvoy(t)
	before := testutil.ToFloat64(RejectedTotal.WithLabelValues("Cluster"))
	good, err := s.cache.GetSnapshot(node)
	if err != nil {
		t.Fatal(err)
	}
	resources := map[resource.Type][]types.Resource{}
	for _, typ := range []resource.Type{resource.ListenerType, resource.ClusterType, resource.EndpointType} {
		for _, r := range good.GetResources(typ) {
			r = proto.Clone(r)
			if c, ok := r.(*cluster.Cluster); ok {
				// Envoy requires a connect timeout above 0.
				c.ConnectTimeout = durationpb.New(-time.Second)
			}
			resources[typ] = append(resources[typ], r)
		}
	}
	bad, err := cache.NewSnapshot("rejected", resources)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cache.SetSnapshot(context.Background(), node, bad); err != nil {
		t.Fatal(err)
	}
	p.eventually("Envoy rejects the clusters", func() string {
		got := s.Rejections()
		if len(got) == 1 && strings.HasPrefix(got[0], "pod "+node+" rejected Cluster: ") {
			return ""
		}
		return fmt.Sprintf("Rejections() = %q", got)
	})
	t.Logf("rejection: %s", s.Rejections()[0])
	// Envoy rejects the configuration once: it is not sent again until it changes.
	time.Sleep(2 * time.Second)
	if got := testutil.ToFloat64(RejectedTotal.WithLabelValues("Cluster")) - before; got != 1 {
		t.Errorf("strait_xds_rejected_total{type=Cluster} grew by %v, want 1", got)
	}
	if got := p.stat("cluster_manager.cds.update_rejected"); got != "1" {
		t.Errorf("Envoy's cluster_manager.cds.update_rejected = %s, want 1", got)
	}
	expect(t, p, "GET", "http://127.0.0.1:30900/healthz/exports/app/admin", 200, "")

	s.Trigger()
	p.eventually("Envoy accepts the clusters again", func() string {
		if got := s.Rejections(); len(got) > 0 {
			return fmt.Sprintf("Rejections() = %q", got)
		}
		return ""
	})
}

// The Envoy stats that the gateway agent serves have no address or port as a label value, and no object's name in a
// metric's name, also for objects whose names hold a dot. Envoy's /clusters counts the open connections of an import
// at its link's address, which the agent sums per link.
func TestEnvoyStatsLabels(t *testing.T) {
	m := loopback()
	m.Exports = append(m.Exports, Export{Namespace: "app", Name: "a.b", Port: 30903, Addresses: []string{"127.0.0.1"}, TargetPort: AdminPort})
	m.Imports = append(m.Imports, Import{Namespace: "app", Name: "x.y-b", Port: 15002, Peer: "b", Export: "a.b", RemotePort: 30903})
	snap, err := Build(m, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := staticConfig(snap)
	if err != nil {
		t.Fatal(err)
	}
	_, p := runEnvoy(t, configDir(t, map[string][]byte{"envoy.yaml": cfg}), "none")
	p.ready()
	waitHealthy(p, "import/app/x.y-b", map[string]string{"127.0.0.1:30903": "healthy"})
	expect(t, p, "GET", "http://127.0.0.1:15002/ready", 200, "LIVE")
	expect(t, p, "GET", "http://127.0.0.1:14999/peers/b/healthz", 200, "ok")
	// One connection through the import stays open while the stats are read.
	if out, err := docker("exec", "-d", p.name, "sh", "-c", "sleep 60 | nc 127.0.0.1 15002"); err != nil {
		t.Fatalf("open a connection: %v\n%s", err, out)
	}

	_, body := p.get("http://127.0.0.1:9901/stats/prometheus?filter=" + url.QueryEscape(gateway.EnvoyStats))
	names := map[string]bool{}
	values := map[string]map[string]bool{}
	sample := regexp.MustCompile(`^([a-z_]+)(\{(.*)\})? `)
	label := regexp.MustCompile(`([a-z_]+)="([^"]*)"`)
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sample.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("not a sample: %q", line)
			continue
		}
		names[m[1]] = true
		for _, l := range label.FindAllStringSubmatch(m[3], -1) {
			if values[l[1]] == nil {
				values[l[1]] = map[string]bool{}
			}
			values[l[1]][l[2]] = true
			v := strings.ReplaceAll(l[2], "_", ":")
			if _, err := netip.ParseAddrPort(v); err == nil || isIP(l[2]) {
				t.Errorf("label %s=%q is an address", l[1], l[2])
			}
			if host, _, ok := strings.Cut(l[2], "_"); ok && isIP(host) {
				t.Errorf("label %s=%q holds an address", l[1], l[2])
			}
		}
	}
	family := regexp.MustCompile(`^envoy_(cluster|tcp|http|server|control_plane|cluster_manager|listener_manager)_[a-z_]+$`)
	for n := range names {
		if !family.MatchString(n) {
			t.Errorf("metric %s is not one of EnvoyStats' families", n)
		}
		for _, part := range []string{"admin", "empty", "app", "a_b", "x_y", "peer"} {
			if strings.Contains(n, part) {
				t.Errorf("metric %s holds the name of an object", n)
			}
		}
	}
	for _, want := range []string{"export/app/a_b", "import/app/x_y-b", "import/app/admin-b", "peer/b", "controller"} {
		if !values["envoy_cluster_name"][want] {
			t.Errorf("no stats of cluster %s; cluster names: %v", want, values["envoy_cluster_name"])
		}
	}
	if !values["envoy_tcp_prefix"]["import/app/x_y-b"] {
		t.Errorf("no stats of the TCP proxy import/app/x_y-b; prefixes: %v", values["envoy_tcp_prefix"])
	}
	for _, want := range []string{"envoy_cluster_upstream_cx_active", "envoy_cluster_upstream_cx_rx_bytes_total", "envoy_cluster_membership_healthy",
		"envoy_tcp_downstream_cx_total", "envoy_server_live"} {
		if !names[want] {
			t.Errorf("no metric %s", want)
		}
	}
	t.Logf("%d metrics, %d samples", len(names), strings.Count(body, "\n")-2*len(names))

	_, js := p.get("http://127.0.0.1:9901/clusters?format=json")
	clusters, err := gateway.ParseClusters([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	var open int64
	for _, h := range clusters["import/app/x.y-b"] {
		if h.Address == "127.0.0.1" {
			open = h.Connections
		}
	}
	if open != 1 {
		t.Errorf("import/app/x.y-b has %d open connections at 127.0.0.1, want 1: %v", open, clusters["import/app/x.y-b"])
	}
}

// sessionState returns the state of the probe's connection from local to the import listener at addr, as netstat shows
// it from the client's end, or "" when there is none. With local empty, it returns the state of the first connection
// to addr, and its local address.
func sessionState(p probe, local, addr string) (state, from string) {
	out, _ := docker("exec", p.name, "netstat", "-tn")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 6 && f[4] == addr && (local == "" || f[3] == local) {
			return f[5], f[3]
		}
	}
	return "", ""
}

// closedImport holds a session through import app/admin-b to a backend that never closes it, then closes the import.
// It returns how long the session lasted after the import was closed, up to hold.
func closedImport(t *testing.T, hold time.Duration, args ...string) time.Duration {
	ctx := context.Background()
	s, c, p, _, ip := adsEnvoy(t, args...)
	// The backend holds its one connection open: its input never ends.
	if out, err := docker("exec", "-d", p.name, "sh", "-c", "sleep 600 | nc -l -p 9902 > /dev/null"); err != nil {
		t.Fatalf("backend: %v\n%s", err, out)
	}
	var es discoveryv1.EndpointSlice
	if err := c.Get(ctx, client.ObjectKey{Namespace: "app", Name: "admin-1"}, &es); err != nil {
		t.Fatal(err)
	}
	es.Ports[0].Port = ptr.To[int32](9902)
	if err := c.Update(ctx, &es); err != nil {
		t.Fatal(err)
	}
	s.Trigger()
	waitHealthy(p, "import/app/admin-b", map[string]string{"127.0.0.1:30901": "healthy"})

	// The client's input is a FIFO held open, so it never closes its end either.
	session := net.JoinHostPort(ip, "15000")
	if out, err := docker("exec", "-d", p.name, "sh", "-c", "mkfifo /tmp/in; nc "+ip+" 15000 < /tmp/in & exec 3>/tmp/in; sleep 600"); err != nil {
		t.Fatalf("client: %v\n%s", err, out)
	}
	// The session is the one connection to the listener now. Later checks follow it by its local address, since the
	// check of a refused connection below can leave sockets of its own to the same listener.
	var local string
	p.eventually("the session is established", func() string {
		st, from := sessionState(p, "", session)
		if st != "ESTABLISHED" {
			return "state " + st
		}
		local = from
		return ""
	})

	var imp straitv1.Import
	if err := c.Get(ctx, client.ObjectKey{Namespace: "app", Name: "admin-b"}, &imp); err != nil {
		t.Fatal(err)
	}
	imp.Spec.Closed = true
	if err := c.Update(ctx, &imp); err != nil {
		t.Fatal(err)
	}
	s.Trigger()
	closed := time.Now()
	// A new connection is refused once Envoy has the configuration.
	p.eventually("a new connection is refused", func() string {
		out, err := docker("exec", p.name, "sh", "-c", "echo | nc -w 1 "+ip+" 15000; echo rc=$?")
		if err == nil && strings.Contains(out, "rc=1") {
			return ""
		}
		return out
	})
	for time.Since(closed) < hold {
		if st, _ := sessionState(p, local, session); st != "ESTABLISHED" {
			lasted := time.Since(closed)
			t.Logf("the session left ESTABLISHED (%s) %s after the import was closed", st, lasted.Round(100*time.Millisecond))
			// Opened again, the import has its listener back.
			imp.Spec.Closed = false
			if err := c.Update(ctx, &imp); err != nil {
				t.Fatal(err)
			}
			s.Trigger()
			p.eventually("the import's listener is back", func() string {
				if _, body := p.get("http://127.0.0.1:9901/listeners"); !strings.Contains(body, ImportName("app", "admin-b")+"::") {
					return body
				}
				return ""
			})
			return lasted
		}
		time.Sleep(250 * time.Millisecond)
	}
	return hold
}

// A closed import has no listener: a new connection is refused at once, and Envoy ends an established one when its
// drain time has passed, so a short --drain-time-s ends it within seconds (RFC0005). With Envoy's default drain time,
// 10 minutes, the session outlives the test's 20s.
func TestEnvoyClosedImport(t *testing.T) {
	t.Run("drain time 2s", func(t *testing.T) {
		if lasted := closedImport(t, 20*time.Second, "--drain-time-s", "2"); lasted >= 15*time.Second {
			t.Errorf("the session lasted %s after the import was closed, want under 15s", lasted)
		}
	})
	t.Run("default drain time", func(t *testing.T) {
		if lasted := closedImport(t, 20*time.Second); lasted < 20*time.Second {
			t.Errorf("the session ended %s after the import was closed, want it kept for Envoy's default drain time", lasted)
		}
	})
}
