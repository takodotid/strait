# A controller or a gateway that is not ready

Find the pod state or the log token below, then read its cause and next step. Each command reads one site. The examples use site a and the namespace `strait-system`.

```sh
kubectl --context site-a -n strait-system get pods -o wide
kubectl --context site-a -n strait-system get events --sort-by=.lastTimestamp
```

## Controller

The controller pod is ready only while it holds the leader election Lease `strait-controller` ([RFC0004, Leader election](../../rfcs/RFC0004.md#leader-election)). While no controller is ready, the other sites cannot read this site: see [Upgrade](./upgrade.md) for what they show.

The controller's log:

```sh
kubectl --context site-a -n strait-system logs deployment/strait-controller
```

Add `--previous` for the run before a restart.

The controller pod:

```sh
kubectl --context site-a -n strait-system describe pod -l app.kubernetes.io/component=controller
```

### `Pending`

Cause: no node fits the pod.

Next step: describe the pod, as above. The pod's requests are `controller.resources` in the chart's values.

### `ErrImagePull` or `ImagePullBackOff`

Cause: the node cannot pull the image.

Next step: read the image in `describe pod`, as above, and check the node's access to the registry.

### Running, not ready, for up to 15s after a start

Cause: the previous controller did not release the Lease, for example after a crash, or after it could not renew the Lease. The new one takes it 15s after it first reads it.

Next step: wait. The Lease's `spec.holderIdentity` starts with the holder's pod name.

### Running, not ready, for longer

Cause: another process holds the Lease and renews it.

Next step: read the holder, then find that pod.

```sh
kubectl --context site-a -n strait-system get lease strait-controller -o jsonpath='{.spec.holderIdentity}'
```

### `OOMKilled`

The pod restarts, and `describe pod` shows `Last State: Terminated` with `Reason: OOMKilled`.

Cause: the controller outgrew `controller.resources.limits.memory`. Its cache holds every Service and EndpointSlice of the cluster, unless `controller.namespacedReads` is set.

Next step: raise the limit, or set `controller.namespacedReads`: [Sizing](../sizing.md). `describe pod`, as above, shows the last state.

### `Liveness probe failed`

The pod restarts, and the events have `Liveness probe failed`.

Cause: the elected controller's poll round or xDS rebuild loop stopped for longer than twice its period plus 30s, 34s at the default `pollInterval` ([RFC0004](../../rfcs/RFC0004.md#requirements), requirement 18).

Next step: read the log of the run before the restart (`--previous`), and `strait_peer_last_seen_timestamp_seconds` ([Metrics](../metrics.md)).

### `CrashLoopBackOff` for another reason

Cause: an error that stops the controller is logged as `controller stopped`, with the error.

Next step: read the last lines of the log of the run before the restart (`--previous`).

### `want exactly one Site, found 0`

The controller is ready, but no route changes, and the log has this line.

Cause: the `Site` is gone, for example after `helm upgrade` with `site.create: false`.

Next step: restore it: [Roll back, the `Site`](rollback.md#the-site).

## Gateway

A gateway pod is ready while its Envoy's admin endpoint `/ready` answers 200. Only a ready replica holds the gateway Lease ([RFC0005, Gateway Lease](../../rfcs/RFC0005.md#gateway-lease)).

### No gateway pod

The ReplicaSet's events say the pod violates PodSecurity.

Cause: the namespace's Pod Security level is not `privileged`.

Next step: [Getting started, step 1](../getting-started.md#1-namespace-and-wireguard-key).

### `envoy` not ready, and no controller ready

Cause: Envoy takes its configuration from the elected controller over xDS.

Next step: fix the controller first, above.

### `envoy` not ready, with errors for `strait-controller.strait-system.svc:18000`

The `envoy` container's log has these errors.

Cause: Envoy cannot reach the controller's xDS port.

Next step: the chart's NetworkPolicy admits the gateway pods to that port. Check that no other policy denies it.

### `envoy` restarts with `OOMKilled`

Cause: Envoy's memory limit is too low for the site's traffic.

Next step: raise `gateway.resources.envoy.limits.memory`, then `helm upgrade`.

### `envoy` turns not ready under load

Cause: Envoy starves for CPU on its node.

Next step: raise `gateway.resources.envoy.requests.cpu` ([benchmark](../benchmark.md#under-cpu-contention)).

### Pods ready, but `strait status` shows no active gateway

Cause: no ready pod holds the gateway Lease yet.

Next step: read the agent's log. It logs `Envoy is not ready; not joining the gateway election`, and `leading` once it holds the Lease. Replace `POD` with the name of a gateway pod from the `get pods` output at the top of this page. The `Site`'s gateway conditions: [Troubleshooting, the conditions of the `Site`](../troubleshooting.md#the-conditions-of-the-site).

```sh
kubectl --context site-a -n strait-system logs POD -c agent
```
