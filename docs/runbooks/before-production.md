# Before production

Check each row at every site before the site serves production traffic.

| Check | Why | How |
|---|---|---|
| Socket load balancing covers pods | Where the CNI translates Service traffic per packet with connection tracking, a new connection can still reach the previous member after a route change, while that member is a backend of any Service ([RFC0002, Route changes and connections](../../rfcs/RFC0002.md#route-changes-and-connections)). | For Cilium: socket LB on, with `socketLB.hostNamespaceOnly=false`. Check it as [Troubleshooting](../troubleshooting.md#connections-after-a-route-change) shows. |
| Clients that write refuse a read-only server | A route change never moves an established connection, and a new one can still reach the demoted member, as above. Only the member can refuse that traffic. A demoted database refuses writes, but serves reads. | A PostgreSQL client through libpq: `target_session_attrs=read-write` in its connection string. libpq then fails a new connection to a server in hot standby, or with `default_transaction_read_only` on, so the client cannot write to or read from a demoted member over it. Other client libraries have their own setting. |
| Gateway ports outside the band of dynamic node ports | Another Service can take a gateway port, and the gateway Service then takes no new port ([Gateway node ports](#gateway-node-ports)). | Below. |
| `endpointCIDRs` on each `direct` link with `endpointDirect` | Without it, a peer, or anyone on the path of the link, picks the address that an application name points at ([RFC0007, Limitations](../../rfcs/RFC0007.md#limitations)). | Set it to the peer's pod CIDRs. `strait validate` warns without it, and the link's condition `EndpointsLimited` is `False`. |

## Gateway node ports

A gateway Service of type `NodePort`, or `LoadBalancer` without `allocateLoadBalancerNodePorts: false`, opens on every node a node port equal to each of its ports ([RFC0005, Ports](../../rfcs/RFC0005.md#ports)).

| Gateway Service | Type by default | Ports |
|---|---|---|
| `strait-gateway-direct` | `ClusterIP` | `Site.spec.gateway.controlPort`, 30900, and the port of each `Export`, from `exportPorts`, 30901-31899. |
| `strait-gateway-wireguard` | `NodePort` | `listenPort` of each `wireguard` `SiteLink`, 31820 by default. |

Kubernetes 1.29 and later give a node port to each Service that names none. They pick one at random from the upper part of the cluster's node port range, and from the lower part only once the upper part is full.

| Node port range | Lower part | Upper part |
|---|---|---|
| Any | The first ports: the size of the range divided by 32, at least 16 and at most 128. | The rest. |
| 30000-32767, the default | 30000-30085 | 30086-32767 |

Strait's default ports are in the upper part. The gateway Service names the port of each new `Export` only when that `Export` gets it, so another Service can take that port first. The API server then refuses the whole gateway Service: it takes no new port, and the `Site`'s condition `GatewayServices` is `False` with the API server's error ([RFC0005, Limitations](../../rfcs/RFC0005.md#limitations)).

| Step | What to do |
|---|---|
| 1. Find the range | The kube-apiserver flag `--service-node-port-range`, 30000-32767 when unset. On a managed cluster, the provider's documentation. No API serves it, so Strait cannot check it. |
| 2. Pick the ports | Put `controlPort`, `exportPorts` and each WireGuard `listenPort` in the lower part, with `exportPorts` no larger than the exports that the site needs. Or keep a gateway Service's ports off the nodes: `LoadBalancer` with `allocateLoadBalancerNodePorts: false`, for a load balancer that sends traffic to the pods. |
| 3. Tell the peers | A new `controlPort` goes into the `SitePeer` of this site at each peer, `spec.controlPort`. A new `listenPort` goes into the `endpoint` of the peer's link, at a peer that dials. |
| 4. Check that no other Service holds them | `kubectl --context site-a get services -A`: `PORT(S)` shows each node port as `PORT:NODEPORT/TCP` or `/UDP`. |
| 5. After the install, and after each change of the ports | The `Site`'s condition `GatewayServices` is `True` ([Troubleshooting](../troubleshooting.md#unreachable-peers-and-unhealthy-links)). |

Choose the ports before production: a change of `exportPorts` moves each `Export` outside the new range to another port ([RFC0005, Ports](../../rfcs/RFC0005.md#ports)).
