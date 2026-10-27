package xds

import (
	"time"
	"context"
	"net"
	"slices"
	"strconv"
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	healthcheck "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/takodotid/strait/api/v1alpha1"
)

const podIP = "10.244.1.7"

func scheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func objectMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

// fixture is site a with 2 exports, 2 imports and peer b over a direct link (cost 10) and a wireguard link (cost 100).
// It also holds objects that Collect leaves out: an export and an import with no port, and a link with a host name.
func fixture(t *testing.T) client.Client {
	site := &v1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       v1alpha1.SiteSpec{Gateway: v1alpha1.GatewaySpec{ControlPort: 30900, PeerPort: 14999}},
	}
	export := func(name, svc string, port, gatewayPort int32) *v1alpha1.Export {
		return &v1alpha1.Export{
			ObjectMeta: objectMeta("app", name),
			Spec:       v1alpha1.ExportSpec{Service: v1alpha1.ServicePortRef{Name: svc, Port: port}},
			Status:     v1alpha1.ExportStatus{GatewayPort: gatewayPort},
		}
	}
	service := func(name, portName string, port int32) *corev1.Service {
		return &corev1.Service{ObjectMeta: objectMeta("app", name), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: portName, Port: port}}}}
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "app", Name: "pg-a-rw-1", Labels: map[string]string{discoveryv1.LabelServiceName: "pg-a-rw"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("postgres"), Port: ptr.To[int32](5432)}},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.110.1.5"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}},
			{Addresses: []string{"10.110.1.6"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}},
		},
	}
	imp := func(name, export string, gatewayPort, remotePort int32) *v1alpha1.Import {
		return &v1alpha1.Import{
			ObjectMeta: objectMeta("app", name),
			Spec:       v1alpha1.ImportSpec{Site: "b", Export: export},
			Status:     v1alpha1.ImportStatus{GatewayPort: gatewayPort, RemotePort: remotePort},
		}
	}
	peer := &v1alpha1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.SitePeerSpec{ControlPort: 30910}}
	direct := func(name string, cost int32, addr string) *v1alpha1.SiteLink {
		return &v1alpha1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.SiteLinkSpec{
			Peer: "b", Transport: v1alpha1.TransportDirect, Cost: cost, Encryption: v1alpha1.EncryptionNone,
			Direct: &v1alpha1.DirectLink{Address: addr},
		}}
	}
	wg := &v1alpha1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: "a-b-wg"}, Spec: v1alpha1.SiteLinkSpec{
		Peer: "b", Transport: v1alpha1.TransportWireGuard, Cost: 100,
		WireGuard: &v1alpha1.WireGuardLink{Address: "10.200.0.1/30", PeerAddress: "10.200.0.2"},
	}}
	return fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		site,
		export("pg", "pg-a-rw", 5432, 30901),
		export("redis", "redis", 6379, 30902),
		export("pending", "pending", 80, 0),
		service("pg-a-rw", "postgres", 5432),
		service("redis", "redis", 6379),
		slice,
		imp("pg-b", "pg-b", 15000, 30905),
		imp("redis-b", "redis-b", 15001, 0),
		imp("pending-b", "pending-b", 0, 30906),
		peer,
		wg,
		direct("a-b-lan", 10, "192.0.2.10"),
		direct("a-b-name", 5, "gateway-b.example"),
	).Build()
}

func collect(t *testing.T) Model {
	m, err := Collect(context.Background(), fixture(t), "controller.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func build(t *testing.T, m Model) *cache.Snapshot {
	snap, err := Build(m, podIP)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func names[T proto.Message](t *testing.T, snap *cache.Snapshot, typ resource.Type) map[string]T {
	out := map[string]T{}
	for name, r := range snap.GetResources(typ) {
		v, ok := r.(T)
		if !ok {
			t.Fatalf("%s is %T", name, r)
		}
		out[name] = v
	}
	return out
}

func keys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestCollect(t *testing.T) {
	m := collect(t)
	want := Model{
		ControlPort: 30900, PeerPort: 14999, Controller: "controller.example:8080",
		Exports: []Export{
			{Namespace: "app", Name: "pg", Port: 30901, Addresses: []string{"10.110.1.5"}, TargetPort: 5432},
			{Namespace: "app", Name: "redis", Port: 30902},
		},
		Imports: []Import{
			{Namespace: "app", Name: "pg-b", Port: 15000, Peer: "b", Export: "pg-b", RemotePort: 30905},
			{Namespace: "app", Name: "redis-b", Port: 15001, Peer: "b", Export: "redis-b"},
		},
		Peers: []Peer{{Name: "b", ControlPort: 30910, Links: []Link{
			{Name: "a-b-lan", Address: "192.0.2.10", Priority: 0},
			{Name: "a-b-wg", Address: "10.200.0.2", Priority: 1},
		}}},
	}
	if !equalModel(m, want) {
		t.Fatalf("Collect =\n%+v\nwant\n%+v", m, want)
	}
}

func equalModel(a, b Model) bool {
	return a.ControlPort == b.ControlPort && a.PeerPort == b.PeerPort && a.Controller == b.Controller &&
		slices.EqualFunc(a.Exports, b.Exports, func(x, y Export) bool {
			return x.Namespace == y.Namespace && x.Name == y.Name && x.Port == y.Port && x.TargetPort == y.TargetPort && slices.Equal(x.Addresses, y.Addresses)
		}) &&
		slices.Equal(a.Imports, b.Imports) &&
		slices.EqualFunc(a.Peers, b.Peers, func(x, y Peer) bool {
			return x.Name == y.Name && x.ControlPort == y.ControlPort && slices.Equal(x.Links, y.Links)
		})
}

func TestBuildListeners(t *testing.T) {
	snap := build(t, collect(t))
	ls := names[*listener.Listener](t, snap, resource.ListenerType)
	want := map[string]string{
		"export/app/pg":      "0.0.0.0:30901",
		"export/app/redis":   "0.0.0.0:30902",
		"import/app/pg-b":    podIP + ":15000",
		"import/app/redis-b": podIP + ":15001",
		"control":            "0.0.0.0:30900",
		"peers":              podIP + ":14999",
	}
	if got := keys(ls); !slices.Equal(got, keys(want)) {
		t.Fatalf("listeners = %v, want %v", got, keys(want))
	}
	for name, addr := range want {
		if got := hostPort(ls[name].GetAddress().GetSocketAddress()); got != addr {
			t.Errorf("%s binds %s, want %s", name, got, addr)
		}
	}
	for _, name := range []string{"export/app/pg", "import/app/pg-b"} {
		var tp tcpproxy.TcpProxy
		f := ls[name].GetFilterChains()[0].GetFilters()[0]
		if f.GetName() != wellknown.TCPProxy {
			t.Fatalf("%s filter = %s", name, f.GetName())
		}
		if err := f.GetTypedConfig().UnmarshalTo(&tp); err != nil {
			t.Fatal(err)
		}
		if tp.GetCluster() != name || tp.GetStatPrefix() != name || len(tp.GetAccessLog()) != 0 {
			t.Errorf("%s tcp proxy = %v", name, &tp)
		}
	}
}

func hostPort(sa interface {
	GetAddress() string
	GetPortValue() uint32
}) string {
	return net.JoinHostPort(sa.GetAddress(), strconv.Itoa(int(sa.GetPortValue())))
}

func connManager(t *testing.T, l *listener.Listener) *hcm.HttpConnectionManager {
	var m hcm.HttpConnectionManager
	if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestBuildControl(t *testing.T) {
	snap := build(t, collect(t))
	m := connManager(t, names[*listener.Listener](t, snap, resource.ListenerType)["control"])
	filters := m.GetHttpFilters()
	if len(filters) != 3 || filters[2].GetName() != wellknown.Router {
		t.Fatalf("filters = %v", filters)
	}
	for i, name := range []string{"pg", "redis"} {
		var hc healthcheck.HealthCheck
		if filters[i].GetName() != wellknown.HealthCheck {
			t.Fatalf("filter %d = %s", i, filters[i].GetName())
		}
		if err := filters[i].GetTypedConfig().UnmarshalTo(&hc); err != nil {
			t.Fatal(err)
		}
		if hc.GetPassThroughMode() == nil || hc.GetPassThroughMode().GetValue() {
			t.Errorf("%s pass_through_mode = %v", name, hc.GetPassThroughMode())
		}
		path := hc.GetHeaders()[0]
		if path.GetName() != ":path" || path.GetStringMatch().GetExact() != "/healthz/exports/app/"+name {
			t.Errorf("%s path match = %v", name, path)
		}
		if method := hc.GetHeaders()[1]; method.GetName() != ":method" || method.GetStringMatch().GetExact() != "GET" {
			t.Errorf("%s method match = %v", name, method)
		}
		pct := hc.GetClusterMinHealthyPercentages()
		if len(pct) != 1 || pct["export/app/"+name].GetValue() != 1 {
			t.Errorf("%s cluster_min_healthy_percentages = %v", name, pct)
		}
	}

	routes := m.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	if len(routes) != 3 {
		t.Fatalf("routes = %v", routes)
	}
	healthz, observations, rest := routes[0], routes[1], routes[2]
	if healthz.GetMatch().GetPath() != "/healthz" || healthz.GetDirectResponse().GetStatus() != 200 ||
		healthz.GetDirectResponse().GetBody().GetInlineString() != "ok" {
		t.Errorf("healthz route = %v", healthz)
	}
	if observations.GetMatch().GetPath() != "/v1alpha1/observations" ||
		observations.GetMatch().GetHeaders()[0].GetName() != ":method" ||
		observations.GetMatch().GetHeaders()[0].GetStringMatch().GetExact() != "GET" ||
		observations.GetRoute().GetCluster() != "controller" {
		t.Errorf("observations route = %v", observations)
	}
	if rest.GetMatch().GetPrefix() != "/" || rest.GetDirectResponse().GetStatus() != 404 {
		t.Errorf("last route = %v", rest)
	}
}

func TestBuildPeers(t *testing.T) {
	snap := build(t, collect(t))
	m := connManager(t, names[*listener.Listener](t, snap, resource.ListenerType)["peers"])
	routes := m.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	if len(routes) != 1 {
		t.Fatalf("routes = %v", routes)
	}
	r := routes[0]
	if r.GetMatch().GetPrefix() != "/peers/b/" || r.GetRoute().GetCluster() != "peer/b" || r.GetRoute().GetPrefixRewrite() != "/" {
		t.Errorf("peer route = %v", r)
	}
	if len(m.GetHttpFilters()) != 1 || m.GetHttpFilters()[0].GetName() != wellknown.Router {
		t.Errorf("filters = %v", m.GetHttpFilters())
	}
}

type ep struct {
	priority  uint32
	addr      string
	checkPort uint32
}

func endpoints(cla *endpoint.ClusterLoadAssignment) []ep {
	var out []ep
	for _, l := range cla.GetEndpoints() {
		for _, e := range l.GetLbEndpoints() {
			out = append(out, ep{l.GetPriority(), hostPort(e.GetEndpoint().GetAddress().GetSocketAddress()), e.GetEndpoint().GetHealthCheckConfig().GetPortValue()})
		}
	}
	return out
}

func TestBuildClusters(t *testing.T) {
	snap := build(t, collect(t))
	cs := names[*cluster.Cluster](t, snap, resource.ClusterType)
	want := []string{"controller", "export/app/pg", "export/app/redis", "import/app/pg-b", "import/app/redis-b", "peer/b"}
	if got := keys(cs); !slices.Equal(got, want) {
		t.Fatalf("clusters = %v, want %v", got, want)
	}
	checks := map[string]string{"import/app/pg-b": "/healthz/exports/app/pg-b", "import/app/redis-b": "/healthz/exports/app/redis-b", "peer/b": "/healthz"}
	for name, c := range cs {
		if p := c.GetCommonLbConfig().GetHealthyPanicThreshold(); p == nil || p.GetValue() != 0 {
			t.Errorf("%s healthy_panic_threshold = %v", name, p)
		}
		if c.GetConnectTimeout().AsDuration().Seconds() != 2 {
			t.Errorf("%s connect_timeout = %v", name, c.GetConnectTimeout())
		}
		wantType := cluster.Cluster_EDS
		if name == "controller" {
			wantType = cluster.Cluster_STRICT_DNS
		}
		if c.GetType() != wantType {
			t.Errorf("%s type = %v", name, c.GetType())
		}
		if wantType == cluster.Cluster_EDS && c.GetEdsClusterConfig().GetEdsConfig().GetAds() == nil {
			t.Errorf("%s eds config = %v", name, c.GetEdsClusterConfig())
		}
		path, ok := checks[name]
		if !ok {
			if len(c.GetHealthChecks()) != 0 {
				t.Errorf("%s has health checks", name)
			}
			continue
		}
		hc := c.GetHealthChecks()[0]
		if hc.GetHttpHealthCheck().GetPath() != path {
			t.Errorf("%s health check path = %s, want %s", name, hc.GetHttpHealthCheck().GetPath(), path)
		}
		if hc.GetInterval().AsDuration().Seconds() != 1 || hc.GetTimeout().AsDuration() != 500*time.Millisecond ||
			hc.GetNoTrafficInterval().AsDuration().Seconds() != 1 ||
			hc.GetUnhealthyThreshold().GetValue() != 2 || hc.GetHealthyThreshold().GetValue() != 2 {
			t.Errorf("%s health check = %v", name, hc)
		}
	}
	if sa := cs["controller"].GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress(); sa.GetAddress() != "controller.example" || sa.GetPortValue() != 8080 {
		t.Errorf("controller address = %v", sa)
	}

	clas := names[*endpoint.ClusterLoadAssignment](t, snap, resource.EndpointType)
	if got := keys(clas); !slices.Equal(got, want[1:]) {
		t.Fatalf("assignments = %v, want %v", got, want[1:])
	}
	for name, cla := range clas {
		if f := cla.GetPolicy().GetOverprovisioningFactor(); f.GetValue() != 100000 {
			t.Errorf("%s overprovisioning_factor = %v", name, f)
		}
	}
	wantEndpoints := map[string][]ep{
		"export/app/pg":      {{0, "10.110.1.5:5432", 0}},
		"export/app/redis":   nil,
		"import/app/pg-b":    {{0, "192.0.2.10:30905", 30910}, {1, "10.200.0.2:30905", 30910}},
		"import/app/redis-b": nil,
		"peer/b":             {{0, "192.0.2.10:30910", 0}, {1, "10.200.0.2:30910", 0}},
	}
	for name, w := range wantEndpoints {
		if got := endpoints(clas[name]); !slices.Equal(got, w) {
			t.Errorf("%s endpoints = %v, want %v", name, got, w)
		}
	}
}

func TestBuildDeterministic(t *testing.T) {
	m := collect(t)
	a, b := build(t, m), build(t, m)
	if a.GetVersion(resource.ListenerType) != b.GetVersion(resource.ListenerType) {
		t.Fatalf("versions differ: %s, %s", a.GetVersion(resource.ListenerType), b.GetVersion(resource.ListenerType))
	}
}

// RFC#0005 requirement 12: an endpoint change touches assignments only, so Envoy neither restarts nor drains a listener.
func TestBuildEndpointChange(t *testing.T) {
	m := collect(t)
	a := build(t, m)
	m.Exports[0].Addresses = []string{"10.110.1.6"}
	m.Peers[0].Links = m.Peers[0].Links[1:]
	m.Peers[0].Links[0].Priority = 0
	b := build(t, m)
	if a.GetVersion(resource.EndpointType) == b.GetVersion(resource.EndpointType) {
		t.Fatal("version did not change")
	}
	for _, typ := range []resource.Type{resource.ListenerType, resource.ClusterType} {
		ra, rb := a.GetResources(typ), b.GetResources(typ)
		if len(ra) != len(rb) {
			t.Fatalf("%s: %d resources, then %d", typ, len(ra), len(rb))
		}
		for name, r := range ra {
			if !proto.Equal(r, rb[name]) {
				t.Errorf("%s %s changed", typ, name)
			}
		}
	}
}

func TestBuildPortConflict(t *testing.T) {
	m := collect(t)
	m.Exports[1].Port = m.Exports[0].Port
	m.Imports[1].Port = m.ControlPort
	ls := names[*listener.Listener](t, build(t, m), resource.ListenerType)
	if _, ok := ls["export/app/redis"]; ok {
		t.Error("export/app/redis shares a port and was built")
	}
	if _, ok := ls["import/app/redis-b"]; ok {
		t.Error("import/app/redis-b shares the control port and was built")
	}
	if _, ok := ls["export/app/pg"]; !ok {
		t.Error("export/app/pg is missing")
	}
}

func TestBuildErrors(t *testing.T) {
	m := collect(t)
	if _, err := Build(m, "gateway-0"); err == nil {
		t.Error("a pod address that is not an IP address was accepted")
	}
	m.Controller = "controller.example"
	if _, err := Build(m, podIP); err == nil {
		t.Error("a controller address without a port was accepted")
	}
	m = collect(t)
	m.PeerPort = m.ControlPort
	if _, err := Build(m, podIP); err == nil {
		t.Error("a peer port equal to the control port was accepted")
	}
}
