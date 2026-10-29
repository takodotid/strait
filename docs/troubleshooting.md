# Troubleshooting

Each site decides its own routes and keeps its own view. Read each site with its own `--context`, and compare the sites.

A route to nowhere is a safe state, not a fault to work around. Find its cause and fix the cause: Strait follows. Do not edit Strait's resources to force a route.

## Start here

| Step | Command | Look at |
|---|---|---|
| 1 | `strait --context site-a status -A` | Names whose `ROUTE` is `nowhere`, and their `REASON`. Peers whose `REACHABLE` is false. |
| 2 | `strait --context site-a explain pg -n app` | `Reason` and `Meaning`, the members table, the declarations and the conditions. |
| 3 | `strait --context site-a peers` | When a member's `STATE` is `Unknown`. |
| 4 | `strait --context site-a links` | When a peer is not reachable. |

`strait explain` prints the meaning of the route's reason. The skill document, `strait --skill`, lists every reason with its meaning. The rules that pick a reason: [RFC0002, Decision](../rfcs/RFC0002.md#decision).

## By route reason

| Reason | Look at | Cause and next step |
|---|---|---|
| `Active`, `PreferDeclared` or `Eligible`, but connections fail | `Endpoints:` and the `Routed` condition | `Routed` is False with `NoEndpoints`: the routed member has no ready endpoint. With `ServiceConflict`: a Service with the `SiteService`'s name exists, and Strait does not own it. For a routed member whose `PATH` is `Gateway`, check `strait links`. |
| `HeldUnknown` | The `REASON` of the `Unknown` member | The route is kept, since the member was confirmed less than `policy.unknownHold` ago. See [Unknown members](#unknown-members). |
| `UnknownTimeout`, `MemberUnknown` | The `REASON` of the `Unknown` member | See [Unknown members](#unknown-members). |
| `Transitioning` | The `REASON` of the member whose `STATE` is `Transitioning` | The workload is moving. The route comes back once the move ends. The `cnpg` reasons: [RFC0003, role and state](../rfcs/RFC0003.md#cnpg-role-and-state). A routine configuration change and the restart after a promotion also count: [RFC0003, Limitations](../rfcs/RFC0003.md#limitations). |
| `DeclarationsDisagree` | `Declarations:` at each site | The sites name different primaries. For `cnpg`, compare `.spec.replica.primary` of each site's `Cluster`. For `declared`, compare `spec.plugin.declared.active` at each site. Make them agree. |
| `NotDeclared` | `Declarations:` and the members table | One member is writable, but a site names another member. Compare each site's declaration with where the primary runs, and fix the workload's configuration. |
| `MultipleWritable` | The members whose `ROLE` is `primary` and `ELIGIBLE` is true | Two members accept writes, for example after a partition heals. Strait never picks one. Demote one with the workload's own tools, and Strait follows. The `Multiple` condition is True. |
| `NoEligibleMember` | The `REASON` of each member | No member can take traffic. See [Members that are not eligible](#members-that-are-not-eligible). |
| `AwaitingPeers` | `strait peers` | `policy.startupHold` is set, and the controller started recently. It has not yet read every site with a member, so it does not route to the local member. The route follows once it reads them, or once the hold ends. |

## Unknown members

The `REASON` column of the members table, for a member whose `STATE` is `Unknown`:

| Reason | Means | Next step |
|---|---|---|
| `SiteUnknown` | The member's site is neither this site nor a reachable peer. | `strait peers`. A `SitePeer` must exist whose name is the member's `site`. Then see [Unreachable peers](#unreachable-peers-and-unhealthy-links). |
| `NotObserved` | The member's site is reachable, but its document holds no observation of this member. | Apply the same `SiteService`, with the same namespace, name and member names, at the member's site. |
| `ReadFailed: ...` | The plugin could not read the workload at this site. The rest of the reason is the error. | Follow the error. For `cnpg`, check that CloudNativePG is installed at this site. |

While a peer is unreachable, every member at that peer counts as `Unknown` ([RFC0004](../rfcs/RFC0004.md)). A routed member stays in the route for `policy.unknownHold` after it was last confirmed, then the name routes nowhere.

## Members that are not eligible

| Reason | Means | Next step |
|---|---|---|
| `ClusterNotFound` | No CloudNativePG `Cluster` named `members[].cnpg.cluster` in the `SiteService`'s namespace. | The name, and the namespace: a member's `Cluster` and its `SiteService` share one. |
| `ServiceNotFound` | The member's endpoint Service does not exist. For `cnpg`, the `Cluster`'s Service for the role ([RFC0003](../rfcs/RFC0003.md#cnpg-endpoint-by-role)). For `declared`, `members[].service.name`. | The workload's Services. |
| `NoReadyEndpoint` | The endpoint Service has no ready address. | The workload's pods. |
| `PoolerNotFound`, `PoolerOfAnotherCluster`, `PoolerTypeMismatch`, `PoolerPaused` | The `Pooler` named in `members[].cnpg.pooler` cannot be used. | The `Pooler` must exist, belong to the member's `Cluster`, match the role, and not be paused. |
| `ReplicaCluster` | The `Cluster` is a replica cluster, so it is not writable. | None. Expected for every member but the primary. |
| `PrimaryChanging`, `Demoting`, `Promoting`, `NotYetWritable`, `NotYetReadOnly`, `Phase: ...` | CloudNativePG is moving the primary. The `STATE` is `Transitioning`. | Wait for the move to end. [RFC0003, role and state](../rfcs/RFC0003.md#cnpg-role-and-state). |

An eligible member shows `PrimaryCluster`, `Ready` or `Declared`.

## Unreachable peers and unhealthy links

Read both ends: a link needs the gateway at each site.

| Step | Command | Look at |
|---|---|---|
| 1 | `strait --context site-a peers` | `REACHABLE` false, and `LAST ERROR`. |
| 2 | `strait --context site-a links` | Whether any link to that peer is `HEALTHY`. |
| 3 | `kubectl --context site-a get sitelink b-wan -o yaml` | The reason and message of the `Healthy` condition. |
| 4 | `kubectl --context site-a get site a -o yaml` | The `GatewayActive` and `GatewayServices` conditions. |

| Symptom | Cause and next step |
|---|---|
| `LAST ERROR` holds `site mismatch` | The peer's document names another site. Name the `SitePeer` after the peer's `site.name`. |
| A healthy link, but the peer is not reachable | The peer's controller does not serve its document. Check the peer: `strait --context site-b status`, and its controller's logs. |
| No healthy link to the peer | See the `Healthy` condition below. |

The `Healthy` condition of a `SiteLink`:

| Reason | Means | Next step |
|---|---|---|
| `NotConfigured` | The link's WireGuard interface is not in place. The message says why. | The site's private key: the Secret is missing, or holds no valid key. The `SitePeer`: missing, or no valid public key. A listen port taken by another link. Or the kernel's error when it creates the interface, for example on a node without the `wireguard` module. |
| `HealthCheckFailed` | The peer's control port does not answer `GET /healthz` over this link. | For `wireguard`: `strait links -o yaml` shows `lastHandshakeTime`. With no handshake, check that each `SitePeer` holds the other site's current public key, and that the waiting side's listen port is reachable over UDP. For `direct`: the link's `direct.address`, and how the peer exposes `directService`. |
| `NotInEnvoy` | Envoy has no endpoint at this link's address yet. | Brief after a change. If it stays, the controller's logs. |
| `EndpointUnhealthy` | Envoy marks the endpoint unhealthy. | The controller's and the gateway agent's logs. |
| `EnvoyUnreachable` | The gateway agent cannot read Envoy's admin interface. | The `envoy` container of the active gateway pod. |

The conditions of the `Site`:

| Condition | Means | Next step |
|---|---|---|
| `GatewayActive` False, `NoActiveGateway` | No ready gateway pod holds the gateway lease. | `kubectl --context site-a -n strait-system get pods,events`. The API server rejects the gateway pods unless the namespace's Pod Security level is `privileged`. |
| `GatewayServices` False, `DirectServiceFailed` or `WireGuardServiceFailed` | The controller could not write a gateway Service. The message holds the API server's error. | For example, a WireGuard listen port outside the cluster's node port range, with a `NodePort` or `LoadBalancer` Service. |

## Logs, events and metrics

| Source | Command or place |
|---|---|
| Events | `kubectl --context site-a -n app get events --field-selector involvedObject.name=pg`. Each route change is a `RouteChanged` event. `MultipleWritable`, `DeclarationsDisagree` and `UnknownTimeout` are Warning events. |
| Controller logs | `kubectl --context site-a -n strait-system logs deployment/strait-controller` |
| CloudNativePG watch | The controller logs `cloudnative-pg` at start. `watched` false means CloudNativePG's CRDs were missing then: restart the controller. |
| Gateway agent logs | `kubectl --context site-a -n strait-system logs POD -c agent`, where `POD` is the active gateway pod that `strait status` shows. |
| Metrics | Port `metrics`, 8080, of the controller pod. The route, the conditions, peer reachability and link health: [RFC0002, Metrics](../rfcs/RFC0002.md#metrics). A route to nowhere shows as `strait_siteservice_endpoints` 0. |
