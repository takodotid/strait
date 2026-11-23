# Concepts

## The pieces

| Piece | Runs as | Does |
|---|---|---|
| Application name | A Service named after the `SiteService`, with no selector | The name the application connects to. Strait writes its endpoints: the member's pods, the local gateway, or nothing. |
| `strait-controller` | Deployment, one per site | Runs the plugins on local members, reads the peers' observations, decides the route, writes the endpoints, and configures the gateway over xDS. |
| Plugin | Inside the controller | Reads a workload's state, read-only: `cnpg` or `declared`. |
| Observation exchange | HTTP `GET` over the link | Each site publishes what it observes about its own members, and each peer polls it. No site holds credentials to another site's API. |
| `strait-gateway` | Deployment per site, one active replica | Envoy carries TCP between sites. An agent next to it holds the WireGuard interfaces and reports link health. |
| Link | One `SiteLink` per path to a peer | A transport, `direct` or `wireguard`, and a cost. The healthy link with the lowest cost carries the traffic. |
| `strait` | CLI on the operator's machine | Shows and explains the state of one site. Read-only. |

Each site has one `Site`, one `SitePeer` per peer and one `SiteLink` per path. Every resource is listed in [Resources and chart values](../reference/api.md).

## A SiteService

Apply the same object at every site. The full example is in [Getting started](../getting-started.md).

Applications at both sites connect to `pg.app.svc:5432`. Each site's controller writes that Service's endpoints: the primary's pods when it is local, the local gateway when it is remote, and nothing when it can't tell which member is writable. What Strait does and doesn't do is in [Limitations](../limitations.md#what-strait-doesn-t-do).

## Modes

| Mode | Routes to |
|---|---|
| `Single` | The one writable member, such as a database primary. |
| `Set` | Every eligible member, such as read replicas, filtered by locality: `LocalFirst`, `LocalOnly` or `Any`. |

See [RFC0002](../../rfcs/RFC0002.md).

## Plugins

| Plugin | Reads |
|---|---|
| `cnpg` | CloudNativePG `Cluster`s, and a `Pooler` when named. |
| `declared` | Nothing: the active member comes from the spec, for a workload that no plugin observes. |

See [RFC0003](../../rfcs/RFC0003.md).

## Links

| Transport | Is |
|---|---|
| `direct` | A private path with no tunnel, declared `encryption: None`. |
| `wireguard` | A kernel WireGuard tunnel between the gateways. |

When the active link fails, new connections move to the next one within 5 seconds. See [RFC0005](../../rfcs/RFC0005.md).

| Feature | Is |
|---|---|
| Endpoint-direct | On a `direct` link with routable pod addresses, the name points straight at the remote pods, with no gateway hop. Set the link's `endpointCIDRs` to the peer's pod CIDRs. See [RFC0005](../../rfcs/RFC0005.md) and the [Gateway benchmark](../benchmark.md). |
| Envoy over xDS | The controller configures Envoy with no reload. A route change never closes an established connection, unless `policy.connections: EndGateway` asks for it. See [RFC0002](../../rfcs/RFC0002.md#ending-connections-through-the-gateway). |
