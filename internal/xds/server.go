package xds

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	xdslog "github.com/envoyproxy/go-control-plane/pkg/log"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Timing of rebuilds.
const (
	resync   = 2 * time.Second
	debounce = 200 * time.Millisecond
)

var (
	_ manager.Runnable               = (*Server)(nil)
	_ manager.LeaderElectionRunnable = (*Server)(nil)
)

// Server serves each gateway Envoy its configuration over ADS.
// A node ID is the name of a gateway pod. Its configuration binds to that pod's address.
type Server struct {
	reader     client.Reader
	namespace  string
	address    string
	controller string
	log        logr.Logger
	cache      cache.SnapshotCache
	trigger    chan struct{}

	ctx context.Context
	// build serializes rebuilds, so an older model never replaces a newer one.
	build sync.Mutex
	mu    sync.Mutex
	// due is when the rebuild loop must next begin a turn. Zero before Start.
	due time.Time
	// streams maps an open stream to its node ID.
	streams map[int64]string
	// sent holds, by stream and type URL, the nonce and version of the last response.
	sent map[int64]map[string]response
	// rejected holds, by node ID and type URL, the error of each configuration that the node rejected and has not
	// accepted a configuration of that type since.
	rejected map[string]map[string]string

	// Rejected is called, on the stream's goroutine, when the set of rejections changes. It must not block.
	Rejected func()
}

// RejectedTotal counts the configurations that a gateway Envoy rejected, by resource type. The controller registers it.
var RejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "strait_xds_rejected_total",
	Help: "Configurations that a gateway Envoy rejected, by resource type.",
}, []string{"type"})

// NewServer returns a Server that listens on address, reads pods in namespace,
// and routes the observation path to controller, a host:port.
func NewServer(r client.Reader, namespace, address, controller string) *Server {
	l := log.Log.WithName("xds")
	return &Server{
		reader:     r,
		namespace:  namespace,
		address:    address,
		controller: controller,
		log:        l,
		cache:      cache.NewSnapshotCache(true, cache.IDHash{}, logger(l)),
		trigger:    make(chan struct{}, 1),
		streams:    map[int64]string{},
		sent:       map[int64]map[string]response{},
		rejected:   map[string]map[string]string{},
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. Only the leader serves configuration.
func (s *Server) NeedLeaderElection() bool { return true }

// Trigger asks for a rebuild of every node's configuration. Calls within a short window make one rebuild.
func (s *Server) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Start implements manager.Runnable. It serves until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.address)
	if err != nil {
		return err
	}
	s.ctx = ctx
	g := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 5 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
	)
	discovery.RegisterAggregatedDiscoveryServiceServer(g, server.NewServer(ctx, s.cache, s.callbacks()))
	go s.loop(ctx)

	errc := make(chan error, 1)
	go func() { errc <- g.Serve(lis) }()
	s.log.Info("serving ADS", "address", lis.Addr().String())
	select {
	case <-ctx.Done():
		g.Stop()
		return nil
	case err := <-errc:
		return err
	}
}

// LoopGrace is how much longer than its own timing the rebuild loop may take before it counts as stuck. Tests shorten
// it.
var LoopGrace = 30 * time.Second

// Alive returns an error once the rebuild loop is overdue: more than twice the resync period, plus 30s, after its
// last turn began. A rebuild reads only the controller's cache, and hands each node's configuration to its stream
// without waiting for the node, so neither a slow API server nor a gateway that does not answer delays it. Before
// Start, it returns nil. now must come from time.Now.
func (s *Server) Alive(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.due.IsZero() || !now.After(s.due) {
		return nil
	}
	return fmt.Errorf("the xDS rebuild loop is %s overdue", now.Sub(s.due).Round(time.Millisecond))
}

func (s *Server) turn(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.due = now.Add(2*resync + LoopGrace)
}

func (s *Server) loop(ctx context.Context) {
	tick := time.NewTicker(resync)
	defer tick.Stop()
	s.turn(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.trigger:
			select {
			case <-ctx.Done():
				return
			case <-time.After(debounce):
			}
			select {
			case <-s.trigger:
			default:
			}
		}
		s.turn(time.Now())
		s.rebuild(ctx, s.nodes())
	}
}

func (s *Server) callbacks() server.Callbacks {
	return server.CallbackFuncs{
		StreamRequestFunc: func(id int64, req *discovery.DiscoveryRequest) error {
			s.opened(id, req.GetNode().GetId())
			s.answered(id, req)
			return nil
		},
		StreamResponseFunc: func(_ context.Context, id int64, _ *discovery.DiscoveryRequest, resp *discovery.DiscoveryResponse) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.sent[id] == nil {
				s.sent[id] = map[string]response{}
			}
			s.sent[id][resp.GetTypeUrl()] = response{nonce: resp.GetNonce(), version: resp.GetVersionInfo()}
		},
		StreamClosedFunc: func(id int64, _ *core.Node) { s.closed(id) },
	}
}

// opened sets a node's configuration on the first request of a stream, before the server answers it.
func (s *Server) opened(id int64, node string) {
	if node == "" {
		return
	}
	s.mu.Lock()
	_, known := s.streams[id]
	s.streams[id] = node
	s.mu.Unlock()
	if !known {
		s.rebuild(s.ctx, []string{node})
	}
}

// response is a response sent on a stream.
type response struct{ nonce, version string }

// answered records whether a node accepted or rejected the last response of a type that it was sent. A request with
// that response's nonce rejects it when it carries an error detail, and accepts it otherwise. Any other request
// changes nothing: a node's first request of a type carries no nonce, and go-control-plane ignores a request with an
// older nonce.
//
// A request that rejects a response names the version that the node accepted last. go-control-plane would answer it
// at once with the version in the cache, the one rejected, and the node would reject it again, in a loop with no
// pause. So answered sets the request's version to the rejected one: the cache then answers when the node's
// configuration changes.
func (s *Server) answered(id int64, req *discovery.DiscoveryRequest) {
	typ := req.GetTypeUrl()
	s.mu.Lock()
	node, ok := s.streams[id]
	last, sent := s.sent[id][typ]
	if !ok || !sent || req.GetResponseNonce() == "" || req.GetResponseNonce() != last.nonce {
		s.mu.Unlock()
		return
	}
	accepted := req.GetVersionInfo()
	var changed bool
	if detail := req.GetErrorDetail(); detail != nil {
		req.VersionInfo = last.version
		if s.rejected[node] == nil {
			s.rejected[node] = map[string]string{}
		}
		changed = s.rejected[node][typ] != detail.GetMessage()
		s.rejected[node][typ] = detail.GetMessage()
		s.mu.Unlock()
		RejectedTotal.WithLabelValues(shortType(typ)).Inc()
		s.log.Error(nil, "a gateway Envoy rejected its configuration and keeps the one it accepted last", "node", node,
			"type", shortType(typ), "rejected", last.version, "accepted", accepted, "error", detail.GetMessage())
	} else {
		_, changed = s.rejected[node][typ]
		delete(s.rejected[node], typ)
		if len(s.rejected[node]) == 0 {
			delete(s.rejected, node)
		}
		s.mu.Unlock()
		if changed {
			s.log.Info("a gateway Envoy accepted its configuration again", "node", node, "type", shortType(typ), "version", accepted)
		}
	}
	if changed && s.Rejected != nil {
		s.Rejected()
	}
}

// Rejections returns one line for each node and type whose last configuration the node rejected, sorted. Each run of
// white space in Envoy's error is one space.
func (s *Server) Rejections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for node, types := range s.rejected {
		for typ, msg := range types {
			out = append(out, fmt.Sprintf("pod %s rejected %s: %s", node, shortType(typ), strings.Join(strings.Fields(msg), " ")))
		}
	}
	slices.Sort(out)
	return out
}

// shortType is the last part of a type URL, such as Listener.
func shortType(typeURL string) string {
	return typeURL[strings.LastIndex(typeURL, ".")+1:]
}

// closed forgets a node, its configuration and its rejections, when its last stream closes.
func (s *Server) closed(id int64) {
	s.mu.Lock()
	node, ok := s.streams[id]
	delete(s.streams, id)
	delete(s.sent, id)
	if !ok || s.connected(node) {
		s.mu.Unlock()
		return
	}
	s.cache.ClearSnapshot(node)
	_, changed := s.rejected[node]
	delete(s.rejected, node)
	s.mu.Unlock()
	if changed && s.Rejected != nil {
		s.Rejected()
	}
}

func (s *Server) connected(node string) bool {
	for _, n := range s.streams {
		if n == node {
			return true
		}
	}
	return false
}

func (s *Server) nodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, n := range s.streams {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// errNoAddress is the error of set for a pod that has no address yet. A new gateway pod's Envoy can connect before the
// kubelet writes the pod's address. The update that writes it triggers a rebuild (controller.XDSTrigger watches the
// gateway pods), and the rebuild every 2s retries too.
var errNoAddress = errors.New("has no address yet")

// rebuild reads the model once and sets the configuration of each node.
// On an error a node keeps its last configuration. A pod with no address yet is not an error.
func (s *Server) rebuild(ctx context.Context, nodes []string) {
	if len(nodes) == 0 {
		return
	}
	s.build.Lock()
	defer s.build.Unlock()
	m, err := Collect(ctx, s.reader, s.controller)
	if err != nil {
		s.log.Error(err, "cannot read the gateway model")
		return
	}
	for _, node := range nodes {
		switch err := s.set(ctx, m, node); {
		case errors.Is(err, errNoAddress):
			s.log.V(1).Info("the gateway configuration waits for the pod's address", "node", node)
		case err != nil:
			s.log.Error(err, "cannot set the gateway configuration", "node", node)
		}
	}
}

func (s *Server) set(ctx context.Context, m Model, node string) error {
	var pod corev1.Pod
	if err := s.reader.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: node}, &pod); err != nil {
		return err
	}
	if pod.Status.PodIP == "" {
		return fmt.Errorf("pod %s %w", node, errNoAddress)
	}
	snap, err := Build(m, pod.Status.PodIP)
	if err != nil {
		return err
	}
	version := snap.GetVersion(resource.ListenerType)
	if old, err := s.cache.GetSnapshot(node); err == nil && old.GetVersion(resource.ListenerType) == version {
		return nil
	}
	s.mu.Lock()
	connected := s.connected(node)
	s.mu.Unlock()
	if !connected {
		return nil
	}
	if err := s.cache.SetSnapshot(ctx, node, snap); err != nil {
		return err
	}
	s.log.Info("configuration set", "node", node, "version", version, "address", pod.Status.PodIP)
	return nil
}

// logger adapts the snapshot cache's logger to logr.
func logger(l logr.Logger) xdslog.Logger {
	return xdslog.LoggerFuncs{
		DebugFunc: func(f string, a ...any) { l.V(2).Info(fmt.Sprintf(f, a...)) },
		InfoFunc:  func(f string, a ...any) { l.V(1).Info(fmt.Sprintf(f, a...)) },
		WarnFunc:  func(f string, a ...any) { l.V(1).Info(fmt.Sprintf(f, a...)) },
		ErrorFunc: func(f string, a ...any) { l.Error(nil, fmt.Sprintf(f, a...)) },
	}
}
