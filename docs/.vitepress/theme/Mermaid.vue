<!-- A mermaid diagram, drawn in the browser in the tokens' colours, again when the page changes between light and dark. -->
<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useData } from "vitepress";

const props = defineProps<{ code: string }>();
/** The diagram's accessible name: its `%% accTitle:` comment line. */
const label = decodeURIComponent(props.code).match(/^\s*%%\s*accTitle:\s*(.+)$/m)?.[1]?.trim() ?? "Diagram";
const { isDark } = useData();
const svg = ref("");
const box = ref<HTMLElement | null>(null);
const natural = ref(0);
const scrolls = ref(false);
let drawn = 0;

/** A token of tokens.css, as the page has it now. */
function token(name: string): string {
    return getComputedStyle(document.documentElement).getPropertyValue(`--ship-${name}`).trim();
}

async function draw() {
    await nextTick();
    const { default: mermaid } = await import("mermaid");
    // Mermaid measures each label as it draws, so the face must be loaded first, or the labels are cut short.
    await document.fonts.load(`16px ${token("font")}`).catch(() => undefined);
    await document.fonts.ready;
    mermaid.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        theme: "base",
        themeVariables: {
            darkMode: isDark.value,
            // Single quotes: mermaid writes the family into style attributes, where a double quote ends the attribute.
            fontFamily: token("font").replaceAll('"', "'"),
            background: token("plane"),
            primaryColor: token("plane"),
            primaryTextColor: token("ink"),
            primaryBorderColor: token("ink"),
            secondaryColor: token("deck"),
            tertiaryColor: token("deck"),
            lineColor: token("ink"),
            textColor: token("ink"),
            clusterBkg: token("deck"),
            clusterBorder: token("line"),
            edgeLabelBackground: token("plane"),
        },
    });
    const { svg: out } = await mermaid.render(`mermaid-${Math.random().toString(36).slice(2)}-${drawn++}`, decodeURIComponent(props.code));
    svg.value = out;
    await nextTick();
    // The drawing keeps the width it was laid out at, so its labels stay at their size; a narrow column scrolls it sideways.
    natural.value = Math.round(box.value?.querySelector("svg")?.viewBox.baseVal.width ?? 0);
    measure();
}

function measure() {
    const el = box.value;
    scrolls.value = !!el && el.scrollWidth > el.clientWidth + 1;
}

let watcher: ResizeObserver | undefined;
onMounted(() => {
    draw();
    watcher = new ResizeObserver(measure);
    if (box.value) watcher.observe(box.value);
});
onBeforeUnmount(() => watcher?.disconnect());
watch(isDark, draw);
</script>

<template>
    <div class="mermaid-frame">
        <div ref="box" class="mermaid" :style="{ '--natural': natural + 'px' }" role="region" :aria-label="label" tabindex="0" v-html="svg" />
        <p v-if="scrolls" class="mermaid-hint">Scroll the diagram sideways. ⟷</p>
    </div>
</template>

<style scoped>
.mermaid-frame {
    margin: 16px 0;
}

.mermaid {
    overflow-x: auto;
}

.mermaid :deep(svg) {
    min-width: var(--natural);
}

.mermaid-hint {
    margin: 4px 0 0;
    font-size: 13px;
    color: var(--ship-soft-ink);
}
</style>
