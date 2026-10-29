# End-to-end tests

Two kind clusters, one per site, with CloudNativePG in the distributed topology and Strait between them.

## Run

```sh
make e2e
```

`make e2e` builds the image (`make images`), then runs `go test -tags e2e ./test/e2e/...`. One run takes about 7 minutes.

| Variable | Default | Effect |
|---|---|---|
| `STRAIT_IMAGE` | `ghcr.io/takodotid/strait:dev` | The Strait image under test. It must be in the local docker image store. |
| `STRAIT_E2E_KEEP` | unset | `1` keeps the clusters and the kubeconfigs after the run. |

The test prints each site's kubeconfig path and context at start. It never reads or writes `~/.kube/config`.

## Prerequisites

| Tool | Version |
|---|---|
| Host | Linux with docker. 8 vCPU and 16 GiB are enough. |
| docker | recent |
| kind | v0.33.0. Its default node image is the pinned one. |
| kubectl, helm | recent |
| Go | as in `go.mod` |
| `wireguard` kernel module | loaded on the host (`sudo modprobe wireguard`) |

The host needs network access to pull the images and the CloudNativePG manifest. CloudNativePG's operator Deployment pulls with `imagePullPolicy: Always`, so its node checks the registry even though the image is loaded.

## Fixture

| Item | Site a | Site b |
|---|---|---|
| kind cluster, context | `strait-e2e-a`, `kind-strait-e2e-a` | `strait-e2e-b`, `kind-strait-e2e-b` |
| Pod, Service CIDR | `10.110.0.0/16`, `10.111.0.0/16` | `10.120.0.0/16`, `10.121.0.0/16` |
| Address on `strait-direct` | `172.31.0.11` | `172.31.0.21` |
| CloudNativePG Cluster | `cluster-a`, primary at start | `cluster-b`, replica cluster at start |
| SiteLinks | `b-direct` (cost 10), `b-wan` (wireguard, cost 100, waits) | `a-direct` (cost 10), `a-wan` (wireguard, cost 100, dials) |

| Docker network | Subnet | Carries |
|---|---|---|
| `strait-net` | `172.30.0.0/24` | Each node's primary network, and the wireguard links. |
| `strait-direct` | `172.31.0.0/24`, internal | The direct links. |

The test creates both networks when they are absent and never deletes them: other `strait-*` clusters share them.

Faults are nftables tables inside the node containers: `strait-cut-direct`, `strait-cut-wan`, and `strait-cut-site` with `docker pause` for a lost site.

## Scenarios

They run in this order on one fixture and stop at the first failure.

| # | Scenario | Checks |
|---|---|---|
| 1 | Baseline | Both sites route `pg` to `pg-a`. A write from each site lands on `cluster-a`. |
| 2 | Link failover | Cutting the direct link makes `a-wan` active at site b within 10s. Writes from b keep succeeding. |
| 3 | Switchover with tokens | Site a stops naming `pg-a` within one 500ms sample of the demotion patch, site b within `peerTimeout`. `pg-b` never shows before the promotion patch. Both sites pass through nowhere, then route `pg-b`. |
| 4 | Partition | Site b keeps `pg-b` with reason `Active`. Site a holds `pg-b` (`HeldUnknown`), then routes nowhere once `unknownHold` has passed. A connection at a is refused in under 2s. After the heal, a routes `pg-b` within 15s. |
| 5 | Disagreeing declarations | Two `declared` SiteServices that name different members route nowhere at both sites. Once they agree, a request from each site reaches the declared member. |
| 6 | Site loss | Site a holds `pg-b`, then routes nowhere. After the operator promotes `cluster-a` without a token, a routes `pg-a`. |
| 7 | Two writable members | Site b comes back with `cluster-b` still primary, still cut off from site a, and its controller restarts. With `startupHold: 30s` on `pg`, site b routes nowhere with `AwaitingPeers` and never to `pg-b` until the hold ends, then to `pg-b`. Once the cut is removed, within 30s both sites route nowhere and the `Multiple` condition is `True`. |
| rbac | Least privilege | No Strait controller or gateway agent logged a `forbidden` request. |

## Output

| Path | Holds |
|---|---|
| `_output/e2e/timings.txt` | Every measured duration, one line each, appended by every run. |
| `_output/e2e/<site>/` | On failure: Strait resources, CloudNativePG Clusters, pods, events, and Strait pod logs. |
