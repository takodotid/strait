package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// The metrics server answers /metrics from the agent and /stats/prometheus from Envoy, with EnvoyStats as the filter.
// It sends Envoy's admin no other request than those and the agent's own read of /clusters, whatever the scraper asks.
func TestMetricsPaths(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.URL.Query().Get("filter"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, "envoy_server_live 1\n")
	}))
	t.Cleanup(admin.Close)
	a, _, _ := newAgent(t, testSite())
	a.EnvoyAdmin = admin.URL
	srv := httptest.NewServer(Metrics(a))
	t.Cleanup(srv.Close)

	get := func(method, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("GET", "/stats/prometheus?filter=.*&usedonly"); code != 200 || body != "envoy_server_live 1\n" {
		t.Errorf("GET /stats/prometheus = %d %q", code, body)
	}
	if code, body := get("GET", "/metrics"); code != 200 || !strings.Contains(body, "strait_gateway_leading 0\n") {
		t.Errorf("GET /metrics = %d %q", code, body)
	}
	for _, r := range []string{"GET /clusters", "GET /stats", "GET /quitquitquit", "POST /quitquitquit", "POST /stats/prometheus", "GET /metrics/../clusters"} {
		method, path, _ := strings.Cut(r, " ")
		if code, _ := get(method, path); code < 400 {
			t.Errorf("%s = %d", r, code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// The agent's own metrics read /clusters.
	if want := []string{"GET /stats/prometheus " + EnvoyStats, "GET /clusters "}; !slices.Equal(asked, want) {
		t.Errorf("Envoy's admin was asked %q, want %q", asked, want)
	}
}

// The agent's metrics: whether it leads, the open connections of the imports over each link, and each WireGuard
// interface's bytes.
func TestAgentMetrics(t *testing.T) {
	clusters := `{"cluster_statuses": [
		{"name": "import/app/pg", "host_statuses": [
			{"address": {"socket_address": {"address": "192.0.2.2", "port_value": 30901}}, "stats": [{"name": "cx_active", "value": "3", "type": "GAUGE"}]},
			{"address": {"socket_address": {"address": "100.64.0.2", "port_value": 30901}}, "priority": 1, "stats": [{"name": "cx_active", "value": "1", "type": "GAUGE"}]}]},
		{"name": "import/app/redis", "host_statuses": [
			{"address": {"socket_address": {"address": "192.0.2.2", "port_value": 30902}}, "stats": [{"name": "cx_active", "value": "2", "type": "GAUGE"}]}]},
		{"name": "peer/b", "host_statuses": [
			{"address": {"socket_address": {"address": "192.0.2.2", "port_value": 30900}}, "stats": [{"name": "cx_active", "value": "7", "type": "GAUGE"}]}]}]}`
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/clusters" {
			_, _ = io.WriteString(w, clusters)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(admin.Close)
	wg := wgLink("b-wan", "b", 100, 51820, "", "100.64.0.1/32", "100.64.0.2")
	direct := directLink("b-direct", "b", 10, "192.0.2.2")
	a, k, _ := newAgent(t, testSite(), &wg, &direct)
	a.EnvoyAdmin = admin.URL
	if err := k.Create("st1"); err != nil {
		t.Fatal(err)
	}
	k.devs["st1"].Peers = []wgtypes.Peer{{ReceiveBytes: 1000, TransmitBytes: 2000}}
	a.setLinks(map[string]string{"b-wan": "st1"}, nil)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()

	want := `
# HELP strait_gateway_leading 1 while this gateway replica holds the gateway Lease and carries the site's traffic.
# TYPE strait_gateway_leading gauge
strait_gateway_leading 1
# HELP strait_gateway_link_connections Open connections of the imports to the peer over the link, from this replica's Envoy.
# TYPE strait_gateway_link_connections gauge
strait_gateway_link_connections{link="b-direct",peer="b"} 5
strait_gateway_link_connections{link="b-wan",peer="b"} 1
# HELP strait_gateway_wireguard_receive_bytes_total Bytes received from the peer on the link's WireGuard interface, since the interface was created.
# TYPE strait_gateway_wireguard_receive_bytes_total counter
strait_gateway_wireguard_receive_bytes_total{link="b-wan",peer="b"} 1000
# HELP strait_gateway_wireguard_transmit_bytes_total Bytes sent to the peer on the link's WireGuard interface, since the interface was created.
# TYPE strait_gateway_wireguard_transmit_bytes_total counter
strait_gateway_wireguard_transmit_bytes_total{link="b-wan",peer="b"} 2000
`
	if err := testutil.CollectAndCompare(&collector{a: a}, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}
