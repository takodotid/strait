# Add or remove a site

A site joins in two stages, its peers and links first, then its members, and leaves in the reverse order. Some cases change a route: [What changes the route](#what-changes-the-route).

## Add a site

The examples add site c to sites a and b.

### 1. Install

At site c: [Getting started](../getting-started.md), steps 1 and 2. Then read the key. Change `site-c` to the site's context.

```sh
kubectl --context site-c get site c -o jsonpath='{.status.wireGuardPublicKey}'
```

Check: this prints a key.

### 2. Pick the link values

For each new `wireguard` link: a listen port that no other `wireguard` link of its site uses, and a /30 for the tunnel that no network at either end uses. `strait validate` compares listen ports only within the files it reads, and it skips the `List` that `kubectl get -o yaml` writes.

List the ports and addresses in use, at each site. Change `site-a` to each site's context.

```sh
kubectl --context site-a get sitelinks \
  -o custom-columns=NAME:.metadata.name,PORT:.spec.wireguard.listenPort,ADDRESS:.spec.wireguard.address
```

### 3. Add the peers and links

At sites a and b: a `SitePeer` named c, and a `SiteLink` to c. At site c: a `SitePeer` and a `SiteLink` for each of a and b. The objects and their fields: [Getting started, step 3](../getting-started.md#3-join-the-sites).

The example is for site a, with `site-a-c.yaml` holding the `SitePeer` c and the `SiteLink` to c. Change the context, the file names and the chart version to the site's own.

Validate:

```sh
helm template strait oci://ghcr.io/takodotid/charts/strait --version 1.5.0 \
  --namespace strait-system --values values-a.yaml | strait validate -f - -f site-a-c.yaml
```

Check: the output ends with `Errors: 0. Warnings: 0.` Go on only then.

Dry run:

```sh
kubectl --context site-a apply -f site-a-c.yaml --dry-run=server
```

Then apply:

```sh
kubectl --context site-a apply -f site-a-c.yaml
```

### 4. Check the links

At every site.

```sh
strait --context site-a peers
strait --context site-a links
```

Check: every peer `REACHABLE`. A link to each peer `HEALTHY` and `ACTIVE`.

### 5. Join the workload

Join site c's workload to the others with the workload's own procedure. It must declare the same primary as the other sites. For CloudNativePG in the distributed topology: c's `Cluster` has `.spec.replica.primary` set to the current primary's name ([CloudNativePG, replica clusters](https://cloudnative-pg.io/documentation/current/replica_cluster/)).

### 6. Add the members

Add site c's members to each `SiteService`, at every site, with the same spec everywhere. Then run this at every site. Change `site-a` to each site's context.

```sh
strait --context site-a explain pg -n app
```

Check: site c's members show their own state, not `SiteUnknown`. The route and its reason are unchanged.

## Remove a site

The examples remove site c from sites a and b.

### 1. Move the primaries

Move each primary at site c to another site, with the workload's own procedure. Strait never moves a primary.

```sh
strait --context site-a status -A
```

Check: no `Single` mode name routes to a member at site c.

### 2. Remove the members

Remove site c's members from each `SiteService`, at every site. Strait deletes the `Export`s and `Import`s it made for them.

```sh
strait --context site-a explain pg -n app
```

Check: at every site, no member at site c, and the route and its reason are unchanged.

### 3. Delete the imports written by hand

At sites a and b, delete each `Import` whose `spec.site` is c. List the imports to find them, and again to check. Change `site-a` to each site's context.

```sh
kubectl --context site-a get imports -A \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SITE:.spec.site
```

Check: the list shows none at site c.

### 4. Delete the links, then the peer

::: warning Delete only when steps 1 to 3 are done
The deletes in this step come last, in the reverse order of the join.
:::

At sites a and b: delete each `SiteLink` whose `spec.peer` is c, then the `SitePeer` c.

The example is for site a, with the link `c-wan`. List the links:

```sh
kubectl --context site-a get sitelinks -o custom-columns=NAME:.metadata.name,PEER:.spec.peer
```

Links first. Dry run:

```sh
kubectl --context site-a delete sitelink c-wan --dry-run=server
```

Then delete:

```sh
kubectl --context site-a delete sitelink c-wan
```

Then the peer. Dry run:

```sh
kubectl --context site-a delete sitepeer c --dry-run=server
```

Then delete:

```sh
kubectl --context site-a delete sitepeer c
```

Check: run this at each site. The output shows no c.

```sh
strait --context site-a peers
strait --context site-a links
```

### 5. Remove Strait from site c

Take its workload out with the workload's own procedure. Remove Strait: [Roll back, the `Site`](rollback.md#the-site).

## What changes the route

A site reads only the peers it has a `SitePeer` for, and observes only its own members ([RFC0004](../../rfcs/RFC0004.md)).

At every other site, a member whose site is not a reachable peer is `Unknown`, with reason `SiteUnknown` ([RFC0002](../../rfcs/RFC0002.md#requirements), requirement 16). An `Unknown` member is never writable, so in `Single` mode it does not stop the route to the one writable member. These cases do change the route:

| Case | Effect |
|---|---|
| A routed member whose site stops being read | It stays in the route for `policy.unknownHold`, then the name routes nowhere: `HeldUnknown`, then `UnknownTimeout`. |
| `policy.startupHold` set, and a member at a site that is not read | Each start of the controller keeps its local member unrouted for the full `startupHold`, with reason `AwaitingPeers` ([RFC0002, Startup hold](../../rfcs/RFC0002.md#startup-hold)). |
| `policy.isolationTimeout` set, and no peer read for that long | The local member is not routed, with reason `SiteIsolated`. A site with no `SitePeer` left is never isolated ([RFC0002, The isolated-site policy](../../rfcs/RFC0002.md#the-isolated-site-policy)). |
| A new site whose workload declares another primary | Every site that reads it routes the name nowhere, with `DeclarationsDisagree` ([RFC0002](../../rfcs/RFC0002.md#requirements), requirement 18, row 1). |
