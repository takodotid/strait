package plugin

import (
	"context"
	"slices"
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

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/decide"
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

func siteService(role straitv1.CNPGRole) *straitv1.SiteService {
	mode := straitv1.ModeSingle
	if role != straitv1.CNPGRolePrimary {
		mode = straitv1.ModeSet
	}
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg"},
		Spec: straitv1.SiteServiceSpec{
			Mode:   mode,
			Port:   straitv1.ServicePort{Name: "postgres", Port: 5432},
			Plugin: straitv1.PluginSpec{CNPG: &straitv1.CNPGPlugin{Role: role}},
			Members: []straitv1.Member{
				{Name: "pg-a", Site: "a", CNPG: &straitv1.CNPGMember{Cluster: "cluster-a"}},
				{Name: "pg-b", Site: "b", CNPG: &straitv1.CNPGMember{Cluster: "cluster-b"}},
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
		role     straitv1.CNPGRole
		cluster  *unstructured.Unstructured
		ready    []string
		want     straitv1.Role
		eligible bool
		state    straitv1.State
		declared string
	}{
		{"writable primary", straitv1.CNPGRolePrimary, clusterObject("cluster-a", primarySpec, reported("cluster-a-1", true)), []string{"10.0.0.1"}, straitv1.RolePrimary, true, straitv1.StateStable, "pg-a"},
		{"primary with no ready endpoint", straitv1.CNPGRolePrimary, clusterObject("cluster-a", primarySpec, reported("cluster-a-1", true)), nil, straitv1.RolePrimary, false, straitv1.StateStable, "pg-a"},
		{"replica cluster", straitv1.CNPGRolePrimary, clusterObject("cluster-a", demotedSpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, straitv1.RoleReplica, false, straitv1.StateStable, "pg-b"},
		{"demotion in progress", straitv1.CNPGRolePrimary, clusterObject("cluster-a", demotedSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["switchReplicaClusterStatus"] = map[string]any{"inProgress": true}
			return s
		}()), []string{"10.0.0.1"}, straitv1.RoleReplica, false, straitv1.StateTransitioning, "pg-b"},
		{"demoted spec, still reported primary", straitv1.CNPGRolePrimary, clusterObject("cluster-a", demotedSpec, reported("cluster-a-1", true)), []string{"10.0.0.1"}, straitv1.RoleReplica, false, straitv1.StateTransitioning, "pg-b"},
		{"promotion token not yet used", straitv1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, straitv1.RolePrimary, false, straitv1.StateTransitioning, "pg-a"},
		{"promoted, restarting", straitv1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["lastPromotionToken"] = "t1"
			s["phase"] = "Primary instance is being restarted in-place"
			return s
		}()), []string{"10.0.0.1"}, straitv1.RolePrimary, false, straitv1.StateTransitioning, "pg-a"},
		{"promoted, applying configuration before the restart", straitv1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["lastPromotionToken"] = "t1"
			s["phase"] = "Applying configuration"
			return s
		}()), []string{"10.0.0.1"}, straitv1.RolePrimary, false, straitv1.StateTransitioning, "pg-a"},
		{"promoted", straitv1.CNPGRolePrimary, clusterObject("cluster-a", promotingSpec, func() map[string]any {
			s := reported("cluster-a-1", true)
			s["lastPromotionToken"] = "t1"
			return s
		}()), []string{"10.0.0.1"}, straitv1.RolePrimary, true, straitv1.StateStable, "pg-a"},
		{"spec primary, not yet reported writable", straitv1.CNPGRolePrimary, clusterObject("cluster-a", primarySpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, straitv1.RolePrimary, false, straitv1.StateTransitioning, "pg-a"},
		{"no replica stanza", straitv1.CNPGRolePrimary, clusterObject("cluster-a", map[string]any{}, reported("cluster-a-1", true)), []string{"10.0.0.1"}, straitv1.RolePrimary, true, straitv1.StateStable, "pg-a"},
		{"replica role on a replica cluster", straitv1.CNPGRoleReplica, clusterObject("cluster-a", demotedSpec, reported("cluster-a-1", false)), []string{"10.0.0.1"}, straitv1.RoleReplica, true, straitv1.StateStable, "pg-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			endpoint := "cluster-a-rw"
			if tt.role == straitv1.CNPGRoleReplica {
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
	rep := CNPG{}.Observe(context.Background(), r, siteService(straitv1.CNPGRolePrimary), "a", time.Now())
	if o := rep.Members["pg-a"]; o.Eligible || o.Reason != "ClusterNotFound" {
		t.Fatalf("got %+v", o)
	}
}

// Two local Clusters that name different primaries: the site declares none, and the plugin reports both.
func TestCNPGLocalClustersDisagree(t *testing.T) {
	svc := siteService(straitv1.CNPGRolePrimary)
	svc.Spec.Members = append(svc.Spec.Members, straitv1.Member{Name: "pg-a2", Site: "a", CNPG: &straitv1.CNPGMember{Cluster: "cluster-a2"}})
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	objs := append(service("cluster-a-rw", "10.0.0.1"),
		clusterObject("cluster-a", map[string]any{"replica": map[string]any{"primary": "cluster-a", "source": "cluster-b"}}, reported("cluster-a-1", true)),
		clusterObject("cluster-a2", map[string]any{"replica": map[string]any{"primary": "cluster-b", "source": "cluster-b"}}, reported("cluster-a2-1", false)))
	r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	rep := CNPG{}.Observe(context.Background(), r, svc, "a", time.Now())
	if rep.Declared != "" || !slices.Equal(rep.Conflicting, []string{"pg-a", "pg-b"}) {
		t.Fatalf("declared %q, conflicting %v", rep.Declared, rep.Conflicting)
	}
}

// The declaration names a Cluster by its self name, and maps to the member whose cnpg.cluster has that name. A self
// that differs from the Cluster's name declares a name that is no member, and the site routes nowhere (RFC#0003,
// requirement 7).
func TestCNPGSelf(t *testing.T) {
	tests := []struct {
		name     string
		self     string
		declared string
		reason   string
	}{
		{"self is the Cluster's name", "cluster-a", "pg-a", straitv1.ReasonActive},
		{"self differs from the Cluster's name", "site-a", "site-a", straitv1.ReasonNotDeclared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			spec := map[string]any{"replica": map[string]any{"self": tt.self, "primary": tt.self, "source": "cluster-b"}}
			objs := append(service("cluster-a-rw", "10.0.0.1"), clusterObject("cluster-a", spec, reported("cluster-a-1", true)))
			r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			svc := siteService(straitv1.CNPGRolePrimary)
			rep := CNPG{}.Observe(context.Background(), r, svc, "a", time.Now())
			if o := rep.Members["pg-a"]; !o.Writable() || rep.Declared != tt.declared {
				t.Fatalf("writable %v, declared %q; want writable, declared %q", o.Writable(), rep.Declared, tt.declared)
			}
			got := decide.Decide(decide.Input{
				Spec:         svc.Spec,
				LocalSite:    "a",
				Sites:        map[string]decide.SiteView{"a": {Known: true, Declared: rep.Declared}},
				Observations: rep.Members,
			})
			if got.Reason != tt.reason {
				t.Fatalf("reason %s (%s), want %s", got.Reason, got.Message, tt.reason)
			}
		})
	}
}
