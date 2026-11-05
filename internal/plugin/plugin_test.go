package plugin

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ReadOnly hides every write method of the client behind it (RFC#0003, requirement 4).
func TestReadOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "web"}},
	).Build()
	var v any = ReadOnly(c)
	if _, ok := v.(client.Writer); ok {
		t.Error("the reader can write")
	}
	if _, ok := v.(client.StatusClient); ok {
		t.Error("the reader can write status")
	}
	if _, ok := v.(client.SubResourceClientConstructor); ok {
		t.Error("the reader can write a subresource")
	}
	r := v.(client.Reader)
	var svc corev1.Service
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "db", Name: "web"}, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	var list corev1.ServiceList
	if err := r.List(context.Background(), &list, client.InNamespace("db")); err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %v, %d items", err, len(list.Items))
	}
}
