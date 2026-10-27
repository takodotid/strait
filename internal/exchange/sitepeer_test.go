package exchange

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/takodotid/strait/api/v1alpha1"
)

func TestStatusWriter(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&v1alpha1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "b"}}, &v1alpha1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "c"}}).
		WithStatusSubresource(&v1alpha1.SitePeer{}).
		Build()
	timeout := 6 * time.Second
	polled := time.Now()
	p := &Poller{Exchange: exchangeSpec(2*time.Second, timeout)}
	p.peers = map[string]*peer{
		"b": {state: PeerState{LastSuccess: polled, LastError: "unexpected status 503", LastErrorTime: polled.Add(-time.Second)}, document: siteDocument("b", obs("pg-b", "b"))},
	}
	w := &StatusWriter{Client: c, Poller: p}
	ctx := context.Background()
	get := func(name string) v1alpha1.SitePeer {
		t.Helper()
		var sp v1alpha1.SitePeer
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &sp); err != nil {
			t.Fatal(err)
		}
		return sp
	}

	if err := w.sync(ctx, polled.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	b := get("b")
	st := b.Status
	if !st.Reachable || st.Services != 1 || st.Exports != 1 || st.LastError != "unexpected status 503" {
		t.Errorf("status: %+v", st)
	}
	if st.LastSeenTime == nil || !st.LastSeenTime.Equal(&metav1.Time{Time: polled.Truncate(time.Second)}) {
		t.Errorf("lastSeenTime: %v, want %v", st.LastSeenTime, polled.Truncate(time.Second))
	}
	cond := meta.FindStatusCondition(st.Conditions, ConditionReachable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonFresh {
		t.Errorf("condition: %+v", cond)
	}
	cs := get("c").Status
	if cs.Reachable || cs.LastSeenTime != nil {
		t.Errorf("peer never polled: %+v", cs)
	}
	if cond := meta.FindStatusCondition(cs.Conditions, ConditionReachable); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonNoDocument {
		t.Errorf("condition of a peer never polled: %+v", cond)
	}

	// Nothing changed: no write.
	if err := w.sync(ctx, polled.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if rv := get("b").ResourceVersion; rv != b.ResourceVersion {
		t.Errorf("status written without a change: resourceVersion %s, then %s", b.ResourceVersion, rv)
	}

	// The peer goes stale.
	if err := w.sync(ctx, polled.Add(timeout+time.Second)); err != nil {
		t.Fatal(err)
	}
	st = get("b").Status
	cond = meta.FindStatusCondition(st.Conditions, ConditionReachable)
	if st.Reachable || cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonStale {
		t.Errorf("stale peer: %+v", st)
	}
	if st.LastSeenTime == nil || st.Services != 1 {
		t.Errorf("stale peer lost its last document: %+v", st)
	}
}

func TestPeerStatusMismatch(t *testing.T) {
	now := time.Now()
	st := peerStatus(v1alpha1.SitePeerStatus{}, 3, Document{}, PeerState{LastSuccess: now, LastError: "site mismatch: document is from \"c\"", Mismatch: true}, now, time.Minute)
	cond := meta.FindStatusCondition(st.Conditions, ConditionReachable)
	if st.Reachable || cond == nil || cond.Reason != ReasonSiteMismatch || cond.ObservedGeneration != 3 {
		t.Errorf("status: %+v", st)
	}
}
