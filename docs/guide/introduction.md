# Introduction

Strait gives an application one stable local name per dependency, at every site, and points that name at the member that is active now. A site is one Kubernetes cluster. The member can run at the same site or at another one, and Strait carries the TCP traffic between sites.

The application holds no topology: no host lists, no address maps, no failover logic. It connects to `pg.app.svc:5432` at every site. When the PostgreSQL primary moves to another site, the name follows.

The workload's own controller moves the primary. See [Limitations](../limitations.md#what-strait-doesn-t-do).

## How it works

The same `SiteService`, named `pg`, is applied at both sites. Each site decides its own route. A connection to `pg.app.svc` takes this path:

```mermaid
block-beta
  %% accTitle: Path of a connection from the application to pg-a
  columns 5
  appA["app<br/>site a"] space nameA["pg.app.svc"] space pgA[("pg-a<br/>primary")]
  space space space space space
  space space space space gwA["gateway a"]
  space space space space space
  appB["app<br/>site b"] space nameB["pg.app.svc"] space gwB["gateway b"]
  appA --> nameA
  nameA -- "pods of pg-a" --> pgA
  gwA --> pgA
  appB --> nameB
  nameB -- "gateway b" --> gwB
  gwB == "link" ==> gwA
```

- At site a, the primary `pg-a` is local. `pg.app.svc` points at its pods.
- At site b, the primary is remote. `pg.app.svc` points at gateway b, which carries each connection over the link to gateway a, and on to `pg-a`.

Each site's controller reads only its own members and polls the other site's observations over the same link. The pieces and the resources are in [Concepts](./concepts.md).

## What it looks like

`strait explain` at site b, while the primary runs at site a:

```
$ strait --context site-b explain pg -n app
SiteService:  app/pg
Mode:         Single
Plugin:       cnpg, role primary
Policy:       multiple RouteNowhere, unknownHold 30s
Route:        pg-a
Reason:       Active
Meaning:      Single mode. Exactly one member is writable, and no known site declares another. The name points at it.
Message:      pg-a is the only writable member
Since:        2026-10-27T09:48:00Z (12m ago)
Endpoints:    1

Members:
NAME  SITE  ROUTED  PATH     ROLE     ELIGIBLE  STATE   REASON          OBSERVED  CONFIRMED
pg-a  a     true    Gateway  primary  true      Stable  PrimaryCluster  1s        1s
pg-b  b     false   -        replica  false     Stable  ReplicaCluster  1s        -
```

Every command of `strait` is in [The strait command](../reference/cli.md).

## Try it on kind

`make e2e` runs every scenario on kind: see [the e2e README](../../test/e2e/README.md).

To set up two real clusters, start with [Install](./install.md), then [Getting started](../getting-started.md).
