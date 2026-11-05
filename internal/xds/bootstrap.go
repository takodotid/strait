package xds

import (
	"fmt"
	"time"
)

// Fixed values of the gateway's Envoy.
const (
	// Port is the controller's ADS port.
	Port = 18000
	// NodeCluster is the node cluster of every gateway Envoy. The node ID is the pod name, set with --service-node.
	NodeCluster = "strait-gateway"
	// AdminPort is Envoy's admin port, on 127.0.0.1 only.
	AdminPort = 9901
	// WatchdogKill is how long a thread of Envoy may not answer its watchdog before Envoy kills itself.
	WatchdogKill = 60 * time.Second
)

// Bootstrap returns the Envoy bootstrap of the gateway. Everything else comes from the controller over ADS.
func Bootstrap(xdsAddress string, xdsPort int) string {
	return fmt.Sprintf(`# Envoy bootstrap of the Strait gateway. Envoy takes its node ID, the pod name, from --service-node.
node:
  cluster: %s
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: %d
dynamic_resources:
  ads_config:
    api_type: GRPC
    transport_api_version: V3
    grpc_services:
    - envoy_grpc:
        cluster_name: xds
  cds_config:
    ads: {}
    resource_api_version: V3
  lds_config:
    ads: {}
    resource_api_version: V3
static_resources:
  clusters:
  - name: xds
    type: STRICT_DNS
    connect_timeout: 2s
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options:
            connection_keepalive:
              interval: 10s
              timeout: 5s
    load_assignment:
      cluster_name: xds
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: %s
                port_value: %d
layered_runtime:
  layers:
  - name: static
    static_layer: {}
# Envoy kills itself when a thread has not answered its watchdog for 60s, and the kubelet restarts it. Its admin
# interface is on 127.0.0.1 only, so no liveness probe can reach it.
watchdogs:
  main_thread_watchdog:
    kill_timeout: %ds
  worker_watchdog:
    kill_timeout: %ds
`, NodeCluster, AdminPort, xdsAddress, xdsPort, int(WatchdogKill.Seconds()), int(WatchdogKill.Seconds()))
}
