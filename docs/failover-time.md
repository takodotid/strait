# Failover time

This page adds up Strait's share of the time until writes work again after a failover. It covers detection, holds, the workload's own move, and applying the new route. Each row gives the default and what to tune.

Strait never moves a primary. The workload's controller or the operator does that, and Strait follows ([RFC0001](../rfcs/RFC0001.md)). The time that the workload takes, such as a database promotion, is in the budget because Strait routes nowhere while it lasts.

Real timings come from a run on real clusters, not from CI or kind.

## When the route became `Active` again

Read `status.route.lastChangeTime` together with `status.route.reason`:

```sh
kubectl --context site-b -n app get siteservice pg \
  -o jsonpath='{.status.route.reason} {.status.route.members} {.status.route.lastChangeTime}{"\n"}'
```

| Field | Means |
|---|---|
| `status.route.lastChangeTime` | The time of the decision whose status first showed the current members, on the controller's clock, to the second. It does not change while the members stay, also while a member is held `Unknown`. |
| `status.route.reason` | `Active` once the one writable member is routed. |
| The `Routed` condition's `lastTransitionTime` | When the Service last went from no endpoint to an endpoint, or back. |

After a failover that passes through nowhere, a reason of `Active` with a `lastChangeTime` after the failover began is the time writes could reach the new primary again ([RFC0002](../rfcs/RFC0002.md#requirements), requirement 24). Each site has its own `SiteService`, so read each site.

## The parts

| Part | What decides it | Default | Tune |
|---|---|---|---|
| A local member's change seen | The controller watches CloudNativePG's `Cluster`s and decides again at once, and at least every 2s. | Under 2s | Nothing. |
| A remote member's change seen | Each site polls its peers every `pollInterval`. A change shows at the other sites within `peerTimeout` ([RFC0004](../rfcs/RFC0004.md), requirement 13). | `pollInterval` 2s, `peerTimeout` 6s | `Site.spec.exchange`. `peerTimeout` must be at least twice `pollInterval`. Shorter values notice faster, and also turn a short flap into `Unknown` sooner. |
| A lost site seen | Its peer's document goes stale after `peerTimeout`. Its members are then `Unknown`. | 6s | `Site.spec.exchange.peerTimeout`. |
| A routed member that turned `Unknown` | Kept for `unknownHold` after it was last confirmed, then nowhere. It does not delay a promotion elsewhere: once another member is the one writable member, it is routed at once ([RFC0002](../rfcs/RFC0002.md#decision), rows 5 and 7). | `unknownHold` 30s | `spec.policy.unknownHold`, 0s to 10m. Shorter values stop routing to a dead member sooner. Longer ones ride out longer link flaps. |
| The workload's move | A CloudNativePG `Cluster` reads `Transitioning` while it switches over, promotes, demotes, or restarts its primary. Strait routes nowhere meanwhile ([RFC0003](../rfcs/RFC0003.md#cnpg-role-and-state)). | Depends on the workload | See [The promotion restart](#the-promotion-restart). |
| Declarations agreeing | In a switchover, the sites declare the new primary one after another. Until every known site does, the route is nowhere with `DeclarationsDisagree` ([RFC0002](../rfcs/RFC0002.md#why-single-declarations-matter)). | Up to `peerTimeout` after the last site changes | Change every site's declaration at about the same time. |
| A returning site's controller | With `startupHold`, a site that starts does not route to its own member until it has read every other site, or until the hold ends. | Off | `spec.policy.startupHold`. |
| A cut-off site | With `isolationTimeout`, a site that has read no peer for that long stops routing to its own member. This limits lost writes at the old primary, and does not add to the new primary's time. | Off | `spec.policy.isolationTimeout`. Keep it below the time the other sites take to promote ([RFC0002](../rfcs/RFC0002.md#the-isolated-site-policy)). |
| Applying the route | The controller deletes old EndpointSlices, then writes the new ones, in one decision ([RFC0002](../rfcs/RFC0002.md#applying-a-decision)). The CNI then applies them. | One API round trip per write, plus the CNI | Nothing in Strait. |
| A remote member's path | Through the gateway, the import must have a healthy link. A dead link fails over within 5s ([RFC0005](../rfcs/RFC0005.md), requirement 4). | Checks every 1s, 2 failures | Nothing. |
| A gateway handover | A standby takes the gateway Lease within `duration` + 4.4 × `retryPeriod` of the holder's last renewal ([RFC0005](../rfcs/RFC0005.md), requirement 50). | 6s + 4.4s | `Site.spec.gateway.lease`. |
| Sessions to the old member | Kept, unless `policy.connections` is `EndGateway`: then sessions through this site's gateway end after Envoy's drain time ([RFC0002](../rfcs/RFC0002.md#what-endgateway-covers-and-costs)). | Kept | `spec.policy.connections`, and the chart value `envoy.drainTimeSeconds`. |

## The promotion restart

When CloudNativePG promotes a replica cluster, it restarts the new primary. The restart begins with a smart shutdown, which waits for open sessions to end, for up to `smartShutdownTimeout`. That is 180s by default. The `Cluster` reads `Transitioning` meanwhile, so Strait routes nowhere for as long as it lasts ([RFC0003](../rfcs/RFC0003.md#limitations)).

Set a short `smartShutdownTimeout` on every `Cluster` of the topology, for example 10s to 30s. A session still open then is cut when the timeout ends, and the restart goes on.

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
spec:
  smartShutdownTimeout: 15
```

## A planned switchover

From `pg-a` at site a to `pg-b` at site b:

| Step | Route | Strait's share |
|---|---|---|
| Site a is demoted, then declares `pg-b` | Nowhere at a at once. At b within `peerTimeout`. | Up to `peerTimeout` until b sees it. |
| Site b is promoted | Nowhere while `pg-b` is `Transitioning`. | None. This is the promotion and its restart. |
| `pg-b` is writable | `pg-b` at b within one decision. At a within `peerTimeout`. | Under 2s at b. Up to `peerTimeout` at a, plus the gateway import's link check. |

## A site loss

Site a, which held the primary, is lost:

| Step | Route at site b | Strait's share |
|---|---|---|
| Site a stops answering | `pg-a` is kept, `HeldUnknown`. | Up to `peerTimeout`, then `unknownHold` after `pg-a` was last confirmed. Writes to `pg-a` fail anyway. |
| The operator promotes `pg-b` | Nowhere while `pg-b` is `Transitioning`. | None. Detecting the loss and deciding to promote are the operator's or the workload's. |
| `pg-b` is writable | `pg-b`, `Active`, within one decision. `pg-a`'s hold does not delay it. | Under 2s. With `isolationTimeout` and no other peer, the route stays nowhere until the operator sets it to 0s ([Site loss](runbooks/site-loss.md)). |

Steps for the operator: [Site loss](runbooks/site-loss.md).
