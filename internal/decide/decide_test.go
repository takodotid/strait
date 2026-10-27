package decide_test

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/takodotid/strait/api/v1alpha1"
	"github.com/takodotid/strait/internal/decide"
	"github.com/takodotid/strait/internal/observe"
)

var now = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

const hold = 30 * time.Second

func ago(d time.Duration) time.Time { return now.Add(-d) }

// pgSpec is a Single SiteService with one member pg-<site> per site.
func pgSpec(multiple v1alpha1.MultiplePolicy, unknownHold time.Duration, sites ...string) v1alpha1.SiteServiceSpec {
	s := v1alpha1.SiteServiceSpec{
		Mode:   v1alpha1.ModeSingle,
		Port:   v1alpha1.ServicePort{Name: "postgres", Port: 5432},
		Plugin: v1alpha1.PluginSpec{CNPG: &v1alpha1.CNPGPlugin{Role: v1alpha1.CNPGRolePrimary}},
		Policy: v1alpha1.Policy{Multiple: multiple, UnknownHold: metav1.Duration{Duration: unknownHold}},
	}
	for _, site := range sites {
		name := "pg-" + site
		s.Members = append(s.Members, v1alpha1.Member{Name: name, Site: site, CNPG: &v1alpha1.CNPGMember{Cluster: name}})
	}
	return s
}

// setSpec is a Set SiteService with members r-a1 and r-a2 at site a, and r-b1 at site b.
func setSpec(locality v1alpha1.Locality, multiple v1alpha1.MultiplePolicy) v1alpha1.SiteServiceSpec {
	s := v1alpha1.SiteServiceSpec{
		Mode:     v1alpha1.ModeSet,
		Locality: locality,
		Port:     v1alpha1.ServicePort{Name: "postgres", Port: 5432},
		Plugin:   v1alpha1.PluginSpec{CNPG: &v1alpha1.CNPGPlugin{Role: v1alpha1.CNPGRoleAny}},
		Policy:   v1alpha1.Policy{Multiple: multiple, UnknownHold: metav1.Duration{Duration: hold}},
	}
	for _, m := range [][2]string{{"r-a1", "a"}, {"r-a2", "a"}, {"r-b1", "b"}} {
		s.Members = append(s.Members, v1alpha1.Member{Name: m[0], Site: m[1], CNPG: &v1alpha1.CNPGMember{Cluster: m[0]}})
	}
	return s
}

func declares(member string) decide.SiteView { return decide.SiteView{Known: true, Declared: member} }

func stale(member string) decide.SiteView { return decide.SiteView{Declared: member} }

func obs(member, site string, state v1alpha1.State, role v1alpha1.Role, eligible bool) observe.Observation {
	return observe.Observation{Member: member, Site: site, Role: role, Eligible: eligible, State: state, ObservedAt: now}
}

func writable(member, site string) observe.Observation {
	return obs(member, site, v1alpha1.StateStable, v1alpha1.RolePrimary, true)
}

func replica(member, site string) observe.Observation {
	return obs(member, site, v1alpha1.StateStable, v1alpha1.RoleReplica, true)
}

func moving(member, site string) observe.Observation {
	return obs(member, site, v1alpha1.StateTransitioning, v1alpha1.RolePrimary, true)
}

func unknown(member, site string) observe.Observation {
	return observe.Unknown(member, site, "NotRead", now)
}

func observations(list ...observe.Observation) map[string]observe.Observation {
	m := map[string]observe.Observation{}
	for _, o := range list {
		m[o.Member] = o
	}
	return m
}

func same(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func check(t *testing.T, got, want decide.Result) {
	t.Helper()
	if !same(got.Members, want.Members) {
		t.Errorf("members = %v, want %v", got.Members, want.Members)
	}
	if got.Reason != want.Reason {
		t.Errorf("reason = %q, want %q", got.Reason, want.Reason)
	}
	if got.Multiple != want.Multiple {
		t.Errorf("multiple = %v, want %v", got.Multiple, want.Multiple)
	}
	if got.Disagree != want.Disagree {
		t.Errorf("disagree = %v, want %v", got.Disagree, want.Disagree)
	}
	if !same(got.Held, want.Held) {
		t.Errorf("held = %v, want %v", got.Held, want.Held)
	}
}

func TestSingle(t *testing.T) {
	two := pgSpec(v1alpha1.RouteNowhere, hold, "a", "b")
	three := pgSpec(v1alpha1.RouteNowhere, hold, "a", "b", "c")
	prefer := pgSpec(v1alpha1.PreferDeclaredIfConsistent, hold, "a", "b", "c")

	tests := []struct {
		name string
		in   decide.Input
		want decide.Result
	}{
		{
			name: "row 1: sites declare different primaries",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-b")},
				Observations: observations(replica("pg-a", "a"), replica("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Disagree: true},
		},
		{
			name: "row 1 before row 2: disagreement wins over PreferDeclared",
			in: decide.Input{
				Spec: prefer, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a"), "c": declares("pg-b")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b"), replica("pg-c", "c")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Multiple: true, Disagree: true},
		},
		{
			name: "row 1 before row 3: disagreement wins over Multiple",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-b")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Multiple: true, Disagree: true},
		},
		{
			name: "row 1 before row 5: disagreement wins over one writable member",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-b")},
				Observations: observations(writable("pg-a", "a"), replica("pg-b", "b")),
				Previous:     []string{"pg-a"},
			},
			want: decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Disagree: true},
		},
		{
			name: "row 1 before row 7: disagreement wins over a held Unknown member",
			in: decide.Input{
				Spec: three, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-b")},
				Observations:  observations(replica("pg-a", "a"), replica("pg-b", "b")),
				Previous:      []string{"pg-c"},
				LastConfirmed: map[string]time.Time{"pg-c": ago(5 * time.Second)},
			},
			want: decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Disagree: true},
		},
		{
			name: "row 2: every site declares the same writable member",
			in: decide.Input{
				Spec: prefer, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a"), "c": declares("pg-a")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b"), replica("pg-c", "c")),
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonPreferDeclared, Multiple: true},
		},
		{
			name: "row 2: the declared member may be remote",
			in: decide.Input{
				Spec: prefer, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-b"), "c": declares("pg-b")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b"), replica("pg-c", "c")),
			},
			want: decide.Result{Members: []string{"pg-b"}, Reason: v1alpha1.ReasonPreferDeclared, Multiple: true},
		},
		{
			name: "row 2 does not match when a site with a member is unknown",
			in: decide.Input{
				Spec: prefer, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonMultipleWritable, Multiple: true},
		},
		{
			name: "row 2 does not match when a site with a member is stale, even if its last declaration agrees",
			in: decide.Input{
				Spec: prefer, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a"), "c": stale("pg-a")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonMultipleWritable, Multiple: true},
		},
		{
			name: "row 2 does not match when the declared member is not writable",
			in: decide.Input{
				Spec: prefer, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-c"), "b": declares("pg-c"), "c": declares("pg-c")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b"), replica("pg-c", "c")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonMultipleWritable, Multiple: true},
		},
		{
			name: "row 2 does not match without the policy",
			in: decide.Input{
				Spec: three, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a"), "c": declares("pg-a")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b"), replica("pg-c", "c")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonMultipleWritable, Multiple: true},
		},
		{
			name: "row 3: two writable members",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b")),
				Previous:     []string{"pg-a"},
			},
			want: decide.Result{Reason: v1alpha1.ReasonMultipleWritable, Multiple: true},
		},
		{
			name: "row 4: the writable member is not the declared one",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-b")},
				Observations: observations(writable("pg-a", "a"), replica("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonNotDeclared},
		},
		{
			name: "row 4: only the local site is known and it declares another member",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-b")},
				Observations: observations(writable("pg-a", "a")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonNotDeclared},
		},
		{
			name: "row 5: one writable member at the local site",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(writable("pg-a", "a"), replica("pg-b", "b")),
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "row 5: one writable member at a remote site",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-b")},
				Observations: observations(replica("pg-a", "a"), writable("pg-b", "b")),
			},
			want: decide.Result{Members: []string{"pg-b"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "row 5 before row 6: a Transitioning member does not block the writable one",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(writable("pg-a", "a"), moving("pg-b", "b")),
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "row 5 before row 7: a writable member wins over a held Unknown one",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-a")},
				Observations:  observations(writable("pg-a", "a")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(5 * time.Second)},
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "row 6: a member is Transitioning",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-b")},
				Observations: observations(replica("pg-a", "a"), moving("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonTransitioning},
		},
		{
			name: "row 6 before row 7: Transitioning wins over a held Unknown member",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b")},
				Observations:  observations(moving("pg-a", "a")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(5 * time.Second)},
			},
			want: decide.Result{Reason: v1alpha1.ReasonTransitioning},
		},
		{
			name: "row 7: the previous member is Unknown within its hold",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b")},
				Observations:  observations(replica("pg-a", "a")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(10 * time.Second)},
			},
			want: decide.Result{Members: []string{"pg-b"}, Reason: v1alpha1.ReasonHeldUnknown, Held: []string{"pg-b"}},
		},
		{
			name: "row 7: a known site reports its member Unknown",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-b")},
				Observations:  observations(replica("pg-a", "a"), unknown("pg-b", "b")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(29 * time.Second)},
			},
			want: decide.Result{Members: []string{"pg-b"}, Reason: v1alpha1.ReasonHeldUnknown, Held: []string{"pg-b"}},
		},
		{
			name: "row 8: the previous member is Unknown past its hold",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b")},
				Observations:  observations(replica("pg-a", "a")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(31 * time.Second)},
			},
			want: decide.Result{Reason: v1alpha1.ReasonUnknownTimeout},
		},
		{
			name: "row 8: elapsed time equal to the hold is past it",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b")},
				Observations:  observations(replica("pg-a", "a")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(hold)},
			},
			want: decide.Result{Reason: v1alpha1.ReasonUnknownTimeout},
		},
		{
			name: "row 8: a zero hold drops an Unknown member at once",
			in: decide.Input{
				Spec: pgSpec(v1alpha1.RouteNowhere, 0, "a", "b"), LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b")},
				Observations:  observations(replica("pg-a", "a")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": now},
			},
			want: decide.Result{Reason: v1alpha1.ReasonUnknownTimeout},
		},
		{
			name: "row 8: a previous member never confirmed is past its hold",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-b")},
				Observations: observations(replica("pg-a", "a")),
				Previous:     []string{"pg-b"},
			},
			want: decide.Result{Reason: v1alpha1.ReasonUnknownTimeout},
		},
		{
			name: "row 9: no writable member and no previous route",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(replica("pg-a", "a"), replica("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonNoEligibleMember},
		},
		{
			name: "row 9: every member Unknown and no previous route",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-a")},
				Observations:  observations(unknown("pg-a", "a")),
				LastConfirmed: map[string]time.Time{"pg-a": ago(time.Second), "pg-b": ago(time.Second)},
			},
			want: decide.Result{Reason: v1alpha1.ReasonMemberUnknown},
		},
		{
			name: "row 9: a member Unknown after its hold dropped it from the route",
			in: decide.Input{
				Spec: two, LocalSite: "b",
				Sites:        map[string]decide.SiteView{"b": declares("pg-a")},
				Observations: observations(replica("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonMemberUnknown},
		},
		{
			name: "row 9: the previous member is now a replica",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(replica("pg-a", "a"), replica("pg-b", "b")),
				Previous:     []string{"pg-a"},
			},
			want: decide.Result{Reason: v1alpha1.ReasonNoEligibleMember},
		},
		{
			name: "row 9: a Stable primary that is not eligible is not writable",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(obs("pg-a", "a", v1alpha1.StateStable, v1alpha1.RolePrimary, false), replica("pg-b", "b")),
			},
			want: decide.Result{Reason: v1alpha1.ReasonNoEligibleMember},
		},
		{
			name: "req 16: a missing observation counts as Unknown",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
				Observations: observations(writable("pg-a", "a")),
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "req 16: a stale peer's last writable observation does not count as writable",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": stale("pg-a")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b")),
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "req 16: a stale peer's member is held as Unknown",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b"), "b": stale("pg-b")},
				Observations:  observations(replica("pg-a", "a"), writable("pg-b", "b")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(5 * time.Second)},
			},
			want: decide.Result{Members: []string{"pg-b"}, Reason: v1alpha1.ReasonHeldUnknown, Held: []string{"pg-b"}},
		},
		{
			name: "req 16: a stale peer's member past its hold routes nowhere",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-b"), "b": stale("pg-b")},
				Observations:  observations(replica("pg-a", "a"), writable("pg-b", "b")),
				Previous:      []string{"pg-b"},
				LastConfirmed: map[string]time.Time{"pg-b": ago(31 * time.Second)},
			},
			want: decide.Result{Reason: v1alpha1.ReasonUnknownTimeout},
		},
		{
			name: "a stale peer's declaration is not compared",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": stale("pg-b")},
				Observations: observations(writable("pg-a", "a")),
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "partition: site a keeps its primary while site b is unknown",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:         map[string]decide.SiteView{"a": declares("pg-a")},
				Observations:  observations(writable("pg-a", "a")),
				Previous:      []string{"pg-a"},
				LastConfirmed: map[string]time.Time{"pg-a": now, "pg-b": ago(time.Minute)},
			},
			want: decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "partition: site b routes to its own primary after the operator promotes it",
			in: decide.Input{
				Spec: two, LocalSite: "b",
				Sites:         map[string]decide.SiteView{"b": declares("pg-b")},
				Observations:  observations(writable("pg-b", "b")),
				Previous:      []string{"pg-a"},
				LastConfirmed: map[string]time.Time{"pg-a": ago(time.Minute), "pg-b": now},
			},
			want: decide.Result{Members: []string{"pg-b"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name: "partition healed: site a sees two writable members",
			in: decide.Input{
				Spec: two, LocalSite: "a",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-b")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b")),
				Previous:     []string{"pg-a"},
			},
			want: decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Multiple: true, Disagree: true},
		},
		{
			name: "partition healed: site b sees two writable members",
			in: decide.Input{
				Spec: two, LocalSite: "b",
				Sites:        map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-b")},
				Observations: observations(writable("pg-a", "a"), writable("pg-b", "b")),
				Previous:     []string{"pg-b"},
			},
			want: decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Multiple: true, Disagree: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.Now = now
			check(t, decide.Decide(tt.in), tt.want)
		})
	}
}

func TestSwitchover(t *testing.T) {
	steps := []struct {
		name     string
		sites    map[string]decide.SiteView
		obs      map[string]observe.Observation
		previous []string
		want     decide.Result
	}{
		{
			name:     "before",
			sites:    map[string]decide.SiteView{"a": declares("pg-a"), "b": declares("pg-a")},
			obs:      observations(writable("pg-a", "a"), replica("pg-b", "b")),
			previous: []string{"pg-a"},
			want:     decide.Result{Members: []string{"pg-a"}, Reason: v1alpha1.ReasonActive},
		},
		{
			name:     "a is demoted",
			sites:    map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-a")},
			obs:      observations(replica("pg-a", "a"), replica("pg-b", "b")),
			previous: []string{"pg-a"},
			want:     decide.Result{Reason: v1alpha1.ReasonDeclarationsDisagree, Disagree: true},
		},
		{
			name:  "b is told to promote",
			sites: map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-b")},
			obs:   observations(replica("pg-a", "a"), moving("pg-b", "b")),
			want:  decide.Result{Reason: v1alpha1.ReasonTransitioning},
		},
		{
			name:  "b is promoted",
			sites: map[string]decide.SiteView{"a": declares("pg-b"), "b": declares("pg-b")},
			obs:   observations(replica("pg-a", "a"), writable("pg-b", "b")),
			want:  decide.Result{Members: []string{"pg-b"}, Reason: v1alpha1.ReasonActive},
		},
	}
	for _, local := range []string{"a", "b"} {
		for _, step := range steps {
			t.Run("site "+local+"/"+step.name, func(t *testing.T) {
				in := decide.Input{
					Spec:          pgSpec(v1alpha1.RouteNowhere, hold, "a", "b"),
					LocalSite:     local,
					Sites:         step.sites,
					Observations:  step.obs,
					Previous:      step.previous,
					LastConfirmed: map[string]time.Time{"pg-a": now, "pg-b": now},
					Now:           now,
				}
				check(t, decide.Decide(in), step.want)
			})
		}
	}
}

func TestSet(t *testing.T) {
	both := map[string]decide.SiteView{"a": declares(""), "b": declares("")}
	all := []string{"r-a1", "r-a2", "r-b1"}

	tests := []struct {
		name      string
		spec      v1alpha1.SiteServiceSpec
		sites     map[string]decide.SiteView
		obs       map[string]observe.Observation
		previous  []string
		confirmed map[string]time.Time
		want      []string
		held      []string
	}{
		{
			name:  "Any routes every eligible member",
			spec:  setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites: both,
			obs:   observations(replica("r-a1", "a"), replica("r-a2", "a"), replica("r-b1", "b")),
			want:  all,
		},
		{
			name:  "Any skips a Stable member that is not eligible",
			spec:  setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites: both,
			obs:   observations(replica("r-a1", "a"), obs("r-a2", "a", v1alpha1.StateStable, v1alpha1.RoleReplica, false), replica("r-b1", "b")),
			want:  []string{"r-a1", "r-b1"},
		},
		{
			name:  "LocalFirst routes local eligible members",
			spec:  setSpec(v1alpha1.LocalFirst, v1alpha1.RouteNowhere),
			sites: both,
			obs:   observations(replica("r-a1", "a"), replica("r-a2", "a"), replica("r-b1", "b")),
			want:  []string{"r-a1", "r-a2"},
		},
		{
			name:  "an empty locality acts as LocalFirst",
			spec:  setSpec("", v1alpha1.RouteNowhere),
			sites: both,
			obs:   observations(replica("r-a1", "a"), replica("r-a2", "a"), replica("r-b1", "b")),
			want:  []string{"r-a1", "r-a2"},
		},
		{
			name:     "LocalFirst falls back to every eligible member when no local member is confirmed",
			spec:     setSpec(v1alpha1.LocalFirst, v1alpha1.RouteNowhere),
			sites:    both,
			obs:      observations(obs("r-a1", "a", v1alpha1.StateStable, v1alpha1.RoleReplica, false), unknown("r-a2", "a"), replica("r-b1", "b")),
			previous: []string{"r-b1"},
			want:     []string{"r-b1"},
		},
		{
			name:  "LocalFirst falls back when local members are Transitioning and were not routed",
			spec:  setSpec(v1alpha1.LocalFirst, v1alpha1.RouteNowhere),
			sites: both,
			obs:   observations(moving("r-a1", "a"), moving("r-a2", "a"), replica("r-b1", "b")),
			want:  []string{"r-b1"},
		},
		{
			name:  "LocalOnly routes local eligible members only",
			spec:  setSpec(v1alpha1.LocalOnly, v1alpha1.RouteNowhere),
			sites: both,
			obs:   observations(replica("r-a1", "a"), replica("r-a2", "a"), replica("r-b1", "b")),
			want:  []string{"r-a1", "r-a2"},
		},
		{
			name:  "LocalOnly routes nowhere without a local eligible member",
			spec:  setSpec(v1alpha1.LocalOnly, v1alpha1.RouteNowhere),
			sites: both,
			obs:   observations(obs("r-a1", "a", v1alpha1.StateStable, v1alpha1.RoleReplica, false), unknown("r-a2", "a"), replica("r-b1", "b")),
		},
		{
			name:     "req 20: a Transitioning member in the previous route stays",
			spec:     setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites:    both,
			obs:      observations(replica("r-a1", "a"), moving("r-a2", "a"), replica("r-b1", "b")),
			previous: all,
			want:     all,
		},
		{
			name:     "req 20: a Transitioning member not in the previous route is not added",
			spec:     setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites:    both,
			obs:      observations(replica("r-a1", "a"), moving("r-a2", "a"), replica("r-b1", "b")),
			previous: []string{"r-a1", "r-b1"},
			want:     []string{"r-a1", "r-b1"},
		},
		{
			name:      "req 17: an Unknown member in the previous route is held",
			spec:      setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites:     map[string]decide.SiteView{"a": declares("")},
			obs:       observations(replica("r-a1", "a"), replica("r-a2", "a")),
			previous:  all,
			confirmed: map[string]time.Time{"r-b1": ago(10 * time.Second)},
			want:      all,
			held:      []string{"r-b1"},
		},
		{
			name:      "req 17: an Unknown member past its hold is dropped",
			spec:      setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites:     map[string]decide.SiteView{"a": declares("")},
			obs:       observations(replica("r-a1", "a"), replica("r-a2", "a")),
			previous:  all,
			confirmed: map[string]time.Time{"r-b1": ago(31 * time.Second)},
			want:      []string{"r-a1", "r-a2"},
		},
		{
			name:      "req 17: elapsed time equal to the hold drops an Unknown member",
			spec:      setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites:     map[string]decide.SiteView{"a": declares("")},
			obs:       observations(replica("r-a1", "a"), replica("r-a2", "a")),
			previous:  all,
			confirmed: map[string]time.Time{"r-b1": ago(hold)},
			want:      []string{"r-a1", "r-a2"},
		},
		{
			name:      "req 17: an Unknown member not in the previous route is not added",
			spec:      setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites:     map[string]decide.SiteView{"a": declares("")},
			obs:       observations(replica("r-a1", "a"), replica("r-a2", "a")),
			previous:  []string{"r-a1", "r-a2"},
			confirmed: map[string]time.Time{"r-b1": ago(time.Second)},
			want:      []string{"r-a1", "r-a2"},
		},
		{
			name:      "req 17: LocalFirst holds an Unknown local member",
			spec:      setSpec(v1alpha1.LocalFirst, v1alpha1.RouteNowhere),
			sites:     both,
			obs:       observations(replica("r-a1", "a"), unknown("r-a2", "a"), replica("r-b1", "b")),
			previous:  []string{"r-a1", "r-a2"},
			confirmed: map[string]time.Time{"r-a1": now, "r-a2": ago(10 * time.Second)},
			want:      []string{"r-a1", "r-a2"},
			held:      []string{"r-a2"},
		},
		{
			name:  "req 16: a stale peer's last eligible observation does not count",
			spec:  setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites: map[string]decide.SiteView{"a": declares(""), "b": stale("")},
			obs:   observations(replica("r-a1", "a"), replica("r-a2", "a"), replica("r-b1", "b")),
			want:  []string{"r-a1", "r-a2"},
		},
		{
			name:  "req 21: disagreeing declarations do not change the route",
			spec:  setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites: map[string]decide.SiteView{"a": declares("r-a1"), "b": declares("r-b1")},
			obs:   observations(replica("r-a1", "a"), replica("r-a2", "a"), replica("r-b1", "b")),
			want:  all,
		},
		{
			name:  "req 21: RouteNowhere does not empty a Set with two primaries",
			spec:  setSpec(v1alpha1.Any, v1alpha1.RouteNowhere),
			sites: map[string]decide.SiteView{"a": declares("r-a1"), "b": declares("r-a1")},
			obs:   observations(writable("r-a1", "a"), replica("r-a2", "a"), writable("r-b1", "b")),
			want:  all,
		},
		{
			name:  "req 21: PreferDeclaredIfConsistent does not narrow a Set",
			spec:  setSpec(v1alpha1.Any, v1alpha1.PreferDeclaredIfConsistent),
			sites: map[string]decide.SiteView{"a": declares("r-a1"), "b": declares("r-a1")},
			obs:   observations(writable("r-a1", "a"), replica("r-a2", "a"), writable("r-b1", "b")),
			want:  all,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decide.Decide(decide.Input{
				Spec:          tt.spec,
				LocalSite:     "a",
				Sites:         tt.sites,
				Observations:  tt.obs,
				Previous:      tt.previous,
				LastConfirmed: tt.confirmed,
				Now:           now,
			})
			if !same(got.Members, tt.want) {
				t.Errorf("members = %v, want %v", got.Members, tt.want)
			}
			if !same(got.Held, tt.held) {
				t.Errorf("held = %v, want %v", got.Held, tt.held)
			}
		})
	}
}
