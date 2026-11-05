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
	"net/netip"
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

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/controller"
)

// Timing of the Lease and the loops. LeaseDuration, RenewDeadline and RetryPeriod are the defaults of
// Site.spec.gateway.lease.
const (
	LeaseDuration     = 6 * time.Second
	RenewDeadline     = 4 * time.Second
	RetryPeriod       = time.Second
	WireGuardInterval = 2 * time.Second
	HealthInterval    = time.Second
	// EnvoyNotReady is how long Envoy may be not ready before the active replica steps down. It matches the chart's
	// readiness probe of the Envoy container, 3 failures 2s apart: the Lease moves only once the pod is not ready and
	// the controller has stopped publishing it, so a shorter stall moves nothing.
	EnvoyNotReady = 6 * time.Second
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
	// Poke sends a packet to a peer's tunnel address after the agent drops the link's session. Nil sends none.
	Poke func(peer netip.Addr) error
	Log  logr.Logger

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	held     map[string]string
	problems map[string]string
	logged   map[string]string
	want     []Interface
	// sessionTurn is when the leader's session loop last finished a turn. Zero while not leading.
	sessionTurn time.Time
	// kernel serializes the changes to the interfaces, so that no change acts on a read taken before another.
	kernel sync.Mutex
}

// SessionGrace is how long the leader's session loop may go without a turn before the agent counts as stuck. The loop
// turns every SessionInterval, and only waits for the kernel. Tests shorten it.
var SessionGrace = 30 * time.Second

// Alive returns an error while this replica leads and its session loop has not finished a turn for SessionGrace. The
// loop reads the WireGuard interfaces through netlink, under the lock that every change of an interface holds, and
// makes no request to the API server or Envoy. So it fails when a change of the interfaces hangs, and never for a
// slow API server or a link that is down. A standby replica always passes. now must come from time.Now.
func (a *Agent) Alive(now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil || a.sessionTurn.IsZero() || now.Sub(a.sessionTurn) <= SessionGrace {
		return nil
	}
	return fmt.Errorf("the leader's WireGuard session loop has not turned for %s", now.Sub(a.sessionTurn).Round(time.Millisecond))
}

func (a *Agent) turned(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionTurn = t
}

// Run takes part in the gateway Lease until ctx ends, and re-joins the election each time the Lease is lost.
// It joins only while Envoy is ready. The interfaces are gone before the Lease is released.
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
		ready:  func(ctx context.Context) error { return EnvoyReady(ctx, a.HTTP, a.EnvoyAdmin) },
	}
	for ctx.Err() == nil {
		if !a.waitForEnvoy(ctx) {
			break
		}
		l.reset()
		term, stepDown := context.WithCancel(ctx)
		config := func(t Timing) leaderelection.LeaderElectionConfig {
			l.setRenewDeadline(t.RenewDeadline)
			return leaderelection.LeaderElectionConfig{
				Lock:            l,
				Name:            controller.GatewayLease,
				LeaseDuration:   t.LeaseDuration,
				RenewDeadline:   t.RenewDeadline,
				RetryPeriod:     t.RetryPeriod,
				ReleaseOnCancel: true,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(lctx context.Context) { a.lead(lctx, l, stepDown) },
					OnStoppedLeading: func() { a.Log.Info("not leading") },
					OnNewLeader:      func(id string) { a.Log.Info("leader", "identity", id) },
				},
			}
		}
		t := a.timing(ctx)
		le, err := leaderelection.NewLeaderElector(config(t))
		if err != nil {
			// The CRD rejects such timings. A Site written past it falls back to the defaults.
			a.Log.Error(err, "the Site's gateway Lease timing cannot be used; using the defaults", "duration", t.LeaseDuration.String(),
				"renewDeadline", t.RenewDeadline.String(), "retryPeriod", t.RetryPeriod.String())
			le, err = leaderelection.NewLeaderElector(config(LeaseTiming(nil)))
		}
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

// Timing is the timing of the gateway Lease.
type Timing struct {
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
}

// LeaseTiming returns the timing of the gateway Lease that the Site sets, with the default for each field it leaves
// unset or that a nil Site leaves out.
func LeaseTiming(site *straitv1.Site) Timing {
	t := Timing{LeaseDuration, RenewDeadline, RetryPeriod}
	if site == nil {
		return t
	}
	l := site.Spec.Gateway.Lease
	t.LeaseDuration = cmp.Or(l.Duration.Duration, t.LeaseDuration)
	t.RenewDeadline = cmp.Or(l.RenewDeadline.Duration, t.RenewDeadline)
	t.RetryPeriod = cmp.Or(l.RetryPeriod.Duration, t.RetryPeriod)
	return t
}

// timing reads the timing of the gateway Lease from the one Site. It gives the defaults when the Site cannot be read.
func (a *Agent) timing(ctx context.Context) Timing {
	var sites straitv1.SiteList
	if err := a.Client.List(ctx, &sites); err != nil || len(sites.Items) != 1 {
		return LeaseTiming(nil)
	}
	return LeaseTiming(&sites.Items[0])
}

// waitForEnvoy returns true once Envoy is ready, and false if ctx ends first.
func (a *Agent) waitForEnvoy(ctx context.Context) bool {
	logged := false
	for {
		err := EnvoyReady(ctx, a.HTTP, a.EnvoyAdmin)
		if err == nil {
			return true
		}
		if !logged && ctx.Err() == nil {
			a.Log.Info("Envoy is not ready; not joining the gateway election", "error", err.Error())
			logged = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(RetryPeriod):
		}
	}
}

// lease is the gateway Lease. It records each renewal, stops the leader's work before a release, and takes the Lease
// only while ready allows it.
type lease struct {
	resourcelock.Interface
	before func()
	// ready is checked before each write that takes the Lease, as opposed to one that renews it. Nil allows every one.
	ready func(context.Context) error

	mu      sync.Mutex
	renewed time.Time
	// renewDeadline is how long the active replica may go without a renewal before it steps down.
	renewDeadline time.Duration
	// holding is true from a write that took or renewed the Lease in this election round until the Lease is released,
	// or a read shows another holder or none.
	holding bool
}

// reset starts an election round: the next write takes the Lease.
func (l *lease) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holding = false
}

// Get implements resourcelock.Interface.
func (l *lease) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	r, raw, err := l.Interface.Get(ctx)
	if err == nil && r.HolderIdentity != l.Identity() {
		l.reset()
	}
	return r, raw, err
}

// Create implements resourcelock.Interface. A create takes the Lease.
func (l *lease) Create(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	if err := l.mayTake(ctx); err != nil {
		return err
	}
	start := time.Now()
	err := l.Interface.Create(ctx, r)
	l.record(start, err)
	return err
}

// Update implements resourcelock.Interface. A release is an update with no holder.
func (l *lease) Update(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	if r.HolderIdentity == "" {
		l.before()
		err := l.Interface.Update(ctx, r)
		if err == nil {
			l.reset()
		}
		return err
	}
	l.mu.Lock()
	holding := l.holding
	l.mu.Unlock()
	if !holding {
		if err := l.mayTake(ctx); err != nil {
			return err
		}
	}
	start := time.Now()
	err := l.Interface.Update(ctx, r)
	l.record(start, err)
	return err
}

// mayTake returns an error when ready refuses that this replica take the Lease (RFC#0005, requirement 33).
func (l *lease) mayTake(ctx context.Context) error {
	if l.ready == nil {
		return nil
	}
	if err := l.ready(ctx); err != nil {
		return fmt.Errorf("not taking the gateway Lease: Envoy is not ready: %w", err)
	}
	return nil
}

// record keeps the time a successful write started, since the Lease's renew time is set before the request is sent.
func (l *lease) record(start time.Time, err error) {
	if err == nil {
		l.mu.Lock()
		l.renewed = start
		l.holding = true
		l.mu.Unlock()
	}
}

func (l *lease) sinceRenewed() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.renewed)
}

func (l *lease) setRenewDeadline(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewDeadline = d
}

// deadline is the renew deadline of the election round, RenewDeadline unless set.
func (l *lease) deadline() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cmp.Or(l.renewDeadline, RenewDeadline)
}

// lead runs the WireGuard and health loops until lctx ends, then removes every interface.
// It steps down when the Lease goes unrenewed for the round's renew deadline, so the interfaces are gone
// before the Lease can expire and another replica take it. It also steps down when Envoy has not been ready for
// EnvoyNotReady, so that a ready standby can take the Lease.
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
		// The first sync, above, may wait for the API server. The loop's turns count from here.
		a.turned(time.Now())
		s := sessions{}
		a.loop(ctx, SessionInterval, nil, func(ctx context.Context) {
			a.renewStale(ctx, s, time.Now)
			a.turned(time.Now())
		})
	})
	wg.Go(func() {
		a.loop(ctx, RetryPeriod/4, nil, func(context.Context) {
			if d := l.sinceRenewed(); d > l.deadline() {
				a.Log.Info("the Lease was not renewed in time; stepping down", "since", d.String())
				stepDown()
				cancel()
			}
		})
	})
	wg.Go(func() {
		// notReady is when the first check that failed since Envoy was last ready began. Zero while Envoy is ready.
		var notReady time.Time
		a.loop(ctx, HealthInterval, nil, func(ctx context.Context) {
			start := time.Now()
			err := EnvoyReady(ctx, a.HTTP, a.EnvoyAdmin)
			switch {
			case err == nil:
				notReady = time.Time{}
			case notReady.IsZero():
				notReady = start
			case time.Since(notReady) >= EnvoyNotReady && ctx.Err() == nil:
				a.Log.Info("Envoy is not ready; stepping down", "since", time.Since(notReady).String(), "error", err.Error())
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
	a.sessionTurn = time.Time{}
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
	a.kernel.Lock()
	for range 3 {
		if _, err = Apply(a.Kernel, nil); err == nil {
			break
		}
	}
	a.kernel.Unlock()
	if err != nil {
		a.Log.Error(err, "remove WireGuard interfaces")
	}
	a.setLinks(nil, nil)
	a.mu.Lock()
	a.want = nil
	maps.DeleteFunc(a.logged, func(k, _ string) bool { return strings.HasPrefix(k, "link/") })
	a.mu.Unlock()
}

// renewStale drops the WireGuard session of each dialing link whose peer sent nothing for StaleSession while this side
// sent, and sends a packet through the link so that WireGuard starts a handshake. A peer that stays silent is dropped
// again every StaleRetry.
func (a *Agent) renewStale(ctx context.Context, s sessions, now func() time.Time) {
	a.kernel.Lock()
	defer a.kernel.Unlock()
	if ctx.Err() != nil {
		return
	}
	devs, err := a.Kernel.Devices()
	if err != nil {
		return
	}
	a.mu.Lock()
	want := a.want
	a.mu.Unlock()
	drop, answered := s.check(want, devs, now())
	for _, st := range answered {
		a.Log.Info("the WireGuard peer answers again", "link", st.W.Link, "interface", st.W.Name, "after", st.Silent.Round(time.Millisecond).String(), "drops", st.Drops)
	}
	for _, st := range drop {
		if err := a.Kernel.Configure(st.W.Name, renewConfig(st.W)); err != nil {
			a.Log.Error(err, "drop the WireGuard session", "link", st.W.Link, "interface", st.W.Name)
			continue
		}
		s.dropped(st.W.Name, now())
		if a.Poke != nil {
			if err := a.Poke(st.W.PeerIP.Addr()); err != nil {
				a.Log.V(1).Info("send a packet to the WireGuard peer", "link", st.W.Link, "error", err.Error())
			}
		}
		if st.Drops == 0 {
			a.Log.Info("the WireGuard peer sent nothing; dropping the session to start a new handshake", "link", st.W.Link, "interface", st.W.Name, "silent", st.Silent.Round(time.Millisecond).String())
		}
	}
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
	a.kernel.Lock()
	failed, err := Apply(a.Kernel, want)
	a.kernel.Unlock()
	a.mu.Lock()
	a.want = want
	a.mu.Unlock()
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
func (a *Agent) inputs(ctx context.Context) (*straitv1.Site, Inputs, error) {
	in := Inputs{Resolve: a.Resolve}
	var sites straitv1.SiteList
	if err := a.Client.List(ctx, &sites); err != nil {
		return nil, in, err
	}
	var site *straitv1.Site
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
	var peers straitv1.SitePeerList
	if err := a.Client.List(ctx, &peers); err != nil {
		return nil, in, err
	}
	var ls straitv1.SiteLinkList
	if err := a.Client.List(ctx, &ls); err != nil {
		return nil, in, err
	}
	in.Peers, in.Links = peers.Items, ls.Items
	return site, in, nil
}

// privateKey reads the site's private key. Neither the key nor the Secret's data ever reaches an error or a log.
func (a *Agent) privateKey(ctx context.Context, ref straitv1.SecretKeyRef) (*wgtypes.Key, string, error) {
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
func (a *Agent) publishKey(ctx context.Context, site *straitv1.Site, key *wgtypes.Key) {
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
	var ls straitv1.SiteLinkList
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
		log := a.Log.V(1)
		if old.Status.Healthy != st.Healthy || old.Status.Active != st.Active {
			// A link that fails, comes back, or starts or stops carrying the pair's traffic.
			log = a.Log
		}
		log.Info("SiteLink status", "link", l.Name, "peer", l.Spec.Peer, "healthy", st.Healthy, "active", st.Active,
			"reason", obs[l.Name].Reason, "message", obs[l.Name].Message)
	}
}

// statusPatch is a merge patch from old to l that always carries status.healthy and status.active.
// A patch computed from two structs leaves out a false that both hold, and the stored object may lack it.
func statusPatch(old, l *straitv1.SiteLink) (client.Patch, error) {
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
