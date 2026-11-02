package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	straitv1 "github.com/takodotid/strait/api/v1"
)

func TestCollect(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = straitv1.AddToScheme(scheme)
	ss := &straitv1.SiteService{ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg"}}
	ss.Status.Route = straitv1.Route{Reason: straitv1.ReasonMultipleWritable}
	ss.Status.Conditions = []metav1.Condition{{Type: straitv1.ConditionMultiple, Status: metav1.ConditionTrue}}
	peer := &straitv1.SitePeer{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Status: straitv1.SitePeerStatus{Reachable: true}}
	link := &straitv1.SiteLink{ObjectMeta: metav1.ObjectMeta{Name: "b-direct"}, Spec: straitv1.SiteLinkSpec{Peer: "b", Transport: straitv1.TransportDirect}, Status: straitv1.SiteLinkStatus{Healthy: true}}
	r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ss, peer, link).Build()
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
`
	if err := testutil.CollectAndCompare(&Collector{Reader: r}, strings.NewReader(want),
		"strait_siteservice_condition", "strait_siteservice_endpoints", "strait_peer_reachable", "strait_link_active"); err != nil {
		t.Fatal(err)
	}
}
