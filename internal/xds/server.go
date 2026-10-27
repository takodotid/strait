package xds

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	xdslog "github.com/envoyproxy/go-control-plane/pkg/log"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
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
	// streams maps an open stream to its node ID.
	streams map[int64]string
}

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

func (s *Server) loop(ctx context.Context) {
	tick := time.NewTicker(resync)
	defer tick.Stop()
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
		s.rebuild(ctx, s.nodes())
	}
}

func (s *Server) callbacks() server.Callbacks {
	return server.CallbackFuncs{
		StreamRequestFunc: func(id int64, req *discovery.DiscoveryRequest) error {
			s.opened(id, req.GetNode().GetId())
			return nil
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

// closed forgets a node when its last stream closes.
func (s *Server) closed(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.streams[id]
	delete(s.streams, id)
	if !ok || s.connected(node) {
		return
	}
	s.cache.ClearSnapshot(node)
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

// rebuild reads the model once and sets the configuration of each node.
// On an error a node keeps its last configuration.
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
		if err := s.set(ctx, m, node); err != nil {
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
		return fmt.Errorf("pod %s has no address", node)
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
