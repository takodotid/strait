<script setup lang="ts">
// The home page: the hull with the name following the primary between two sites, the path of one connection as draft
// marks, the three routes, the two kinds of link, and the first command.

import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { withBase } from "vitepress";
import { EXPLAIN_COMMAND, LINK_FAILOVER, LINKS, MOMENTS, NAME, PATH, REPOSITORY, ROUTES } from "./home-content";

type Site = "a" | "b";
type Layout = "wide" | "tall";

const SITES = ["a", "b"] as const;

/** How long a moment stays, in ms: long enough to read its longest caption, which has 22 words. */
const DWELL = 9000;

/** The moment shown, 0 to 2. The drawing goes across the moments and back. */
const shown = ref(0);
const playing = ref(true);
/** What a screen reader hears. It stays empty while the drawing plays by itself, and holds the caption after the operator picks a moment. */
const announced = ref("");
const copyState = ref<"idle" | "copied" | "selected">("idle");
const commandText = ref<HTMLElement | null>(null);
const moment = computed(() => MOMENTS[shown.value]);

let timer: number | undefined;
let heading = 1;
let copyTimer: number | undefined;

/** What one site's name points at, in the moment shown. */
function endpoints(site: Site) {
    if (!moment.value.routed) return "no endpoints";
    return moment.value.primary === site ? `the pods of pg-${site}` : `gateway ${site}`;
}

function role(site: Site) {
    if (!moment.value.routed) return "declared primary";
    return moment.value.primary === site ? "primary" : "replica";
}

/**
 * Where the parts of a site sit, in the drawing's units. The wide layout puts the sites side by side, and site b is site a
 * mirrored, so the two gateways face each other across the link. The tall layout stacks them for a narrow sheet: site b is
 * site a turned upside down, so the gateways face each other across the link there too.
 */
function parts(layout: Layout, site: Site) {
    const member = { x: 24, width: 120, height: 44 };
    const gateway = { x: 162, width: 90, height: 34 };
    const app = { x: 24, width: 96, height: 34 };
    if (layout === "wide") {
        const x = (at: number, width = 0) => (site === "a" ? at : 276 - at - width);
        const sign = site === "a" ? -1 : 1;
        const spine = x(72);
        const gate = x(207);
        return {
            site,
            at: `translate(${site === "a" ? 0 : 304} 0)`,
            frame: 314,
            app: { ...app, x: x(24, 96), y: 52 },
            name: { x: 24, y: 114, width: 228, height: 52 },
            member: { ...member, x: x(24, 120), y: 262 },
            gateway: { ...gateway, x: x(162, 90), y: 206 },
            wire: `M${spine} 86v28`,
            local: `M${spine} 166v96`,
            remote: `M${gate} 166v40`,
            entry: `M${gate} 240v44h${sign * 63}`,
        };
    }
    if (site === "a") {
        return {
            site,
            at: "translate(0 0)",
            frame: 248,
            app: { ...app, y: 52 },
            name: { x: 24, y: 110, width: 228, height: 52 },
            member: { ...member, y: 196 },
            gateway: { ...gateway, y: 201 },
            wire: "M72 86v24",
            local: "M72 162v34",
            remote: "M207 162v39",
            entry: "M162 218h-18",
        };
    }
    return {
        site,
        at: "translate(0 288)",
        frame: 248,
        app: { ...app, y: 208 },
        name: { x: 24, y: 130, width: 228, height: 52 },
        member: { ...member, y: 52 },
        gateway: { ...gateway, y: 57 },
        wire: "M72 182v26",
        local: "M72 130v-34",
        remote: "M207 130v-39",
        entry: "M162 74h-18",
    };
}

/** Both layouts are drawn, and the sheet's own width decides which one shows. */
const DRAWINGS = (["wide", "tall"] as const).map((layout) => ({
    layout,
    viewBox: layout === "wide" ? "0 0 580 330" : "0 0 276 552",
    sites: SITES.map((site) => parts(layout, site)),
    link: layout === "wide" ? "M252 223h76" : "M207 235v110",
    label: layout === "wide" ? { x: 290, y: 200, anchor: "middle" } : { x: 222, y: 281, anchor: "start" },
}));

function advance() {
    if (shown.value === MOMENTS.length - 1) heading = -1;
    else if (shown.value === 0) heading = 1;
    shown.value += heading;
}

function stop() {
    clearInterval(timer);
    timer = undefined;
}

function play() {
    stop();
    playing.value = true;
    announced.value = "";
    timer = window.setInterval(advance, DWELL);
}

function pause() {
    stop();
    playing.value = false;
}

function pick(index: number) {
    pause();
    shown.value = index;
    announced.value = MOMENTS[index].line;
}

/** Selects the command, so the operator can copy it where the page can't. */
function select() {
    const text = commandText.value;
    const selection = window.getSelection();
    if (!text || !selection) return;
    const range = document.createRange();
    range.selectNodeContents(text);
    selection.removeAllRanges();
    selection.addRange(range);
}

async function copy() {
    clearTimeout(copyTimer);
    try {
        await navigator.clipboard.writeText(EXPLAIN_COMMAND);
        copyState.value = "copied";
    } catch {
        // No clipboard here, for instance on a page served over http: select the text instead.
        select();
        copyState.value = "selected";
    }
    copyTimer = window.setTimeout(() => (copyState.value = "idle"), 4000);
}

onMounted(() => {
    // With reduced motion nothing moves by itself. The controls reach every moment.
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) playing.value = false;
    else play();
});

onBeforeUnmount(() => {
    stop();
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
                        <a class="sh-button" :href="withBase('/guide/introduction')">Read the introduction</a>
                    </p>
                    <p class="sh-supports">
                        Today Strait carries TCP, and reads a workload through two plugins: <code>cnpg</code>, for CloudNativePG, and
                        <code>declared</code>. <a :href="withBase('/limitations')">Limitations</a> has the rest.
                    </p>
                </div>

                <figure class="sh-sheet">
                    <figcaption>{{ NAME }}</figcaption>
                    <div class="sh-body">
                        <svg
                            v-for="drawing in DRAWINGS"
                            :key="drawing.layout"
                            class="sh-drawing"
                            :class="[drawing.layout, `is-primary-${moment.primary || 'none'}`, { 'is-nowhere': !moment.routed }]"
                            :viewBox="drawing.viewBox"
                            role="img"
                            aria-label="Two sites, a and b. The name pg.app.svc points at the primary wherever it runs, and at nothing when the sites disagree."
                        >
                            <g v-for="s in drawing.sites" :key="s.site" :transform="s.at">
                                <rect class="frame" x="8" y="8" width="260" :height="s.frame" />
                                <text class="marking" x="24" y="34">Site {{ s.site }}</text>

                                <rect class="box" v-bind="s.app" />
                                <text :x="s.app.x + s.app.width / 2" :y="s.app.y + 22" text-anchor="middle">app</text>
                                <path class="wire" :d="s.wire" />

                                <rect class="box name" v-bind="s.name" />
                                <text class="mono" :x="s.name.x + 14" :y="s.name.y + 22">pg.app.svc</text>
                                <text class="soft endpoints" :x="s.name.x + 14" :y="s.name.y + 41">{{ endpoints(s.site) }}</text>

                                <rect class="box member" :class="{ 'is-primary': moment.primary === s.site }" v-bind="s.member" />
                                <text class="mono member-text" :class="{ 'is-primary': moment.primary === s.site }" :x="s.member.x + 14" :y="s.member.y + 19">pg-{{ s.site }}</text>
                                <text class="soft member-text" :class="{ 'is-primary': moment.primary === s.site }" :x="s.member.x + 14" :y="s.member.y + 35">{{ role(s.site) }}</text>

                                <rect class="box" v-bind="s.gateway" />
                                <text :x="s.gateway.x + s.gateway.width / 2" :y="s.gateway.y + 22" text-anchor="middle">gateway {{ s.site }}</text>

                                <!-- The local route: the name straight to the member's pods. -->
                                <path class="route" :class="{ 'is-on': moment.primary === s.site }" :d="s.local" />
                                <!-- The remote route: the name to this site's gateway. -->
                                <path class="route" :class="{ 'is-on': moment.routed && moment.primary !== s.site }" :d="s.remote" />
                                <!-- The way in from the other site: this site's gateway to the member. -->
                                <path class="route" :class="{ 'is-on': moment.primary === s.site }" :d="s.entry" />
                            </g>

                            <!-- The link is up in every moment. It is buff while a route uses it, and solid grey when none does. -->
                            <path class="route link" :class="{ 'is-on': moment.routed }" :d="drawing.link" />
                            <text class="marking" :x="drawing.label.x" :y="drawing.label.y" :text-anchor="drawing.label.anchor">Link</text>
                        </svg>
                        <p class="sh-sheet-note sh-moment">
                            <svg viewBox="0 0 14 14" aria-hidden="true">
                                <path v-if="moment.routed" d="M1.5 7.5l3.5 3.5 7.5-8" class="tick" />
                                <rect v-else x="2" y="2" width="10" height="10" />
                            </svg>
                            <span class="sh-moment-text">
                                <span>{{ moment.line }}</span>
                                <!-- The room for the longest caption, so the sheet keeps its height from one moment to the next. -->
                                <span v-for="other in MOMENTS" :key="other.line" aria-hidden="true">{{ other.line }}</span>
                            </span>
                        </p>
                        <p class="sh-only" role="status">{{ announced }}</p>
                        <div class="sh-controls">
                            <button type="button" class="sh-ctl sh-ctl-play" @click="playing ? pause() : play()">
                                <svg viewBox="0 0 14 14" aria-hidden="true">
                                    <path v-if="playing" d="M2.5 1.5h3v11h-3zM8.5 1.5h3v11h-3z" />
                                    <path v-else d="M3 1.5l9 5.5-9 5.5z" />
                                </svg>
                                {{ playing ? "Pause" : "Play" }}
                            </button>
                            <div class="sh-ctl-moments" role="group" aria-label="Moments of the drawing">
                                    <span class="sh-ctl-label" aria-hidden="true">Moment</span>
                                <button
                                    v-for="(m, index) in MOMENTS"
                                    :key="m.short"
                                    type="button"
                                    class="sh-ctl"
                                    :class="{ 'is-current': index === shown }"
                                    :aria-current="index === shown ? 'true' : undefined"
                                    :aria-label="`Moment ${index + 1} of ${MOMENTS.length}: ${m.short}`"
                                    :title="m.short"
                                    @click="pick(index)"
                                >
                                    {{ index + 1 }}
                                </button>
                            </div>
                        </div>
                        <ul class="sh-key" aria-label="Key to the lines">
                            <li>
                                <svg viewBox="0 0 28 8" aria-hidden="true"><path class="in-use" d="M0 4h28" /></svg>
                                Route in use
                            </li>
                            <li>
                                <svg viewBox="0 0 28 8" aria-hidden="true"><path class="not-in-use" d="M0 4h28" /></svg>
                                Route not in use
                            </li>
                            <li>
                                <svg viewBox="0 0 28 8" aria-hidden="true"><path class="plain" d="M0 4h28" /></svg>
                                Wire or link
                            </li>
                        </ul>
                        <p class="sh-sheet-note">
                            A drawn example, from <a :href="withBase('/guide/introduction#how-it-works')">How it works in the introduction</a>.
                        </p>
                    </div>
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
                    <p>One link for each path to a peer. {{ LINK_FAILOVER }}</p>
                    <p><a :href="withBase('/guide/concepts#links')">Links</a> has the rest.</p>
                </div>

                <div class="sh-modes">
                    <div v-for="link in LINKS" :key="link.flag">
                        <h3>{{ link.name }}</h3>
                        <p class="sh-flag"><code>{{ link.flag }}</code></p>
                        <dl>
                            <dt>Is</dt>
                            <dd>{{ link.is }}</dd>
                            <template v-if="link.note">
                                <dt>Worth knowing</dt>
                                <dd>{{ link.note }}</dd>
                            </template>
                        </dl>
                    </div>
                </div>
            </div>
        </section>

        <section class="sh-buff">
            <div class="sh-wrap sh-close">
                <h2>Ask why a name points where it does.</h2>
                <div class="sh-command">
                    <code class="sh-command-text"><span class="sh-prompt" aria-hidden="true">$</span> <span ref="commandText">{{ EXPLAIN_COMMAND }}</span></code>
                    <button type="button" class="sh-copy" @click="copy">
                        {{ copyState === "copied" ? "Copied" : copyState === "selected" ? "Selected: copy it" : "Copy" }}
                    </button>
                    <span class="sh-only" role="status">
                        {{ copyState === "copied" ? "Copied" : copyState === "selected" ? "Selected. Copy it with the keyboard." : "" }}
                    </span>
                </div>
                <p>
                    <a :href="withBase('/guide/install')">Install Strait</a>, follow <a :href="withBase('/getting-started')">getting started</a>, read
                    <a :href="withBase('/limitations')">what Strait does not do</a>, or see the source on <a :href="REPOSITORY">GitHub</a>.
                </p>
                <p>
                    Running Strait already? See <a :href="withBase('/troubleshooting')">troubleshooting</a> and the
                    <a :href="withBase('/runbooks/before-production')">runbooks</a>.
                </p>
            </div>
        </section>
    </article>
</template>
