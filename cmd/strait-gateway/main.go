// Command strait-gateway is the gateway agent of RFC#0005. It runs next to Envoy in each gateway pod.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/gateway"
	"github.com/takodotid/strait/internal/version"
)

type options struct {
	envoyAdmin string
	probeAddr  string
	kubeconfig string
	context    string
}

func main() {
	var o options
	fs := flag.NewFlagSet("strait-gateway", flag.ExitOnError)
	fs.StringVar(&o.envoyAdmin, "envoy-admin", "http://127.0.0.1:9901", "base URL of Envoy's admin interface")
	fs.StringVar(&o.probeAddr, "probe-addr", ":8090", "address that serves /healthz and /readyz")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to a kubeconfig, to run outside a cluster")
	fs.StringVar(&o.context, "context", "", "kubeconfig context, to run outside a cluster")
	_ = fs.Parse(os.Args[1:])

	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil))
	ctrllog.SetLogger(log)
	klog.SetLogger(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, log); err != nil {
		log.Error(err, "exit")
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, log logr.Logger) error {
	pod, ns := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE")
	if pod == "" || ns == "" {
		return errors.New("POD_NAME and POD_NAMESPACE must be set")
	}
	log.Info("start", "version", version.Version, "pod", pod, "namespace", ns)

	httpc := &http.Client{Timeout: 2 * time.Second}
	probes := &http.Server{Addr: o.probeAddr, Handler: gateway.Probes(o.envoyAdmin, httpc), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := probes.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "probes")
		}
	}()
	defer func() { _ = probes.Close() }()

	cfg, err := restConfig(o.kubeconfig, o.context)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	changed := make(chan struct{}, 1)
	for _, obj := range []client.Object{&straitv1.Site{}, &straitv1.SitePeer{}, &straitv1.SiteLink{}} {
		inf, err := c.GetInformer(ctx, obj)
		if err != nil {
			return err
		}
		if _, err := inf.AddEventHandler(gateway.SpecChanges(changed)); err != nil {
			return err
		}
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			log.Error(err, "cache")
		}
	}()
	if !c.WaitForCacheSync(ctx) {
		return errors.New("cache did not sync")
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: c, DisableFor: []client.Object{&corev1.Secret{}}}})
	if err != nil {
		return err
	}
	leases, err := coordinationv1client.NewForConfig(rest.CopyConfig(cfg))
	if err != nil {
		return err
	}
	kernel, err := gateway.NewNetlink()
	if err != nil {
		return err
	}
	defer func() { _ = kernel.Close() }()

	a := &gateway.Agent{
		Client:     cl,
		Leases:     leases,
		Kernel:     kernel,
		Namespace:  ns,
		Identity:   pod,
		EnvoyAdmin: o.envoyAdmin,
		HTTP:       httpc,
		Changed:    changed,
		Resolve:    func(hostport string) (*net.UDPAddr, error) { return net.ResolveUDPAddr("udp", hostport) },
		Log:        log,
	}
	return a.Run(ctx)
}

func restConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	if kubeconfig == "" && kubeContext == "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w; set --kubeconfig or --context to run outside a cluster", err)
		}
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
}
