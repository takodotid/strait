// The Limitations section of each RFC, read from rfcs/ when the site builds, so the Limitations page shows them without a copy.

import { readFileSync, readdirSync } from "node:fs";
import { resolve } from "node:path";
import { createMarkdownRenderer, defineLoader } from "vitepress";
import { Links, root } from "./links.ts";

export type Section = { rfc: string; title: string; link: string; html: string };

declare const data: Section[];
export { data };

const repository = "https://github.com/takodotid/strait";
const rfcs = resolve(root, "rfcs");

export default defineLoader({
    watch: ["../../rfcs/*.md"],
    async load(): Promise<Section[]> {
        // VitePress keeps one renderer per build, the site's, so the section's links are made absolute in its text first.
        const md = await createMarkdownRenderer(resolve(root, "docs"));

        const sections: Section[] = [];
        for (const name of readdirSync(rfcs).filter((file) => /^RFC\d{4}\.md$/.test(file)).sort()) {
            const file = resolve(rfcs, name);
            const text = readFileSync(file, "utf8").replace(/\r\n/g, "\n");
            const body = text.match(/\n# Limitations\n([\s\S]*?)(?=\n# )/)?.[1]?.trim();
            // An RFC whose Limitations hold no table, such as "None.", has none to show.
            if (!body || !body.includes("|")) continue;

            const title = text.match(/\[RFC#\d{4}\]\s*([^<\n]+)/)?.[1]?.trim() ?? name;
            sections.push({
                rfc: name.replace(/\.md$/, ""),
                title,
                link: `${repository}/blob/main/rfcs/${name}#limitations`,
                html: md.render(
                    body.replace(/\]\(([^)\s]+)\)/g, (_, href: string) => `](${Links.rewrite(href, file, repository, true)})`),
                    { path: file, relativePath: `../rfcs/${name}`, cleanUrls: true }
                ),
            });
        }
        return sections;
    },
});
