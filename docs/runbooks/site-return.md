# Site return

A site comes back after a [site loss](site-loss.md), or a partition between sites heals. Its workload may still run a primary that the other sites have replaced. Until it is demoted, it takes the writes of the returning site's clients wherever Strait routes them to it.

Run each write first with `--dry-run=server`, then without it. The examples show the dry run.

## Steps

### 1. Watch the returning site

As soon as its API answers, run this.

```sh
strait --context site-b status -A
```

Check: with [`startupHold`](#ahead-of-time-startuphold), a name whose primary is local routes nowhere with `AwaitingPeers` until the site has read the other sites, or until the hold ends.

### 2. Demote the old primary

If the returning site's workload still runs a primary that another site has replaced, demote it with the workload's own procedure, before the hold ends. For CloudNativePG in the distributed topology: set `.spec.replica.primary` of the returning site's `Cluster` to the new primary's name, as in [Site loss, step 3](site-loss.md#_3-promote).

The example is for a returning site b, where `pg-a` at site a is the new primary.

```sh
kubectl --context site-b -n app patch cluster.postgresql.cnpg.io pg-b --type merge \
  -p '{"spec":{"replica":{"primary":"pg-a"}}}' --dry-run=server
```

Then read the route:

```sh
strait --context site-b explain pg -n app
```

Check: the returning site's member has `ROLE` `replica`.

### 3. Check every site

Run [Upgrade, step 3](upgrade.md#_3-wait) at each site, with that site's context.

Check: every name routes to the new primary, reason `Active`.

## What the sites show until step 2 is done

| Case | Route | Reason, conditions |
|---|---|---|
| The returning site reads the other sites. | Nowhere, at every site that reads both primaries. | `DeclarationsDisagree` when the sites declare different primaries, else `MultipleWritable`. The `Multiple` condition is True, and a Warning event fires. Strait never picks one primary. |
| The returning site cannot read the other sites, and its hold has ended or is not set. Without a hold, this is also the case from its controller's first decision until it reads the other sites. | Its own primary, at the returning site. | `Active`. Its clients write to the old primary. |

## A partition that heals

A site that was only cut off kept its controller running, so no hold applies. During the partition, each side routes as in a [site loss](site-loss.md#what-strait-does). Once the sites read each other again:

- No member was promoted during the partition: every site routes to the one primary, `Active`.
- A member was promoted at another site during the partition: the route is as in the first row above, nowhere until one primary is demoted. Run [step 2](#2-demote-the-old-primary) and [step 3](#3-check-every-site): demote the primary that the other sites replaced, then check every site.

## Ahead of time: startupHold

Set `spec.policy.startupHold` on every `Single` mode `SiteService`, at every site, in production. A site's controller starts when the site comes back, so the hold must be in the spec before the loss.

| Item | Value |
|---|---|
| Field | `spec.policy.startupHold`, 0s to 10m, `Single` mode only. |
| Effect | After the controller starts, it does not route to a local member until it has read every other site with a member, or until the hold ends. The route is nowhere meanwhile, with reason `AwaitingPeers` ([RFC0002, Startup hold](../../rfcs/RFC0002.md#startup-hold)). |
| Suggested value | 30s. The hold ends once every other site with a member has been read, so it costs time only when a controller starts while cut off from those sites. |
| Cost | A controller that starts during a real partition, for example after an upgrade or a crash, keeps its local primary unrouted for up to `startupHold`. |
| Not covered | A controller that kept running through the loss. It is not held ([RFC0002, Limitations](../../rfcs/RFC0002.md#limitations)): [A partition that heals](#a-partition-that-heals). |

Set it at each site. The example is for the `SiteService` `pg` at site a, with the suggested 30s.

```sh
kubectl --context site-a -n app patch siteservice pg --type merge \
  -p '{"spec":{"policy":{"startupHold":"30s"}}}' --dry-run=server
```

Then check that the controller reads it:

```sh
strait --context site-a explain pg -n app
```

Check: `strait explain` shows `startupHold 30s` on the `Policy:` line.
