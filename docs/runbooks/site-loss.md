# Site loss

A site is lost when no other site can read it: its cluster is down, or every link to it is cut. The operator moves the primary with the workload's own tools, and Strait follows ([What Strait does](#what-strait-does), [Limitations](../limitations.md#what-strait-doesn-t-do)).

Run each write first with `--dry-run=server`, then without it. The examples show the dry run.

## Steps

### 1. Confirm

At each other site, read its peers and links.

```sh
strait --context site-a peers
strait --context site-a links
```

Check: the lost site's `REACHABLE` is false. No link to it is `HEALTHY`.

### 2. Stop the old primary and its clients

If the lost site may still run, make sure its primary takes no writes, with the workload's or the infrastructure's tools. For example, power off its nodes, or stop its database and its applications.

Strait does not end sessions at the lost site: a client that connects to the member's own Service, or keeps an open connection, still writes. With `isolationTimeout` set there, its names route nowhere once it has seen no peer for that long. At the surviving sites, `policy.connections: EndGateway` ends the sessions through their gateway to the lost member.

Check: use the same tools to confirm that the primary takes no writes. Strait cannot see a site that it cannot read.

### 3. Promote

Promote a member at a surviving site with the workload's own procedure. Every surviving site must then declare the new primary, or the names route nowhere with `DeclarationsDisagree`. For CloudNativePG in the distributed topology: set `.spec.replica.primary` of each surviving `Cluster` to the new primary's name, with no promotion token ([CloudNativePG, replica clusters](https://cloudnative-pg.io/documentation/current/replica_cluster/)).

The example is for site a, where `pg-a` is the new primary. Run it at each surviving site, and change the context and the `Cluster` name (the first `pg-a`) to that site's own. The value `"primary":"pg-a"` stays the new primary's name at every site.

```sh
kubectl --context site-a -n app patch cluster.postgresql.cnpg.io pg-a --type merge \
  -p '{"spec":{"replica":{"primary":"pg-a"}}}' --dry-run=server
```

Then read the route:

```sh
strait --context site-a explain pg -n app
```

Check: the names route to the new primary, reason `Active`. While CloudNativePG restarts the new primary, the route is nowhere with `Transitioning`. Then check every surviving site as in [Upgrade, step 3](upgrade.md#_3-wait).

### 4. Lift the isolation timeout, if the surviving site is isolated

With `isolationTimeout` set and no peer left to read, the names stay nowhere with `SiteIsolated` after the promotion. Set `isolationTimeout` to 0s in that site's `SiteService`s. Set it back once the lost site is read again.

The example is for the `SiteService` `pg` at site a.

```sh
kubectl --context site-a -n app patch siteservice pg --type merge \
  -p '{"spec":{"policy":{"isolationTimeout":"0s"}}}' --dry-run=server
```

Then read the route:

```sh
strait --context site-a explain pg -n app
```

Check: reason `Active`.

### 5. Before the lost site returns

Prepare its return: [Site return](site-return.md).

## What Strait does

For a `Single` mode `SiteService` with a member at the lost site, the route outcomes are in [RFC0002](../../rfcs/RFC0002.md). In short:

- Each other site keeps the lost site's routed member for `policy.unknownHold`, then routes nowhere: `HeldUnknown`, then `UnknownTimeout`.
- A site whose own member is the one writable member is unchanged: `Active`.
- The lost site, if it still runs, keeps routing to its own primary while it is writable: `Active`. Stop it in [step 2](#2-stop-the-old-primary-and-its-clients).
- With `policy.isolationTimeout`, a site that has read no peer for that long routes nowhere: `SiteIsolated`. With two sites, a surviving site sees no other peer, so this includes it, also after its member is promoted ([step 4](#4-lift-the-isolation-timeout-if-the-surviving-site-is-isolated)). With three or more sites, a surviving site still sees another peer and is not isolated ([RFC0002, The isolated-site policy](../../rfcs/RFC0002.md#the-isolated-site-policy)).

In `Set` mode, the lost site's members leave the route once their hold ends, and the other members stay. At the other sites, the lost site's members show `STATE` `Unknown`.
