// Package metrics exposes Strait's state as Prometheus metrics, read at scrape time: from the statuses, from the
// gateway Lease, and from the time of each SiteService's last decision.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/controller"
)

var (
	endpoints = prometheus.NewDesc("strait_siteservice_endpoints",
		"Endpoints written for the application name. 0 means the route is nowhere.", []string{"namespace", "siteservice"}, nil)
	reason = prometheus.NewDesc("strait_siteservice_route_reason",
		"1 for the reason of the current route.", []string{"namespace", "siteservice", "reason"}, nil)
	condition = prometheus.NewDesc("strait_siteservice_condition",
		"1 while the condition is True.", []string{"namespace", "siteservice", "condition"}, nil)
	reachable = prometheus.NewDesc("strait_peer_reachable",
		"1 while the peer's observation document is fresh.", []string{"peer"}, nil)
	healthy = prometheus.NewDesc("strait_link_healthy",
		"1 while the link is healthy.", []string{"link", "peer", "transport"}, nil)
	active = prometheus.NewDesc("strait_link_active",
		"1 while the link carries the pair's traffic.", []string{"link", "peer", "transport"}, nil)
	siteCondition = prometheus.NewDesc("strait_site_condition",
		"1 while the condition of the local Site is True.", []string{"condition"}, nil)
	lastSeen = prometheus.NewDesc("strait_peer_last_seen_timestamp_seconds",
		"When the peer's document was last read, in Unix seconds.", []string{"peer"}, nil)
	lastDecision = prometheus.NewDesc("strait_siteservice_last_decision_timestamp_seconds",
		"When the elected controller last decided the route of the application name, in Unix seconds.", []string{"namespace", "siteservice"}, nil)
	leaseTransitions = prometheus.NewDesc("strait_gateway_lease_transitions_total",
		"Times a gateway replica took the gateway Lease from another holder, or after a release, from the Lease.", nil, nil)
	readFailed = prometheus.NewDesc("strait_status_read_failed",
		"1 when this scrape could not read the objects of the kind, whose metrics are then missing.", []string{"kind"}, nil)
)

// Collector reads SiteServices, SitePeers, SiteLinks, the Site and the gateway Lease through a reader at each scrape.
type Collector struct {
	Reader client.Reader
	// GatewayNamespace holds the gateway Lease. Empty reads no Lease.
	GatewayNamespace string
	// Decisions returns when each SiteService's last decision was made. Nil reports none.
	Decisions func() map[types.NamespacedName]time.Time
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{endpoints, reason, condition, reachable, healthy, active, siteCondition, lastSeen, lastDecision,
		leaseTransitions, readFailed} {
		ch <- d
	}
}

// read lists the objects of a kind, and reports whether the list failed in strait_status_read_failed.
func (c *Collector) read(ctx context.Context, ch chan<- prometheus.Metric, kind string, list client.ObjectList) bool {
	err := c.Reader.List(ctx, list)
	ch <- prometheus.MustNewConstMetric(readFailed, prometheus.GaugeValue, value(err != nil), kind)
	return err == nil
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var decisions map[types.NamespacedName]time.Time
	if c.Decisions != nil {
		decisions = c.Decisions()
	}
	var services straitv1.SiteServiceList
	if c.read(ctx, ch, "siteservices", &services) {
		for _, s := range services.Items {
			if at, ok := decisions[client.ObjectKeyFromObject(&s)]; ok {
				ch <- prometheus.MustNewConstMetric(lastDecision, prometheus.GaugeValue, unix(at), s.Namespace, s.Name)
			}
			ch <- prometheus.MustNewConstMetric(endpoints, prometheus.GaugeValue, float64(s.Status.Route.Endpoints), s.Namespace, s.Name)
			if s.Status.Route.Reason != "" {
				ch <- prometheus.MustNewConstMetric(reason, prometheus.GaugeValue, 1, s.Namespace, s.Name, s.Status.Route.Reason)
			}
			for _, cond := range s.Status.Conditions {
				ch <- prometheus.MustNewConstMetric(condition, prometheus.GaugeValue, value(cond.Status == metav1.ConditionTrue), s.Namespace, s.Name, cond.Type)
			}
		}
	}
	var peers straitv1.SitePeerList
	if c.read(ctx, ch, "sitepeers", &peers) {
		for _, p := range peers.Items {
			ch <- prometheus.MustNewConstMetric(reachable, prometheus.GaugeValue, value(p.Status.Reachable), p.Name)
			if p.Status.LastSeenTime != nil {
				ch <- prometheus.MustNewConstMetric(lastSeen, prometheus.GaugeValue, unix(p.Status.LastSeenTime.Time), p.Name)
			}
		}
	}
	var links straitv1.SiteLinkList
	if c.read(ctx, ch, "sitelinks", &links) {
		for _, l := range links.Items {
			t := string(l.Spec.Transport)
			ch <- prometheus.MustNewConstMetric(healthy, prometheus.GaugeValue, value(l.Status.Healthy), l.Name, l.Spec.Peer, t)
			ch <- prometheus.MustNewConstMetric(active, prometheus.GaugeValue, value(l.Status.Active), l.Name, l.Spec.Peer, t)
		}
	}
	var sites straitv1.SiteList
	if c.read(ctx, ch, "sites", &sites) && len(sites.Items) == 1 {
		for _, cond := range sites.Items[0].Status.Conditions {
			ch <- prometheus.MustNewConstMetric(siteCondition, prometheus.GaugeValue, value(cond.Status == metav1.ConditionTrue), cond.Type)
		}
	}
	if c.GatewayNamespace != "" {
		var leases coordinationv1.LeaseList
		if c.read(ctx, ch, "leases", &leases) {
			for _, l := range leases.Items {
				if l.Namespace == c.GatewayNamespace && l.Name == controller.GatewayLease {
					ch <- prometheus.MustNewConstMetric(leaseTransitions, prometheus.CounterValue, float64(ptr.Deref(l.Spec.LeaseTransitions, 0)))
				}
			}
		}
	}
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func value(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
