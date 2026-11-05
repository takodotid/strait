package controller

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/decide"
	"github.com/takodotid/strait/internal/observe"
	"github.com/takodotid/strait/internal/plugin"
)

// resync bounds the time between two decisions for one SiteService.
const resync = 2 * time.Second

// statusRefresh bounds the age of the timestamps in a SiteService's status.
const statusRefresh = 10 * time.Second

// SiteServiceReconciler decides each SiteService's route and writes its Service and EndpointSlices.
type SiteServiceReconciler struct {
	client.Client
	// Reader is what the controller reads its inputs through. Plugins get it wrapped by plugin.ReadOnly.
	Reader client.Reader
	// APIReader reads from the API server, with no cache. A write of an EndpointSlice that finds the cache behind the
	// controller's own last write of it reads the slice through APIReader, and writes again. Nil writes once.
	APIReader client.Reader
	Recorder  events.EventRecorder
	Peers     Peers
	Local     Local
	Now       func() time.Time

	// confirmed keeps each member's last confirmed time at full precision, by SiteService and member.
	confirmed map[types.NamespacedName]map[string]time.Time
	// written keeps what the last status write of each SiteService returned, until the cache shows it.
	written written[straitv1.SiteService, *straitv1.SiteService]
	// inPlace keeps, by SiteService, the EndpointSlices in place after the last apply, so that a slice the cache does
	// not show yet is still deleted.
	inPlace map[types.NamespacedName]map[string]discoveryv1.EndpointSlice
	// decided keeps, by SiteService, the members of the last decision: the next decision's previous route.
	decided map[types.NamespacedName][]string
	// pluginFor returns the plugin of a SiteService. Nil means plugin.For. Tests set it.
	pluginFor func(*straitv1.SiteService) (plugin.Plugin, error)
}

// Reconcile implements reconcile.Reconciler.
func (r *SiteServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var svc straitv1.SiteService
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			r.Local.DeleteService(req.Namespace, req.Name)
			delete(r.confirmed, req.NamespacedName)
			delete(r.inPlace, req.NamespacedName)
			delete(r.decided, req.NamespacedName)
			r.written.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// The previous route, the status comparison and the events start from the status last written.
	r.written.latest(&svc)
	if !svc.DeletionTimestamp.IsZero() {
		r.Local.DeleteService(req.Namespace, req.Name)
		return ctrl.Result{}, nil
	}
	site, err := LocalSite(ctx, r.Reader)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, err
	}
	now := r.Now()

	pluginFor := plugin.For
	if r.pluginFor != nil {
		pluginFor = r.pluginFor
	}
	p, err := pluginFor(&svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A plugin reads and never writes (RFC#0003, requirement 4), whatever client the controller holds.
	report := p.Observe(ctx, plugin.ReadOnly(r.Reader), &svc, site.Name, now)
	local := make([]observe.Observation, 0, len(report.Members))
	for _, m := range plugin.LocalMembers(&svc, site.Name) {
		local = append(local, report.Members[m.Name])
	}
	r.Local.SetService(svc.Namespace, svc.Name, report.Declared, local)

	timeout := site.Spec.Exchange.PeerTimeout.Duration
	sites := map[string]decide.SiteView{site.Name: {Known: true, Declared: report.Declared, Conflicting: report.Conflicting}}
	observations := map[string]observe.Observation{}
	for name, o := range report.Members {
		observations[name] = o
	}
	for peer, ps := range r.Peers.View(svc.Namespace, svc.Name, svc.Spec.Members, now, timeout) {
		sites[peer] = decide.SiteView{Known: true, Declared: ps.Declared}
		for name, o := range ps.Members {
			observations[name] = o
		}
	}

	confirmed := r.lastConfirmed(&svc)
	for name, o := range observations {
		if o.Confirmed() {
			confirmed[name] = now
		}
	}
	// The startup hold counts from the first poll. Before it, polling has not begun, and the hold has just begun.
	started := r.Peers.Started()
	if started.IsZero() {
		started = now
	}
	read := map[string]bool{}
	for _, m := range svc.Spec.Members {
		if _, st := r.Peers.Snapshot(m.Site); m.Site != site.Name && !st.LastSuccess.IsZero() {
			read[m.Site] = true
		}
	}
	inPlace, err := r.slicesInPlace(ctx, &svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	in := decide.Input{
		Spec:          svc.Spec,
		LocalSite:     site.Name,
		Sites:         sites,
		Observations:  observations,
		Previous:      r.previousRoute(&svc, inPlace),
		LastConfirmed: confirmed,
		Now:           now,
		Started:       started,
		Read:          read,
	}
	result := decide.Decide(in)
	shown := startupStatus(result, in, r.Peers.Polled())
	if r.decided == nil {
		r.decided = map[types.NamespacedName][]string{}
	}
	r.decided[req.NamespacedName] = slices.Clone(result.Members)

	a := r.apply(ctx, &svc, site, result.Members, observations, report, inPlace)
	if a.err != nil {
		logger.Error(a.err, "route not applied", "route", result.Members)
	}
	if err := r.writeStatus(ctx, &svc, sites, observations, confirmed, shown, a, now); err != nil {
		logger.Error(err, "status not written")
	}
	logger.V(1).Info("decided", "route", result.Members, "reason", result.Reason, "endpoints", a.endpoints())
	// An error does not return: the controller's backoff would delay the decision that ends a hold.
	return ctrl.Result{RequeueAfter: r.next(&svc, result, confirmed, started, now)}, nil
}

// startupStatus returns the result that the status shows. Until the first poll of every peer has ended, read or not,
// while a site with a member has not been read, the reason is AwaitingPeers and the message gives the decision's
// reason. With a startup hold, this ends at the latest when the hold ends. The route, and so the endpoints, stay the
// decision's.
func startupStatus(result decide.Result, in decide.Input, polled bool) decide.Result {
	unread := in.Unread()
	hold := in.Spec.Policy.StartupHold.Duration
	if polled || len(unread) == 0 || result.Reason == straitv1.ReasonAwaitingPeers || (hold > 0 && in.Now.Sub(in.Started) >= hold) {
		return result
	}
	result.Message = fmt.Sprintf("the first poll of every peer since the controller started has not ended, and site %s is not read yet; the decision is %s: %s",
		strings.Join(unread, ", "), result.Reason, result.Message)
	result.Reason = straitv1.ReasonAwaitingPeers
	return result
}

// previousRoute returns the members of the last decision. After a start, it returns the members of status.route that
// have an EndpointSlice in place. Status alone can be older than the last decision, when a status write failed, and the
// slices alone can hold a member that the last decision dropped, when its delete failed.
func (r *SiteServiceReconciler) previousRoute(svc *straitv1.SiteService, inPlace map[string]discoveryv1.EndpointSlice) []string {
	if members, ok := r.decided[client.ObjectKeyFromObject(svc)]; ok {
		return members
	}
	var out []string
	for _, m := range svc.Status.Route.Members {
		if es, ok := inPlace[memberObjectName(svc, m)]; ok && es.Labels[LabelMember] == m {
			out = append(out, m)
		}
	}
	return out
}

// lastConfirmed returns each member's last confirmed time, from memory or else from status.
func (r *SiteServiceReconciler) lastConfirmed(svc *straitv1.SiteService) map[string]time.Time {
	if r.confirmed == nil {
		r.confirmed = map[types.NamespacedName]map[string]time.Time{}
	}
	key := client.ObjectKeyFromObject(svc)
	m, ok := r.confirmed[key]
	if !ok {
		m = map[string]time.Time{}
		for _, s := range svc.Status.Members {
			if s.LastConfirmedTime != nil {
				m[s.Name] = s.LastConfirmedTime.Time
			}
		}
		r.confirmed[key] = m
	}
	return m
}

// next returns when to decide again: at the next resync, or when a hold expires, whichever is first.
func (r *SiteServiceReconciler) next(svc *straitv1.SiteService, result decide.Result, confirmed map[string]time.Time, started, now time.Time) time.Duration {
	ends := make([]time.Time, 0, len(result.Held)+1)
	for _, m := range result.Held {
		ends = append(ends, confirmed[m].Add(svc.Spec.Policy.UnknownHold.Duration))
	}
	if result.Reason == straitv1.ReasonAwaitingPeers {
		ends = append(ends, started.Add(svc.Spec.Policy.StartupHold.Duration))
	}
	d := resync
	for _, end := range ends {
		if left := end.Sub(now); left > 0 && left < d {
			d = left + 10*time.Millisecond
		}
	}
	return d
}

func memberObjectName(svc *straitv1.SiteService, member string) string {
	return svc.Name + "-" + member
}

// syncExportsImports keeps one Export per local member and one Import per remote member.
func (r *SiteServiceReconciler) syncExportsImports(ctx context.Context, svc *straitv1.SiteService, site string, report observe.Report) error {
	wantExports := map[string]bool{}
	wantImports := map[string]bool{}
	for _, m := range svc.Spec.Members {
		name := memberObjectName(svc, m.Name)
		if m.Site == site {
			wantExports[name] = true
			ep := report.Members[m.Name].Endpoint
			if ep.Service == "" || ep.ServicePort == 0 {
				continue
			}
			exp := &straitv1.Export{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, exp, func() error {
				exp.Labels = map[string]string{LabelSiteService: svc.Name, LabelMember: m.Name}
				exp.Spec.Service = straitv1.ServicePortRef{Name: ep.Service, Port: ep.ServicePort}
				return controllerutil.SetControllerReference(svc, exp, r.Scheme())
			}); err != nil {
				return fmt.Errorf("export %s: %w", name, err)
			}
			continue
		}
		wantImports[name] = true
		imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, imp, func() error {
			imp.Labels = map[string]string{LabelSiteService: svc.Name, LabelMember: m.Name}
			imp.Spec = straitv1.ImportSpec{Site: m.Site, Export: name}
			return controllerutil.SetControllerReference(svc, imp, r.Scheme())
		}); err != nil {
			return fmt.Errorf("import %s: %w", name, err)
		}
	}

	var exports straitv1.ExportList
	if err := r.List(ctx, &exports, client.InNamespace(svc.Namespace), client.MatchingLabels{LabelSiteService: svc.Name}); err != nil {
		return err
	}
	for i := range exports.Items {
		e := &exports.Items[i]
		if !wantExports[e.Name] && metav1.IsControlledBy(e, svc) {
			if err := r.Delete(ctx, e); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	var imports straitv1.ImportList
	if err := r.List(ctx, &imports, client.InNamespace(svc.Namespace), client.MatchingLabels{LabelSiteService: svc.Name}); err != nil {
		return err
	}
	for i := range imports.Items {
		im := &imports.Items[i]
		if !wantImports[im.Name] && metav1.IsControlledBy(im, svc) {
			if err := r.Delete(ctx, im); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// serviceConflict reports whether another owner holds the application name's Service.
func (r *SiteServiceReconciler) serviceConflict(ctx context.Context, svc *straitv1.SiteService) (bool, error) {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing)
	switch {
	case err == nil:
		return !metav1.IsControlledBy(&existing, svc), nil
	case apierrors.IsNotFound(err):
		return false, nil
	}
	return false, fmt.Errorf("read service %s: %w", svc.Name, err)
}

// writeService writes the application name's Service.
func (r *SiteServiceReconciler) writeService(ctx context.Context, svc *straitv1.SiteService) error {
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[LabelSiteService] = svc.Name
		s.Spec.Selector = nil
		s.Spec.Ports = []corev1.ServicePort{{
			Name:       svc.Spec.Port.Name,
			Port:       svc.Spec.Port.Port,
			TargetPort: intstr.FromInt32(svc.Spec.Port.Port),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(svc, s, r.Scheme())
	})
	if err != nil {
		return fmt.Errorf("service %s: %w", svc.Name, err)
	}
	return nil
}

// target is one routed member's addresses and port, and the path they take.
type target struct {
	addresses []string
	port      int32
	path      straitv1.Path
}

// targets returns where each routed member takes traffic. A member with no target is left out. Dropped holds, by
// member, the addresses from a peer's document that endpoint-direct does not accept, with the reason.
func (r *SiteServiceReconciler) targets(ctx context.Context, svc *straitv1.SiteService, site *straitv1.Site, members []string, observations map[string]observe.Observation) (out map[string]target, dropped map[string][]string, err error) {
	direct, err := endpointDirectPeers(ctx, r.Reader)
	if err != nil {
		return nil, nil, err
	}
	out, dropped = map[string]target{}, map[string][]string{}
	for _, name := range members {
		var m straitv1.Member
		for _, sm := range svc.Spec.Members {
			if sm.Name == name {
				m = sm
			}
		}
		o := observations[name]
		if m.Site == site.Name {
			if len(o.Endpoint.Addresses) > 0 && o.Endpoint.Port > 0 {
				out[name] = target{o.Endpoint.Addresses, o.Endpoint.Port, straitv1.PathLocal}
			}
			continue
		}
		// A member whose document gives no address that endpoint-direct accepts takes the gateway.
		if ranges, ok := direct[m.Site]; ok && len(o.Endpoint.Addresses) > 0 {
			addresses, bad := ranges.accept(o.Endpoint.Addresses)
			if o.Endpoint.Port < 1 || o.Endpoint.Port > 65535 {
				addresses, bad = nil, []string{fmt.Sprintf("port %d (out of range)", o.Endpoint.Port)}
			}
			if len(bad) > 0 {
				dropped[name] = bad
			}
			if len(addresses) > 0 {
				out[name] = target{addresses, o.Endpoint.Port, straitv1.PathEndpointDirect}
				continue
			}
		}
		var imp straitv1.Import
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: memberObjectName(svc, name)}, &imp); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, err
		}
		if imp.Status.GatewayPort > 0 && site.Status.Gateway.Address != "" {
			out[name] = target{[]string{site.Status.Gateway.Address}, imp.Status.GatewayPort, straitv1.PathGateway}
		}
	}
	return out, dropped, nil
}

// directRanges are the ranges that endpoint-direct accepts a peer's member addresses from.
type directRanges struct {
	// limited is true when the link sets endpointCIDRs. Then only addresses in prefixes are accepted.
	limited  bool
	prefixes []netip.Prefix
}

// accept returns, in their canonical form, the addresses that endpoint-direct accepts, and each other address with
// the reason it is dropped. The addresses come from a peer's document, so none is trusted to be an IP address, and a
// dropped one is cut to 64 bytes: it goes into a condition's message, which the API server limits.
func (d directRanges) accept(addresses []string) (ok, dropped []string) {
	for _, a := range addresses {
		ip, err := netip.ParseAddr(a)
		ip = ip.Unmap()
		switch {
		case err != nil || ip.Zone() != "":
			if len(a) > 64 {
				a = a[:64] + "..."
			}
			dropped = append(dropped, fmt.Sprintf("%q (not an IP address)", a))
		case ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast():
			dropped = append(dropped, ip.String()+" (unspecified, loopback, link-local or multicast)")
		case d.limited && !slices.ContainsFunc(d.prefixes, func(p netip.Prefix) bool { return p.Contains(ip) }):
			dropped = append(dropped, ip.String()+" (outside endpointCIDRs)")
		default:
			ok = append(ok, ip.String())
		}
	}
	return ok, dropped
}

// endpointDirectPeers returns, for each peer whose active link is a healthy direct link with endpointDirect, the
// ranges that the link accepts member addresses from.
func endpointDirectPeers(ctx context.Context, r client.Reader) (map[string]directRanges, error) {
	var links straitv1.SiteLinkList
	if err := r.List(ctx, &links); err != nil {
		return nil, err
	}
	out := map[string]directRanges{}
	for _, l := range links.Items {
		if l.Spec.Transport == straitv1.TransportDirect && l.Spec.Direct != nil && l.Spec.Direct.EndpointDirect && l.Status.Active && l.Status.Healthy {
			d := directRanges{limited: len(l.Spec.Direct.EndpointCIDRs) > 0}
			for _, c := range l.Spec.Direct.EndpointCIDRs {
				// The CRD admits only CIDRs. One that does not parse admits nothing.
				if p, err := netip.ParsePrefix(c); err == nil {
					d.prefixes = append(d.prefixes, p.Masked())
				}
			}
			out[l.Spec.Peer] = d
		}
	}
	return out, nil
}

// slicesInPlace returns the application name's EndpointSlices by name: those that the cache lists, and those that
// this controller left in place at its last apply, which the cache may not show yet.
func (r *SiteServiceReconciler) slicesInPlace(ctx context.Context, svc *straitv1.SiteService) (map[string]discoveryv1.EndpointSlice, error) {
	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list, client.InNamespace(svc.Namespace), client.MatchingLabels{LabelSiteService: svc.Name, LabelManagedBy: ManagedBy}); err != nil {
		return nil, err
	}
	out := map[string]discoveryv1.EndpointSlice{}
	for _, es := range list.Items {
		out[es.Name] = es
	}
	for name, es := range r.inPlace[client.ObjectKeyFromObject(svc)] {
		out[name] = es
	}
	return out, nil
}

// applied is what one apply of a decision left in place.
type applied struct {
	// slices are the application name's EndpointSlices in place, by name.
	slices map[string]discoveryv1.EndpointSlice
	// written holds the path of each member whose slice this apply wrote.
	written map[string]straitv1.Path
	// owed holds the members whose slice this apply had to write.
	owed map[string]bool
	// conflict is true when another owner holds the application name's Service.
	conflict bool
	// dropped holds, by member, the addresses from a peer's document that endpoint-direct does not accept.
	dropped map[string][]string
	// err is the first read or write that failed. The steps after it did not run.
	err error
}

// endpoints returns the number of endpoints in place.
func (a applied) endpoints() int {
	n := 0
	for _, es := range a.slices {
		n += len(es.Endpoints)
	}
	return n
}

// in reports whether the member has an EndpointSlice in place.
func (a applied) in(svc *straitv1.SiteService, member string) (discoveryv1.EndpointSlice, bool) {
	es, ok := a.slices[memberObjectName(svc, member)]
	return es, ok && es.Labels[LabelMember] == member
}

// apply writes a decision in the order of RFC#0002, and stops at the first read or write that fails:
//  1. it deletes every EndpointSlice in place that is not the slice of a routed member with a target, and every one
//     when another owner holds the Service;
//  2. it writes the Exports and Imports, then the Service;
//  3. it writes the slice of each routed member with a target.
//
// A member leaves the route before another joins it, and a failure after step 1 leaves no member that the decision
// dropped. When the targets cannot be read, step 1 keeps the slices of every routed member.
func (r *SiteServiceReconciler) apply(ctx context.Context, svc *straitv1.SiteService, site *straitv1.Site, members []string, observations map[string]observe.Observation, report observe.Report, inPlace map[string]discoveryv1.EndpointSlice) (a applied) {
	a = applied{slices: inPlace, written: map[string]straitv1.Path{}, owed: map[string]bool{}}
	if r.inPlace == nil {
		r.inPlace = map[types.NamespacedName]map[string]discoveryv1.EndpointSlice{}
	}
	// What is in place when this returns, whatever fails.
	defer func() { r.inPlace[client.ObjectKeyFromObject(svc)] = a.slices }()

	conflict, cerr := r.serviceConflict(ctx, svc)
	a.conflict = conflict
	targets, dropped, terr := r.targets(ctx, svc, site, members, observations)
	a.dropped = dropped
	switch {
	case conflict:
		// Another owner's Service: no slice of this SiteService may feed it (RFC#0002, requirement 9).
	case terr == nil:
		for m := range targets {
			a.owed[m] = true
		}
	default:
		for _, m := range members {
			a.owed[m] = true
		}
	}

	for _, name := range slices.Sorted(maps.Keys(a.slices)) {
		es := a.slices[name]
		if m := es.Labels[LabelMember]; a.owed[m] && name == memberObjectName(svc, m) {
			continue
		}
		if err := r.Delete(ctx, &es); client.IgnoreNotFound(err) != nil {
			a.err = fmt.Errorf("delete endpointslice %s: %w", name, err)
			return a
		}
		delete(a.slices, name)
	}
	if a.err = cmp.Or(cerr, terr); a.err != nil {
		return a
	}

	if a.err = r.syncExportsImports(ctx, svc, site.Name, report); a.err != nil {
		return a
	}
	if conflict {
		return a
	}
	if a.err = r.writeService(ctx, svc); a.err != nil {
		return a
	}
	for _, member := range slices.Sorted(maps.Keys(targets)) {
		t := targets[member]
		// A slice's address type cannot change. A slice that would hold none of the member's addresses is deleted,
		// and written again with their address type (issue #38).
		if name := memberObjectName(svc, member); !hasFamily(t.addresses, a.slices[name].AddressType) {
			es := a.slices[name]
			if err := r.Delete(ctx, &es); client.IgnoreNotFound(err) != nil {
				a.err = fmt.Errorf("delete endpointslice %s: %w", name, err)
				return a
			}
			delete(a.slices, name)
		}
		es, err := r.writeSlice(ctx, svc, member, t)
		if err != nil {
			a.err = err
			return a
		}
		a.slices[es.Name] = *es
		a.written[member] = t.path
	}
	return a
}

// writeSlice writes the EndpointSlice of one routed member. Right after the controller's own last write of the slice,
// the cache may not show that write yet: a create then finds that the slice exists, and an update finds a newer
// version, or none after a delete. The slice is then read from the API server, and written once more (issue #38).
func (r *SiteServiceReconciler) writeSlice(ctx context.Context, svc *straitv1.SiteService, member string, t target) (*discoveryv1.EndpointSlice, error) {
	es, err := r.upsertSlice(ctx, r.Client, svc, member, t)
	if r.APIReader != nil && (apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) || apierrors.IsNotFound(err)) {
		es, err = r.upsertSlice(ctx, liveReads{Client: r.Client, live: r.APIReader}, svc, member, t)
	}
	if err != nil {
		return nil, fmt.Errorf("endpointslice %s: %w", memberObjectName(svc, member), err)
	}
	return es, nil
}

// liveReads is a client whose Get reads from the API server.
type liveReads struct {
	client.Client
	live client.Reader
}

func (l liveReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return l.live.Get(ctx, key, obj, opts...)
}

// upsertSlice creates or updates the EndpointSlice of one routed member, with the slice that c reads as its base.
func (r *SiteServiceReconciler) upsertSlice(ctx context.Context, c client.Client, svc *straitv1.SiteService, member string, t target) (*discoveryv1.EndpointSlice, error) {
	es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: memberObjectName(svc, member), Namespace: svc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, es, func() error {
		es.Labels = map[string]string{
			discoveryv1.LabelServiceName: svc.Name,
			LabelManagedBy:               ManagedBy,
			LabelSiteService:             svc.Name,
			LabelMember:                  member,
		}
		if es.AddressType == "" {
			es.AddressType = addressType(t.addresses)
		}
		es.Ports = []discoveryv1.EndpointPort{{
			Name:     ptr.To(svc.Spec.Port.Name),
			Port:     ptr.To(t.port),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}}
		es.Endpoints = nil
		for _, a := range t.addresses {
			if addressType([]string{a}) != es.AddressType {
				continue
			}
			es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{
				Addresses:  []string{a},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			})
		}
		return controllerutil.SetControllerReference(svc, es, r.Scheme())
	}); err != nil {
		return nil, err
	}
	return es, nil
}

// hasFamily reports whether a slice of address type at holds one of the addresses. A slice that is not in place,
// with no address type, holds any.
func hasFamily(addresses []string, at discoveryv1.AddressType) bool {
	return at == "" || slices.ContainsFunc(addresses, func(a string) bool { return addressType([]string{a}) == at })
}

func addressType(addresses []string) discoveryv1.AddressType {
	if len(addresses) > 0 && strings.Contains(addresses[0], ":") {
		return discoveryv1.AddressTypeIPv6
	}
	return discoveryv1.AddressTypeIPv4
}

// writeStatus writes the status of a decision and of its apply. The route shows the decision's members, less each
// member whose EndpointSlice the apply had to write and that is not in place. The endpoints, the paths and the Routed
// condition show the EndpointSlices in place.
func (r *SiteServiceReconciler) writeStatus(ctx context.Context, svc *straitv1.SiteService, sites map[string]decide.SiteView, observations map[string]observe.Observation, confirmed map[string]time.Time, result decide.Result, a applied, now time.Time) error {
	old := svc.Status.DeepCopy()
	st := &svc.Status
	st.ObservedGeneration = svc.Generation

	previous := map[string]straitv1.MemberStatus{}
	for _, m := range old.Members {
		previous[m.Name] = m
	}
	// A slice that this apply did not write keeps the path that status last gave it.
	paths := map[string]straitv1.Path{}
	for _, m := range svc.Spec.Members {
		es, ok := a.in(svc, m.Name)
		if !ok || len(es.Endpoints) == 0 {
			continue
		}
		if p, ok := a.written[m.Name]; ok {
			paths[m.Name] = p
		} else {
			paths[m.Name] = previous[m.Name].Path
		}
	}
	route := []string{}
	for _, m := range result.Members {
		if _, ok := a.in(svc, m); ok || !a.owed[m] {
			route = append(route, m)
		}
	}
	if len(route) == 0 {
		route = nil
	}
	endpoints := a.endpoints()
	st.Members = nil
	for _, m := range svc.Spec.Members {
		o, ok := observations[m.Name]
		if !ok {
			o = observe.Unknown(m.Name, m.Site, "NotObserved", now)
		}
		ms := straitv1.MemberStatus{
			Name: m.Name, Site: m.Site, Role: o.Role, Eligible: o.Eligible, State: o.State, Reason: o.Reason,
			ObservedTime: previous[m.Name].ObservedTime, Path: paths[m.Name],
		}
		if !o.ObservedAt.IsZero() {
			ms.ObservedTime = &metav1.Time{Time: o.ObservedAt}
		}
		if t, ok := confirmed[m.Name]; ok {
			ms.LastConfirmedTime = &metav1.Time{Time: t}
		}
		st.Members = append(st.Members, ms)
	}

	st.Declarations = nil
	names := make([]string, 0, len(sites))
	for s := range sites {
		names = append(names, s)
	}
	slices.Sort(names)
	for _, s := range names {
		st.Declarations = append(st.Declarations, straitv1.Declaration{Site: s, Primary: sites[s].Declared})
	}

	// Events are recorded once the status that shows them is written, so a write that fails does not repeat them.
	var events []func()
	event := func(eventType, reason, action, note string, args ...any) {
		events = append(events, func() { r.Recorder.Eventf(svc, nil, eventType, reason, action, note, args...) })
	}
	if !slices.Equal(old.Route.Members, route) || old.Route.LastChangeTime == nil {
		st.Route.LastChangeTime = &metav1.Time{Time: now}
		if old.Route.LastChangeTime != nil {
			event(corev1.EventTypeNormal, "RouteChanged", "Route", "route %s -> %s (%s)", routeString(old.Route.Members), routeString(route), result.Reason)
		}
	}
	st.Route.Members = route
	st.Route.Reason = result.Reason
	st.Route.Message = result.Message
	st.Route.Endpoints = int32(endpoints)

	routed := metav1.Condition{Type: straitv1.ConditionRouted, Status: metav1.ConditionTrue, Reason: result.Reason, Message: result.Message, ObservedGeneration: svc.Generation}
	switch {
	case a.conflict:
		routed.Status, routed.Reason, routed.Message = metav1.ConditionFalse, "ServiceConflict", fmt.Sprintf("a Service named %s exists and Strait does not own it", svc.Name)
	case endpoints == 0 && len(result.Members) > 0 && a.err != nil:
		routed.Status, routed.Reason, routed.Message = metav1.ConditionFalse, "NoEndpoints", fmt.Sprintf("routed to %s but no endpoint is written; see the %s condition", routeString(result.Members), straitv1.ConditionApplied)
	case endpoints == 0 && len(result.Members) > 0:
		routed.Status, routed.Reason, routed.Message = metav1.ConditionFalse, "NoEndpoints", fmt.Sprintf("routed to %s but no endpoint is ready", routeString(result.Members))
	case endpoints == 0:
		routed.Status = metav1.ConditionFalse
	}
	if note := droppedNote(a.dropped); note != "" {
		routed.Message = strings.TrimPrefix(routed.Message+"; "+note, "; ")
	}
	meta.SetStatusCondition(&st.Conditions, routed)
	written := metav1.Condition{Type: straitv1.ConditionApplied, Status: metav1.ConditionTrue, Reason: "Synced", ObservedGeneration: svc.Generation}
	if a.err != nil {
		written.Status, written.Reason, written.Message = metav1.ConditionFalse, "SyncFailed", a.err.Error()
	}
	meta.SetStatusCondition(&st.Conditions, written)
	setFlag(&st.Conditions, straitv1.ConditionMultiple, result.Multiple, straitv1.ReasonMultipleWritable, svc.Generation)
	setFlag(&st.Conditions, straitv1.ConditionDeclarationsDisagree, result.Disagree, straitv1.ReasonDeclarationsDisagree, svc.Generation)
	setFlag(&st.Conditions, straitv1.ConditionHeld, len(result.Held) > 0, straitv1.ReasonHeldUnknown, svc.Generation)

	if result.Multiple && !meta.IsStatusConditionTrue(old.Conditions, straitv1.ConditionMultiple) {
		event(corev1.EventTypeWarning, straitv1.ReasonMultipleWritable, "Decide", "%s", result.Message)
	}
	if result.Disagree && !meta.IsStatusConditionTrue(old.Conditions, straitv1.ConditionDeclarationsDisagree) {
		event(corev1.EventTypeWarning, straitv1.ReasonDeclarationsDisagree, "Decide", "%s", result.Message)
	}
	if result.Reason == straitv1.ReasonUnknownTimeout && old.Route.Reason != straitv1.ReasonUnknownTimeout {
		event(corev1.EventTypeWarning, straitv1.ReasonUnknownTimeout, "Decide", "%s", result.Message)
	}

	if !statusChanged(old, st, now) {
		return nil
	}
	wrote, err := r.written.updateStatus(ctx, r.Client, svc)
	if err != nil || !wrote {
		return err
	}
	for _, e := range events {
		e()
	}
	return nil
}

// droppedNote names the addresses that endpoint-direct dropped, at most 5 per member.
func droppedNote(dropped map[string][]string) string {
	var parts []string
	for _, m := range slices.Sorted(maps.Keys(dropped)) {
		d := dropped[m]
		more := ""
		if len(d) > 5 {
			d, more = d[:5], fmt.Sprintf(" and %d more", len(d)-5)
		}
		parts = append(parts, fmt.Sprintf("endpoint-direct dropped from %s: %s%s", m, strings.Join(d, ", "), more))
	}
	return strings.Join(parts, "; ")
}

func setFlag(conds *[]metav1.Condition, t string, on bool, reason string, gen int64) {
	c := metav1.Condition{Type: t, Status: metav1.ConditionFalse, Reason: "No" + reason, ObservedGeneration: gen}
	if on {
		c.Status, c.Reason = metav1.ConditionTrue, reason
	}
	meta.SetStatusCondition(conds, c)
}

// statusChanged ignores timestamps that moved by less than statusRefresh, so a steady state does not write every resync.
func statusChanged(old, cur *straitv1.SiteServiceStatus, now time.Time) bool {
	a, b := old.DeepCopy(), cur.DeepCopy()
	for _, s := range []*straitv1.SiteServiceStatus{a, b} {
		for i := range s.Members {
			s.Members[i].ObservedTime, s.Members[i].LastConfirmedTime = nil, nil
		}
	}
	if !reflect.DeepEqual(a, b) {
		return true
	}
	if len(old.Members) != len(cur.Members) {
		return true
	}
	for i := range cur.Members {
		if stale(old.Members[i].ObservedTime, cur.Members[i].ObservedTime, now) || stale(old.Members[i].LastConfirmedTime, cur.Members[i].LastConfirmedTime, now) {
			return true
		}
	}
	return false
}

func stale(old, cur *metav1.Time, now time.Time) bool {
	if (old == nil) != (cur == nil) {
		return true
	}
	return old != nil && !old.Equal(cur) && now.Sub(old.Time) >= statusRefresh
}

func routeString(members []string) string {
	if len(members) == 0 {
		return "nowhere"
	}
	return strings.Join(members, ",")
}
