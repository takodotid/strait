package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// The elected controller writes its version into the Site's status, and a new version replaces the old one.
func TestGatewayControllerVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}}).
		WithStatusSubresource(&straitv1.Site{}).Build()
	r := &GatewayReconciler{Client: c, Reader: c, Namespace: "strait-system"}

	for _, v := range []string{"v1.0.0", "v1.0.1"} {
		r.Version = v
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatal(err)
		}
		var site straitv1.Site
		if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &site); err != nil {
			t.Fatal(err)
		}
		if site.Status.ControllerVersion != v {
			t.Errorf("controllerVersion: got %q, want %q", site.Status.ControllerVersion, v)
		}
	}
}
