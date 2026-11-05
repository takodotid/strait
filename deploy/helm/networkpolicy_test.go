package helm

import (
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
// the gateway pods is off by default; when on, it admits every source to the probes, every pod of the cluster, and
// networkPolicy.gateway.peerCIDRs (RFC0007; issue #30, row 39).
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

	r = render(t, "networkPolicy.gateway.enabled=true", "networkPolicy.gateway.peerCIDRs={192.0.2.21/32,2001:db8::/64}")
	g, ok := r.networkPolicies["strait-gateway"]
	if !ok {
		t.Fatal("networkPolicy.gateway.enabled: no policy on the gateway pods")
	}
	if !reflect.DeepEqual(g.Spec.PodSelector.MatchLabels, gatewayLabels) || !slices.Equal(g.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) {
		t.Errorf("gateway policy selects %v, types %v", g.Spec.PodSelector.MatchLabels, g.Spec.PolicyTypes)
	}
	probes := intstr.FromInt32(8090)
	wantRules := []networkingv1.NetworkPolicyIngressRule{
		{Ports: []networkingv1.NetworkPolicyPort{{Port: &probes}}},
		{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}},
		{From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "192.0.2.21/32"}}, {IPBlock: &networkingv1.IPBlock{CIDR: "2001:db8::/64"}}}},
	}
	if !reflect.DeepEqual(g.Spec.Ingress, wantRules) {
		t.Errorf("gateway policy rules:\n%+v\nwant\n%+v", g.Spec.Ingress, wantRules)
	}

	if r := render(t, "networkPolicy.enabled=false"); len(r.networkPolicies) != 0 {
		t.Errorf("networkPolicy.enabled false: policies %v, want none", r.networkPolicies)
	}
}

// The gateway agent's probe server binds to the pod's own address, so a peer cannot reach it through a WireGuard
// interface (RFC0007, requirement 12).
func TestAgentProbeBindsThePodAddress(t *testing.T) {
	pod := render(t).pods["strait-gateway"]
	i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == "agent" })
	if i < 0 {
		t.Fatal("no agent container")
	}
	agent := pod.Spec.Containers[i]
	if !slices.Contains(agent.Args, "--probe-ip=$(POD_IP)") {
		t.Errorf("agent args %v, want --probe-ip=$(POD_IP)", agent.Args)
	}
	j := slices.IndexFunc(agent.Env, func(e corev1.EnvVar) bool { return e.Name == "POD_IP" })
	if j < 0 || agent.Env[j].ValueFrom == nil || agent.Env[j].ValueFrom.FieldRef == nil || agent.Env[j].ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Errorf("agent env %+v, want POD_IP from status.podIP", agent.Env)
	}
}
