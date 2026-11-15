# Upgrade

Upgrade one site at a time. Sites on different 1.x releases still read each other's observations. To go back to an earlier release: [Roll back within 1.x](./rollback.md).

## Steps

| Step | What to do |
|---|---|
| 1. Pull the images | Required when a pull can take longer than the site's budget, in [How to plan](#how-to-plan). List the release's images with `helm template`, as below. Pull each one on every node of the site, for example with `crictl pull`. From 1.3.0, the chart's pre-pull Job also runs the release's image on the controller's node before the upgrade changes anything (`controller.prePull`), but the new controller pod can start on another node. |
| 2. Upgrade the site | Read [Notes by version](#notes-by-version) for every release after the site's. Apply the release's CRDs first, then upgrade the chart with the same values, as below. Helm installs the chart's CRDs on the first install only, and never upgrades or deletes them. |
| 3. Wait | Run `strait --context SITE status -A` at every site. Go on once each shows every peer `REACHABLE`, and every name with the route and reason it had before the upgrade, usually `Active`. |
| 4. Next site | Back to step 1. |

```sh
v=v1.5.0
helm template strait oci://ghcr.io/takodotid/charts/strait --version "${v#v}" --values values-a.yaml | grep 'image:' | sort -u
curl -fsSLO "https://github.com/takodotid/strait/releases/download/$v/strait-crds.yaml"
kubectl --context site-a apply --server-side --force-conflicts -f strait-crds.yaml
helm --kube-context site-a upgrade strait oci://ghcr.io/takodotid/charts/strait --version "${v#v}" \
  --namespace strait-system --values values-a.yaml --wait
```

When the release's image can't be pulled or run, the pre-pull Job fails, and `helm upgrade` fails with `pre-upgrade hooks failed` on `Job/strait-system/strait-pre-pull`. Nothing else has changed, and the old controller still runs. Read why with `kubectl --context site-a -n strait-system describe pod -l app.kubernetes.io/component=pre-pull`. The failed Job stays until the next upgrade replaces it.

## Notes by version

Each row applies when the site runs a release before that version.

| From a release before | What changes | What to do |
|---|---|---|
| 1.2.0 | The new CRDs add `SiteLink.spec.direct.endpointCIDRs`, which the API server doesn't know until the new CRDs are applied. | Apply the CRDs before the chart. |
| 1.3.0 | The new CRDs add `Site.spec.gateway.lease`. | Apply the CRDs before the chart. |
| 1.4.0 | The new CRDs refuse a `Site` whose gateway ports include 8090, 8091 or 9901, where the gateway agent and Envoy's admin interface listen. On Kubernetes 1.31 and 1.32, under the new CRDs, every write to the status of a `Site` stored with such a port is refused. The controller then stops updating that status, including the active gateway's address. This was seen on envtest's API server, 1.31.0 to 1.37.0, and from 1.33 the status write passes ([RFC0005](../../rfcs/RFC0005.md#requirements), requirement 52). | Change such a port first. |
| 1.5.0 | With `networkPolicy.gateway.enabled`, the gateway's policy lets the cluster's pods reach the import and export ports only, and lets the controller reach the peer port. A pod that connects to another gateway port, such as the control port, is refused. The CIDRs in `networkPolicy.gateway.peerCIDRs` can reach only these: the control and export ports, and the UDP ports in the new value `networkPolicy.gateway.wireGuardPorts` ([RFC0007](../../rfcs/RFC0007.md#network-policies), requirement 28). | List the site's WireGuard listen ports in `networkPolicy.gateway.wireGuardPorts`, or `[]` when the site has no `wireguard` link. With `site.create: false`, set `networkPolicy.gateway.ports` too. Without them, `helm upgrade` fails. |
| 1.5.0 | The image's index lists the platform images only, and each platform image carries its provenance and SBOM as cosign attestations. The verify commands of 1.4.0 still pass: `cosign verify` of the chart and the image, and `gh attestation verify` of an archive. `docker buildx imagetools inspect --format '{{json .Provenance}}'` and `'{{json .SBOM}}'` read attestations from the index, so they print `{}`. | Read the attestations with `cosign verify-attestation` ([Verify a release](../guide/install.md#verify-a-release)). |
| 1.5.0 | The gateway agent uses nftables to filter the traffic that arrives on its WireGuard interfaces. On a node whose kernel lacks `nf_tables`, once the gateway rolls, each `wireguard` link has no interface and shows `NotConfigured` ([RFC0005](../../rfcs/RFC0005.md#wireguard)). | Check the nodes that can run a gateway pod first. |

## What the site shows

| Part | What happens | What the operator sees |
|---|---|---|
| Controller | One replica with the `Recreate` strategy ([RFC0004](../../rfcs/RFC0004.md#requirements), requirement 12). The site has no controller from the moment its old pod stops until its new pod serves. That gap includes the image pull, the start, and the wait for the leader election Lease. There is no wait when the old controller released the Lease as it stopped, and a 15s wait when it didn't ([RFC0004](../../rfcs/RFC0004.md#leader-election)). A controller of 1.1.3 or later releases it. | At the other sites, once the gap passes `peerTimeout` (6s by default), the site's peer isn't `REACHABLE`, and its members are `Unknown`. A routed `Unknown` member stays in the route, with reason `HeldUnknown`, for `policy.unknownHold` (30s by default) after it was last confirmed. Then the name routes nowhere with `UnknownTimeout` until the new controller serves ([RFC0002](../../rfcs/RFC0002.md#requirements), requirement 17). At the site itself, its names keep the endpoints written last, and their status doesn't change. Then their status shows `AwaitingPeers` until the new controller's first poll of every peer has ended. |
| Gateway | Every release rolls the gateway, because its agent runs the release's image. With the chart's 2 replicas, the Deployment starts one new pod, and stops an old pod once the new one is ready, until no old pod is left. The active pod has a deletion cost, so it stops last ([RFC0005, Gateway rollout](../../rfcs/RFC0005.md#gateway-rollout)). When it stops, its agent removes its WireGuard interfaces and releases the gateway Lease, and a new replica takes it ([RFC0005, Failover sequence](../../rfcs/RFC0005.md#failover-sequence)). The Lease moves once. From a release before 1.3.0, it can move twice, because the active pod has no deletion cost until the new controller sets it. | Each move resets every connection through the site's gateway, in both directions: this site's clients of remote members, and the other sites' clients of this site's members. Connections to a local member, and endpoint-direct connections, don't cross the gateway. New connections through the gateway fail until the new active pod's links pass their health checks. The polls between the site and its peers cross the gateway too. When a move lasts longer than `peerTimeout`, each side counts the other's members as `Unknown`, as in the controller row. `strait status` shows the new active gateway pod. |

## How to plan

| Plan | Why |
|---|---|
| Keep the controller gap under the smallest `policy.unknownHold` of the names with a member at the site. | Past that, those names route nowhere at the other sites until the new controller serves. |
| Pull the images first (step 1) when a pull can take longer than that budget, minus the start and the Lease wait. | The pull is part of the gap. |
| Upgrade while the clients of names that cross the gateway can reconnect. | Each move of the gateway Lease resets their connections. |
| Move no primary at any site while a site upgrades. | During the gap, the site's names keep the endpoints written last, and the other sites can't read what the site declares. |
| One site at a time. | Only that site's members are at risk, and step 3 shows the upgrade worked before the next site. Every site at once puts every site's members at risk together. |

## Node drains

A drain moves Strait's pods off a node. The chart's PodDisruptionBudget lets a drain evict one gateway pod at a time. A gateway pod that isn't ready can be evicted at any time (`gateway.podDisruptionBudget`). The controller has no budget, because it runs one replica, and a budget would block every drain of its node.

| The drained node runs | Effect |
|---|---|
| The controller | The controller gap, as in an upgrade. The new pod pulls its image on its new node, unless step 1 of the upgrade pulled it there. |
| The active gateway pod | One move of the gateway Lease, as in an upgrade. |
| A standby gateway pod | None. |

With `gateway.antiAffinity: required`, each gateway replica runs on a node of its own, so one drain never takes two. A rollout then needs one node more than `gateway.replicas`, for the new pod it starts first. With the default, `preferred`, two replicas can share a node when no other node fits.

Drain the nodes of one site at a time, and plan each drain as an upgrade. After a node that ran the controller or the active gateway pod, wait as in step 3 before the next node. `kubectl --context site-a -n strait-system get pods -o wide` shows each pod's node, and `strait --context site-a status` shows the active gateway pod.

## From 0.1 to 1.0

1.0 serves the API as `strait.tako.id/v1`. It still serves `strait.tako.id/v1alpha1`, deprecated, with the same schema ([RFC0001](../../rfcs/RFC0001.md#api-versions)).

| Step | What to do |
|---|---|
| 1. Upgrade each site | As above. Objects that 0.1 stored keep working and keep their identity. |
| 2. Move your manifests | Change `apiVersion: strait.tako.id/v1alpha1` to `strait.tako.id/v1`. Nothing else changes. `strait validate` warns about each object that still names `v1alpha1`, and `kubectl` prints a deprecation warning. |
| 3. Store every object as `v1` | Needed before a release that stops serving `v1alpha1`. Run the commands below at each site. |

```sh
ctx=site-a
for r in sites sitepeers sitelinks exports imports siteservices; do
  kubectl --context "$ctx" get "$r.strait.tako.id" --all-namespaces -o json | kubectl --context "$ctx" replace -f -
done
for r in sites sitepeers sitelinks exports imports siteservices; do
  kubectl --context "$ctx" patch crd "$r.strait.tako.id" --subresource=status --type=merge \
    -p '{"status":{"storedVersions":["v1"]}}'
done
```

`replace` writes each object again, in the version that the API server stores, which is `v1`. If it reports a conflict, the controller changed that object in the meantime. Run the first loop again. Run the second loop only once the first has replaced every object.

The 1.0 CLI reads `v1`, so it needs a site whose CRDs are 1.0. With the 1.0 CRDs in place, `helm rollback` to 0.1 still works, since they serve `v1alpha1`.
