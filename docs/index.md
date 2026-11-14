---
layout: home
title: Strait
titleTemplate: One name per dependency, at every site

hero:
    name: Strait
    text: One name per dependency, at every site.
    tagline: Strait points each application's local name at the member that is active now, in its own Kubernetes cluster or another one, and carries the TCP traffic between them.
    actions:
        - theme: brand
          text: Introduction
          link: /guide/introduction
        - theme: alt
          text: Install
          link: /guide/install
        - theme: alt
          text: Getting started
          link: /getting-started

features:
    - title: Routes nowhere rather than to a guess
      details: When Strait can't tell which member is active, the name has no endpoints. A new connection is refused at once, and the client retries.
      link: /guide/concepts#routes-nowhere-rather-than-to-a-guess
    - title: Observes, never acts
      details: Plugins read a workload's state with read-only permissions. Strait never promotes, demotes or fences anything. It follows.
      link: /guide/concepts#observes-never-acts
    - title: Links between sites
      details: A direct path or a WireGuard tunnel between the sites' gateways. When the active link fails, new connections move to the next one.
      link: /guide/concepts#links
    - title: A read-only CLI
      details: strait explain says why a name points where it does. strait --skill prints a skill document for AI agents.
      link: /reference/cli
---
