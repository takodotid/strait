package controller

import (
	"context"
	"fmt"
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
	// Reader is what plugins read through.
	Reader   client.Reader
	Recorder events.EventRecorder
	Peers    Peers
	Local    Local
	Now      func() time.Time

	// confirmed keeps each member's last confirmed time at full precision, by SiteService and member.
	confirmed map[types.NamespacedName]map[string]time.Time
}

// Reconcile implements reconcile.Reconciler.
func (r *SiteServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var svc straitv1.SiteService
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			r.Local.DeleteService(req.Namespace, req.Name)
			delete(r.confirmed, req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !svc.DeletionTimestamp.IsZero() {
		r.Local.DeleteService(req.Namespace, req.Name)
		return ctrl.Result{}, nil
	}
	site, err := LocalSite(ctx, r.Reader)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, err
	}
	now := r.Now()

	p, err := plugin.For(&svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	report := p.Observe(ctx, r.Reader, &svc, site.Name, now)
	local := make([]observe.Observation, 0, len(report.Members))
	for _, m := range plugin.LocalMembers(&svc, site.Name) {
		local = append(local, report.Members[m.Name])
	}
	r.Local.SetService(svc.Namespace, svc.Name, report.Declared, local)

	timeout := site.Spec.Exchange.PeerTimeout.Duration
	sites := map[string]decide.SiteView{site.Name: {Known: true, Declared: report.Declared}}
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
	result := decide.Decide(decide.Input{
		Spec:          svc.Spec,
		LocalSite:     site.Name,
		Sites:         sites,
		Observations:  observations,
		Previous:      svc.Status.Route.Members,
		LastConfirmed: confirmed,
		Now:           now,
	})

	if err := r.syncExportsImports(ctx, &svc, site.Name, report); err != nil {
		return ctrl.Result{}, err
	}
	conflict, err := r.syncService(ctx, &svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	endpoints := 0
	var paths map[string]straitv1.Path
	if !conflict {
		endpoints, paths, err = r.syncSlices(ctx, &svc, site, result.Members, observations)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.writeStatus(ctx, &svc, sites, observations, confirmed, result, endpoints, paths, conflict, now); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("decided", "route", result.Members, "reason", result.Reason, "endpoints", endpoints)
	return ctrl.Result{RequeueAfter: r.next(&svc, result, confirmed, now)}, nil
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
func (r *SiteServiceReconciler) next(svc *straitv1.SiteService, result decide.Result, confirmed map[string]time.Time, now time.Time) time.Duration {
	d := resync
	for _, m := range result.Held {
		left := confirmed[m].Add(svc.Spec.Policy.UnknownHold.Duration).Sub(now)
		if left > 0 && left < d {
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

// syncService keeps the application name's Service. It reports a conflict when another owner holds the name.
func (r *SiteServiceReconciler) syncService(ctx context.Context, svc *straitv1.SiteService) (bool, error) {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), &existing)
	switch {
	case err == nil && !metav1.IsControlledBy(&existing, svc):
		return true, nil
	case err != nil && !apierrors.IsNotFound(err):
		return false, err
	}
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
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
	return false, err
}

// target is one routed member's addresses and port, and the path they take.
type target struct {
	addresses []string
	port      int32
	path      straitv1.Path
}

// targets returns where each routed member takes traffic. A member with no target is left out.
func (r *SiteServiceReconciler) targets(ctx context.Context, svc *straitv1.SiteService, site *straitv1.Site, members []string, observations map[string]observe.Observation) (map[string]target, error) {
	direct, err := endpointDirectPeers(ctx, r.Reader)
	if err != nil {
		return nil, err
	}
	out := map[string]target{}
	for _, name := range members {
		var m straitv1.Member
		for _, sm := range svc.Spec.Members {
			if sm.Name == name {
				m = sm
			}
		}
		o := observations[name]
		switch {
		case m.Site == site.Name:
			if len(o.Endpoint.Addresses) > 0 && o.Endpoint.Port > 0 {
				out[name] = target{o.Endpoint.Addresses, o.Endpoint.Port, straitv1.PathLocal}
			}
		case direct[m.Site] && len(o.Endpoint.Addresses) > 0 && o.Endpoint.Port > 0:
			out[name] = target{o.Endpoint.Addresses, o.Endpoint.Port, straitv1.PathEndpointDirect}
		default:
			var imp straitv1.Import
			if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: memberObjectName(svc, name)}, &imp); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, err
			}
			if imp.Status.GatewayPort > 0 && site.Status.Gateway.Address != "" {
				out[name] = target{[]string{site.Status.Gateway.Address}, imp.Status.GatewayPort, straitv1.PathGateway}
			}
		}
	}
	return out, nil
}

// endpointDirectPeers returns the peers whose active link is a healthy direct link with endpointDirect.
func endpointDirectPeers(ctx context.Context, r client.Reader) (map[string]bool, error) {
	var links straitv1.SiteLinkList
	if err := r.List(ctx, &links); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, l := range links.Items {
		if l.Spec.Transport == straitv1.TransportDirect && l.Spec.Direct != nil && l.Spec.Direct.EndpointDirect && l.Status.Active && l.Status.Healthy {
			out[l.Spec.Peer] = true
		}
	}
	return out, nil
}

// syncSlices writes one EndpointSlice per routed member that has a target and deletes the rest.
// It returns the number of endpoints written, and the path of each member that has endpoints.
func (r *SiteServiceReconciler) syncSlices(ctx context.Context, svc *straitv1.SiteService, site *straitv1.Site, members []string, observations map[string]observe.Observation) (int, map[string]straitv1.Path, error) {
	targets, err := r.targets(ctx, svc, site, members, observations)
	if err != nil {
		return 0, nil, err
	}
	count := 0
	paths := map[string]straitv1.Path{}
	for member, t := range targets {
		name := memberObjectName(svc, member)
		es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
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
			return 0, nil, fmt.Errorf("endpointslice %s: %w", name, err)
		}
		count += len(es.Endpoints)
		if len(es.Endpoints) > 0 {
			paths[member] = t.path
		}
	}

	var existing discoveryv1.EndpointSliceList
	if err := r.List(ctx, &existing, client.InNamespace(svc.Namespace), client.MatchingLabels{LabelSiteService: svc.Name, LabelManagedBy: ManagedBy}); err != nil {
		return 0, nil, err
	}
	for i := range existing.Items {
		es := &existing.Items[i]
		if _, ok := targets[es.Labels[LabelMember]]; ok && es.Name == memberObjectName(svc, es.Labels[LabelMember]) {
			continue
		}
		if err := r.Delete(ctx, es); client.IgnoreNotFound(err) != nil {
			return 0, nil, err
		}
	}
	return count, paths, nil
}

func addressType(addresses []string) discoveryv1.AddressType {
	if len(addresses) > 0 && strings.Contains(addresses[0], ":") {
		return discoveryv1.AddressTypeIPv6
	}
	return discoveryv1.AddressTypeIPv4
}

func (r *SiteServiceReconciler) writeStatus(ctx context.Context, svc *straitv1.SiteService, sites map[string]decide.SiteView, observations map[string]observe.Observation, confirmed map[string]time.Time, result decide.Result, endpoints int, paths map[string]straitv1.Path, conflict bool, now time.Time) error {
	old := svc.Status.DeepCopy()
	st := &svc.Status
	st.ObservedGeneration = svc.Generation

	previous := map[string]straitv1.MemberStatus{}
	for _, m := range old.Members {
		previous[m.Name] = m
	}
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

	if !slices.Equal(old.Route.Members, result.Members) || old.Route.LastChangeTime == nil {
		st.Route.LastChangeTime = &metav1.Time{Time: now}
		if old.Route.LastChangeTime != nil {
			r.Recorder.Eventf(svc, nil, corev1.EventTypeNormal, "RouteChanged", "Route", "route %s -> %s (%s)", routeString(old.Route.Members), routeString(result.Members), result.Reason)
		}
	}
	st.Route.Members = result.Members
	st.Route.Reason = result.Reason
	st.Route.Message = result.Message
	st.Route.Endpoints = int32(endpoints)

	routed := metav1.Condition{Type: straitv1.ConditionRouted, Status: metav1.ConditionTrue, Reason: result.Reason, Message: result.Message, ObservedGeneration: svc.Generation}
	switch {
	case conflict:
		routed.Status, routed.Reason, routed.Message = metav1.ConditionFalse, "ServiceConflict", fmt.Sprintf("a Service named %s exists and Strait does not own it", svc.Name)
	case endpoints == 0 && len(result.Members) > 0:
		routed.Status, routed.Reason, routed.Message = metav1.ConditionFalse, "NoEndpoints", fmt.Sprintf("routed to %s but no endpoint is ready", routeString(result.Members))
	case endpoints == 0:
		routed.Status = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&st.Conditions, routed)
	setFlag(&st.Conditions, straitv1.ConditionMultiple, result.Multiple, straitv1.ReasonMultipleWritable, svc.Generation)
	setFlag(&st.Conditions, straitv1.ConditionDeclarationsDisagree, result.Disagree, straitv1.ReasonDeclarationsDisagree, svc.Generation)
	setFlag(&st.Conditions, straitv1.ConditionHeld, len(result.Held) > 0, straitv1.ReasonHeldUnknown, svc.Generation)

	if result.Multiple && !meta.IsStatusConditionTrue(old.Conditions, straitv1.ConditionMultiple) {
		r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, straitv1.ReasonMultipleWritable, "Decide", "%s", result.Message)
	}
	if result.Disagree && !meta.IsStatusConditionTrue(old.Conditions, straitv1.ConditionDeclarationsDisagree) {
		r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, straitv1.ReasonDeclarationsDisagree, "Decide", "%s", result.Message)
	}
	if result.Reason == straitv1.ReasonUnknownTimeout && old.Route.Reason != straitv1.ReasonUnknownTimeout {
		r.Recorder.Eventf(svc, nil, corev1.EventTypeWarning, straitv1.ReasonUnknownTimeout, "Decide", "%s", result.Message)
	}

	if !statusChanged(old, st, now) {
		return nil
	}
	return r.Status().Update(ctx, svc)
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
