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
	args := []string{"template", "strait", "strait", "--namespace", namespace, "--set", "site.name=a"}
	for _, s := range set {
		args = append(args, "--set", s)
	}
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
	out, err := helmTemplate(t, set...)
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
			r.roles[o.Name] = o
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

// grant is one verb on one resource, by scope. Names is the rule's resourceNames, joined.
type grant struct {
	cluster  bool
	group    string
	resource string
	verb     string
	names    string
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
	add := func(cluster bool, rules []rbacv1.PolicyRule) {
		for _, rule := range rules {
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					for _, v := range rule.Verbs {
						out = append(out, grant{cluster, g, res, v, strings.Join(rule.ResourceNames, ",")})
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
		add(true, role.Rules)
	}
	for _, b := range r.roleBindings {
		if !bound(b.Subjects) {
			continue
		}
		role, ok := r.roles[b.RoleRef.Name]
		if b.RoleRef.Kind != "Role" || !ok || b.Namespace != namespace || role.Namespace != namespace {
			t.Fatalf("RoleBinding %s binds %s %s, which the chart does not render in %s", b.Name, b.RoleRef.Kind, b.RoleRef.Name, namespace)
		}
		add(false, role.Rules)
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
	scope := "namespace"
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
		switch row[3] {
		case "Cluster":
			cluster = true
		case "Cluster, while cnpg.enabled":
			if !v.cnpg {
				continue
			}
			cluster = true
		case "Its namespace":
		case "Its namespace, strait-gateway only":
			names = "strait-gateway"
		case "Its namespace, the WireGuard Secret only":
			if v.secret == "" {
				continue
			}
			names = v.secret
		default:
			t.Fatalf("%s: scope %q is not one this test knows", heading, row[3])
		}
		for _, res := range strings.Split(row[1], ",") {
			for _, verb := range strings.Split(row[2], ",") {
				out = append(out, grant{cluster, group, strings.TrimSpace(res), strings.TrimSpace(verb), names})
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
