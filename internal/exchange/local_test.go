package exchange

import (
	"reflect"
	"testing"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/observe"
)

func obs(member, site string) observe.Observation {
	return observe.Observation{Member: member, Site: site, Role: v1alpha1.RoleReplica, State: v1alpha1.StateStable, Eligible: true, ObservedAt: at}
}

func TestLocalDocument(t *testing.T) {
	l := &Local{}
	l.SetService("web", "cache", "", []observe.Observation{obs("cache-b", "a"), obs("cache-a", "a")})
	l.SetService("app", "pg", "pg-a", []observe.Observation{obs("pg-a", "a"), obs("pg-b", "b")})
	l.SetService("app", "remote", "", []observe.Observation{obs("remote-b", "b")})
	l.SetService("app", "gone", "", []observe.Observation{obs("gone-a", "a")})
	l.DeleteService("app", "gone")
	l.SetExport("web", "z", 30903, true)
	l.SetExport("app", "y", 30902, false)
	l.SetExport("app", "x", 30901, true)
	l.DeleteExport("app", "x")

	d := l.Document("a", at)
	if d.APIVersion != "strait.tako.id/v1alpha1" || d.Kind != "Observations" || d.Site != "a" || !d.GeneratedAt.Equal(at) {
		t.Fatalf("header: %+v", d)
	}
	var services []string
	for _, s := range d.Services {
		services = append(services, s.Namespace+"/"+s.Name)
		for _, m := range s.Members {
			services = append(services, "  "+m.Name)
		}
	}
	want := []string{"app/pg", "  pg-a", "web/cache", "  cache-a", "  cache-b"}
	if !reflect.DeepEqual(services, want) {
		t.Errorf("services\nwant %v\nhave %v", want, services)
	}
	var exports []string
	for _, e := range d.Exports {
		exports = append(exports, e.Namespace+"/"+e.Name)
	}
	if !reflect.DeepEqual(exports, []string{"app/y", "web/z"}) {
		t.Errorf("exports: %v", exports)
	}
	if !reflect.DeepEqual(d, l.Document("a", at)) {
		t.Errorf("two snapshots differ")
	}
}

func TestLocalCopiesInput(t *testing.T) {
	l := &Local{}
	o := obs("pg-a", "a")
	o.Endpoint.Addresses = []string{"10.0.0.1"}
	members := []observe.Observation{o}
	l.SetService("app", "pg", "pg-a", members)
	members[0].Endpoint.Addresses[0] = "10.0.0.9"
	members[0].State = v1alpha1.StateUnknown
	m := l.Document("a", at).Services[0].Members[0]
	if m.Addresses[0] != "10.0.0.1" || m.State != v1alpha1.StateStable {
		t.Errorf("document follows a change of the input: %+v", m)
	}
}
