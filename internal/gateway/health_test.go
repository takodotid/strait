package gateway

import (
	"errors"
	"os"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/links"
)

// testdata/clusters.json is the /clusters?format=json output of Envoy v1.40.2 with three static clusters:
// peer/b has a dead endpoint at priority 0 and a live one at priority 1, peer/c has a live endpoint that
// endpoint discovery marks unhealthy, and import/app/pg has no health check.
func fixture(t *testing.T) map[string][]Host {
	t.Helper()
	b, err := os.ReadFile("testdata/clusters.json")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := ParseClusters(b)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestParseClusters(t *testing.T) {
	cs := fixture(t)
	want := map[string][]Host{
		"peer/b": {
			{Address: "192.0.2.99", Priority: 0, FailedActiveHealthCheck: true, EDSHealth: "HEALTHY"},
			{Address: "192.0.2.2", Priority: 1, EDSHealth: "HEALTHY"},
		},
		"peer/c":        {{Address: "192.0.2.2", EDSHealth: "UNHEALTHY"}},
		"import/app/pg": {{Address: "192.0.2.2", EDSHealth: "HEALTHY"}},
	}
	if len(cs) != len(want) {
		t.Fatalf("clusters = %v", cs)
	}
	for name, hosts := range want {
		got := cs[name]
		if len(got) != len(hosts) {
			t.Fatalf("%s: %v", name, got)
		}
		for i := range hosts {
			if got[i] != hosts[i] {
				t.Errorf("%s host %d = %+v, want %+v", name, i, got[i], hosts[i])
			}
		}
	}
	if cs["peer/b"][0].Healthy() || !cs["peer/b"][1].Healthy() || cs["peer/c"][0].Healthy() {
		t.Error("Healthy disagrees with the fixture")
	}
}

func fixtureLinks() []straitv1.SiteLink {
	return []straitv1.SiteLink{
		directLink("b-direct", "b", 10, "192.0.2.99"),
		wgLink("b-wg", "b", 100, 51820, "", "100.64.0.1/32", "192.0.2.2"),
		directLink("b-spare", "b", 200, "192.0.2.50"),
		directLink("c-direct", "c", 10, "192.0.2.2"),
		wgLink("c-wg", "c", 100, 51821, "", "100.64.1.1/32", "100.64.1.2"),
	}
}

func TestObserve(t *testing.T) {
	held := map[string]string{"b-wg": links.Interface("b-wg")}
	problems := map[string]string{"c-wg": "SitePeer c has no WireGuard public key"}
	obs := Observe(fixtureLinks(), fixture(t), nil, held, problems)
	want := map[string]Observation{
		"b-direct": {Reason: "HealthCheckFailed"},
		"b-wg":     {Healthy: true, Active: true, Reason: "HealthCheckPassed"},
		"b-spare":  {Reason: "NotInEnvoy"},
		"c-direct": {Reason: "EndpointUnhealthy"},
		"c-wg":     {Reason: "NotConfigured", Message: "SitePeer c has no WireGuard public key"},
	}
	for name, w := range want {
		o := obs[name]
		if o.Healthy != w.Healthy || o.Active != w.Active || o.Reason != w.Reason || (w.Message != "" && o.Message != w.Message) {
			t.Errorf("%s = %+v, want %+v", name, o, w)
		}
	}

	obs = Observe(fixtureLinks(), nil, errors.New("connection refused"), held, problems)
	for name, o := range obs {
		if o.Healthy || o.Active || o.Reason != "EnvoyUnreachable" {
			t.Errorf("%s with Envoy unreachable = %+v", name, o)
		}
	}
}

func TestActiveLink(t *testing.T) {
	ls := []straitv1.SiteLink{
		directLink("lan-2", "b", 10, "198.51.100.2"),
		directLink("lan-1", "b", 10, "198.51.100.1"),
		directLink("wan", "b", 5, "192.0.2.1"),
	}
	hosts := func(healthy ...bool) map[string][]Host {
		var hs []Host
		for i, addr := range []string{"192.0.2.1", "198.51.100.1", "198.51.100.2"} {
			hs = append(hs, Host{Address: addr, FailedActiveHealthCheck: !healthy[i]})
		}
		return map[string][]Host{"peer/b": hs}
	}
	for _, tc := range []struct {
		healthy []bool
		active  string
	}{
		{[]bool{true, true, true}, "wan"},
		{[]bool{false, true, true}, "lan-1"},
		{[]bool{false, false, true}, "lan-2"},
		{[]bool{false, false, false}, ""},
	} {
		obs := Observe(ls, hosts(tc.healthy...), nil, nil, nil)
		var active []string
		for name, o := range obs {
			if o.Active {
				active = append(active, name)
			}
		}
		if (tc.active == "" && len(active) != 0) || (tc.active != "" && (len(active) != 1 || active[0] != tc.active)) {
			t.Errorf("healthy %v: active %v, want %q", tc.healthy, active, tc.active)
		}
	}
}

func TestLinkStatus(t *testing.T) {
	l := wgLink("b-wg", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
	l.Generation = 3
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 600_000_000, time.UTC)
	hs := t0.Add(-10*time.Second + 300*time.Millisecond)

	st := LinkStatus(l, Observation{Healthy: true, Active: true, Reason: "HealthCheckPassed", Message: "ok"}, "st01234567", hs, t0)
	if !st.Healthy || !st.Active || st.Interface != "st01234567" || !st.LastTransitionTime.Time.Equal(t0.Truncate(time.Second)) {
		t.Fatalf("status = %+v", st)
	}
	if !st.LastHandshakeTime.Time.Equal(hs.Truncate(time.Second)) {
		t.Errorf("handshake = %v", st.LastHandshakeTime)
	}
	c := meta.FindStatusCondition(st.Conditions, ConditionHealthy)
	if c == nil || c.Status != "True" || c.ObservedGeneration != 3 {
		t.Fatalf("condition = %+v", c)
	}

	l.Status = st
	t1 := t0.Add(5 * time.Second)
	if again := LinkStatus(l, Observation{Healthy: true, Active: true, Reason: "HealthCheckPassed", Message: "ok"}, "st01234567", hs, t1); !again.LastTransitionTime.Equal(st.LastTransitionTime) {
		t.Errorf("unchanged health moved the transition time to %v", again.LastTransitionTime)
	}
	down := LinkStatus(l, Observation{Reason: "HealthCheckFailed"}, "", time.Time{}, t1)
	if down.Healthy || down.Active || down.Interface != "" || down.LastHandshakeTime != nil || !down.LastTransitionTime.Time.Equal(t1.Truncate(time.Second)) {
		t.Fatalf("status = %+v", down)
	}
}
