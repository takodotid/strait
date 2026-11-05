package exchange

import (
	"reflect"
	"testing"
	"time"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/observe"
)

func obs(member, site string) observe.Observation {
	return observe.Observation{Member: member, Site: site, Role: straitv1.RoleReplica, State: straitv1.StateStable, Eligible: true, ObservedAt: at}
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

	d := l.Document("a", at, 0)
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
	if !reflect.DeepEqual(d, l.Document("a", at, 0)) {
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
	members[0].State = straitv1.StateUnknown
	m := l.Document("a", at, 0).Services[0].Members[0]
	if m.Addresses[0] != "10.0.0.1" || m.State != straitv1.StateStable {
		t.Errorf("document follows a change of the input: %+v", m)
	}
}

// A member whose last observation is older than the publisher's peerTimeout is published as Unknown, with no address,
// so that a site whose reconciles hang does not keep an old observation fresh at its peers (RFC0004, requirement 15).
func TestDocumentAgesObservations(t *testing.T) {
	maxAge := 6 * time.Second
	l := &Local{}
	old := obs("pg-a", "a")
	old.Role, old.Reason = straitv1.RolePrimary, "ClusterPrimary"
	old.Endpoint = observe.Endpoint{Addresses: []string{"10.0.0.1"}, Port: 5432}
	fresh := obs("pg-c", "a")
	fresh.ObservedAt = at.Add(5 * time.Second)
	l.SetService("app", "pg", "pg-a", []observe.Observation{old, fresh})

	member := func(d Document, name string) Member {
		t.Helper()
		for _, m := range d.Services[0].Members {
			if m.Name == name {
				return m
			}
		}
		t.Fatalf("no member %s in %+v", name, d)
		return Member{}
	}

	// At the limit, the observation is still published as it was.
	d := l.Document("a", at.Add(maxAge), maxAge)
	if m := member(d, "pg-a"); m.State != straitv1.StateStable || !m.Eligible || m.Role != straitv1.RolePrimary {
		t.Errorf("at peerTimeout: %+v, want the observation as it was", m)
	}

	d = l.Document("a", at.Add(maxAge+time.Millisecond), maxAge)
	want := Member{
		Name: "pg-a", Role: straitv1.RoleUnknown, State: straitv1.StateUnknown, Reason: ReasonObservationStale,
		ObservedAt: at, Addresses: []string{}, Port: 0,
	}
	if m := member(d, "pg-a"); !reflect.DeepEqual(m, want) {
		t.Errorf("past peerTimeout:\nwant %+v\nhave %+v", want, m)
	}
	if m := member(d, "pg-c"); m.State != straitv1.StateStable {
		t.Errorf("a member observed within peerTimeout is aged: %+v", m)
	}
	if got := d.Services[0].DeclaredPrimary; got != "pg-a" {
		t.Errorf("declaredPrimary %q, want the last one observed, pg-a", got)
	}

	// A peerTimeout of 0s ages nothing.
	if m := member(l.Document("a", at.Add(time.Hour), 0), "pg-a"); m.State != straitv1.StateStable {
		t.Errorf("with no peerTimeout: %+v, want the observation as it was", m)
	}

	// A peerTimeout under 6s ages a member after 6s only: a healthy site observes each member at least every 2s, and
	// every SiteService shares one worker.
	if m := member(l.Document("a", at.Add(MinAgeLimit), 2*time.Second), "pg-a"); m.State != straitv1.StateStable {
		t.Errorf("peerTimeout 2s, 6s after the observation: %+v, want the observation as it was", m)
	}
	if m := member(l.Document("a", at.Add(MinAgeLimit+time.Millisecond), 2*time.Second), "pg-a"); m.State != straitv1.StateUnknown {
		t.Errorf("peerTimeout 2s, past 6s after the observation: %+v, want Unknown", m)
	}
}
