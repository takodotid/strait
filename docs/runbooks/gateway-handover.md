# Gateway handover

One gateway replica is active: it holds the gateway Lease `strait-gateway`. A handover moves the Lease to another replica. Each handover resets every connection through the site's gateway, in both directions. New connections through the gateway fail until the new replica's links pass their health checks. The sequence and its timing: [RFC0005, Failover sequence](../../rfcs/RFC0005.md#failover-sequence).

| Cause | Moves |
|---|---|
| A gateway rollout: an upgrade to another release, or `kubectl rollout restart` | Once ([Upgrade](./upgrade.md)). |
| A drain of the active pod's node | Once ([Node drains](./upgrade.md#node-drains)). |
| The active pod is deleted, or its node fails | Once. |
| The active pod's Envoy is not ready for 6s | Once. With one replica, the same replica takes the Lease again once its Envoy is ready ([Not ready](not-ready.md#gateway)). |

## After a handover

| Check | Command |
|---|---|
| The new active pod | `strait --context site-a status`: the `GATEWAY` column. |
| Each move, with the pods before and after | `kubectl --context site-a get events -A --field-selector reason=GatewayHandover`. A Warning event marks a moment when no pod held the Lease. |
| The links of the new active pod | `strait --context site-a links`, and at each peer `strait --context site-b links`: a link to the other site `HEALTHY` and `ACTIVE`. |
| The peers | `strait --context site-a peers` and `strait --context site-b peers`: `REACHABLE`. |

The metric `strait_gateway_lease_transitions_total` counts the moves, and the alert `StraitGatewayHandovers` fires on more than 3 in an hour ([Metrics](../metrics.md)).

## A planned handover

To move the active gateway off a node without a drain, for example before work on that node:

| Step | What to do | Check |
|---|---|---|
| 1. A ready standby | The site runs at least 2 gateway replicas. With one, the site has no gateway until the replacement pod is ready. | `kubectl --context site-a -n strait-system get pods -l app.kubernetes.io/component=gateway -o wide`: 2 or more ready. |
| 2. Keep the replacement off the node | `kubectl --context site-a cordon NODE`. Otherwise the replacement pod may start on the same node. | |
| 3. Delete the active pod | Its agent removes its WireGuard interfaces and releases the Lease as it stops. A standby takes the Lease within 2.2 × `lease.retryPeriod`, 2.2s by default. | [After a handover](#after-a-handover). |
| 4. After the work | `kubectl --context site-a uncordon NODE`. | |

Step 3, with the active pod from `strait --context site-a status`:

```sh
kubectl --context site-a -n strait-system delete pod POD --dry-run=server
kubectl --context site-a -n strait-system delete pod POD
```

The end-to-end step `gateway-handover` deletes the active pod in the same way, with the direct link cut. It requires a write from the other site to land within 8s of the takeover: [`test/e2e/README.md`](../../test/e2e/README.md#scenarios).
