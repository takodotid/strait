# Upgrade

Upgrade one site at a time. Sites on different 1.x releases read each other's observations.

## Before the upgrade

- Work out the pull budget: [How to plan](#how-to-plan). Step 1 needs it.
- Read [Notes by version](#notes-by-version) for every release after the site's.

## Steps

### 1. Pull the images

Required when a pull can take longer than the site's budget, in [How to plan](#how-to-plan). List the release's images with `helm template`. Pull each one on every node of the site, for example with `crictl pull`.

Change `v` and `values-a.yaml` to the release and the site's values file.

```sh
v=v1.5.0
helm template strait oci://ghcr.io/takodotid/charts/strait --version "${v#v}" --values values-a.yaml | grep 'image:' | sort -u
```

`controller.prePull` pulls only on the controller's node.

### 2. Upgrade the site

Apply the release's CRDs first, then upgrade the chart with the same values. Helm installs the chart's CRDs on the first install only, and never upgrades or deletes them.

Change `v`, `site-a` and `values-a.yaml` as in step 1.

```sh
v=v1.5.0
curl -fsSLO "https://github.com/takodotid/strait/releases/download/$v/strait-crds.yaml"
kubectl --context site-a apply --server-side --force-conflicts -f strait-crds.yaml
```

Check: the `curl` and `kubectl apply` commands both succeed. If either fails, stop: do not upgrade the chart.

Then upgrade the chart. Run it in the same shell, so that `v` is still set.

```sh
helm --kube-context site-a upgrade strait oci://ghcr.io/takodotid/charts/strait --version "${v#v}" \
  --namespace strait-system --values values-a.yaml --wait
```

If `helm upgrade` fails with `pre-upgrade hooks failed`: [If the pre-pull Job fails](#if-the-pre-pull-job-fails).

### 3. Wait

Run this at every site. Change `site-a` to each site's context.

```sh
strait --context site-a status -A
strait --context site-a peers
strait --context site-a links
```

Check: go on once each shows:

- every peer `REACHABLE`.
- every link `HEALTHY` and `ACTIVE`.
- every name with the route and reason it had before the upgrade, usually `Active`.

If a check fails, or to go back to an earlier release: [Roll back within 1.x](./rollback.md).

### 4. Upgrade the next site

Back to step 1.

## If the pre-pull Job fails

When the release's image can't be pulled or run, the pre-pull Job fails, and `helm upgrade` fails with `pre-upgrade hooks failed` on `Job/strait-system/strait-pre-pull`. Nothing else has changed, and the old controller still runs. The failed Job stays until the next upgrade replaces it.

Read why:

```sh
kubectl --context site-a -n strait-system describe pod -l app.kubernetes.io/component=pre-pull
```

## Notes by version

Each note applies when the site runs a release before that version.

### 1.2.0

- What to do: Apply the CRDs before the chart.
- What changes: The new CRDs add `SiteLink.spec.direct.endpointCIDRs`, which the API server doesn't know until the new CRDs are applied.

### 1.3.0

- What to do: Apply the CRDs before the chart.
- What changes: The new CRDs add `Site.spec.gateway.lease`.
- If the controller loops in `CrashLoopBackOff` with `forbidden` for `clusters` of `postgresql.cnpg.io` in its log: a controller before 1.3.0 with `cnpg.enabled: false`, on a cluster with CloudNativePG's CRDs. Upgrade to 1.3.0 or later, or set `cnpg.enabled: true` ([Getting started, The cnpg plugin switch](../getting-started.md#the-cnpg-plugin-switch)).

### 1.4.0

- What to do: Change a gateway port of 8090, 8091 or 9901 first.
- What changes:
  - The new CRDs refuse a `Site` whose gateway ports include 8090, 8091 or 9901, where the gateway agent and Envoy's admin interface listen.
  - On Kubernetes 1.31 and 1.32, under the new CRDs, every write to the status of a `Site` stored with such a port is refused. The controller then stops updating that status, including the active gateway's address.

### 1.5.0

- Network policy:
  - What to do: List the site's WireGuard listen ports in `networkPolicy.gateway.wireGuardPorts`, or `[]` when the site has no `wireguard` link. With `site.create: false`, set `networkPolicy.gateway.ports` too. With `networkPolicy.gateway.enabled`, `helm upgrade` fails without `wireGuardPorts` when `peerCIDRs` is set, and without `networkPolicy.gateway.ports` when `site.create` is false.
  - What changes: With `networkPolicy.gateway.enabled`, the gateway's policy lets the cluster's pods reach the import and export ports only, and lets the controller reach the peer port. A pod that connects to another gateway port, such as the control port, is refused. The CIDRs in `networkPolicy.gateway.peerCIDRs` can reach only these: the control and export ports, and the UDP ports in the new value `networkPolicy.gateway.wireGuardPorts` ([RFC0007](../../rfcs/RFC0007.md#network-policies), requirement 28).
- Attestations:
  - What to do: Read the attestations with `cosign verify-attestation` ([Verify a release](../guide/install.md#verify-a-release)).
  - What changes: The image's index lists the platform images only, and each platform image carries its provenance and SBOM as cosign attestations. The verify commands of 1.4.0 still pass. `docker buildx imagetools inspect` reads attestations from the index and prints `{}`.
- nftables:
  - What to do: Check the nodes that can run a gateway pod first.
  - What changes: The gateway agent uses nftables to filter the traffic that arrives on its WireGuard interfaces. On a node whose kernel lacks `nf_tables`, once the gateway rolls, each `wireguard` link has no interface and shows `NotConfigured` ([RFC0005](../../rfcs/RFC0005.md#wireguard)).

## What the site shows

| Event | What the operator sees |
|---|---|
| Controller gap: one replica, `Recreate` strategy ([RFC0004](../../rfcs/RFC0004.md#requirements), requirement 12). The site has no controller from the old pod's stop until the new pod serves: image pull, start and the Lease wait (none if the old controller released the Lease, which 1.1.3 and later do, else 15s; [RFC0004](../../rfcs/RFC0004.md#leader-election)). | At the other sites, once the gap passes `peerTimeout`, the site's peer isn't `REACHABLE` and its members are `Unknown`. Routed members then follow `policy.unknownHold`: `HeldUnknown`, then `UnknownTimeout` ([RFC0002](../../rfcs/RFC0002.md#requirements), requirement 17). At the site itself, names keep the endpoints written last, then show `AwaitingPeers` until the new controller's first poll of every peer has ended. |
| Gateway Lease move: every release rolls the gateway, one new pod at a time, and the active pod stops last ([RFC0005, Gateway rollout](../../rfcs/RFC0005.md#gateway-rollout)). The Lease moves once, or twice from a release before 1.3.0. | Every connection through the gateway resets: [Gateway handover](./gateway-handover.md). Polls between the site and its peers cross the gateway, so a move longer than `peerTimeout` leaves members `Unknown`, as for the controller. `strait status` shows the new active gateway pod. |

## How to plan

| Plan | Why |
|---|---|
| Keep the controller gap under the smallest `policy.unknownHold` of the names with a member at the site. | Past that, those names route nowhere at the other sites until the new controller serves. |
| Pull the images first (step 1) when a pull can take longer than that budget, minus the start and the Lease wait. | The pull is part of the gap. |
| Upgrade while the clients of names that cross the gateway can reconnect. | Each move of the gateway Lease resets their connections. |
| Move no primary at any site while a site upgrades. | During the gap, the site's names keep the endpoints written last, and the other sites can't read what the site declares. |
| One site at a time. | Step 3 shows the upgrade worked before the next site. |

## Node drains

A drain moves Strait's pods off a node. The chart's PodDisruptionBudget lets a drain evict one gateway pod at a time. A gateway pod that isn't ready can be evicted at any time (`gateway.podDisruptionBudget`).

| The drained node runs | Effect |
|---|---|
| The controller | The controller gap, as in an upgrade. The new pod pulls its image on its new node, unless step 1 of the upgrade pulled it there. |
| The active gateway pod | One move of the gateway Lease, as in an upgrade. |
| A standby gateway pod | None. |

With `gateway.antiAffinity: required`, each gateway replica runs on a node of its own, so one drain never takes two. A rollout then needs one node more than `gateway.replicas`, for the new pod it starts first. With the default, `preferred`, two replicas can share a node when no other node fits.

Drain the nodes of one site at a time, and plan each drain as an upgrade. After a node that ran the controller or the active gateway pod, wait as in [step 3](#3-wait) before the next node. These commands show each pod's node, and the active gateway pod:

```sh
kubectl --context site-a -n strait-system get pods -o wide
strait --context site-a status
```

## From 0.1 to 1.0

1.0 serves the API as `strait.tako.id/v1`. It still serves `strait.tako.id/v1alpha1`, deprecated, with the same schema ([RFC0001](../../rfcs/RFC0001.md#api-versions)).

### 1. Upgrade each site

As above. Objects that 0.1 stored keep working and keep their identity.

### 2. Move the manifests

Change `apiVersion: strait.tako.id/v1alpha1` to `strait.tako.id/v1`. Nothing else changes. `strait validate` warns about each object that still names `v1alpha1`, and `kubectl` prints a deprecation warning.

### 3. Store every object as `v1`

Needed before a release that stops serving `v1alpha1`. Run the two loops below at each site, one after the other.

`replace` writes each object again, in the version that the API server stores, which is `v1`. If it reports a conflict, the controller changed that object in the meantime. Run this loop again. Set `ctx` to the site's context, in both blocks, and run both for the same site.

```sh
ctx=site-a
for r in sites sitepeers sitelinks exports imports siteservices; do
  kubectl --context "$ctx" get "$r.strait.tako.id" --all-namespaces -o json \
    | kubectl --context "$ctx" replace -f -
done
```

Run the second loop only once the first has replaced every object.

```sh
ctx=site-a
for r in sites sitepeers sitelinks exports imports siteservices; do
  kubectl --context "$ctx" patch crd "$r.strait.tako.id" --subresource=status --type=merge \
    -p '{"status":{"storedVersions":["v1"]}}'
done
```

The 1.0 CLI reads `v1`, so it needs a site whose CRDs are 1.0.
