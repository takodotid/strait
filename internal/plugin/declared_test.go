package plugin

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	straitv1 "github.com/takodotid/strait/api/v1"
)

func declaredService() *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "web"},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "postgres", Port: 5432},
			Plugin: straitv1.PluginSpec{Declared: &straitv1.DeclaredPlugin{Active: "web-a"}},
			Members: []straitv1.Member{
				{Name: "web-a", Site: "a", Service: &straitv1.ServicePortRef{Name: "web-a", Port: 5432}},
				{Name: "web-b", Site: "b", Service: &straitv1.ServicePortRef{Name: "web-b", Port: 5432}},
			},
		},
	}
}

func readerOf(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// A member is eligible when its Service has a ready address, and Stable unless the Service cannot be read
// (RFC#0003, requirement 12).
func TestDeclared(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	otherPort := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "web-a"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
	tests := []struct {
		name     string
		objs     []client.Object
		eligible bool
		reason   string
	}{
		{"ready", service("web-a", "10.0.0.1"), true, "Declared"},
		{"no ready address", service("web-a"), false, "NoReadyEndpoint"},
		{"no Service", nil, false, "ServiceNotFound"},
		{"no Service port of that number", []client.Object{otherPort}, false, "PortNotFound"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := Declared{}.Observe(context.Background(), readerOf(t, tt.objs...), declaredService(), "a", now)
			o := rep.Members["web-a"]
			if o.State != straitv1.StateStable || o.Role != straitv1.RolePrimary || o.Eligible != tt.eligible || o.Reason != tt.reason {
				t.Fatalf("got state=%s role=%s eligible=%v reason=%q; want state=Stable role=primary eligible=%v reason=%q",
					o.State, o.Role, o.Eligible, o.Reason, tt.eligible, tt.reason)
			}
			if rep.Declared != "web-a" {
				t.Fatalf("declared %q", rep.Declared)
			}
			if _, ok := rep.Members["web-b"]; ok {
				t.Fatal("observed a member of another site")
			}
		})
	}
}
