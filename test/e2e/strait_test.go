//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	straitv1 "github.com/takodotid/strait/api/v1"
)

const (
	wireGuardSecret = "strait-wireguard"
	wireGuardPort   = 32100
)

// ensureNamespace creates a namespace with labels, or sets the labels on an existing one.
func ensureNamespace(ctx context.Context, c client.Client, name string, labels map[string]string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		for k, v := range labels {
			ns.Labels[k] = v
		}
		return nil
	})
	return err
}

// installStrait creates the site's WireGuard Secret from a key generated here, then installs the chart.
// The private key is never printed or written outside the cluster.
func (e *env) installStrait(ctx context.Context, s *site) error {
	if err := ensureNamespace(ctx, s.c, straitNamespace, map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}); err != nil {
		return err
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: wireGuardSecret, Namespace: straitNamespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"privateKey": key.String()},
	}
	if err := s.c.Create(ctx, secret); err != nil {
		return fmt.Errorf("secret %s: %w", wireGuardSecret, err)
	}
	s.wgPublicKey = key.PublicKey().String()

	i := strings.LastIndex(e.image, ":")
	if i < 0 || strings.Contains(e.image[i:], "/") {
		return fmt.Errorf("STRAIT_IMAGE %q has no tag", e.image)
	}
	// With externalTrafficPolicy Local, kube-proxy serves the direct Service only from an endpoint that names its node.
	// The peer dials the one node of the site, which hosts the gateway.
	values := fmt.Sprintf(`image: {repository: %q, tag: %q, pullPolicy: Never}
envoy: {image: {repository: %q, tag: %q, pullPolicy: Never}}
controller: {logLevel: debug}
site:
  name: %s
  spec:
    wireguard: {privateKeySecretRef: {name: %s}}
    exchange: {pollInterval: %s, peerTimeout: %s}
    gateway: {directService: {type: NodePort, externalTrafficPolicy: Local}}
`, e.image[:i], e.image[i+1:], envoyRepository, envoyTag, s.name, wireGuardSecret, pollInterval, peerTimeout)
	path := filepath.Join(e.dir, "values-"+s.name+".yaml")
	if err := os.WriteFile(path, []byte(values), 0o644); err != nil {
		return err
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "deploy", "helm", "strait"))
	if err != nil {
		return err
	}
	_, err = run(ctx, 6*time.Minute, s.kubeconfig, nil, "helm", "install", "strait", chart,
		"--kubeconfig", s.kubeconfig, "--kube-context", s.context, "--namespace", straitNamespace,
		"--values", path, "--wait", "--timeout", "5m")
	return err
}

// connectSites declares each site as the other's peer, with a direct link (cost 10) and a wireguard link (cost 100).
// Only site b dials the wireguard link.
func (e *env) connectSites(ctx context.Context) error {
	for _, s := range e.sites() {
		p := e.peer(s)
		peer := &straitv1.SitePeer{
			ObjectMeta: metav1.ObjectMeta{Name: p.name},
			Spec:       straitv1.SitePeerSpec{WireGuard: &straitv1.PeerWireGuard{PublicKey: p.wgPublicKey}},
		}
		direct := &straitv1.SiteLink{
			ObjectMeta: metav1.ObjectMeta{Name: p.name + "-direct"},
			Spec: straitv1.SiteLinkSpec{
				Peer: p.name, Transport: straitv1.TransportDirect, Cost: 10, Encryption: straitv1.EncryptionNone,
				Direct: &straitv1.DirectLink{Address: p.directIP},
			},
		}
		peerAddress, _, _ := strings.Cut(p.wgAddress, "/")
		wan := &straitv1.SiteLink{
			ObjectMeta: metav1.ObjectMeta{Name: p.name + "-wan"},
			Spec: straitv1.SiteLinkSpec{
				Peer: p.name, Transport: straitv1.TransportWireGuard, Cost: 100,
				WireGuard: &straitv1.WireGuardLink{ListenPort: wireGuardPort, Address: s.wgAddress, PeerAddress: peerAddress},
			},
		}
		if s == e.b {
			wan.Spec.WireGuard.Endpoint = fmt.Sprintf("%s:%d", e.a.wanIP, wireGuardPort)
		}
		objs := []client.Object{peer, direct, wan}
		if s == e.b {
			// Site b's SitePeer goes through the deprecated version, as a manifest written for v0.1 would.
			// The controller, the gateway and the CLI read it as v1.
			old, err := deprecated(peer)
			if err != nil {
				return err
			}
			objs[0] = old
		}
		for _, o := range objs {
			if err := s.c.Create(ctx, o); err != nil {
				return fmt.Errorf("site %s: create %s: %w", s.name, o.GetName(), err)
			}
		}
	}
	return nil
}

// deprecated returns the object as an unstructured object of strait.tako.id/v1alpha1.
func deprecated(o client.Object) (*unstructured.Unstructured, error) {
	gvks, _, err := scheme().ObjectKinds(o)
	if err != nil {
		return nil, err
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(straitv1.DeprecatedGroupVersion.WithKind(gvks[0].Kind))
	return u, nil
}

// linkState returns a site's SiteLinks by name.
func linkState(ctx context.Context, s *site) (map[string]straitv1.SiteLinkStatus, error) {
	var list straitv1.SiteLinkList
	if err := s.c.List(ctx, &list); err != nil {
		return nil, err
	}
	out := map[string]straitv1.SiteLinkStatus{}
	for _, l := range list.Items {
		out[l.Name] = l.Status
	}
	return out, nil
}

func linkString(links map[string]straitv1.SiteLinkStatus) string {
	var parts []string
	for name, st := range links {
		parts = append(parts, fmt.Sprintf("%s healthy=%t active=%t", name, st.Healthy, st.Active))
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}

// linksSteady reports whether both of the site's links to its peer are healthy and the direct link is active.
func (e *env) linksSteady(ctx context.Context, s *site) (bool, string) {
	p := e.peer(s).name
	links, err := linkState(ctx, s)
	if err != nil {
		return false, err.Error()
	}
	d, w := links[p+"-direct"], links[p+"-wan"]
	return d.Healthy && d.Active && w.Healthy && !w.Active, "site " + s.name + ": " + linkString(links)
}

// siteService reads a SiteService straight from the API server.
func siteService(ctx context.Context, s *site, namespace, name string) (*straitv1.SiteService, error) {
	var ss straitv1.SiteService
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &ss); err != nil {
		return nil, err
	}
	return &ss, nil
}

// routeString is "nowhere" or the routed members.
func routeString(members []string) string {
	if len(members) == 0 {
		return "nowhere"
	}
	return strings.Join(members, ",")
}

// describe is a one-line view of a SiteService's decision.
func describe(ss *straitv1.SiteService) string {
	var conds []string
	for _, c := range ss.Status.Conditions {
		conds = append(conds, fmt.Sprintf("%s=%s", c.Type, c.Status))
	}
	var members []string
	for _, m := range ss.Status.Members {
		members = append(members, fmt.Sprintf("%s(%s %s eligible=%t %s path=%s)", m.Name, m.Role, m.State, m.Eligible, m.Reason, m.Path))
	}
	return fmt.Sprintf("route=%s reason=%s endpoints=%d [%s] members=[%s] message=%q",
		routeString(ss.Status.Route.Members), ss.Status.Route.Reason, ss.Status.Route.Endpoints,
		strings.Join(conds, " "), strings.Join(members, " "), ss.Status.Route.Message)
}

// routeIs returns a check that the SiteService routes to the members, with one of the reasons when given.
func routeIs(s *site, namespace, name string, members []string, reasons ...string) func(ctx context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		ss, err := siteService(ctx, s, namespace, name)
		if err != nil {
			return false, err.Error()
		}
		ok := slices.Equal(ss.Status.Route.Members, members) && (len(reasons) == 0 || slices.Contains(reasons, ss.Status.Route.Reason))
		if len(members) > 0 {
			ok = ok && ss.Status.Route.Endpoints > 0 && pathsAsExpected(s, ss)
		}
		return ok, "site " + s.name + ": " + describe(ss)
	}
}

// pathsAsExpected reports whether every routed member shows path Local at its own site and Gateway elsewhere,
// and every other member shows none. The fixture has no endpoint-direct link.
func pathsAsExpected(s *site, ss *straitv1.SiteService) bool {
	for _, m := range ss.Status.Members {
		var want straitv1.Path
		switch {
		case !slices.Contains(ss.Status.Route.Members, m.Name):
		case m.Site == s.name:
			want = straitv1.PathLocal
		default:
			want = straitv1.PathGateway
		}
		if m.Path != want {
			return false
		}
	}
	return true
}

// conditionTrue reports whether the SiteService's condition is True.
func conditionTrue(ss *straitv1.SiteService, t string) bool {
	return meta.IsStatusConditionTrue(ss.Status.Conditions, t)
}

// sample is one read of a SiteService's route.
type sample struct {
	start, end time.Time
	route      []string
	reason     string
	err        error
}

func (s sample) names(member string) bool { return s.err == nil && slices.Contains(s.route, member) }
func (s sample) nowhere() bool            { return s.err == nil && len(s.route) == 0 }

// sampler reads a SiteService every sampleEvery until stopped.
type sampler struct {
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	samples []sample
}

func startSampler(ctx context.Context, s *site, namespace, name string) *sampler {
	sp := &sampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(sp.done)
		tick := time.NewTicker(sampleEvery)
		defer tick.Stop()
		for {
			start := time.Now()
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			ss, err := siteService(cctx, s, namespace, name)
			cancel()
			smp := sample{start: start, end: time.Now(), err: err}
			if err == nil {
				smp.route, smp.reason = ss.Status.Route.Members, ss.Status.Route.Reason
			}
			sp.mu.Lock()
			sp.samples = append(sp.samples, smp)
			sp.mu.Unlock()
			select {
			case <-sp.stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return sp
}

// StopAfter waits, for at most five samples, until a sample started after t, then stops the sampler and returns its samples.
func (sp *sampler) StopAfter(t time.Time) []sample {
	deadline := time.Now().Add(5 * sampleEvery)
	for time.Now().Before(deadline) {
		sp.mu.Lock()
		n := len(sp.samples)
		last := sample{}
		if n > 0 {
			last = sp.samples[n-1]
		}
		sp.mu.Unlock()
		if n > 0 && last.start.After(t) {
			break
		}
		time.Sleep(sampleEvery / 10)
	}
	return sp.Stop()
}

// Stop ends the sampler and returns its samples.
func (sp *sampler) Stop() []sample {
	select {
	case <-sp.stop:
	default:
		close(sp.stop)
	}
	<-sp.done
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return slices.Clone(sp.samples)
}

// summary lists the distinct consecutive routes of the samples, with the time of each change relative to t0.
func summary(samples []sample, t0 time.Time) string {
	var parts []string
	last := ""
	for _, s := range samples {
		cur := routeString(s.route) + "/" + s.reason
		if s.err != nil {
			cur = "error"
		}
		if cur != last {
			parts = append(parts, fmt.Sprintf("%+.1fs %s", s.start.Sub(t0).Seconds(), cur))
			last = cur
		}
	}
	return strings.Join(parts, " -> ")
}

// routeChange is one RouteChanged event of a SiteService.
type routeChange struct {
	at   time.Time
	note string
}

// routeChanges returns the RouteChanged events of a SiteService since a time, oldest first.
// Their times come from the controller's clock at microsecond precision.
func routeChanges(ctx context.Context, s *site, namespace, name string, since time.Time) ([]routeChange, error) {
	var list eventsv1.EventList
	if err := s.c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []routeChange
	for _, ev := range list.Items {
		if ev.Reason != "RouteChanged" || ev.Regarding.Kind != "SiteService" || ev.Regarding.Name != name {
			continue
		}
		at := ev.EventTime.Time
		if ev.Series != nil {
			at = ev.Series.LastObservedTime.Time
		}
		if at.Before(since) {
			continue
		}
		out = append(out, routeChange{at: at, note: ev.Note})
	}
	slices.SortFunc(out, func(a, b routeChange) int { return a.at.Compare(b.at) })
	return out, nil
}

// firstChange returns the time of the first route change whose note starts with prefix.
func firstChange(changes []routeChange, prefix string) (time.Time, bool) {
	for _, c := range changes {
		if strings.HasPrefix(c.note, prefix) {
			return c.at, true
		}
	}
	return time.Time{}, false
}
