package xds

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	healthcheck "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	router "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Names of the objects that are not per export, import or peer.
const (
	ControlListener   = "control"
	PeersListener     = "peers"
	ControllerCluster = "controller"
)

// Values of RFC#0005's Envoy configuration.
const (
	overprovisioningFactor = 100000
	// Envoy truncates the percentage to an integer, and 0 lets an empty cluster pass.
	// Export clusters have no active health check, so 1 means "has a ready endpoint".
	minHealthyPercentage = 1
	connectTimeout       = 2 * time.Second
	checkInterval        = time.Second
	checkTimeout         = 500 * time.Millisecond
	unhealthyThreshold   = 2
	healthyThreshold     = 2
	keepaliveTime        = 30
	keepaliveInterval    = 5
	keepaliveProbes      = 3
	// checkHost is the Host header of health checks. The default, the cluster name, is not a valid authority.
	checkHost = "strait-gateway"
)

// ExportName is the name of an export's listener and cluster.
func ExportName(namespace, name string) string { return "export/" + namespace + "/" + name }

// ImportName is the name of an import's listener and cluster.
func ImportName(namespace, name string) string { return "import/" + namespace + "/" + name }

// PeerName is the name of a peer's cluster.
func PeerName(peer string) string { return "peer/" + peer }

// ExportHealthPath is the control port path that answers an export's health.
func ExportHealthPath(namespace, name string) string {
	return "/healthz/exports/" + namespace + "/" + name
}

// Build returns the configuration of the gateway pod at podIP. The version is a hash of the content.
func Build(m Model, podIP string) (*cache.Snapshot, error) {
	if !isIP(podIP) {
		return nil, fmt.Errorf("pod address %q is not an IP address", podIP)
	}
	if m.ControlPort == m.PeerPort {
		return nil, fmt.Errorf("control port and peer port are both %d", m.ControlPort)
	}
	host, port, err := net.SplitHostPort(m.Controller)
	if err != nil {
		return nil, fmt.Errorf("controller address: %w", err)
	}
	controllerPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("controller port: %w", err)
	}

	b := &builder{}
	var listeners, clusters, assignments []types.Resource
	used := map[int32]bool{m.ControlPort: true, m.PeerPort: true}
	var checks []*hcm.HttpFilter
	for _, e := range m.Exports {
		if e.Port <= 0 || used[e.Port] {
			continue
		}
		used[e.Port] = true
		name := ExportName(e.Namespace, e.Name)
		listeners = append(listeners, b.tcpListener(name, "0.0.0.0", e.Port))
		clusters = append(clusters, edsCluster(name, nil))
		var eps []*endpoint.LocalityLbEndpoints
		if e.TargetPort > 0 && len(e.Addresses) > 0 {
			l := &endpoint.LocalityLbEndpoints{}
			for _, a := range e.Addresses {
				l.LbEndpoints = append(l.LbEndpoints, lbEndpoint(a, e.TargetPort, 0))
			}
			eps = append(eps, l)
		}
		assignments = append(assignments, assignment(name, eps))
		checks = append(checks, b.healthFilter(e.Namespace, e.Name))
	}

	peers := map[string]Peer{}
	for _, p := range m.Peers {
		peers[p.Name] = p
	}
	for _, i := range m.Imports {
		if i.Port <= 0 || used[i.Port] {
			continue
		}
		used[i.Port] = true
		name := ImportName(i.Namespace, i.Name)
		listeners = append(listeners, b.tcpListener(name, podIP, i.Port))
		clusters = append(clusters, edsCluster(name, httpCheck(ExportHealthPath(i.Namespace, i.Export))))
		var eps []*endpoint.LocalityLbEndpoints
		if p, ok := peers[i.Peer]; ok && i.RemotePort > 0 {
			for _, l := range p.Links {
				eps = append(eps, &endpoint.LocalityLbEndpoints{
					Priority:    l.Priority,
					LbEndpoints: []*endpoint.LbEndpoint{lbEndpoint(l.Address, i.RemotePort, p.ControlPort)},
				})
			}
		}
		assignments = append(assignments, assignment(name, eps))
	}

	var peerRoutes []*route.Route
	for _, p := range m.Peers {
		name := PeerName(p.Name)
		clusters = append(clusters, edsCluster(name, httpCheck("/healthz")))
		var eps []*endpoint.LocalityLbEndpoints
		for _, l := range p.Links {
			eps = append(eps, &endpoint.LocalityLbEndpoints{
				Priority:    l.Priority,
				LbEndpoints: []*endpoint.LbEndpoint{lbEndpoint(l.Address, p.ControlPort, 0)},
			})
		}
		assignments = append(assignments, assignment(name, eps))
		peerRoutes = append(peerRoutes, &route.Route{
			Match: &route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/peers/" + p.Name + "/"}},
			Action: &route.Route_Route{Route: &route.RouteAction{
				ClusterSpecifier: &route.RouteAction_Cluster{Cluster: name},
				PrefixRewrite:    "/",
			}},
		})
	}
	clusters = append(clusters, controllerCluster(host, uint32(controllerPort)))

	listeners = append(listeners,
		b.httpListener(ControlListener, "0.0.0.0", m.ControlPort, controlRoutes(), checks),
		b.httpListener(PeersListener, podIP, m.PeerPort, peerRoutes, nil))
	if b.err != nil {
		return nil, b.err
	}

	version, err := hash(listeners, clusters, assignments)
	if err != nil {
		return nil, err
	}
	snap, err := cache.NewSnapshot(version, map[resource.Type][]types.Resource{
		resource.ListenerType: listeners,
		resource.ClusterType:  clusters,
		resource.EndpointType: assignments,
	})
	if err != nil {
		return nil, err
	}
	if err := snap.Consistent(); err != nil {
		return nil, err
	}
	return snap, nil
}

// builder packs filter configurations and keeps the first error.
type builder struct{ err error }

// any packs m with a deterministic encoding, so equal content gives equal bytes.
func (b *builder) any(m proto.Message) *anypb.Any {
	a := &anypb.Any{}
	if err := anypb.MarshalFrom(a, m, proto.MarshalOptions{Deterministic: true}); err != nil && b.err == nil {
		b.err = err
	}
	return a
}

func controlRoutes() []*route.Route {
	return []*route.Route{
		{
			Match:  &route.RouteMatch{PathSpecifier: &route.RouteMatch_Path{Path: "/healthz"}, Headers: []*route.HeaderMatcher{methodGet()}},
			Action: directResponse(200, "ok"),
		},
		{
			Match: &route.RouteMatch{PathSpecifier: &route.RouteMatch_Path{Path: "/v1alpha1/observations"}, Headers: []*route.HeaderMatcher{methodGet()}},
			Action: &route.Route_Route{Route: &route.RouteAction{
				ClusterSpecifier: &route.RouteAction_Cluster{Cluster: ControllerCluster},
			}},
		},
		{
			Match:  &route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"}},
			Action: directResponse(404, ""),
		},
	}
}

func directResponse(status uint32, body string) *route.Route_DirectResponse {
	d := &route.DirectResponseAction{Status: status}
	if body != "" {
		d.Body = &core.DataSource{Specifier: &core.DataSource_InlineString{InlineString: body}}
	}
	return &route.Route_DirectResponse{DirectResponse: d}
}

func headerExact(name, value string) *route.HeaderMatcher {
	return &route.HeaderMatcher{
		Name: name,
		HeaderMatchSpecifier: &route.HeaderMatcher_StringMatch{StringMatch: &matcher.StringMatcher{
			MatchPattern: &matcher.StringMatcher_Exact{Exact: value},
		}},
	}
}

func methodGet() *route.HeaderMatcher { return headerExact(":method", "GET") }

// healthFilter answers an export's health path from the health of its cluster: 200 while any endpoint is healthy.
func (b *builder) healthFilter(namespace, name string) *hcm.HttpFilter {
	return &hcm.HttpFilter{
		Name: wellknown.HealthCheck,
		ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: b.any(&healthcheck.HealthCheck{
			PassThroughMode: wrapperspb.Bool(false),
			Headers:         []*route.HeaderMatcher{headerExact(":path", ExportHealthPath(namespace, name)), methodGet()},
			ClusterMinHealthyPercentages: map[string]*typev3.Percent{
				ExportName(namespace, name): {Value: minHealthyPercentage},
			},
		})},
	}
}

func (b *builder) httpListener(name, addr string, port int32, routes []*route.Route, filters []*hcm.HttpFilter) *listener.Listener {
	filters = append(filters, &hcm.HttpFilter{
		Name:       wellknown.Router,
		ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: b.any(&router.Router{})},
	})
	m := &hcm.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  hcm.HttpConnectionManager_AUTO,
		RouteSpecifier: &hcm.HttpConnectionManager_RouteConfig{RouteConfig: &route.RouteConfiguration{
			Name: name,
			// The clusters come over CDS, so a static check against them would race.
			ValidateClusters: wrapperspb.Bool(false),
			VirtualHosts:     []*route.VirtualHost{{Name: name, Domains: []string{"*"}, Routes: routes}},
		}},
		HttpFilters: filters,
	}
	return b.networkListener(name, addr, port, wellknown.HTTPConnectionManager, m)
}

func (b *builder) tcpListener(name, addr string, port int32) *listener.Listener {
	return b.networkListener(name, addr, port, wellknown.TCPProxy, &tcpproxy.TcpProxy{
		StatPrefix:       name,
		ClusterSpecifier: &tcpproxy.TcpProxy_Cluster{Cluster: name},
		// Pooled database connections stay idle for long; TCP keepalive finds dead ones instead.
		IdleTimeout: durationpb.New(0),
	})
}

func (b *builder) networkListener(name, addr string, port int32, filter string, config proto.Message) *listener.Listener {
	return &listener.Listener{
		Name:    name,
		Address: socketAddress(addr, uint32(port)),
		FilterChains: []*listener.FilterChain{{
			Filters: []*listener.Filter{{Name: filter, ConfigType: &listener.Filter_TypedConfig{TypedConfig: b.any(config)}}},
		}},
	}
}

func edsCluster(name string, check *core.HealthCheck) *cluster.Cluster {
	c := baseCluster(name)
	c.ClusterDiscoveryType = &cluster.Cluster_Type{Type: cluster.Cluster_EDS}
	c.EdsClusterConfig = &cluster.Cluster_EdsClusterConfig{EdsConfig: &core.ConfigSource{
		ConfigSourceSpecifier: &core.ConfigSource_Ads{Ads: &core.AggregatedConfigSource{}},
		ResourceApiVersion:    core.ApiVersion_V3,
	}}
	if check != nil {
		c.HealthChecks = []*core.HealthCheck{check}
	}
	return c
}

func controllerCluster(host string, port uint32) *cluster.Cluster {
	c := baseCluster(ControllerCluster)
	c.ClusterDiscoveryType = &cluster.Cluster_Type{Type: cluster.Cluster_STRICT_DNS}
	c.LoadAssignment = &endpoint.ClusterLoadAssignment{
		ClusterName: ControllerCluster,
		Endpoints:   []*endpoint.LocalityLbEndpoints{{LbEndpoints: []*endpoint.LbEndpoint{lbEndpoint(host, int32(port), 0)}}},
	}
	return c
}

func baseCluster(name string) *cluster.Cluster {
	return &cluster.Cluster{
		Name:           name,
		ConnectTimeout: durationpb.New(connectTimeout),
		CommonLbConfig: &cluster.Cluster_CommonLbConfig{HealthyPanicThreshold: &typev3.Percent{Value: 0}},
		UpstreamConnectionOptions: &cluster.UpstreamConnectionOptions{TcpKeepalive: &core.TcpKeepalive{
			KeepaliveTime:     wrapperspb.UInt32(keepaliveTime),
			KeepaliveInterval: wrapperspb.UInt32(keepaliveInterval),
			KeepaliveProbes:   wrapperspb.UInt32(keepaliveProbes),
		}},
	}
}

func httpCheck(path string) *core.HealthCheck {
	return &core.HealthCheck{
		Interval:           durationpb.New(checkInterval),
		Timeout:            durationpb.New(checkTimeout),
		NoTrafficInterval:  durationpb.New(checkInterval),
		UnhealthyThreshold: wrapperspb.UInt32(unhealthyThreshold),
		HealthyThreshold:   wrapperspb.UInt32(healthyThreshold),
		HealthChecker: &core.HealthCheck_HttpHealthCheck_{HttpHealthCheck: &core.HealthCheck_HttpHealthCheck{
			Host: checkHost,
			Path: path,
		}},
	}
}

func assignment(name string, eps []*endpoint.LocalityLbEndpoints) *endpoint.ClusterLoadAssignment {
	return &endpoint.ClusterLoadAssignment{
		ClusterName: name,
		Endpoints:   eps,
		Policy: &endpoint.ClusterLoadAssignment_Policy{
			OverprovisioningFactor: wrapperspb.UInt32(overprovisioningFactor),
		},
	}
}

func lbEndpoint(addr string, port, checkPort int32) *endpoint.LbEndpoint {
	e := &endpoint.Endpoint{Address: socketAddress(addr, uint32(port))}
	if checkPort > 0 {
		e.HealthCheckConfig = &endpoint.Endpoint_HealthCheckConfig{PortValue: uint32(checkPort)}
	}
	return &endpoint.LbEndpoint{HostIdentifier: &endpoint.LbEndpoint_Endpoint{Endpoint: e}}
}

func socketAddress(addr string, port uint32) *core.Address {
	return &core.Address{Address: &core.Address_SocketAddress{SocketAddress: &core.SocketAddress{
		Address:       addr,
		PortSpecifier: &core.SocketAddress_PortValue{PortValue: port},
	}}}
}

func hash(groups ...[]types.Resource) (string, error) {
	h := sha256.New()
	opts := proto.MarshalOptions{Deterministic: true}
	for _, g := range groups {
		for _, r := range g {
			b, err := opts.Marshal(r)
			if err != nil {
				return "", err
			}
			h.Write(strconv.AppendInt(nil, int64(len(b)), 10))
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
