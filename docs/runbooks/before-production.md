# Before production

Check each row at every site before the site serves production traffic. The gateway ports have steps of their own: [Gateway node ports](#gateway-node-ports).

| Check | How | Why |
|---|---|---|
| Socket load balancing covers pods | For Cilium: socket LB on, with `socketLB.hostNamespaceOnly=false`. Check it as [Troubleshooting](../troubleshooting.md#connections-after-a-route-change) shows. | After a route change, a new connection can still reach the previous member. This happens where the CNI translates Service traffic per packet with connection tracking. It happens while that member is a backend of any Service ([RFC0002, Route changes and connections](../../rfcs/RFC0002.md#route-changes-and-connections)). |
| Clients that write refuse a read-only server | A PostgreSQL client through libpq: `target_session_attrs=read-write` in its connection string. Other client libraries have their own setting. | A route change never moves an established connection, and a new one can still reach the demoted member, as above. Only the member can refuse that traffic. A demoted database refuses writes, but serves reads. With `target_session_attrs=read-write`, libpq fails a new connection to a server that is in hot standby or has `default_transaction_read_only` on. So the client cannot write to or read from a demoted member over that connection. |
| A short `smartShutdownTimeout` on each CloudNativePG `Cluster` | Set 10 to 30 seconds in each `Cluster`'s spec. | A promotion restarts the new primary with a smart shutdown that waits up to `smartShutdownTimeout`, 180s by default. Strait routes nowhere meanwhile ([Failover time](../failover-time.md#the-promotion-restart)). |
| Gateway ports outside the band of dynamic node ports | Choose them as in [Gateway node ports](#gateway-node-ports). | Another Service can take a gateway port. The gateway Service then gets no new port. |
| `endpointCIDRs` on each `direct` link with `endpointDirect` | Set it to the peer's pod CIDRs. `strait validate` warns without it, and the link's condition `EndpointsLimited` is `False`. | Without it, a peer, or anyone on the path of the link, picks the address that an application name points at ([RFC0007, Limitations](../../rfcs/RFC0007.md#limitations)). |

## Gateway node ports

Choose the ports before production. A change of `exportPorts` moves each `Export` that is outside the new range to another port ([RFC0005, Ports](../../rfcs/RFC0005.md#ports)).

### 1. Find the range

The kube-apiserver flag `--service-node-port-range`, 30000-32767 when unset. On a managed cluster, the provider's documentation. No API serves it, so Strait cannot check it.

### 2. Pick the ports

Put `controlPort`, `exportPorts` and each WireGuard `listenPort` in the lower part of the range ([How node ports are given](#how-node-ports-are-given)), with `exportPorts` no larger than the exports that the site needs.

Or keep a gateway Service's ports off the nodes: `LoadBalancer` with `allocateLoadBalancerNodePorts: false`, for a load balancer that sends traffic to the pods.

### 3. Check that no other Service holds them

List the Services of every namespace. Change `site-a` to the site's context.

```sh
kubectl --context site-a get services -A
```

Check: `PORT(S)` shows each node port as `PORT:NODEPORT/TCP` or `/UDP`. No other Service lists a port chosen in step 2. If one does, go back to step 2.

### 4. Tell the peers

At each peer, a new `controlPort` goes into `spec.controlPort` of this site's `SitePeer`. At a peer that dials, a new `listenPort` goes into the `endpoint` of the peer's link.

### 5. Check the site

After the install, and after each change of the ports.

Check: the `Site`'s condition `GatewayServices` is `True` ([Troubleshooting](../troubleshooting.md#unreachable-peers-and-unhealthy-links)).

## How node ports are given

A gateway Service of type `NodePort`, or of type `LoadBalancer` without `allocateLoadBalancerNodePorts: false`, opens a node port on every node for each of its ports, with the same number as that port ([RFC0005, Ports](../../rfcs/RFC0005.md#ports)).

| Gateway Service | Type by default | Ports |
|---|---|---|
| `strait-gateway-direct` | `ClusterIP` | `Site.spec.gateway.controlPort`, 30900, and the port of each `Export`, from `exportPorts`, 30901-31899. |
| `strait-gateway-wireguard` | `NodePort` | `listenPort` of each `wireguard` `SiteLink`, 31820 by default. |

Kubernetes 1.29 and later give a node port to each Service that does not name one. They pick one at random from the upper part of the cluster's node port range, and from the lower part only once the upper part is full.

| Node port range | Lower part | Upper part |
|---|---|---|
| Any | The first ports: the size of the range divided by 32, at least 16 and at most 128. | The rest. |
| 30000-32767, the default | 30000-30085 | 30086-32767 |

Strait's default ports are in the upper part. The gateway Service names a new `Export`'s port only once that `Export` gets it, so another Service can take that port first. The API server then refuses the whole gateway Service: it gets no new port, and the `Site`'s condition `GatewayServices` is `False` with the API server's error ([RFC0005, Limitations](../../rfcs/RFC0005.md#limitations)).
