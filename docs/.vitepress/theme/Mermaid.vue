<!-- A mermaid diagram, drawn in the browser in the tokens' colours, again when the page changes between light and dark. -->
<script setup lang="ts">
import { nextTick, onMounted, ref, watch } from "vue";
import { useData } from "vitepress";

const props = defineProps<{ code: string }>();
const { isDark } = useData();
const svg = ref("");
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
}

onMounted(draw);
watch(isDark, draw);
</script>

<template>
    <div class="mermaid" v-html="svg" />
</template>

<style scoped>
.mermaid {
    margin: 16px 0;
    overflow-x: auto;
}
</style>
