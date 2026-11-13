//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// webOf is the name of the site's web server, its Service and its member.
func webOf(s *site) string { return "web-" + s.name }

// webFixture runs one small HTTP server per site, behind a Service of its own name.
func webFixture(ctx context.Context, s *site) error {
	if err := ensureNamespace(ctx, s.c, webNamespace, nil); err != nil {
		return err
	}
	name := webOf(s)
	labels := map[string]string{"app": name}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: new(int64),
					Containers: []corev1.Container{{
						Name:            "netexec",
						Image:           agnhostImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{"netexec", "--http-port=8080"},
						Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8080)}},
							PeriodSeconds: 2,
						},
					}},
				},
			},
		},
	}
	if err := s.c.Create(ctx, dep); err != nil {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	return s.c.Create(ctx, svc)
}

// webService is the SiteService web, with the declared plugin naming active.
func webService(e *env, active string) *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: webNamespace},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "http", Port: 8080},
			Plugin: straitv1.PluginSpec{Declared: &straitv1.DeclaredPlugin{Active: active}},
			Members: []straitv1.Member{
				{Name: webOf(e.a), Site: e.a.name, Service: &straitv1.ServicePortRef{Name: webOf(e.a), Port: 8080}},
				{Name: webOf(e.b), Site: e.b.name, Service: &straitv1.ServicePortRef{Name: webOf(e.b), Port: 8080}},
			},
		},
	}
}

// pgService is the SiteService pg. Both sites apply the same one.
func pgService(e *env) *straitv1.SiteService {
	return &straitv1.SiteService{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: dbNamespace},
		Spec: straitv1.SiteServiceSpec{
			Mode:   straitv1.ModeSingle,
			Port:   straitv1.ServicePort{Name: "postgres", Port: 5432},
			Plugin: straitv1.PluginSpec{CNPG: &straitv1.CNPGPlugin{Role: straitv1.CNPGRolePrimary}},
			Members: []straitv1.Member{
				{Name: memberOf(e.a), Site: e.a.name, CNPG: &straitv1.CNPGMember{Cluster: clusterOf(e.a)}},
				{Name: memberOf(e.b), Site: e.b.name, CNPG: &straitv1.CNPGMember{Cluster: clusterOf(e.b)}},
			},
			Policy: straitv1.Policy{UnknownHold: metav1.Duration{Duration: unknownHold}},
		},
	}
}

// requestReaches returns a check that a request from the site's own web server to the Service, in namespace web,
// is answered by the member.
func requestReaches(s *site, service, member string) func(ctx context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		out, err := s.kubectl(ctx, 15*time.Second, "-n", webNamespace, "exec", "deploy/"+webOf(s), "--",
			"curl", "-sS", "--max-time", "3", "http://"+service+"."+webNamespace+".svc:8080/hostname")
		if err != nil {
			return false, "site " + s.name + ": " + err.Error()
		}
		host := strings.TrimSpace(out)
		return strings.HasPrefix(host, member+"-"), "site " + s.name + ": answered by " + host
	}
}

// envoyDrainSeconds is the chart value envoy.drainTimeSeconds at both sites: a closed import ends its connections
// after it (RFC0005 requirement 54).
const envoyDrainSeconds = 2

// sessionScript opens one TCP connection to the web Service, sends nothing, and prints "open" with the time, then
// "closed" with the time once the server's end closes or resets it. The server waits for a request with no time limit.
const sessionScript = `exec 3<>/dev/tcp/web.web.svc/8080 || exit 1
echo "open $(date +%s%3N)"
while :; do
  read -t 1 -u 3 x
  if [ $? -le 128 ]; then echo "closed $(date +%s%3N)"; exit 0; fi
done
`

// session is a connection held from a site's client pod.
type session struct {
	cmd    *exec.Cmd
	opened chan time.Time
	closed chan time.Time
}

// holdSession opens a session to the web Service from the site's client pod.
func holdSession(ctx context.Context, s *site) (*session, error) {
	ss := &session{opened: make(chan time.Time, 1), closed: make(chan time.Time, 1)}
	ss.cmd = exec.CommandContext(ctx, "kubectl", "--kubeconfig", s.kubeconfig, "--context", s.context,
		"-n", dbNamespace, "exec", clientPod, "--", "bash", "-c", sessionScript)
	ss.cmd.Env = append(ss.cmd.Environ(), "KUBECONFIG="+s.kubeconfig)
	ss.cmd.WaitDelay = 5 * time.Second
	out, err := ss.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := ss.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) != 2 {
				continue
			}
			ms, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				continue
			}
			switch f[0] {
			case "open":
				ss.opened <- time.UnixMilli(ms)
			case "closed":
				ss.closed <- time.UnixMilli(ms)
			}
		}
	}()
	select {
	case <-ss.opened:
		return ss, nil
	case <-time.After(30 * time.Second):
		ss.Stop()
		return nil, fmt.Errorf("the session from site %s did not open", s.name)
	}
}

// Stop ends the session's command.
func (ss *session) Stop() {
	if ss.cmd.Process != nil {
		_ = ss.cmd.Process.Kill()
	}
	_ = ss.cmd.Wait()
}

// declareWeb makes both sites declare member active for web, and waits until site s routes it with reason Active.
func (e *env) declareWeb(ctx context.Context, t *testing.T, s *site, active string) time.Time {
	t.Helper()
	for _, x := range e.sites() {
		ss, err := siteService(ctx, x, webNamespace, "web")
		if err != nil {
			t.Fatal(err)
		}
		base := ss.DeepCopy()
		ss.Spec.Plugin.Declared.Active = active
		if err := x.c.Patch(ctx, ss, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "site "+s.name+" routes web to "+active, routeIs(s, webNamespace, "web", []string{active}, straitv1.ReasonActive))
	return time.Now()
}

// setConnections sets web's policy.connections at a site. "" removes it.
func setConnections(ctx context.Context, s *site, policy straitv1.ConnectionsPolicy) error {
	value := "null"
	if policy != "" {
		value = fmt.Sprintf("%q", policy)
	}
	ss := &straitv1.SiteService{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: webNamespace}}
	return s.c.Patch(ctx, ss, client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"policy":{"connections":%s}}}`, value))))
}

// endGateway: at site b, web routes to web-a through the gateway, and a client holds a session to it. With Keep, the
// default, a move of the route to web-b keeps the session. With EndGateway, the move ends it, and a new request
// reaches web-b (RFC0002 requirement 51). web-a stays up throughout, so only Strait can end the session.
func (e *env) endGateway(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "site b routes web to web-a", routeIs(b, webNamespace, "web", []string{webOf(a)}, straitv1.ReasonActive))
	const kept = 20 * time.Second

	// Keep: the session outlives the route change.
	ss, err := holdSession(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	moved := e.declareWeb(ctx, t, b, webOf(b))
	select {
	case at := <-ss.closed:
		t.Errorf("with Keep, the session closed %s after the route moved", at.Sub(moved).Round(100*time.Millisecond))
	case <-time.After(kept):
		t.Logf("with Keep, the session stays %s after the route moved", kept)
	}
	ss.Stop()
	e.declareWeb(ctx, t, b, webOf(a))

	// EndGateway at site b: the session ends, and a new request reaches web-b.
	if err := setConnections(ctx, b, straitv1.ConnectionsEndGateway); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := setConnections(context.WithoutCancel(ctx), b, ""); err != nil {
			t.Errorf("remove policy.connections: %v", err)
		}
	}()
	eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "a request from site b reaches web-a", requestReaches(b, "web", webOf(a)))
	ss, err = holdSession(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Stop()
	moved = e.declareWeb(ctx, t, b, webOf(b))
	select {
	case at := <-ss.closed:
		e.times.record(t, "5b/session-ended-after-route", at.Sub(moved))
		t.Logf("with EndGateway, the session closed %s after the route moved", at.Sub(moved).Round(100*time.Millisecond))
	case <-time.After(kept):
		t.Errorf("with EndGateway, the session stays %s after the route moved, want it ended", kept)
	}
	eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "a request from site b reaches web-b", requestReaches(b, "web", webOf(b)))
}
