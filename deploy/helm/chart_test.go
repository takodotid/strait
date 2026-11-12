// Package helm tests the rendered chart against RFC0007.
package helm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const namespace = "strait-system"

// rendered is the chart as helm template renders it.
type rendered struct {
	clusterRoles        map[string]rbacv1.ClusterRole
	roles               map[string]rbacv1.Role
	clusterRoleBindings []rbacv1.ClusterRoleBinding
	roleBindings        []rbacv1.RoleBinding
	serviceAccounts     map[string]corev1.ServiceAccount
	pods                map[string]corev1.PodTemplateSpec
	disruptionBudgets   map[string]policyv1.PodDisruptionBudget
	networkPolicies     map[string]networkingv1.NetworkPolicy
}

// helmTemplate runs helm template on the chart with the values given. It returns the output, or on a failure the
// error output.
func helmTemplate(t *testing.T, set ...string) (string, error) {
	t.Helper()
	return helmTemplateChart(t, "strait", set...)
}

// helmTemplateChart runs helm template on the chart in the directory given.
func helmTemplateChart(t *testing.T, chart string, set ...string) (string, error) {
	t.Helper()
	var args []string
	for _, s := range set {
		args = append(args, "--set", s)
	}
	return helmTemplateRaw(t, chart, args...)
}

// helmTemplateArgs runs helm template on the chart with more arguments, such as --values FILE.
func helmTemplateArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return helmTemplateRaw(t, "strait", args...)
}

// helmTemplateRaw runs helm template on the chart in the directory given, with the arguments given.
func helmTemplateRaw(t *testing.T, chart string, extra ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not installed; CI must run this test")
		}
		t.Skip("helm is not installed")
	}
	// helm reads the chart in another process. Reading it here too makes go test's cache see a change to it.
	if err := filepath.WalkDir("strait", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		_, err = os.ReadFile(path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"template", "strait", chart, "--namespace", namespace, "--set", "site.name=a"}, extra...)
	cmd := exec.Command("helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return stderr.String(), err
	}
	return string(out), nil
}

// render runs helm template on the chart with the values given, and decodes what the tests read.
func render(t *testing.T, set ...string) rendered {
	t.Helper()
	return renderChart(t, "strait", set...)
}

// renderChart is render on the chart in the directory given.
func renderChart(t *testing.T, chart string, set ...string) rendered {
	t.Helper()
	out, err := helmTemplateChart(t, chart, set...)
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, out)
	}
	r := rendered{
		clusterRoles:      map[string]rbacv1.ClusterRole{},
		roles:             map[string]rbacv1.Role{},
		serviceAccounts:   map[string]corev1.ServiceAccount{},
		pods:              map[string]corev1.PodTemplateSpec{},
		disruptionBudgets: map[string]policyv1.PodDisruptionBudget{},
		networkPolicies:   map[string]networkingv1.NetworkPolicy{},
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(out)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var head struct{ Kind string }
		if err := yaml.Unmarshal(doc, &head); err != nil {
			t.Fatal(err)
		}
		decode := func(v any) {
			if err := yaml.UnmarshalStrict(doc, v); err != nil {
				t.Fatalf("%s: %v", head.Kind, err)
			}
		}
		switch head.Kind {
		case "ClusterRole":
			var o rbacv1.ClusterRole
			decode(&o)
			r.clusterRoles[o.Name] = o
		case "Role":
			var o rbacv1.Role
			decode(&o)
			r.roles[o.Namespace+"/"+o.Name] = o
		case "ClusterRoleBinding":
			var o rbacv1.ClusterRoleBinding
			decode(&o)
			r.clusterRoleBindings = append(r.clusterRoleBindings, o)
		case "RoleBinding":
			var o rbacv1.RoleBinding
			decode(&o)
			r.roleBindings = append(r.roleBindings, o)
		case "ServiceAccount":
			var o corev1.ServiceAccount
			decode(&o)
			r.serviceAccounts[o.Name] = o
		case "Deployment":
			var o appsv1.Deployment
			decode(&o)
			r.pods[o.Name] = o.Spec.Template
		case "Job":
			var o batchv1.Job
			decode(&o)
			r.pods[o.Name] = o.Spec.Template
		case "PodDisruptionBudget":
			var o policyv1.PodDisruptionBudget
			decode(&o)
			r.disruptionBudgets[o.Name] = o
		case "NetworkPolicy":
			var o networkingv1.NetworkPolicy
			decode(&o)
			r.networkPolicies[o.Name] = o
		}
	}
	return r
}

// grant is one verb on one resource, by scope: the cluster, or a namespace. Names is the rule's resourceNames, joined.
type grant struct {
	cluster   bool
	namespace string
	group     string
	resource  string
	verb      string
	names     string
}

// grants returns what the roles bound to the service account allow. It fails on a binding to a role the chart does
// not render.
func (r rendered) grants(t *testing.T, serviceAccount string) []grant {
	t.Helper()
	bound := func(subjects []rbacv1.Subject) bool {
		return slices.ContainsFunc(subjects, func(s rbacv1.Subject) bool {
			return s.Kind == "ServiceAccount" && s.Name == serviceAccount && s.Namespace == namespace
		})
	}
	var out []grant
	add := func(cluster bool, ns string, rules []rbacv1.PolicyRule) {
		for _, rule := range rules {
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					for _, v := range rule.Verbs {
						out = append(out, grant{cluster, ns, g, res, v, strings.Join(rule.ResourceNames, ",")})
					}
				}
			}
		}
	}
	for _, b := range r.clusterRoleBindings {
		if !bound(b.Subjects) {
			continue
		}
		role, ok := r.clusterRoles[b.RoleRef.Name]
		if b.RoleRef.Kind != "ClusterRole" || !ok {
			t.Fatalf("ClusterRoleBinding %s binds %s %s, which the chart does not render", b.Name, b.RoleRef.Kind, b.RoleRef.Name)
		}
		add(true, "", role.Rules)
	}
	for _, b := range r.roleBindings {
		if !bound(b.Subjects) {
			continue
		}
		role, ok := r.roles[b.Namespace+"/"+b.RoleRef.Name]
		if b.RoleRef.Kind != "Role" || !ok {
			t.Fatalf("RoleBinding %s/%s binds %s %s, which the chart does not render in %s", b.Namespace, b.Name, b.RoleRef.Kind, b.RoleRef.Name, b.Namespace)
		}
		add(false, b.Namespace, role.Rules)
	}
	return sortGrants(out)
}

func sortGrants(g []grant) []grant {
	slices.SortFunc(g, func(a, b grant) int {
		return strings.Compare(a.String(), b.String())
	})
	return slices.Compact(g)
}

func (g grant) String() string {
	scope := "namespace " + g.namespace
	if g.cluster {
		scope = "cluster"
	}
	group := g.group
	if group == "" {
		group = "core"
	}
	s := scope + " " + group + " " + g.resource + " " + g.verb
	if g.names != "" {
		s += " [" + g.names + "]"
	}
	return s
}

// rfcTable returns the rows of the first table after the heading, as cells with backticks removed.
func rfcTable(t *testing.T, heading string) [][]string {
	t.Helper()
	data, err := os.ReadFile("../../rfcs/RFC0007.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "\n"+heading+"\n")
	if !ok {
		t.Fatalf("RFC0007 has no heading %q", heading)
	}
	var rows [][]string
	for _, line := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		var cells []string
		for _, c := range strings.Split(strings.Trim(line, "|"), "|") {
			cells = append(cells, strings.TrimSpace(strings.ReplaceAll(c, "`", "")))
		}
		rows = append(rows, cells)
	}
	if len(rows) < 3 {
		t.Fatalf("RFC0007 %q: no table", heading)
	}
	// The header and the delimiter row.
	return rows[2:]
}

// values are the chart values a render was made with.
type values struct {
	cnpg   bool
	secret string
	// namespaces is controller.namespaces.
	namespaces []string
	// namespacedReads is controller.namespacedReads.
	namespacedReads bool
}

// wanted returns the grants that an RBAC table of RFC0007 gives for the values.
func wanted(t *testing.T, heading string, v values) []grant {
	t.Helper()
	var out []grant
	for _, row := range rfcTable(t, heading) {
		if len(row) != 5 {
			t.Fatalf("%s: row %v: want 5 cells", heading, row)
		}
		group := row[0]
		if group == "core" {
			group = ""
		}
		var cluster bool
		var names string
		scopes := []string{namespace}
		switch row[3] {
		case "Cluster":
			cluster = true
		case "Cluster, or each namespace of controller.namespaces and its namespace with controller.namespacedReads":
			if v.namespacedReads {
				scopes = append(slices.Clone(v.namespaces), namespace)
			} else {
				cluster = true
			}
		case "Cluster while cnpg.enabled, or each namespace of controller.namespaces and its namespace with controller.namespacedReads":
			if !v.cnpg {
				continue
			}
			if v.namespacedReads {
				scopes = append(slices.Clone(v.namespaces), namespace)
			} else {
				cluster = true
			}
		case "Cluster, or each namespace of controller.namespaces and its namespace":
			if len(v.namespaces) == 0 {
				cluster = true
			} else {
				scopes = append(slices.Clone(v.namespaces), namespace)
			}
		case "Its namespace":
		case "Its namespace, strait-gateway only":
			names = "strait-gateway"
		case "Its namespace, strait-controller only":
			names = "strait-controller"
		case "Its namespace, the WireGuard Secret only":
			if v.secret == "" {
				continue
			}
			names = v.secret
		default:
			t.Fatalf("%s: scope %q is not one this test knows", heading, row[3])
		}
		if cluster {
			scopes = []string{""}
		}
		for _, ns := range scopes {
			for _, res := range strings.Split(row[1], ",") {
				for _, verb := range strings.Split(row[2], ",") {
					out = append(out, grant{cluster, ns, group, strings.TrimSpace(res), strings.TrimSpace(verb), names})
				}
			}
		}
	}
	return sortGrants(out)
}

func diffGrants(t *testing.T, who string, got, want []grant) {
	t.Helper()
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("%s: the chart grants %s, which RFC0007 does not list", who, g)
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s: RFC0007 lists %s, which the chart does not grant", who, w)
		}
	}
}

// The controller's and the gateway agent's RBAC are exactly RFC0007's tables (requirements 1 and 2). The rules for
// workload resources are read-only (RFC0001 requirement 1, RFC0003 requirement 5).
func TestRBAC(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  []string
		v    values
	}{
		{"defaults", nil, values{cnpg: true}},
		{"cnpg disabled", []string{"cnpg.enabled=false"}, values{}},
		{"wireguard secret", []string{"site.spec.wireguard.privateKeySecretRef.name=strait-wg"}, values{cnpg: true, secret: "strait-wg"}},
		{"namespaces", []string{"controller.namespaces={db,web}"}, values{cnpg: true, namespaces: []string{"db", "web"}}},
		{"namespaces with its own", []string{"controller.namespaces={db," + namespace + "}"}, values{cnpg: true, namespaces: []string{"db"}}},
		{"namespaced reads", []string{"controller.namespaces={db,web}", "controller.namespacedReads=true"},
			values{cnpg: true, namespaces: []string{"db", "web"}, namespacedReads: true}},
		{"namespaced reads, cnpg disabled", []string{"controller.namespaces={db}", "controller.namespacedReads=true", "cnpg.enabled=false"},
			values{namespaces: []string{"db"}, namespacedReads: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := render(t, tt.set...)
			diffGrants(t, "controller", r.grants(t, "strait-controller"), wanted(t, "## Controller RBAC", tt.v))
			gateway := r.grants(t, "strait-gateway")
			diffGrants(t, "gateway agent", gateway, wanted(t, "## Gateway agent RBAC", tt.v))
			for _, g := range r.grants(t, "strait-controller") {
				if g.group == "postgresql.cnpg.io" && !slices.Contains([]string{"get", "list", "watch"}, g.verb) {
					t.Errorf("controller: %s is not read-only", g)
				}
			}
		})
	}
}

// container is one row of RFC0007's Containers table, and where the chart runs it.
var containers = map[string]struct{ pod, container string }{
	"controller":    {"strait-controller", "controller"},
	"gateway agent": {"strait-gateway", "agent"},
	"Envoy":         {"strait-gateway", "envoy"},
	"pre-pull":      {"strait-pre-pull", "pre-pull"},
}

// tokenPath is where a container finds a Kubernetes service account token.
const tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount"

// Every container drops all capabilities and has a read-only root file system; user, added capabilities and token
// follow RFC0007's Containers table (requirements 3 and 4).
func TestContainers(t *testing.T) {
	r := render(t, "site.spec.wireguard.privateKeySecretRef.name=strait-wg")
	rows := rfcTable(t, "## Containers")
	if len(rows) != len(containers) {
		t.Fatalf("RFC0007 lists %d containers, the test knows %d", len(rows), len(containers))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		where, ok := containers[row[0]]
		if !ok || len(row) != 4 {
			t.Fatalf("container row %v is not one this test knows", row)
		}
		pod, ok := r.pods[where.pod]
		if !ok {
			t.Fatalf("no pod %s", where.pod)
		}
		i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == where.container })
		if i < 0 {
			t.Fatalf("pod %s has no container %s", where.pod, where.container)
		}
		seen[where.pod+"/"+where.container] = true
		c := pod.Spec.Containers[i]
		sc := c.SecurityContext
		name := row[0]
		if sc == nil || sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Errorf("%s: does not drop ALL capabilities", name)
			continue
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: root file system is not read-only", name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: allows privilege escalation", name)
		}

		runAsNonRoot := sc.RunAsNonRoot
		if runAsNonRoot == nil && pod.Spec.SecurityContext != nil {
			runAsNonRoot = pod.Spec.SecurityContext.RunAsNonRoot
		}
		user := sc.RunAsUser
		if user == nil && pod.Spec.SecurityContext != nil {
			user = pod.Spec.SecurityContext.RunAsUser
		}
		switch {
		case row[1] == "non-root":
			if runAsNonRoot == nil || !*runAsNonRoot || user == nil || *user == 0 {
				t.Errorf("%s: does not run as non-root", name)
			}
		case strings.HasPrefix(row[1], "root"):
			if user == nil || *user != 0 {
				t.Errorf("%s: does not run as root", name)
			}
		default:
			t.Fatalf("%s: user %q is not one this test knows", name, row[1])
		}

		var add []corev1.Capability
		if row[2] != "none" {
			for _, a := range strings.Split(row[2], ",") {
				add = append(add, corev1.Capability(strings.TrimSpace(a)))
			}
		}
		if !slices.Equal(sc.Capabilities.Add, add) {
			t.Errorf("%s: adds capabilities %v, want %v", name, sc.Capabilities.Add, add)
		}

		automount := pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken
		if sa, ok := r.serviceAccounts[pod.Spec.ServiceAccountName]; ok && pod.Spec.AutomountServiceAccountToken == nil && sa.AutomountServiceAccountToken != nil {
			automount = *sa.AutomountServiceAccountToken
		}
		projected := false
		for _, m := range c.VolumeMounts {
			for _, v := range pod.Spec.Volumes {
				if v.Name != m.Name || v.Projected == nil {
					continue
				}
				for _, src := range v.Projected.Sources {
					if src.ServiceAccountToken != nil {
						projected = true
						if m.MountPath != tokenPath {
							t.Errorf("%s: token mounted at %s", name, m.MountPath)
						}
					}
				}
			}
		}
		switch row[3] {
		case "yes":
			if !automount && !projected {
				t.Errorf("%s: has no token", name)
			}
		case "yes, projected":
			if automount || !projected {
				t.Errorf("%s: automount %v, projected token %v; want a projected token only", name, automount, projected)
			}
		case "no":
			// The pod itself sets automountServiceAccountToken: false (requirement 3).
			if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || projected {
				t.Errorf("%s: the pod does not set automountServiceAccountToken: false, or the container mounts a token", name)
			}
		default:
			t.Fatalf("%s: token %q is not one this test knows", name, row[3])
		}
	}
	for name, pod := range r.pods {
		for _, c := range pod.Spec.Containers {
			if !seen[name+"/"+c.Name] {
				t.Errorf("container %s/%s is not in RFC0007's Containers table", name, c.Name)
			}
		}
		if len(pod.Spec.InitContainers) > 0 {
			t.Errorf("pod %s has init containers, which RFC0007's Containers table does not list", name)
		}
	}
}

// The controller watches and reads CloudNativePG's kinds only while the chart grants the read (RFC0003, requirement
// 13).
func TestCNPGFlagFollowsValue(t *testing.T) {
	for _, tt := range []struct {
		set  []string
		want string
	}{
		{nil, "--cnpg=true"},
		{[]string{"cnpg.enabled=false"}, "--cnpg=false"},
	} {
		pod := render(t, tt.set...).pods["strait-controller"]
		var got []string
		for _, a := range pod.Spec.Containers[0].Args {
			if strings.HasPrefix(a, "--cnpg") {
				got = append(got, a)
			}
		}
		if !slices.Equal(got, []string{tt.want}) {
			t.Errorf("%v: controller args %v, want %s", tt.set, got, tt.want)
		}
	}
}

// The Site carries helm.sh/resource-policy: keep, so helm uninstall, and an upgrade with site.create false, leave it in
// place. Helm reads the annotation from the live object.
func TestSiteKept(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, out)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(out)))
	sites := 0
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			Kind     string
			Metadata struct{ Annotations map[string]string }
		}
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatal(err)
		}
		if o.Kind != "Site" {
			continue
		}
		sites++
		if got := o.Metadata.Annotations["helm.sh/resource-policy"]; got != "keep" {
			t.Errorf("Site: helm.sh/resource-policy %q, want keep", got)
		}
	}
	if sites != 1 {
		t.Fatalf("%d Sites rendered, want 1", sites)
	}
}

// The controller refuses the writes that its Roles do not grant, so that status says why (RFC0007, requirement 16).
func TestNamespacesFlagFollowsValue(t *testing.T) {
	for _, tt := range []struct {
		set  []string
		want []string
	}{
		{nil, nil},
		{[]string{"controller.namespaces={db,web}"}, []string{"--namespaces=db,web"}},
	} {
		pod := render(t, tt.set...).pods["strait-controller"]
		var got []string
		for _, a := range pod.Spec.Containers[0].Args {
			if strings.HasPrefix(a, "--namespaces") {
				got = append(got, a)
			}
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%v: controller args %v, want %v", tt.set, got, tt.want)
		}
	}
}

// Envoy's drain time is its default unless envoy.drainTimeSeconds sets it (RFC0005).
func TestDrainTimeFollowsValue(t *testing.T) {
	for _, tt := range []struct {
		set  []string
		want []string
	}{
		{nil, nil},
		{[]string{"envoy.drainTimeSeconds=3"}, []string{"--drain-time-s", "3"}},
	} {
		pod := render(t, tt.set...).pods["strait-gateway"]
		var got []string
		for _, c := range pod.Spec.Containers {
			if c.Name != "envoy" {
				continue
			}
			if i := slices.Index(c.Args, "--drain-time-s"); i >= 0 && i+1 < len(c.Args) {
				got = c.Args[i : i+2]
			}
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%v: envoy args %v, want %v", tt.set, got, tt.want)
		}
	}
}

// The controller reads where its read Roles grant it, and the chart refuses namespacedReads without namespaces
// (RFC0007, requirement 26).
func TestNamespacedReadsFlagFollowsValue(t *testing.T) {
	for _, tt := range []struct {
		set  []string
		want bool
	}{
		{nil, false},
		{[]string{"controller.namespaces={db,web}"}, false},
		{[]string{"controller.namespaces={db,web}", "controller.namespacedReads=true"}, true},
	} {
		pod := render(t, tt.set...).pods["strait-controller"]
		if got := slices.Contains(pod.Spec.Containers[0].Args, "--namespaced-reads"); got != tt.want {
			t.Errorf("%v: --namespaced-reads %v, want %v", tt.set, got, tt.want)
		}
	}
	if out, err := helmTemplate(t, "controller.namespacedReads=true"); err == nil || !strings.Contains(out, "controller.namespacedReads needs controller.namespaces") {
		t.Errorf("namespacedReads without namespaces: %v %s, want a refusal", err, out)
	}
}

// The controller learns its container's memory limit, and sets the Go runtime's soft memory limit from it
// (docs/sizing.md).
func TestControllerMemoryLimit(t *testing.T) {
	pod := render(t).pods["strait-controller"]
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "MEMORY_LIMIT" {
			ref := e.ValueFrom.ResourceFieldRef
			if ref == nil || ref.ContainerName != "controller" || ref.Resource != "limits.memory" || !ref.Divisor.IsZero() {
				t.Errorf("MEMORY_LIMIT from %+v, want the controller's limits.memory in bytes", e.ValueFrom)
			}
			return
		}
	}
	t.Error("the controller has no MEMORY_LIMIT")
}

// releasedChart copies the chart and writes the image digest into it, as make chart does for a release.
func releasedChart(t *testing.T, digest string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "strait")
	if err := os.CopyFS(dir, os.DirFS("strait")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Chart.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The line that make chart replaces.
	const line = "\n  strait.tako.id/image-digest: \"\"\n"
	if n := strings.Count(string(b), line); n != 1 {
		t.Fatalf("Chart.yaml holds the line %q %d times, want once: make chart writes the digest there", line, n)
	}
	b = []byte(strings.Replace(string(b), line, "\n  strait.tako.id/image-digest: \""+digest+"\"\n", 1))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A released chart names the image by the digest its release pushed, while image.tag and image.digest are empty. A
// values file of an earlier release that sets image.tag keeps its meaning (RFC0007, requirement 21).
func TestImageDigest(t *testing.T) {
	released := "sha256:" + strings.Repeat("a", 64)
	own := "sha256:" + strings.Repeat("b", 64)
	repo := "ghcr.io/takodotid/strait"
	for _, tt := range []struct {
		name     string
		released bool
		set      []string
		want     string
	}{
		{"repository chart", false, nil, repo + ":dev"},
		{"repository chart, digest", false, []string{"image.digest=" + own}, repo + ":dev@" + own},
		{"released chart", true, nil, repo + ":dev@" + released},
		{"released chart, tag", true, []string{"image.tag=1.2.1"}, repo + ":1.2.1"},
		{"released chart, digest", true, []string{"image.digest=" + own}, repo + ":dev@" + own},
		{"released chart, tag and digest", true, []string{"image.tag=1.2.1", "image.digest=" + own}, repo + ":1.2.1@" + own},
		{"released chart, mirror", true, []string{"image.repository=mirror.example/strait"}, "mirror.example/strait:dev@" + released},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chart := "strait"
			if tt.released {
				chart = releasedChart(t, released)
			}
			r := renderChart(t, chart, tt.set...)
			var images []string
			for _, pod := range r.pods {
				for _, c := range pod.Spec.Containers {
					if c.Name != "envoy" && !slices.Contains(images, c.Image) {
						images = append(images, c.Image)
					}
				}
			}
			if !slices.Equal(images, []string{tt.want}) {
				t.Errorf("images %v, want %s", images, tt.want)
			}
			if len(r.pods) < 3 {
				t.Errorf("%d pods rendered, want the controller, the gateway and the pre-pull Job", len(r.pods))
			}
		})
	}
	out, err := helmTemplate(t, "image.digest=sha256:abc")
	if err == nil || !strings.Contains(out, `image digest "sha256:abc"`) {
		t.Errorf("a malformed digest: %v: %s, want a failure that names it", err, out)
	}
}
