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
| `nf_tables` | in the host's kernel: the gateway agent filters its WireGuard interfaces' traffic with it ([RFC0005](../../rfcs/RFC0005.md#wireguard)) |

The host needs network access to pull the images and the CloudNativePG manifest. CloudNativePG's operator Deployment pulls with `imagePullPolicy: Always`, so its node checks the registry even though the image is loaded.

## Fixture

| Item | Site a | Site b |
|---|---|---|
| kind cluster, context | `strait-e2e-a`, `kind-strait-e2e-a` | `strait-e2e-b`, `kind-strait-e2e-b` |
| Pod, Service CIDR | `10.110.0.0/16`, `10.111.0.0/16` | `10.120.0.0/16`, `10.121.0.0/16` |
| Address on `strait-direct` | `172.31.0.11` | `172.31.0.21` |
| CloudNativePG Cluster | `cluster-a`, primary at start | `cluster-b`, replica cluster at start |
| SiteLinks | `b-direct` (cost 10), `b-wan` (wireguard, cost 100, waits) | `a-direct` (cost 10), `a-wan` (wireguard, cost 100, dials) |
| `directService` | `NodePort`, `externalTrafficPolicy: Local` | same |
| `controller.namespaces` | `db`, `web`: the controller writes Services and EndpointSlices there and in `strait-system` only | same |
| `cnpg.confirmRole.secretName` | `strait-cnpg-role`: the user name and a random password of `strait_role_check` | same |
| `envoy.drainTimeSeconds` | `2`: a closed `Import` ends its sessions after 2s | same |
| `networkPolicy.gateway` | On. `peerCIDRs`: site b's node on both networks. `wireGuardPorts`: `32100`. `metricsFrom`: namespace `monitoring`, which holds no pod. kindnet enforces it. | The same, with site a's node |

| Docker network | Subnet | Carries |
|---|---|---|
| `strait-net` | `172.30.0.0/24` | Each node's primary network, and the wireguard links. |
| `strait-direct` | `172.31.0.0/24`, internal | The direct links. |

The test creates both networks when they are absent and never deletes them: other `strait-*` clusters share them.

The test makes faults with nftables tables inside the node containers: `strait-cut-direct`, `strait-cut-wan`, and `strait-cut-site` with `docker pause` for a lost site.

## Scenarios

They run in this order on one fixture and stop at the first failure.

| # | Scenario | Checks |
|---|---|---|
| 1 | Baseline | Both sites route `pg` to `pg-a`. A write from each site lands on `cluster-a`. |
| role-check | Role confirmed with the database | `confirmRole` on `pg` at site a, with the role `strait_role_check` (`LOGIN` only) created on `cluster-a`. `pg-a` stays routed. With the role's password changed, `pg-a` is `Unknown` with `RoleNotConfirmed`, and site a routes nowhere once `unknownHold` has passed. With the password back, both sites route `pg-a`. `confirmRole` is off afterwards. |
| import | Import Service | An `Export` of `web-a` at site a and an `Import` of it with `port` at site b. A request from site b to `web-a.web.svc` reaches `web-a`. Under the gateway's policy, a pod of site b reaches the active gateway pod's import port on the pod's address, and times out on 8091. Without `port`, site b deletes the Service. |
| switch | A route change through nowhere | At site a, the `declared` SiteService `local` has two members on `web-a`, and its route moves from `one` to `two` in one decision. A watch of its EndpointSlices never sees both members at once. |
| conflict | A Service-name conflict | Strait's Service `local` at site a loses its owner. Strait deletes `local`'s EndpointSlices, and `Routed` is False with `ServiceConflict`. Once that Service is deleted, Strait writes its own and the slice of `two` again. Then `local` is deleted. |
| ports | Ports of objects created together | 8 `Export`s at site a and 8 `Import`s at site b, created in parallel, each get a gateway port. No two hold one port at any read, every 100ms. The race needs a reconcile before the cache shows the last port, so a build without the fix can pass. |
| envoy-not-ready | Envoy of the active gateway not ready | The Envoy container of site a's active gateway replica is paused. The standby takes the gateway Lease, and a write from site b lands on `cluster-a` again. Then Envoy resumes. |
| gateway-handover | A handover at the site whose wireguard link waits | With the direct link cut, site a's active gateway pod is deleted. A write from site b lands on `cluster-a` within 8s of the standby taking the gateway Lease. |
| gateway-rollout | A rollout of the gateway | Site a's gateway Deployment restarts, as an upgrade rolls it. Only the active pod has a deletion cost, and the rollout moves the gateway Lease once. |
| 2 | Link failover | Cutting the direct link makes `a-wan` active at site b within 10s. Writes from b keep succeeding. A new connection from b every 250ms: each one that starts 5s or more after the cut reaches `cluster-a` (RFC0005 requirement 4). |
| 3 | Switchover with tokens | Site a stops naming `pg-a` within one 500ms sample of the demotion patch, site b within `peerTimeout`. `pg-b` never shows before the promotion patch. Both sites pass through nowhere, then route `pg-b`. |
| 4 | Partition | Site b keeps `pg-b` with reason `Active`. Site a holds `pg-b` (`HeldUnknown`), then routes nowhere once `unknownHold` has passed. A connection at a is refused in under 2s. After the heal, a routes `pg-b` within 15s. |
| 4b | Isolated site | The partition again, with `isolationTimeout: 8s` on `pg` at both sites. Site b routes `pg-b`, then nowhere with `SiteIsolated` no sooner than `isolationTimeout` after the cut, minus one poll, and a connection at b is refused. Site a routes nowhere as in step 4. Setting the field to 0s at b routes `pg-b` again, and setting it back isolates b again. After the heal, both sites route `pg-b`. The field is removed afterwards. Step 4 is the same partition with the policy off. |
| 5 | Disagreeing declarations | Two `declared` SiteServices that name different members route nowhere at both sites. Once they agree, a request from each site reaches the declared member. |
| 5b | Ending gateway sessions | At site b, `web` routes to `web-a` through the gateway, and the client pod holds an idle TCP session to it. With `policy.connections` unset, moving the route to `web-b` keeps the session for 20s. With `EndGateway` at site b, the move ends it within 20s, and a new request from b reaches `web-b`. `web-a` stays up throughout. The policy is removed afterwards. |
| 6 | Site loss | Site a holds `pg-b`, then routes nowhere. After the operator promotes `cluster-a` without a token, a routes `pg-a`. |
| 7 | Two writable members | Site b comes back with `cluster-b` still primary, still cut off from site a, and its controller restarts. With `startupHold: 30s` on `pg`, site b routes nowhere with `AwaitingPeers` and never to `pg-b` until the hold ends, then to `pg-b`. The controller restarts again, and holds again: every start begins a new hold (RFC0002 requirement 32). Once the cut is removed, within 30s both sites route nowhere and the `Multiple` condition is `True`. |
| rbac | Least privilege | No Strait controller or gateway agent logged a `forbidden` request, though the controller writes Services and EndpointSlices in `db`, `web` and `strait-system` only. |

## Output

| Path | Holds |
|---|---|
| `_output/e2e/timings.txt` | Every measured duration, one line each, appended by every run. |
| `_output/e2e/<site>/` | On failure: Strait resources, CloudNativePG Clusters, pods, events, and Strait pod logs. |
