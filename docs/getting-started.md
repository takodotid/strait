# Getting started

This guide installs Strait on two clusters, sites `a` and `b`, joins them with a WireGuard link, and points the name `pg.app.svc` at the CloudNativePG primary, wherever it runs.

## Prerequisites

| Requirement | Detail |
|---|---|
| Two Kubernetes clusters | One per site, of a version that [Install](./guide/install.md) supports, reached with the kubeconfig contexts `site-a` and `site-b`. |
| Kernel modules `wireguard` and `nf_tables` | On every node that can run a gateway pod. |
| Pod Security level `privileged` | In the gateway's namespace. The gateway agent runs as root with `NET_ADMIN`, to hold the WireGuard interfaces ([RFC0007](../rfcs/RFC0007.md)). |
| A UDP path from site b to site a | Site b dials a node of site a on UDP port 32100. Here, `203.0.113.10` stands for that node's address. |
| Socket load balancing for pods | Recommended, where the CNI load-balances Services. For Cilium: socket LB on, with `socketLB.hostNamespaceOnly=false`. See [RFC0002, Route changes and connections](../rfcs/RFC0002.md#route-changes-and-connections). |
| CloudNativePG | At both sites, installed before Strait. One `Cluster` per site in namespace `app`, in CloudNativePG's distributed topology: `pg-a` at site a, `pg-b` at site b. Strait only reads them. Whether the chart turns the plugin on: [The cnpg plugin switch](#the-cnpg-plugin-switch). |
| Tools | `kubectl`, `helm`, `wg` from wireguard-tools, and the `strait` CLI ([Install](./guide/install.md), [on Windows](./guide/install.md#on-windows)). |
| A shell | bash (WSL on Windows). |

Names used at both sites:

| Item | Value |
|---|---|
| Strait namespace | `strait-system` |
| Helm release | `strait` |
| WireGuard Secret | `strait-wireguard`, key `privateKey` |
| WireGuard listen port | 32100 |
| Tunnel addresses | Site a `100.64.0.1/30`, site b `100.64.0.2/30` |

## Steps

### 1. Namespace and WireGuard key

At each site, create the namespace with the Pod Security level `privileged`, and a Secret that holds a new WireGuard private key:

```sh
for s in a b; do
  kubectl --context site-$s create namespace strait-system
  kubectl --context site-$s label namespace strait-system \
    pod-security.kubernetes.io/enforce=privileged
  wg genkey | kubectl --context site-$s -n strait-system \
    create secret generic strait-wireguard --from-file=privateKey=/dev/stdin
done
```

### 2. Install the chart

`values-a.yaml`:

```yaml
site:
  name: a
  spec:
    wireguard:
      privateKeySecretRef:
        name: strait-wireguard
```

`values-b.yaml` is the same, with `name: b`.

| Value | Why |
|---|---|
| `site.name` | The site's name. Members and peers refer to it. Required. |
| `site.spec.wireguard.privateKeySecretRef.name` | The Secret from step 1. The chart lets the gateway agent read this Secret and no other. |

Size `gateway.resources.envoy` to the site's traffic: Envoy slows down when its node runs short of CPU ([benchmark](benchmark.md#under-cpu-contention)). Every other value is in the chart's [values.yaml](../deploy/helm/strait/values.yaml).

Dry run, which checks the rendered `Site`:

```sh
helm template strait oci://ghcr.io/takodotid/charts/strait \
  --version 1.5.0 \
  --namespace strait-system --values values-a.yaml | strait validate -f -
```

Then install at both sites:

```sh
for s in a b; do
  helm --kube-context site-$s install strait oci://ghcr.io/takodotid/charts/strait \
    --version 1.5.0 \
    --namespace strait-system --values values-$s.yaml --wait
done
```

Then read the state of each site:

```sh
strait --context site-a status
strait --context site-b status
```

Check: `strait status` shows each site, its controller's version, its active gateway pod, and the site's WireGuard public key.

### 3. Join the sites

Read each site's public key. The active gateway agent writes it into the `Site`'s status once it has read the Secret.

```sh
A_KEY=$(kubectl --context site-a get site a \
  -o jsonpath='{.status.wireGuardPublicKey}')
B_KEY=$(kubectl --context site-b get site b \
  -o jsonpath='{.status.wireGuardPublicKey}')
```

Check: both values are set before the next step.

Site a declares its peer b and a link that waits for b to dial:

```sh
cat > site-a.yaml <<EOF
apiVersion: strait.tako.id/v1
kind: SitePeer
metadata:
  name: b
spec:
  wireguard:
    publicKey: ${B_KEY}
---
apiVersion: strait.tako.id/v1
kind: SiteLink
metadata:
  name: b-wan
spec:
  peer: b
  transport: wireguard
  wireguard:
    listenPort: 32100
    address: 100.64.0.1/30
    peerAddress: 100.64.0.2
EOF
```

Site b declares its peer a and a link that dials a node of site a:

```sh
cat > site-b.yaml <<EOF
apiVersion: strait.tako.id/v1
kind: SitePeer
metadata:
  name: a
spec:
  wireguard:
    publicKey: ${A_KEY}
---
apiVersion: strait.tako.id/v1
kind: SiteLink
metadata:
  name: a-wan
spec:
  peer: a
  transport: wireguard
  wireguard:
    listenPort: 32100
    endpoint: 203.0.113.10:32100
    address: 100.64.0.2/30
    peerAddress: 100.64.0.1
EOF
```

| Field | Value here | Why |
|---|---|---|
| `SitePeer` name | The peer's `site.name` | A peer's observation document names its site. A document whose site differs from the `SitePeer`'s name is rejected. |
| `wireguard.publicKey` | The peer's public key | The link accepts only this key. |
| `wireguard.listenPort` | 32100 | Must be in the cluster's node port range, 30000-32767 by default: the gateway's WireGuard Service is a `NodePort` with the same port number. |
| `wireguard.endpoint` | Site b only | Site b dials, and keeps the path open with keepalives. Site a waits. Traffic flows both ways once b has dialed. |
| `wireguard.address`, `wireguard.peerAddress` | A /30 that no other network at either site uses | The tunnel's addresses. They live in the gateway pods only. |

Dry run, which checks each site's objects together with its `Site`:

```sh
for s in a b; do
  helm template strait oci://ghcr.io/takodotid/charts/strait \
    --version 1.5.0 \
    --namespace strait-system --values values-$s.yaml \
    | strait validate -f - -f site-$s.yaml
done
```

Check: the output ends with `Errors: 0. Warnings: 0.`

Then apply:

```sh
kubectl --context site-a apply -f site-a.yaml
kubectl --context site-b apply -f site-b.yaml
```

Then read the link and the peer at each site:

```sh
strait --context site-a links
strait --context site-a peers
strait --context site-b links
strait --context site-b peers
```

Check:

| Command | Expected |
|---|---|
| `strait links` | The link to the peer: `HEALTHY` true, `ACTIVE` true. |
| `strait peers` | The peer: `REACHABLE` true, `LAST SEEN` a few seconds ago. |

If not, see [Troubleshooting](troubleshooting.md#unreachable-peers-and-unhealthy-links).

### 4. Point a name at the database

`pg.yaml`, the same at both sites:

```yaml
apiVersion: strait.tako.id/v1
kind: SiteService
metadata:
  name: pg
  namespace: app
spec:
  mode: Single
  port:
    name: postgres
    port: 5432
  plugin:
    cnpg:
      role: primary
  members:
    - name: pg-a
      site: a
      cnpg:
        cluster: pg-a
    - name: pg-b
      site: b
      cnpg:
        cluster: pg-b
  policy:
    unknownHold: 30s
```

Fields:

| Field | Means |
|---|---|
| `spec.mode: Single` | Route to the one writable member. |
| `spec.plugin.cnpg.role: primary` | Observe CloudNativePG `Cluster`s, and route to the primary. |
| `spec.members` | One member per `Cluster`, with the site that hosts it. Each site observes only its own members. |
| `spec.port` | The port of the application name. |
| `spec.plugin.cnpg.confirmRole` | Optional. Also ask each local database `pg_is_in_recovery()`. Needs a role with `LOGIN` and nothing else, and its Secret named in the chart value `cnpg.confirmRole.secretName`. [RFC0003](../rfcs/RFC0003.md#cnpg-confirming-the-role). |
| `spec.policy.unknownHold` | How long a routed member stays in the route while its site cannot be read. |
| `spec.policy.startupHold` | Optional. Caps the wait after start to read every other site. See [Site return](runbooks/site-return.md#ahead-of-time-startuphold). |
| `spec.policy.connections` | Optional. `Keep` or `EndGateway`. `EndGateway` ends sessions through this site's gateway to a member that leaves the route, so remote clients reconnect; open transactions fail. Needs the chart value `envoy.drainTimeSeconds` set to a few seconds. [RFC0002](../rfcs/RFC0002.md#what-endgateway-covers-and-costs). |
| `spec.policy.isolationTimeout` | Optional. A site that has read no other site for this long stops routing to its own member. Use it with three or more sites, or when a site that loses the others must stop writing. With two sites, the survivor of a site loss stops too until the operator sets `0s` there. [RFC0002](../rfcs/RFC0002.md#the-isolated-site-policy). |

Every field and rule: [RFC0002](../rfcs/RFC0002.md).

Dry run:

```sh
strait validate -f pg.yaml
```

Then apply at both sites:

```sh
kubectl --context site-a apply -f pg.yaml
kubectl --context site-b apply -f pg.yaml
```

### 5. Check the route

Read the route at both sites:

```sh
strait --context site-a status -n app
strait --context site-b explain pg -n app
```

Check, while `pg-a` is the primary:

| Site | Route | Reason | Path of pg-a in explain |
|---|---|---|---|
| a | `pg-a` | `Active` | `Local`: its own pods. |
| b | `pg-a` | `Active` | `Gateway`: gateway b, over the link. |

Applications at both sites connect to `pg.app.svc:5432`. A client that checks the server's host name, such as one with `sslmode=verify-full`, needs that name in each `Cluster`'s `spec.certificates.serverAltDNSNames`.

To move the primary, use CloudNativePG's own procedure. Strait follows: [RFC0002](../rfcs/RFC0002.md#why-single-declarations-matter).

### 6. Add a direct link

A private path between the sites, if there is one, can carry the traffic with no tunnel. Give its link a lower cost: it carries the traffic while it is healthy, and the WireGuard link takes over within 5 seconds when it fails.

::: warning Traffic crosses a direct link in clear
`encryption: None` is required on a `direct` link, and the traffic crosses it in clear.
:::

At site a:

```yaml
apiVersion: strait.tako.id/v1
kind: SiteLink
metadata:
  name: b-direct
spec:
  peer: b
  transport: direct
  encryption: None
  cost: 10
  direct:
    address: 192.0.2.21
```

Site b gets the same with `name: a-direct`, `peer: a`, and site a's address.

| Field or value | Means |
|---|---|
| `encryption: None` | Required on a `direct` link. |
| `cost: 10` | Lower than the WireGuard link's default, 100. |
| `direct.address` | The peer gateway's IP address on the private path. |
| `site.spec.gateway.directService` | How peers reach the control port and the export ports over the direct link. See below. |

The value `site.spec.gateway.directService` is `ClusterIP` by default, which a peer cannot reach over a direct link. Set it in the chart's values, then run `helm upgrade`. Use `NodePort` on nodes that no other network reaches. Otherwise use `LoadBalancer`, with the `annotations` that make the load balancer internal. Only the peer gateways need to reach it: [RFC0007, Sources of the gateway Services](../rfcs/RFC0007.md#sources-of-the-gateway-services).

With `endpointDirect: true` on the direct link, the name points straight at the remote pods: see [RFC0005](../rfcs/RFC0005.md#endpoint-direct) and set `endpointCIDRs` to the peer's pod CIDRs.

## The cnpg plugin switch

The chart value `cnpg.enabled` turns on the `cnpg` plugin: the chart's read-only rule for `postgresql.cnpg.io`, and the controller's flag `--cnpg`. See [RFC0003](../rfcs/RFC0003.md#cnpg-on-and-off), requirement 13.

| Cluster | `cnpg.enabled` |
|---|---|
| Runs a `SiteService` with the `cnpg` plugin | `true`. With `false`, each of its members at this site is `Unknown`, reason `PluginDisabled`. |
| Gets CloudNativePG after Strait | `true`. Install CloudNativePG, then run `kubectl --context site-a -n strait-system rollout restart deployment/strait-controller`. |

## Next

| Topic | Where |
|---|---|
| The checks before production | [Before production](runbooks/before-production.md) |
| A name routed nowhere, Unknown members, unhealthy links | [Troubleshooting](troubleshooting.md) |
| Design and rules | [The RFCs](../rfcs/) |
