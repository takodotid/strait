---
name: Strait
description: A paper sea chart. Ink on warm paper, sea bands and contour rules, one magenta accent, set in a serif with condensed caps for labels. For the documentation site.
colors:
    paper: "#f4efe3"
    card: "#fffdf7"
    sea: "#dce8ea"
    land: "#ecdcae"
    contour: "#c4d8dd"
    ink: "#16233b"
    soft-ink: "#5b6880"
    faint: "#9aa7b6"
    magenta: "#b0127a"
    magenta-hover: "#8d0d62"
    on-magenta: "#fffdf7"
    orange: "#c96a12"
    green: "#1e7a4b"
    red: "#c8321f"
    night-paper: "#0e1828"
    night-card: "#16233b"
    night-sea: "#1b2d44"
    night-land: "#2c2a24"
    night-contour: "#33495f"
    night-ink: "#f4efe3"
    night-soft-ink: "#b3bccb"
    night-faint: "#7c899b"
    night-magenta: "#f27ac2"
    night-magenta-hover: "#ff9fd5"
    night-on-magenta: "#0e1828"
    night-orange: "#f0a24f"
    night-green: "#5cc98f"
    night-red: "#ff7d6b"
typography:
    display:
        fontFamily: "Spectral, Georgia, Times New Roman, serif"
        fontSize: "56px"
        fontWeight: 600
        lineHeight: 1.1
        letterSpacing: "-0.01em"
    title:
        fontFamily: "Spectral, Georgia, Times New Roman, serif"
        fontSize: "32px"
        fontWeight: 600
        lineHeight: 1.25
        letterSpacing: "-0.01em"
    body:
        fontFamily: "Spectral, Georgia, Times New Roman, serif"
        fontSize: "16px"
        fontWeight: 400
        lineHeight: 1.75
    label:
        fontFamily: "IBM Plex Sans Condensed, Arial Narrow, sans-serif"
        fontSize: "12.5px"
        fontWeight: 600
        letterSpacing: "0.12em"
        textTransform: "uppercase"
    wordmark:
        fontFamily: "Spectral, Georgia, Times New Roman, serif"
        fontSize: "20px"
        fontWeight: 600
    mono:
        fontFamily: "IBM Plex Mono, ui-monospace, Menlo, Consolas, monospace"
        fontSize: "14px"
        fontWeight: 400
rounded:
    none: "0px"
borders:
    rule: "1px {colors.contour}"
    frame: "1px {colors.ink}"
    callout: "2px, the state's colour"
components:
    button-primary:
        backgroundColor: "{colors.magenta}"
        textColor: "{colors.on-magenta}"
        rounded: "{rounded.none}"
    button-primary-hover:
        backgroundColor: "{colors.magenta-hover}"
        textColor: "{colors.on-magenta}"
    button-alt:
        backgroundColor: "{colors.card}"
        textColor: "{colors.ink}"
        border: "{borders.frame}"
        rounded: "{rounded.none}"
    table-head:
        backgroundColor: "{colors.sea}"
        textColor: "{colors.ink}"
        typography: "{typography.label}"
    table-cell:
        backgroundColor: "{colors.card}"
        textColor: "{colors.ink}"
        border: "{borders.rule}"
    code-block:
        backgroundColor: "{colors.card}"
        textColor: "{colors.ink}"
        border: "{borders.rule}"
        typography: "{typography.mono}"
    code-inline:
        backgroundColor: "{colors.sea}"
        textColor: "{colors.ink}"
    callout:
        backgroundColor: "{colors.sea}"
        textColor: "{colors.ink}"
        border: "{borders.callout}"
    hero:
        backgroundColor: "{colors.land}"
        textColor: "{colors.ink}"
---

# Design System: Strait

## Overview

**Creative North Star: "The Chart Table"**

A strait is the water between two pieces of land, and Strait carries traffic between two sites. The docs look like a paper sea chart spread on a chart table: ink on warm paper, sea tinted in pale bands, contour lines as rules, and one magenta mark for where to go. Everything is printed, flat and square. Nothing glows.

The site is the reader's chart while they set up and run Strait: they come to find a fact, a step or a reason, often during an incident. The chart's job is to be read quickly and trusted.

Every colour and font is a token in one file, `docs/.vitepress/theme/tokens.css`. `style.css` maps them onto VitePress's default theme and names no colour or font of its own. Code colours and the diagrams read the same tokens.

**Key Characteristics:**

- Warm paper, ink text, sea bands, contour rules.
- One accent, magenta, for links, the primary button and the current page.
- A serif for reading and headings. Condensed upright caps only for short labels.
- Square corners, thin borders, no shadows.
- A night chart for dark mode: deep ink ground, paper-coloured text, the same magenta made lighter.

## Colors

### Primary

- **Magenta** (magenta / night-magenta): the one accent. Links, the primary button, the active page in the navigation, strings in code. Hover is darker by day (magenta-hover) and lighter by night (night-magenta-hover).

### Neutral

- **Paper** (paper / night-paper): the page.
- **Card** (card / night-card): code blocks, table cells, feature cards: anything set on the page.
- **Sea** (sea / night-sea): tinted bands. Table heads, callouts, inline code, the selected search result.
- **Land** (land / night-land): sparingly. The home page's hero, or one highlight band. Never behind long text.
- **Contour** (contour / night-contour): borders and rules: table cells, code frames, dividers.
- **Ink** (ink / night-ink): text, headings, frames that must be seen.
- **Soft ink** (soft-ink / night-soft-ink): secondary text: the sidebar, captions, code comments, card details. On paper and card only.
- **Faint** (faint / night-faint): marks that carry no text. It fails contrast as text on every surface.

### States

- **Orange** (orange / night-orange): warning.
- **Green** (green / night-green): tip, and what works.
- **Red** (red / night-red): danger only.

### Named Rules

**The One Accent Rule.** Magenta is the only accent. No second accent colour, no gradients, no glows, no blur.

**The Red Means Danger Rule.** Red is for danger callouts and destructive steps only, such as a split brain or data loss. Warnings are orange. Every colour state also carries a word or an icon: a callout's title says WARNING or DANGER, and the CLI's route states have the symbols ✔ ▲ ■.

**The State Frame Rule.** Orange, green and red frame a callout or mark an icon. Its text stays ink. In light, orange text fails 4.5:1 on every surface, and green and red fail on sea and land.

### Contrast

Checked with WCAG 2.2's formula. Body text needs 4.5:1, large text and frames 3:1.

| Pair | Used for | Light | Night | Needs |
|---|---|---|---|---|
| ink on paper | body text | 13.68 | 15.51 | 4.5:1 |
| ink on card | tables, code, cards | 15.43 | 13.68 | 4.5:1 |
| ink on sea | table heads, callouts, inline code | 12.54 | 12.16 | 4.5:1 |
| ink on land | the home hero | 11.52 | 12.50 | 4.5:1 |
| soft-ink on paper | sidebar, secondary text | 4.90 | 9.30 | 4.5:1 |
| soft-ink on card | code comments, card details | 5.53 | 8.20 | 4.5:1 |
| magenta on paper | links, active navigation | 5.70 | 7.07 | 4.5:1 |
| magenta on card | strings in code, links in tables | 6.42 | 6.23 | 4.5:1 |
| magenta on sea | links in callouts | 5.22 | 5.54 | 4.5:1 |
| magenta-hover on paper | link hover | 7.73 | 9.47 | 4.5:1 |
| on-magenta on magenta | the primary button | 6.42 | 7.07 | 4.5:1 |
| on-magenta on magenta-hover | the primary button, hover | 8.72 | 9.47 | 4.5:1 |
| orange on paper | warning frame | 3.30 | 8.46 | 3:1 |
| green on paper | tip frame | 4.64 | 8.65 | 3:1 |
| red on paper | danger frame | 4.65 | 7.12 | 3:1 |
| orange on sea | warning frame, inside | 3.02 | 6.63 | 3:1 |

Pairs that fail, and so are never used: soft-ink on sea (4.49 light) and on land (4.13 light); faint as text anywhere (1.80 to 2.41 light, 3.92 to 5.01 night); orange as text (2.78 to 3.72 light); green and red as text on sea or land (3.91 to 4.27 light).

## Typography

**Text font:** Spectral, a serif, for headings and reading.
**Label font:** IBM Plex Sans Condensed, 600, upright caps tracked 0.12em, for short labels only.
**Mono font:** IBM Plex Mono for code, commands and file names.

All three are open-licensed (SIL Open Font License) and served by the site itself from `@fontsource` packages. No font comes from an external CDN.

### Hierarchy

- **Display** (600, 56px, -0.01em): the home page's hero only.
- **Title** (600, 32px page title, 24px section): page and section headings, sentence case.
- **Body** (400, 16px, 1.75): all reading text.
- **Label** (600, 12.5px, caps, 0.12em): navigation groups, table heads, callout titles, badges.
- **Wordmark** (600, 20px): "Strait" in the navigation bar.
- **Mono** (400, 14px in blocks): code.

### Named Rules

**The Short Label Rule.** Caps are for labels of a few words. Headings, buttons, navigation links and reading text stay in sentence case.

## Layout

VitePress's default layout: a navigation bar, a sidebar of five groups (Guide, Operations, Reference, Limitations, Project), the page, and its outline. Tables are the main form of the docs. A wide one scrolls sideways instead of squeezing its columns.

## Elevation & Depth

Flat. Depth comes from surfaces (card on paper, a sea band) and from 1px contour rules. No shadow anywhere.

### Named Rules

**The Flat Chart Rule.** A thing that needs separation gets a surface or a rule, never a shadow or a blur.

## Motion

None beyond VitePress's defaults. Nothing pulses, nothing loops.

## Shapes

Square corners everywhere: buttons, code blocks, tables, callouts, the search box. Borders are 1px contour, or 1px ink where a frame must be seen. A callout's frame is 2px.

## Components

### Buttons

The primary button is magenta with on-magenta text. Others are card with a 1px ink frame and ink text.

### Tables

Cells are card, ruled 1px contour. The head is a sea band with label text, never a heavier rule.

### Code

Blocks are card in a 1px contour frame, in IBM Plex Mono. Text is ink, comments soft ink, strings magenta. Inline code sits on sea.

### Callouts

A sea band with a 2px frame in the state's colour: contour for a note, green for a tip, orange for a warning, red for danger. The title says the state in words. The text stays ink.

### Diagrams

Mermaid diagrams take the tokens: card boxes with ink frames and text, sea for groups, ink lines.

### Identity marks

No logo yet. The wordmark is "Strait" in Spectral 600.

## Do's and Don'ts

### Do:

- **Do** take every colour and font from `tokens.css`, by its role name.
- **Do** use magenta for links, the primary action and the current page, and nothing else.
- **Do** give every state a word or an icon as well as its colour.
- **Do** set the head of a table on a sea band.
- **Do** keep soft ink on paper and card.
- **Do** check a new pair of colours for 4.5:1, or 3:1 for large text and frames, and add it to the contrast table.
- **Do** write plain English: the answer first, short sentences, everyday words.

### Don't:

- **Don't** add a second accent colour, a gradient, a glow, a shadow or a blur.
- **Don't** use red for a warning. Warnings are orange; red is danger.
- **Don't** set text in orange, or in faint.
- **Don't** round a corner.
- **Don't** set long text on land.
- **Don't** load a font from an external CDN.
- **Don't** use em-dashes in any copy.
