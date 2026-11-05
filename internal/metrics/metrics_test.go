package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	straitv1 "github.com/takodotid/strait/api/v1"
)

func objects() []client.Object {
	ss := &straitv1.SiteService{ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg"}}
	ss.Status.Route = straitv1.Route{Reason: straitv1.ReasonMultipleWritable}
	ss.Status.Conditions = []metav1.Condition{{Type: straitv1.ConditionMultiple, Status: metav1.ConditionTrue}}
	seen := metav1.NewTime(time.Unix(1700000000, 0))
	peer := &straitv1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Status: straitv1.SitePeerStatus{Reachable: true, LastSeenTime: &seen}}
	link := &straitv1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: "b-direct"}, Spec: straitv1.SiteLinkSpec{Peer: "b", Transport: straitv1.TransportDirect}, Status: straitv1.SiteLinkStatus{Healthy: true}}
	site := &straitv1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: straitv1.SiteStatus{Conditions: []metav1.Condition{
		{Type: "GatewayActive", Status: metav1.ConditionTrue},
		{Type: "GatewayConfig", Status: metav1.ConditionFalse},
	}}}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "strait-system", Name: "strait-gateway"}, Spec: coordinationv1.LeaseSpec{LeaseTransitions: ptr.To[int32](4)}}
	other := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "strait-system", Name: "strait-controller"}, Spec: coordinationv1.LeaseSpec{LeaseTransitions: ptr.To[int32](9)}}
	return []client.Object{ss, peer, link, site, lease, other}
}

func reader(t *testing.T, funcs interceptor.Funcs) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := straitv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects()...).WithInterceptorFuncs(funcs).Build()
}

func TestCollect(t *testing.T) {
	decided := time.Unix(1700000100, 500_000_000)
	c := &Collector{
		Reader:           reader(t, interceptor.Funcs{}),
		GatewayNamespace: "strait-system",
		Decisions: func() map[types.NamespacedName]time.Time {
			return map[types.NamespacedName]time.Time{{Namespace: "db", Name: "pg"}: decided, {Namespace: "db", Name: "gone"}: decided}
		},
	}
	want := `
# HELP strait_siteservice_condition 1 while the condition is True.
# TYPE strait_siteservice_condition gauge
strait_siteservice_condition{condition="Multiple",namespace="db",siteservice="pg"} 1
# HELP strait_siteservice_endpoints Endpoints written for the application name. 0 means the route is nowhere.
# TYPE strait_siteservice_endpoints gauge
strait_siteservice_endpoints{namespace="db",siteservice="pg"} 0
# HELP strait_peer_reachable 1 while the peer's observation document is fresh.
# TYPE strait_peer_reachable gauge
strait_peer_reachable{peer="b"} 1
# HELP strait_link_active 1 while the link carries the pair's traffic.
# TYPE strait_link_active gauge
strait_link_active{link="b-direct",peer="b",transport="direct"} 0
# HELP strait_site_condition 1 while the condition of the local Site is True.
# TYPE strait_site_condition gauge
strait_site_condition{condition="GatewayActive"} 1
strait_site_condition{condition="GatewayConfig"} 0
# HELP strait_peer_last_seen_timestamp_seconds When the peer's document was last read, in Unix seconds.
# TYPE strait_peer_last_seen_timestamp_seconds gauge
strait_peer_last_seen_timestamp_seconds{peer="b"} 1.7e+09
# HELP strait_siteservice_last_decision_timestamp_seconds When the elected controller last decided the route of the application name, in Unix seconds.
# TYPE strait_siteservice_last_decision_timestamp_seconds gauge
strait_siteservice_last_decision_timestamp_seconds{namespace="db",siteservice="pg"} 1.7000001005e+09
# HELP strait_gateway_lease_transitions_total Times a gateway replica took the gateway Lease from another holder, or after a release, from the Lease.
# TYPE strait_gateway_lease_transitions_total counter
strait_gateway_lease_transitions_total 4
# HELP strait_status_read_failed 1 when this scrape could not read the objects of the kind, whose metrics are then missing.
# TYPE strait_status_read_failed gauge
strait_status_read_failed{kind="leases"} 0
strait_status_read_failed{kind="siteservices"} 0
strait_status_read_failed{kind="sitelinks"} 0
strait_status_read_failed{kind="sitepeers"} 0
strait_status_read_failed{kind="sites"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"strait_siteservice_condition", "strait_siteservice_endpoints", "strait_peer_reachable", "strait_link_active", "strait_site_condition",
		"strait_peer_last_seen_timestamp_seconds", "strait_siteservice_last_decision_timestamp_seconds", "strait_gateway_lease_transitions_total",
		"strait_status_read_failed"); err != nil {
		t.Fatal(err)
	}
}

// A kind that cannot be read shows as 1 in strait_status_read_failed, so that its missing metrics are not read as
// zero. The other kinds keep their metrics.
func TestCollectReadFailed(t *testing.T) {
	c := &Collector{Reader: reader(t, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*straitv1.SitePeerList); ok {
				return errors.New("no")
			}
			return cl.List(ctx, list, opts...)
		},
	})}
	want := `
# HELP strait_status_read_failed 1 when this scrape could not read the objects of the kind, whose metrics are then missing.
# TYPE strait_status_read_failed gauge
strait_status_read_failed{kind="siteservices"} 0
strait_status_read_failed{kind="sitelinks"} 0
strait_status_read_failed{kind="sitepeers"} 1
strait_status_read_failed{kind="sites"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "strait_status_read_failed"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(c, "strait_peer_reachable"); n != 0 {
		t.Errorf("%d strait_peer_reachable series with SitePeers unreadable", n)
	}
	if n := testutil.CollectAndCount(c, "strait_link_active"); n != 1 {
		t.Errorf("%d strait_link_active series, want 1", n)
	}
}
