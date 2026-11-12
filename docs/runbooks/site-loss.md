# Site loss

A site is lost when no other site can read it: its cluster is down, or every link to it is cut. Strait never moves a primary. The operator moves it with the workload's own tools, and Strait follows.

## What Strait does

For a `Single` mode `SiteService` with a member at the lost site:

| Where | Route | Reason |
|---|---|---|
| Each other site, while the lost site's member is routed | Kept for `policy.unknownHold` after the member was last confirmed, then nowhere. | `HeldUnknown`, then `UnknownTimeout` |
| Each other site, while its own member is the one writable member | Unchanged. | `Active` |
| The lost site, if it still runs | Its own primary, as long as it is writable. | `Active` |
| The lost site, if it still runs, with `policy.isolationTimeout` | Its own primary until it has read no peer for `isolationTimeout`, then nowhere. Its clients then fail instead of writing to the old primary. | `Active`, then `SiteIsolated` |
| A surviving site with `policy.isolationTimeout` that sees no other peer, as with two sites | Its own primary until `isolationTimeout`, then nowhere, also after its member is promoted. | `SiteIsolated` |

`isolationTimeout`, off unless set, is in [RFC0002, The isolated-site policy](../../rfcs/RFC0002.md#the-isolated-site-policy). With three or more sites, a surviving site still sees another peer and is not isolated.

In `Set` mode, the lost site's members leave the route once their hold ends, and the other members stay. At the other sites, the lost site's members show `STATE` `Unknown`. Why a site that still runs keeps its own primary: [RFC0002, Why a partition does not stop the local primary](../../rfcs/RFC0002.md#why-a-partition-does-not-stop-the-local-primary).

## Steps

| Step | What to do | Check |
|---|---|---|
| 1. Confirm | At each other site, read its peers and links. | `strait --context site-a peers`: the lost site's `REACHABLE` is false. `strait --context site-a links`: no link to it is `HEALTHY`. |
| 2. Stop the old primary and its clients | If the lost site may still run, make sure its primary takes no writes, with the workload's or the infrastructure's tools. For example, power off its nodes, or stop its database and its applications. With `isolationTimeout` set there, its names route nowhere once it has seen no peer for that long, but a client that connects to the member's own Service, or keeps an open connection, still writes. At the surviving sites, `policy.connections: EndGateway` ends the sessions through their gateway to the lost member. Nothing in Strait ends a session that a client at the lost site opened to its own primary. | Strait cannot see a site that it cannot read. A primary that still runs keeps taking writes from its own site's clients. |
| 3. Promote | Promote a member at a surviving site with the workload's own procedure. Every surviving site must then declare the new primary, or the names route nowhere with `DeclarationsDisagree`. For CloudNativePG in the distributed topology: set `.spec.replica.primary` of each surviving `Cluster` to the new primary's name, with no promotion token ([CloudNativePG, replica clusters](https://cloudnative-pg.io/documentation/current/replica_cluster/)). | At each surviving site, `strait --context site-a status -A`: the names route to the new primary, reason `Active`. While CloudNativePG restarts the new primary, the route is nowhere with `Transitioning`. |
| 3b. If the surviving site is isolated | With `isolationTimeout` set and no peer left to read, the names stay nowhere with `SiteIsolated` after the promotion. Set `isolationTimeout` to 0s in that site's `SiteService`s, as below. Set it back once the lost site is read again. | `strait --context site-a explain pg -n app`: reason `Active`. |
| 4. Before the lost site returns | Prepare its return: [Site return](site-return.md). | |

Step 3 at site a, where `pg-a` is the new primary:

```sh
kubectl --context site-a -n app patch cluster.postgresql.cnpg.io pg-a --type merge \
  -p '{"spec":{"replica":{"primary":"pg-a"}}}' --dry-run=server
kubectl --context site-a -n app patch cluster.postgresql.cnpg.io pg-a --type merge \
  -p '{"spec":{"replica":{"primary":"pg-a"}}}'
strait --context site-a explain pg -n app
```

Step 3b at site a, for `pg`:

```sh
kubectl --context site-a -n app patch siteservice pg --type merge \
  -p '{"spec":{"policy":{"isolationTimeout":"0s"}}}' --dry-run=server
kubectl --context site-a -n app patch siteservice pg --type merge \
  -p '{"spec":{"policy":{"isolationTimeout":"0s"}}}'
```

The end-to-end step `Site loss` runs steps 1 and 3 on two sites: [`test/e2e/README.md`](../../test/e2e/README.md#scenarios).
