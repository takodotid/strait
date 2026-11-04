//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
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
