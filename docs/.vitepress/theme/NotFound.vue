<script setup lang="ts">
import { VPButton } from "vitepress/theme-without-fonts";
import { onMounted, ref } from "vue";

// The address is read in the browser: the page is built once, for every address that has no page.
const address = ref("");
onMounted(() => {
    address.value = location.pathname;
});
</script>

<template>
    <div class="NotFound">
        <h1 class="title">No page at this address.</h1>
        <p v-if="address" class="address"><code>{{ address }}</code></p>
        <p class="note">Start from a section, or use the search.</p>
        <p class="actions">
            <VPButton size="big" theme="brand" text="Guide" href="/guide/introduction" />
            <VPButton size="big" theme="alt" text="Operations" href="/runbooks/before-production" />
            <VPButton size="big" theme="alt" text="Reference" href="/reference/cli" />
        </p>
    </div>
</template>

<style scoped>
.NotFound {
    max-width: 40rem;
    margin: 0 auto;
    padding: 64px 24px 112px;
}

@media (min-width: 768px) {
    .NotFound {
        padding: 96px 32px 160px;
    }
}

.title {
    margin: 0;
    padding-bottom: 22px;
    font-size: clamp(28px, 9vw, 42px);
    font-stretch: 106%;
    font-weight: 800;
    line-height: 1.1;
    letter-spacing: -0.015em;
    text-wrap: balance;
    position: relative;
}

.title::after {
    content: "";
    position: absolute;
    left: 0;
    bottom: 0;
    width: 64px;
    height: 10px;
    border-top: 4px solid var(--ship-ink);
    background-color: var(--ship-buff);
}

.address {
    margin: 24px 0 0;
    overflow-wrap: anywhere;
}

.address code {
    padding: 2px 6px;
    border: 1px solid var(--ship-line);
    border-radius: var(--ship-radius);
    background: var(--ship-deck);
    font-size: 0.9em;
}

.note {
    margin: 16px 0 0;
    font-size: 16.5px;
    line-height: 1.72;
}

.actions {
    display: flex;
    flex-wrap: wrap;
    gap: 12px;
    margin: 32px 0 0;
}
</style>
