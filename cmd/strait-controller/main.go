// Command strait-controller runs plugins, decides routes, serves the observation document and the gateway's xDS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/controller"
	"github.com/takodotid/strait/internal/exchange"
	"github.com/takodotid/strait/internal/metrics"
	"github.com/takodotid/strait/internal/plugin"
	"github.com/takodotid/strait/internal/version"
	"github.com/takodotid/strait/internal/xds"
)

func main() {
	var (
		metricsAddr, probeAddr, observationsAddr, xdsAddr string
		gatewayNamespace, observationsService, namespaces string
		leaderElect, cnpg, namespacedReads                bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address of the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address of the health probes.")
	flag.StringVar(&observationsAddr, "observations-bind-address", ":8082", "Address of the observation document.")
	flag.StringVar(&xdsAddr, "xds-bind-address", ":18000", "Address of the gateway's xDS server.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", os.Getenv("POD_NAMESPACE"), "Namespace of the gateway, its Lease and its Services.")
	flag.StringVar(&observationsService, "observations-service", "", "Host name of the controller's Service, which the gateway forwards observation requests to.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Elect one active controller.")
	flag.StringVar(&namespaces, "namespaces", "", "Comma-separated namespaces where the controller writes Services and EndpointSlices, besides --gateway-namespace. Empty writes in every namespace. The chart sets it from controller.namespaces, which grants the writes.")
	flag.BoolVar(&namespacedReads, "namespaced-reads", false, "Read Services, EndpointSlices and CloudNativePG's objects only in --namespaces and --gateway-namespace, so that the controller's cache holds only theirs. Needs --namespaces. The chart sets it from controller.namespacedReads, which grants those reads there only.")
	flag.BoolVar(&cnpg, "cnpg", true, "Watch and read CloudNativePG's kinds, when their CRDs exist. False reports every member of the cnpg plugin Unknown. The chart sets it from cnpg.enabled, which grants the read.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	if gatewayNamespace == "" {
		log.Error(errors.New("missing"), "--gateway-namespace or POD_NAMESPACE is required")
		os.Exit(2)
	}
	writes := writeNamespaces(namespaces, gatewayNamespace)
	var reads []string
	if namespacedReads {
		if len(writes) == 0 {
			log.Error(errors.New("missing"), "--namespaced-reads needs --namespaces")
			os.Exit(2)
		}
		reads = writes
	}
	if limit, ok := memoryLimit(os.Getenv("MEMORY_LIMIT"), os.Getenv("GOMEMLIMIT")); ok {
		debug.SetMemoryLimit(limit)
		log.Info("Go memory limit", "bytes", limit)
	}
	if err := run(metricsAddr, probeAddr, observationsAddr, xdsAddr, gatewayNamespace, observationsService, leaderElect, cnpg, writes, reads); err != nil {
		log.Error(err, "controller stopped")
		os.Exit(1)
	}
}

// run runs the controller. It writes Services and EndpointSlices in namespaces only, and reads them, and
// CloudNativePG's objects, in reads only. Either empty means every namespace.
func run(metricsAddr, probeAddr, observationsAddr, xdsAddr, gatewayNamespace, observationsService string, leaderElect, cnpg bool, namespaces, reads []string) error {
	log := ctrl.Log.WithName("setup")
	log.Info("starting", "version", version.Version)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		return err
	}
	cfg := ctrl.GetConfigOrDie()
	lock, err := leaderLock(cfg, gatewayNamespace)
	if err != nil {
		return err
	}
	opts, watchCNPG, err := options(cfg, scheme, lock, gatewayNamespace, metricsAddr, probeAddr, leaderElect, cnpg, reads)
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, opts)
	if err != nil {
		return err
	}
	log.Info("cloudnative-pg", "enabled", cnpg, "watched", watchCNPG)
	log.Info("writes Services and EndpointSlices", "namespaces", namespaces)
	log.Info("reads Services, EndpointSlices and CloudNativePG's objects", "namespaces", reads)
	// Everything reads through it, so a read outside the namespaces shows in status (RFC0007, requirement 26).
	reader := controller.NamespacedReads(mgr.GetClient(), reads)
	// Every reconciler writes through it, so a write outside the namespaces shows in status (RFC0007, requirement 16).
	writer := controller.NamespacedWrites(reader, namespaces)

	local := &exchange.Local{}
	xdsServer := xds.NewServer(reader, gatewayNamespace, xdsAddr, net.JoinHostPort(observationsService, portOf(observationsAddr)))

	ssChanges := make(chan event.GenericEvent, 1)
	impChanges := make(chan event.GenericEvent, 1)
	gwChanges := make(chan event.GenericEvent, 1)
	xdsServer.Rejected = func() { notify(gwChanges) }
	poller := newPoller(reader, func(string) {
		notify(ssChanges)
		notify(impChanges)
		xdsServer.Trigger()
	})

	siteServices := &controller.SiteServiceReconciler{
		Client:    writer,
		Reader:    reader,
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder("strait-controller"),
		Peers:     poller,
		Local:     local,
		Now:       time.Now,
		Plugins:   plugin.Options{CNPGOff: !cnpg},
	}
	if err := siteServices.SetupWithManager(mgr, ssChanges, watchCNPG); err != nil {
		return fmt.Errorf("siteservice controller: %w", err)
	}
	if err := (&controller.ExportReconciler{Client: writer, Reader: reader, Local: local}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("export controller: %w", err)
	}
	if err := (&controller.ImportReconciler{Client: writer, Reader: reader, Peers: poller, Now: time.Now}).SetupWithManager(mgr, impChanges); err != nil {
		return fmt.Errorf("import controller: %w", err)
	}
	if err := (&controller.GatewayReconciler{
		Client: writer, Reader: reader, Namespace: gatewayNamespace, Version: version.Version,
		ConfigRejections: xdsServer.Rejections, Recorder: mgr.GetEventRecorder("strait-controller"),
	}).SetupWithManager(mgr, gwChanges); err != nil {
		return fmt.Errorf("gateway controller: %w", err)
	}
	if err := (&controller.XDSTrigger{Trigger: xdsServer.Trigger, Namespace: gatewayNamespace}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("xds trigger: %w", err)
	}

	for _, r := range runnables(observationsAddr, localDocument(reader, local, time.Now), poller, mgr.GetClient(), xdsServer) {
		if err := mgr.Add(r); err != nil {
			return err
		}
	}

	ctrlmetrics.Registry.MustRegister(&metrics.Collector{Reader: reader, GatewayNamespace: gatewayNamespace, Decisions: siteServices.Decisions},
		xds.RejectedTotal)

	if err := mgr.AddHealthzCheck("loops", loopsAlive(poller, xdsServer)); err != nil {
		return err
	}
	// Only the leader serves, so only the leader is ready.
	if err := mgr.AddReadyzCheck("leader", func(*http.Request) error {
		select {
		case <-mgr.Elected():
			return nil
		default:
			return errors.New("not the leader")
		}
	}); err != nil {
		return err
	}
	return start(ctrl.SetupSignalHandler(), mgr, lock, leaderElect)
}

// start runs the manager until ctx ends or the manager fails. The manager returns nil only once it stopped on ctx and
// every runnable has returned: it waits for them with no timeout. start then releases the Lease if this controller
// still holds it, so the next controller takes it at once instead of after the lease duration (RFC0004, requirement
// 14). On a failure, as when the Lease could not be renewed, the manager does not wait for its runnables, so the Lease
// stays held until it expires (requirement 16). client-go's own release on cancel would come before the runnables
// stop on that path. The release is safe only because the process exits right after: main returns once run does.
func start(ctx context.Context, mgr manager.Manager, lock resourcelock.Interface, leaderElect bool) error {
	if err := mgr.Start(ctx); err != nil {
		return err
	}
	if !leaderElect {
		return nil
	}
	if err := release(lock); err != nil {
		ctrl.Log.WithName("setup").Error(err, "release the leader election Lease")
	}
	return nil
}

// release gives up the Lease when this controller holds it, with the record that client-go's leader election writes:
// no holder, and a duration of 1s.
func release(lock resourcelock.Interface) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, _, err := lock.Get(ctx)
	if err != nil {
		return err
	}
	if rec.HolderIdentity != lock.Identity() {
		return nil
	}
	now := metav1.NewTime(time.Now())
	return lock.Update(ctx, resourcelock.LeaderElectionRecord{
		LeaderTransitions:    rec.LeaderTransitions,
		LeaseDurationSeconds: 1,
		RenewTime:            now,
		AcquireTime:          now,
	})
}

// runnables are the document server, the poller, the SitePeer status writer and the xDS server. Each runs on the
// elected controller only: one that is not elected holds no observations, and its empty document would read as a site
// with no members (RFC0004, requirement 12).
func runnables(observationsAddr string, document func() exchange.Document, poller *exchange.Poller, c client.Client, xdsServer *xds.Server) []manager.Runnable {
	return []manager.Runnable{
		leaderOnly(&httpServer{addr: observationsAddr, handler: exchange.NewHandler(document)}),
		leaderOnly(poller),
		leaderOnly(&exchange.StatusWriter{Client: c, Poller: poller}),
		xdsServer,
	}
}

// newPoller returns the poller of the peers' documents. Every read goes to r, the controller's cache, and every poll
// through the local gateway (RFC0004).
func newPoller(r client.Reader, changed func(peer string)) *exchange.Poller {
	return &exchange.Poller{
		Peers: func() []string {
			var peers straitv1.SitePeerList
			if err := r.List(context.Background(), &peers); err != nil {
				return nil
			}
			out := make([]string, 0, len(peers.Items))
			for _, p := range peers.Items {
				out = append(out, p.Name)
			}
			return out
		},
		URL: func(peer string) (string, error) { return peerURL(context.Background(), r, peer) },
		Exchange: func() straitv1.ExchangeSpec {
			site, err := controller.LocalSite(context.Background(), r)
			if err != nil {
				return straitv1.ExchangeSpec{}
			}
			return site.Spec.Exchange
		},
		Changed: changed,
	}
}

// loopsAlive is the liveness check: it fails while the poller's round or the xDS rebuild loop is overdue. Both run on
// the elected controller only, and pass before they start. Neither waits for the API server or a link, so a slow API
// server or a link that is down cannot fail it (RFC0004). Work that does wait for them, such as decisions and status
// writes, shows in metrics instead (docs/metrics.md).
func loopsAlive(loops ...interface{ Alive(time.Time) error }) healthz.Checker {
	return func(*http.Request) error {
		now := time.Now()
		for _, l := range loops {
			if err := l.Alive(now); err != nil {
				return err
			}
		}
		return nil
	}
}

// writeNamespaces returns the namespaces of the comma-separated list, and the gateway's, where the gateway Services
// live. An empty list returns none: the controller then writes in every namespace.
func writeNamespaces(list, gateway string) []string {
	var out []string
	for _, ns := range strings.Split(list, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return append(out, gateway)
}

// cnpgWatched reports whether the controller watches CloudNativePG's kinds: only while the cnpg plugin is on, since
// the chart grants the read with it, and their CRDs exist. A watch of a kind that the controller may not list never
// syncs, and the manager stops once its cache sync times out (RFC#0003, requirement 13).
func cnpgWatched(on bool, mapper meta.RESTMapper) bool {
	if !on {
		return false
	}
	_, err := mapper.RESTMapping(controller.CNPGCluster.GroupKind(), controller.CNPGCluster.Version)
	return err == nil
}

// localDocument returns the site's document as now gives it. A member observed longer ago than the site's peerTimeout
// is published as Unknown (RFC0004, requirement 15). now must be time.Now outside tests.
func localDocument(r client.Reader, local *exchange.Local, now func() time.Time) func() exchange.Document {
	return func() exchange.Document {
		name, maxAge := "", time.Duration(0)
		if site, err := controller.LocalSite(context.Background(), r); err == nil {
			name, maxAge = site.Name, site.Spec.Exchange.PeerTimeout.Duration
		}
		return local.Document(name, now(), maxAge)
	}
}

// peerURL is the base URL of a peer's observation document: the active gateway's peer listener, which routes
// /peers/<peer>/ over the link that the gateway picks for that peer's traffic. The controller never dials a peer
// itself (RFC0004, requirement 10).
func peerURL(ctx context.Context, r client.Reader, peer string) (string, error) {
	site, err := controller.LocalSite(ctx, r)
	if err != nil {
		return "", err
	}
	if site.Status.Gateway.Address == "" {
		return "", errors.New("no active gateway")
	}
	return fmt.Sprintf("http://%s/peers/%s", net.JoinHostPort(site.Status.Gateway.Address, strconv.Itoa(int(site.Spec.Gateway.PeerPort))), peer), nil
}

// leaderElectionID names the controller's leader election Lease.
const leaderElectionID = "strait-controller"

// reconcileTimeout bounds each reconcile, so that a request that hangs does not hold the one worker of its kind. It
// equals the leader election's renew deadline: an API server that takes longer to answer costs the Lease anyway.
const reconcileTimeout = 10 * time.Second

// managerOptions are the manager's options. The manager never releases the Lease itself: client-go would release it
// on a failed renewal too, before the manager stops its runnables. start releases it once they have stopped.
// The manager waits for every runnable to stop, with no timeout: a release while one still runs would let the next
// controller overlap with it. A runnable that hangs is ended by the pod's termination grace period, and the Lease
// then expires.
func managerOptions(scheme *runtime.Scheme, lock resourcelock.Interface, namespace, metricsAddr, probeAddr string, leaderElect bool) ctrl.Options {
	inGateway := cache.ByObject{Namespaces: map[string]cache.Config{namespace: {}}}
	return ctrl.Options{
		Scheme:                              scheme,
		Metrics:                             metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:              probeAddr,
		LeaderElection:                      leaderElect,
		LeaderElectionID:                    leaderElectionID,
		LeaderElectionNamespace:             namespace,
		LeaderElectionResourceLockInterface: lock,
		LeaderElectionReleaseOnCancel:       false,
		GracefulShutdownTimeout:             ptr.To(time.Duration(-1)),
		Controller:                          config.Controller{ReconciliationTimeout: reconcileTimeout},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}:           inGateway,
				&coordinationv1.Lease{}: inGateway,
			},
			// The controller never reads managedFields. An update from a copy without them leaves the API server's.
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	}
}

// options returns the manager's options, with the cache's reads limited to reads, and whether CloudNativePG's kinds are
// watched. The manager shares the REST mapper that tells: the cache maps each kind it limits when it starts, and a
// kind whose CRD does not exist would stop it.
func options(cfg *rest.Config, scheme *runtime.Scheme, lock resourcelock.Interface, namespace, metricsAddr, probeAddr string, leaderElect, cnpg bool, reads []string) (ctrl.Options, bool, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return ctrl.Options{}, false, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return ctrl.Options{}, false, err
	}
	watchCNPG := cnpgWatched(cnpg, mapper)
	opts := managerOptions(scheme, lock, namespace, metricsAddr, probeAddr, leaderElect)
	opts.MapperProvider = func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil }
	readsIn(&opts.Cache, reads, watchCNPG)
	return opts, watchCNPG, nil
}

// readsIn limits the cache's Services, EndpointSlices and, with cnpg, CloudNativePG's clusters and poolers to
// namespaces (RFC0007, requirement 26). With no namespaces, the cache holds those of the cluster.
func readsIn(opts *cache.Options, namespaces []string, cnpg bool) {
	if len(namespaces) == 0 {
		return
	}
	in := cache.ByObject{Namespaces: map[string]cache.Config{}}
	for _, ns := range namespaces {
		in.Namespaces[ns] = cache.Config{}
	}
	if opts.ByObject == nil {
		opts.ByObject = map[client.Object]cache.ByObject{}
	}
	opts.ByObject[&corev1.Service{}] = in
	opts.ByObject[&discoveryv1.EndpointSlice{}] = in
	if cnpg {
		for _, gvk := range []schema.GroupVersionKind{controller.CNPGCluster, controller.CNPGPooler} {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk)
			opts.ByObject[u] = in
		}
	}
}

// memoryLimit returns the Go runtime's soft memory limit: 90% of limit, a byte count, so that the garbage collector
// keeps the heap under the container's memory limit instead of letting it grow to twice the live heap. The chart sets
// limit from the container's limits.memory. A GOMEMLIMIT that is set, or a limit that is not a positive byte count,
// sets none.
func memoryLimit(limit, gomemlimit string) (int64, bool) {
	n, err := strconv.ParseInt(limit, 10, 64)
	if gomemlimit != "" || err != nil || n <= 0 {
		return 0, false
	}
	return n - n/10, true
}

// leaderLock is the controller's leader election Lease. It records no event: controller-runtime's own lock writes
// core Events, and the controller may only write events.k8s.io Events (RFC0007).
func leaderLock(cfg *rest.Config, namespace string) (resourcelock.Interface, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	cfg = rest.AddUserAgent(rest.CopyConfig(cfg), "leader-election")
	// Half the manager's renew deadline of 10s, so one hung request does not lose the Lease.
	cfg.Timeout = 5 * time.Second
	leases, err := coordinationv1client.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: namespace, Name: leaderElectionID},
		Client:     leases,
		LockConfig: resourcelock.ResourceLockConfig{Identity: host + "_" + string(uuid.NewUUID())},
	}, nil
}

func notify(ch chan event.GenericEvent) {
	select {
	case ch <- event.GenericEvent{Object: &straitv1.Site{}}:
	default:
	}
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "8082"
	}
	return port
}

// httpServer serves a handler until the context ends.
type httpServer struct {
	addr    string
	handler http.Handler
}

func (s *httpServer) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	return serve(ctx, lis, s.handler)
}

// serve serves handler on lis until ctx ends, then returns once the requests in flight have ended, or after 5s.
// So a leader-only server answers nothing after the manager releases the Lease.
func serve(ctx context.Context, lis net.Listener, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return nil
}

// leader wraps a runnable so it runs on the leader only.
type leader struct{ manager.Runnable }

func (leader) NeedLeaderElection() bool { return true }

func leaderOnly(r manager.Runnable) manager.Runnable { return leader{r} }
