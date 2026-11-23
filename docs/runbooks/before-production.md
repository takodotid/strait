# Before production

Check each row at every site before the site serves production traffic. The gateway ports have steps of their own: [Gateway node ports](#gateway-node-ports).

| Check | How | Why |
|---|---|---|
| Socket load balancing covers pods | For Cilium: socket LB on, with `socketLB.hostNamespaceOnly=false`. Check it as [Troubleshooting](../troubleshooting.md#connections-after-a-route-change) shows. | After a route change, a new connection can still reach the previous member ([RFC0002, Route changes and connections](../../rfcs/RFC0002.md#route-changes-and-connections)). |
| Clients that write refuse a read-only server | A PostgreSQL client through libpq: `target_session_attrs=read-write` in its connection string. Other client libraries have their own setting. | Only the client can refuse a demoted member. |
| A short `smartShutdownTimeout` on each CloudNativePG `Cluster` | Set 10 to 30 seconds in each `Cluster`'s spec. | A promotion restart waits up to `smartShutdownTimeout`, and Strait routes nowhere meanwhile ([Failover time](../failover-time.md#the-promotion-restart)). |
| `endpointCIDRs` on each `direct` link with `endpointDirect` | Set it to the peer's pod CIDRs. `strait validate` warns without it, and the link's condition `EndpointsLimited` is `False`. | Without it, a peer, or anyone on the path of the link, picks the address that an application name points at ([RFC0007, Limitations](../../rfcs/RFC0007.md#limitations)). |

## Gateway node ports

Choose the ports before production. A change of `exportPorts` moves each `Export` that is outside the new range to another port ([RFC0005, Ports](../../rfcs/RFC0005.md#ports)).

### 1. Find the range

The kube-apiserver flag `--service-node-port-range`, 30000-32767 when unset. On a managed cluster, the provider's documentation.

### 2. Pick the ports

Put `controlPort`, `exportPorts` and each WireGuard `listenPort` in the lower part of the range, 30000-30085 with the default range ([RFC0005, Ports](../../rfcs/RFC0005.md#ports)), with `exportPorts` no larger than the exports that the site needs.

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
