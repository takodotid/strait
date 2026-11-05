package helm

import (
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// gatewayLabels select the gateway pods.
var gatewayLabels = map[string]string{"app.kubernetes.io/name": "strait", "app.kubernetes.io/component": "gateway"}

// The gateway's PodDisruptionBudget lets a drain evict one gateway pod at a time, and a pod that is not ready at any
// time, so a ready standby stays for the Lease and one replica never blocks a drain. It is on by default
// (issue #30, row 20).
func TestGatewayDisruptionBudget(t *testing.T) {
	r := render(t)
	pdb, ok := r.disruptionBudgets["strait-gateway"]
	if !ok {
		t.Fatalf("no PodDisruptionBudget strait-gateway by default; have %v", r.disruptionBudgets)
	}
	if pdb.Namespace != namespace || pdb.Spec.MinAvailable != nil || !reflect.DeepEqual(pdb.Spec.MaxUnavailable, ptr.To(intstr.FromInt32(1))) ||
		ptr.Deref(pdb.Spec.UnhealthyPodEvictionPolicy, "") != policyv1.AlwaysAllow {
		t.Errorf("PodDisruptionBudget = %+v, want maxUnavailable 1 and unhealthyPodEvictionPolicy AlwaysAllow in %s", pdb.Spec, namespace)
	}
	if pdb.Spec.Selector == nil || !reflect.DeepEqual(pdb.Spec.Selector.MatchLabels, gatewayLabels) {
		t.Errorf("selector = %+v, want %v", pdb.Spec.Selector, gatewayLabels)
	}
	for k, v := range gatewayLabels {
		if r.pods["strait-gateway"].Labels[k] != v {
			t.Errorf("the gateway pods lack the label %s=%s that the budget selects", k, v)
		}
	}
	if len(r.disruptionBudgets) != 1 {
		t.Errorf("PodDisruptionBudgets %v, want the gateway's only: the controller runs one replica", r.disruptionBudgets)
	}
	if r := render(t, "gateway.podDisruptionBudget.enabled=false"); len(r.disruptionBudgets) != 0 {
		t.Errorf("disabled: PodDisruptionBudgets %v, want none", r.disruptionBudgets)
	}
}

// Each pod takes its priorityClassName, nodeSelector, tolerations and affinity from the values. With none set, the
// pods are as before: no priority class, node selector or tolerations, and the gateway's preferred anti-affinity
// (issue #30, row 20).
func TestScheduling(t *testing.T) {
	r := render(t)
	for name, pod := range r.pods {
		s := pod.Spec
		if s.PriorityClassName != "" || s.NodeSelector != nil || s.Tolerations != nil {
			t.Errorf("%s by default: priorityClassName %q, nodeSelector %v, tolerations %v; want none", name, s.PriorityClassName, s.NodeSelector, s.Tolerations)
		}
	}
	if a := r.pods["strait-controller"].Spec.Affinity; a != nil {
		t.Errorf("controller affinity by default = %+v, want none", a)
	}
	anti := r.pods["strait-gateway"].Spec.Affinity
	if anti == nil || anti.PodAntiAffinity == nil || len(anti.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 ||
		len(anti.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Fatalf("gateway affinity by default = %+v, want one preferred anti-affinity term", anti)
	}
	if term := anti.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm; term.TopologyKey != "kubernetes.io/hostname" ||
		!reflect.DeepEqual(term.LabelSelector.MatchLabels, gatewayLabels) {
		t.Errorf("preferred term = %+v", term)
	}

	r = render(t, "gateway.antiAffinity=required")
	anti = r.pods["strait-gateway"].Spec.Affinity
	if anti == nil || anti.PodAntiAffinity == nil || len(anti.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 ||
		len(anti.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Fatalf("gateway affinity with antiAffinity required = %+v, want one required term", anti)
	}
	if term := anti.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]; term.TopologyKey != "kubernetes.io/hostname" ||
		!reflect.DeepEqual(term.LabelSelector.MatchLabels, gatewayLabels) {
		t.Errorf("required term = %+v", term)
	}

	for _, component := range []string{"controller", "gateway"} {
		pod := "strait-" + component
		r := render(t,
			component+".priorityClassName=strait-critical",
			component+".nodeSelector.role=edge",
			component+".tolerations[0].key=dedicated",
			component+".tolerations[0].operator=Exists",
			component+".affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=zone",
			component+".affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists",
		)
		s := r.pods[pod].Spec
		if s.PriorityClassName != "strait-critical" || !reflect.DeepEqual(s.NodeSelector, map[string]string{"role": "edge"}) ||
			!reflect.DeepEqual(s.Tolerations, []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}) {
			t.Errorf("%s: priorityClassName %q, nodeSelector %v, tolerations %v", pod, s.PriorityClassName, s.NodeSelector, s.Tolerations)
		}
		if s.Affinity == nil || s.Affinity.NodeAffinity == nil || s.Affinity.PodAntiAffinity != nil {
			t.Errorf("%s: affinity %+v, want the values' node affinity only: it replaces the chart's", pod, s.Affinity)
		}
		// The other pod keeps its defaults.
		other := "strait-gateway"
		if component == "gateway" {
			other = "strait-controller"
		}
		if o := r.pods[other].Spec; o.PriorityClassName != "" || o.NodeSelector != nil {
			t.Errorf("%s takes %s's values", other, pod)
		}
	}
}

// A value of gateway.antiAffinity other than preferred or required fails the render.
func TestAntiAffinityValue(t *testing.T) {
	out, err := helmTemplate(t, "gateway.antiAffinity=soft")
	if err == nil || !strings.Contains(out, "gateway.antiAffinity must be preferred or required") {
		t.Errorf("helm template with antiAffinity soft: %v, %s; want the chart's failure", err, out)
	}
}

// The pre-pull Job runs before an upgrade or a rollback, never at install, with the controller's image and its
// scheduling values. With no controller affinity set, it prefers the node of the running controller pod. It fails
// within its deadline, and is off with controller.prePull.enabled false (issue #30, row 15).
func TestPrePull(t *testing.T) {
	out, err := helmTemplate(t, "controller.nodeSelector.zone=a", "controller.prePull.deadlineSeconds=120")
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, out)
	}
	_, after, ok := strings.Cut(out, "\nkind: Job\n")
	if !ok {
		t.Fatal("no Job rendered")
	}
	var job batchv1.Job
	doc, _, _ := strings.Cut(after, "\n---")
	if err := yaml.UnmarshalStrict([]byte("kind: Job\n"+doc), &job); err != nil {
		t.Fatal(err)
	}
	if got := job.Annotations["helm.sh/hook"]; got != "pre-upgrade,pre-rollback" {
		t.Errorf("hook %q, want pre-upgrade,pre-rollback", got)
	}
	r := render(t, "controller.nodeSelector.zone=a")
	controller, pod := r.pods["strait-controller"].Spec, job.Spec.Template.Spec
	if pod.Containers[0].Image != controller.Containers[0].Image || pod.Containers[0].ImagePullPolicy != controller.Containers[0].ImagePullPolicy {
		t.Errorf("image %s %s, want the controller's, %s %s", pod.Containers[0].Image, pod.Containers[0].ImagePullPolicy, controller.Containers[0].Image, controller.Containers[0].ImagePullPolicy)
	}
	if !reflect.DeepEqual(pod.NodeSelector, controller.NodeSelector) || pod.RestartPolicy != corev1.RestartPolicyNever ||
		ptr.Deref(job.Spec.BackoffLimit, -1) != 0 || ptr.Deref(job.Spec.ActiveDeadlineSeconds, 0) != 120 {
		t.Errorf("job = %+v", job.Spec)
	}
	terms := pod.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 || terms[0].PodAffinityTerm.TopologyKey != "kubernetes.io/hostname" ||
		!reflect.DeepEqual(terms[0].PodAffinityTerm.LabelSelector.MatchLabels, map[string]string{"app.kubernetes.io/name": "strait", "app.kubernetes.io/component": "controller"}) {
		t.Errorf("affinity = %+v, want a preferred one to the controller pod", pod.Affinity)
	}
	if a := render(t, "controller.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=zone",
		"controller.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists").pods; !reflect.DeepEqual(a["strait-pre-pull"].Spec.Affinity, a["strait-controller"].Spec.Affinity) {
		t.Errorf("with controller.affinity: pre-pull affinity %+v, want the controller's %+v", a["strait-pre-pull"].Spec.Affinity, a["strait-controller"].Spec.Affinity)
	}
	if _, ok := render(t, "controller.prePull.enabled=false").pods["strait-pre-pull"]; ok {
		t.Error("controller.prePull.enabled false renders the Job")
	}
}
