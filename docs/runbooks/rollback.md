# Roll back within 1.x

Roll back one site at a time, as an [upgrade](../../README.md#upgrade): the site shows the same controller gap and gateway handover. Keep the CRDs of the newer release.

```sh
helm --kube-context site-a history strait --namespace strait-system
helm --kube-context site-a rollback strait REVISION --namespace strait-system --wait
```

`helm upgrade --version VERSION` with the same values also works. Then wait as in step 3 of the upgrade, before the next site.

From 1.3.0, a rollback to a release of 1.3.0 or later first runs the pre-pull Job with that release's image ([Upgrade](../../README.md#upgrade)). When that image cannot be pulled, the rollback fails after Helm's `--timeout`, and the current release stays. `helm rollback --no-hooks` skips the Job: the controller then stops before its new pod pulls the image.

## Compatibility

| Item | Between 1.x releases |
|---|---|
| Observation document | The same in every 1.x release. Sites on different 1.x releases read each other. |
| CRDs | Every 1.x release reads the CRDs of a later 1.x release. Helm never downgrades CRDs, so the newer ones stay. |

Rolling back across these releases changes what the site does:

| Roll back to | Effect | Before the rollback |
|---|---|---|
| Before 1.3.0 | `Site.spec.gateway.lease` is ignored: the gateway Lease takes 6s, 4s and 1s again. A site whose reconciles hang publishes its last observations as fresh again. A controller that cannot renew its Lease releases it before its leader-only work has stopped. A standby whose Envoy is not ready can take the gateway Lease. The direct gateway Service can list a port twice, and the API server then refuses it. The chart's PodDisruptionBudget and the gateway's network policy are deleted, and the scheduling values are ignored. With `cnpg.enabled: false`, the controller still watches CloudNativePG's kinds when their CRDs exist, and exits at each start. The `Site` loses `helm.sh/resource-policy: keep` ([The `Site`](#the-site)). A gateway rollout can move the gateway Lease twice. Writes of Services and EndpointSlices are allowed in every namespace again, and `controller.namespaces` is ignored. A rollback to it runs no pre-pull Job. The `GatewayConfig` condition of the `Site` keeps what 1.3 last wrote: the older controller does not update it, and neither logs nor counts a configuration that Envoy rejects. The chart's PodMonitors and PrometheusRule are deleted, the gateway serves no metrics, and the metrics of 1.3 that the alert rules use are missing. Liveness checks only that the processes answer. `controller.namespacedReads` is ignored: the controller reads and caches every Service and EndpointSlice of the cluster again, with their `managedFields`, and its Go heap can grow to twice what it holds ([Sizing](../sizing.md)). | With `Site.spec.gateway.lease` set: count a stall of the API server longer than 4s as a gateway handover. On a cluster with CloudNativePG's CRDs, set `cnpg.enabled: true`. Read the gateway's state from `strait status` and the gateway pods' logs, not from `GatewayConfig`. On a large cluster, size the controller's memory limit for 1.2.x's cache ([Sizing](../sizing.md)). |
| Before 1.2.1 | Local `Cluster`s that name different primaries route to the one writable member again, `Active`, where 1.2.1 routes nowhere with `DeclarationsDisagree`. A member whose Service lacks its port is `Unknown` with `ReadFailed` again, and is held while routed. A member whose addresses change family keeps an EndpointSlice with no endpoint until the slice is deleted. | Make the local `Cluster`s of each `SiteService` name one primary: no route at the site is `DeclarationsDisagree` with a message that says `the workloads of site` and names this site. |
| Before 1.2.0 | `SiteLink.spec.direct.endpointCIDRs` is ignored, and endpoint-direct writes any address from the peer's document, unchecked ([RFC0005](../../rfcs/RFC0005.md#endpoint-direct-addresses), requirement 35). The `Applied` condition of each `SiteService` keeps what 1.2 last wrote: the older controller does not update it. | Set `endpointDirect: false` on each `direct` link, unless every peer and the path to it are trusted. Read the route from `strait status`, not from `Applied`. |
| Before 1.1.3 | The controller does not release its leader election Lease when it stops. Each later restart or upgrade of the controller waits 15s more for the Lease ([RFC0004, Leader election](../../rfcs/RFC0004.md#leader-election)). | Count the 15s in the budget of the next upgrade. |

## The `Site`

The chart renders the cluster-scoped `Site` when `site.create` is true, the default. From 1.3.0, the `Site` carries `helm.sh/resource-policy: keep`. Helm reads that annotation from the `Site` in the cluster, so it protects the `Site` once a release of 1.3.0 or later has written it.

| Action | Effect |
|---|---|
| `helm uninstall` | Keeps the `Site`, and deletes the controller and the gateway. The CRDs and the `SiteService`s stay, and so do the Services and EndpointSlices that Strait wrote. Each application name keeps the endpoints written last, and nothing updates them: they no longer follow the primary. |
| `helm upgrade` with `site.create: false` | Keeps the `Site`. From a release before 1.3.0 in one upgrade, it deletes the `Site`: upgrade to 1.3.0 with `site.create: true` first. |
| `helm install` again, after `helm uninstall` | With the same release name and namespace: takes the kept `Site` back. With another one, Helm refuses the install with `invalid ownership metadata`. |
| `helm rollback`, or `helm upgrade`, to a release before 1.3.0 | Removes the annotation. A later `helm uninstall`, or `site.create: false`, deletes the `Site`. |
| `helm upgrade` with `site.create: true` | Keeps the `Site`, or writes it again. |

Without its `Site`, the controller decides no route, and its names keep the endpoints written last. The gateway agent removes its WireGuard interfaces. The site's observation document names no site, so its peers reject it and count its members `Unknown`.

Never roll back with `helm uninstall`. To remove Strait from a site, first delete its `SiteService`s, so that their Services and EndpointSlices go with them, then run `helm uninstall` and delete the `Site`.

To restore a deleted `Site`, run `helm upgrade` with `site.create: true` and the same values. The controller and the gateway agent write its status again.
