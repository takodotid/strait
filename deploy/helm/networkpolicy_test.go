package helm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// ports returns the ports of a policy rule.
func ports(rule networkingv1.NetworkPolicyIngressRule) []int32 {
	var out []int32
	for _, p := range rule.Ports {
		out = append(out, p.Port.IntVal)
	}
	return out
}

// ruleFor returns the rule of a policy that names the port.
func ruleFor(t *testing.T, p networkingv1.NetworkPolicy, port int32) networkingv1.NetworkPolicyIngressRule {
	t.Helper()
	for _, r := range p.Spec.Ingress {
		if slices.Contains(ports(r), port) {
			return r
		}
	}
	t.Fatalf("policy %s has no rule for port %d", p.Name, port)
	return networkingv1.NetworkPolicyIngressRule{}
}

// The controller's policy admits the gateway pods to the observation and xDS ports, and every source to the probes.
// The metrics port admits every source by default, and the sources of networkPolicy.metricsFrom when set. A policy on
// the gateway pods is off by default; when on, it admits every source to the probes, the metrics as the controller's,
// every pod of the cluster to the import and export ports only, the controller to the peer port, and
// networkPolicy.gateway.peerCIDRs (RFC0007 requirement 28; issue #30, rows 39 and 27; issue #64).
func TestNetworkPolicies(t *testing.T) {
	r := render(t)
	if _, ok := r.networkPolicies["strait-gateway"]; ok {
		t.Error("a policy on the gateway pods by default, want none")
	}
	c, ok := r.networkPolicies["strait-controller"]
	if !ok {
		t.Fatal("no policy on the controller by default")
	}
	gateway := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: gatewayLabels}}}
	for _, port := range []int32{8082, 18000} {
		if from := ruleFor(t, c, port).From; !reflect.DeepEqual(from, gateway) {
			t.Errorf("controller port %d admits %+v, want the gateway pods", port, from)
		}
	}
	for _, port := range []int32{8080, 8081} {
		if rule := ruleFor(t, c, port); rule.From != nil || len(rule.Ports) != 1 {
			t.Errorf("controller port %d by default: %+v, want a rule of its own that admits every source", port, rule)
		}
	}

	r = render(t, "networkPolicy.metricsFrom[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=monitoring")
	c = r.networkPolicies["strait-controller"]
	want := []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"}}}}
	if from := ruleFor(t, c, 8080).From; !reflect.DeepEqual(from, want) {
		t.Errorf("metrics with metricsFrom: %+v, want %+v", from, want)
	}
	if rule := ruleFor(t, c, 8081); rule.From != nil {
		t.Errorf("probes with metricsFrom: %+v, want every source", rule)
	}

	r = render(t, "networkPolicy.gateway.enabled=true", "networkPolicy.gateway.peerCIDRs={192.0.2.21/32,2001:db8::/64}",
		"networkPolicy.gateway.wireGuardPorts={31820}")
	g, ok := r.networkPolicies["strait-gateway"]
	if !ok {
		t.Fatal("networkPolicy.gateway.enabled: no policy on the gateway pods")
	}
	if !reflect.DeepEqual(g.Spec.PodSelector.MatchLabels, gatewayLabels) || !slices.Equal(g.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) {
		t.Errorf("gateway policy selects %v, types %v", g.Spec.PodSelector.MatchLabels, g.Spec.PolicyTypes)
	}
	probes, metrics := intstr.FromInt32(8090), intstr.FromInt32(8091)
	imports, exports, peer, control, listen := intstr.FromInt32(15000), intstr.FromInt32(30901), intstr.FromInt32(14999), intstr.FromInt32(30900), intstr.FromInt32(31820)
	udp := corev1.ProtocolUDP
	importsLast, exportsLast := int32(15999), int32(31899)
	controller := map[string]string{"app.kubernetes.io/name": "strait", "app.kubernetes.io/component": "controller"}
	wantRules := []networkingv1.NetworkPolicyIngressRule{
		{Ports: []networkingv1.NetworkPolicyPort{{Port: &probes}}},
		{Ports: []networkingv1.NetworkPolicyPort{{Port: &metrics}}},
		{
			From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
			Ports: []networkingv1.NetworkPolicyPort{{Port: &imports, EndPort: &importsLast}, {Port: &exports, EndPort: &exportsLast}},
		},
		{
			From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: controller}}},
			Ports: []networkingv1.NetworkPolicyPort{{Port: &peer}},
		},
		{
			From:  []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "192.0.2.21/32"}}, {IPBlock: &networkingv1.IPBlock{CIDR: "2001:db8::/64"}}},
			Ports: []networkingv1.NetworkPolicyPort{{Port: &control}, {Port: &exports, EndPort: &exportsLast}, {Protocol: &udp, Port: &listen}},
		},
	}
	if !reflect.DeepEqual(g.Spec.Ingress, wantRules) {
		t.Errorf("gateway policy rules:\n%+v\nwant\n%+v", g.Spec.Ingress, wantRules)
	}

	// The gateway's metrics follow networkPolicy.metricsFrom, as the controller's do.
	r = render(t, "networkPolicy.gateway.enabled=true",
		"networkPolicy.metricsFrom[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=monitoring")
	if from := ruleFor(t, r.networkPolicies["strait-gateway"], 8091).From; !reflect.DeepEqual(from, want) {
		t.Errorf("gateway metrics with metricsFrom: %+v, want %+v", from, want)
	}

	if r := render(t, "networkPolicy.enabled=false"); len(r.networkPolicies) != 0 {
		t.Errorf("networkPolicy.enabled false: policies %v, want none", r.networkPolicies)
	}
}

// The gateway agent's probe and metrics servers bind to the pod's own address, so a peer cannot reach them through a
// WireGuard interface (RFC0007, requirement 12).
func TestAgentProbeBindsThePodAddress(t *testing.T) {
	pod := render(t).pods["strait-gateway"]
	i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == "agent" })
	if i < 0 {
		t.Fatal("no agent container")
	}
	agent := pod.Spec.Containers[i]
	if !slices.Contains(agent.Args, "--probe-ip=$(POD_IP)") || !slices.Contains(agent.Args, "--metrics-addr=:8091") {
		t.Errorf("agent args %v, want --probe-ip=$(POD_IP) and --metrics-addr=:8091", agent.Args)
	}
	j := slices.IndexFunc(agent.Env, func(e corev1.EnvVar) bool { return e.Name == "POD_IP" })
	if j < 0 || agent.Env[j].ValueFrom == nil || agent.Env[j].ValueFrom.FieldRef == nil || agent.Env[j].ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Errorf("agent env %+v, want POD_IP from status.podIP", agent.Env)
	}
}

// podPorts returns the ports, as first-last, that the gateway policy admits from the peer given.
func podPorts(t *testing.T, p networkingv1.NetworkPolicy, from networkingv1.NetworkPolicyPeer) []string {
	t.Helper()
	var out []string
	for _, r := range p.Spec.Ingress {
		if len(r.From) != 1 || !reflect.DeepEqual(r.From[0], from) {
			continue
		}
		for _, port := range r.Ports {
			last := port.Port.IntVal
			if port.EndPort != nil {
				last = *port.EndPort
			}
			out = append(out, fmt.Sprintf("%d-%d", port.Port.IntVal, last))
		}
	}
	return out
}

// siteGatewayDefaults returns the Site CRD's defaults of exportPorts, importPorts and peerPort, as first-last.
func siteGatewayDefaults(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("strait/crds/strait.tako.id_sites.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	for _, v := range crd.Spec.Versions {
		if !v.Storage {
			continue
		}
		props := v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["gateway"].Properties
		var out []string
		for _, f := range []string{"importPorts", "exportPorts"} {
			var r struct{ First, Last int32 }
			if err := json.Unmarshal(props[f].Default.Raw, &r); err != nil {
				t.Fatalf("%s default: %v", f, err)
			}
			out = append(out, fmt.Sprintf("%d-%d", r.First, r.Last))
		}
		return append(out, string(props["peerPort"].Default.Raw)+"-"+string(props["peerPort"].Default.Raw))
	}
	t.Fatal("the Site CRD has no storage version")
	return nil
}

// The gateway policy admits the pods of the cluster to the gateway's import and export ports, and the controller to
// its peer port. With site.create true, the chart takes them from site.spec.gateway, and the Site CRD's defaults where
// it sets none. With site.create false, from networkPolicy.gateway.ports, and it fails to render without all three
// (RFC0007 requirement 28; issue #64).
func TestGatewayPolicyPorts(t *testing.T) {
	pods := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{}}
	controller := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
		"app.kubernetes.io/name": "strait", "app.kubernetes.io/component": "controller"}}}
	defaults := siteGatewayDefaults(t)
	if !slices.Equal(defaults, []string{"15000-15999", "30901-31899", "14999-14999"}) {
		t.Fatalf("Site CRD defaults %v; update this test with RFC0005's Ports", defaults)
	}
	set := []string{
		"networkPolicy.gateway.ports.exportPorts.first=40000", "networkPolicy.gateway.ports.exportPorts.last=40099",
		"networkPolicy.gateway.ports.importPorts.first=16000", "networkPolicy.gateway.ports.importPorts.last=16000",
		"networkPolicy.gateway.ports.peerPort=14000", "networkPolicy.gateway.ports.controlPort=40100",
	}
	for _, c := range []struct {
		name            string
		set             []string
		pods, peerPorts []string
	}{
		{"site.create true, the CRD's defaults", nil, defaults[:2], defaults[2:]},
		{"site.create true, site.spec.gateway", []string{
			"site.spec.gateway.exportPorts.first=40000", "site.spec.gateway.exportPorts.last=40099",
			"site.spec.gateway.importPorts.first=16000", "site.spec.gateway.importPorts.last=16000",
			"site.spec.gateway.peerPort=14000",
		}, []string{"16000-16000", "40000-40099"}, []string{"14000-14000"}},
		{"site.create true, one field of site.spec.gateway", []string{"site.spec.gateway.peerPort=14000"},
			defaults[:2], []string{"14000-14000"}},
		{"site.create false, networkPolicy.gateway.ports", append([]string{"site.create=false"}, set...),
			[]string{"16000-16000", "40000-40099"}, []string{"14000-14000"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := render(t, append([]string{"networkPolicy.gateway.enabled=true"}, c.set...)...)
			g := r.networkPolicies["strait-gateway"]
			if got := podPorts(t, g, pods); !slices.Equal(got, c.pods) {
				t.Errorf("pods of the cluster reach %v, want %v", got, c.pods)
			}
			if got := podPorts(t, g, controller); !slices.Equal(got, c.peerPorts) {
				t.Errorf("the controller reaches %v, want %v", got, c.peerPorts)
			}
			for _, rule := range g.Spec.Ingress {
				if len(rule.Ports) == 0 && !slices.ContainsFunc(rule.From, func(p networkingv1.NetworkPolicyPeer) bool { return p.IPBlock != nil }) {
					t.Errorf("rule %+v admits every port", rule)
				}
			}
		})
	}

	for _, c := range []struct {
		name string
		set  []string
		want string
	}{
		{"site.create false, no ports", []string{"site.create=false"}, "networkPolicy.gateway.ports.controlPort is required"},
		{"site.create false, no peerPort", append([]string{"site.create=false", set[5]}, set[:4]...), "networkPolicy.gateway.ports.peerPort is required"},
		{"site.create false, no controlPort", append([]string{"site.create=false"}, set[:5]...), "networkPolicy.gateway.ports.controlPort is required"},
		{"site.create false, a range without last", []string{"site.create=false", set[0], set[2], set[3], set[4], set[5]}, "networkPolicy.gateway.ports.exportPorts needs first and last"},
		{"site.create true, networkPolicy.gateway.ports", set, "networkPolicy.gateway.ports applies with site.create false only"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := helmTemplate(t, append([]string{"networkPolicy.gateway.enabled=true"}, c.set...)...)
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("helm template: %v, %s; want a failure with %q", err, out, c.want)
			}
		})
	}

	// The controller's rule selects the controller's pods, so that its polls of peers pass.
	r := render(t, "networkPolicy.gateway.enabled=true")
	labels := r.pods["strait-controller"].Labels
	for k, v := range controller.PodSelector.MatchLabels {
		if labels[k] != v {
			t.Errorf("the controller pod has labels %v; the gateway policy selects it by %v", labels, controller.PodSelector.MatchLabels)
		}
	}

	// Without the gateway policy, the ports are not needed.
	if r := render(t, "site.create=false"); len(r.networkPolicies) != 1 {
		t.Errorf("site.create false without the gateway policy: policies %v, want the controller's", r.networkPolicies)
	}
}

// The peer gateways reach the control port and the export ports over TCP, and the WireGuard listen ports of
// networkPolicy.gateway.wireGuardPorts over UDP, nothing else. The control port comes from site.spec.gateway, or from
// networkPolicy.gateway.ports with site.create false. With peerCIDRs, the chart fails to render while wireGuardPorts
// is unset; an empty list is a site with no wireguard link (RFC0007 requirement 28; issue #64).
func TestGatewayPolicyPeers(t *testing.T) {
	peers := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "192.0.2.21/32"}}
	rule := func(r rendered) []string {
		t.Helper()
		var out []string
		for _, rule := range r.networkPolicies["strait-gateway"].Spec.Ingress {
			if len(rule.From) != 1 || !reflect.DeepEqual(rule.From[0], peers) {
				continue
			}
			for _, p := range rule.Ports {
				proto := "TCP"
				if p.Protocol != nil {
					proto = string(*p.Protocol)
				}
				last := p.Port.IntVal
				if p.EndPort != nil {
					last = *p.EndPort
				}
				out = append(out, fmt.Sprintf("%s %d-%d", proto, p.Port.IntVal, last))
			}
			if len(rule.Ports) == 0 {
				out = append(out, "every port")
			}
		}
		return out
	}
	on := []string{"networkPolicy.gateway.enabled=true", "networkPolicy.gateway.peerCIDRs={192.0.2.21/32}"}
	for _, c := range []struct {
		name string
		set  []string
		want []string
	}{
		{"the CRD's defaults, two listen ports", []string{"networkPolicy.gateway.wireGuardPorts={31820,32100}"},
			[]string{"TCP 30900-30900", "TCP 30901-31899", "UDP 31820-31820", "UDP 32100-32100"}},
		{"no wireguard link", []string{"networkPolicy.gateway.wireGuardPorts={}"}, []string{"TCP 30900-30900", "TCP 30901-31899"}},
		{"site.spec.gateway", []string{"networkPolicy.gateway.wireGuardPorts={31820}", "site.spec.gateway.controlPort=40100",
			"site.spec.gateway.exportPorts.first=40000", "site.spec.gateway.exportPorts.last=40099"},
			[]string{"TCP 40100-40100", "TCP 40000-40099", "UDP 31820-31820"}},
		{"site.create false", []string{"networkPolicy.gateway.wireGuardPorts={31820}", "site.create=false",
			"networkPolicy.gateway.ports.controlPort=40100", "networkPolicy.gateway.ports.peerPort=14000",
			"networkPolicy.gateway.ports.exportPorts.first=40000", "networkPolicy.gateway.ports.exportPorts.last=40099",
			"networkPolicy.gateway.ports.importPorts.first=16000", "networkPolicy.gateway.ports.importPorts.last=16999"},
			[]string{"TCP 40100-40100", "TCP 40000-40099", "UDP 31820-31820"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rule(render(t, append(slices.Clone(on), c.set...)...)); !slices.Equal(got, c.want) {
				t.Errorf("the peers reach %v, want %v", got, c.want)
			}
		})
	}

	// A values file's empty list, as the README tells, renders as no listen port.
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte("networkPolicy: {gateway: {enabled: true, peerCIDRs: [192.0.2.21/32], wireGuardPorts: []}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := helmTemplateArgs(t, "--values", file)
	if err != nil || !strings.Contains(out, "{port: 30900}") || strings.Contains(out, "UDP") {
		t.Errorf("wireGuardPorts: [] in a values file: %v, %s", err, out)
	}

	out, err = helmTemplate(t, on...)
	if err == nil || !strings.Contains(out, "networkPolicy.gateway.wireGuardPorts is required with networkPolicy.gateway.peerCIDRs") {
		t.Errorf("peerCIDRs without wireGuardPorts: %v, %s; want a failure", err, out)
	}
	// Without peerCIDRs, no rule for peers, and no need of wireGuardPorts.
	if got := rule(render(t, "networkPolicy.gateway.enabled=true")); got != nil {
		t.Errorf("without peerCIDRs, the peers reach %v", got)
	}
}
