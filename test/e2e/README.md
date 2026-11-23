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

The test prints each site's kubeconfig path and context at start. It never touches `~/.kube/config`.

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

The host needs network access to pull the images and the CloudNativePG manifest. CloudNativePG's operator Deployment pulls with `imagePullPolicy: Always`, so its node checks the registry even with the image loaded.

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

The test creates both networks when absent and never deletes them: other `strait-*` clusters share them.

Faults are nftables tables inside the node containers: `strait-cut-direct`, `strait-cut-wan`, and `strait-cut-site` with `docker pause` for a lost site.

## Scenarios

They run in this order on one fixture and stop at the first failure.

| # | Scenario | Checks |
|---|---|---|
| 1 | Baseline | Both sites route `pg` to `pg-a`. A write from each site lands on `cluster-a`. |
| role-check | Role confirmed with the database | `confirmRole` on `pg` at site a. With the role's password changed, `pg-a` is `Unknown` with `RoleNotConfirmed` and site a routes nowhere; with it back, both sites route `pg-a`. |
| import | Import Service | An `Export` of `web-a` at site a and an `Import` with `port` at site b: a request from b to `web-a.web.svc` reaches `web-a`. Without `port`, b deletes the Service. |
| switch | A route change through nowhere | At site a, the `declared` SiteService `local` moves its route from `one` to `two` in one decision. A watch of its EndpointSlices never sees both members at once. |
| conflict | A Service-name conflict | Strait's Service `local` at site a loses its owner. Strait deletes its EndpointSlices and `Routed` is False with `ServiceConflict`. Once the Service is deleted, Strait writes its own again. |
| ports | Ports of objects created together | 8 `Export`s at site a and 8 `Import`s at site b, created in parallel, each get a gateway port. No two hold one port at any read. |
| envoy-not-ready | Envoy of the active gateway not ready | The Envoy container of site a's active gateway replica is paused. The standby takes the gateway Lease, and a write from site b lands on `cluster-a` again. Then Envoy resumes. |
| gateway-handover | A handover at the site whose wireguard link waits | With the direct link cut, site a's active gateway pod is deleted. A write from site b lands on `cluster-a` within 8s of the standby taking the gateway Lease. |
| gateway-rollout | A rollout of the gateway | Site a's gateway Deployment restarts, as an upgrade rolls it. Only the active pod has a deletion cost, and the rollout moves the gateway Lease once. |
| 2 | Link failover | Cutting the direct link makes `a-wan` active at site b within 10s. Writes keep succeeding; a new connection from b started 5s or more after the cut reaches `cluster-a`. |
| 3 | Switchover with tokens | Site a stops naming `pg-a` within one 500ms sample of the demotion patch, site b within `peerTimeout`. Both sites pass through nowhere, then route `pg-b`. |
| 4 | Partition | Site b keeps `pg-b` with reason `Active`. Site a holds `pg-b` (`HeldUnknown`), then routes nowhere once `unknownHold` has passed. After the heal, a routes `pg-b`. |
| 4b | Isolated site | Step 4's partition with `isolationTimeout: 8s` at both sites. Site b routes `pg-b`, then nowhere with `SiteIsolated`; a connection at b is refused. After the heal, both route `pg-b`. |
| 5 | Disagreeing declarations | Two `declared` SiteServices that name different members route nowhere at both sites. Once they agree, a request from each site reaches the declared member. |
| 5b | Ending gateway sessions | At site b, `web` routes to `web-a` through the gateway; the client pod holds an idle TCP session. With `EndGateway`, moving the route to `web-b` ends the session within 20s. |
| 6 | Site loss | Site a holds `pg-b`, then routes nowhere. After the operator promotes `cluster-a` without a token, a routes `pg-a`. |
| 7 | Two writable members | Site b returns with `cluster-b` still primary, cut off from site a. With `startupHold: 30s`, its restarted controller routes nowhere with `AwaitingPeers` until the hold ends, then `pg-b`. |
| rbac | Least privilege | No Strait controller or gateway agent logged a `forbidden` request, though the controller writes Services and EndpointSlices in `db`, `web` and `strait-system` only. |

Details the cells leave out:

- `role-check`: the role `strait_role_check` (`LOGIN` only) is created on `cluster-a`. `pg-a` stays routed while the password is right. Site a routes nowhere once `unknownHold` has passed. `confirmRole` is off afterwards.
- `import`: under the gateway's policy, a pod of b reaches the active gateway pod's import port on the pod's address, and times out on 8091.
- `switch`: `local` has two members on `web-a`.
- `conflict`: Strait also writes the slice of `two` again; then `local` is deleted.
- `ports`: the ports are read every 100ms. The race needs a reconcile before the cache shows the last port, so a build without the fix can pass.
- 2: a new connection from b is made every 250ms. The 5s bound is RFC0005 requirement 4.
- 3: `pg-b` never shows before the promotion patch.
- 4: a connection at a is refused in under 2s. After the heal, a routes `pg-b` within 15s.
- 4b: step 4 is the same partition with the policy off. Site b goes nowhere no sooner than `isolationTimeout` after the cut, minus one poll. Site a routes nowhere as in step 4. At b, `0s` routes `pg-b` again, and setting it back isolates b again. The field is removed afterwards.
- 5b: with `policy.connections` unset, the same move keeps the session for 20s. `EndGateway` is set at site b, and after the move a new request from b reaches `web-b`. `web-a` stays up throughout. The policy is removed afterwards.
- 7: site b never routes `pg-b` before the hold ends. The controller restarts again and holds again: every start begins a new hold (RFC0002 requirement 32). Once the cut is removed, within 30s both sites route nowhere and the `Multiple` condition is `True`.

## Output

| Path | Holds |
|---|---|
| `_output/e2e/timings.txt` | Every measured duration, one line each, appended by every run. |
| `_output/e2e/<site>/` | On failure: Strait resources, CloudNativePG Clusters, pods, events, and Strait pod logs. |
