//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	_ "embed"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

//go:embed testdata/cluster.yaml.tmpl
var clusterTemplate string

var cnpgCluster = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

const (
	appSecret    = "pg-app"
	clientPod    = "pg-client"
	healthyPhase = "Cluster in healthy state"
)

// SQL of one write through the application name. The answer names the cluster that took it.
const (
	createTable = `CREATE TABLE IF NOT EXISTS e2e (id bigserial PRIMARY KEY, site text NOT NULL, at timestamptz NOT NULL DEFAULT now())`
	whoAnswered = `SELECT pg_is_in_recovery(), current_setting('cluster_name')`
)

func clusterOf(s *site) string { return "cluster-" + s.name }
func memberOf(s *site) string  { return "pg-" + s.name }

// installCNPG applies the pinned CloudNativePG manifest and waits for its CRDs.
func installCNPG(ctx context.Context, s *site, manifest string) error {
	if _, err := s.kubectl(ctx, 5*time.Minute, "apply", "--server-side", "-f", manifest); err != nil {
		return err
	}
	_, err := s.kubectl(ctx, 2*time.Minute, "wait", "--for=condition=Established", "--timeout=90s",
		"crd/clusters.postgresql.cnpg.io", "crd/poolers.postgresql.cnpg.io")
	return err
}

// waitCNPG waits for the CloudNativePG operator to roll out.
func waitCNPG(ctx context.Context, s *site) error {
	_, err := s.kubectl(ctx, 6*time.Minute, "-n", "cnpg-system", "rollout", "status", "deployment/cnpg-controller-manager", "--timeout=5m")
	return err
}

// dbFixture creates the db namespace, the application Secret with a password shared by both sites, and the client pod.
func dbFixture(ctx context.Context, s *site, password string) error {
	if err := ensureNamespace(ctx, s.c, dbNamespace, nil); err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: appSecret, Namespace: dbNamespace},
		Type:       corev1.SecretTypeBasicAuth,
		StringData: map[string]string{"username": "app", "password": password},
	}
	if err := s.c.Create(ctx, secret); err != nil {
		return fmt.Errorf("secret %s: %w", appSecret, err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: clientPod, Namespace: dbNamespace},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: new(int64),
			Containers: []corev1.Container{{
				Name:            "psql",
				Image:           postgresImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				Env: []corev1.EnvVar{
					{Name: "PGUSER", Value: "app"},
					{Name: "PGDATABASE", Value: "app"},
					{Name: "PGCONNECT_TIMEOUT", Value: "3"},
					{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: appSecret}, Key: "password",
					}}},
				},
			}},
		},
	}
	return s.c.Create(ctx, pod)
}

// randomPassword returns 32 hex digits.
func randomPassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// clusterManifest renders the site's Cluster: the primary of the topology at a, a replica cluster at b.
func (e *env) clusterManifest(s *site) (*unstructured.Unstructured, error) {
	type external struct{ Name, Host string }
	data := struct {
		Name, Namespace, Image, Peer, Host string
		Replica                            bool
		External                           []external
	}{
		Name: clusterOf(s), Namespace: dbNamespace, Image: postgresImage, Peer: clusterOf(e.peer(s)), Host: s.node,
		Replica:  s == e.b,
		External: []external{{clusterOf(e.a), e.a.node}, {clusterOf(e.b), e.b.node}},
	}
	var buf bytes.Buffer
	if err := template.Must(template.New("cluster").Parse(clusterTemplate)).Execute(&buf, data); err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(buf.Bytes(), &u.Object); err != nil {
		return nil, err
	}
	return u, nil
}

// createCNPGCluster creates the site's Cluster, retrying while the operator's webhook warms up, and waits for it to be Ready.
func (e *env) createCNPGCluster(ctx context.Context, s *site) error {
	u, err := e.clusterManifest(s)
	if err != nil {
		return err
	}
	if _, last, err := poll(ctx, 3*time.Minute, 2*time.Second, func(ctx context.Context) (bool, string) {
		err := s.c.Create(ctx, u.DeepCopy())
		if err == nil || apierrors.IsAlreadyExists(err) {
			return true, ""
		}
		return false, err.Error()
	}); err != nil {
		return fmt.Errorf("create %s: %w; last: %s", clusterOf(s), err, last)
	}
	if _, last, err := poll(ctx, 10*time.Minute, 2*time.Second, clusterHealthy(s)); err != nil {
		return fmt.Errorf("%s not ready: %w; last: %s", clusterOf(s), err, last)
	}
	return nil
}

// getCluster reads the site's Cluster.
func getCluster(ctx context.Context, s *site) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpgCluster)
	return u, s.c.Get(ctx, types.NamespacedName{Namespace: dbNamespace, Name: clusterOf(s)}, u)
}

// field reads a string field of an unstructured object.
func field(u *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedFieldNoCopy(u.Object, path...)
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// clusterString is a one-line view of a Cluster's role fields.
func clusterString(u *unstructured.Unstructured) string {
	return fmt.Sprintf("%s: phase=%q primary=%s switching=%s demotionToken=%t lastPromotionToken=%t",
		u.GetName(), field(u, "status", "phase"), field(u, "spec", "replica", "primary"),
		field(u, "status", "switchReplicaClusterStatus", "inProgress"),
		field(u, "status", "demotionToken") != "", field(u, "status", "lastPromotionToken") != "")
}

// clusterHealthy reports whether the site's Cluster is Ready, healthy, and not switching.
func clusterHealthy(s *site) func(ctx context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		u, err := getCluster(ctx, s)
		if err != nil {
			return false, err.Error()
		}
		var conds []metav1.Condition
		if raw, ok, _ := unstructured.NestedSlice(u.Object, "status", "conditions"); ok {
			for _, r := range raw {
				m, _ := r.(map[string]any)
				conds = append(conds, metav1.Condition{Type: fmt.Sprint(m["type"]), Status: metav1.ConditionStatus(fmt.Sprint(m["status"]))})
			}
		}
		ok := meta.IsStatusConditionTrue(conds, "Ready") && field(u, "status", "phase") == healthyPhase &&
			field(u, "status", "switchReplicaClusterStatus", "inProgress") != "true"
		return ok, clusterString(u)
	}
}

// patchCluster applies a merge patch to the site's Cluster.
func patchCluster(ctx context.Context, s *site, patch string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpgCluster)
	u.SetNamespace(dbNamespace)
	u.SetName(clusterOf(s))
	return s.c.Patch(ctx, u, client.RawPatch(types.MergePatchType, []byte(patch)))
}

// copySecrets copies a Cluster's replication client certificate and its CA certificate to the other site.
// Only name, type and data are copied: an owner reference would let the target's garbage collector delete the copy.
// The CA's private key is never copied, and no data is printed.
func copySecrets(ctx context.Context, from, to *site, cluster string) error {
	for name, keys := range map[string][]string{cluster + "-replication": {"tls.crt", "tls.key"}, cluster + "-ca": {"ca.crt"}} {
		var src corev1.Secret
		if err := from.c.Get(ctx, types.NamespacedName{Namespace: dbNamespace, Name: name}, &src); err != nil {
			return fmt.Errorf("site %s: secret %s: %w", from.name, name, err)
		}
		dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dbNamespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, to.c, dst, func() error {
			dst.Type = src.Type
			dst.Data = map[string][]byte{}
			for _, k := range keys {
				dst.Data[k] = src.Data[k]
			}
			return nil
		}); err != nil {
			return fmt.Errorf("site %s: secret %s: %w", to.name, name, err)
		}
	}
	return nil
}

// answerRE matches the answer of whoAnswered.
var answerRE = regexp.MustCompile(`^([tf])\|(cluster-[a-z]+)$`)

// write inserts one row through pg.db.svc from the site's client pod and returns the cluster that took it.
func write(ctx context.Context, s *site) (string, error) {
	out, err := s.kubectl(ctx, 20*time.Second, "-n", dbNamespace, "exec", clientPod, "--",
		"timeout", "8", "psql", "-h", "pg.db.svc", "-XtAq", "-v", "ON_ERROR_STOP=1",
		"-c", fmt.Sprintf("INSERT INTO e2e (site) VALUES ('%s')", s.name), "-c", whoAnswered)
	if err != nil {
		return "", err
	}
	m := answerRE.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", fmt.Errorf("unexpected answer %q", strings.TrimSpace(out))
	}
	if m[1] == "t" {
		return m[2], fmt.Errorf("%s answered in recovery", m[2])
	}
	return m[2], nil
}

// writesLandOn returns a check that a write from the site succeeds and lands on the cluster.
func writesLandOn(s *site, cluster string) func(ctx context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		got, err := write(ctx, s)
		if err != nil {
			return false, "site " + s.name + ": " + err.Error()
		}
		return got == cluster, fmt.Sprintf("site %s: write landed on %s", s.name, got)
	}
}

// attempt is one write of a writer loop, timed inside the client pod.
type attempt struct {
	start, end time.Time
	ok         bool
	cluster    string
	out        string
}

func (a attempt) readOnly() bool { return strings.Contains(a.out, "read-only") }

// writerScript writes through pg.db.svc every 200ms until the stop file exists or the time runs out.
// Each line is: start ms, end ms, exit status, answer.
const writerScript = `site=$1; secs=$2; stop=$3
end=$((SECONDS+secs))
while [ ! -e "$stop" ] && [ "$SECONDS" -lt "$end" ]; do
  s=$(date +%s%3N)
  out=$(timeout 5 psql -h pg.db.svc -XtAq -v ON_ERROR_STOP=1 -c "INSERT INTO e2e (site) VALUES ('$site')" -c "` + whoAnswered + `" 2>&1)
  rc=$?
  e=$(date +%s%3N)
  echo "$s $e $rc ${out//$'\n'/ }"
  sleep 0.2
done
`

// writer is a write loop running in a site's client pod.
type writer struct {
	s    *site
	stop string
	cmd  *exec.Cmd
	mu   sync.Mutex
	all  []attempt
	done chan struct{}
}

// startWriter starts a write loop that runs at most maxRun.
func startWriter(ctx context.Context, s *site, maxRun time.Duration) (*writer, error) {
	w := &writer{s: s, stop: fmt.Sprintf("/tmp/stop-%d", time.Now().UnixNano()), done: make(chan struct{})}
	w.cmd = exec.CommandContext(ctx, "kubectl", "--kubeconfig", s.kubeconfig, "--context", s.context,
		"-n", dbNamespace, "exec", clientPod, "--", "bash", "-c", writerScript, "writer", s.name,
		strconv.Itoa(int(maxRun.Seconds())), w.stop)
	w.cmd.Env = append(w.cmd.Environ(), "KUBECONFIG="+s.kubeconfig)
	w.cmd.WaitDelay = 5 * time.Second
	out, err := w.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := w.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		defer close(w.done)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if a, ok := parseAttempt(sc.Text()); ok {
				w.mu.Lock()
				w.all = append(w.all, a)
				w.mu.Unlock()
			}
		}
	}()
	return w, nil
}

func parseAttempt(line string) (attempt, bool) {
	f := strings.SplitN(line, " ", 4)
	if len(f) < 3 {
		return attempt{}, false
	}
	s, err1 := strconv.ParseInt(f[0], 10, 64)
	e, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return attempt{}, false
	}
	a := attempt{start: time.UnixMilli(s), end: time.UnixMilli(e)}
	if len(f) == 4 {
		a.out = strings.TrimSpace(f[3])
	}
	if m := answerRE.FindStringSubmatch(a.out); f[2] == "0" && m != nil && m[1] == "f" {
		a.ok, a.cluster = true, m[2]
	}
	return a, true
}

// attempts returns the attempts so far.
func (w *writer) attempts() []attempt {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]attempt(nil), w.all...)
}

// Stop ends the loop and returns every attempt.
func (w *writer) Stop(ctx context.Context) []attempt {
	_, _ = w.s.kubectl(ctx, 20*time.Second, "-n", dbNamespace, "exec", clientPod, "--", "touch", w.stop)
	select {
	case <-w.done:
	case <-time.After(20 * time.Second):
		_ = w.cmd.Process.Kill()
		<-w.done
	}
	_ = w.cmd.Wait()
	return w.attempts()
}

// longestGap returns the longest time between two successful writes that end after from, and the failures in that time.
func longestGap(all []attempt, from time.Time) (time.Duration, int) {
	var gap time.Duration
	failed := 0
	var last time.Time
	for _, a := range all {
		if a.end.Before(from) {
			if a.ok {
				last = a.end
			}
			continue
		}
		if !a.ok {
			failed++
			continue
		}
		if !last.IsZero() && a.end.Sub(last) > gap {
			gap = a.end.Sub(last)
		}
		last = a.end
	}
	return gap, failed
}

// outcome is a short class of an attempt's result.
func (a attempt) outcome() string {
	switch {
	case a.ok:
		return "ok " + a.cluster
	case a.readOnly():
		return "read-only"
	case strings.Contains(a.out, "refused"):
		return "refused"
	case strings.Contains(a.out, "timeout expired") || a.out == "":
		return "timeout"
	}
	return "error: " + tail(a.out, 80)
}

// attemptSummary lists runs of attempts with the same outcome, with the start of each run relative to t0.
func attemptSummary(all []attempt, t0 time.Time) string {
	var parts []string
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].outcome() == all[i].outcome() {
			j++
		}
		parts = append(parts, fmt.Sprintf("%+.1fs %dx %s", all[i].start.Sub(t0).Seconds(), j-i, all[i].outcome()))
		i = j
	}
	return strings.Join(parts, " -> ")
}

// connectTimed connects from the site's client pod to pg.db.svc once and reports the time it took, the exit status and the answer.
func connectTimed(ctx context.Context, s *site) (time.Duration, int, string, error) {
	out, err := s.kubectl(ctx, 20*time.Second, "-n", dbNamespace, "exec", clientPod, "--", "bash", "-c",
		`s=$(date +%s%N); out=$(timeout 8 psql -h pg.db.svc -XtAc 'SELECT 1' 2>&1); rc=$?; e=$(date +%s%N); echo "$(( (e-s)/1000000 )) $rc ${out//$'\n'/ }"`)
	if err != nil {
		return 0, 0, "", err
	}
	f := strings.SplitN(strings.TrimSpace(out), " ", 3)
	if len(f) < 2 {
		return 0, 0, "", fmt.Errorf("unexpected output %q", out)
	}
	ms, _ := strconv.Atoi(f[0])
	rc, _ := strconv.Atoi(f[1])
	answer := ""
	if len(f) == 3 {
		answer = f[2]
	}
	return time.Duration(ms) * time.Millisecond, rc, answer, nil
}

// clusterState is one distinct state of a Cluster's role fields, as the cnpg plugin reads them.
type clusterState struct {
	at    time.Time
	state string
}

// clusterWatch records each change of both Clusters' role fields, every 100ms, to explain a route.
type clusterWatch struct {
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	states []clusterState
}

func watchClusters(ctx context.Context, sites []*site) *clusterWatch {
	w := &clusterWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		last := map[string]string{}
		for {
			for _, s := range sites {
				cctx, cancel := context.WithTimeout(ctx, time.Second)
				u, err := getCluster(cctx, s)
				cancel()
				if err != nil {
					continue
				}
				current := field(u, "status", "currentPrimary")
				st := fmt.Sprintf("%s: phase=%q primary=%s isPrimary=%s currentPrimary=%s targetPrimary=%s switching=%s promotionTokenSet=%t promoted=%t",
					u.GetName(), field(u, "status", "phase"), field(u, "spec", "replica", "primary"),
					field(u, "status", "instancesReportedState", current, "isPrimary"), current, field(u, "status", "targetPrimary"),
					field(u, "status", "switchReplicaClusterStatus", "inProgress"), field(u, "spec", "replica", "promotionToken") != "",
					field(u, "spec", "replica", "promotionToken") != "" && field(u, "spec", "replica", "promotionToken") == field(u, "status", "lastPromotionToken"))
				if st != last[s.name] {
					last[s.name] = st
					w.mu.Lock()
					w.states = append(w.states, clusterState{at: time.Now(), state: st})
					w.mu.Unlock()
				}
			}
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	return w
}

// Stop ends the watch and returns the recorded states.
func (w *clusterWatch) Stop() []clusterState {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]clusterState(nil), w.states...)
}
