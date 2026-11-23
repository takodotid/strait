# Troubleshooting

Each site decides its own routes and keeps its own view. Read each site with its own `--context`, and compare the sites.

A route to nowhere is a safe state, not a fault to work around. Find its cause and fix that. Strait follows. Do not edit Strait's resources to force a route.

## Start here

Four steps, in order. Run step 3 when a member's `STATE` is `Unknown`, and step 4 when a peer is not reachable.

### 1. Read every name's route

```sh
strait --context site-a status -A
```

Check: names whose `ROUTE` is `nowhere`, and their `REASON`. Peers whose `REACHABLE` is false.

### 2. Explain one name

```sh
strait --context site-a explain pg -n app
```

Check: `Reason` and `Meaning`, the members table, the declarations and the conditions.

`strait explain` prints the meaning of the route's reason. The skill document, `strait --skill`, lists every reason with its meaning. The rules that pick a reason: [RFC0002, Decision](../rfcs/RFC0002.md#decision).

### 3. Read the peers

Run when a member's `STATE` is `Unknown`.

```sh
strait --context site-a peers
```

Check: `REACHABLE` false, and `LAST ERROR`.

### 4. Read the links

Run when a peer is not reachable.

```sh
strait --context site-a links
```

Check: whether any link to that peer is `HEALTHY`.

## By route reason

Find the `Reason` of the name in `strait explain`, then read its row. `Active`, `PreferDeclared` and `Eligible` with failing connections, and `DeclarationsDisagree`, have their own headings below the table.

| Reason | Look at | Means and next step |
|---|---|---|
| `HeldUnknown`, `UnknownTimeout`, `MemberUnknown` | The `REASON` of the `Unknown` member. | [Unknown members](#unknown-members). |
| `Transitioning` | The `REASON` of the member whose `STATE` is `Transitioning`. | The workload is moving. The route comes back once the move ends. The `cnpg` reasons: [RFC0003, role and state](../rfcs/RFC0003.md#cnpg-role-and-state). A routine configuration change and the restart after a promotion also count: [RFC0003, Limitations](../rfcs/RFC0003.md#limitations). |
| `NotDeclared` | `Declarations:` and the members table. | One member is writable, but a site names another member. Compare each site's declaration with where the primary runs, and fix the workload's configuration. A declaration that is no member's name: see `DeclarationUnmatched` ([RFC0002](../rfcs/RFC0002.md), requirement 42). |
| `MultipleWritable` | The members whose `ROLE` is `primary` and `ELIGIBLE` is true. | Two members accept writes, for example after a partition heals. Demote one with the workload's own tools. The `Multiple` condition is True. |
| `NoEligibleMember` | The `REASON` of each member. | [Members that are not eligible](#members-that-are-not-eligible). |
| `SiteIsolated` | `strait peers`, and the route's `Message`. | `policy.isolationTimeout` is set, and this site has read no peer for that long. The local member is not routed. It ends when a peer is read again. After a site loss: [Site loss](runbooks/site-loss.md). |
| `AwaitingPeers` | `strait peers`, and the route's `Message`. | The first poll of every peer has not ended yet. The message gives the reason of the route decided before it. It ends within about one `pollInterval`. With `policy.startupHold`: [Site return, startupHold](runbooks/site-return.md#ahead-of-time-startuphold). |

### `Active`, `PreferDeclared` or `Eligible`, but connections fail

Look at: `Endpoints:`, and the `Routed` and `Applied` conditions.

- `Applied` is False with `SyncFailed`: a write of the route failed, and its message gives the error.
  - A message that says the namespace `is not one where the controller writes`: add the namespace to the chart value `controller.namespaces`, then `helm upgrade` ([RFC0007](../rfcs/RFC0007.md#writes-by-namespace)).
- `Routed` is False with `NoEndpoints`: the routed member has no ready endpoint.
- `Routed` is False with `ServiceConflict`: a Service with the `SiteService`'s name exists, and Strait does not own it. Strait deletes its EndpointSlices for that name and writes none until that Service is gone.
- For a routed member whose `PATH` is `Gateway`, check `strait links`.

### `DeclarationsDisagree`

Look at: `Declarations:` at each site, and the route's `Message`.

The sites name different primaries, or the `Cluster`s of one site do: the message then names that site, and its `Declarations:` line is empty. Make them agree.

- For `cnpg`, compare `.spec.replica.primary` of every `Cluster`.
- For `declared`, compare `spec.plugin.declared.active` at each site.

## Connections after a route change

### A client still reaches the previous member

The `SiteService`'s EndpointSlices no longer name it.

A route change never moves an established connection: the client must reconnect. A new connection that reaches the previous member comes from the CNI's connection tracking. For Cilium, check in the agent that socket load balancing is on:

```sh
cilium-dbg status --verbose
```

Check: `Socket LB: Enabled`, and that the agent's configuration does not set `bpf-lb-sock-hostns-only` to `"true"`. [RFC0002, Route changes and connections](../rfcs/RFC0002.md#route-changes-and-connections).

### A session through the gateway stays open after its member left the route

This is with `policy.connections: EndGateway`.

Set the chart value `envoy.drainTimeSeconds` to a few seconds ([RFC0002](../rfcs/RFC0002.md)). Replace `SITESERVICE-MEMBER` with the `SiteService`'s name and the member's name, joined by a hyphen:

```sh
kubectl get import SITESERVICE-MEMBER -o jsonpath='{.spec.closed}'
```

Check: the member's `Import` shows `true`.

A local client's session, or one over endpoint-direct, always stays ([RFC0002, What `EndGateway` covers and costs](../rfcs/RFC0002.md#what-endgateway-covers-and-costs)).

## Unknown members

The `REASON` column of the members table, for a member whose `STATE` is `Unknown`:

| Reason | Means | Next step |
|---|---|---|
| `SiteUnknown` | The member's site is neither this site nor a reachable peer. | `strait peers`. A `SitePeer` must exist whose name is the member's `site`. Then see [Unreachable peers](#unreachable-peers-and-unhealthy-links). |
| `NotObserved` | The member's site is reachable, but its document holds no observation of this member. | Apply the same `SiteService`, with the same namespace, name and member names, at the member's site. |
| `ObservationStale` | The member's site is reachable, but its controller last observed the member more than that site's `peerTimeout` ago, and at least 6s ago. Its reconciles fail or hang ([RFC0004](../rfcs/RFC0004.md#age-of-the-publishers-observations)). | The controller's logs at the member's site, and its API server. |
| `ReadFailed: ...` | The plugin could not read the workload at this site. The rest of the reason is the error. | Follow the error. For `cnpg`, check that CloudNativePG is installed at this site. |
| `RoleMismatch: ...` | `spec.plugin.cnpg.confirmRole` is on, and the database at the `-rw` address named in the reason gives a `pg_is_in_recovery()` answer that contradicts the `Cluster`'s role. CloudNativePG's status and the database disagree. | Run `kubectl get cluster NAME -o yaml` (`NAME` is the `Cluster`'s name) and compare `.status.currentPrimary` and `.spec.replica` with the database. Fix the workload with its own tools. Strait routes nowhere rather than pick one ([RFC0003](../rfcs/RFC0003.md#cnpg-confirming-the-role)). |
| `RoleNotConfirmed: ...` | `confirmRole` is on, and the check got no answer. The rest of the reason is the error: no credentials, a refused login, a timeout, or no ready address. | Set the chart value `cnpg.confirmRole.secretName` to a Secret with `username` and `password` of a role with `LOGIN`, at this site. Check that the role exists, may log in to `postgres` over TLS, and that the controller's pod reaches the database's pods on 5432. |
| `PluginDisabled` | The member's plugin is off at this site: the chart value `cnpg.enabled` is `false`. | Set `cnpg.enabled: true` with `helm upgrade` at the member's site. |

## Members that are not eligible

| Reason | Means | Next step |
|---|---|---|
| `ClusterNotFound` | No CloudNativePG `Cluster` named `members[].cnpg.cluster` in the `SiteService`'s namespace. | The name, and the namespace: a member's `Cluster` and its `SiteService` share one. |
| `ServiceNotFound` | The member's endpoint Service does not exist. For `cnpg`, the `Cluster`'s Service for the role ([RFC0003](../rfcs/RFC0003.md#cnpg-endpoint-by-role)). For `declared`, `members[].service.name`. | The workload's Services. |
| `NoReadyEndpoint` | The endpoint Service has no ready address. | The workload's pods. |
| `PortNotFound` | The member's endpoint Service has no port of the number it needs. For `cnpg`, port 5432 on the `Cluster`'s Service or the `Pooler`'s. For `declared`, `members[].service.port`. | The Service's ports. |
| `PoolerNotFound`, `PoolerOfAnotherCluster`, `PoolerTypeMismatch`, `PoolerPaused` | The `Pooler` named in `members[].cnpg.pooler` cannot be used. | The `Pooler` must exist, belong to the member's `Cluster`, match the role, and not be paused. |
| `ReplicaCluster` | The `Cluster` is a replica cluster, so it is not writable. | None. Expected for every member but the primary. |
| `PrimaryChanging`, `Demoting`, `Promoting`, `NotYetWritable`, `NotYetReadOnly`, `Phase: ...` | CloudNativePG is moving the primary. The `STATE` is `Transitioning`. | Wait for the move to end. [RFC0003, role and state](../rfcs/RFC0003.md#cnpg-role-and-state). |

An eligible member shows `PrimaryCluster`, `Ready` or `Declared`.

## Unreachable peers and unhealthy links

Read both ends: a link needs the gateway at each site.

Steps 3 and 4 of [Start here](#start-here) read the peers and the links.

### Read the link's condition

```sh
kubectl --context site-a get sitelink b-wan -o yaml
```

Check: the reason and message of the `Healthy` condition.

### Read the site's conditions

```sh
kubectl --context site-a get site a -o yaml
```

Check: the `GatewayActive`, `GatewayServices` and `GatewayConfig` conditions.

### What the symptom means

| Symptom | Cause and next step |
|---|---|
| `LAST ERROR` holds `site mismatch` | The peer's document names another site. Name the `SitePeer` after the peer's `site.name`. |
| A healthy link, but the peer is not reachable | The peer's controller does not serve its document. Run `strait --context site-b status` at the peer, and read its controller's logs. |
| No healthy link to the peer | See the `Healthy` condition below. |


### The `Healthy` condition of a `SiteLink`

#### `NotConfigured`

The link's WireGuard interface is not in place. The message says why. Look at:

- The site's private key: the Secret is missing, or holds no valid key.
- The `SitePeer`: missing, or no valid public key.
- A listen port taken by another link.
- The kernel's error when it creates the interface, for example on a node without the `wireguard` module.
- A message that starts with `filter the WireGuard interfaces' traffic`: the agent could not write its nftables table, for example on a node without `nf_tables`, and holds no interface until it can ([RFC0007](../rfcs/RFC0007.md#links), requirement 12).

#### `HealthCheckFailed`

The peer's control port does not answer `GET /healthz` over this link.

For `wireguard`, `strait links -o yaml` shows `lastHandshakeTime`. With no handshake, check that each `SitePeer` holds the other site's current public key, and that the waiting side's listen port is reachable over UDP. On the side with an `endpoint`, the gateway agent logs `the WireGuard peer sent nothing` when the peer stops answering, then drops the session each second until it answers, which clears `lastHandshakeTime` ([RFC0005](../rfcs/RFC0005.md#wireguard-sessions), requirement 37).

For `direct`, check the link's `direct.address`, and how the peer exposes `directService`. With a `NodePort` and `externalTrafficPolicy: Local`, the address must be the node of the peer's active gateway: [RFC0005, Limitations](../rfcs/RFC0005.md#limitations).

#### `NotInEnvoy`

Envoy has no endpoint at this link's address yet. Brief after a change. If it stays, read the controller's logs.

#### `EndpointUnhealthy`

Envoy marks the endpoint unhealthy. Read the controller's and the gateway agent's logs.

#### `EnvoyUnreachable`

The gateway agent cannot read Envoy's admin interface. Read the `envoy` container of the active gateway pod.

A `direct` `SiteLink` with `endpointDirect` also has the condition `EndpointsLimited`. When it is False, with `NoEndpointCIDRs`, endpoint-direct writes any member address from the peer's document, except unspecified, loopback, link-local and multicast addresses. Set `endpointCIDRs` to the peer's pod CIDRs ([RFC0005, Endpoint-direct](../rfcs/RFC0005.md#endpoint-direct)).

### The conditions of the `Site`

#### `GatewayActive` False, `NoActiveGateway`

No ready gateway pod holds the gateway lease. Read the pods and events:

```sh
kubectl --context site-a -n strait-system get pods,events
```

The API server rejects the gateway pods unless the namespace's Pod Security level is `privileged`.

#### `GatewayServices` False, `DirectServiceFailed` or `WireGuardServiceFailed`

The controller could not write a gateway Service. The message holds the API server's error.

An example is a WireGuard listen port outside the cluster's node port range, with a `NodePort` or `LoadBalancer` Service. Another is a node port that another Service already holds. Find it, and move that Service to another port ([RFC0005, Limitations](../rfcs/RFC0005.md#limitations)):

```sh
kubectl --context site-a get services -A
```

#### `GatewayConfig` False, `Rejected`

A gateway pod's Envoy rejected its configuration, and keeps the one it accepted last. New exports, imports and links may be missing from it. The message names the pod, the type and Envoy's error.

Read the controller's log, `a gateway Envoy rejected its configuration`. The condition clears once the pod accepts a later configuration. A rejection is a defect in Strait: report it with the error.

#### `GatewayActive` moves to another pod while nothing failed

The active gateway replica could not renew the gateway Lease within `spec.gateway.lease.renewDeadline`, for example while the API server stalled. Every handover resets connections: [Gateway handover](runbooks/gateway-handover.md).

Check the API server's availability. A longer `renewDeadline` and `duration` ride out longer stalls, and keep a failed replica active for longer ([RFC0005, Timing of the gateway Lease](../rfcs/RFC0005.md#timing-of-the-gateway-lease)).

## Logs, events and metrics

### Events

```sh
kubectl --context site-a -n app get events --field-selector involvedObject.name=pg
```

Each route change is a `RouteChanged` event. `MultipleWritable`, `DeclarationsDisagree`, `DeclarationUnmatched` and `UnknownTimeout` are Warning events.

### Controller logs

```sh
kubectl --context site-a -n strait-system logs deployment/strait-controller
```

An `Import` whose Service is not written logs `Import Service not synced` when its error is new. `controller.logLevel: debug` logs each retry, every 2s.

### CloudNativePG watch

The controller logs `cloudnative-pg` at start. `enabled` follows `cnpg.enabled`. `watched` false with `enabled` true means CloudNativePG's CRDs were missing then: restart the controller. [Getting started, The cnpg plugin switch](getting-started.md#the-cnpg-plugin-switch).

### Gateway agent logs

`POD` is the active gateway pod that `strait status` shows:

```sh
kubectl --context site-a -n strait-system logs POD -c agent
```

Each link that fails, comes back, or starts or stops carrying traffic logs `SiteLink status`. `gateway.logLevel: debug` logs every write of a link's status.

### Gateway handovers

```sh
kubectl --context site-a get events -A --field-selector reason=GatewayHandover
```

Each one resets every connection: [Gateway handover](runbooks/gateway-handover.md).

### Metrics

Port `metrics`, 8080, of the controller pod, and 8091 of each gateway pod. Every metric, the scrape configuration and the alert rules: [Metrics and alerts](metrics.md). A route to nowhere shows as `strait_siteservice_endpoints` 0.

## Runbooks

| Case | Runbook |
|---|---|
| A site is lost, and a member at another site must become primary. | [Site loss](runbooks/site-loss.md) |
| A lost site comes back, or a partition heals, maybe with two primaries. `startupHold` for production. | [Site return](runbooks/site-return.md) |
| A site joins the others, or leaves them. | [Add or remove a site](runbooks/peers.md) |
| A site goes into production: socket load balancing, clients that write, gateway node ports, `endpointCIDRs`. | [Before production](runbooks/before-production.md) |
| A site's WireGuard private key must change. | [WireGuard key rotation](runbooks/wireguard-key-rotation.md) |
| The controller or a gateway pod does not become ready, or restarts. | [Not ready](runbooks/not-ready.md) |
| The active gateway moves to another replica, or must move off a node. | [Gateway handover](runbooks/gateway-handover.md) |
| An upgrade, or a node drain. | [Upgrade](runbooks/upgrade.md) |
| A site must go back to an earlier 1.x release. | [Roll back within 1.x](runbooks/rollback.md) |
| The operator publishes a release, or the release workflow refuses one. | [Release](runbooks/release.md) |
| Someone checks that a release's CLI archive or image is what its tag builds. | [Rebuild a release](runbooks/rebuild.md) |
