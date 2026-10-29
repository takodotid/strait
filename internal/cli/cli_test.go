package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/version"
)

var now = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

func ago(d time.Duration) *metav1.Time {
	t := metav1.NewTime(now.Add(-d))
	return &t
}

// cluster is the state of site a that most tests read.
func cluster() []client.Object {
	return []client.Object{
		&straitv1.Site{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Status: straitv1.SiteStatus{
				Gateway:            straitv1.GatewayStatus{ActivePod: "strait-gateway-0", Address: "10.0.0.15"},
				WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				ControllerVersion:  "v1.0.0",
			},
		},
		&straitv1.SitePeer{
			ObjectMeta: metav1.ObjectMeta{Name: "c"},
			Status:     straitv1.SitePeerStatus{LastSeenTime: ago(5 * time.Minute), LastError: "poll: context deadline exceeded"},
		},
		&straitv1.SitePeer{
			ObjectMeta: metav1.ObjectMeta{Name: "b"},
			Status:     straitv1.SitePeerStatus{Reachable: true, LastSeenTime: ago(2 * time.Second), Services: 2, Exports: 1},
		},
		&straitv1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "d"}},
		&straitv1.SiteLink{
			ObjectMeta: metav1.ObjectMeta{Name: "b-wireguard"},
			Spec: straitv1.SiteLinkSpec{Peer: "b", Transport: straitv1.TransportWireGuard, Cost: 100,
				WireGuard: &straitv1.WireGuardLink{ListenPort: 51820, Address: "10.200.0.1/30", PeerAddress: "10.200.0.2"}},
			Status: straitv1.SiteLinkStatus{Healthy: true, LastTransitionTime: ago(time.Hour), Interface: "wg0"},
		},
		&straitv1.SiteLink{
			ObjectMeta: metav1.ObjectMeta{Name: "b-direct"},
			Spec: straitv1.SiteLinkSpec{Peer: "b", Transport: straitv1.TransportDirect, Encryption: straitv1.EncryptionNone, Cost: 10,
				Direct: &straitv1.DirectLink{Address: "192.0.2.10"}},
			Status: straitv1.SiteLinkStatus{Healthy: true, Active: true, LastTransitionTime: ago(2 * time.Hour)},
		},
		pg(),
		&straitv1.SiteService{
			ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "app", Generation: 1},
			Spec: straitv1.SiteServiceSpec{Mode: straitv1.ModeSet,
				Members: []straitv1.Member{{Name: "cache-a", Site: "a"}, {Name: "cache-b", Site: "b"}}},
			Status: straitv1.SiteServiceStatus{
				ObservedGeneration: 1,
				Route:              straitv1.Route{Reason: straitv1.ReasonNoEligibleMember, Message: "no eligible member for locality LocalFirst", LastChangeTime: ago(30 * time.Second)},
			},
		},
		&straitv1.SiteService{
			ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "app", Generation: 1},
			Spec: straitv1.SiteServiceSpec{Mode: straitv1.ModeSingle,
				Members: []straitv1.Member{{Name: "fresh-a", Site: "a"}}},
		},
		&straitv1.SiteService{
			ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "other", Generation: 1},
			Spec:       straitv1.SiteServiceSpec{Mode: straitv1.ModeSingle},
			Status: straitv1.SiteServiceStatus{ObservedGeneration: 1,
				Route: straitv1.Route{Members: []string{"pg-b"}, Reason: straitv1.ReasonActive, LastChangeTime: ago(time.Minute)}},
		},
	}
}

func pg() *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "app", Generation: 3},
		Spec: straitv1.SiteServiceSpec{
			Mode:    straitv1.ModeSingle,
			Port:    straitv1.ServicePort{Name: "postgres", Port: 5432},
			Plugin:  straitv1.PluginSpec{CNPG: &straitv1.CNPGPlugin{Role: straitv1.CNPGRolePrimary}},
			Members: []straitv1.Member{{Name: "pg-a", Site: "a"}, {Name: "pg-b", Site: "b"}, {Name: "pg-c", Site: "c"}},
			Policy:  straitv1.Policy{Multiple: straitv1.RouteNowhere, UnknownHold: metav1.Duration{Duration: 30 * time.Second}},
		},
		Status: straitv1.SiteServiceStatus{
			ObservedGeneration: 3,
			Route: straitv1.Route{Members: []string{"pg-a"}, Reason: straitv1.ReasonActive, Message: "pg-a is the only writable member",
				LastChangeTime: ago(5 * time.Minute), Endpoints: 1},
			Members: []straitv1.MemberStatus{
				{Name: "pg-a", Site: "a", Role: straitv1.RolePrimary, Eligible: true, State: straitv1.StateStable, Reason: "ClusterPrimary",
					ObservedTime: ago(time.Second), LastConfirmedTime: ago(time.Second), Path: straitv1.PathLocal},
				{Name: "pg-b", Site: "b", Role: straitv1.RoleReplica, Eligible: true, State: straitv1.StateStable, Reason: "ClusterReplica",
					ObservedTime: ago(3 * time.Second), LastConfirmedTime: ago(3 * time.Second)},
			},
			Declarations: []straitv1.Declaration{{Site: "a", Primary: "pg-a"}, {Site: "b", Primary: "pg-a"}},
			Conditions: []metav1.Condition{
				{Type: straitv1.ConditionRouted, Status: metav1.ConditionTrue, Reason: straitv1.ReasonActive, Message: "1 endpoint", LastTransitionTime: *ago(5 * time.Minute)},
				{Type: straitv1.ConditionMultiple, Status: metav1.ConditionFalse, Reason: "OneWritable", LastTransitionTime: *ago(time.Hour)},
			},
		},
	}
}

func fakeReader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

type result struct {
	code   int
	stdout string
	stderr string
}

// cli runs the command line against r, with app as the context's namespace.
func cli(t *testing.T, r client.Reader, stdin string, args ...string) result {
	t.Helper()
	o := &options{
		connect: func(*options) (client.Reader, string, error) { return r, "app", nil },
		now:     func() time.Time { return now },
	}
	var out, errOut bytes.Buffer
	code := run(newRoot("strait", o), args, strings.NewReader(stdin), &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

func (r result) ok(t *testing.T) result {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	return r
}

func diff(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestStatus(t *testing.T) {
	r := cli(t, fakeReader(cluster()...), "", "status").ok(t)
	diff(t, r.stdout, `SITE  VERSION  GATEWAY           ADDRESS    WIREGUARD PUBLIC KEY
a     v1.0.0   strait-gateway-0  10.0.0.15  AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=

PEER  REACHABLE  LAST SEEN  SERVICES  EXPORTS  LAST ERROR
b     true       2s         2         1        -
c     false      5m         0         0        poll: context deadline exceeded
d     false      never      0         0        -

NAMESPACE  NAME   MODE    ROUTE    REASON            SINCE
app        cache  Set     nowhere  NoEligibleMember  30s
app        fresh  Single  -        -                 -
app        pg     Single  pg-a     Active            5m
`)
}

func TestStatusNamespaces(t *testing.T) {
	r := cli(t, fakeReader(cluster()...), "", "status", "-A", "-o", "json").ok(t)
	var v statusView
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.SiteServices) != 4 || v.SiteServices[3].Namespace != "other" {
		t.Errorf("-A: got %+v", v.SiteServices)
	}

	r = cli(t, fakeReader(cluster()...), "", "status", "-n", "other", "-o", "json").ok(t)
	v = statusView{}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.SiteServices) != 1 || v.SiteServices[0].Route[0] != "pg-b" {
		t.Errorf("-n other: got %+v", v.SiteServices)
	}
	if len(v.Sites) != 1 || v.Sites[0].GatewayAddress != "10.0.0.15" || v.Sites[0].ControllerVersion != "v1.0.0" || len(v.Peers) != 3 {
		t.Errorf("got %+v", v)
	}
}

func TestStatusEmpty(t *testing.T) {
	r := cli(t, fakeReader(), "", "status").ok(t)
	diff(t, r.stdout, "No Site found. Strait is not set up in this cluster.\n\nNo SitePeer found.\n\nNo SiteService found.\n")

	r = cli(t, fakeReader(), "", "status", "-o", "json").ok(t)
	diff(t, r.stdout, "{\n  \"sites\": [],\n  \"peers\": [],\n  \"siteServices\": []\n}\n")
}

func TestExplain(t *testing.T) {
	r := cli(t, fakeReader(cluster()...), "", "explain", "pg").ok(t)
	diff(t, r.stdout, `SiteService:  app/pg
Mode:         Single
Plugin:       cnpg, role primary
Policy:       multiple RouteNowhere, unknownHold 30s
Route:        pg-a
Reason:       Active
Meaning:      `+reasons[straitv1.ReasonActive]+`
Message:      pg-a is the only writable member
Since:        2026-10-27T09:55:00Z (5m ago)
Endpoints:    1

Members:
NAME  SITE  ROUTED  PATH   ROLE     ELIGIBLE  STATE   REASON          OBSERVED  CONFIRMED
pg-a  a     true    Local  primary  true      Stable  ClusterPrimary  1s        1s
pg-b  b     false   -      replica  true      Stable  ClusterReplica  3s        3s
pg-c  c     false   -      -        -         -       -               -         -

Declarations:
SITE  PRIMARY
a     pg-a
b     pg-a

Conditions:
TYPE      STATUS  REASON       SINCE  MESSAGE
Routed    True    Active       5m     1 endpoint
Multiple  False   OneWritable  60m    -
`)
}

func TestExplainStates(t *testing.T) {
	r := cli(t, fakeReader(cluster()...), "", "explain", "cache").ok(t)
	for _, want := range []string{
		"Locality:     LocalFirst\n",
		"Route:        nowhere. The name has no endpoints, and new connections are refused.\n",
		"Meaning:      " + reasons[straitv1.ReasonNoEligibleMember] + "\n",
		"Declarations:\nNone.\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}

	r = cli(t, fakeReader(cluster()...), "", "explain", "fresh").ok(t)
	for _, want := range []string{"Status:       none. The controller has not written a status yet.\n", "Route:        -\n"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}

	stale := pg()
	stale.Generation = 4
	r = cli(t, fakeReader(stale), "", "explain", "pg").ok(t)
	if !strings.Contains(r.stdout, "Status:       stale. It is from generation 3, and the spec is at generation 4.\n") {
		t.Errorf("no stale status in:\n%s", r.stdout)
	}
}

func TestExplainOutput(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		r := cli(t, fakeReader(cluster()...), "", "explain", "pg", "-o", format).ok(t)
		var v explainView
		if err := yaml.Unmarshal([]byte(r.stdout), &v); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if v.Route.Reason != straitv1.ReasonActive || v.Route.Meaning != reasons[straitv1.ReasonActive] ||
			len(v.Members) != 3 || !v.Members[0].Routed || v.Members[2].Observed || v.Members[0].Role != straitv1.RolePrimary ||
			v.Policy.UnknownHold.Duration != 30*time.Second || len(v.Declarations) != 2 || len(v.Conditions) != 2 {
			t.Errorf("%s: got %+v", format, v)
		}
	}
}

func TestExplainNamespaces(t *testing.T) {
	r := cli(t, fakeReader(cluster()...), "", "explain", "pg", "-n", "other", "-o", "json").ok(t)
	if !strings.Contains(r.stdout, `"pg-b"`) {
		t.Errorf("got %s", r.stdout)
	}

	r = cli(t, fakeReader(cluster()...), "", "explain", "cache", "-A", "-o", "json").ok(t)
	if !strings.Contains(r.stdout, `"namespace": "app"`) {
		t.Errorf("got %s", r.stdout)
	}

	r = cli(t, fakeReader(cluster()...), "", "explain", "pg", "-A")
	if r.code != 2 || !strings.Contains(r.stderr, `SiteService "pg" is in namespaces app, other: pick one with -n`) {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}

	r = cli(t, fakeReader(cluster()...), "", "explain", "nope")
	if r.code != 1 || r.stderr != "error: SiteService \"nope\" not found in namespace \"app\"\n" {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}

	r = cli(t, fakeReader(cluster()...), "", "explain", "nope", "-A")
	if r.code != 1 || !strings.Contains(r.stderr, "not found in any namespace") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestLinks(t *testing.T) {
	r := cli(t, fakeReader(cluster()...), "", "links").ok(t)
	diff(t, r.stdout, `PEER  LINK         TRANSPORT  ENCRYPTION  COST  ENDPOINT-DIRECT  HEALTHY  ACTIVE  CHANGED
b     b-direct     direct     None        10    false            true     true    120m
b     b-wireguard  wireguard  WireGuard   100   false            true     false   60m
`)

	r = cli(t, fakeReader(cluster()...), "", "links", "-o", "json").ok(t)
	var v struct{ Links []linkView }
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Links) != 2 || !v.Links[0].Active || v.Links[1].Interface != "wg0" {
		t.Errorf("got %+v", v)
	}

	r = cli(t, fakeReader(), "", "links").ok(t)
	diff(t, r.stdout, "No SiteLink found.\n")
}

func TestPeers(t *testing.T) {
	r := cli(t, fakeReader(cluster()...), "", "peers", "-o", "yaml").ok(t)
	var v struct{ Peers []peerView }
	if err := yaml.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Peers) != 3 || v.Peers[0].Name != "b" || !v.Peers[0].Reachable || v.Peers[1].LastError == "" || v.Peers[2].LastSeenTime != nil {
		t.Errorf("got %+v", v)
	}

	r = cli(t, fakeReader(), "", "peers").ok(t)
	diff(t, r.stdout, "No SitePeer found.\n")
}

func TestFailedRead(t *testing.T) {
	failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	for _, args := range [][]string{{"status"}, {"links"}, {"peers"}, {"explain", "pg", "-A"}} {
		r := cli(t, failing, "", args...)
		if r.code != 1 || !strings.Contains(r.stderr, "connection refused") {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}

	o := &options{connect: func(*options) (client.Reader, string, error) {
		return nil, "", errors.New(`context was not found for specified context: nope`)
	}, now: time.Now}
	var out, errOut bytes.Buffer
	if code := run(newRoot("strait", o), []string{"status", "--context", "nope"}, nil, &out, &errOut); code != 1 {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestUsageErrors(t *testing.T) {
	r := fakeReader(cluster()...)
	for _, args := range [][]string{
		{"bogus"},
		{"status", "extra"},
		{"status", "--bogus"},
		{"status", "-o", "wide"},
		{"explain"},
		{"explain", "a", "b"},
		{"validate"},
		{"links", "-n"},
		{"version", "extra"},
	} {
		res := cli(t, r, "", args...)
		if res.code != 2 || !strings.HasPrefix(res.stderr, "error: ") || !strings.Contains(res.stderr, "--help' for usage.") {
			t.Errorf("%v: exit %d, stderr %q", args, res.code, res.stderr)
		}
		if res.stdout != "" {
			t.Errorf("%v: stdout %q", args, res.stdout)
		}
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"status", "--help"}, {"help", "explain"}} {
		r := cli(t, fakeReader(), "", args...).ok(t)
		if !strings.Contains(r.stdout, "Usage:") {
			t.Errorf("%v: got %q", args, r.stdout)
		}
	}
}

func TestKubectlPlugin(t *testing.T) {
	o := &options{connect: connect, now: time.Now}
	root := newRoot("kubectl strait", o)
	var out, errOut bytes.Buffer
	run(root, []string{"status", "--help"}, nil, &out, &errOut)
	if !strings.Contains(out.String(), "kubectl strait status [flags]") {
		t.Errorf("got %q", out.String())
	}
	out.Reset()
	if code := run(newRoot("kubectl strait", o), []string{"explain"}, nil, &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "Run 'kubectl strait explain --help' for usage.") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

func kubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	data := `apiVersion: v1
kind: Config
current-context: a
clusters:
- name: a
  cluster: {server: "https://192.0.2.1:6443"}
- name: b
  cluster: {server: "https://198.51.100.1:6443"}
users:
- name: reader
  user: {}
contexts:
- name: a
  context: {cluster: a, user: reader}
- name: b
  context: {cluster: b, user: reader, namespace: app}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContext(t *testing.T) {
	path := kubeconfig(t)
	for _, c := range []struct{ context, host, namespace string }{
		{"", "https://192.0.2.1:6443", "default"},
		{"a", "https://192.0.2.1:6443", "default"},
		{"b", "https://198.51.100.1:6443", "app"},
	} {
		cfg, ns, err := restConfig(&options{kubeconfig: path, kubeContext: c.context})
		if err != nil {
			t.Fatalf("%q: %v", c.context, err)
		}
		if cfg.Host != c.host || ns != c.namespace {
			t.Errorf("%q: got %s %s, want %s %s", c.context, cfg.Host, ns, c.host, c.namespace)
		}
	}
	if _, _, err := restConfig(&options{kubeconfig: path, kubeContext: "c"}); err == nil {
		t.Error("a context that does not exist must be an error")
	}

	t.Setenv("KUBECONFIG", path)
	cfg, _, err := restConfig(&options{kubeContext: "b"})
	if err != nil || cfg.Host != "https://198.51.100.1:6443" {
		t.Errorf("KUBECONFIG: got %v %v", cfg, err)
	}
}

// The reader has no write methods, and none can be reached by a type assertion.
func TestReadOnly(t *testing.T) {
	r, _, err := connect(&options{kubeconfig: kubeconfig(t), kubeContext: "b"})
	if err != nil {
		t.Fatal(err)
	}
	var v any = r
	if _, ok := v.(client.Writer); ok {
		t.Error("the reader can write")
	}
	if _, ok := v.(client.StatusClient); ok {
		t.Error("the reader can write status")
	}
}

// A site whose controller has not written its version shows -.
func TestStatusNoControllerVersion(t *testing.T) {
	r := cli(t, fakeReader(&straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}}), "", "status").ok(t)
	if !strings.HasPrefix(r.stdout, "SITE  VERSION  GATEWAY  ADDRESS  WIREGUARD PUBLIC KEY\na     -        -        -        -\n") {
		t.Errorf("got:\n%s", r.stdout)
	}
}

// setVersion sets the version and the commit of the build until the test ends.
func setVersion(t *testing.T, v, commit string) {
	t.Helper()
	oldVersion, oldCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = oldVersion, oldCommit })
	version.Version, version.Commit = v, commit
}

func TestVersion(t *testing.T) {
	setVersion(t, "v1.0.0", "e76a8cdd00f51eb4d84c9dbb300a546952d46b05")
	r := cli(t, nil, "", "version").ok(t)
	diff(t, r.stdout, "Version:  v1.0.0\nCommit:   e76a8cdd00f51eb4d84c9dbb300a546952d46b05\n")

	for _, format := range []string{"json", "yaml"} {
		r = cli(t, nil, "", "version", "-o", format).ok(t)
		var v versionView
		if err := yaml.UnmarshalStrict([]byte(r.stdout), &v); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if v.Version != "v1.0.0" || v.Commit != "e76a8cdd00f51eb4d84c9dbb300a546952d46b05" {
			t.Errorf("%s: got %+v", format, v)
		}
	}

	for _, args := range [][]string{{"--version"}, {"status", "--version"}} {
		r = cli(t, nil, "", args...).ok(t)
		diff(t, r.stdout, "strait version v1.0.0, commit e76a8cdd00f51eb4d84c9dbb300a546952d46b05\n")
	}
}

// validate, --skill, version and --version need no cluster.
func TestNoCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	var out, errOut bytes.Buffer
	if code := Main("strait", []string{"validate", "-f", "../validate/testdata/set/good.yaml"}, nil, &out, &errOut); code != 0 {
		t.Errorf("validate: exit %d, stderr %q", code, errOut.String())
	}
	out.Reset()
	if code := Main("strait", []string{"--skill"}, nil, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "---\n") {
		t.Errorf("--skill: exit %d, stderr %q", code, errOut.String())
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"version"}, "Version:  " + version.Version + "\nCommit:   " + version.Commit + "\n"},
		{[]string{"version", "-o", "json"}, "{\n  \"version\": \"" + version.Version + "\",\n  \"commit\": \"" + version.Commit + "\"\n}\n"},
		{[]string{"--version"}, "strait version " + version.Version + ", commit " + version.Commit + "\n"},
	} {
		out.Reset()
		errOut.Reset()
		if code := Main("strait", c.args, nil, &out, &errOut); code != 0 || out.String() != c.want {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", c.args, code, out.String(), errOut.String())
		}
	}
}
