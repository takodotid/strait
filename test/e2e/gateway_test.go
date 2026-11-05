//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// gatewayState returns site s's active gateway pod and address, and the number of ready gateway pods.
func gatewayState(ctx context.Context, s *site) (active, addr string, ready int, err error) {
	var st straitv1.Site
	if err := s.c.Get(ctx, types.NamespacedName{Name: s.name}, &st); err != nil {
		return "", "", 0, err
	}
	var pods corev1.PodList
	if err := s.c.List(ctx, &pods, client.InNamespace(straitNamespace), client.MatchingLabels{"app.kubernetes.io/component": "gateway"}); err != nil {
		return "", "", 0, err
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && p.DeletionTimestamp == nil {
				ready++
			}
		}
	}
	if !meta.IsStatusConditionTrue(st.Status.Conditions, "GatewayActive") {
		return st.Status.Gateway.ActivePod, "", ready, nil
	}
	return st.Status.Gateway.ActivePod, st.Status.Gateway.Address, ready, nil
}

// envoyTask pauses or resumes the Envoy container of a pod on site s's node.
func (e *env) envoyTask(ctx context.Context, s *site, pod, verb string) error {
	id, err := e.docker(ctx, 30*time.Second, "exec", s.node, "crictl", "ps", "-q", "--name", "envoy", "--label", "io.kubernetes.pod.name="+pod)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "\n") {
		return fmt.Errorf("pod %s: Envoy containers %q, want one", pod, id)
	}
	_, err = e.docker(ctx, 30*time.Second, "exec", s.node, "ctr", "-n", "k8s.io", "task", verb, id)
	return err
}

// envoyNotReady: the Envoy of site a's active gateway replica freezes. Its agent steps down, the standby takes the
// gateway Lease, and writes from site b land on cluster-a again (issue #35).
func (e *env) envoyNotReady(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	var active string
	eventually(ctx, t, 2*time.Minute, time.Second, "site a has an active gateway and a ready standby", func(ctx context.Context) (bool, string) {
		pod, addr, ready, err := gatewayState(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		active = pod
		return addr != "" && ready == 2, fmt.Sprintf("active %q at %q, %d ready", pod, addr, ready)
	})
	eventually(ctx, t, time.Minute, time.Second, "a write from site b lands on cluster-a", writesLandOn(b, clusterOf(a)))

	if err := e.envoyTask(ctx, a, active, "pause"); err != nil {
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			if err := e.envoyTask(context.Background(), a, active, "resume"); err != nil {
				t.Errorf("resume Envoy of %s: %v", active, err)
			}
		}
	}
	defer resume()
	d := eventually(ctx, t, 30*time.Second, 250*time.Millisecond, "a standby takes site a's gateway", func(ctx context.Context) (bool, string) {
		pod, addr, _, err := gatewayState(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return pod != active && addr != "", fmt.Sprintf("active %q at %q, was %q", pod, addr, active)
	})
	e.times.record(t, "envoy/handover", d)
	d = eventually(ctx, t, time.Minute, time.Second, "a write from site b lands on cluster-a after the handover", writesLandOn(b, clusterOf(a)))
	e.times.record(t, "envoy/first-write", d)

	resume()
	eventually(ctx, t, time.Minute, time.Second, "both gateway replicas at site a are ready again", func(ctx context.Context) (bool, string) {
		pod, addr, ready, err := gatewayState(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return addr != "" && ready == 2, fmt.Sprintf("active %q at %q, %d ready", pod, addr, ready)
	})
}

// handoverRecovery bounds the time from a gateway takeover at one site until writes from the other site land again
// over a wireguard link. On kind it took 1.1 to 4.8s, and 15.8 to 17.7s without the drop of a stale session (RFC0005,
// requirement 37).
const handoverRecovery = 8 * time.Second

// gatewayLease returns the holder of site s's gateway Lease and when it took it.
func gatewayLease(ctx context.Context, s *site) (string, time.Time, error) {
	var l coordinationv1.Lease
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: straitNamespace, Name: "strait-gateway"}, &l); err != nil {
		return "", time.Time{}, err
	}
	var at time.Time
	if l.Spec.AcquireTime != nil {
		at = l.Spec.AcquireTime.Time
	}
	return ptr.Deref(l.Spec.HolderIdentity, ""), at, nil
}

// handshake returns the last WireGuard handshake that site s's agent reports on its link.
func handshake(ctx context.Context, s *site, link string) string {
	links, err := linkState(ctx, s)
	if err != nil {
		return err.Error()
	}
	if hs := links[link].LastHandshakeTime; hs != nil {
		return hs.UTC().Format(time.TimeOnly)
	}
	return "none"
}

// gatewayHandover: with the direct link cut, site b reaches site a over the wireguard link only, which site a's side
// waits on. Site a's active gateway pod is deleted and the standby takes the gateway Lease. A write from site b lands on
// cluster-a within handoverRecovery of the takeover (issue #44).
func (e *env) gatewayHandover(ctx context.Context, t *testing.T) {
	a, b := e.a, e.b
	direct, wan := a.name+"-direct", a.name+"-wan"
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
	}
	eventually(ctx, t, 2*time.Minute, time.Second, "site a has an active gateway and a ready standby", func(ctx context.Context) (bool, string) {
		pod, addr, ready, err := gatewayState(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return addr != "" && ready == 2, fmt.Sprintf("active %q at %q, %d ready", pod, addr, ready)
	})
	eventually(ctx, t, time.Minute, time.Second, "a write from site b lands on cluster-a", writesLandOn(b, clusterOf(a)))
	if err := e.cutDirectLink(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 10*time.Second, 200*time.Millisecond, "site b uses the wireguard link", func(ctx context.Context) (bool, string) {
		links, err := linkState(ctx, b)
		if err != nil {
			return false, err.Error()
		}
		return !links[direct].Healthy && links[wan].Active, linkString(links)
	})
	w, err := startWriter(ctx, b, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop(context.Background())
	eventually(ctx, t, 30*time.Second, 200*time.Millisecond, "writes from b over the wireguard link", func(context.Context) (bool, string) {
		n := 0
		for _, at := range w.attempts() {
			if at.ok && at.cluster == clusterOf(a) {
				n++
			}
		}
		return n >= 3, fmt.Sprintf("%d writes on %s", n, clusterOf(a))
	})

	old, _, err := gatewayLease(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	before := handshake(ctx, b, wan)
	deleted := time.Now()
	if err := a.c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: straitNamespace, Name: old}}); err != nil {
		t.Fatal(err)
	}
	// The Lease's acquire time comes from the new holder's clock, and the writes' times from the client pod's. Every kind
	// node shares the host's clock.
	var holder string
	var takeover time.Time
	eventually(ctx, t, 30*time.Second, 100*time.Millisecond, "a standby takes site a's gateway Lease", func(ctx context.Context) (bool, string) {
		var err error
		holder, takeover, err = gatewayLease(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return holder != "" && holder != old && !takeover.IsZero(), fmt.Sprintf("holder %q, was %q", holder, old)
	})
	e.times.record(t, "handover/takeover", takeover.Sub(deleted))
	var recovered time.Time
	eventually(ctx, t, time.Minute, 100*time.Millisecond, "a write from b lands on cluster-a after the takeover", func(context.Context) (bool, string) {
		for _, at := range w.attempts() {
			if at.ok && at.cluster == clusterOf(a) && at.end.After(takeover) {
				recovered = at.end
				return true, ""
			}
		}
		return false, "no write since the takeover"
	})
	recovery := recovered.Sub(takeover)
	e.times.record(t, "handover/recovery", recovery)
	after := handshake(ctx, b, wan)
	all := w.Stop(ctx)
	gap, failed := longestGap(all, deleted)
	e.times.record(t, "handover/write-gap", gap)
	t.Logf("site a: %s took the gateway Lease from %s at %s, %s after the delete", holder, old, takeover.UTC().Format(time.StampMilli), takeover.Sub(deleted).Round(time.Millisecond))
	t.Logf("site b: last handshake on %s %s before the delete, %s after the recovery", wan, before, after)
	t.Logf("site b writes, from the delete: %s; %d failed", attemptSummary(all, deleted), failed)

	if err := e.restore(ctx, cutDirect); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady again", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
	}
	eventually(ctx, t, 2*time.Minute, time.Second, "site a has a ready standby again", func(ctx context.Context) (bool, string) {
		pod, addr, ready, err := gatewayState(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return addr != "" && ready == 2, fmt.Sprintf("active %q at %q, %d ready", pod, addr, ready)
	})
	if recovery > handoverRecovery {
		t.Errorf("writes from site b recovered %s after the takeover, want at most %s", recovery.Round(time.Millisecond), handoverRecovery)
	}
}

// gatewayPods returns site s's gateway pods by name.
func gatewayPods(ctx context.Context, s *site) (map[string]corev1.Pod, error) {
	var pods corev1.PodList
	if err := s.c.List(ctx, &pods, client.InNamespace(straitNamespace), client.MatchingLabels{"app.kubernetes.io/component": "gateway"}); err != nil {
		return nil, err
	}
	out := map[string]corev1.Pod{}
	for _, p := range pods.Items {
		out[p.Name] = p
	}
	return out, nil
}

// leaseTransitions returns how many times site s's gateway Lease has changed holder.
func leaseTransitions(ctx context.Context, s *site) (int32, error) {
	var l coordinationv1.Lease
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: straitNamespace, Name: "strait-gateway"}, &l); err != nil {
		return 0, err
	}
	return ptr.Deref(l.Spec.LeaseTransitions, 0), nil
}

// gatewayRollout: a restart of site a's gateway Deployment, as an upgrade rolls it, moves the gateway Lease once. The
// active pod has a deletion cost, so the Deployment stops the standby first (RFC0005, requirement 39).
func (e *env) gatewayRollout(ctx context.Context, t *testing.T) {
	a := e.a
	const deletionCost = "controller.kubernetes.io/pod-deletion-cost"
	eventually(ctx, t, 2*time.Minute, time.Second, "site a has an active gateway and a ready standby, and only the active pod has a deletion cost", func(ctx context.Context) (bool, string) {
		active, addr, ready, err := gatewayState(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		pods, err := gatewayPods(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		costs := map[string]string{}
		ok := addr != "" && ready == 2
		for name, p := range pods {
			costs[name] = p.Annotations[deletionCost]
			want := ""
			if name == active {
				want = "1"
			}
			if costs[name] != want {
				ok = false
			}
		}
		return ok, fmt.Sprintf("active %q at %q, %d ready, deletion costs %v", active, addr, ready, costs)
	})
	before, err := leaseTransitions(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	old, err := gatewayPods(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	restart := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, start.UTC().Format(time.RFC3339))
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: straitNamespace, Name: "strait-gateway"}}
	if err := a.c.Patch(ctx, deploy, client.RawPatch(types.StrategicMergePatchType, []byte(restart))); err != nil {
		t.Fatal(err)
	}
	eventually(ctx, t, 4*time.Minute, time.Second, "every old gateway pod is gone, and a new one is active", func(ctx context.Context) (bool, string) {
		pods, err := gatewayPods(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		left := 0
		for name := range pods {
			if _, ok := old[name]; ok {
				left++
			}
		}
		active, addr, ready, err := gatewayState(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		_, wasOld := old[active]
		return left == 0 && !wasOld && addr != "" && ready == 2, fmt.Sprintf("%d old pods left; active %q at %q, %d ready", left, active, addr, ready)
	})
	after, err := leaseTransitions(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	e.times.record(t, "rollout/duration", time.Since(start))
	t.Logf("site a: the gateway rollout moved the Lease %d times", after-before)
	if after-before != 1 {
		t.Errorf("the gateway rollout moved the gateway Lease %d times, want 1", after-before)
	}
	for _, s := range e.sites() {
		eventually(ctx, t, time.Minute, time.Second, "links steady after the rollout", func(ctx context.Context) (bool, string) { return e.linksSteady(ctx, s) })
	}
}
