package helm

import (
	"bufio"
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/yaml"

	"github.com/takodotid/strait/internal/gateway"
	"github.com/takodotid/strait/internal/metrics"
	"github.com/takodotid/strait/internal/xds"
)

// monitoring is what the chart renders for the Prometheus Operator.
type monitoring struct {
	podMonitors map[string]podMonitor
	rules       []rule
}

type podMonitor struct {
	Spec struct {
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		PodMetricsEndpoints []struct {
			Port string `json:"port"`
			Path string `json:"path"`
		} `json:"podMetricsEndpoints"`
	} `json:"spec"`
}

type rule struct {
	Alert string `json:"alert"`
	Expr  string `json:"expr"`
}

func renderMonitoring(t *testing.T, set ...string) monitoring {
	t.Helper()
	out, err := helmTemplate(t, set...)
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, out)
	}
	m := monitoring{podMonitors: map[string]podMonitor{}}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(out)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			Kind     string
			Metadata struct{ Name string }
		}
		if err := yaml.Unmarshal(doc, &head); err != nil {
			t.Fatal(err)
		}
		switch head.Kind {
		case "PodMonitor":
			var pm podMonitor
			if err := yaml.Unmarshal(doc, &pm); err != nil {
				t.Fatal(err)
			}
			m.podMonitors[head.Metadata.Name] = pm
		case "PrometheusRule":
			var pr struct {
				Spec struct {
					Groups []struct {
						Rules []rule `json:"rules"`
					} `json:"groups"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal(doc, &pr); err != nil {
				t.Fatal(err)
			}
			for _, g := range pr.Spec.Groups {
				m.rules = append(m.rules, g.Rules...)
			}
		}
	}
	return m
}

// The PodMonitors and the PrometheusRule are off by default, since their CRDs may be absent. On, each PodMonitor
// selects the pods of one component, on a port that the pods name.
func TestPodMonitors(t *testing.T) {
	if m := renderMonitoring(t); len(m.podMonitors) != 0 || len(m.rules) != 0 {
		t.Fatalf("by default: %d PodMonitors, %d rules", len(m.podMonitors), len(m.rules))
	}
	m := renderMonitoring(t, "metrics.podMonitor.enabled=true")
	pods := render(t).pods
	want := map[string][]string{
		"strait-controller": {"metrics /metrics"},
		"strait-gateway":    {"metrics /metrics", "metrics /stats/prometheus"},
	}
	if len(m.podMonitors) != len(want) {
		t.Fatalf("PodMonitors: %v", m.podMonitors)
	}
	for name, endpoints := range want {
		pm, ok := m.podMonitors[name]
		pod, found := pods[name]
		if !ok || !found {
			t.Fatalf("PodMonitor %s: %v, pod: %v", name, ok, found)
		}
		for k, v := range pm.Spec.Selector.MatchLabels {
			if pod.Labels[k] != v {
				t.Errorf("PodMonitor %s selects %s=%s, the pods have %q", name, k, v, pod.Labels[k])
			}
		}
		var got []string
		for _, e := range pm.Spec.PodMetricsEndpoints {
			got = append(got, e.Port+" "+e.Path)
			named := false
			for _, c := range pod.Spec.Containers {
				named = named || slices.ContainsFunc(c.Ports, func(p corev1.ContainerPort) bool { return p.Name == e.Port })
			}
			if !named {
				t.Errorf("PodMonitor %s scrapes port %s, which no container of the pod names", name, e.Port)
			}
		}
		if !slices.Equal(got, endpoints) {
			t.Errorf("PodMonitor %s endpoints %v, want %v", name, got, endpoints)
		}
	}
}

// exported returns the name of every metric that Strait's controller and gateway agent export, other than those of
// Go, the process, controller-runtime and client-go.
func exported(t *testing.T) []string {
	t.Helper()
	descName := regexp.MustCompile(`fqName: "([^"]+)"`)
	var out []string
	for _, c := range []prometheus.Collector{&metrics.Collector{}, xds.RejectedTotal, gateway.NewCollector(nil)} {
		ch := make(chan *prometheus.Desc, 100)
		c.Describe(ch)
		close(ch)
		for d := range ch {
			m := descName.FindStringSubmatch(d.String())
			if m == nil {
				t.Fatalf("no name in %s", d)
			}
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// envoyStats are the Envoy metrics that the alert rules use, by the name of their Envoy stat.
var envoyStats = map[string]string{"envoy_control_plane_connected_state": "control_plane.connected_state"}

// Every metric that an alert rule uses is exported: by Strait, by controller-runtime's leader election, or by Envoy
// through the gateway agent's filter.
func TestAlertRulesUseExportedMetrics(t *testing.T) {
	m := renderMonitoring(t, "metrics.prometheusRule.enabled=true")
	if len(m.rules) == 0 {
		t.Fatal("no rules")
	}
	known := exported(t)
	// controller-runtime registers the leader election gauge; registering it again fails with the same descriptor.
	leader := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "leader_election_master_status",
		Help: "Gauge of if the reporting system is master of the relevant lease, 0 indicates backup, 1 indicates master. 'name' is the string used to identify the lease. Please make sure to group by name.",
	}, []string{"name"})
	if err := ctrlmetrics.Registry.Register(leader); !errors.As(err, &prometheus.AlreadyRegisteredError{}) {
		t.Fatalf("controller-runtime does not register leader_election_master_status: %v", err)
	}
	known = append(known, "leader_election_master_status")
	filter := regexp.MustCompile(gateway.EnvoyStats)
	name := regexp.MustCompile(`\b(strait|leader_election|envoy)_[a-z_]+\b`)
	seen := map[string]bool{}
	for _, r := range m.rules {
		names := name.FindAllString(r.Expr, -1)
		if len(names) == 0 {
			t.Errorf("alert %s uses no metric of Strait: %s", r.Alert, r.Expr)
		}
		for _, n := range names {
			seen[n] = true
			if strings.HasPrefix(n, "envoy_") {
				if stat, ok := envoyStats[n]; !ok || !filter.MatchString(stat) {
					t.Errorf("alert %s uses %s, which the gateway agent does not serve", r.Alert, n)
				}
				continue
			}
			if !slices.Contains(known, n) {
				t.Errorf("alert %s uses %s, which nothing exports", r.Alert, n)
			}
		}
	}
	t.Logf("the alert rules use %d metrics", len(seen))
}

// docs/metrics.md lists every metric that Strait exports.
func TestMetricsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/metrics.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range exported(t) {
		if !strings.Contains(string(b), "`"+n+"`") {
			t.Errorf("docs/metrics.md does not list %s", n)
		}
	}
}
