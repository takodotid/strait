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
    return getComputedStyle(document.documentElement).getPropertyValue(`--chart-${name}`).trim();
}

async function draw() {
    await nextTick();
    const { default: mermaid } = await import("mermaid");
    mermaid.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        theme: "base",
        themeVariables: {
            darkMode: isDark.value,
            fontFamily: token("font-text"),
            background: token("paper"),
            primaryColor: token("card"),
            primaryTextColor: token("ink"),
            primaryBorderColor: token("ink"),
            secondaryColor: token("sea"),
            tertiaryColor: token("sea"),
            lineColor: token("ink"),
            textColor: token("ink"),
            clusterBkg: token("sea"),
            clusterBorder: token("contour"),
            edgeLabelBackground: token("paper"),
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
