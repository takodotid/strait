# Roll back within 1.x

Roll back one site at a time, like an [upgrade](./upgrade.md). Keep the CRDs of the newer release.

## Steps

### 1. Do what the target release needs

A rollback across a release changes what the site does. Each table applies when the target release is before its version, and the site runs that version or later. Do each action before the rollback. A change with no action needs none.

#### Before 1.5.0

| Change | Action |
|---|---|
| A peer of a `wireguard` link can reach the gateway pod's own address through its tunnel again: the import, peer, probe and metrics ports. Where the pod's network namespace forwards, it can reach other addresses through the pod too. | |
| With `networkPolicy.gateway.enabled`, the gateway's policy admits every pod of the cluster, and `peerCIDRs`, to every port again, and the chart ignores `networkPolicy.gateway.ports` and `wireGuardPorts`. | |
| The `DeclarationUnmatched` condition of each `SiteService` keeps what 1.5 last wrote: the older controller does not update it. | Read a declaration that names no member from the route's message and `Declarations:` of `strait explain`. |

#### Before 1.4.0

| Change | Action |
|---|---|
| The gateway agent's requests to the API server have no timeout: one that hangs can hold a standby's takeover of the gateway Lease, or the agent's writes of status. | |
| A failed write of an `Import`'s Service or EndpointSlice is tried again with a backoff that grows to about 16 minutes. | |
| The `EndpointsLimited` condition of each `SiteLink` keeps what 1.4 last wrote: the older gateway agent does not update it. | Read endpoint-direct's ranges from `strait links`, `ENDPOINT-CIDRS`. |

#### Before 1.3.0

| Change | Action |
|---|---|
| `Site.spec.gateway.lease` is ignored: the gateway Lease takes 6s, 4s and 1s again. | With `Site.spec.gateway.lease` set: count a stall of the API server longer than 4s as a gateway handover. |
| With `cnpg.enabled: false`, the controller still watches CloudNativePG's kinds when their CRDs exist, and exits at each start. | On a cluster with CloudNativePG's CRDs, set `cnpg.enabled: true`. |
| The `GatewayConfig` condition of the `Site` keeps what 1.3 last wrote: the older controller does not update it, and neither logs nor counts a configuration that Envoy rejects. | Read the gateway's state from `strait status` and the gateway pods' logs. |
| `controller.namespacedReads` is ignored: the controller reads and caches every Service and EndpointSlice of the cluster again, with their `managedFields`, and its Go heap can grow to twice what it holds. | On a large cluster, size the controller's memory limit for 1.2.x's cache ([Sizing](../sizing.md)). |
| A site whose reconciles hang publishes its last observations as fresh again. | |
| A controller that cannot renew its Lease releases it before its leader-only work has stopped. | |
| A standby whose Envoy is not ready can take the gateway Lease. | |
| A gateway rollout can move the gateway Lease twice. | |
| The direct gateway Service can list a port twice, and the API server then refuses it. | |
| The chart's PodDisruptionBudget and the gateway's network policy are deleted, and the scheduling values are ignored. | |
| The `Site` loses `helm.sh/resource-policy: keep` ([The `Site`](#the-site)). | |
| Writes of Services and EndpointSlices are allowed in every namespace again, and `controller.namespaces` is ignored. | |
| The chart's PodMonitors and PrometheusRule are deleted, the gateway serves no metrics, and the metrics of 1.3 that the alert rules use are missing. | |
| Liveness checks only that the processes answer. | |

#### Before 1.2.1

| Change | Action |
|---|---|
| Local `Cluster`s that name different primaries route to the one writable member again, `Active`, where 1.2.1 routes nowhere with `DeclarationsDisagree`. | Make the local `Cluster`s of each `SiteService` name one primary: no route at the site is `DeclarationsDisagree` with a message that says `the workloads of site` and names this site. |
| A member whose Service lacks its port is `Unknown` with `ReadFailed` again, and is held while routed. | |
| A member whose addresses change family keeps an EndpointSlice with no endpoint until the slice is deleted. | |

#### Before 1.2.0

| Change | Action |
|---|---|
| `SiteLink.spec.direct.endpointCIDRs` is ignored, and endpoint-direct writes any address from the peer's document, unchecked ([RFC0005](../../rfcs/RFC0005.md#endpoint-direct-addresses), requirement 35). | Set `endpointDirect: false` on each `direct` link, unless every peer and the path to it are trusted. |
| The `Applied` condition of each `SiteService` keeps what 1.2 last wrote: the older controller does not update it. | Read the route from `strait status`. |

#### Before 1.1.3

| Change | Action |
|---|---|
| The controller does not release its leader election Lease when it stops. Each later restart or upgrade of the controller waits 15s more for the Lease ([RFC0004, Leader election](../../rfcs/RFC0004.md#leader-election)). | Count the 15s in the budget of the next upgrade. |

### 2. Find the revision

```sh
helm --kube-context site-a history strait --namespace strait-system
```

### 3. Roll back

In the block, replace `REVISION` with the revision of the target release, from the history that step 2 printed. Change `site-a` to the site's context.

```sh
helm --kube-context site-a rollback strait REVISION --namespace strait-system --wait
```

`helm upgrade --version VERSION` with the same values also works. If the rollback fails: [The pre-pull Job](#the-pre-pull-job).

### 4. Wait

Wait as in [step 3 of the upgrade](./upgrade.md#3-wait), before the next site.

## The pre-pull Job

From 1.3.0, a rollback to a release of 1.3.0 or later first runs the pre-pull Job with that release's image ([Upgrade](./upgrade.md)). When that image cannot be pulled, the rollback fails after Helm's `--timeout`, and the current release stays. `helm rollback --no-hooks` skips the Job: the controller then stops before its new pod pulls the image.

## The `Site`

The chart renders the cluster-scoped `Site` when `site.create` is true, the default. From 1.3.0, the `Site` carries `helm.sh/resource-policy: keep`. Helm reads that annotation from the `Site` in the cluster, so it protects the `Site` once a release of 1.3.0 or later has written it.

| Action | Effect |
|---|---|
| `helm uninstall` | Keeps the `Site`, and deletes the controller and the gateway. The CRDs and the `SiteService`s stay, and so do the Services and EndpointSlices that Strait wrote. Each application name keeps the endpoints written last, and nothing updates them: they no longer follow the primary. |
| `helm upgrade` with `site.create: false` | Keeps the `Site`. In one upgrade from a release before 1.3.0, it deletes the `Site`: upgrade to 1.3.0 with `site.create: true` first. |
| `helm install` again, after `helm uninstall` | With the same release name and namespace: takes the kept `Site` back. With another one, Helm refuses the install with `invalid ownership metadata`. |
| `helm rollback`, or `helm upgrade`, to a release before 1.3.0 | Removes the annotation. A later `helm uninstall`, or `site.create: false`, deletes the `Site`. |
| `helm upgrade` with `site.create: true` | Keeps the `Site`, or writes it again. |

Without its `Site`, the controller decides no route, and its names keep the endpoints written last. The gateway agent removes its WireGuard interfaces. The site's observation document names no site, so its peers reject it and count its members `Unknown`.

To restore a deleted `Site`, run `helm upgrade` with `site.create: true` and the same values. The controller and the gateway agent write its status again.
