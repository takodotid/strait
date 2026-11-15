import { createCssVariablesTheme } from "shiki";
import { defineConfig, type DefaultTheme } from "vitepress";
import { Links } from "./links.ts";
import { Seo } from "./seo.ts";

const repository = "https://github.com/takodotid/strait";
const hostname = "https://strait.tako.id";
const description =
    "A virtual service between Kubernetes clusters: one local name per dependency, at every site, pointed at the member that is active now.";

const sidebar: DefaultTheme.SidebarItem[] = [
    {
        text: "Guide",
        items: [
            { text: "Introduction", link: "/guide/introduction" },
            { text: "Install", link: "/guide/install" },
            { text: "Getting started", link: "/getting-started" },
            { text: "Concepts", link: "/guide/concepts" },
        ],
    },
    {
        text: "Operations",
        items: [
            { text: "Before production", link: "/runbooks/before-production" },
            { text: "Upgrade", link: "/runbooks/upgrade" },
            { text: "Roll back", link: "/runbooks/rollback" },
            { text: "Troubleshooting", link: "/troubleshooting" },
            { text: "Failover time", link: "/failover-time" },
            { text: "Sizing the controller", link: "/sizing" },
            { text: "Site loss", link: "/runbooks/site-loss" },
            { text: "Site return", link: "/runbooks/site-return" },
            { text: "Add or remove a site", link: "/runbooks/peers" },
            { text: "WireGuard key rotation", link: "/runbooks/wireguard-key-rotation" },
            { text: "Not ready", link: "/runbooks/not-ready" },
            { text: "Gateway handover", link: "/runbooks/gateway-handover" },
            { text: "Release", link: "/runbooks/release" },
            { text: "Rebuild a release", link: "/runbooks/rebuild" },
        ],
    },
    {
        text: "Reference",
        items: [
            { text: "The strait command", link: "/reference/cli" },
            { text: "Resources and chart values", link: "/reference/api" },
            { text: "Metrics and alerts", link: "/metrics" },
            { text: "Gateway benchmark", link: "/benchmark" },
            { text: "RFCs", link: "/reference/rfcs" },
        ],
    },
    {
        text: "Limitations",
        items: [{ text: "Limitations", link: "/limitations" }],
    },
    {
        text: "Project",
        items: [
            { text: "How this project is built", link: `${repository}#how-this-project-is-built` },
            { text: "Contributing", link: `${repository}#contributing` },
            { text: "License", link: `${repository}/blob/main/LICENSE` },
        ],
    },
];

export default defineConfig({
    title: "Strait",
    description,
    lang: "en",
    cleanUrls: true,
    lastUpdated: true,

    // docs/package.json is the site's own package, not a page.
    srcExclude: ["node_modules/**"],

    sitemap: {
        hostname,
    },

    head: [
        ["meta", { name: "robots", content: "index, follow, max-image-preview:large, max-snippet:-1" }],
    ],

    // A page without a description of its own is described by its first paragraph, so each page shows its own words in search results.
    transformPageData(pageData, { siteConfig }) {
        if (pageData.frontmatter.description || pageData.isNotFound) return;

        const found = Seo.describe(`${siteConfig.srcDir}/${pageData.relativePath}`);
        if (found) pageData.description = found;
    },

    transformHead: ({ pageData }) => Seo.head(pageData, { hostname, name: "Strait", description, repository, sidebar }),

    markdown: {
        // Code takes its colours from the tokens, in both themes.
        theme: createCssVariablesTheme({ name: "strait", variablePrefix: "--shiki-", fontStyle: true }),
        // Headings get GitHub's anchors, so a link to a heading works on GitHub and on the site alike.
        anchor: { slugify: Links.slug },
        config(md) {
            Links.outward(md, repository);
            Links.mermaid(md);
            Links.verbatim(md);
        },
    },

    themeConfig: {
        nav: [
            { text: "Guide", link: "/guide/introduction" },
            { text: "Operations", link: "/runbooks/before-production" },
            { text: "Reference", link: "/reference/cli" },
            { text: "Limitations", link: "/limitations" },
            { text: "Releases", link: `${repository}/releases` },
        ],

        sidebar,

        socialLinks: [{ icon: "github", link: repository }],

        editLink: {
            pattern: `${repository}/edit/main/docs/:path`,
        },

        // The outline beside each page lists its second- and third-level headings.
        outline: { level: [2, 3], label: "On this page" },

        search: {
            provider: "local",
            options: {
                translations: { button: { buttonText: "Search docs" } },
            },
        },

        footer: {
            message: "Released under the Apache License 2.0.",
        },
    },
});
