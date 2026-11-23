<script setup lang="ts">
import DefaultTheme from "vitepress/theme-without-fonts";
import { useChrome } from "./chrome.ts";
import NotFound from "./NotFound.vue";

const { Layout } = DefaultTheme;
useChrome();
</script>

<template>
    <Layout>
        <template #not-found><NotFound /></template>
    </Layout>
</template>
