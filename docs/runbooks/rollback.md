# Roll back within 1.x

Roll back one site at a time, as an [upgrade](../../README.md#upgrade): the site shows the same controller gap and gateway handover. Keep the CRDs of the newer release.

```sh
helm --kube-context site-a history strait --namespace strait-system
helm --kube-context site-a rollback strait REVISION --namespace strait-system --wait
```

`helm upgrade --version VERSION` with the same values also works. Then wait as in step 3 of the upgrade, before the next site.

## Compatibility

| Item | Between 1.x releases |
|---|---|
| Observation document | The same in every 1.x release. Sites on different 1.x releases read each other. |
| CRDs | Every 1.x release reads the CRDs of a later 1.x release. Helm never downgrades CRDs, so the newer ones stay. |

Rolling back across these releases changes what the site does:

| Roll back to | Effect | Before the rollback |
|---|---|---|
| Before 1.3.0 | `Site.spec.gateway.lease` is ignored: the gateway Lease takes 6s, 4s and 1s again. A site whose reconciles hang publishes its last observations as fresh again. A controller that cannot renew its Lease releases it before its leader-only work has stopped. A standby whose Envoy is not ready can take the gateway Lease. The direct gateway Service can list a port twice, and the API server then refuses it. The chart's PodDisruptionBudget and the gateway's network policy are deleted, and the scheduling values are ignored. | With `Site.spec.gateway.lease` set: count a stall of the API server longer than 4s as a gateway handover. |
| Before 1.2.1 | Local `Cluster`s that name different primaries route to the one writable member again, `Active`, where 1.2.1 routes nowhere with `DeclarationsDisagree`. A member whose Service lacks its port is `Unknown` with `ReadFailed` again, and is held while routed. A member whose addresses change family keeps an EndpointSlice with no endpoint until the slice is deleted. | Make the local `Cluster`s of each `SiteService` name one primary: no route at the site is `DeclarationsDisagree` with a message that says `the workloads of site` and names this site. |
| Before 1.2.0 | `SiteLink.spec.direct.endpointCIDRs` is ignored, and endpoint-direct writes any address from the peer's document, unchecked ([RFC0005](../../rfcs/RFC0005.md#endpoint-direct-addresses), requirement 35). The `Applied` condition of each `SiteService` keeps what 1.2 last wrote: the older controller does not update it. | Set `endpointDirect: false` on each `direct` link, unless every peer and the path to it are trusted. Read the route from `strait status`, not from `Applied`. |
| Before 1.1.3 | The controller does not release its leader election Lease when it stops. Each later restart or upgrade of the controller waits 15s more for the Lease ([RFC0004, Leader election](../../rfcs/RFC0004.md#leader-election)). | Count the 15s in the budget of the next upgrade. |

## The `Site`

The chart renders the cluster-scoped `Site` when `site.create` is true, the default.

| Action | Effect |
|---|---|
| `helm uninstall` | Deletes the `Site`, the controller and the gateway. The CRDs and the `SiteService`s stay, and so do the Services and EndpointSlices that Strait wrote. Each application name keeps the endpoints written last, and nothing updates them: they no longer follow the primary. |
| `helm upgrade` with `site.create: false` on a release that created the `Site` | Deletes the `Site`. The controller then decides no route, and its names keep the endpoints written last. The gateway agent removes its WireGuard interfaces. The site's observation document names no site, so its peers reject it and count its members `Unknown`. |
| `helm rollback`, or `helm upgrade` with `site.create: true` | Keeps the `Site`. |

Never roll back with `helm uninstall`. To remove Strait from a site, first delete its `SiteService`s, so that their Services and EndpointSlices go with them.

To restore a deleted `Site`, run `helm upgrade` with `site.create: true` and the same values. The controller and the gateway agent write its status again.
