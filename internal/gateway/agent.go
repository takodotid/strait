package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/controller"
)

// Timing of the Lease and the loops.
const (
	LeaseDuration     = 6 * time.Second
	RenewDeadline     = 4 * time.Second
	RetryPeriod       = time.Second
	WireGuardInterval = 2 * time.Second
	HealthInterval    = time.Second
)

// Agent is the gateway agent.
type Agent struct {
	// Client reads Sites, SitePeers and SiteLinks from a cache, reads Secrets from the API server, and writes status.
	Client client.Client
	// Leases holds the gateway Lease. Give it its own rate limit, apart from Client.
	Leases    coordinationv1client.LeasesGetter
	Kernel    Kernel
	Namespace string
	// Identity is the pod's name.
	Identity string
	// EnvoyAdmin is the base URL of Envoy's admin interface.
	EnvoyAdmin string
	HTTP       *http.Client
	// Changed receives a value when a Site, SitePeer or SiteLink spec changes.
	Changed <-chan struct{}
	Resolve func(hostport string) (*net.UDPAddr, error)
	Log     logr.Logger

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	held     map[string]string
	problems map[string]string
	logged   map[string]string
}

// Run takes part in the gateway Lease until ctx ends, and re-joins the election each time the Lease is lost.
// The interfaces are gone before the Lease is released.
func (a *Agent) Run(ctx context.Context) error {
	// A restarted container finds the interfaces of its last run in the pod's network namespace.
	a.removeAll()
	l := &lease{
		Interface: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Namespace: a.Namespace, Name: controller.GatewayLease},
			Client:     a.Leases,
			LockConfig: resourcelock.ResourceLockConfig{Identity: a.Identity},
		},
		before: a.stopLeading,
	}
	for ctx.Err() == nil {
		term, stepDown := context.WithCancel(ctx)
		le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            l,
			Name:            controller.GatewayLease,
			LeaseDuration:   LeaseDuration,
			RenewDeadline:   RenewDeadline,
			RetryPeriod:     RetryPeriod,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(lctx context.Context) { a.lead(lctx, l, stepDown) },
				OnStoppedLeading: func() { a.Log.Info("not leading") },
				OnNewLeader:      func(id string) { a.Log.Info("leader", "identity", id) },
			},
		})
		if err != nil {
			stepDown()
			return err
		}
		le.Run(term)
		stepDown()
		a.stopLeading()
	}
	return nil
}

// lease is the gateway Lease. It records each renewal, and stops the leader's work before a release.
type lease struct {
	resourcelock.Interface
	before func()

	mu      sync.Mutex
	renewed time.Time
}

// Create implements resourcelock.Interface.
func (l *lease) Create(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	start := time.Now()
	err := l.Interface.Create(ctx, r)
	l.record(start, err)
	return err
}

// Update implements resourcelock.Interface. A release is an update with no holder.
func (l *lease) Update(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	if r.HolderIdentity == "" {
		l.before()
		return l.Interface.Update(ctx, r)
	}
	start := time.Now()
	err := l.Interface.Update(ctx, r)
	l.record(start, err)
	return err
}

// record keeps the time a successful write started, since the Lease's renew time is set before the request is sent.
func (l *lease) record(start time.Time, err error) {
	if err == nil {
		l.mu.Lock()
		l.renewed = start
		l.mu.Unlock()
	}
}

func (l *lease) sinceRenewed() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.renewed)
}

// lead runs the WireGuard and health loops until lctx ends, then removes every interface.
// It steps down when the Lease goes unrenewed for RenewDeadline, so the interfaces are gone
// before the Lease can expire and another replica take it.
func (a *Agent) lead(lctx context.Context, l *lease, stepDown func()) {
	ctx, cancel := context.WithCancel(lctx)
	done := make(chan struct{})
	a.mu.Lock()
	if lctx.Err() != nil {
		a.mu.Unlock()
		cancel()
		return
	}
	a.cancel, a.done = cancel, done
	a.mu.Unlock()
	defer close(done)
	defer a.removeAll()

	a.Log.Info("leading")
	a.syncWireGuard(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { a.loop(ctx, WireGuardInterval, a.Changed, a.syncWireGuard) })
	wg.Go(func() { a.loop(ctx, HealthInterval, nil, a.reportHealth) })
	wg.Go(func() {
		a.loop(ctx, RetryPeriod/4, nil, func(context.Context) {
			if d := l.sinceRenewed(); d > RenewDeadline {
				a.Log.Info("the Lease was not renewed in time; stepping down", "since", d.String())
				stepDown()
				cancel()
			}
		})
	})
	wg.Wait()
}

// stopLeading stops the leader's loops and waits until the interfaces are gone.
func (a *Agent) stopLeading() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.cancel, a.done = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (a *Agent) loop(ctx context.Context, every time.Duration, kick <-chan struct{}, f func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
		f(ctx)
	}
}

// removeAll deletes every interface the agent may hold.
func (a *Agent) removeAll() {
	if devs, err := a.Kernel.Devices(); err == nil {
		for _, d := range devs {
			a.Log.Info("remove WireGuard interface", "interface", d.Name)
		}
	}
	var err error
	for range 3 {
		if _, err = Apply(a.Kernel, nil); err == nil {
			break
		}
	}
	if err != nil {
		a.Log.Error(err, "remove WireGuard interfaces")
	}
	a.setLinks(nil, nil)
	a.mu.Lock()
	maps.DeleteFunc(a.logged, func(k, _ string) bool { return strings.HasPrefix(k, "link/") })
	a.mu.Unlock()
}

// syncWireGuard brings the interfaces to the desired state and publishes the site's public key.
// When an input cannot be read, the interfaces stay as they are.
func (a *Agent) syncWireGuard(ctx context.Context) {
	site, in, err := a.inputs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.note("inputs", err.Error())
		}
		return
	}
	a.note("inputs", "")
	want, problems := Desired(in)
	if ctx.Err() != nil {
		return
	}
	failed, err := Apply(a.Kernel, want)
	if err != nil {
		a.note("apply", err.Error())
	} else {
		a.note("apply", "")
	}
	held := map[string]string{}
	for _, w := range want {
		if e := failed[w.Link]; e != nil {
			problems[w.Link] = e.Error()
			continue
		}
		held[w.Link] = w.Name
	}
	a.setLinks(held, problems)
	a.notePerLink(held, problems)
	if site != nil {
		a.publishKey(ctx, site, in.Key)
	}
}

// inputs reads what the desired state needs. Site is nil unless exactly one Site exists.
// An error means a read failed for a reason other than absence.
func (a *Agent) inputs(ctx context.Context) (*v1alpha1.Site, Inputs, error) {
	in := Inputs{Resolve: a.Resolve}
	var sites v1alpha1.SiteList
	if err := a.Client.List(ctx, &sites); err != nil {
		return nil, in, err
	}
	var site *v1alpha1.Site
	switch {
	case len(sites.Items) != 1:
		in.NoKey = fmt.Sprintf("want exactly one Site, found %d", len(sites.Items))
	case sites.Items[0].Spec.WireGuard == nil:
		site = &sites.Items[0]
		in.NoKey = "the Site sets no WireGuard private key"
	default:
		site = &sites.Items[0]
		key, noKey, err := a.privateKey(ctx, site.Spec.WireGuard.PrivateKeySecretRef)
		if err != nil {
			return nil, in, err
		}
		in.Key, in.NoKey = key, noKey
	}
	var peers v1alpha1.SitePeerList
	if err := a.Client.List(ctx, &peers); err != nil {
		return nil, in, err
	}
	var ls v1alpha1.SiteLinkList
	if err := a.Client.List(ctx, &ls); err != nil {
		return nil, in, err
	}
	in.Peers, in.Links = peers.Items, ls.Items
	return site, in, nil
}

// privateKey reads the site's private key. Neither the key nor the Secret's data ever reaches an error or a log.
func (a *Agent) privateKey(ctx context.Context, ref v1alpha1.SecretKeyRef) (*wgtypes.Key, string, error) {
	var s corev1.Secret
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: ref.Name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("Secret %s/%s not found", a.Namespace, ref.Name), nil
		}
		return nil, "", fmt.Errorf("get Secret %s/%s: %w", a.Namespace, ref.Name, err)
	}
	name := cmp.Or(ref.Key, "privateKey")
	v, ok := s.Data[name]
	if !ok {
		return nil, fmt.Sprintf("Secret %s/%s has no key %s", a.Namespace, ref.Name, name), nil
	}
	key, err := wgtypes.ParseKey(strings.TrimSpace(string(v)))
	if err != nil {
		return nil, fmt.Sprintf("Secret %s/%s key %s is not a WireGuard private key", a.Namespace, ref.Name, name), nil
	}
	return &key, "", nil
}

// publishKey writes the public key of the site's private key into the Site's status, when it differs.
func (a *Agent) publishKey(ctx context.Context, site *v1alpha1.Site, key *wgtypes.Key) {
	pub := ""
	if key != nil {
		pub = key.PublicKey().String()
	}
	if site.Status.WireGuardPublicKey == pub {
		return
	}
	patch := client.MergeFrom(site.DeepCopy())
	site.Status.WireGuardPublicKey = pub
	if err := a.Client.Status().Patch(ctx, site, patch); err != nil {
		if ctx.Err() == nil {
			a.Log.Error(err, "publish the WireGuard public key", "site", site.Name)
		}
		return
	}
	a.Log.Info("published the WireGuard public key", "site", site.Name, "publicKey", pub)
}

// reportHealth writes each SiteLink's status from Envoy's view of the peer clusters, when it changed.
func (a *Agent) reportHealth(ctx context.Context) {
	var ls v1alpha1.SiteLinkList
	if err := a.Client.List(ctx, &ls); err != nil {
		return
	}
	clusters, envoyErr := a.clusters(ctx)
	if ctx.Err() != nil {
		return
	}
	held, problems := a.links()
	handshakes := map[string]time.Time{}
	if devs, err := a.Kernel.Devices(); err == nil {
		for _, d := range devs {
			for _, p := range d.Peers {
				if p.LastHandshakeTime.After(handshakes[d.Name]) {
					handshakes[d.Name] = p.LastHandshakeTime
				}
			}
		}
	}
	obs := Observe(ls.Items, clusters, envoyErr, held, problems)
	now := time.Now()
	for i := range ls.Items {
		l := &ls.Items[i]
		iface := held[l.Name]
		st := LinkStatus(*l, obs[l.Name], iface, handshakes[iface], now)
		if equality.Semantic.DeepEqual(l.Status, st) {
			continue
		}
		old := l.DeepCopy()
		l.Status = st
		patch, err := statusPatch(old, l)
		if err == nil {
			err = a.Client.Status().Patch(ctx, l, patch)
		}
		if err != nil {
			if ctx.Err() == nil {
				a.Log.Error(err, "write SiteLink status", "link", l.Name)
			}
			continue
		}
		a.Log.V(1).Info("SiteLink status", "link", l.Name, "healthy", st.Healthy, "active", st.Active)
	}
}

// statusPatch is a merge patch from old to l that always carries status.healthy and status.active.
// A patch computed from two structs leaves out a false that both hold, and the stored object may lack it.
func statusPatch(old, l *v1alpha1.SiteLink) (client.Patch, error) {
	data, err := client.MergeFrom(old).Data(l)
	if err != nil {
		return nil, err
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	st, _ := p["status"].(map[string]any)
	if st == nil {
		st = map[string]any{}
		p["status"] = st
	}
	st["healthy"], st["active"] = l.Status.Healthy, l.Status.Active
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return client.RawPatch(types.MergePatchType, b), nil
}

// clusters reads the hosts of Envoy's clusters from its admin interface.
func (a *Agent) clusters(ctx context.Context) (map[string][]Host, error) {
	ctx, cancel := context.WithTimeout(ctx, HealthInterval)
	defer cancel()
	b, err := get(ctx, a.HTTP, a.EnvoyAdmin+"/clusters?format=json", 64<<20)
	if err != nil {
		return nil, err
	}
	return ParseClusters(b)
}

func get(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return b, nil
}

func (a *Agent) setLinks(held, problems map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held, a.problems = held, problems
}

func (a *Agent) links() (held, problems map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held, a.problems
}

// note logs a message under a key when it differs from the last one. An empty message clears the key.
func (a *Agent) note(key, msg string) {
	a.mu.Lock()
	if a.logged == nil {
		a.logged = map[string]string{}
	}
	last := a.logged[key]
	a.logged[key] = msg
	a.mu.Unlock()
	if msg != "" && msg != last {
		a.Log.Info(msg, "about", key)
	}
}

// notePerLink logs each link's interface or problem when it changes.
func (a *Agent) notePerLink(held, problems map[string]string) {
	for _, name := range slices.Sorted(maps.Keys(held)) {
		if problems[name] == "" {
			a.note("link/"+name, "WireGuard interface "+held[name]+" in place")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(problems)) {
		a.note("link/"+name, "WireGuard link: "+problems[name])
	}
}

// SpecChanges returns an informer handler that sends to ch when an object is added or deleted, or its spec changes.
func SpecChanges(ch chan<- struct{}) toolscache.ResourceEventHandler {
	send := func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { send() },
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(client.Object)
			n, ok2 := newObj.(client.Object)
			if !ok1 || !ok2 || o.GetGeneration() != n.GetGeneration() {
				send()
			}
		},
		DeleteFunc: func(any) { send() },
	}
}
