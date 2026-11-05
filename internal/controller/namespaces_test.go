package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// counting wraps c and counts the write requests that reach it.
func counting(t *testing.T, c client.Client, n *int) client.Client {
	t.Helper()
	count := func() { *n++ }
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			count()
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			count()
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			count()
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			count()
			return c.Delete(ctx, obj, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			count()
			return c.DeleteAllOf(ctx, obj, opts...)
		},
	})
}

// A write of a Service or an EndpointSlice outside the namespaces sends no request and names the namespace. Every
// other write goes through (RFC0007, requirement 16).
func TestNamespacedWrites(t *testing.T) {
	ctx := context.Background()
	var requests int
	c := NamespacedWrites(counting(t, fakeClient(t), &requests), []string{"db", "strait-system"})
	writes := map[string]func(client.Object) error{
		"create": func(o client.Object) error { return c.Create(ctx, o) },
		"update": func(o client.Object) error { return c.Update(ctx, o) },
		"patch": func(o client.Object) error {
			return c.Patch(ctx, o, client.MergeFrom(o.DeepCopyObject().(client.Object)))
		},
		"delete": func(o client.Object) error { return c.Delete(ctx, o) },
	}
	for _, ns := range []string{"app", ""} {
		for _, o := range []client.Object{
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"}},
			&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"}, AddressType: discoveryv1.AddressTypeIPv4},
		} {
			for verb, write := range writes {
				err := write(o)
				if err == nil || !strings.Contains(err.Error(), `namespace "`+ns+`" is not one`) {
					t.Errorf("%s %T in %q: %v, want a refusal", verb, o, ns, err)
				}
			}
		}
		err := c.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace(ns))
		if err == nil || !strings.Contains(err.Error(), `namespace "`+ns+`" is not one`) {
			t.Errorf("delete all Services in %q: %v, want a refusal", ns, err)
		}
	}
	if requests != 0 {
		t.Fatalf("%d write requests outside the namespaces", requests)
	}
	for _, o := range []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg"}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "strait-system", Name: "gw"}, AddressType: discoveryv1.AddressTypeIPv4},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "other"}},
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Errorf("create %T %s/%s: %v", o, o.GetNamespace(), o.GetName(), err)
		}
	}
	if requests != 3 {
		t.Errorf("%d write requests, want 3", requests)
	}
	if _, wrapped := NamespacedWrites(fakeClient(t), nil).(namespacedWrites); wrapped {
		t.Error("no namespaces wraps the client")
	}
}

// A SiteService outside the namespaces gets no Service and no EndpointSlice, and its Applied condition names the
// namespace (RFC0007, requirement 16).
func TestSiteServiceOutsideNamespaces(t *testing.T) {
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))
	r, c := reconciler(t, readB, objs...)
	var requests int
	r.Client = NamespacedWrites(counting(t, c, &requests), []string{"db", "strait-system"})
	run(t, r)
	st := status(t, c)
	applied := meta.FindStatusCondition(st.Conditions, straitv1.ConditionApplied)
	if applied == nil || applied.Status != metav1.ConditionFalse || applied.Reason != "SyncFailed" || !strings.Contains(applied.Message, `namespace "app" is not one`) {
		t.Errorf("applied = %+v", applied)
	}
	if len(st.Route.Members) != 0 || st.Route.Endpoints != 0 {
		t.Errorf("route = %+v, want no member in place", st.Route)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web"}, &corev1.Service{}); err == nil {
		t.Error("the Service web was written")
	}
	if len(endpointSlices(t, c)) != 0 {
		t.Errorf("slices = %v", endpointSlices(t, c))
	}
}

// An Import with a port outside the namespaces gets no Service and no EndpointSlice, and its Service condition names
// the namespace (RFC0007, requirement 16).
func TestImportOutsideNamespaces(t *testing.T) {
	r, c := importReconciler(t, importSite("10.0.9.9"), webImport(webPort))
	var requests int
	r.Client = NamespacedWrites(counting(t, c, &requests), []string{"db", "strait-system"})
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web-b"}})
	if err == nil || !strings.Contains(err.Error(), `namespace "app" is not one`) {
		t.Errorf("reconcile: %v, want the refusal, so that it is retried", err)
	}
	cond := meta.FindStatusCondition(importStatus(t, c).Conditions, ConditionService)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "SyncFailed" || !strings.Contains(cond.Message, `namespace "app" is not one`) {
		t.Errorf("service condition = %+v", cond)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "web-b"}, &corev1.Service{}); err == nil {
		t.Error("the Service web-b was written")
	}
	if es := importSlice(t, c); es != nil {
		t.Errorf("slice = %+v", es)
	}
}

// A read of a Service, an EndpointSlice or a CloudNativePG object outside the namespaces sends no request, is
// ErrNotRead and names the namespace. Every other read goes through (RFC0007, requirement 26).
func TestNamespacedReads(t *testing.T) {
	ctx := context.Background()
	var reads int
	base := fakeClient(t, append(appService("web-a", "10.0.0.1"), webImport(nil))...)
	counted := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads++
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads++
			return c.List(ctx, list, opts...)
		},
	})
	c := NamespacedReads(counted, []string{"db", "strait-system"})
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGCluster)
	clusters := &unstructured.UnstructuredList{}
	clusters.SetGroupVersionKind(CNPGCluster.GroupVersion().WithKind("ClusterList"))
	key := types.NamespacedName{Namespace: "app", Name: "web-a"}
	refused := map[string]error{
		"get Service":         c.Get(ctx, key, &corev1.Service{}),
		"get EndpointSlice":   c.Get(ctx, key, &discoveryv1.EndpointSlice{}),
		"get Cluster":         c.Get(ctx, key, cluster),
		"list Services":       c.List(ctx, &corev1.ServiceList{}, client.InNamespace("app")),
		"list EndpointSlices": c.List(ctx, &discoveryv1.EndpointSliceList{}, client.InNamespace("app")),
		"list Clusters":       c.List(ctx, clusters, client.InNamespace("app")),
	}
	for what, err := range refused {
		if !errors.Is(err, ErrNotRead) || !strings.Contains(err.Error(), `namespace "app" is not one`) {
			t.Errorf("%s in app: %v, want a refusal", what, err)
		}
	}
	if reads != 0 {
		t.Fatalf("%d read requests outside the namespaces", reads)
	}
	for what, err := range map[string]error{
		"get Import":                c.Get(ctx, types.NamespacedName{Namespace: "app", Name: "web-b"}, &straitv1.Import{}),
		"list Services everywhere":  c.List(ctx, &corev1.ServiceList{}),
		"get Service in db":         client.IgnoreNotFound(c.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg"}, &corev1.Service{})),
		"list EndpointSlices in db": c.List(ctx, &discoveryv1.EndpointSliceList{}, client.InNamespace("db")),
	} {
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	if reads != 4 {
		t.Errorf("%d read requests, want the 4 allowed", reads)
	}
	if NamespacedReads(base, nil) != base {
		t.Error("no namespaces: want the client itself")
	}
}

// An Import without a port, in a namespace where the controller reads no Services, reconciles with no error: the
// controller wrote nothing there to delete. With a port, its Service condition names the namespace.
func TestImportOutsideNamespacedReads(t *testing.T) {
	for _, port := range []*straitv1.ServicePort{nil, webPort} {
		r, c := importReconciler(t, importSite("10.0.9.9"), webImport(port))
		ns := []string{"db", "strait-system"}
		r.Client = NamespacedWrites(NamespacedReads(c, ns), ns)
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web-b"}})
		cond := meta.FindStatusCondition(importStatus(t, c).Conditions, ConditionService)
		if port == nil {
			if err != nil || cond != nil {
				t.Errorf("no port: reconcile %v, service condition %+v; want neither", err, cond)
			}
			continue
		}
		if !errors.Is(err, ErrNotRead) {
			t.Errorf("port: reconcile %v, want the refusal, so that it is retried", err)
		}
		if cond == nil || cond.Reason != "SyncFailed" || !strings.Contains(cond.Message, `namespace "app" is not one where the controller reads`) {
			t.Errorf("port: service condition %+v", cond)
		}
	}
}

// An Export in a namespace where the controller reads no Services has no ready endpoint, and says why.
func TestExportOutsideNamespacedReads(t *testing.T) {
	r, _, c := laggingExport(t)
	ns := []string{"db", "strait-system"}
	r.Client = NamespacedReads(c, ns)
	r.Reader = r.Client
	runExport(t, r)
	ready := meta.FindStatusCondition(exportStatus(t, c).Conditions, ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReadFailed" || !strings.Contains(ready.Message, `namespace "app" is not one`) {
		t.Errorf("ready = %+v", ready)
	}
}
