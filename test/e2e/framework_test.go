//go:build e2e

// Package e2e runs Strait end to end on two kind clusters, one per site, with CloudNativePG in the distributed topology.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// Docker networks. Other strait-* clusters share them: the test creates them when absent and never deletes them.
const (
	netWAN          = "strait-net"
	netWANSubnet    = "172.30.0.0/24"
	netDirect       = "strait-direct"
	netDirectSubnet = "172.31.0.0/24"
)

// Pinned versions.
const (
	kindVersion        = "v0.33.0"
	envoyRepository    = "envoyproxy/envoy"
	envoyTag           = "distroless-v1.40.2"
	postgresImage      = "ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie"
	cnpgImage          = "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1"
	agnhostImage       = "registry.k8s.io/e2e-test-images/agnhost:2.53"
	cnpgManifestURL    = "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml"
	cnpgManifestSHA256 = "37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb"
	defaultStraitImage = "ghcr.io/takodotid/strait:dev"
)

// Namespaces of the fixture.
const (
	straitNamespace = "strait-system"
	dbNamespace     = "db"
	webNamespace    = "web"
)

// Timing of the exchange and of the pg SiteService.
const (
	pollInterval = time.Second
	peerTimeout  = 3 * time.Second
	unknownHold  = 10 * time.Second
	sampleEvery  = 500 * time.Millisecond
)

// totalTimeout bounds the whole run, below go test's own -timeout, which panics without running cleanups.
const totalTimeout = 45 * time.Minute

// site is one kind cluster and the Strait site it runs.
type site struct {
	name        string
	cluster     string
	context     string
	kubeconfig  string
	podCIDR     string
	serviceCIDR string
	// directIP is the node's pinned address on strait-direct.
	directIP string
	// wgAddress is this side's tunnel address on the wireguard link.
	wgAddress string
	// node is the kind node container. wanIP is its address on strait-net.
	node  string
	wanIP string
	// wgPublicKey is the site's WireGuard public key. The private key only exists in the site's Secret.
	wgPublicKey string
	c           client.Client
}

// env is the fixture shared by every scenario.
type env struct {
	dir   string
	out   string
	image string
	keep  bool
	a, b  *site
	times *timings
}

func (e *env) sites() []*site { return []*site{e.a, e.b} }

// peer returns the other site.
func (e *env) peer(s *site) *site {
	if s == e.a {
		return e.b
	}
	return e.a
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir, err := os.MkdirTemp("", "strait-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "_output", "e2e")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	ctrllog.SetLogger(logr.Discard())
	image := os.Getenv("STRAIT_IMAGE")
	if image == "" {
		image = defaultStraitImage
	}
	mk := func(name, podCIDR, serviceCIDR, directIP, wgAddress string) *site {
		cluster := "strait-e2e-" + name
		return &site{
			name: name, cluster: cluster, context: "kind-" + cluster,
			kubeconfig: filepath.Join(dir, "kubeconfig-"+name),
			podCIDR:    podCIDR, serviceCIDR: serviceCIDR, directIP: directIP, wgAddress: wgAddress,
		}
	}
	e := &env{
		dir: dir, out: out, image: image, keep: os.Getenv("STRAIT_E2E_KEEP") == "1",
		a:     mk("a", "10.110.0.0/16", "10.111.0.0/16", "172.31.0.11", "100.64.0.1/30"),
		b:     mk("b", "10.120.0.0/16", "10.121.0.0/16", "172.31.0.21", "100.64.0.2/30"),
		times: &timings{path: filepath.Join(out, "timings.txt"), run: time.Now().UTC().Format(time.RFC3339)},
	}
	// Printed to stderr at once, so a kept fixture can be found even if the test hangs.
	for _, s := range e.sites() {
		fmt.Fprintf(os.Stderr, "e2e: site %s: kubectl --kubeconfig %s --context %s\n", s.name, s.kubeconfig, s.context)
	}
	return e
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := straitv1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// connect builds the site's client for its own kubeconfig and context.
func (s *site) connect() error {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: s.kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: s.context},
	).ClientConfig()
	if err != nil {
		return err
	}
	// A paused site must not hang a call.
	cfg.Timeout = 10 * time.Second
	cfg.QPS, cfg.Burst = 50, 100
	s.c, err = client.New(cfg, client.Options{Scheme: scheme()})
	return err
}

// run runs a command and returns its standard output. The error holds the end of standard error.
// Every child gets KUBECONFIG, so no tool falls back to the user's kubeconfig.
func run(ctx context.Context, timeout time.Duration, kubeconfig string, stdin io.Reader, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	cmd.Stdin = stdin
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tail(stderr.String()+stdout.String(), 2000))
	}
	return stdout.String(), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// kubectl runs kubectl against the site, always with an explicit kubeconfig and context.
func (s *site) kubectl(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return run(ctx, timeout, s.kubeconfig, nil, "kubectl", append([]string{"--kubeconfig", s.kubeconfig, "--context", s.context}, args...)...)
}

// docker runs docker. The kubeconfig is set only as a guard.
func (e *env) docker(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return e.dockerStdin(ctx, timeout, nil, args...)
}

// dockerStdin runs docker with stdin.
func (e *env) dockerStdin(ctx context.Context, timeout time.Duration, stdin io.Reader, args ...string) (string, error) {
	return run(ctx, timeout, filepath.Join(e.dir, "kubeconfig-none"), stdin, "docker", args...)
}

// eventually calls check every interval until it reports done, and fails the test after timeout with the last state.
// It returns the time it took.
func eventually(ctx context.Context, t testing.TB, timeout, interval time.Duration, what string, check func(ctx context.Context) (bool, string)) time.Duration {
	t.Helper()
	d, last, err := poll(ctx, timeout, interval, check)
	if err != nil {
		t.Fatalf("%s: %v; last: %s", what, err, last)
	}
	return d
}

// poll is eventually without a test: it returns the time it took, the last state, and an error on timeout.
func poll(ctx context.Context, timeout, interval time.Duration, check func(ctx context.Context) (bool, string)) (time.Duration, string, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	last := ""
	for {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		done, state := check(cctx)
		cancel()
		if state != "" {
			last = state
		}
		if done {
			return time.Since(start), last, nil
		}
		if time.Now().After(deadline) {
			return time.Since(start), last, fmt.Errorf("not within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return time.Since(start), last, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// parallel runs fns at once and returns the first error.
func parallel(fns ...func() error) error {
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Go(func() { errs[i] = fn() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// timings logs each measured duration and appends it to _output/e2e/timings.txt, so a failed run keeps them.
type timings struct {
	mu   sync.Mutex
	path string
	run  string
}

func (r *timings) record(t testing.TB, name string, d time.Duration) {
	t.Helper()
	d = d.Round(time.Millisecond)
	t.Logf("timing %s: %s", name, d)
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("timings: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\t%s\n", r.run, name, d)
}
