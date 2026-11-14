---
name: Strait
description: Ink on warm paper, like a sea chart, with one magenta accent, set in Spectral. For the documentation site.
colors:
    paper: "#f4efe3"
    card: "#fffdf7"
    sea: "#dce8ea"
    contour: "#c4d8dd"
    land: "#ecdcae"
    ink: "#16233b"
    soft-ink: "#5b6880"
    faint: "#9aa7b6"
    magenta: "#b0127a"
    magenta-hover: "#8d0d62"
    on-magenta: "#f4efe3"
    warning: "#c96a12"
    tip: "#1e7a4b"
    danger: "#c8321f"
    night-paper: "#0e1828"
    night-card: "#16233b"
    night-sea: "#1a2d40"
    night-contour: "#304660"
    night-land: "#2a3a4a"
    night-ink: "#f4efe3"
    night-soft-ink: "#b4bfcc"
    night-faint: "#7d8b9c"
    night-magenta: "#f27cc6"
    night-magenta-hover: "#f7a6d8"
    night-on-magenta: "#0e1828"
    night-warning: "#f0a24f"
    night-tip: "#5cc98d"
    night-danger: "#ff8a78"
typography:
    headline:
        fontFamily: "Spectral, Georgia, Times New Roman, serif"
        fontWeight: 700
        letterSpacing: "-0.01em"
    body:
        fontFamily: "Spectral, Georgia, Times New Roman, serif"
        fontSize: "17px"
        fontWeight: 400
        lineHeight: 1.7
    label:
        fontFamily: "IBM Plex Sans Condensed, Arial Narrow, Arial, sans-serif"
        fontSize: "12px"
        fontWeight: 600
        letterSpacing: "0.12em"
        textTransform: "uppercase"
    wordmark:
        fontFamily: "Spectral, Georgia, Times New Roman, serif"
        fontSize: "22px"
        fontWeight: 700
    mono:
        fontFamily: "IBM Plex Mono, ui-monospace, Menlo, Consolas, monospace"
        fontWeight: 400
rounded:
    none: "0px"
borders:
    hairline: "1px"
    frame: "2px"
components:
    button-primary:
        backgroundColor: "{colors.magenta}"
        textColor: "{colors.on-magenta}"
        rounded: "{rounded.none}"
    button-primary-hover:
        backgroundColor: "{colors.magenta-hover}"
        textColor: "{colors.on-magenta}"
    button-default:
        backgroundColor: "{colors.card}"
        textColor: "{colors.ink}"
        borderColor: "{colors.ink}"
        rounded: "{rounded.none}"
    table:
        backgroundColor: "{colors.card}"
        borderColor: "{colors.contour}"
        rounded: "{rounded.none}"
    table-head:
        backgroundColor: "{colors.sea}"
        textColor: "{colors.ink}"
        typography: "{typography.label}"
    code-block:
        backgroundColor: "{colors.card}"
        textColor: "{colors.ink}"
        borderColor: "{colors.contour}"
        typography: "{typography.mono}"
        rounded: "{rounded.none}"
    callout:
        backgroundColor: "{colors.sea}"
        textColor: "{colors.ink}"
        borderWidth: "{borders.frame}"
        rounded: "{rounded.none}"
    callout-tip:
        borderColor: "{colors.tip}"
    callout-warning:
        borderColor: "{colors.warning}"
    callout-danger:
        borderColor: "{colors.danger}"
    badge:
        backgroundColor: "{colors.card}"
        textColor: "{colors.ink}"
        typography: "{typography.label}"
        rounded: "{rounded.none}"
    hero:
        backgroundColor: "{colors.land}"
        textColor: "{colors.ink}"
        borderColor: "{colors.ink}"
---

# Design System: Strait

## Overview

**Creative North Star: "The Chart Table"**

The docs read like a paper sea chart spread on a chart table: ink on warm paper, pale sea bands, thin contour lines, and land used sparingly. A strait is the water between two pieces of land, and Strait carries traffic between two sites.

Nothing on the chart decorates. Lines are drawn in ink or in contour blue, corners are square, and the paper is flat: no shadow, no glow, no gradient. One colour, magenta, marks what you can act on: the brand, links and the active place in the navigation. Orange, green and red each mean one state, and always come with a word or an icon.

There is a night chart for dark mode: deep ink paper, slate bands, text in the paper's colour, and the same magenta, lightened so it keeps its contrast.

Every colour and font is a token in one file, `docs/.vitepress/theme/tokens.css`, by its role. `style.css` maps the tokens onto VitePress's default theme and names no colour or font of its own. The code colours and the diagrams read the same tokens.

**Key Characteristics:**

- Ink on warm paper; sea bands for table heads, callouts and the current page; land only for the home page's opening.
- One accent, magenta. Everything else is ink, paper and the chart's blues.
- Spectral, a serif, for headings and reading; IBM Plex Sans Condensed in small tracked caps for short labels; IBM Plex Mono for code.
- Square corners and 1px or 2px lines. Flat.
- Plain English: the answer first, in short sentences.

## Colors

Warm paper and ink, a sea blue for bands and contours, a sand land, and one magenta.

### Primary

- **Ink** (ink / night-ink): text, headings, the wordmark, the 2px rule over each second-level heading, frames.
- **Paper** (paper / night-paper): the page.
- **Magenta** (magenta / night-magenta): the one accent. Links, the active nav item, the current page in the sidebar, primary buttons, search hits, code keywords. Hover darkens it (magenta-hover) on paper, and lightens it at night.

### Secondary

- **Card** (card / night-card): code blocks, inline code, tables and feature cards, a step whiter than the paper.
- **Sea** (sea / night-sea): tinted bands. A table's head, a callout's ground, the current page in the sidebar, groups in a diagram.
- **Contour** (contour / night-contour): borders and rules: table cells, code frames, the nav's edge.
- **Land** (land / night-land): the home page's opening, and nothing else.

### Tertiary

- **Warning** (warning / night-warning): orange. The frame of a warning callout and a warning badge. Never text: on paper it reaches 3.30:1, enough for a mark, not for words.
- **Tip** (tip / night-tip): green. The frame of a tip, and what works.
- **Danger** (danger / night-danger): red. The frame of a danger callout: split brain, data loss, a destructive step.

### Neutral

- **Soft Ink** (soft-ink / night-soft-ink): secondary text and code comments, on paper and card only.
- **Faint** (faint / night-faint): decoration only. Never text, never the only mark of a state.
- **On Magenta** (on-magenta / night-on-magenta): text on a magenta ground.

### Named Rules

**The One Accent Rule.** Magenta is the only accent. There is no second accent colour, and no gradient, glow, shadow or blur.

**The Red Means Danger Rule.** Red is for danger only: split brain, data loss, a destructive step. Warnings are orange. Every colour state also carries a word or an icon, so it reads without colour: a callout's title says WARNING or DANGER, and the CLI's route states have the symbols ✔ ▲ ■.

**The Ink Text Rule.** Text in a callout, a table head or a badge is ink. The state's colour goes on the frame.

## Typography

**Display Font:** Spectral (with Georgia, Times New Roman, serif)
**Body Font:** Spectral (same stack)
**Label/Mono Font:** IBM Plex Sans Condensed 600 for labels; IBM Plex Mono for code

**Character:** a book serif for reading, like the type on a chart, with a narrow sans for the short labels a chart prints in caps. All three families are open-licensed and self-hosted with the site, from the `@fontsource` packages: no font is fetched from another host.

### Hierarchy

- **Headline** (Spectral 700, -0.01em): page titles and section headings, in sentence case. A second-level heading has a 2px ink rule above it.
- **Body** (Spectral 400, 17px, line height 1.7): reading text.
- **Label** (IBM Plex Sans Condensed 600, 12px, uppercase, 0.12em tracking): nav links, sidebar group names, table heads, callout titles, badges, the outline's title, feature titles on the home page.
- **Wordmark** (Spectral 700, 22px): "Strait" in the nav. There is no logo yet.
- **Mono** (IBM Plex Mono 400 and 600): code, commands, field names.

### Named Rules

**The Short Caps Rule.** Uppercase is only for short labels, set in Plex Sans Condensed and tracked 0.12em. Everything else is sentence case.

## Layout

VitePress's documentation layout: the nav across the top (Guide, Operations, Reference, Limitations, Releases), the sidebar with the Guide, Operations, Reference, Limitations and Project groups, the page, and its outline. The home page opens on land, framed below by a 2px ink rule, then lists its features on cards. Tables are the main form of the docs; a wide one scrolls sideways instead of squeezing its columns.

## Elevation & Depth

Flat. Depth comes from the paper, the card and the sea band, and from lines: 1px contour around tables and code, 2px around callouts. A table's head is a sea band, not a rule. There is no shadow anywhere.

## Motion

None beyond VitePress's own. Nothing pulses.

## Shapes

Every corner is square: radius 0, for buttons, code, tables, callouts, badges and the search box.

## Components

### Buttons

The primary button is magenta with paper-coloured text, and darkens on hover. The other buttons are card with an ink frame and ink text, and take a sea ground on hover.

### Tables

A card with a contour frame and contour cell lines. The head row is a sea band with ink labels in tracked caps.

### Code

Ink on card, in a contour frame. Comments are soft ink and italic, keywords are magenta, and everything else is ink.

### Callouts

A sea ground with a 2px frame in the state's colour: green for a tip, orange for a warning, red for danger, ink for information. The title is the state's word, in ink.

### Badges

Card with ink text in tracked caps, framed in the state's colour.

### Navigation

The wordmark in Spectral; nav links and sidebar groups in tracked caps; the current page on a sea band, in magenta.

### Diagrams

Mermaid diagrams take the tokens: card boxes with ink frames and text, sea for groups, ink lines.

## Contrast

Every text pair meets WCAG AA: 4.5:1 for text, 3:1 for a mark that is not text. Computed with the WCAG 2 formula.

| Use | Pair | Needs | Day | Night |
|---|---|---|---|---|
| Text | `ink` on `paper` | 4.5:1 | 13.68 | 15.51 |
| Text, code, inline code | `ink` on `card` | 4.5:1 | 15.43 | 13.68 |
| Text in a callout or table head | `ink` on `sea` | 4.5:1 | 12.54 | 12.26 |
| Text on the home opening | `ink` on `land` | 4.5:1 | 11.52 | 10.15 |
| Secondary text | `soft-ink` on `paper` | 4.5:1 | 4.90 | 9.55 |
| Secondary text, code comments | `soft-ink` on `card` | 4.5:1 | 5.53 | 8.42 |
| Links, active nav | `magenta` on `paper` | 4.5:1 | 5.70 | 7.18 |
| Links in tables, code keywords | `magenta` on `card` | 4.5:1 | 6.42 | 6.33 |
| Links in a callout, the current page | `magenta` on `sea` | 4.5:1 | 5.22 | 5.67 |
| Link hover | `magenta-hover` on `paper` | 4.5:1 | 7.73 | 9.66 |
| Text on a magenta button, search hits, selection | `on-magenta` on `magenta` | 4.5:1 | 5.70 | 7.18 |
| Text on a magenta button, hover | `on-magenta` on `magenta-hover` | 4.5:1 | 7.73 | 9.66 |
| Tip mark (frame, icon) | `tip` on `paper` | 3:1 | 4.64 | 8.64 |
| Danger mark (frame, icon) | `danger` on `paper` | 3:1 | 4.65 | 7.76 |
| Warning mark (frame, icon) | `warning` on `paper` | 3:1 | 3.30 | 8.46 |
| Tip frame on a callout | `tip` on `sea` | 3:1 | 4.26 | 6.83 |
| Danger frame on a callout | `danger` on `sea` | 3:1 | 4.27 | 6.13 |
| Warning frame on a callout | `warning` on `sea` | 3:1 | 3.02 | 6.69 |

## Do's and Don'ts

### Do:

- **Do** take every colour and font from `docs/.vitepress/theme/tokens.css`, by its role.
- **Do** keep text ink, or soft ink on paper and card, and put a state's colour on a frame or an icon.
- **Do** give every state a word or an icon as well as its colour.
- **Do** keep corners square and lines 1px or 2px.
- **Do** check a new pair of colours for 4.5:1, or 3:1 for a mark, and add it to the contrast table.
- **Do** write the answer first, in short sentences of everyday English.

### Don't:

- **Don't** add a second accent colour, a gradient, a glow, a shadow or a blur.
- **Don't** use red for anything but danger. Warnings are orange.
- **Don't** set text in orange or faint, or soft ink on sea or land: they fall under 4.5:1.
- **Don't** use land beyond the home page's opening.
- **Don't** load a font from another host.
- **Don't** use em-dashes in any copy.
