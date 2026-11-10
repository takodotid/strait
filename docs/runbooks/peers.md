# Add or remove a site

A site reads only the peers it has a `SitePeer` for, and observes only its own members ([RFC0004](../../rfcs/RFC0004.md)). A site joins in two stages: its peers and links first, then its members. It leaves in the reverse order.

At every other site, a member whose site is not a reachable peer is `Unknown`, with reason `SiteUnknown` ([RFC0002](../../rfcs/RFC0002.md#requirements), requirement 16). An `Unknown` member is never writable, so in `Single` mode it does not stop the route to the one writable member. These cases do change the route:

| Case | Effect |
|---|---|
| A routed member whose site stops being read | It stays in the route for `policy.unknownHold`, then the name routes nowhere: `HeldUnknown`, then `UnknownTimeout`. |
| `policy.startupHold` set, and a member at a site that is not read | Each start of the controller keeps its local member unrouted for the full `startupHold`, with reason `AwaitingPeers` ([RFC0002, Startup hold](../../rfcs/RFC0002.md#startup-hold)). |
| A new site whose workload declares another primary | Every site that reads it routes the name nowhere, with `DeclarationsDisagree` ([RFC0002](../../rfcs/RFC0002.md#requirements), requirement 18, row 1). |

## Add a site

The examples add site c to sites a and b.

| Step | What to do | Check |
|---|---|---|
| 1. Install | At site c: [Getting started](../getting-started.md), steps 1 and 2. | `kubectl --context site-c get site c -o jsonpath='{.status.wireGuardPublicKey}'` prints a key. |
| 2. Pick the link values | For each new `wireguard` link: a listen port that no other `wireguard` link of its site uses, and a /30 for the tunnel that no network at either end uses. `strait validate` compares listen ports only within the files it reads, and it skips the `List` that `kubectl get -o yaml` writes. | The ports and addresses in use, at each site: `kubectl --context site-a get sitelinks -o custom-columns=NAME:.metadata.name,PORT:.spec.wireguard.listenPort,ADDRESS:.spec.wireguard.address` |
| 3. Peers and links | At sites a and b: a `SitePeer` named c, and a `SiteLink` to c. At site c: a `SitePeer` and a `SiteLink` for each of a and b. The objects and their fields: [Getting started, step 3](../getting-started.md#3-join-the-sites). | Below. |
| 4. Check the links | At every site. | `strait --context site-a peers`: every peer `REACHABLE`. `strait --context site-a links`: a link to each peer `HEALTHY` and `ACTIVE`. |
| 5. The workload | Join site c's workload to the others with the workload's own procedure. It must declare the same primary as the other sites. For CloudNativePG in the distributed topology: c's `Cluster` has `.spec.replica.primary` set to the current primary's name ([CloudNativePG, replica clusters](https://cloudnative-pg.io/documentation/current/replica_cluster/)). | |
| 6. The members | Add site c's members to each `SiteService`, at every site, with the same spec everywhere. | `strait --context site-a explain pg -n app` at every site: site c's members show their own state, not `SiteUnknown`. The route and its reason are unchanged. |

Step 3 at site a, with `site-a-c.yaml` holding the `SitePeer` c and the `SiteLink` to c. The check must end with `Errors: 0. Warnings: 0.`:

```sh
helm template strait oci://ghcr.io/takodotid/charts/strait --version 1.4.0 \
  --namespace strait-system --values values-a.yaml | strait validate -f - -f site-a-c.yaml
kubectl --context site-a apply -f site-a-c.yaml --dry-run=server
kubectl --context site-a apply -f site-a-c.yaml
```

## Remove a site

The examples remove site c from sites a and b.

| Step | What to do | Check |
|---|---|---|
| 1. Move the primaries | Move each primary at site c to another site, with the workload's own procedure. Strait never moves a primary. | `strait --context site-a status -A`: no `Single` mode name routes to a member at site c. |
| 2. The members | Remove site c's members from each `SiteService`, at every site. Strait deletes the `Export`s and `Import`s it made for them. | `strait --context site-a explain pg -n app` at every site: no member at site c, and the route and its reason are unchanged. |
| 3. Imports written by hand | At sites a and b, delete each `Import` whose `spec.site` is c. | `kubectl --context site-a get imports -A -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SITE:.spec.site` lists none at site c. |
| 4. Links, then the peer | At sites a and b: delete each `SiteLink` whose `spec.peer` is c, then the `SitePeer` c. | `strait --context site-a peers` and `strait --context site-a links` show no c. |
| 5. Site c | Take its workload out with the workload's own procedure. Remove Strait: [Roll back, the `Site`](rollback.md#the-site). | |

Step 4 at site a:

```sh
kubectl --context site-a get sitelinks -o custom-columns=NAME:.metadata.name,PEER:.spec.peer
kubectl --context site-a delete sitelink c-wan --dry-run=server
kubectl --context site-a delete sitelink c-wan
kubectl --context site-a delete sitepeer c --dry-run=server
kubectl --context site-a delete sitepeer c
```
