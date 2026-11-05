package gateway

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/links"
)

// EnvoyStats is the regular expression of the Envoy stats that the agent serves at /stats/prometheus: per cluster
// and per TCP proxy, connections, bytes, health and rejected updates; per HTTP listener of the gateway, requests;
// Envoy's state and its connection to the controller. Listener stats are left out: Envoy tags them with an address
// and a port. The names of clusters and TCP proxies hold no dot (xds.StatName), so [^.]+ is the whole name.
const EnvoyStats = `^(` +
	`cluster\.[^.]+\.(upstream_cx_(total|active|connect_fail|connect_timeout|none_healthy|rx_bytes_total|tx_bytes_total)|membership_(healthy|total)|health_check\.(success|failure|network_failure)|update_rejected)` +
	`|tcp\.[^.]+\.downstream_cx_(total|rx_bytes_total|tx_bytes_total|no_route)` +
	`|http\.(control|peers)\.downstream_rq_(total|[1-5]xx)` +
	`|server\.(live|state|uptime|memory_allocated|memory_heap_size|total_connections)` +
	`|control_plane\.connected_state` +
	`|cluster_manager\.(active_clusters|cds\.update_(success|rejected))` +
	`|listener_manager\.(total_listeners_active|lds\.update_(success|rejected))` +
	`)$`

// ImportCluster starts the name of the Envoy cluster of an import ([RFC#0005]).
const ImportCluster = "import/"

// maxEnvoyStats is the largest answer of Envoy's /stats/prometheus that the agent passes on.
const maxEnvoyStats = 64 << 20

var (
	leadingDesc = prometheus.NewDesc("strait_gateway_leading",
		"1 while this gateway replica holds the gateway Lease and carries the site's traffic.", nil, nil)
	linkConnectionsDesc = prometheus.NewDesc("strait_gateway_link_connections",
		"Open connections of the imports to the peer over the link, from this replica's Envoy.", []string{"link", "peer"}, nil)
	wireGuardReceiveDesc = prometheus.NewDesc("strait_gateway_wireguard_receive_bytes_total",
		"Bytes received from the peer on the link's WireGuard interface, since the interface was created.", []string{"link", "peer"}, nil)
	wireGuardTransmitDesc = prometheus.NewDesc("strait_gateway_wireguard_transmit_bytes_total",
		"Bytes sent to the peer on the link's WireGuard interface, since the interface was created.", []string{"link", "peer"}, nil)
)

// Metrics serves the agent's metrics at /metrics, and the Envoy stats that EnvoyStats names at /stats/prometheus.
// It serves no other path, so the scraper reaches nothing else of Envoy's admin interface.
func Metrics(a *Agent) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), NewCollector(a))
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /stats/prometheus", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.EnvoyAdmin+"/stats/prometheus?filter="+url.QueryEscape(EnvoyStats), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := a.HTTP.Do(req)
		if err != nil {
			http.Error(w, "read Envoy's stats: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, "read Envoy's stats: "+resp.Status, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		_, _ = io.Copy(w, io.LimitReader(resp.Body, maxEnvoyStats))
	})
	return mux
}

// collector reads the agent's state, its links, Envoy's clusters and the WireGuard interfaces at each scrape.
type collector struct{ a *Agent }

// NewCollector returns the collector of the agent's own metrics.
func NewCollector(a *Agent) prometheus.Collector { return &collector{a: a} }

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{leadingDesc, linkConnectionsDesc, wireGuardReceiveDesc, wireGuardTransmitDesc} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	a := c.a
	ch <- prometheus.MustNewConstMetric(leadingDesc, prometheus.GaugeValue, b2f(a.Leading()))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ls straitv1.SiteLinkList
	if err := a.Client.List(ctx, &ls); err != nil {
		return
	}
	if clusters, err := a.clusters(ctx); err == nil {
		for _, l := range ls.Items {
			ch <- prometheus.MustNewConstMetric(linkConnectionsDesc, prometheus.GaugeValue, float64(importConnections(clusters, links.Address(l))), l.Name, l.Spec.Peer)
		}
	}
	held, _ := a.links()
	if len(held) == 0 {
		return
	}
	a.kernel.Lock()
	devs, err := a.Kernel.Devices()
	a.kernel.Unlock()
	if err != nil {
		return
	}
	byName := map[string]Device{}
	for _, d := range devs {
		byName[d.Name] = d
	}
	for _, l := range ls.Items {
		d, ok := byName[held[l.Name]]
		if !ok || held[l.Name] == "" {
			continue
		}
		var rx, tx int64
		for _, p := range d.Peers {
			rx += p.ReceiveBytes
			tx += p.TransmitBytes
		}
		ch <- prometheus.MustNewConstMetric(wireGuardReceiveDesc, prometheus.CounterValue, float64(rx), l.Name, l.Spec.Peer)
		ch <- prometheus.MustNewConstMetric(wireGuardTransmitDesc, prometheus.CounterValue, float64(tx), l.Name, l.Spec.Peer)
	}
}

// importConnections is the number of open connections of every import cluster to the host at a link's address.
func importConnections(clusters map[string][]Host, addr string) int64 {
	var n int64
	for name, hosts := range clusters {
		if !strings.HasPrefix(name, ImportCluster) {
			continue
		}
		if h, ok := hostAt(hosts, addr); ok {
			n += h.Connections
		}
	}
	return n
}

// Leading reports whether this replica holds the gateway Lease and runs the leader's loops.
func (a *Agent) Leading() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancel != nil
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
