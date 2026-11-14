// How the pages' markdown reads on the site and on GitHub alike: a link to a file outside docs/, such as an RFC, stays relative in
// the source, so it works on GitHub, and points at that file on GitHub on the site. A mermaid block is drawn as a diagram.

import { existsSync, statSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import type { MarkdownRenderer } from "vitepress";

/** The repository's root, and the site's pages under it. */
export const root = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const docs = resolve(root, "docs");

export class Links {
    /**
     * The address of `href` on the site, for a link in the file `from`. A link to a file outside docs/ becomes that file on GitHub, at
     * main: a directory as a tree, a file as a blob. Any other link stays as it is, for VitePress to check.
     */
    static rewrite(href: string, from: string, repository: string, absolute = false): string {
        if (/^[a-z][a-z0-9+.-]*:/i.test(href) || href.startsWith("#") || href.startsWith("/")) return href;

        const [path = "", hash = ""] = href.split(/(?=#)/);
        const target = resolve(dirname(from), decodeURI(path));
        if (target === docs || target.startsWith(`${docs}/`)) {
            if (!absolute) return href;
            // Markdown rendered outside its own page, as an RFC's section on another page, names a page by its address.
            if (!existsSync(target)) throw new Error(`${from}: ${href} names no page`);
            return `/${relative(docs, target).replace(/(^|\/)index\.md$/, "$1").replace(/\.md$/, "")}${hash}`;
        }

        const kind = existsSync(target) && statSync(target).isDirectory() ? "tree" : "blob";
        return `${repository}/${kind}/main/${relative(root, target)}${hash}`;
    }

    /** A heading's anchor as GitHub makes it: lower case, no punctuation but hyphens and underscores, a hyphen for each space. */
    static slug(text: string): string {
        return text
            .trim()
            .toLowerCase()
            .replace(/[^\p{L}\p{N}\s_-]/gu, "")
            .replace(/\s/g, "-");
    }

    /** Rewrites each link of a page that leaves docs/, before VitePress checks and renders the page's links. */
    static outward(md: MarkdownRenderer, repository: string): void {
        md.core.ruler.push("strait_outward_links", (state) => {
            const from: string | undefined = state.env?.path;
            if (!from) return;

            for (const block of state.tokens) {
                for (const token of block.children ?? []) {
                    const href = token.type === "link_open" ? token.attrGet("href") : null;
                    if (href) token.attrSet("href", Links.rewrite(href, from, repository));
                }
            }
        });
    }

    /** Leaves inline code to Vue as it is: `{{json .SBOM}}` is a Go template, not an expression of the page. */
    static verbatim(md: MarkdownRenderer): void {
        const code = md.renderer.rules.code_inline!;
        md.renderer.rules.code_inline = (tokens, index, options, env, self) =>
            code(tokens, index, options, env, self).replace(/^<code/, "<code v-pre");
    }

    /** Draws a mermaid block with the Mermaid component, which renders it in the browser. */
    static mermaid(md: MarkdownRenderer): void {
        const fence = md.renderer.rules.fence!;
        md.renderer.rules.fence = (tokens, index, options, env, self) => {
            const token = tokens[index]!;
            if (token.info.trim() !== "mermaid") return fence(tokens, index, options, env, self);

            return `<Mermaid code="${encodeURIComponent(token.content)}" />`;
        };
    }
}
