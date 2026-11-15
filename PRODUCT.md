# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Primary: platform and database operators who run an application at two or more sites, each one Kubernetes cluster, with a stateful dependency that has one leader, such as a PostgreSQL primary under CloudNativePG. They know Kubernetes well. They install the chart with Helm, write `SiteService`s in YAML, and read the state with `strait` from a terminal.

Secondary: the operator on call during an incident, who needs to know at once why a name points where it does; application developers, who only need the one name to connect to; AI agents, which read Strait's state through the `strait` skill.

## Product Purpose

Strait is a virtual service between Kubernetes clusters at different sites. It gives an application one stable local name per dependency, at every site, points that name at the member that is active now, and carries the TCP traffic between sites. Success: an application holds no topology, a move of the leader is followed within seconds, and a name never points at a guess.

One web surface:

- **The docs site** (`docs/`, strait.tako.id), VitePress: the guide, the operations runbooks, the reference, and the limitations.

## Positioning

Strait routes. It never fails over, promotes, demotes or fences a workload: the workload's own controller, or the operator, does that, and Strait follows. When it can't tell which member is active, the name routes nowhere rather than to a guess. Plugins read a workload's state with read-only permissions. Each site decides its own route from what it sees and what its peers publish.

## Operating Context

- Each site runs the chart: one `strait-controller` and a `strait-gateway` Deployment with one active replica.
- Sites reach each other over links: a `direct` private path or a `wireguard` tunnel between the gateways. Each site publishes its observations, and its peers poll them over the same link. No site holds credentials to another site's API.
- Everything is set in a resource's spec, and every observation shows in its status. Nothing is configured by hand on a gateway or a node.
- Releases are signed tags; each release's image, chart and archives are signed and attested.

## Capabilities and Constraints

- Carries TCP only. No L7, no protocol parsing.
- No routing through a third site, no traffic weights, no pod-to-pod networking between clusters.
- The `strait` CLI is read-only.
- Design decisions live in `rfcs/`. When the code and an RFC disagree, the RFC wins.

## Brand Commitments

- Name: **Strait**. The docs' home title: "One name per dependency, at every site."
- Voice: plain English for operators. Answer first, short sentences, everyday words, tables for facts, no em-dashes, nothing personal. Refer to "the operator" or a role, never a person.
- Examples use neutral names: sites `a` and `b`, the name `pg` in namespace `app`, documentation addresses.
- Licensed under the Apache License 2.0.

## Evidence on Hand

- The maintainers run Strait in production for their own databases. No other confirmed users.
- Measured numbers: the gateway benchmark (`docs/benchmark.md`) and the controller's memory per Service (`docs/sizing.md`), each with how it was measured.
- The end-to-end scenarios on kind (`test/e2e/README.md`), run by CI on every pull request.
- No testimonials, no adoption figures, no failover times from real clusters yet. Never invent them.

## Product Principles

1. **Route nowhere rather than to a guess.** Never imply Strait knows what it doesn't.
2. **Observe, never act.** The step that moves a workload stays with its controller or a person.
3. **Answer during an incident.** Every page and every command says why, not only what.
4. **Honest about limits.** Each limitation is stated with what to do about it.
5. **Modest presence.**
