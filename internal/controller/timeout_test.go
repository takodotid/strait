package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/exchange"
)

// A reconcile whose time runs out while a write hangs still writes status, with the failure in the Applied condition
// (RFC0002, requirement 39). The API server here refuses a request whose context has ended, as a real one does, and
// the write of the Service hangs until the reconcile's context ends.
func TestStatusWrittenWhenTheReconcileTimesOut(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := append(appService("web-a", "10.0.0.1"), site("a", "10.0.9.9"), webService("web-a"))
	hang := func(ctx context.Context, obj client.Object) error {
		if s, ok := obj.(*corev1.Service); ok && s.Name == "web" {
			<-ctx.Done()
		}
		return ctx.Err()
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&straitv1.SiteService{}, &straitv1.Export{}, &straitv1.Import{}, &straitv1.Site{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := hang(ctx, obj); err != nil {
					return err
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := hang(ctx, obj); err != nil {
					return err
				}
				return c.Update(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &SiteServiceReconciler{
		Client: c, Reader: c, Recorder: events.NewFakeRecorder(100), Peers: readB, Local: &exchange.Local{},
		Now: func() time.Time { return now },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web"}}); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	st := status(t, c)
	applied := meta.FindStatusCondition(st.Conditions, straitv1.ConditionApplied)
	if applied == nil || applied.Status != "False" || !strings.Contains(applied.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("Applied = %+v, want False with the deadline; status %+v", applied, st)
	}
	if st.Route.Reason != straitv1.ReasonActive || st.Route.Endpoints != 0 {
		t.Errorf("route = %+v, want the decision, with no endpoint in place", st.Route)
	}
}
