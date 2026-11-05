package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/exchange"
	"github.com/takodotid/strait/internal/observe"
)

// faults is a fake API server that can fail one write, and that records every state, after a write, in which the
// application name web has the EndpointSlices of two members.
type faults struct {
	// fail is the write to fail, counted from 1 since the last arm. 0 fails none.
	fail int
	// failOn fails every write whose description contains it, while armed.
	failOn string
	armed  bool
	writes []string
	// twice holds each write after which two members had a slice.
	twice []string
}

// arm counts writes from now on, and fails the nth.
func (f *faults) arm(n int) {
	f.armed, f.fail, f.failOn, f.writes = true, n, "", nil
}

// armOn fails, from now on, every write whose description contains what.
func (f *faults) armOn(what string) {
	f.armed, f.fail, f.failOn, f.writes = true, 0, what, nil
}

func (f *faults) disarm() { f.armed = false }

func (f *faults) client(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	do := func(ctx context.Context, c client.Client, what string, write func() error) error {
		if f.armed {
			f.writes = append(f.writes, what)
			if len(f.writes) == f.fail || (f.failOn != "" && strings.Contains(what, f.failOn)) {
				return apierrors.NewInternalError(fmt.Errorf("injected failure of write %d, %s", len(f.writes), what))
			}
		}
		if err := write(); err != nil {
			return err
		}
		if m := sliceMembers(ctx, t, c); len(m) > 1 {
			f.twice = append(f.twice, fmt.Sprintf("after %s: %v", what, m))
		}
		return nil
	}
	name := func(verb string, obj client.Object) string {
		return fmt.Sprintf("%s %T %s", verb, obj, obj.GetName())
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&straitv1.SiteService{}, &straitv1.Export{}, &straitv1.Import{}, &straitv1.Site{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return do(ctx, c, name("create", obj), func() error { return c.Create(ctx, obj, opts...) })
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return do(ctx, c, name("update", obj), func() error { return c.Update(ctx, obj, opts...) })
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				return do(ctx, c, name("patch", obj), func() error { return c.Patch(ctx, obj, p, opts...) })
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return do(ctx, c, name("delete", obj), func() error { return c.Delete(ctx, obj, opts...) })
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return do(ctx, c, name("update status of", obj), func() error { return c.SubResource(sub).Update(ctx, obj, opts...) })
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				return do(ctx, c, name("patch status of", obj), func() error { return c.SubResource(sub).Patch(ctx, obj, p, opts...) })
			},
		}).Build()
}

// sliceMembers returns the members that have an EndpointSlice of the application name web, sorted.
func sliceMembers(ctx context.Context, t *testing.T, c client.Reader) []string {
	t.Helper()
	var list discoveryv1.EndpointSliceList
	if err := c.List(ctx, &list, client.InNamespace("app"), client.MatchingLabels{discoveryv1.LabelServiceName: "web"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, es := range list.Items {
		seen[es.Labels[LabelMember]] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// declaring is a fresh peer b that declares active, with web-b primary when it is active and a replica otherwise.
func declaring(active string) peers {
	role := straitv1.RoleReplica
	if active == "web-b" {
		role = straitv1.RolePrimary
	}
	return peers{
		fresh: map[string]bool{"b": true},
		view: map[string]observe.Report{"b": {Declared: active, Members: map[string]observe.Observation{
			"web-b": {Member: "web-b", Site: "b", Role: role, Eligible: true, State: straitv1.StateStable, ObservedAt: now},
		}}},
	}
}

// routedToA returns a reconciler at site a whose route is web-a, with web-b reachable through the gateway.
func routedToA(t *testing.T, f *faults) (*SiteServiceReconciler, client.Client) {
	t.Helper()
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"), imp)
	c := f.client(t, objs...)
	r := &SiteServiceReconciler{
		Client: c, Reader: c, Recorder: events.NewFakeRecorder(100), Peers: declaring("web-a"), Local: &exchange.Local{},
		Now: func() time.Time { return now },
	}
	run(t, r)
	if m := sliceMembers(context.Background(), t, c); !slices.Equal(m, []string{"web-a"}) {
		t.Fatalf("before the switch: slices of %v", m)
	}
	return r, c
}

// declare makes both sites declare active.
func declare(t *testing.T, r *SiteServiceReconciler, c client.Client, active string) {
	t.Helper()
	var ss straitv1.SiteService
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &ss); err != nil {
		t.Fatal(err)
	}
	ss.Spec.Plugin.Declared.Active = active
	if err := c.Update(context.Background(), &ss); err != nil {
		t.Fatal(err)
	}
	r.Peers = declaring(active)
}

func reconcileWeb(r *SiteServiceReconciler) error {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web"}})
	return err
}

// A route that moves from one member to another in one reconcile passes through nowhere: two members never have an
// EndpointSlice at once, whichever write fails (issue #29).
func TestSwitchNeverRoutesTwo(t *testing.T) {
	// The writes of a clean switch.
	f := &faults{}
	r, c := routedToA(t, f)
	declare(t, r, c, "web-b")
	f.arm(0)
	if err := reconcileWeb(r); err != nil {
		t.Fatal(err)
	}
	f.disarm()
	if len(f.twice) > 0 {
		t.Fatalf("clean switch: two members routed %v; writes %v", f.twice, f.writes)
	}
	writes := slices.Clone(f.writes)
	if len(writes) == 0 {
		t.Fatal("the switch wrote nothing")
	}

	for n := 1; n <= len(writes); n++ {
		t.Run(fmt.Sprintf("fail write %d", n), func(t *testing.T) {
			f := &faults{}
			r, c := routedToA(t, f)
			declare(t, r, c, "web-b")
			f.arm(n)
			_ = reconcileWeb(r)
			f.disarm()
			if len(f.twice) > 0 {
				t.Fatalf("failed %s: two members routed %v", f.writes[n-1], f.twice)
			}
			if m := sliceMembers(context.Background(), t, c); len(m) > 1 {
				t.Fatalf("failed %s: slices of %v", f.writes[n-1], m)
			}

			// The next reconcile completes the switch.
			if err := reconcileWeb(r); err != nil {
				t.Fatal(err)
			}
			if m := sliceMembers(context.Background(), t, c); !slices.Equal(m, []string{"web-b"}) {
				t.Fatalf("after failed %s and a clean reconcile: slices of %v", f.writes[n-1], m)
			}
			if st := status(t, c); !slices.Equal(st.Route.Members, []string{"web-b"}) || st.Route.Reason != straitv1.ReasonActive {
				t.Fatalf("after failed %s and a clean reconcile: route %+v", f.writes[n-1], st.Route)
			}
			if len(f.twice) > 0 {
				t.Fatalf("after failed %s: two members routed %v", f.writes[n-1], f.twice)
			}
		})
	}
}

// hiding is a cache that does not show some EndpointSlices yet.
type hiding struct {
	client.Client
	hidden map[string]bool
}

func (h *hiding) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*discoveryv1.EndpointSlice); ok && h.hidden[key.Name] {
		return apierrors.NewNotFound(discoveryv1.Resource("endpointslices"), key.Name)
	}
	return h.Client.Get(ctx, key, obj, opts...)
}

func (h *hiding) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := h.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if l, ok := list.(*discoveryv1.EndpointSliceList); ok {
		l.Items = slices.DeleteFunc(l.Items, func(es discoveryv1.EndpointSlice) bool { return h.hidden[es.Name] })
	}
	return nil
}

// A slice that the controller wrote is deleted before another member's is written, also while the cache does not
// show it yet (issue #29).
func TestSwitchWhileCacheLags(t *testing.T) {
	f := &faults{}
	r, c := routedToA(t, f)
	cache := &hiding{Client: c, hidden: map[string]bool{"web-web-a": true}}
	r.Client, r.Reader = cache, cache
	declare(t, r, c, "web-b")
	if err := reconcileWeb(r); err != nil {
		t.Fatal(err)
	}
	if len(f.twice) > 0 {
		t.Fatalf("two members routed %v", f.twice)
	}
	if m := sliceMembers(context.Background(), t, c); !slices.Equal(m, []string{"web-b"}) {
		t.Fatalf("slices of %v", m)
	}
}

// moving is a fresh peer b whose member web-b is a primary in a switchover.
var moving = peers{
	fresh: map[string]bool{"b": true},
	view: map[string]observe.Report{"b": {Declared: "web-b", Members: map[string]observe.Observation{
		"web-b": {Member: "web-b", Site: "b", Role: straitv1.RolePrimary, State: straitv1.StateTransitioning, ObservedAt: now},
	}}},
}

// The previous route is the last decision, not a status that a failed write left behind. A member that the route
// dropped is not held when it turns Unknown, also after a restart (issue #32).
func TestPreviousRouteIsTheLastDecision(t *testing.T) {
	// web-b is routed through the gateway, then moves: the route drops it, but the status write fails.
	setup := func(t *testing.T) (*SiteServiceReconciler, client.Client) {
		t.Helper()
		f := &faults{}
		imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
		imp.Status.GatewayPort = 15001
		c := f.client(t, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-b"), imp)...)
		r := &SiteServiceReconciler{
			Client: c, Reader: c, Recorder: events.NewFakeRecorder(100), Peers: declaring("web-b"), Local: &exchange.Local{},
			Now: func() time.Time { return now },
		}
		run(t, r)
		if st := status(t, c); !slices.Equal(st.Route.Members, []string{"web-b"}) {
			t.Fatalf("before the move: route %+v", st.Route)
		}
		r.Peers = moving
		r.Now = func() time.Time { return now.Add(time.Second) }
		f.armOn("status")
		_ = reconcileWeb(r)
		f.disarm()
		if m := sliceMembers(context.Background(), t, c); len(m) != 0 {
			t.Fatalf("while web-b moves: slices of %v", m)
		}
		if st := status(t, c); !slices.Equal(st.Route.Members, []string{"web-b"}) {
			t.Fatalf("the status write did not fail: route %+v", st.Route)
		}
		return r, c
	}
	// Then site b goes stale, within web-b's hold.
	stale := func(t *testing.T, r *SiteServiceReconciler, c client.Client) {
		t.Helper()
		r.Peers = readB
		r.Now = func() time.Time { return now.Add(2 * time.Second) }
		run(t, r)
		if st := status(t, c); len(st.Route.Members) != 0 || st.Route.Reason != straitv1.ReasonMemberUnknown {
			t.Fatalf("route = %+v, want nowhere, MemberUnknown", st.Route)
		}
		if m := sliceMembers(context.Background(), t, c); len(m) != 0 {
			t.Fatalf("slices of %v", m)
		}
	}

	t.Run("same controller", func(t *testing.T) {
		r, c := setup(t)
		stale(t, r, c)
	})
	t.Run("after a restart", func(t *testing.T) {
		r, c := setup(t)
		restarted := &SiteServiceReconciler{Client: r.Client, Reader: r.Reader, Recorder: r.Recorder, Peers: r.Peers, Local: &exchange.Local{}, Now: r.Now}
		stale(t, restarted, c)
	})
}

// routedToB returns a reconciler at site a whose route is web-b, reached through the gateway.
func routedToB(t *testing.T, f *faults) (*SiteServiceReconciler, client.Client) {
	t.Helper()
	imp := &straitv1.Import{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web-web-b"}, Spec: straitv1.ImportSpec{Site: "b", Export: "web-web-b"}}
	imp.Status.GatewayPort = 15001
	c := f.client(t, append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-b"), imp)...)
	r := &SiteServiceReconciler{
		Client: c, Reader: c, Recorder: events.NewFakeRecorder(100), Peers: declaring("web-b"), Local: &exchange.Local{},
		Now: func() time.Time { return now },
	}
	run(t, r)
	if m := sliceMembers(context.Background(), t, c); !slices.Equal(m, []string{"web-b"}) {
		t.Fatalf("before the switch: slices of %v", m)
	}
	return r, c
}

// An error after the decision still drops the member that the decision dropped. Status shows the route as far as it
// is in place, and the Applied condition gives the error. The next decision comes at the next resync, not after the
// controller's backoff (issue #31).
func TestFailedApplyAfterTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gone is the object deleted before the switch, so the switch must write it.
		gone client.Object
		fail string
	}{
		{"slice", nil, "create *v1.EndpointSlice web-web-a"},
		{"service", &corev1.Service{}, "create *v1.Service web"},
		{"export", &straitv1.Export{}, "create *v1.Export web-web-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &faults{}
			r, c := routedToB(t, f)
			if tc.gone != nil {
				name := map[bool]string{true: "web", false: "web-web-a"}[tc.name == "service"]
				tc.gone.SetNamespace("app")
				tc.gone.SetName(name)
				if err := c.Delete(context.Background(), tc.gone); err != nil {
					t.Fatal(err)
				}
			}
			declare(t, r, c, "web-a")
			f.armOn(tc.fail)
			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web"}})
			f.disarm()
			if !slices.Contains(f.writes, tc.fail) {
				t.Fatalf("no write %q in %v", tc.fail, f.writes)
			}
			if m := sliceMembers(context.Background(), t, c); len(m) != 0 {
				t.Fatalf("slices of %v, want none", m)
			}
			if err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > resync {
				t.Fatalf("result %+v, error %v; want a requeue within %s and no error", res, err, resync)
			}
			st := status(t, c)
			applied := meta.FindStatusCondition(st.Conditions, "Applied")
			if len(st.Route.Members) != 0 || st.Route.Reason != straitv1.ReasonActive || st.Route.Endpoints != 0 ||
				applied == nil || applied.Status != metav1.ConditionFalse || applied.Reason != "SyncFailed" || !strings.Contains(applied.Message, "injected") ||
				meta.IsStatusConditionTrue(st.Conditions, straitv1.ConditionRouted) {
				t.Fatalf("route %+v, applied %+v", st.Route, applied)
			}

			run(t, r)
			st = status(t, c)
			if !slices.Equal(st.Route.Members, []string{"web-a"}) || !meta.IsStatusConditionTrue(st.Conditions, "Applied") ||
				!slices.Equal(sliceMembers(context.Background(), t, c), []string{"web-a"}) {
				t.Fatalf("after a clean reconcile: route %+v, conditions %+v", st.Route, st.Conditions)
			}
		})
	}
}

// A failed write does not delay the decision that ends a hold (issue #31).
func TestFailedWriteKeepsTheHoldTimer(t *testing.T) {
	f := &faults{}
	r, _ := routedToB(t, f)
	r.Peers = readB
	r.Now = func() time.Time { return now.Add(9 * time.Second) }
	f.armOn("status")
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web"}})
	f.disarm()
	if err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > time.Second+50*time.Millisecond {
		t.Fatalf("result %+v, error %v; want a requeue when the hold ends, in 1s, and no error", res, err)
	}
}

// A route change writes the application name's EndpointSlices and the SiteService's status, and nothing else: the
// Service, the Exports and the Imports stay as they are, so the gateway's configuration does not change either
// (RFC#0002, requirement 14).
func TestRouteChangeWritesOnlySlices(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *faults) (*SiteServiceReconciler, client.Client)
		to    string
	}{
		{"local to gateway", routedToA, "web-b"},
		{"gateway to local", routedToB, "web-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &faults{}
			r, c := tc.setup(t, f)
			declare(t, r, c, tc.to)
			f.arm(0)
			if err := reconcileWeb(r); err != nil {
				t.Fatal(err)
			}
			f.disarm()
			if m := sliceMembers(context.Background(), t, c); !slices.Equal(m, []string{tc.to}) {
				t.Fatalf("slices of %v, want [%s]", m, tc.to)
			}
			slicesWritten := 0
			for _, w := range f.writes {
				switch {
				case strings.Contains(w, " *v1.EndpointSlice web-web-"):
					slicesWritten++
				case w == "update status of *v1.SiteService web":
				default:
					t.Errorf("the route change wrote %q; all writes: %v", w, f.writes)
				}
			}
			if slicesWritten == 0 {
				t.Fatalf("the route change wrote no EndpointSlice: %v", f.writes)
			}
		})
	}
}

// lagging is a cache that shows an older version of some EndpointSlices.
type lagging struct {
	client.Client
	old map[string]*discoveryv1.EndpointSlice
}

func (s *lagging) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if es, ok := obj.(*discoveryv1.EndpointSlice); ok && s.old[key.Name] != nil {
		s.old[key.Name].DeepCopyInto(es)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// A slice write right after the controller's own last write of that slice reads it from the API server when the cache
// does not show that write yet, and writes again: the decision is applied in that reconcile (issue #38, item 1).
func TestSliceWriteWhileCacheLags(t *testing.T) {
	t.Run("cache shows no slice", func(t *testing.T) {
		f := &faults{}
		r, c := routedToA(t, f)
		cache := &hiding{Client: c, hidden: map[string]bool{"web-web-a": true}}
		r.Client, r.Reader, r.APIReader = cache, cache, c
		if err := reconcileWeb(r); err != nil {
			t.Fatal(err)
		}
		st := status(t, c)
		if applied := meta.FindStatusCondition(st.Conditions, straitv1.ConditionApplied); applied == nil || applied.Status != metav1.ConditionTrue {
			t.Fatalf("Applied = %+v, want True", applied)
		}
		if !slices.Equal(st.Route.Members, []string{"web-a"}) || st.Route.Endpoints != 1 {
			t.Fatalf("route = %+v", st.Route)
		}
	})
	t.Run("cache shows an older slice", func(t *testing.T) {
		f := &faults{}
		r, c := routedToA(t, f)
		var old discoveryv1.EndpointSlice
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-a"}, &old); err != nil {
			t.Fatal(err)
		}
		// web-a gets a second ready address, and the controller writes it into the slice.
		var eps discoveryv1.EndpointSlice
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-a-x"}, &eps); err != nil {
			t.Fatal(err)
		}
		eps.Endpoints = append(eps.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
		if err := c.Update(context.Background(), &eps); err != nil {
			t.Fatal(err)
		}
		run(t, r)
		// The cache still shows the slice before that write. Then a third address comes.
		cache := &lagging{Client: c, old: map[string]*discoveryv1.EndpointSlice{"web-web-a": &old}}
		r.Client, r.Reader, r.APIReader = cache, cache, c
		eps.Endpoints = append(eps.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
		if err := c.Update(context.Background(), &eps); err != nil {
			t.Fatal(err)
		}
		if err := reconcileWeb(r); err != nil {
			t.Fatal(err)
		}
		st := status(t, c)
		if applied := meta.FindStatusCondition(st.Conditions, straitv1.ConditionApplied); applied == nil || applied.Status != metav1.ConditionTrue {
			t.Fatalf("Applied = %+v, want True", applied)
		}
		if st.Route.Endpoints != 3 {
			t.Fatalf("route = %+v, want 3 endpoints", st.Route)
		}
	})
}

// An EndpointSlice's address type cannot change. When a member's addresses change family, its slice is deleted and
// written again with the new family, so the member keeps its endpoints (issue #38, item 2).
func TestSliceAddressFamilyChange(t *testing.T) {
	f := &faults{}
	r, c := routedToA(t, f)
	var before discoveryv1.EndpointSlice
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-a"}, &before); err != nil {
		t.Fatal(err)
	}
	if before.AddressType != discoveryv1.AddressTypeIPv4 {
		t.Fatalf("before: address type %s", before.AddressType)
	}
	// web-a's pods move to IPv6.
	var eps discoveryv1.EndpointSlice
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-a-x"}, &eps); err != nil {
		t.Fatal(err)
	}
	eps.AddressType = discoveryv1.AddressTypeIPv6
	eps.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"fd00::1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}
	if err := c.Update(context.Background(), &eps); err != nil {
		t.Fatal(err)
	}
	f.arm(0)
	if err := reconcileWeb(r); err != nil {
		t.Fatal(err)
	}
	f.disarm()

	var after discoveryv1.EndpointSlice
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-a"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.AddressType != discoveryv1.AddressTypeIPv6 || !slices.Equal(sliceAddresses(after), []string{"fd00::1"}) {
		t.Fatalf("after: address type %s, addresses %v; want IPv6, [fd00::1]", after.AddressType, sliceAddresses(after))
	}
	// A real API server refuses to change the address type, so the slice must be a new object.
	del := slices.Index(f.writes, "delete *v1.EndpointSlice web-web-a")
	create := slices.Index(f.writes, "create *v1.EndpointSlice web-web-a")
	if del < 0 || create < del || slices.Contains(f.writes, "update *v1.EndpointSlice web-web-a") {
		t.Fatalf("writes %v, want a delete of web-web-a, then a create", f.writes)
	}
	st := status(t, c)
	if applied := meta.FindStatusCondition(st.Conditions, straitv1.ConditionApplied); applied == nil || applied.Status != metav1.ConditionTrue {
		t.Fatalf("Applied = %+v, want True", applied)
	}
	if !slices.Equal(st.Route.Members, []string{"web-a"}) || st.Route.Endpoints != 1 {
		t.Fatalf("route = %+v", st.Route)
	}
}

// The slice of a new address type is written also while the cache still shows the deleted one: one that still has
// an endpoint, or one with none, as a release without this fix leaves it (issue #38, items 1 and 2).
func TestSliceAddressFamilyChangeWhileCacheLags(t *testing.T) {
	for _, tc := range []struct {
		name string
		// empty empties the slice in place before the change, as a release without the fix leaves it.
		empty bool
	}{
		{"old slice with an endpoint", false},
		{"old slice with no endpoint", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &faults{}
			r, c := routedToA(t, f)
			var before discoveryv1.EndpointSlice
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-a"}, &before); err != nil {
				t.Fatal(err)
			}
			if tc.empty {
				before.Endpoints = nil
				if err := c.Update(context.Background(), &before); err != nil {
					t.Fatal(err)
				}
			}
			var eps discoveryv1.EndpointSlice
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-a-x"}, &eps); err != nil {
				t.Fatal(err)
			}
			eps.AddressType = discoveryv1.AddressTypeIPv6
			eps.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"fd00::1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}
			if err := c.Update(context.Background(), &eps); err != nil {
				t.Fatal(err)
			}
			cache := &lagging{Client: c, old: map[string]*discoveryv1.EndpointSlice{"web-web-a": &before}}
			r.Client, r.Reader, r.APIReader = cache, cache, c
			f.arm(0)
			if err := reconcileWeb(r); err != nil {
				t.Fatal(err)
			}
			f.disarm()
			var after discoveryv1.EndpointSlice
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-web-a"}, &after); err != nil {
				t.Fatalf("no slice of web-a after the decision: %v; writes %v", err, f.writes)
			}
			if after.AddressType != discoveryv1.AddressTypeIPv6 || !slices.Equal(sliceAddresses(after), []string{"fd00::1"}) {
				t.Fatalf("after: address type %s, addresses %v; want IPv6, [fd00::1]", after.AddressType, sliceAddresses(after))
			}
			if del, create := slices.Index(f.writes, "delete *v1.EndpointSlice web-web-a"), slices.Index(f.writes, "create *v1.EndpointSlice web-web-a"); del < 0 || create < del {
				t.Fatalf("writes %v, want a delete of web-web-a, then a create", f.writes)
			}
			st := status(t, c)
			if applied := meta.FindStatusCondition(st.Conditions, straitv1.ConditionApplied); applied == nil || applied.Status != metav1.ConditionTrue || st.Route.Endpoints != 1 {
				t.Fatalf("Applied = %+v, route %+v; want True with 1 endpoint", applied, st.Route)
			}
		})
	}
}
