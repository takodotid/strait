// Package metrics exposes Strait's state as Prometheus gauges. Every value is read from a status at scrape time.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
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
)

// Collector reads SiteServices, SitePeers and SiteLinks through a reader at each scrape.
type Collector struct {
	Reader client.Reader
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{endpoints, reason, condition, reachable, healthy, active} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var services straitv1.SiteServiceList
	if err := c.Reader.List(ctx, &services); err == nil {
		for _, s := range services.Items {
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
	if err := c.Reader.List(ctx, &peers); err == nil {
		for _, p := range peers.Items {
			ch <- prometheus.MustNewConstMetric(reachable, prometheus.GaugeValue, value(p.Status.Reachable), p.Name)
		}
	}
	var links straitv1.SiteLinkList
	if err := c.Reader.List(ctx, &links); err == nil {
		for _, l := range links.Items {
			t := string(l.Spec.Transport)
			ch <- prometheus.MustNewConstMetric(healthy, prometheus.GaugeValue, value(l.Status.Healthy), l.Name, l.Spec.Peer, t)
			ch <- prometheus.MustNewConstMetric(active, prometheus.GaugeValue, value(l.Status.Active), l.Name, l.Spec.Peer, t)
		}
	}
}

func value(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
