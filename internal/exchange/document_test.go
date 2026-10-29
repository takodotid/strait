package exchange

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/observe"
)

// rfcExample is the document of RFC#0004, verbatim.
const rfcExample = `{
  "apiVersion": "strait.tako.id/v1alpha1",
  "kind": "Observations",
  "site": "a",
  "generatedAt": "2026-10-27T10:00:00Z",
  "services": [
    {
      "namespace": "app",
      "name": "pg",
      "declaredPrimary": "pg-a",
      "members": [
        {
          "name": "pg-a",
          "role": "primary",
          "eligible": true,
          "state": "Stable",
          "reason": "ClusterPrimary",
          "observedAt": "2026-10-27T10:00:00Z",
          "addresses": ["10.110.1.5"],
          "port": 5432
        }
      ]
    }
  ],
  "exports": [
    { "namespace": "app", "name": "pg-pg-a", "port": 30901, "ready": true }
  ]
}`

var at = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

func exampleLocal() *Local {
	l := &Local{}
	l.SetService("app", "pg", "pg-a", []observe.Observation{{
		Member:     "pg-a",
		Site:       "a",
		Role:       straitv1.RolePrimary,
		Eligible:   true,
		State:      straitv1.StateStable,
		Reason:     "ClusterPrimary",
		ObservedAt: at,
		Endpoint:   observe.Endpoint{Service: "pg-a-rw", Addresses: []string{"10.110.1.5"}, Port: 5432},
	}})
	l.SetExport("app", "pg-pg-a", 30901, true)
	return l
}

func TestDocumentShape(t *testing.T) {
	got, err := json.Marshal(exampleLocal().Document("a", at))
	if err != nil {
		t.Fatal(err)
	}
	var want, have map[string]any
	if err := json.Unmarshal([]byte(rfcExample), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, have) {
		t.Fatalf("document differs from RFC#0004\nwant %v\nhave %v", want, have)
	}
}

func TestDocumentEmptyFields(t *testing.T) {
	l := &Local{}
	l.SetService("app", "pg", "", []observe.Observation{observe.Unknown("pg-a", "a", "NotObserved", at)})
	l.SetExport("app", "pg-pg-a", 30901, false)
	got, err := json.Marshal(l.Document("a", at))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"declaredPrimary":""`, `"eligible":false`, `"addresses":[]`, `"port":0`, `"ready":false`} {
		if !strings.Contains(string(got), field) {
			t.Errorf("document lacks %s: %s", field, got)
		}
	}
	empty, err := json.Marshal((&Local{}).Document("a", at))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"services":[],"exports":[]`) {
		t.Errorf("empty document lacks empty lists: %s", empty)
	}
}

func TestMemberObservation(t *testing.T) {
	o := observe.Observation{
		Member:     "pg-b",
		Site:       "b",
		Role:       straitv1.RoleReplica,
		Eligible:   true,
		State:      straitv1.StateTransitioning,
		Reason:     "Switchover",
		ObservedAt: at,
		Endpoint:   observe.Endpoint{Addresses: []string{"10.0.0.2", "10.0.0.1"}, Port: 5432},
	}
	m := MemberFrom(o)
	if !reflect.DeepEqual(m.Addresses, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Errorf("addresses not sorted: %v", m.Addresses)
	}
	if o.Endpoint.Addresses[0] != "10.0.0.2" {
		t.Errorf("MemberFrom changed its input")
	}
	got := m.Observation("b")
	o.Endpoint.Addresses = []string{"10.0.0.1", "10.0.0.2"}
	if !reflect.DeepEqual(got, o) {
		t.Errorf("round trip\nwant %+v\nhave %+v", o, got)
	}
}

func TestValidate(t *testing.T) {
	good := exampleLocal().Document("a", at)
	if err := good.validate("a"); err != nil {
		t.Fatalf("good document: %v", err)
	}
	tests := []struct {
		name   string
		change func(*Document)
		want   string
	}{
		{"apiVersion", func(d *Document) { d.APIVersion = "v1" }, "unexpected document"},
		{"kind", func(d *Document) { d.Kind = "ConfigMap" }, "unexpected document"},
		{"site", func(d *Document) { d.Site = "c" }, "site mismatch"},
		{"duplicate service", func(d *Document) { d.Services = append(d.Services, d.Services[0]) }, "duplicate service"},
		{"duplicate member", func(d *Document) {
			d.Services[0].Members = append(d.Services[0].Members, d.Services[0].Members[0])
		}, "duplicate member"},
		{"duplicate export", func(d *Document) { d.Exports = append(d.Exports, d.Exports[0]) }, "duplicate export"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := exampleLocal().Document("a", at)
			tt.change(&d)
			err := d.validate("a")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error %q, have %v", tt.want, err)
			}
		})
	}
}
