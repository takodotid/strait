# Gateway benchmark

This benchmark measures what the gateway costs an application at site b that talks to a server at site a. It compares Strait's Envoy gateway with a plain HAProxy pair on the same `direct` link, and with endpoint-direct, which has no proxy. [RFC0005](../rfcs/RFC0005.md) cites its results.

## Paths

| Path | Hops from the client pod at b to the server pod at a |
|---|---|
| Strait gateway (Envoy), direct link | `bench.bench.svc:7000` (ClusterIP) -> gateway b, import listener -> node a's direct address, export NodePort -> gateway a, export listener -> server pod |
| HAProxy pair, direct link | `haproxy-b.bench.svc:7000` (ClusterIP) -> HAProxy b -> node a's direct address, NodePort 32100 -> HAProxy a -> server pod |
| Strait endpoint-direct, no proxy | `bench.bench.svc:7000` (ClusterIP) -> server pod, routed over the direct link |
| Strait gateway (Envoy), WireGuard link | As the first row, with the gateways joined by the `wireguard` link |

The `direct` link has no tunnel, so the first three rows compare the proxies, not encryption. The fourth row shows what the tunnel adds.

## Fixture

| Item | Value |
|---|---|
| Sites | kind clusters `strait-bench-a` and `strait-bench-b`, one node each, on one host. |
| Networks | Docker network `strait-net` carries each node's primary address and the `wireguard` link. Docker network `strait-direct` is the private link: node a at 172.31.0.12, node b at 172.31.0.22. |
| Strait | The chart in `deploy/helm/strait`, one gateway replica per site. Links per site: `direct`, cost 10, and `wireguard`, cost 20. |
| Application name | SiteService `bench`, plugin `declared`, active member `srv-a` at a. Member `srv-b` at b has no pod. |
| Server | One pod at a. Its Service maps port 7000 to target port 7001, so endpoint-direct must carry the target port. |
| Client | One pod at b. |
| Node routes | Each node routes the other site's pod subnet over `strait-direct`, without masquerade. Endpoint-direct needs this. It stands in for a network that makes pod addresses routable between sites. Strait does not set it up. |

Files: [`bench/`](../bench). The runner is [`bench/run.sh`](../bench/run.sh). The client and server are one Go program, [`bench/tcpbench`](../bench/tcpbench).

## Measurements

Every message is 64 bytes. The server echoes each request.

| Metric | How |
|---|---|
| Request/response latency | One open connection, 1000 requests per second for 10 s after 1 s of warmup. p50, p90 and p99 of the round trip. |
| Connection setup | 500 new connections, one after another, after 20 unmeasured. Time from dial to the first echoed response. A bare connect would end at the local proxy, so the time includes one round trip through the whole path. |
| Throughput | The server sends as fast as it can. Bytes the client receives over 10 s, after 1 s of warmup, on 1 and on 4 connections. |
| CPU per connection | 50 connections, each 100 requests per second, 5000 per second in all. Proxy container CPU from its cgroup's `cpu.stat`, over 16 s from second 4 of the load. Minus the same container's CPU over 16 s with no traffic, in the same round. Divided by 50. Envoy container only: the gateway agent is not on the data path. Endpoint-direct has no proxy and counts 0. |
| Latency under load | p50 and p99 of the round trips of those 50 connections. |

## Fair comparison

| Setting | Envoy | HAProxy |
|---|---|---|
| Image | `envoyproxy/envoy:distroless-v1.40.2`, the chart's | `haproxy:3.2.26-alpine` |
| Worker threads | 2, the chart's `--concurrency` | 2, `nbthread 2` |
| CPU request, CPU limit | 1, none. The chart's default request is 50m. | 1, none |
| Access logs | none | none |
| Idle timeout | disabled | 1 h |
| Upstream TCP keepalive | 30 s, every 5 s, 3 probes, at both sites | the same, at b only. No run is long enough for a probe. |
| Buffers | defaults, 1 MiB per connection | defaults, `tune.bufsize` 16 KiB, no splice |
| Health checks | every second, as Strait runs them | none |
| Reached at b through | a ClusterIP Service | a ClusterIP Service |
| Reached at a through | a NodePort on node a's direct address | a NodePort on node a's direct address |
| Upstream at a | the server pod's address, from EDS | the server pod's address, from a headless Service |

The client and the server also request 1 CPU with no limit. No pod is pinned to a core: the host is shared, and the benchmark cannot reserve cores on it.

## Checks

Before and after each path, the runner checks that the traffic took that path. A round whose check fails is left out of the results and listed.

| Check | Paths |
|---|---|
| The server sees the expected source: gateway a's pod, HAProxy a's pod, or the client pod. | all |
| The EndpointSlice of `bench` at b holds gateway b and the import's port, or the server pod and port 7001. | Strait paths |
| b's `SiteLink` for the path is healthy and active. | Strait paths |
| A one-second transfer crosses gateway b's WireGuard interface, or does not. | Strait paths |
| WireGuard bytes received during the path's measurements cover every streamed byte, or stay under 50 MB. | all |
| The load held at least 98% of its target rate. | all |

## Run order

| Item | Value |
|---|---|
| Rounds | 5 |
| Order in each round | gateway with direct link, HAProxy, endpoint-direct, gateway with WireGuard link |
| Between paths | The runner patches b's links: `spec.direct.endpointDirect` on `a-direct`, and the cost of `a-wireguard`, 5 or 20. Then it waits for the checks. |
| Recorded before each round | `nproc`, load average, host CPU busy, `docker stats`, idle proxy CPU |
| Recorded before each load window | host CPU busy, `docker stats` |

Each cell of the results is the median over rounds, with the lowest and highest round in brackets. Differences against endpoint-direct, and between Envoy and HAProxy, are taken within each round before the median. That cancels most of the drift between rounds.

## Environment

| Item | Value |
|---|---|
| Host | One Linux host with Docker, 8 vCPU, shared with other workloads. Both sites, the client and the server run on it. |
| Linux | 7.0 |
| Docker | 29.9.2 |
| kind | v0.33.0, node image `kindest/node:v1.37.0` |
| Kubernetes | v1.37.0, kube-proxy in `iptables` mode, kindnet |
| Envoy | v1.40.2 |
| HAProxy | 3.2.26 |
| Go | 1.27.3, for `tcpbench` |

## Results

`make bench`, five rounds, every check passed. Each cell is the median, with the lowest and highest round in brackets.

Latency, µs:

| Path | Request/response p50 | p90 | p99 | Connection setup p50 | p90 | p99 |
|---|---|---|---|---|---|---|
| Strait gateway (Envoy), direct link | 339 (299 to 341) | 404 (343 to 408) | 483 (407 to 489) | 841 (807 to 879) | 909 (857 to 1001) | 1040 (986 to 1127) |
| HAProxy pair, direct link | 270 (249 to 293) | 329 (287 to 350) | 387 (344 to 428) | 629 (609 to 663) | 689 (673 to 736) | 784 (736 to 863) |
| Strait endpoint-direct, no proxy | 112 (92 to 114) | 141 (113 to 143) | 174 (140 to 177) | 299 (274 to 317) | 351 (316 to 373) | 422 (358 to 478) |
| Strait gateway (Envoy), WireGuard link | 481 (458 to 489) | 616 (576 to 630) | 2298 (2187 to 2488) | 1141 (1100 to 1227) | 1467 (1352 to 1791) | 3401 (3173 to 3976) |

Added over endpoint-direct in the same round, µs:

| Path | Request/response p50 | Request/response p99 | Connection setup p50 |
|---|---|---|---|
| Strait gateway (Envoy), direct link | 225 (206 to 229) | 306 (268 to 317) | 546 (529 to 571) |
| HAProxy pair, direct link | 156 (138 to 183) | 210 (175 to 256) | 344 (292 to 357) |
| Strait gateway (Envoy), WireGuard link | 366 (345 to 397) | 2124 (2047 to 2316) | 835 (783 to 928) |

Throughput, CPU and latency under load. CPU is in millicores per connection, at 100 requests per second on each connection:

| Path | 1 stream, Gbit/s | 4 streams, Gbit/s | CPU at a | CPU at b | CPU, both | Under load p50, µs | Under load p99, µs |
|---|---|---|---|---|---|---|---|
| Strait gateway (Envoy), direct link | 8.71 (7.21 to 10.55) | 11.62 (11.13 to 12.50) | 10.6 (9.9 to 10.9) | 10.7 (10.3 to 11.1) | 21.3 (20.2 to 22.0) | 332 (309 to 354) | 566 (467 to 640) |
| HAProxy pair, direct link | 3.34 (3.07 to 3.52) | 6.67 (6.52 to 7.08) | 9.9 (9.3 to 10.1) | 10.1 (9.4 to 10.2) | 20.0 (18.7 to 20.3) | 282 (244 to 285) | 443 (376 to 451) |
| Strait endpoint-direct, no proxy | 15.59 (12.46 to 16.26) | 48.10 (47.46 to 50.58) | 0 | 0 | 0 | 152 (139 to 155) | 296 (266 to 300) |
| Strait gateway (Envoy), WireGuard link | 0.75 (0.72 to 0.79) | 0.75 (0.60 to 0.79) | 8.8 (8.6 to 9.4) | 8.8 (8.7 to 9.5) | 17.6 (17.3 to 18.8) | 528 (504 to 563) | 1550 (1485 to 1607) |

Envoy minus HAProxy, on the direct link, in the same round. Positive latency and CPU favour HAProxy. Positive throughput favours Envoy.

| Metric | Envoy minus HAProxy |
|---|---|
| Request/response p50, µs | 50 (36 to 90) |
| Request/response p99, µs | 63 (36 to 137) |
| Connection setup p50, µs | 212 (172 to 270) |
| Connection setup p99, µs | 273 (159 to 343) |
| Under load p50, µs | 66 (45 to 76) |
| Under load p99, µs | 137 (91 to 198) |
| 1 stream, Gbit/s | 5.38 (3.69 to 7.17) |
| 4 streams, Gbit/s | 4.80 (4.58 to 5.43) |
| CPU, both sites, millicores per connection | 1.5 (0.7 to 2.0) |

Host during the run:

| Item | Value |
|---|---|
| Load average, 1 min, before each round | 3.4 to 4.5 |
| Host CPU busy over 2 s before each load window, benchmark included | 0.4 to 0.7 cores |
| Host CPU busy during each load window, benchmark included | 1.1 to 2.6 cores |
| Idle CPU, both Envoys | 10 to 13 millicores |
| Idle CPU, both HAProxys | under 1 millicore |

## Under CPU contention

[`bench/contention.sh`](../bench/contention.sh), on the clusters of a kept run. Both nodes are held to the same 4 host CPUs. The load is the one above: 50 connections, 100 requests per second each. Envoy and HAProxy alternate A B B A, 8 runs each, so neither always goes first. Then a neighbour pod per site keeps 2 threads busy, with the same CPU request as each proxy, and the runs repeat. Every run held its rate. The load average was 1.5 to 2.1 without the neighbour, and 7 to 25 with it, other workloads included.

| Condition | Proxy | Under load p50, µs | Under load p99, µs |
|---|---|---|---|
| No neighbour | Envoy | 331 (329 to 335) | 1363 (1087 to 1880) |
| No neighbour | HAProxy | 260 (259 to 296) | 1025 (853 to 1301) |
| Neighbour | Envoy | 11482 (9300 to 12260) | 25877 (20584 to 40850) |
| Neighbour | HAProxy | 10559 (10091 to 11508) | 19593 (12955 to 36046) |

## Reading

A pooled PostgreSQL or Valkey connection pays the request/response cost once per query. A new connection pays the setup cost once.

| Question | Answer |
|---|---|
| What does the gateway add per round trip? | 225 µs with Envoy, 156 µs with HAProxy, over the two hops. Endpoint-direct avoids both. |
| What does Envoy add over HAProxy per round trip? | 50 µs at p50 and 63 µs at p99 on one connection. 66 µs at p50 and 137 µs at p99 under load. |
| What does a real link add? | Light in fibre covers about 200 km per millisecond. A 100 km fibre path adds at least 1 ms per round trip, 1000 km at least 10 ms. In this fixture the whole endpoint-direct round trip is 112 µs, so a share of it overstates the cost. |
| Is 50 µs meaningful against that? | No. It is 5% of a 1 ms round trip and 0.5% of 10 ms, before any server time. |
| What does Envoy add per new connection? | 212 µs. A new PostgreSQL connection already needs at least one more round trip across the link for its startup, and more for password authentication or TLS. A pool pays it once per connection. |
| Which moves more data? | Envoy, at defaults: 2.6 times HAProxy on one stream, 1.7 times on four. |
| Which uses less CPU? | HAProxy, by 1.5 millicores per connection, about 7%. |
| What does WireGuard add? | 142 µs at p50 and 1.8 ms at p99 over the direct link, and one stream tops out near 0.75 Gbit/s on this host. |
| What happens when CPU runs short? | Both proxies slow down 35 to 40 times at p50, from about 0.3 ms to about 11 ms. Within each A B B A block, Envoy's p99 is 1.1 to 1.5 times HAProxy's, with and without the neighbour, and its p50 is higher too. That gap is small next to the slowdown. |

Conclusion: the difference between Envoy and HAProxy is not meaningful for an application that talks to PostgreSQL or Valkey across sites, while the gateway has CPU to spare. Envoy costs about 50 µs more per round trip over two hops and about 7% more CPU per request, and moves 1.7 to 2.6 times as much data at defaults. Against a cross-site round trip of 1 ms or more, the latency difference is 5% or less. Between sites so close that the round trip is shorter, the gateway itself, 156 to 225 µs here, weighs more than the choice of proxy, and endpoint-direct removes it. When the gateway's node runs short of CPU, both proxies slow down far more than they differ: the gateway needs CPU headroom whichever proxy runs.

## Limits

| Limit | Effect |
|---|---|
| Both sites, the client and the server share one host and one kernel. The link is a Linux bridge. | Absolute numbers compare only within one run. The fixture's round trip is far shorter than a real link's. |
| The host is shared, and no core is reserved. | Scheduling delays add to every proxy hop. In `make bench`, Envoy always runs just before HAProxy, so a host whose load falls during a round favours HAProxy. The contention check alternates them. |
| Kernel work is charged to no container: iptables, conntrack, softirq, WireGuard encryption in kernel workers. | The CPU columns cover the proxy processes only. The WireGuard row understates the tunnel's CPU. |
| One client, one server, 64-byte messages, an echo with no server time. | Real replies are larger and servers take time, which shrinks the proxy's share. |
| Both proxies run at their defaults. | HAProxy offers splice and larger buffers, which this run does not use. The throughput gap is at defaults only. |
| One node and one gateway replica per site. | Scaling is not measured. |

## Repeat

```sh
make bench
```

It needs Docker, kind, kubectl, helm, Go and the `wireguard` kernel module. It creates the Docker networks `strait-net` and `strait-direct` when they are missing, and the clusters `strait-bench-a` and `strait-bench-b`. It writes `_output/bench/results.md`, `results.json`, the raw measurements in `raw/`, and the `strait` CLI's view of site b. It deletes the clusters at the end.

| Variable | Effect |
|---|---|
| `STRAIT_BENCH_KEEP=1` | Keeps the clusters. A later run reuses them. |
| `STRAIT_BENCH_SKIP_BUILD=1` | Uses the images of an earlier run. |
| `STRAIT_BENCH_ROUNDS=N` | Rounds. 5 by default. |
| `STRAIT_BENCH_PATHS="..."` | Paths, in measuring order. `pod-ip` adds the client dialing the server pod's address, with no Service. |

The contention check runs on kept clusters and writes `_output/bench/contention.md`:

```sh
STRAIT_BENCH_KEEP=1 make bench
bench/contention.sh
```

| Variable | Effect |
|---|---|
| `STRAIT_BENCH_CPUS=4-7` | Host CPUs that both nodes are held to. Given back at the end. |
| `STRAIT_BENCH_BLOCKS=4` | A B B A blocks per condition. |
