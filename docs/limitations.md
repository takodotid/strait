---
outline: [2, 3]
---

<script setup>
import { data } from "./.vitepress/limitations.data.ts";
</script>

# Limitations

## What Strait doesn't do

These are choices, set as requirements in [RFC0001](../rfcs/RFC0001.md#requirements):

| Strait doesn't | Who does |
|---|---|
| Fail over, switch over, promote, demote, pause or fence a workload. | The workload's own controller, or the operator. Strait follows the move. |
| Guess which member is writable. When it can't tell, the name has no endpoints and a new connection is refused. | Nobody: the workload's state decides. See [RFC0002](../rfcs/RFC0002.md). |
| Parse an application protocol. It carries TCP only. | The application. |
| Route pod-to-pod traffic between clusters, beyond an application name or an `Import`. | A CNI. |
| Route through a third site. | Nobody: traffic between two sites uses a link between those two sites. |

## Known limitations

Each RFC lists the limitations of its subject: what to do about each one, and when to revisit it.

<div v-for="section in data" :key="section.rfc">
  <h3 :id="section.rfc.toLowerCase()">{{ section.rfc }}: {{ section.title }} <a class="header-anchor" :href="'#' + section.rfc.toLowerCase()" :aria-label="'Permalink to ' + section.rfc"></a></h3>
  <p><a :href="section.link">{{ section.rfc }} on GitHub</a></p>
  <div class="limitations" v-html="section.html"></div>
</div>
