package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/controller"
)

// apiServer starts envtest's API server with Strait's CRDs. make test sets KUBEBUILDER_ASSETS. Each flag is a pair of
// the API server's flag and its value.
func apiServer(t *testing.T, flags ...[2]string) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set; CI must run this test with make test")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set; make test sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd"}, ErrorIfCRDPathMissing: true}
	for _, f := range flags {
		env.ControlPlane.GetAPIServer().Configure().Set(f[0], f[1])
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop the API server: %v", err)
		}
	})
	cfg.QPS, cfg.Burst = 2000, 4000
	return cfg
}

func testScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// startCache starts the cache of the manager's options, with informers for the Services and the EndpointSlices of
// the cluster, and waits until they have synced.
func startCache(t *testing.T, ctx context.Context, cfg *rest.Config, scheme *k8sruntime.Scheme, opts cache.Options) cache.Cache {
	t.Helper()
	opts.Scheme = scheme
	c, err := cache.New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{&corev1.Service{}, &discoveryv1.EndpointSlice{}} {
		if _, err := c.GetInformer(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	go func() { _ = c.Start(ctx) }()
	if !c.WaitForCacheSync(ctx) {
		t.Fatal("the cache did not sync")
	}
	return c
}

// TestCacheStripsManagedFields: the controller's cache holds no managedFields (issue #46, row 19). A write from a
// copy read from that cache leaves the API server's managedFields in place.
func TestCacheStripsManagedFields(t *testing.T) {
	cfg := apiServer(t)
	scheme := testScheme(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}}); err != nil {
		t.Fatal(err)
	}
	svc := service("app", "web", 0)
	if err := direct.Create(ctx, svc, client.FieldOwner("someone")); err != nil {
		t.Fatal(err)
	}

	opts := managerOptions(scheme, &memoryLock{id: "a"}, "strait-system", "0", "0", false)
	c := startCache(t, ctx, cfg, scheme, opts.Cache)
	var cached corev1.Service
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), &cached); err != nil {
		t.Fatal(err)
	}
	if n := len(cached.ManagedFields); n != 0 {
		t.Fatalf("the cache holds %d managedFields entries of a Service, want none", n)
	}

	// The controller writes through a client that reads from that cache, as controllerutil.CreateOrUpdate does.
	writer, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: c}})
	if err != nil {
		t.Fatal(err)
	}
	obj := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}}
	if _, err := controllerutil.CreateOrUpdate(ctx, client.WithFieldOwner(writer, "strait"), obj, func() error {
		obj.Labels["written"] = "yes"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var live corev1.Service
	if err := direct.Get(ctx, client.ObjectKeyFromObject(svc), &live); err != nil {
		t.Fatal(err)
	}
	managers := map[string]bool{}
	for _, m := range live.ManagedFields {
		managers[m.Manager] = true
	}
	if live.Labels["written"] != "yes" || !managers["someone"] || !managers["strait"] {
		t.Errorf("after a write from the cache's copy: labels %v, managers %v; want the label, someone and strait", live.Labels, managers)
	}
}

// TestCacheMemory measures the heap that the controller's cache takes for N Services and N EndpointSlices, with and
// without the transform that strips managedFields. It runs only when STRAIT_CACHE_MEMORY is N:
//
//	STRAIT_CACHE_MEMORY=10000 make test
//
// The objects look like those that a Helm chart and the EndpointSlice controller write: four labels and two
// annotations on each, two ports, three ready endpoints with a pod reference, and one managedFields entry.
func TestCacheMemory(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("STRAIT_CACHE_MEMORY"))
	if n <= 0 {
		t.Skip("STRAIT_CACHE_MEMORY is not set")
	}
	// Room for a cluster IP per Service.
	cfg := apiServer(t, [2]string{"service-cluster-ip-range", "10.96.0.0/16"})
	scheme := testScheme(t)
	ctx := context.Background()
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const namespaces = 20
	for i := range namespaces {
		if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ns-%d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	work := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	for range 32 {
		wg.Go(func() {
			for i := range work {
				ns := fmt.Sprintf("ns-%d", i%namespaces)
				svc := service(ns, fmt.Sprintf("svc-%d", i), i)
				err := direct.Create(ctx, svc, client.FieldOwner("helm"))
				if err == nil {
					err = direct.Create(ctx, endpointSlice(svc, i), client.FieldOwner("endpointslice-controller.k8s.io"))
				}
				if err != nil {
					mu.Lock()
					failed = err
					mu.Unlock()
				}
			}
		})
	}
	for i := range n {
		work <- i
	}
	close(work)
	wg.Wait()
	if failed != nil {
		t.Fatal(failed)
	}
	t.Logf("created %d Services and %d EndpointSlices in %s", n, n, time.Since(start).Round(time.Second))

	opts := managerOptions(scheme, &memoryLock{id: "a"}, "strait-system", "0", "0", false).Cache
	before := opts
	before.DefaultTransform = nil
	scoped := opts
	scoped.ByObject = maps.Clone(opts.ByObject)
	readsIn(&scoped, []string{"ns-0", "strait-system"}, false)
	for _, c := range []struct {
		name string
		opts cache.Options
	}{
		{"1.2.x (managedFields kept)", before},
		{"1.3.0 (managedFields stripped)", opts},
		{"1.3.0 with namespacedReads, 1 namespace of 20", scoped},
		{"1.2.x again", before},
	} {
		heap := func() uint64 {
			var m runtime.MemStats
			for range 3 {
				runtime.GC()
			}
			runtime.ReadMemStats(&m)
			return m.HeapAlloc
		}
		base := heap()
		cctx, cancel := context.WithCancel(ctx)
		cc := startCache(t, cctx, cfg, scheme, c.opts)
		var services corev1.ServiceList
		if err := cc.List(cctx, &services); err != nil || len(services.Items) == 0 {
			t.Fatalf("listed %d Services, %v", len(services.Items), err)
		}
		listed := len(services.Items)
		services = corev1.ServiceList{}
		used := heap() - base
		t.Logf("%s: %d Services cached, %d MiB for the cluster's %d Services and %d EndpointSlices, %d bytes per Service and EndpointSlice of the cluster",
			c.name, listed, used>>20, n, n, used/uint64(n))
		cancel()
		time.Sleep(time.Second)
	}
}

// service is a Service as a Helm chart writes it.
func service(namespace, name string, i int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/name": name, "app.kubernetes.io/instance": "release-" + strconv.Itoa(i%50),
				"app.kubernetes.io/managed-by": "Helm", "helm.sh/chart": "chart-1.2.3",
			},
			Annotations: map[string]string{"meta.helm.sh/release-name": "release-" + strconv.Itoa(i%50), "meta.helm.sh/release-namespace": namespace},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/instance": "release-" + strconv.Itoa(i%50)},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
				{Name: "metrics", Port: 9090, TargetPort: intstr.FromInt32(9090), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// endpointSlice is the EndpointSlice that the EndpointSlice controller writes for a Service with three ready pods.
func endpointSlice(svc *corev1.Service, i int) *discoveryv1.EndpointSlice {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: svc.Namespace, Name: svc.Name + "-" + fmt.Sprintf("%05x", i),
			Labels: map[string]string{
				discoveryv1.LabelServiceName: svc.Name, discoveryv1.LabelManagedBy: "endpointslice-controller.k8s.io",
				"app.kubernetes.io/name": svc.Name, "app.kubernetes.io/managed-by": "Helm",
			},
			Annotations: map[string]string{"endpoints.kubernetes.io/last-change-trigger-time": "2026-11-05T10:00:00Z"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: svc.Name,
				UID: types.UID(fmt.Sprintf("00000000-0000-0000-0000-%012d", i)), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{
			{Name: ptr.To("http"), Port: ptr.To[int32](8080), Protocol: ptr.To(corev1.ProtocolTCP)},
			{Name: ptr.To("metrics"), Port: ptr.To[int32](9090), Protocol: ptr.To(corev1.ProtocolTCP)},
		},
	}
	for p := range 3 {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{fmt.Sprintf("10.%d.%d.%d", 100+p, i/250%250, i%250+1)},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true), Serving: ptr.To(true), Terminating: ptr.To(false)},
			NodeName:   ptr.To(fmt.Sprintf("node-%d", (i+p)%40)),
			Zone:       ptr.To(fmt.Sprintf("zone-%d", p)),
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: svc.Namespace, Name: fmt.Sprintf("%s-7d9f8b6c5-%05x", svc.Name, i*3+p),
				UID: types.UID(fmt.Sprintf("00000000-0000-0000-%04d-%012d", p, i))},
		})
	}
	return es
}

// TestCacheHoldsReadNamespacesOnly: with --namespaced-reads, the cache holds the Services and EndpointSlices of the
// namespaces only, and a read elsewhere names the namespace (RFC0007, requirement 26).
func TestCacheHoldsReadNamespacesOnly(t *testing.T) {
	cfg := apiServer(t)
	scheme := testScheme(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"db", "app"} {
		if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
		svc := service(ns, "pg", 0)
		if err := direct.Create(ctx, svc); err != nil {
			t.Fatal(err)
		}
		if err := direct.Create(ctx, endpointSlice(svc, 0)); err != nil {
			t.Fatal(err)
		}
	}
	reads := []string{"db", "strait-system"}
	opts := managerOptions(scheme, &memoryLock{id: "a"}, "strait-system", "0", "0", false)
	readsIn(&opts.Cache, reads, false)
	c := startCache(t, ctx, cfg, scheme, opts.Cache)
	var services corev1.ServiceList
	if err := c.List(ctx, &services); err != nil {
		t.Fatal(err)
	}
	var slices discoveryv1.EndpointSliceList
	if err := c.List(ctx, &slices); err != nil {
		t.Fatal(err)
	}
	for _, o := range append(services.Items, corev1.Service{ObjectMeta: slices.Items[0].ObjectMeta}) {
		if o.Namespace != "db" {
			t.Errorf("the cache holds %s/%s", o.Namespace, o.Name)
		}
	}
	if len(services.Items) != 1 || len(slices.Items) != 1 {
		t.Errorf("the cache holds %d Services and %d EndpointSlices, want 1 of each", len(services.Items), len(slices.Items))
	}
	r := controller.NamespacedReads(&cacheClient{Reader: c}, reads)
	if err := r.Get(ctx, types.NamespacedName{Namespace: "app", Name: "pg"}, &corev1.Service{}); !errors.Is(err, controller.ErrNotRead) {
		t.Errorf("a read in app: %v, want ErrNotRead", err)
	}
}

// cacheClient is a client whose reads go to a cache. It writes nothing.
type cacheClient struct {
	client.Reader
	client.Client
}

func (c *cacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *cacheClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.Reader.List(ctx, list, opts...)
}

// readsIn adds the CloudNativePG kinds only with the cnpg plugin on.
func TestReadsIn(t *testing.T) {
	for _, cnpg := range []bool{false, true} {
		opts := cache.Options{}
		readsIn(&opts, []string{"db", "strait-system"}, cnpg)
		kinds := map[string]bool{}
		for o, by := range opts.ByObject {
			if len(by.Namespaces) != 2 {
				t.Errorf("%T: namespaces %v", o, by.Namespaces)
			}
			if u, ok := o.(*unstructured.Unstructured); ok {
				kinds[u.GetKind()] = true
			} else {
				kinds[fmt.Sprintf("%T", o)] = true
			}
		}
		want := map[string]bool{"*v1.Service": true, "*v1.EndpointSlice": true}
		if cnpg {
			want["Cluster"], want["Pooler"] = true, true
		}
		if !maps.Equal(kinds, want) {
			t.Errorf("cnpg %v: kinds %v, want %v", cnpg, kinds, want)
		}
	}
	opts := cache.Options{}
	readsIn(&opts, nil, true)
	if opts.ByObject != nil {
		t.Errorf("no namespaces: %v", opts.ByObject)
	}
}

func TestMemoryLimit(t *testing.T) {
	for _, c := range []struct {
		limit, gomemlimit string
		want              int64
		ok                bool
	}{
		{"268435456", "", 241591911, true},
		{"268435456", "200MiB", 0, false},
		{"", "", 0, false},
		{"256Mi", "", 0, false},
		{"0", "", 0, false},
	} {
		got, ok := memoryLimit(c.limit, c.gomemlimit)
		if got != c.want || ok != c.ok {
			t.Errorf("memoryLimit(%q, %q) = %d, %v; want %d, %v", c.limit, c.gomemlimit, got, ok, c.want, c.ok)
		}
	}
}
