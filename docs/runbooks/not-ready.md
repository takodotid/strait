# A controller or a gateway that is not ready

Each command reads one site. The examples use site a and the namespace `strait-system`.

```sh
kubectl --context site-a -n strait-system get pods -o wide
kubectl --context site-a -n strait-system get events --sort-by=.lastTimestamp
```

## Controller

The controller pod is ready only while it holds the leader election Lease `strait-controller` ([RFC0004, Leader election](../../rfcs/RFC0004.md#leader-election)). While no controller is ready, the other sites cannot read this site: see [Upgrade](../../README.md#upgrade) for what they show.

| Symptom | Cause | Next step |
|---|---|---|
| `Pending` | No node fits the pod. | `kubectl --context site-a -n strait-system describe pod -l app.kubernetes.io/component=controller`. The pod's requests are `controller.resources` in the chart's values. |
| `ErrImagePull` or `ImagePullBackOff` | The node cannot pull the image. | The image in `describe pod`, and the node's access to the registry. |
| Running, not ready, for up to 15s after a start | The previous controller did not release the Lease, as after a crash. The new one takes it 15s after it first reads it. | Wait. The Lease's `spec.holderIdentity` starts with the holder's pod name. |
| Running, not ready, for longer | Another process holds the Lease and renews it. | `kubectl --context site-a -n strait-system get lease strait-controller -o jsonpath='{.spec.holderIdentity}'`, then find that pod. |
| `CrashLoopBackOff`, and the log has `forbidden` for `clusters` in the API group `postgresql.cnpg.io`, then `failed to wait for siteservice caches to sync` | The chart value `cnpg.enabled` is false on a cluster that has CloudNativePG's CRDs. | Set `cnpg.enabled: true` and run `helm upgrade` with the same values: [Getting started, Prerequisites](../getting-started.md#prerequisites). |
| Ready, but no route changes, and the log has `want exactly one Site, found 0` | The `Site` is gone, for example after `helm upgrade` with `site.create: false`. | Restore it: [Roll back, the `Site`](rollback.md#the-site). |

The controller's log: `kubectl --context site-a -n strait-system logs deployment/strait-controller`. Add `--previous` for the run before a restart.

## Gateway

A gateway pod is ready while its Envoy's admin `/ready` answers 200. Only a ready replica holds the gateway Lease: the agent joins the election once Envoy is ready, and steps down after 6s of Envoy not ready ([RFC0005, Gateway Lease](../../rfcs/RFC0005.md#gateway-lease)).

| Symptom | Cause | Next step |
|---|---|---|
| No gateway pod, and the ReplicaSet's events say the pod violates PodSecurity | The namespace's Pod Security level is not `privileged`. | [Getting started, step 1](../getting-started.md#1-namespace-and-wireguard-key). |
| The `envoy` container is not ready, and no controller is ready | Envoy takes its configuration from the elected controller over xDS. | The controller first, above. |
| The `envoy` container is not ready, and its log has errors for `strait-controller.strait-system.svc:18000` | Envoy cannot reach the controller's xDS port. | The chart's NetworkPolicy admits the gateway pods to that port. Check that no other policy denies it. |
| The `envoy` container restarts with `OOMKilled` | Envoy's memory limit is too low for the site's traffic. | Raise `gateway.resources.envoy.limits.memory`, then `helm upgrade`. |
| The `envoy` container turns not ready under load | Envoy starves for CPU on its node. | Raise `gateway.resources.envoy.requests.cpu` ([benchmark](../benchmark.md#under-cpu-contention)). |
| Pods are ready, but `strait status` shows no active gateway | No ready pod holds the gateway Lease yet. | The agent's log: `kubectl --context site-a -n strait-system logs POD -c agent`. It logs `Envoy is not ready; not joining the gateway election`, and `leading` once it holds the Lease. |

The `Site`'s gateway conditions: [Troubleshooting, the conditions of the `Site`](../troubleshooting.md#unreachable-peers-and-unhealthy-links).
