//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// Reasons of a route that is nowhere because a held member's hold ended.
var timedOut = []string{straitv1.ReasonUnknownTimeout, straitv1.ReasonMemberUnknown}

// baseline: both sites route pg to pg-a, and a write from each site lands on cluster-a.
func (e *env) baseline(ctx context.Context, t *testing.T) {
	for _, s := range e.sites() {
		eventually(ctx, t, 2*time.Minute, time.Second, "site "+s.name+" routes pg to pg-a",
			routeIs(s, dbNamespace, "pg", []string{memberOf(e.a)}, straitv1.ReasonActive))
	}
	eventually(ctx, t, time.Minute, time.Second, "create the table", func(ctx context.Context) (bool, string) {
		_, err := e.a.kubectl(ctx, 20*time.Second, "-n", dbNamespace, "exec", clientPod, "--",
			"psql", "-h", "pg.db.svc", "-XtAq", "-v", "ON_ERROR_STOP=1", "-c", createTable)
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	for _, s := range e.sites() {
		d := eventually(ctx, t, time.Minute, time.Second, "a write from site "+s.name+" lands on cluster-a", writesLandOn(s, clusterOf(e.a)))
		e.times.record(t, "1/first-write-"+s.name, d)
	}
}

// importService: a hand-written Export of web-a at site a, and an Import of it with port at site b, give site b the name
// web-a.web.svc for site a's web-a. Once the Import drops port, its Service is deleted.
func (e *env) importService(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	for _, s := range e.sites() {
		if _, err := s.kubectl(ctx, 3*time.Minute, "-n", webNamespace, "rollout", "status", "deployment/"+webOf(s), "--timeout=2m"); err != nil {
			t.Fatal(err)
		}
	}
	name := webOf(a)
	exp := &straitv1.Export{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace},
		Spec:       straitv1.ExportSpec{Service: straitv1.ServicePortRef{Name: name, Port: 8080}},
	}
	imp := &straitv1.Import{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace},
		Spec:       straitv1.ImportSpec{Site: a.name, Export: name, Port: &straitv1.ServicePort{Name: "http", Port: 8080}},
	}
	if err := a.c.Create(ctx, exp); err != nil {
		t.Fatal(err)
	}
	if err := b.c.Create(ctx, imp); err != nil {
		t.Fatal(err)
	}
	d := eventually(ctx, t, time.Minute, time.Second, "a request from site b to "+name+" reaches "+name, requestReaches(b, name, name))
	e.times.record(t, "import/first-request", d)
	gatewayPolicy(ctx, t, b, name)

	base := imp.DeepCopy()
	imp.Spec.Port = nil
	if err := b.c.Patch(ctx, imp, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 30*time.Second, time.Second, "site b deletes Service "+name, func(ctx context.Context) (bool, string) {
		err := b.c.Get(ctx, types.NamespacedName{Namespace: webNamespace, Name: name}, &corev1.Service{})
		return apierrors.IsNotFound(err), fmt.Sprintf("get: %v", err)
	})
	if err := b.c.Delete(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if err := a.c.Delete(ctx, exp); err != nil {
		t.Fatal(err)
	}
}

// gatewayPolicy: under the gateway's policy, a pod of the site reaches the active gateway pod's import port, on the
// pod's address, and not the gateway agent's metrics, 8091, whose sources networkPolicy.metricsFrom names (RFC0007
// requirement 28). The agent listens on 8091, so a timeout, not a refusal, is the policy.
func gatewayPolicy(ctx context.Context, t *testing.T, s *site, importName string) {
	t.Helper()
	var st straitv1.Site
	if err := s.c.Get(ctx, types.NamespacedName{Name: s.name}, &st); err != nil {
		t.Fatal(err)
	}
	var imp straitv1.Import
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: webNamespace, Name: importName}, &imp); err != nil {
		t.Fatal(err)
	}
	addr, port := st.Status.Gateway.Address, imp.Status.GatewayPort
	if addr == "" || port == 0 {
		t.Fatalf("site %s: gateway address %q, import port %d", s.name, addr, port)
	}
	curl := func(p int) (string, error) {
		return s.kubectl(ctx, 15*time.Second, "-n", webNamespace, "exec", "deploy/"+webOf(s), "--",
			"curl", "-sS", "--max-time", "3", "http://"+net.JoinHostPort(addr, strconv.Itoa(p))+"/hostname")
	}
	if out, err := curl(int(port)); err != nil || !strings.HasPrefix(strings.TrimSpace(out), importName+"-") {
		t.Errorf("site %s: a pod to the gateway's import port %s:%d: %q, %v; want an answer from %s", s.name, addr, port, out, err, importName)
	}
	if out, err := curl(8091); err == nil || !strings.Contains(err.Error(), "(28)") {
		t.Errorf("site %s: a pod to the gateway's metrics %s:8091: %q, %v; want a timeout, curl's exit 28", s.name, addr, out, err)
	}
	t.Logf("site %s: a pod reaches the gateway's import port %d, and not its metrics port 8091", s.name, port)
}

// portsTogether: Exports created at the same moment at site a, and Imports at site b, each get a gateway port that no
// other one holds, at every moment (issue #26). The creates run in parallel, so the controller reconciles the objects
// back to back, likely before its cache shows the port it gave the one before. Timing decides, so a pass on a build
// without the fix is possible.
func (e *env) portsTogether(ctx context.Context, t *testing.T) {
	const n = 8
	a, b := e.a, e.b
	label := map[string]string{"e2e.strait.tako.id/scenario": "ports"}
	var exports, imports []client.Object
	for i := range n {
		name := fmt.Sprintf("together-%d", i)
		exports = append(exports, &straitv1.Export{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace, Labels: label},
			Spec:       straitv1.ExportSpec{Service: straitv1.ServicePortRef{Name: name, Port: 8080}},
		})
		imports = append(imports, &straitv1.Import{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: webNamespace, Labels: label},
			Spec:       straitv1.ImportSpec{Site: a.name, Export: name},
		})
	}
	create := func(s *site, objs []client.Object) func() error {
		return func() error {
			fns := make([]func() error, len(objs))
			for i, o := range objs {
				fns[i] = func() error { return s.c.Create(ctx, o) }
			}
			return parallel(fns...)
		}
	}
	if err := parallel(create(a, exports), create(b, imports)); err != nil {
		t.Fatal(err)
	}

	// ports reads each object's port, and reports the ports that two objects hold.
	type holders map[int32][]string
	ports := func(ctx context.Context, s *site, list client.ObjectList) (holders, int, error) {
		if err := s.c.List(ctx, list, client.InNamespace(webNamespace), client.MatchingLabels(label)); err != nil {
			return nil, 0, err
		}
		out, missing := holders{}, 0
		switch l := list.(type) {
		case *straitv1.ExportList:
			for _, o := range l.Items {
				if o.Status.GatewayPort == 0 {
					missing++
					continue
				}
				out[o.Status.GatewayPort] = append(out[o.Status.GatewayPort], o.Name)
			}
		case *straitv1.ImportList:
			for _, o := range l.Items {
				if o.Status.GatewayPort == 0 {
					missing++
					continue
				}
				out[o.Status.GatewayPort] = append(out[o.Status.GatewayPort], o.Name)
			}
		}
		return out, missing, nil
	}
	shared := map[string]bool{}
	for _, c := range []struct {
		s    *site
		kind string
		list func() client.ObjectList
	}{
		{a, "Export", func() client.ObjectList { return &straitv1.ExportList{} }},
		{b, "Import", func() client.ObjectList { return &straitv1.ImportList{} }},
	} {
		d := eventually(ctx, t, 30*time.Second, 100*time.Millisecond, fmt.Sprintf("site %s gives each %s a port", c.s.name, c.kind), func(ctx context.Context) (bool, string) {
			got, missing, err := ports(ctx, c.s, c.list())
			if err != nil {
				return false, err.Error()
			}
			for p, names := range got {
				if len(names) > 1 {
					shared[fmt.Sprintf("site %s: %s %v on port %d", c.s.name, c.kind, names, p)] = true
				}
			}
			return missing == 0, fmt.Sprintf("%d of %d have no port; ports %v", missing, n, got)
		})
		e.times.record(t, "ports/"+c.s.name, d)
	}
	for msg := range shared {
		t.Errorf("a gateway port was shared: %s", msg)
	}

	if err := a.c.DeleteAllOf(ctx, &straitv1.Export{}, client.InNamespace(webNamespace), client.MatchingLabels(label)); err != nil {
		t.Fatal(err)
	}
	if err := b.c.DeleteAllOf(ctx, &straitv1.Import{}, client.InNamespace(webNamespace), client.MatchingLabels(label)); err != nil {
		t.Fatal(err)
	}
}

// linkFailover: cutting the direct link moves site b's traffic to the wireguard link, and writes from b keep succeeding.
// Every new connection from b that starts 5s or more after the cut reaches cluster-a (RFC0005 requirement 4).
func (e *env) linkFailover(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
	}
	w, err := startWriter(ctx, b, 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop(context.Background())
	dialer, err := startDialer(ctx, b, 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Stop(context.Background())
	eventually(ctx, t, 30*time.Second, 200*time.Millisecond, "writes from b before the cut", func(context.Context) (bool, string) {
		n := 0
		for _, at := range w.attempts() {
			if at.ok && at.cluster == clusterOf(a) {
				n++
			}
		}
		return n >= 3, fmt.Sprintf("%d writes on %s", n, clusterOf(a))
	})

	cut := time.Now()
	if err := e.cutDirectLink(ctx); err != nil {
		t.Fatal(err)
	}
	direct, wan := a.name+"-direct", a.name+"-wan"
	d := eventually(ctx, t, 10*time.Second, 200*time.Millisecond, "site b fails over to the wireguard link", func(ctx context.Context) (bool, string) {
		links, err := linkState(ctx, b)
		if err != nil {
			return false, err.Error()
		}
		return !links[direct].Healthy && links[wan].Active, linkString(links)
	})
	e.times.record(t, "2/failover-in-status", d)
	switched := time.Now()
	eventually(ctx, t, 30*time.Second, 200*time.Millisecond, "five writes in a row from b over the wireguard link", func(context.Context) (bool, string) {
		run := 0
		for _, at := range w.attempts() {
			switch {
			case at.start.Before(switched):
			case at.ok:
				run++
			default:
				run = 0
			}
		}
		return run >= 5, fmt.Sprintf("%d writes in a row", run)
	})

	// New connections until 15s after the cut: ten seconds of them on the wireguard link.
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(time.Until(cut.Add(15 * time.Second))):
	}
	dials := dialer.Stop(ctx)
	e.checkNewConnections(t, dials, cut, clusterOf(a))

	if err := e.restore(ctx, cutDirect); err != nil {
		t.Fatal(err)
	}
	restored := time.Now()
	for _, s := range e.sites() {
		eventually(ctx, t, 30*time.Second, 200*time.Millisecond, "direct link active again", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
		e.times.record(t, "2/direct-active-again-"+s.name, time.Since(restored))
	}
	all := w.Stop(ctx)
	t.Logf("site b writes: %s", attemptSummary(all, cut))
	gap, failed := longestGap(all, cut)
	e.times.record(t, "2/write-gap", gap)
	t.Logf("site b: %d writes, %d failed after the cut", len(all), failed)
	for _, at := range all {
		if at.ok && at.cluster != clusterOf(a) {
			t.Errorf("a write from b landed on %s", at.cluster)
		}
	}
	// RFC0005 requirement 4 gives a new connection the next link within 5s; one write in flight may wait out its own 5s timeout.
	if gap > 10*time.Second {
		t.Errorf("writes from b stopped for %s across the failover, want under 10s", gap)
	}
}

// checkNewConnections checks RFC0005 requirement 4 end to end: every new connection that starts 5s or more after the
// cut reaches the cluster. It records when the last connection that failed started, from the cut.
func (e *env) checkNewConnections(t *testing.T, dials []attempt, cut time.Time, cluster string) {
	t.Helper()
	slices.SortFunc(dials, func(x, y attempt) int { return x.start.Compare(y.start) })
	var after []attempt
	for _, d := range dials {
		if !d.start.Before(cut) {
			after = append(after, d)
		}
	}
	t.Logf("site b new connections: %s", attemptSummary(after, cut))
	lastFailed, late := time.Duration(0), 0
	for _, d := range after {
		since := d.start.Sub(cut)
		if !d.ok || d.cluster != cluster {
			lastFailed = since
			continue
		}
		if since >= 5*time.Second {
			late++
		}
	}
	e.times.record(t, "2/new-connections-fail-until", lastFailed)
	if lastFailed >= 5*time.Second {
		t.Errorf("a new connection from b that started %.1fs after the cut did not reach %s, want every one from 5s on", lastFailed.Seconds(), cluster)
	}
	// One every 250ms from 5s to 15s after the cut.
	if late < 20 {
		t.Errorf("%d new connections from b reached %s 5s or more after the cut, want at least 20", late, cluster)
	}
}

// switchover: cluster-a is demoted and cluster-b promoted with a token. Neither site names a member after it stops
// being writable, beyond the bounds of the exchange, and both pass through nowhere.
func (e *env) switchover(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	for _, s := range e.sites() {
		eventually(ctx, t, 3*time.Minute, time.Second, clusterOf(s)+" healthy", clusterHealthy(s))
		eventually(ctx, t, 30*time.Second, time.Second, "site "+s.name+" routes pg-a", routeIs(s, dbNamespace, "pg", []string{memberOf(a)}, straitv1.ReasonActive))
	}
	u, err := getCluster(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	oldToken := field(u, "status", "demotionToken")

	samplers := map[*site]*sampler{a: startSampler(ctx, a, dbNamespace, "pg"), b: startSampler(ctx, b, dbNamespace, "pg")}
	watch := watchClusters(ctx, e.sites())
	defer watch.Stop()
	writers := map[*site]*writer{}
	for _, s := range e.sites() {
		w, err := startWriter(ctx, s, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Stop(context.Background())
		writers[s] = w
	}
	for _, s := range e.sites() {
		eventually(ctx, t, 30*time.Second, 200*time.Millisecond, "a write from site "+s.name+" before the move", func(context.Context) (bool, string) {
			for _, at := range writers[s].attempts() {
				if at.ok && at.cluster == clusterOf(a) {
					return true, ""
				}
			}
			return false, attemptSummary(writers[s].attempts(), time.Now())
		})
	}

	start := time.Now()
	if err := patchCluster(ctx, a, fmt.Sprintf(`{"spec":{"replica":{"primary":%q,"source":%q,"promotionToken":null}}}`, clusterOf(b), clusterOf(b))); err != nil {
		t.Fatal(err)
	}
	demoted := time.Now()
	t.Logf("demotion patch of %s accepted after %s", clusterOf(a), demoted.Sub(start).Round(time.Millisecond))
	token := ""
	d := eventually(ctx, t, 3*time.Minute, 250*time.Millisecond, clusterOf(a)+" demotion token", func(ctx context.Context) (bool, string) {
		u, err := getCluster(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		if tk := field(u, "status", "demotionToken"); tk != "" && tk != oldToken && field(u, "status", "switchReplicaClusterStatus", "inProgress") != "true" {
			token = tk
			return true, ""
		}
		return false, clusterString(u)
	})
	e.times.record(t, "3/demotion", d)

	promoteSent := time.Now()
	if err := patchCluster(ctx, b, fmt.Sprintf(`{"spec":{"replica":{"primary":%q,"source":%q,"promotionToken":%q}}}`, clusterOf(b), clusterOf(a), token)); err != nil {
		t.Fatal(err)
	}
	d = eventually(ctx, t, 3*time.Minute, 250*time.Millisecond, clusterOf(b)+" promoted", func(ctx context.Context) (bool, string) {
		u, err := getCluster(ctx, b)
		if err != nil {
			return false, err.Error()
		}
		return field(u, "status", "lastPromotionToken") == token, clusterString(u)
	})
	e.times.record(t, "3/promotion", d)
	for _, s := range e.sites() {
		eventually(ctx, t, 2*time.Minute, 250*time.Millisecond, "site "+s.name+" routes pg-b", routeIs(s, dbNamespace, "pg", []string{memberOf(b)}, straitv1.ReasonActive))
		e.times.record(t, "3/demotion-to-pg-b-"+s.name, time.Since(demoted))
	}
	routed := time.Now()
	for _, st := range watch.Stop() {
		t.Logf("%+.2fs %s", st.at.Sub(demoted).Seconds(), st.state)
	}

	// Site a sees the demotion on its next reconcile: one sample of slack. Site b learns it through the exchange,
	// within peerTimeout (RFC0004).
	slack := map[*site]time.Duration{a: sampleEvery, b: peerTimeout}
	for _, s := range e.sites() {
		samples := samplers[s].StopAfter(routed)
		t.Logf("site %s routes: %s", s.name, summary(samples, demoted))
		seq := ""
		lastA := time.Time{}
		for _, smp := range samples {
			switch {
			case smp.err != nil:
				t.Logf("site %s: sample at %+.1fs: %v", s.name, smp.start.Sub(demoted).Seconds(), smp.err)
				continue
			case smp.names(memberOf(a)):
				seq += "a"
				lastA = smp.start
				if smp.start.After(demoted.Add(slack[s])) {
					t.Errorf("site %s names %s at %+.1fs after its demotion was accepted, want none after %s",
						s.name, memberOf(a), smp.start.Sub(demoted).Seconds(), slack[s])
				}
			case smp.names(memberOf(b)):
				seq += "b"
				if smp.end.Before(promoteSent) {
					t.Errorf("site %s names %s at %+.1fs, before its promotion patch was sent", s.name, memberOf(b), smp.end.Sub(demoted).Seconds())
				}
			case smp.nowhere():
				seq += "n"
			default:
				seq += "?"
			}
		}
		// pg-b may leave the route again while CloudNativePG restarts it after the promotion; pg-a never comes back.
		if !regexp.MustCompile(`^a*n[nb]*$`).MatchString(seq) {
			t.Errorf("site %s routes in the order %q, want pg-a, then nowhere, then pg-b", s.name, seq)
		}
		if !lastA.IsZero() {
			t.Logf("site %s: last sample naming %s at %+.2fs", s.name, memberOf(a), lastA.Sub(demoted).Seconds())
		}
		changes, err := routeChanges(ctx, s, dbNamespace, "pg", start.Add(-time.Second))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range changes {
			t.Logf("site %s: %+.3fs %s", s.name, c.at.Sub(demoted).Seconds(), c.note)
		}
		if at, ok := firstChange(changes, "route "+memberOf(a)+" -> "); ok {
			e.times.record(t, "3/lag-off-pg-a-"+s.name, at.Sub(demoted))
		}
		left, okL := firstChange(changes, "route "+memberOf(a)+" -> nowhere")
		back, okB := firstChange(changes, "route nowhere -> "+memberOf(b))
		if okL && okB {
			e.times.record(t, "3/nowhere-"+s.name, back.Sub(left))
		}
	}

	// The data path: no write reached a read-only member once each site could know of the demotion.
	// kube-proxy needs up to a further 2s to apply an endpoint change.
	for _, s := range e.sites() {
		all := writers[s].Stop(ctx)
		within := 0
		for _, at := range all {
			if !at.readOnly() {
				continue
			}
			if at.start.After(demoted.Add(slack[s] + 2*time.Second)) {
				t.Errorf("site %s: a write at %+.1fs reached a read-only member: %s", s.name, at.start.Sub(demoted).Seconds(), at.out)
			} else {
				within++
			}
		}
		gap, failed := longestGap(all, demoted)
		t.Logf("site %s writes: %s", s.name, attemptSummary(all, demoted))
		t.Logf("site %s: %d writes, %d failed, %d read-only within the bound, longest gap %s", s.name, len(all), failed, within, gap.Round(time.Millisecond))
		e.times.record(t, "3/write-gap-"+s.name, gap)
	}

	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "a write from site "+s.name+" lands on cluster-b", writesLandOn(s, clusterOf(b)))
	}
	for _, s := range e.sites() {
		eventually(ctx, t, 5*time.Minute, time.Second, clusterOf(s)+" healthy", clusterHealthy(s))
	}
}

// partition: with both links cut, site b keeps its local primary, and site a holds pg-b for unknownHold, then routes nowhere.
func (e *env) partition(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	pgB := []string{memberOf(b)}
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
		eventually(ctx, t, 30*time.Second, time.Second, "site "+s.name+" routes pg-b", routeIs(s, dbNamespace, "pg", pgB, straitv1.ReasonActive))
	}
	spA, spB := startSampler(ctx, a, dbNamespace, "pg"), startSampler(ctx, b, dbNamespace, "pg")
	defer spA.Stop()
	defer spB.Stop()

	cut := time.Now()
	if err := e.cutDirectLink(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.cutWANPath(ctx); err != nil {
		t.Fatal(err)
	}
	d := eventually(ctx, t, peerTimeout+10*time.Second, 250*time.Millisecond, "site a holds pg-b",
		routeIs(a, dbNamespace, "pg", pgB, straitv1.ReasonHeldUnknown))
	e.times.record(t, "4/held-"+a.name, d)
	eventually(ctx, t, peerTimeout+unknownHold+10*time.Second, 250*time.Millisecond, "site a routes nowhere",
		routeIs(a, dbNamespace, "pg", nil, timedOut...))
	nowhere := time.Now()
	e.times.record(t, "4/nowhere-"+a.name, nowhere.Sub(cut))

	// No endpoint: kube-proxy refuses the connection at once. Retry while it applies the change.
	attempts := 0
	eventually(ctx, t, 15*time.Second, 500*time.Millisecond, "a connection from a is refused", func(ctx context.Context) (bool, string) {
		took, rc, out, err := connectTimed(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		attempts++
		if rc == 0 {
			t.Errorf("a connection from a succeeded while pg routes nowhere: %s", out)
		}
		if took >= 2*time.Second {
			t.Errorf("a connection from a took %s to fail, want under 2s: %s", took, out)
		}
		if strings.Contains(out, "refused") {
			e.times.record(t, "4/refused-in", took)
			return true, ""
		}
		return false, fmt.Sprintf("took %s: %s", took, out)
	})
	t.Logf("site a: refused after %d attempts", attempts)

	// The refusal can come within one sample of the route change, so wait for a sample of site a that shows it.
	sa, sb := spA.StopAfter(nowhere), spB.Stop()
	t.Logf("site a routes: %s", summary(sa, cut))
	t.Logf("site b routes: %s", summary(sb, cut))
	for _, smp := range sb {
		if smp.err != nil || !smp.names(memberOf(b)) || smp.reason != straitv1.ReasonActive {
			t.Errorf("site b at %+.1fs: route %s, reason %s, err %v; want pg-b, Active", smp.start.Sub(cut).Seconds(), routeString(smp.route), smp.reason, smp.err)
		}
	}
	seq := ""
	for _, smp := range sa {
		switch {
		case smp.err != nil:
			t.Errorf("site a at %+.1fs: %v", smp.start.Sub(cut).Seconds(), smp.err)
		case smp.names(memberOf(b)) && smp.reason == straitv1.ReasonActive:
			seq += "a"
		case smp.names(memberOf(b)) && smp.reason == straitv1.ReasonHeldUnknown:
			seq += "h"
		case smp.nowhere() && (smp.reason == timedOut[0] || smp.reason == timedOut[1]):
			seq += "n"
			// The last confirmation is at most one poll before the cut.
			if smp.start.Before(cut.Add(unknownHold - pollInterval)) {
				t.Errorf("site a routes nowhere at %+.1fs, before unknownHold %s passed", smp.start.Sub(cut).Seconds(), unknownHold)
			}
		default:
			seq += "?"
		}
	}
	if !regexp.MustCompile(`^a*h+n+$`).MatchString(seq) {
		t.Errorf("site a routes in the order %q, want pg-b Active, then pg-b HeldUnknown, then nowhere", seq)
	}

	healed := time.Now()
	if err := e.restore(ctx, cutDirect); err != nil {
		t.Fatal(err)
	}
	if err := e.restore(ctx, cutWAN); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 15*time.Second, 250*time.Millisecond, "site a routes pg-b again", routeIs(a, dbNamespace, "pg", pgB, straitv1.ReasonActive))
	e.times.record(t, "4/heal-to-pg-b-"+a.name, time.Since(healed))
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
		eventually(ctx, t, 3*time.Minute, time.Second, clusterOf(s)+" healthy", clusterHealthy(s))
	}
}

// isolationTimeout of the isolated step: above peerTimeout and a few poll intervals, as RFC0002 advises.
const isolationTimeout = 8 * time.Second

// setIsolation sets pg's isolationTimeout at a site. A negative duration removes the field.
func setIsolation(ctx context.Context, e *env, s *site, d time.Duration) error {
	value := "null"
	if d >= 0 {
		value = fmt.Sprintf("%q", d.String())
	}
	patch := fmt.Sprintf(`{"spec":{"policy":{"isolationTimeout":%s}}}`, value)
	return s.c.Patch(ctx, pgService(e), client.RawPatch(types.MergePatchType, []byte(patch)))
}

// isolated: the partition again, with pg's isolationTimeout set at both sites (RFC0002, requirements 44 to 47). Site
// b, cut off from every peer, routes pg-b until isolationTimeout has passed since it last read site a, then nowhere
// with SiteIsolated, and a connection from b is refused. Setting the field to 0s routes pg-b again: the site-loss
// runbook's way out. After the heal, b routes pg-b with the policy on. Step 4 is the same partition with the policy
// off: b keeps pg-b throughout.
func (e *env) isolated(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	pgB := []string{memberOf(b)}
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
		eventually(ctx, t, 30*time.Second, time.Second, "site "+s.name+" routes pg-b", routeIs(s, dbNamespace, "pg", pgB, straitv1.ReasonActive))
	}
	for _, s := range e.sites() {
		if err := setIsolation(ctx, e, s, isolationTimeout); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, s := range e.sites() {
			if err := setIsolation(context.WithoutCancel(ctx), e, s, -1); err != nil {
				t.Errorf("site %s: remove isolationTimeout: %v", s.name, err)
			}
		}
	}()
	// The policy changes nothing while the sites see each other.
	for _, s := range e.sites() {
		eventually(ctx, t, 10*time.Second, 500*time.Millisecond, "site "+s.name+" still routes pg-b", routeIs(s, dbNamespace, "pg", pgB, straitv1.ReasonActive))
	}
	sp := startSampler(ctx, b, dbNamespace, "pg")
	defer sp.Stop()

	cut := time.Now()
	if err := e.cutDirectLink(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.cutWANPath(ctx); err != nil {
		t.Fatal(err)
	}
	d := eventually(ctx, t, isolationTimeout+15*time.Second, 250*time.Millisecond, "site b routes nowhere, isolated",
		routeIs(b, dbNamespace, "pg", nil, straitv1.ReasonSiteIsolated))
	isolatedAt := time.Now()
	e.times.record(t, "4b/isolated-"+b.name, d)
	eventually(ctx, t, 15*time.Second, 500*time.Millisecond, "a connection from b is refused", func(ctx context.Context) (bool, string) {
		took, rc, out, err := connectTimed(ctx, b)
		if err != nil {
			return false, err.Error()
		}
		if rc == 0 {
			return false, "connected: " + out
		}
		if strings.Contains(out, "refused") {
			e.times.record(t, "4b/refused-in", took)
			return true, ""
		}
		return false, fmt.Sprintf("took %s: %s", took, out)
	})
	// Site a, whose member is remote, routes as in step 4.
	eventually(ctx, t, peerTimeout+unknownHold+10*time.Second, 250*time.Millisecond, "site a routes nowhere",
		routeIs(a, dbNamespace, "pg", nil, timedOut...))

	samples := sp.StopAfter(isolatedAt)
	t.Logf("site b routes: %s", summary(samples, cut))
	seq := ""
	for _, smp := range samples {
		switch {
		case smp.err != nil:
			t.Errorf("site b at %+.1fs: %v", smp.start.Sub(cut).Seconds(), smp.err)
		case smp.names(memberOf(b)) && smp.reason == straitv1.ReasonActive:
			seq += "a"
		case smp.nowhere() && smp.reason == straitv1.ReasonSiteIsolated:
			seq += "i"
			// The last read of site a is at most one poll before the cut.
			if smp.end.Before(cut.Add(isolationTimeout - pollInterval)) {
				t.Errorf("site b is isolated at %+.1fs, before isolationTimeout %s passed", smp.end.Sub(cut).Seconds(), isolationTimeout)
			}
		default:
			seq += "?"
		}
	}
	if !regexp.MustCompile(`^a+i+$`).MatchString(seq) {
		t.Errorf("site b routes in the order %q, want pg-b Active, then nowhere with SiteIsolated", seq)
	}

	// The runbook's way out: 0s turns the policy off, and b routes its primary again. Then the policy is set again.
	if err := setIsolation(ctx, e, b, 0); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 10*time.Second, 250*time.Millisecond, "site b routes pg-b with the policy off", routeIs(b, dbNamespace, "pg", pgB, straitv1.ReasonActive))
	if err := setIsolation(ctx, e, b, isolationTimeout); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 10*time.Second, 250*time.Millisecond, "site b is isolated again", routeIs(b, dbNamespace, "pg", nil, straitv1.ReasonSiteIsolated))

	healed := time.Now()
	if err := e.restore(ctx, cutDirect); err != nil {
		t.Fatal(err)
	}
	if err := e.restore(ctx, cutWAN); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.sites() {
		eventually(ctx, t, 15*time.Second, 250*time.Millisecond, "site "+s.name+" routes pg-b again", routeIs(s, dbNamespace, "pg", pgB, straitv1.ReasonActive))
	}
	e.times.record(t, "4b/heal-to-pg-b", time.Since(healed))
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
	}
}

// declarations: sites that declare different members route nowhere; once they agree, both route to the declared member.
func (e *env) declarations(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	for _, s := range e.sites() {
		if _, err := s.kubectl(ctx, 3*time.Minute, "-n", webNamespace, "rollout", "status", "deployment/"+webOf(s), "--timeout=2m"); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range e.sites() {
		if err := s.c.Create(ctx, webService(e, webOf(s))); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range e.sites() {
		d := eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "site "+s.name+" routes web nowhere", func(ctx context.Context) (bool, string) {
			ok, state := routeIs(s, webNamespace, "web", nil, straitv1.ReasonDeclarationsDisagree)(ctx)
			if !ok {
				return false, state
			}
			ss, err := siteService(ctx, s, webNamespace, "web")
			if err != nil {
				return false, err.Error()
			}
			return conditionTrue(ss, straitv1.ConditionDeclarationsDisagree), state
		})
		e.times.record(t, "5/disagree-"+s.name, d)
	}

	ss, err := siteService(ctx, b, webNamespace, "web")
	if err != nil {
		t.Fatal(err)
	}
	base := ss.DeepCopy()
	ss.Spec.Plugin.Declared.Active = webOf(a)
	agreed := time.Now()
	if err := b.c.Patch(ctx, ss, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.sites() {
		eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "site "+s.name+" routes web to web-a", func(ctx context.Context) (bool, string) {
			ok, state := routeIs(s, webNamespace, "web", []string{webOf(a)}, straitv1.ReasonActive)(ctx)
			if !ok {
				return false, state
			}
			ss, err := siteService(ctx, s, webNamespace, "web")
			if err != nil {
				return false, err.Error()
			}
			return !conditionTrue(ss, straitv1.ConditionDeclarationsDisagree), state
		})
		e.times.record(t, "5/agree-to-route-"+s.name, time.Since(agreed))
	}
	for _, s := range e.sites() {
		eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "a request from site "+s.name+" reaches web-a", requestReaches(s, "web", webOf(a)))
	}
}

// siteLoss: site b is lost. Site a holds pg-b, then routes nowhere; the operator promotes cluster-a without a token,
// and site a routes to it.
func (e *env) siteLoss(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	pgB := []string{memberOf(b)}
	for _, s := range e.sites() {
		eventually(ctx, t, 30*time.Second, time.Second, "site "+s.name+" routes pg-b", routeIs(s, dbNamespace, "pg", pgB, straitv1.ReasonActive))
	}
	sp := startSampler(ctx, a, dbNamespace, "pg")
	defer sp.Stop()

	down := time.Now()
	if err := e.siteDown(ctx, b); err != nil {
		t.Fatal(err)
	}
	d := eventually(ctx, t, peerTimeout+10*time.Second, 250*time.Millisecond, "site a holds pg-b", routeIs(a, dbNamespace, "pg", pgB, straitv1.ReasonHeldUnknown))
	e.times.record(t, "6/held-"+a.name, d)
	eventually(ctx, t, peerTimeout+unknownHold+10*time.Second, 250*time.Millisecond, "site a routes nowhere", routeIs(a, dbNamespace, "pg", nil, timedOut...))
	e.times.record(t, "6/nowhere-"+a.name, time.Since(down))

	promoted := time.Now()
	if err := patchCluster(ctx, a, fmt.Sprintf(`{"spec":{"replica":{"primary":%q}}}`, clusterOf(a))); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 3*time.Minute, 250*time.Millisecond, "site a routes pg-a", routeIs(a, dbNamespace, "pg", []string{memberOf(a)}, straitv1.ReasonActive))
	e.times.record(t, "6/promotion-to-pg-a-"+a.name, time.Since(promoted))
	routed := time.Now()
	eventually(ctx, t, time.Minute, time.Second, "a write from site a lands on cluster-a", writesLandOn(a, clusterOf(a)))

	samples := sp.StopAfter(routed)
	t.Logf("site a routes: %s", summary(samples, down))
	seq := ""
	for _, smp := range samples {
		switch {
		case smp.err != nil:
			t.Errorf("site a at %+.1fs: %v", smp.start.Sub(down).Seconds(), smp.err)
		case smp.names(memberOf(b)) && smp.reason == straitv1.ReasonActive:
			seq += "b"
		case smp.names(memberOf(b)) && smp.reason == straitv1.ReasonHeldUnknown:
			seq += "h"
		case smp.nowhere():
			seq += "n"
		case smp.names(memberOf(a)):
			seq += "a"
			if smp.end.Before(promoted) {
				t.Errorf("site a names pg-a at %+.1fs, before the promotion", smp.end.Sub(down).Seconds())
			}
		default:
			seq += "?"
		}
	}
	// pg-a may leave the route again while CloudNativePG restarts it after the promotion.
	if !regexp.MustCompile(`^b*h+n+[an]*$`).MatchString(seq) {
		t.Errorf("site a routes in the order %q, want pg-b, then pg-b HeldUnknown, then nowhere, then pg-a", seq)
	}
}

// twoWritable: site b comes back with cluster-b still primary, still cut off from site a, and its controller restarts.
// With pg's startupHold, b does not route to its local primary before it has read site a, until the hold ends; then it
// routes as without the hold. Once the cut is removed, both members are writable, so both sites route nowhere.
func (e *env) twoWritable(ctx context.Context, t *testing.T) {
	b := e.b
	e.unpause(ctx, b)
	eventually(ctx, t, time.Minute, time.Second, "site b's API answers", func(ctx context.Context) (bool, string) {
		_, err := siteService(ctx, b, dbNamespace, "pg")
		return err == nil, fmt.Sprint(err)
	})
	hold := fmt.Sprintf(`{"spec":{"policy":{"startupHold":%q}}}`, startupHold)
	if err := b.c.Patch(ctx, pgService(e), client.RawPatch(types.MergePatchType, []byte(hold))); err != nil {
		t.Fatal(err)
	}
	// Each start holds anew: the time the controller began to poll, and the peers read since, are in memory only
	// (RFC0002 requirement 32). The second restart comes after a hold has ended, with pg-b routed.
	e.restartHeld(ctx, t, "7/awaiting-")
	e.restartHeld(ctx, t, "7/awaiting-again-")

	up := time.Now()
	if err := e.removeCut(ctx, b.node, cutSite); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "both sites route nowhere with Multiple", func(ctx context.Context) (bool, string) {
		ok := true
		var states []string
		for _, s := range e.sites() {
			ss, err := siteService(ctx, s, dbNamespace, "pg")
			if err != nil {
				ok = false
				states = append(states, "site "+s.name+": "+err.Error())
				continue
			}
			ok = ok && len(ss.Status.Route.Members) == 0 && conditionTrue(ss, straitv1.ConditionMultiple)
			states = append(states, "site "+s.name+": "+describe(ss))
		}
		return ok, strings.Join(states, " | ")
	})
	e.times.record(t, "7/multiple", time.Since(up))
	for _, s := range e.sites() {
		ss, err := siteService(ctx, s, dbNamespace, "pg")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("site %s: %s", s.name, describe(ss))
	}
}

// restartHeld restarts site b's controller while b is cut off from site a, with pg's startupHold. The new controller
// routes nowhere with AwaitingPeers from its first decision, never routes pg-b before the hold ends, then routes pg-b.
// The time to AwaitingPeers is recorded under the label and the site's name.
func (e *env) restartHeld(ctx context.Context, t *testing.T, label string) {
	t.Helper()
	b := e.b
	sp := startSampler(ctx, b, dbNamespace, "pg")
	defer sp.Stop()
	restarted := time.Now()
	if _, err := b.kubectl(ctx, time.Minute, "--namespace", straitNamespace, "delete", "pod", "--selector", "app.kubernetes.io/component=controller", "--wait=false"); err != nil {
		t.Fatal(err)
	}
	d := eventually(ctx, t, 2*time.Minute, 250*time.Millisecond, "site b awaits site a", routeIs(b, dbNamespace, "pg", nil, straitv1.ReasonAwaitingPeers))
	e.times.record(t, label+b.name, d)
	eventually(ctx, t, startupHold+30*time.Second, 250*time.Millisecond, "site b routes pg-b once the hold ends", routeIs(b, dbNamespace, "pg", []string{memberOf(b)}, straitv1.ReasonActive))
	ended := time.Now()

	// The new controller starts holding at its first decision. From then, pg-b is never routed until the hold ends:
	// the hold counts from the first poll, a moment before the first decision.
	samples := sp.StopAfter(ended)
	t.Logf("site b routes: %s", summary(samples, restarted))
	seq, first := "", time.Time{}
	for _, smp := range samples {
		switch {
		case smp.err != nil:
			seq += "e"
		case smp.nowhere() && smp.reason == straitv1.ReasonAwaitingPeers:
			seq += "w"
			if first.IsZero() {
				first = smp.start
			}
		case smp.nowhere():
			seq += "n"
		case smp.names(memberOf(b)):
			seq += "b"
			if !first.IsZero() && smp.end.Sub(first) < startupHold-10*time.Second {
				t.Errorf("site b routes pg-b at %+.1fs, %.1fs after it began to await site a", smp.end.Sub(restarted).Seconds(), smp.end.Sub(first).Seconds())
			}
		default:
			seq += "?"
		}
	}
	// Before the first decision of the new controller, the route is the old controller's, pg-b.
	if !regexp.MustCompile(`^[ben]*w[wn]*b+$`).MatchString(seq) {
		t.Errorf("site b routes in the order %q, want pg-b at most until the restart, then AwaitingPeers, then pg-b", seq)
	}
}
