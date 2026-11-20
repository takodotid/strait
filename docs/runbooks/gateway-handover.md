# Gateway handover

One gateway replica is active: it holds the gateway Lease `strait-gateway`. A handover moves the Lease to another replica. The sequence and its timing: [RFC0005, Failover sequence](../../rfcs/RFC0005.md#failover-sequence).

Each handover resets every connection through the site's gateway, in both directions. If the gateway moved without a plan, go to [After a handover](#after-a-handover) and [Causes of a handover](#causes-of-a-handover).

## A planned handover

To move the active gateway off a node without a drain, for example before work on that node.

::: warning Each handover resets every connection
Each handover resets every connection through the site's gateway, in both directions. New connections through the gateway fail until the new replica's links pass their health checks. The site must run at least 2 gateway replicas: with one, the site has no gateway until the replacement pod is ready.
:::

### 1. Check for a ready standby

List the gateway pods.

```sh
kubectl --context site-a -n strait-system get pods -l app.kubernetes.io/component=gateway -o wide
```

Check: 2 or more are ready.

### 2. Keep the replacement off the node

Cordon the node. Otherwise the replacement pod may start on the same node. Replace `NODE` with the node's name.

```sh
kubectl --context site-a cordon NODE
```

### 3. Delete the active pod

Its agent removes its WireGuard interfaces and releases the Lease as it stops. A standby takes the Lease within 2.2 × `lease.retryPeriod`, 2.2s by default. Replace `POD` with the active pod from `strait --context site-a status`.

Dry run:

```sh
kubectl --context site-a -n strait-system delete pod POD --dry-run=server
```

Then delete the pod:

```sh
kubectl --context site-a -n strait-system delete pod POD
```

Check: [After a handover](#after-a-handover).

### 4. Uncordon after the work

When the work on the node is done, uncordon it. Replace `NODE` as in step 2.

```sh
kubectl --context site-a uncordon NODE
```

## The end-to-end step

The end-to-end step `gateway-handover` deletes the active pod in the same way as step 3, with the direct link cut. It requires a write from the other site to land within 8s of the takeover: [`test/e2e/README.md`](../../test/e2e/README.md#scenarios).

## After a handover

Check the new active pod in the `GATEWAY` column:

```sh
strait --context site-a status
```

Check each move, with the pods before and after. A Warning event marks a moment when no pod held the Lease.

```sh
kubectl --context site-a get events -A --field-selector reason=GatewayHandover
```

Check the links of the new active pod, and at each peer. Expect a link to the other site `HEALTHY` and `ACTIVE`.

```sh
strait --context site-a links
strait --context site-b links
```

Check the peers. Expect `REACHABLE`.

```sh
strait --context site-a peers
strait --context site-b peers
```

The metric `strait_gateway_lease_transitions_total` counts the moves, and the alert `StraitGatewayHandovers` fires on more than 3 in an hour ([Metrics](../metrics.md)).

## Causes of a handover

| Cause | Moves |
|---|---|
| A gateway rollout: an upgrade to another release, or `kubectl rollout restart` | Once ([Upgrade](./upgrade.md)). |
| A drain of the active pod's node | Once ([Node drains](./upgrade.md#node-drains)). |
| The active pod is deleted, or its node fails | Once. |
| The active pod's Envoy is not ready for 6s | Once. With one replica, the same replica takes the Lease again once its Envoy is ready ([Not ready](not-ready.md#gateway)). |
