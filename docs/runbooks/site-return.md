# Site return

A site comes back after a [site loss](site-loss.md), or a partition between sites heals. Its workload may still run a primary that the other sites have replaced. Until it is demoted, that primary takes the writes of the returning site's clients wherever Strait routes them to it.

## Ahead of time: `startupHold`

Set `spec.policy.startupHold` on every `Single` mode `SiteService`, at every site, in production. A site's controller starts when the site comes back, so the hold must be in the spec before the loss.

| Item | Value |
|---|---|
| Field | `spec.policy.startupHold`, 0s to 10m, `Single` mode only. Off unless set. |
| Effect | After the controller starts, it does not route to a local member until it has read every other site with a member, or until the hold ends. The route is nowhere meanwhile, with reason `AwaitingPeers` ([RFC0002, Startup hold](../../rfcs/RFC0002.md#startup-hold)). |
| Without it | The returning site routes its clients to its old primary from its controller's first decision until it reads the other sites. In a two-site test, that lasted about 20 seconds ([RFC0002, Motivation](../../rfcs/RFC0002.md#motivation)). |
| Suggested value | 30s, as in the end-to-end test. The hold ends once every other site with a member has been read, so it costs time only when a controller starts while cut off from those sites. |
| Cost | A controller that starts during a real partition, for example after an upgrade or a crash, keeps its local primary unrouted for up to `startupHold`. |
| Not covered | A controller that kept running through the loss, such as on a site that was only cut off, or whose nodes were frozen and resumed. It is not held ([RFC0002, Limitations](../../rfcs/RFC0002.md#limitations)): [A partition that heals](#a-partition-that-heals). |

Set it at each site, then check that the controller reads it:

```sh
kubectl --context site-a -n app patch siteservice pg --type merge -p '{"spec":{"policy":{"startupHold":"30s"}}}' --dry-run=server
kubectl --context site-a -n app patch siteservice pg --type merge -p '{"spec":{"policy":{"startupHold":"30s"}}}'
strait --context site-a explain pg -n app
```

`strait explain` shows `startupHold 30s` on the `Policy:` line.

## Steps

| Step | What to do | Check |
|---|---|---|
| 1. Watch the returning site | As soon as its API answers: `strait --context site-b status -A`. | With `startupHold`, a name whose primary is local routes nowhere with `AwaitingPeers` until the site has read the other sites, or until the hold ends. |
| 2. Demote the old primary | If the returning site's workload still runs a primary that another site has replaced, demote it with the workload's own procedure, before the hold ends. For CloudNativePG in the distributed topology: set `.spec.replica.primary` of the returning site's `Cluster` to the new primary's name ([CloudNativePG, replica clusters](https://cloudnative-pg.io/documentation/current/replica_cluster/)). | `strait --context site-b explain pg -n app`: the returning site's member has `ROLE` `replica`. |
| 3. Check every site | `strait --context SITE status -A` at each site. | Every peer `REACHABLE`. Every name routes to the new primary, reason `Active`. |

What the sites show until step 2 is done:

| Case | Route | Reason, conditions |
|---|---|---|
| The returning site reads the other sites. | Nowhere, at every site that reads both primaries. | `DeclarationsDisagree` when the sites declare different primaries, else `MultipleWritable`. The `Multiple` condition is True, and a Warning event fires. Strait never picks one primary. |
| The returning site cannot read the other sites, and its hold has ended or is not set. | Its own primary, at the returning site. | `Active`. Its clients write to the old primary. |

Step 2 for a returning site b, where `pg-a` at site a is the new primary:

```sh
kubectl --context site-b -n app patch cluster.postgresql.cnpg.io pg-b --type merge \
  -p '{"spec":{"replica":{"primary":"pg-a"}}}' --dry-run=server
kubectl --context site-b -n app patch cluster.postgresql.cnpg.io pg-b --type merge \
  -p '{"spec":{"replica":{"primary":"pg-a"}}}'
```

The end-to-end step `Two writable members` runs step 1, and the two cases above, on two sites: [`test/e2e/README.md`](../../test/e2e/README.md#scenarios).

## A partition that heals

A site that was only cut off kept its controller running, so no hold applies. Through the partition, it routes its own clients to its own primary ([RFC0002, Why a partition does not stop the local primary](../../rfcs/RFC0002.md#why-a-partition-does-not-stop-the-local-primary)). Once the sites read each other again:

| Case | Route at every site | Reason, conditions | End-to-end step |
|---|---|---|---|
| No member was promoted during the partition. | The one primary. | `Active` | `Partition`: site a routes `pg-b` again within 15s of the heal. In local runs, 2.3 to 3.7s. |
| A member was promoted at another site during the partition. | Nowhere, until one primary is demoted. | `DeclarationsDisagree` when the sites declare different primaries, else `MultipleWritable`. The `Multiple` condition is True, and a Warning event fires. | `Two writable members`: both sites route nowhere, with `Multiple` True, within 30s of the heal. In local runs, 2.7 to 3.8s. |

In the second case, run steps 2 and 3 above: demote the primary that the other sites replaced, then check every site.
