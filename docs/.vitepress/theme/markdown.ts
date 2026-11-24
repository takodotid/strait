// How the pages' markdown reads on the site: what a table, a word of code and a note carry in their markup, so the theme's CSS can set
// them for a narrow screen and a close read. The words of a page are never changed.

import type Token from "markdown-it/lib/token.mjs";
import type { MarkdownRenderer } from "vitepress";

/** A word of code of up to this many characters is never broken across lines: the longest alert name, `StraitGatewayConfigRejected`, is 27. A longer one breaks after `_ . / , = :` first. */
const SHORT_CODE = 28;

/** Of these, a word of code longer than this is marked `mid`: the CSS lets it break again, at `_ . /` first, in a table with no room for it whole. */
const FIRM_CODE = 20;

/** In a table's first column, a multi-word command of up to this many characters is never broken either. */
const COMMAND_CODE = 28;

/** A number with its range, `1141 (1100 to 1227)`, and a port range, `30086-32767`, stay on one line in a table cell. */
const FIGURES = /(?<![\w-])(?:\d[\d.,]*\s\(\d[\d.,]*\sto\s\d[\d.,]*\)|\d+-\d+)(?![\w-])/g;

/** A table's first column is bold only when every cell is a short label of up to this many characters. */
const SHORT_LABEL = 36;

export class Markdown {
    /** The text of an inline token, as it reads: no markup, a line break as a space. */
    private static plain(inline: Token | undefined): string {
        const parts = (inline?.children ?? []).map((token) => {
            if (token.type === "text" || token.type === "code_inline") return token.content;
            if (token.type === "softbreak" || token.type === "hardbreak" || (token.type === "html_inline" && /^<br\s*\/?>$/i.test(token.content))) return " ";
            return "";
        });
        return parts.join("").replace(/\s+/g, " ").trim();
    }

    /** Adds a class to a token once. */
    private static mark(token: Token, name: string): void {
        if (!(token.attrGet("class") ?? "").split(" ").includes(name)) token.attrJoin("class", name);
    }

    /** Splits the text of `children` at each of `FIGURES`, and wraps each match in a span that the CSS keeps on one line. */
    private static keep(state: { Token: typeof Token }, children: Token[]): void {
        for (let index = 0; index < children.length; index++) {
            const token = children[index]!;
            if (token.type !== "text") continue;

            const parts: Token[] = [];
            let from = 0;
            for (const found of token.content.matchAll(FIGURES)) {
                const before = token.content.slice(from, found.index!);
                if (before) parts.push(Markdown.text(state, before));
                const open = new state.Token("html_inline", "", 0);
                open.content = `<span class="nw">`;
                const close = new state.Token("html_inline", "", 0);
                close.content = "</span>";
                parts.push(open, Markdown.text(state, found[0]), close);
                from = found.index! + found[0].length;
            }
            if (parts.length === 0) continue;

            const rest = token.content.slice(from);
            if (rest) parts.push(Markdown.text(state, rest));
            children.splice(index, 1, ...parts);
            index += parts.length - 1;
        }
    }

    private static text(state: { Token: typeof Token }, content: string): Token {
        const token = new state.Token("text", "", 0);
        token.content = content;
        return token;
    }

    /**
     * A table sits in a wrapper that scrolls if it cannot fit. Each body cell carries its column's head text as `data-label`, so the CSS
     * can set a row as a record, on a phone or in a Limitations table, and name each cell; each table part carries its role, so a screen
     * reader still reads a table when the CSS no longer sets it as one. A table whose first column holds only short labels is marked
     * `labels` (bold column); one with an empty head is marked `headless` (no head shown).
     */
    static tables(md: MarkdownRenderer): void {
        md.renderer.rules.table_open = (tokens, index, options, _env, self) => `<div class="table-wrap">${self.renderToken(tokens, index, options).trimEnd()}\n`;
        md.renderer.rules.table_close = (tokens, index, options, _env, self) => `${self.renderToken(tokens, index, options).trimEnd()}</div>\n`;

        md.core.ruler.push("strait_table_labels", (state) => {
            const tokens = state.tokens;
            for (let start = 0; start < tokens.length; start++) {
                const table = tokens[start]!;
                if (table.type !== "table_open") continue;

                const heads: string[] = [];
                const firsts: string[] = [];
                let column = 0;
                let inHead = false;
                let end = start;
                table.attrSet("role", "table");
                // Tables do not nest, so the first table_close after the open is its own.
                for (; end < tokens.length && tokens[end]!.type !== "table_close"; end++) {
                    const token = tokens[end]!;
                    if (token.type === "thead_open" || token.type === "tbody_open") token.attrSet("role", "rowgroup");
                    if (token.type === "thead_open") inHead = true;
                    else if (token.type === "thead_close") inHead = false;
                    else if (token.type === "tr_open") {
                        column = 0;
                        token.attrSet("role", "row");
                    } else if (token.type === "th_open") {
                        token.attrSet("role", "columnheader");
                        if (inHead) heads.push(Markdown.plain(tokens[end + 1]));
                    } else if (token.type === "td_open") {
                        token.attrSet("role", "cell");
                        const label = heads[column];
                        if (label) token.attrSet("data-label", label);
                        const inline = tokens[end + 1];
                        // A cell that holds one long word of code, a key or a metric, is given room in its column for two lines.
                        const only = inline?.children?.length === 1 ? inline.children[0] : undefined;
                        if (only?.type === "code_inline" && only.content.length > SHORT_CODE) Markdown.mark(token, "key");
                        if (inline?.children) Markdown.keep(state, inline.children);
                        if (column === 0) {
                            const text = Markdown.plain(tokens[end + 1]);
                            firsts.push(text);
                            for (const code of tokens[end + 1]?.children ?? []) {
                                if (code.type === "code_inline" && code.content.length <= COMMAND_CODE && /\s/.test(code.content)) Markdown.mark(code, "nb");
                            }
                        }
                        column++;
                    }
                }

                if (firsts.length > 0 && firsts.every((text) => text.length <= SHORT_LABEL)) Markdown.mark(table, "labels");
                if (heads.every((head) => head === "")) Markdown.mark(table, "headless");
                start = end;
            }
        });
    }

    /**
     * A short word of code is marked `nb`, so the CSS keeps it on one line: a flag, a name or `-o yaml` never splits at a hyphen. A long
     * one is marked `long` and gets a break after each `_ . / , = :`, so it breaks there first. A plural `s` or `'s` after a word moves
     * into its chip, set in the reading type, so the chip and the letter read as one word and never part at a line's end.
     */
    static chips(md: MarkdownRenderer): void {
        md.core.ruler.push("strait_code_chips", (state) => {
            for (const block of state.tokens) {
                const children = block.children;
                if (!children) continue;

                for (let index = 0; index < children.length; index++) {
                    const token = children[index]!;
                    if (token.type !== "code_inline") continue;

                    if (token.content.length <= SHORT_CODE) {
                        Markdown.mark(token, "nb");
                        if (token.content.length > FIRM_CODE) Markdown.mark(token, "mid");
                    } else if (!(token.attrGet("class") ?? "").includes("nb")) Markdown.mark(token, "long");
                    const next = children[index + 1];
                    const suffix = next?.type === "text" ? /^(?:['\u2019]s|s)(?![\p{L}\p{N}])/u.exec(next.content)?.[0] : undefined;
                    // The text keeps its letter, so the page's title, read from the tokens after these rules, keeps it too.
                    if (suffix) token.meta = { ...token.meta, suffix };
                }
            }
        });

        // A token that reads as `content` but is otherwise `token`.
        const as = (token: Token, content: string): Token => Object.assign(Object.create(token) as Token, { content });
        const text = md.renderer.rules.text!;
        md.renderer.rules.text = (tokens, index, options, env, self) => {
            const suffix: string | undefined = tokens[index - 1]?.meta?.suffix;
            if (!suffix) return text(tokens, index, options, env, self);
            return text([as(tokens[index]!, tokens[index]!.content.slice(suffix.length))], 0, options, env, self);
        };

        const render = md.renderer.rules.code_inline!;
        md.renderer.rules.code_inline = (tokens, index, options, env, self) => {
            const token = tokens[index]!;
            const kind = /\b(?:long|mid)\b/.exec(token.attrGet("class") ?? "")?.[0];
            // A word that is kept whole, `mid`, takes a break that the CSS turns on (a `wbr` breaks even where white-space says not to).
            const mark = kind === "long" ? "<wbr>" : `<i class="wb"></i>`;
            const suffix: string | undefined = token.meta?.suffix;
            const letter = suffix ? `<span class="sfx">${text([as(tokens[index + 1]!, suffix)], 0, options, env, self)}</span>` : "";
            return render(tokens, index, options, env, self).replace(
                /^(<code[^>]*>)([\s\S]*)(<\/code>)$/,
                (_, open: string, body: string, close: string) => open + (kind ? body.replace(/[_./,=:](?=\S)/g, `$&${mark}`) : body) + letter + close
            );
        };
    }

    /**
     * A line that opens with `Check:` is hard to find when skimming a procedure: the word is wrapped in a `lead-check` span for the CSS
     * to set as a label. `Dry run:` and `Then apply:` get `lead-in`, which is weight only. The line's words are never changed.
     */
    static leads(md: MarkdownRenderer): void {
        md.core.ruler.push("strait_leads", (state) => {
            const tokens = state.tokens;
            for (let index = 0; index < tokens.length - 1; index++) {
                if (tokens[index]!.type !== "paragraph_open" || tokens[index + 1]!.type !== "inline") continue;

                const children = tokens[index + 1]!.children;
                const first = children?.[0];
                const found = first?.type === "text" ? first.content.match(/^(Check|Dry run|Then apply):(?=\s|$)/) : null;
                if (!children || !first || !found) continue;

                const word = found[1]!;
                const open = new state.Token("html_inline", "", 0);
                open.content = `<span class="${word === "Check" ? "lead-check" : "lead-in"}">`;
                const text = new state.Token("text", "", 0);
                text.content = `${word}:`;
                const close = new state.Token("html_inline", "", 0);
                close.content = "</span>";
                const rest = new state.Token("text", "", 0);
                rest.content = first.content.slice(word.length + 1);
                children.splice(0, 1, open, text, close, ...(rest.content ? [rest] : []));
            }
        });
    }

    /** A note says its kind in a word, in a mark of its own, whatever title the page gives it: `::: warning Title` reads "Warning Title". */
    static notes(md: MarkdownRenderer): void {
        for (const [kind, word] of [["tip", "Tip"], ["warning", "Warning"], ["danger", "Danger"]] as const) {
            const name = `container_${kind}_open`;
            const render = md.renderer.rules[name]!;
            md.renderer.rules[name] = (tokens, index, options, env, self) => {
                const titled = tokens[index]!.info.trim().slice(kind.length).trim() !== "";
                return render(tokens, index, options, env, self).replace(
                    /(<p class="custom-block-title">)(.*?)(<\/p>)/s,
                    (_, open: string, title: string, close: string) =>
                        `${open}<span class="custom-block-kind">${word}</span>${titled ? ` <span class="custom-block-name">${title}</span>` : ""}${close}`
                );
            };
        }
    }
}
