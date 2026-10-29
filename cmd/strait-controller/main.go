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
	"strconv"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	"github.com/takodotid/strait/internal/version"
	"github.com/takodotid/strait/internal/xds"
)

func main() {
	var (
		metricsAddr, probeAddr, observationsAddr, xdsAddr string
		gatewayNamespace, observationsService             string
		leaderElect                                       bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address of the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address of the health probes.")
	flag.StringVar(&observationsAddr, "observations-bind-address", ":8082", "Address of the observation document.")
	flag.StringVar(&xdsAddr, "xds-bind-address", ":18000", "Address of the gateway's xDS server.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", os.Getenv("POD_NAMESPACE"), "Namespace of the gateway, its Lease and its Services.")
	flag.StringVar(&observationsService, "observations-service", "", "Host name of the controller's Service, which the gateway forwards observation requests to.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Elect one active controller.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	if gatewayNamespace == "" {
		log.Error(errors.New("missing"), "--gateway-namespace or POD_NAMESPACE is required")
		os.Exit(2)
	}
	if err := run(metricsAddr, probeAddr, observationsAddr, xdsAddr, gatewayNamespace, observationsService, leaderElect); err != nil {
		log.Error(err, "controller stopped")
		os.Exit(1)
	}
}

func run(metricsAddr, probeAddr, observationsAddr, xdsAddr, gatewayNamespace, observationsService string, leaderElect bool) error {
	log := ctrl.Log.WithName("setup")
	log.Info("starting", "version", version.Version)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		return err
	}
	inGateway := cache.ByObject{Namespaces: map[string]cache.Config{gatewayNamespace: {}}}
	cfg := ctrl.GetConfigOrDie()
	lock, err := leaderLock(cfg, gatewayNamespace)
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                              scheme,
		Metrics:                             metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:              probeAddr,
		LeaderElection:                      leaderElect,
		LeaderElectionID:                    leaderElectionID,
		LeaderElectionNamespace:             gatewayNamespace,
		LeaderElectionResourceLockInterface: lock,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:           inGateway,
			&coordinationv1.Lease{}: inGateway,
		}},
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	})
	if err != nil {
		return err
	}
	_, err = mgr.GetRESTMapper().RESTMapping(controller.CNPGCluster.GroupKind(), controller.CNPGCluster.Version)
	watchCNPG := err == nil
	log.Info("cloudnative-pg", "watched", watchCNPG)

	reader := mgr.GetClient()
	local := &exchange.Local{}
	xdsServer := xds.NewServer(reader, gatewayNamespace, xdsAddr, net.JoinHostPort(observationsService, portOf(observationsAddr)))

	ssChanges := make(chan event.GenericEvent, 1)
	impChanges := make(chan event.GenericEvent, 1)
	poller := &exchange.Poller{
		Peers: func() []string {
			var peers straitv1.SitePeerList
			if err := reader.List(context.Background(), &peers); err != nil {
				return nil
			}
			out := make([]string, 0, len(peers.Items))
			for _, p := range peers.Items {
				out = append(out, p.Name)
			}
			return out
		},
		URL: func(peer string) (string, error) {
			site, err := controller.LocalSite(context.Background(), reader)
			if err != nil {
				return "", err
			}
			if site.Status.Gateway.Address == "" {
				return "", errors.New("no active gateway")
			}
			return fmt.Sprintf("http://%s/peers/%s", net.JoinHostPort(site.Status.Gateway.Address, strconv.Itoa(int(site.Spec.Gateway.PeerPort))), peer), nil
		},
		Exchange: func() straitv1.ExchangeSpec {
			site, err := controller.LocalSite(context.Background(), reader)
			if err != nil {
				return straitv1.ExchangeSpec{}
			}
			return site.Spec.Exchange
		},
		Changed: func(string) {
			notify(ssChanges)
			notify(impChanges)
			xdsServer.Trigger()
		},
	}

	if err := (&controller.SiteServiceReconciler{
		Client:   mgr.GetClient(),
		Reader:   reader,
		Recorder: mgr.GetEventRecorder("strait-controller"),
		Peers:    poller,
		Local:    local,
		Now:      time.Now,
	}).SetupWithManager(mgr, ssChanges, watchCNPG); err != nil {
		return fmt.Errorf("siteservice controller: %w", err)
	}
	if err := (&controller.ExportReconciler{Client: mgr.GetClient(), Reader: reader, Local: local}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("export controller: %w", err)
	}
	if err := (&controller.ImportReconciler{Client: mgr.GetClient(), Reader: reader, Peers: poller, Now: time.Now}).SetupWithManager(mgr, impChanges); err != nil {
		return fmt.Errorf("import controller: %w", err)
	}
	if err := (&controller.GatewayReconciler{Client: mgr.GetClient(), Reader: reader, Namespace: gatewayNamespace}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("gateway controller: %w", err)
	}
	if err := (&controller.XDSTrigger{Trigger: xdsServer.Trigger, Namespace: gatewayNamespace}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("xds trigger: %w", err)
	}

	document := func() exchange.Document {
		name := ""
		if site, err := controller.LocalSite(context.Background(), reader); err == nil {
			name = site.Name
		}
		return local.Document(name, time.Now())
	}
	for _, r := range []manager.Runnable{
		leaderOnly(&httpServer{addr: observationsAddr, handler: exchange.NewHandler(document)}),
		leaderOnly(poller),
		leaderOnly(&exchange.StatusWriter{Client: mgr.GetClient(), Poller: poller}),
		xdsServer,
	} {
		if err := mgr.Add(r); err != nil {
			return err
		}
	}

	ctrlmetrics.Registry.MustRegister(&metrics.Collector{Reader: reader})

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
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
	return mgr.Start(ctrl.SetupSignalHandler())
}

// leaderElectionID names the controller's leader election Lease.
const leaderElectionID = "strait-controller"

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
	srv := &http.Server{Addr: s.addr, Handler: s.handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// leader wraps a runnable so it runs on the leader only.
type leader struct{ manager.Runnable }

func (leader) NeedLeaderElection() bool { return true }

func leaderOnly(r manager.Runnable) manager.Runnable { return leader{r} }
