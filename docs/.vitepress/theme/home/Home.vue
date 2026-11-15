<script setup lang="ts">
// The home page: the hull with the name following the primary between two sites, the path of one connection as draft
// marks, the three routes, the two kinds of link, and the first command.

import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { withBase } from "vitepress";
import { EXPLAIN_COMMAND, LINKS, MOMENTS, NAME, PATH, REPOSITORY, ROUTES } from "./home-content";

/** The order the drawing goes through its moments: across, and back. */
const ORDER = [0, 1, 2, 1];
const step = ref(0);
const copied = ref(false);
const moment = computed(() => MOMENTS[ORDER[step.value]]);

let timer: number | undefined;
let copyTimer: number | undefined;

/** What one site's name points at, in the moment shown. */
function endpoints(site: "a" | "b") {
    if (!moment.value.routed) return "no endpoints";
    return moment.value.primary === site ? `the pods of pg-${site}` : `gateway ${site}`;
}

function role(site: "a" | "b") {
    if (!moment.value.routed) return "declared primary";
    return moment.value.primary === site ? "primary" : "replica";
}

async function copy() {
    try {
        await navigator.clipboard.writeText(EXPLAIN_COMMAND);
        copied.value = true;
        copyTimer = window.setTimeout(() => (copied.value = false), 1800);
    } catch {
        // The command stays on the page to be selected by hand.
    }
}

onMounted(() => {
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    timer = window.setInterval(() => (step.value = (step.value + 1) % ORDER.length), 4200);
});

onBeforeUnmount(() => {
    clearInterval(timer);
    clearTimeout(copyTimer);
});
</script>

<template>
    <article class="ship-home">
        <section class="sh-opening">
            <div class="sh-wrap sh-opening-grid">
                <div class="sh-opening-text">
                    <h1>One name per dependency, at every site.</h1>
                    <p class="sh-lede">
                        Strait points each application's local name at the member that is active now, in its own Kubernetes cluster or another
                        one, and carries the TCP traffic between them.
                    </p>
                    <p class="sh-actions">
                        <a class="sh-button sh-button-go" :href="withBase('/getting-started')">
                            Get started
                            <svg viewBox="0 0 20 12" aria-hidden="true"><path d="M0 6h17M12 1l5 5-5 5" /></svg>
                        </a>
                        <a class="sh-button" :href="withBase('/guide/introduction')">How it works</a>
                    </p>
                    <p class="sh-supports">
                        Today Strait carries TCP, and reads a workload through two plugins: <code>cnpg</code>, for CloudNativePG, and
                        <code>declared</code>. <a :href="withBase('/limitations')">Limitations</a> has the rest.
                    </p>
                </div>

                <figure class="sh-sheet">
                    <figcaption>{{ NAME }}</figcaption>
                    <svg
                        class="sh-drawing"
                        :class="[`is-primary-${moment.primary || 'none'}`, { 'is-nowhere': !moment.routed }]"
                        viewBox="0 0 560 330"
                        role="img"
                        aria-label="Two sites, a and b. The name pg.app.svc points at the primary wherever it runs, and at nothing when the sites disagree."
                    >
                        <g v-for="(site, index) in (['a', 'b'] as const)" :key="site" :transform="`translate(${index * 284} 0)`">
                            <rect class="frame" x="8" y="8" width="260" height="314" />
                            <text class="marking" x="24" y="34">Site {{ site }}</text>

                            <rect class="box" x="24" y="52" width="96" height="34" />
                            <text x="72" y="74" text-anchor="middle">app</text>
                            <path class="wire" d="M72 86v28" />

                            <rect class="box name" x="24" y="114" width="228" height="52" />
                            <text class="mono" x="38" y="136">pg.app.svc</text>
                            <text class="soft endpoints" x="38" y="155">{{ endpoints(site) }}</text>

                            <rect class="box member" :class="{ 'is-primary': moment.primary === site }" x="24" y="262" width="120" height="44" />
                            <text class="mono member-text" :class="{ 'is-primary': moment.primary === site }" x="38" y="281">pg-{{ site }}</text>
                            <text class="soft member-text" :class="{ 'is-primary': moment.primary === site }" x="38" y="297">{{ role(site) }}</text>

                            <rect class="box" x="162" y="206" width="90" height="34" />
                            <text x="207" y="228" text-anchor="middle">gateway {{ site }}</text>

                            <!-- The local route: the name straight to the member's pods. -->
                            <path class="route" :class="{ 'is-on': moment.primary === site }" d="M72 166v96" />
                            <!-- The remote route: the name to this site's gateway. -->
                            <path class="route" :class="{ 'is-on': moment.routed && moment.primary !== site }" d="M207 166v40" />
                            <!-- The way in from the other site: this site's gateway to the member. -->
                            <path class="route" :class="{ 'is-on': moment.primary === site }" d="M207 240v44h-63" />
                        </g>

                        <path class="route link" :class="{ 'is-on': moment.routed }" d="M252 223h56" />
                        <text class="marking" x="280" y="200" text-anchor="middle">Link</text>
                    </svg>
                    <p class="sh-sheet-note sh-moment" aria-live="polite">
                        <svg viewBox="0 0 14 14" aria-hidden="true">
                            <path v-if="moment.routed" d="M1.5 7.5l3.5 3.5 7.5-8" class="tick" />
                            <rect v-else x="2" y="2" width="10" height="10" />
                        </svg>
                        <span>{{ moment.line }}</span>
                    </p>
                    <p class="sh-sheet-note">A drawn example, from <a :href="withBase('/guide/introduction#how-it-works')">How it works</a>.</p>
                </figure>
            </div>
        </section>

        <div class="sh-waterline" aria-hidden="true"></div>

        <section class="sh-plane">
            <div class="sh-wrap sh-chapter">
                <div class="sh-chapter-head">
                    <h2>One connection, from site b to a primary at site a.</h2>
                    <p>Each site decides its own route. Its controller reads only the members at its own site, and polls what the other sites observe.</p>
                    <p><a :href="withBase('/guide/concepts')">Concepts</a> has the pieces and what each one guarantees.</p>
                </div>

                <ol class="sh-marks">
                    <li v-for="(hop, index) in PATH" :key="hop.name">
                        <span class="sh-mark" aria-hidden="true">{{ index + 1 }}</span>
                        <div class="sh-mark-text">
                            <h3>{{ hop.name }}</h3>
                            <p>{{ hop.does }}</p>
                        </div>
                        <span class="sh-state">{{ hop.where }}</span>
                    </li>
                </ol>
            </div>
        </section>

        <section class="sh-black">
            <div class="sh-wrap sh-chapter">
                <div class="sh-chapter-head">
                    <h2>Routes nowhere rather than to a guess.</h2>
                    <p>
                        Strait never promotes, demotes or fences anything. It follows. When it can't tell which member is active, the name has no
                        endpoints, and <code>strait explain</code> says why.
                    </p>
                </div>

                <ul class="sh-stops sh-routes">
                    <li v-for="route in ROUTES" :key="route.mark">
                        <strong :class="`is-${route.mark}`">
                            <svg viewBox="0 0 14 14" aria-hidden="true">
                                <path v-if="route.mark === 'confirmed'" d="M1.5 7.5l3.5 3.5 7.5-8" class="tick" />
                                <path v-else-if="route.mark === 'kept'" d="M7 1.5l6 11H1z" />
                                <rect v-else x="2" y="2" width="10" height="10" />
                            </svg>
                            {{ route.name }}
                        </strong>
                        <span>{{ route.when }}</span>
                    </li>
                </ul>
            </div>
        </section>

        <section class="sh-deck">
            <div class="sh-wrap sh-chapter">
                <div class="sh-chapter-head">
                    <h2>Two kinds of link between sites.</h2>
                    <p>A link joins the gateways of two sites. <a :href="withBase('/guide/concepts#links')">Links</a> has the rest.</p>
                </div>

                <div class="sh-modes">
                    <div v-for="link in LINKS" :key="link.flag">
                        <h3>{{ link.name }}</h3>
                        <p class="sh-flag"><code>{{ link.flag }}</code></p>
                        <dl>
                            <dt>Is</dt>
                            <dd>{{ link.is }}</dd>
                            <dt>Worth knowing</dt>
                            <dd>{{ link.note }}</dd>
                        </dl>
                    </div>
                </div>
            </div>
        </section>

        <section class="sh-buff">
            <div class="sh-wrap sh-close">
                <h2>Ask why a name points where it does.</h2>
                <button class="sh-command" type="button" @click="copy">
                    <span><span class="sh-prompt">$</span> {{ EXPLAIN_COMMAND }}</span>
                    <span class="sh-copy" aria-live="polite">{{ copied ? "Copied" : "Copy" }}</span>
                </button>
                <p>
                    <a :href="withBase('/guide/install')">Install Strait</a>, follow <a :href="withBase('/getting-started')">getting started</a>, read
                    <a :href="withBase('/limitations')">what Strait does not do</a>, or see the source on <a :href="REPOSITORY">GitHub</a>.
                </p>
            </div>
        </section>
    </article>
</template>
