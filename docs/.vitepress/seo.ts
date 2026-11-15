// What search engines and link previews read about each page: its own description, its one address, a preview card, and structured data.

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import type { DefaultTheme, HeadConfig, PageData } from "vitepress";

type Site = {
    hostname: string;
    name: string;
    description: string;
    repository: string;
    sidebar: DefaultTheme.SidebarItem[];
};

export class Seo {
    /** The longest description search results show in full. */
    private static readonly LONGEST = 160;


    /** A page's markdown, with each `<!--@include: file-->` replaced by the file it names. */
    private static source(file: string): string {
        const text = readFileSync(file, "utf8").replace(/\r\n/g, "\n");

        return text.replace(/<!--\s*@include:\s*(\S+?)\s*-->/g, (_, path: string) => {
            try {
                return Seo.source(resolve(dirname(file), path.replace(/[#{].*$/, "")));
            } catch {
                return "";
            }
        });
    }

    /** Markdown as the plain text a reader sees, with each code span left as a numbered mark, so its dots and angle brackets are not read as prose. */
    private static plain(markdown: string, code: string[]): string {
        return markdown
            .replace(/`([^`]*)`/g, (_, span: string) => `@@${code.push(span) - 1}@@`)
            .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
            .replace(/(\*\*|__|\*|_)(\S[^*_]*?)\1/g, "$2")
            .replace(/<\/?[a-z][^>]*>/g, "")
            .replace(/\s+/g, " ")
            .trim();
    }

    /** The first prose on a page, as whole sentences that fit in a search result. */
    static describe(file: string): string | undefined {
        const blocks = Seo.source(file)
            .replace(/^---\n[\s\S]*?\n---\n/, "")
            .replace(/^```[\s\S]*?^```/gm, "")
            .replace(/^:::[\s\S]*?^:::/gm, "")
            .split(/\n\s*\n/)
            .map((block) => block.trim());

        for (const block of blocks) {
            // Prose is not a heading, list, table, quote or markup.
            if (!block || /^(#|\||<|>|-|\*|!|\d+\.)/.test(block) || block.includes("{{")) continue;

            const code: string[] = [];
            const text = Seo.plain(block, code);
            const restore = (part: string) => part.replace(/@@(\d+)@@/g, (_, index: string) => code[Number(index)] ?? "").trim();

            // A sentence that leads into what follows it, ending in a colon, is left out.
            const sentences = text.match(/.+?[.!?](?=\s|$)/g);
            if (!sentences) continue;

            let description = "";
            for (const sentence of sentences) {
                if (restore(description + sentence).length > Seo.LONGEST) break;
                description += sentence;
            }

            // One sentence longer than a search result shows is cut at a word.
            return description
                ? restore(description)
                : `${restore(sentences[0] ?? "")
                      .slice(0, Seo.LONGEST - 3)
                      .replace(/\s+\S*$/, "")}...`;
        }

        return undefined;
    }

    /** The page's address, as cleanUrls serves it. */
    static address(hostname: string, relativePath: string): string {
        const path = relativePath.replace(/(^|\/)index\.md$/, "$1").replace(/\.md$/, "");
        return `${hostname}/${path}`;
    }

    /** The sidebar group a page is listed under, named and linked by its first page, for the page's breadcrumb. */
    private static groupOf(sidebar: DefaultTheme.SidebarItem[], path: string): { name: string; link: string } | undefined {
        const group = sidebar.find((candidate) => candidate.items?.some((item) => item.link === path));
        const first = group?.items?.[0]?.link;

        return group?.text && first && first !== path ? { name: group.text, link: first } : undefined;
    }


    /** The tags a page adds to its head for search engines and link previews. */
    static head(page: PageData, site: Site): HeadConfig[] {
        if (page.isNotFound) return [["meta", { name: "robots", content: "noindex" }]];

        const home = page.relativePath === "index.md";
        const url = Seo.address(site.hostname, page.relativePath);
        const title = home ? `${site.name} | ${page.frontmatter.titleTemplate}` : `${page.title} | ${site.name}`;
        const description = page.description || site.description;
        const modified = page.lastUpdated ? new Date(page.lastUpdated).toISOString() : undefined;
        const group = Seo.groupOf(site.sidebar, url.slice(site.hostname.length));

        const website = {
            "@type": "WebSite",
            "@id": `${site.hostname}/#website`,
            name: site.name,
            url: `${site.hostname}/`,
            description: site.description,
            inLanguage: "en",
        };

        const graph = home
            ? [
                  website,
                  {
                      "@type": "SoftwareSourceCode",
                      name: site.name,
                      description: site.description,
                      url: `${site.hostname}/`,
                      codeRepository: site.repository,
                      programmingLanguage: "Go",
                      license: `${site.repository}/blob/main/LICENSE`,
                  },
              ]
            : [
                  website,
                  {
                      "@type": "TechArticle",
                      headline: page.title,
                      description,
                      url,
                      inLanguage: "en",
                      isPartOf: { "@id": `${site.hostname}/#website` },
                      ...(modified ? { dateModified: modified } : {}),
                  },
                  {
                      "@type": "BreadcrumbList",
                      itemListElement: [
                          { name: site.name, item: `${site.hostname}/` },
                          ...(group ? [{ name: group.name, item: `${site.hostname}${group.link}` }] : []),
                          { name: page.title, item: url },
                      ].map((crumb, index) => ({ "@type": "ListItem", position: index + 1, ...crumb })),
                  },
              ];

        return [
            ["link", { rel: "canonical", href: url }],
            ["meta", { property: "og:type", content: home ? "website" : "article" }],
            ["meta", { property: "og:site_name", content: site.name }],
            ["meta", { property: "og:locale", content: "en_US" }],
            ["meta", { property: "og:title", content: title }],
            ["meta", { property: "og:description", content: description }],
            ["meta", { property: "og:url", content: url }],
            ...(modified ? [["meta", { property: "article:modified_time", content: modified }] as HeadConfig] : []),
            ["meta", { name: "twitter:card", content: "summary" }],
            ["meta", { name: "twitter:title", content: title }],
            ["meta", { name: "twitter:description", content: description }],
            ["script", { type: "application/ld+json" }, JSON.stringify({ "@context": "https://schema.org", "@graph": graph })],
        ];
    }
}
