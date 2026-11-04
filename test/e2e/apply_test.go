//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// localService is the SiteService local at site a. Its two members, one and two, are both site a's web server.
func localService(e *env, active string) *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: webNamespace},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "http", Port: 8080},
			Plugin: straitv1.PluginSpec{Declared: &straitv1.DeclaredPlugin{Active: active}},
			Members: []straitv1.Member{
				{Name: "one", Site: e.a.name, Service: &straitv1.ServicePortRef{Name: webOf(e.a), Port: 8080}},
				{Name: "two", Site: e.a.name, Service: &straitv1.ServicePortRef{Name: webOf(e.a), Port: 8080}},
			},
		},
	}
}

// localSlices returns the members that have an EndpointSlice of the SiteService local at site s, sorted.
func localSlices(ctx context.Context, s *site) ([]string, error) {
	var list discoveryv1.EndpointSliceList
	if err := s.c.List(ctx, &list, client.InNamespace(webNamespace), client.MatchingLabels{"strait.tako.id/siteservice": "local"}); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, es := range list.Items {
		seen[es.Labels["strait.tako.id/member"]] = true
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// distinct returns the distinct values of m, sorted.
func distinct(m map[string]string) []string {
	seen := map[string]bool{}
	for _, v := range m {
		seen[v] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// switchThroughNowhere: the SiteService local moves from one to two in one decision, at site a. A watch of its
// EndpointSlices sees every change in order, and never sees both members at once (issue #29).
func (e *env) switchThroughNowhere(ctx context.Context, t *testing.T) {
	a := e.a
	if err := a.c.Create(ctx, localService(e, "one")); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, time.Minute, 250*time.Millisecond, "site a routes local to one", routeIs(a, webNamespace, "local", []string{"one"}, straitv1.ReasonActive))

	wc, err := a.watcher()
	if err != nil {
		t.Fatal(err)
	}
	w, err := wc.Watch(ctx, &discoveryv1.EndpointSliceList{}, client.InNamespace(webNamespace), client.MatchingLabels{"strait.tako.id/siteservice": "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	ss, err := siteService(ctx, a, webNamespace, "local")
	if err != nil {
		t.Fatal(err)
	}
	base := ss.DeepCopy()
	ss.Spec.Plugin.Declared.Active = "two"
	start := time.Now()
	if err := a.c.Patch(ctx, ss, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	// The watch begins with the slices in place, then gives each change in order.
	members := map[string]string{}
	var seen []string
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				t.Fatalf("the watch ended; changes seen: %v", seen)
			}
			es, ok := ev.Object.(*discoveryv1.EndpointSlice)
			if !ok {
				t.Fatalf("watch: %s %T", ev.Type, ev.Object)
			}
			if ev.Type == watch.Deleted {
				delete(members, es.Name)
			} else {
				members[es.Name] = es.Labels["strait.tako.id/member"]
			}
			routed := distinct(members)
			seen = append(seen, fmt.Sprintf("%s %s: %v", ev.Type, es.Name, routed))
			if len(routed) > 1 {
				t.Fatalf("site a: local has EndpointSlices of %v at once; changes: %v", routed, seen)
			}
			if slices.Equal(routed, []string{"two"}) {
				e.times.record(t, "switch/two-written", time.Since(start))
				t.Logf("changes: %v", seen)
				eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "site a routes local to two", routeIs(a, webNamespace, "local", []string{"two"}, straitv1.ReasonActive))
				return
			}
		case <-deadline:
			t.Fatalf("site a: local's EndpointSlices are not two's only after 30s; changes: %v", seen)
		}
	}
}

// serviceConflict: Strait's Service local at site a loses its owner, as when another owner creates a Service of that
// name. Strait deletes local's EndpointSlices, so none feeds that Service. Once it is gone, Strait writes its own again.
// The step then deletes local (issue #33).
func (e *env) serviceConflict(ctx context.Context, t *testing.T) {
	a := e.a
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: webNamespace}}
	if err := a.c.Patch(ctx, svc, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"ownerReferences":null}}`))); err != nil {
		t.Fatal(err)
	}
	d := eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "site a deletes local's EndpointSlices", func(ctx context.Context) (bool, string) {
		got, err := localSlices(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		ss, err := siteService(ctx, a, webNamespace, "local")
		if err != nil {
			return false, err.Error()
		}
		routed := meta.FindStatusCondition(ss.Status.Conditions, straitv1.ConditionRouted)
		return len(got) == 0 && routed != nil && routed.Reason == "ServiceConflict", fmt.Sprintf("slices of %v; %s", got, describe(ss))
	})
	e.times.record(t, "conflict/slices-deleted", d)

	if err := a.c.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "site a writes local's Service and EndpointSlice again", func(ctx context.Context) (bool, string) {
		got, err := localSlices(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		ss, err := siteService(ctx, a, webNamespace, "local")
		if err != nil {
			return false, err.Error()
		}
		return slices.Equal(got, []string{"two"}) && conditionTrue(ss, straitv1.ConditionRouted), fmt.Sprintf("slices of %v; %s", got, describe(ss))
	})
	if err := a.c.Delete(ctx, localService(e, "")); err != nil {
		t.Fatal(err)
	}
}
