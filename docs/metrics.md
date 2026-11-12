# Metrics and alerts

Every metric that Strait exports, how to scrape it, and the alert rules that the chart ships. Each metric is read when it is scraped. No label holds an address or a port.

## Scrape

| Pod | Port | Path | Serves |
|---|---|---|---|
| Controller | `metrics`, 8080 | `/metrics` | The controller's metrics. Only the elected controller decides, polls and serves xDS. |
| Each gateway pod | `metrics`, 8091 | `/metrics` | The gateway agent's metrics. |
| Each gateway pod | `metrics`, 8091 | `/stats/prometheus` | Envoy's stats that the agent lets through ([Envoy](#envoy)). |

Scrape every gateway pod, the standbys too: their Envoy checks the links as the active one does.

With the Prometheus Operator, the chart renders the scrape configuration and the rules. Both are off by default, since their CRDs may be absent:

| Value | Renders |
|---|---|
| `metrics.podMonitor.enabled` | The PodMonitors `strait-controller` and `strait-gateway`. `metrics.podMonitor.labels` holds the labels that the Prometheus's `podMonitorSelector` requires. |
| `metrics.prometheusRule.enabled` | The PrometheusRule `strait`, with the rules of [Alerts](#alerts). `metrics.prometheusRule.labels` holds the labels that the Prometheus's `ruleSelector` requires. |

Without the operator, a Prometheus scrape configuration:

```yaml
scrape_configs:
  - job_name: strait
    kubernetes_sd_configs:
      - role: pod
        namespaces: {names: [strait-system]}
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_name, __meta_kubernetes_pod_container_port_name]
        regex: strait;metrics
        action: keep
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
  - job_name: strait-envoy
    metrics_path: /stats/prometheus
    kubernetes_sd_configs:
      - role: pod
        namespaces: {names: [strait-system]}
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component, __meta_kubernetes_pod_container_port_name]
        regex: gateway;metrics
        action: keep
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
```

The metrics are plain HTTP. `networkPolicy.metricsFrom` limits the sources that reach the controller's port 8080 and, with `networkPolicy.gateway.enabled`, the gateway's port 8091 ([RFC0007](../rfcs/RFC0007.md#network-policies)).

## Controller

| Metric | Type | Labels | Value |
|---|---|---|---|
| `strait_siteservice_endpoints` | gauge | `namespace`, `siteservice` | Endpoints written for the application name. 0 is a route to nowhere. |
| `strait_siteservice_route_reason` | gauge | `namespace`, `siteservice`, `reason` | 1 for the reason of the current route. |
| `strait_siteservice_condition` | gauge | `namespace`, `siteservice`, `condition` | 1 while the condition is True. |
| `strait_siteservice_last_decision_timestamp_seconds` | gauge | `namespace`, `siteservice` | When the elected controller last decided the route, in Unix seconds. It decides each `SiteService` at least every 2s. |
| `strait_peer_reachable` | gauge | `peer` | 1 while the peer's document is fresh ([RFC0004](../rfcs/RFC0004.md)). |
| `strait_peer_last_seen_timestamp_seconds` | gauge | `peer` | When the peer's document was last read, in Unix seconds, from `SitePeer.status.lastSeenTime`. |
| `strait_link_healthy` | gauge | `link`, `peer`, `transport` | 1 while the link is healthy, from `SiteLink.status`. |
| `strait_link_active` | gauge | `link`, `peer`, `transport` | 1 while the link carries the pair's traffic, from `SiteLink.status`. |
| `strait_site_condition` | gauge | `condition` | 1 while the condition of the local `Site` is True: `GatewayActive`, `GatewayServices`, `GatewayConfig`. |
| `strait_gateway_lease_transitions_total` | counter | | Times a gateway replica took the gateway Lease from another holder, or after a release: the Lease's `leaseTransitions`. Each move resets every connection across the site's gateway. |
| `strait_xds_rejected_total` | counter | `type` | Configurations that a gateway Envoy rejected, by resource type, such as `Cluster` ([RFC0005](../rfcs/RFC0005.md#configuration-rejections)). |
| `strait_status_read_failed` | gauge | `kind` | 1 when the scrape could not read the objects of the kind. Their metrics are then missing, not 0. |
| `leader_election_master_status` | gauge | `name` | 1 while this controller holds the Lease `name="strait-controller"`. From controller-runtime. |

controller-runtime also exports its own metrics, such as `controller_runtime_reconcile_total`, `workqueue_depth` and `rest_client_requests_total`, and those of Go and the process.

## Gateway agent

| Metric | Type | Labels | Value |
|---|---|---|---|
| `strait_gateway_leading` | gauge | | 1 while this replica holds the gateway Lease and carries the site's traffic. |
| `strait_gateway_link_connections` | gauge | `link`, `peer` | Open connections of the imports to the peer over the link, from this replica's Envoy. |
| `strait_gateway_wireguard_receive_bytes_total` | counter | `link`, `peer` | Bytes received on the link's WireGuard interface since it was created. Active replica only. |
| `strait_gateway_wireguard_transmit_bytes_total` | counter | `link`, `peer` | Bytes sent on the link's WireGuard interface since it was created. Active replica only. |

The agent also exports the metrics of Go and the process.

## Envoy

At `/stats/prometheus`, the agent serves these of Envoy's stats, and no other path of Envoy's admin interface. Envoy names them `envoy_` and the stat, with each `.` as `_`.

| Stats | Labels | Holds |
|---|---|---|
| `cluster.*.upstream_cx_{total,active,connect_fail,connect_timeout,none_healthy,rx_bytes_total,tx_bytes_total}`, `membership_{healthy,total}`, `health_check.{success,failure,network_failure}`, `update_rejected` | `envoy_cluster_name` | Per cluster: each export and import, each peer, the controller. Connections, bytes, connect failures, healthy endpoints. |
| `tcp.*.downstream_cx_{total,rx_bytes_total,tx_bytes_total,no_route}` | `envoy_tcp_prefix` | Per export and import: connections and bytes from clients. |
| `http.{control,peers}.downstream_rq_{total,1xx,..,5xx}` | `envoy_http_conn_manager_prefix`, `envoy_response_code_class` | Requests to the control port and to the peer listener. |
| `server.{live,state,uptime,memory_allocated,memory_heap_size,total_connections}` | | Envoy's state. |
| `control_plane.connected_state` | | 1 while Envoy holds its xDS stream to the controller. |
| `cluster_manager.{active_clusters,cds.update_success,cds.update_rejected}`, `listener_manager.{total_listeners_active,lds.update_success,lds.update_rejected}` | | Configuration updates. |

| Name in the labels | Means |
|---|---|
| `export/<namespace>/<name>`, `import/<namespace>/<name>` | An `Export` or an `Import`. A `.` in the name shows as `_`: Envoy's labels end a name at its first `.`. |
| `peer/<peer>` | The control port of a peer, over each link. |
| `controller` | The controller's observation port. |

Per-link bytes exist for `wireguard` links only, from the interface. A `direct` link has no interface, and Envoy's stats per endpoint are labelled by address ([RFC0005](../rfcs/RFC0005.md#limitations)).

## Alerts

The rules of `deploy/helm/strait/files/alerts.yaml`. They suit one Prometheus per site: `absent()` and `max()` span every series of the Prometheus.

| Alert | Fires when | Next step |
|---|---|---|
| `StraitNoController` | No controller has led for 2m. Routes are frozen, and the peers count this site's members `Unknown`. | `kubectl -n strait-system get pods,events`. [Troubleshooting](troubleshooting.md). |
| `StraitPeerUnreachable` | A peer's document has been stale for 2m. | `strait peers`. [Troubleshooting](troubleshooting.md). |
| `StraitNoActiveLink` | No link to a peer has been healthy for 2m. | `strait links`. |
| `StraitRouteNowhere` | An application name has routed nowhere for 2m. | `strait explain`. |
| `StraitNotApplied` | The controller has not applied a route for 2m. | The `Applied` condition's message. |
| `StraitNoActiveGateway` | No ready gateway pod has held the gateway Lease for 1m. | [Troubleshooting](troubleshooting.md), the `Site`'s conditions. |
| `StraitGatewayConfigRejected` | A gateway Envoy has rejected its configuration for 5m. | The `GatewayConfig` condition. |
| `StraitDecisionsStalled` | The controller has decided no route of an application name for over a minute. | The controller's log, and the API server's latency. |
| `StraitStatusUnreadable` | The controller's metrics have not read a kind for 5m. | The controller's log. |
| `StraitGatewayHandovers` | The gateway Lease moved more than 3 times in an hour. | The `GatewayHandover` events of the `Site`, and the gateway pods' restarts. |
| `StraitGatewayNotConfigured` | A gateway pod has had no xDS stream to the controller for 5m. | The controller's Service, and the network policy on it. |

The `for` of each rule outlasts what Strait rides out on its own: a link failover, a gateway handover, a hold of an `Unknown` member.

## Probes

Liveness checks only work that waits for neither the API server nor a link, so a slow API server or a link that is down restarts nothing. Work that waits for them shows in the metrics above instead.

| Container | Liveness fails when |
|---|---|
| Controller | The elected controller's poll round, or its xDS rebuild loop, has not begun for twice its period plus 30s: 34s with the default `pollInterval` of 2s. |
| Gateway agent | The active replica's WireGuard session loop has not turned for 30s. It reads the interfaces only. |
| Envoy | No probe: its admin interface is on 127.0.0.1 only. Envoy kills itself when a thread has not answered its watchdog for 60s, and the kubelet restarts it. |
