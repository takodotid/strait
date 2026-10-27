package plugin

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/takodotid/strait/api/v1alpha1"
)

// cnpgCluster builds a Cluster as CloudNativePG reports it.
func clusterObject(name string, spec, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec, "status": status}}
	u.SetGroupVersionKind(cnpgCluster)
	u.SetNamespace("db")
	u.SetName(name)
	return u
}

func service(name string, addresses ...string) []client.Object {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: name},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "postgres", Port: 5432}}},
	}
	es := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "db", Name: name + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: name}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("postgres"), Port: ptr.To[int32](5432)}},
	}
	for _, a := range addresses {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{a}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	}
	return []client.Object{svc, es}
}

func reported(primary string, isPrimary bool) map[string]any {
	return map[string]any{
		"phase":                  "Cluster in healthy state",
		"currentPrimary":         primary,
		"targetPrimary":          primary,
		"instancesReportedState": map[string]any{primary: map[string]any{"isPrimary": isPrimary}},
	}
}

func siteService(role v1alpha1.CNPGRole) *v1alpha1.SiteService {
	mode := v1alpha1.ModeSingle
	if role != v1alpha1.CNPGRolePrimary {
		mode = v1alpha1.ModeSet
	}
	return &v1alpha1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg"},
		Spec: v1alpha1.SiteServiceSpec{
			Mode:   mode,
			Port:   v1alpha1.ServicePort{Name: "postgres", Port: 5432},
			Plugin: v1alpha1.PluginSpec{CNPG: &v1alpha1.CNPGPlugin{Role: role}},
			Members: []v1alpha1.Member{
				{Name: "pg-a", Site: "a", CNPG: &v1alpha1.CNPGMember{Cluster: "cluster-a"}},
				{Name: "pg-b", Site: "b", CNPG: &v1alpha1.CNPGMember{Cluster: "cluster-b"}},
			},
		},
	}
}

func TestCNPG(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	primarySpec := map[string]any{"replica": map[string]any{"primary": "cluster-a", "source": "cluster-b"}}
	demotedSpec := map[string]any{"replica": map[string]any{"primary": "cluster-b", "source": "cluster-b"}}
	promotingSpec := map[string]any{"replica": map[string]any{"primary": "cluster-a", "source": "cluster-b", "promotionToken": "t1"}}
	tests := []struct {
		name     string
		role     v1alpha1.CNPGRole
		cluster  *unstructured.Unstructured
		ready    []string
		want     v1alpha1.Role
		eligible bool
		state    v1alpha1.State
		declared string
	}{
		{"writable primary", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", primarySpec, reported("cluster-a-1", true)), []string{"10.0.0.1"}, v1alpha1.RolePrimary, true, v1alpha1.StateStable, "pg-a"},
		{"primary with no ready endpoint", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", primarySpec, reported("cluster-a-1", true)), nil, v1alpha1.RolePrimary, false, v1alpha1.StateStable, "pg-a"},
		{"replica cluster", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", demotedSpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, v1alpha1.RoleReplica, false, v1alpha1.StateStable, "pg-b"},
		{"demotion in progress", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", demotedSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["switchReplicaClusterStatus"] = map[string]any{"inProgress": true}
			return s
		}()), []string{"10.0.0.1"}, v1alpha1.RoleReplica, false, v1alpha1.StateTransitioning, "pg-b"},
		{"demoted spec, still reported primary", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", demotedSpec, reported("cluster-a-1", true)), []string{"10.0.0.1"}, v1alpha1.RoleReplica, false, v1alpha1.StateTransitioning, "pg-b"},
		{"promotion token not yet used", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, v1alpha1.RolePrimary, false, v1alpha1.StateTransitioning, "pg-a"},
		{"promoted, restarting", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["lastPromotionToken"] = "t1"
			s["phase"] = "Primary instance is being restarted in-place"
			return s
		}()), []string{"10.0.0.1"}, v1alpha1.RolePrimary, false, v1alpha1.StateTransitioning, "pg-a"},
		{"promoted, applying configuration before the restart", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["lastPromotionToken"] = "t1"
			s["phase"] = "Applying configuration"
			return s
		}()), []string{"10.0.0.1"}, v1alpha1.RolePrimary, false, v1alpha1.StateTransitioning, "pg-a"},
		{"promoted", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["lastPromotionToken"] = "t1"
			return s
		}()), []string{"10.0.0.1"}, v1alpha1.RolePrimary, true, v1alpha1.StateStable, "pg-a"},
		{"spec primary, not yet reported writable", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", primarySpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, v1alpha1.RolePrimary, false, v1alpha1.StateTransitioning, "pg-a"},
		{"no replica stanza", v1alpha1.CNPGRolePrimary, clusterObject("cluster-a", map[string]any{}, reported("cluster-a-1", true)), []string{"10.0.0.1"}, v1alpha1.RolePrimary, true, v1alpha1.StateStable, "pg-a"},
		{"replica role on a replica cluster", v1alpha1.CNPGRoleReplica, clusterObject("cluster-a", demotedSpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, v1alpha1.RoleReplica, true, v1alpha1.StateStable, "pg-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			endpoint := "cluster-a-rw"
			if tt.role == v1alpha1.CNPGRoleReplica {
				endpoint = "cluster-a-r"
			}
			objs := append(service(endpoint, tt.ready...), tt.cluster)
			r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			rep := CNPG{}.Observe(context.Background(), r, siteService(tt.role), "a", now)
			o := rep.Members["pg-a"]
			if o.Role != tt.want || o.Eligible != tt.eligible || o.State != tt.state || rep.Declared != tt.declared {
				t.Fatalf("got role=%s eligible=%v state=%s declared=%q reason=%q; want role=%s eligible=%v state=%s declared=%q",
					o.Role, o.Eligible, o.State, rep.Declared, o.Reason, tt.want, tt.eligible, tt.state, tt.declared)
			}
			if _, ok := rep.Members["pg-b"]; ok {
				t.Fatal("observed a member of another site")
			}
		})
	}
}

func TestCNPGClusterMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	r := fake.NewClientBuilder().WithScheme(scheme).Build()
	rep := CNPG{}.Observe(context.Background(), r, siteService(v1alpha1.CNPGRolePrimary), "a", time.Now())
	if o := rep.Members["pg-a"]; o.Eligible || o.Reason != "ClusterNotFound" {
		t.Fatalf("got %+v", o)
	}
}
