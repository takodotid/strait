---
name: Strait
description: A ship's livery for the documentation site. A sea-green hull, superstructure white, a black band, one buff for actions, set in Archivo across its width axis.
colors:
    hull: "#0d5c50"
    hull-deep: "#08443b"
    on-hull: "#f3fbf6"
    on-hull-soft: "#c4e3d9"
    plane: "#f9faf6"
    deck: "#ebf0ea"
    line: "#d2dad3"
    ink: "#101a17"
    soft-ink: "#4f5d58"
    buff: "#edb23c"
    buff-deep: "#dc9d1f"
    link: "#0d5c50"
    link-hover: "#08443b"
    code-bg: "#101a17"
    code-text: "#e8f0ea"
    code-soft: "#97a8a1"
    code-accent: "#edb23c"
    code-line: "#2c3a36"
    code-edge: "#101a17"
    band: "#101a17"
    band-soft: "#c0cdc6"
    band-line: "#2c3a36"
    warning: "#a85400"
    tip: "#1b6e45"
    danger: "#b3261e"
    night-plane: "#0e1513"
    night-deck: "#162019"
    night-line: "#2c3a36"
    night-ink: "#eef4ee"
    night-soft-ink: "#a9b8b1"
    night-link: "#7fd4bf"
    night-link-hover: "#a9e6d6"
    night-code-bg: "#080d0b"
    night-code-edge: "#5a6a64"
    night-band: "#073a32"
    night-band-soft: "#c4e3d9"
    night-band-line: "#1f6a5c"
    night-warning: "#f0a24f"
    night-tip: "#5cc98d"
    night-danger: "#ff8a78"
typography:
    headline:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontWeight: 750
        fontStretch: "106%"
        letterSpacing: "-0.015em"
    title:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "42px"
        fontWeight: 800
        lineHeight: 1.1
    body:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "16.5px"
        fontWeight: 400
        lineHeight: 1.72
    label:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "13.5px"
        fontWeight: 700
        fontStretch: "78%"
        letterSpacing: "0.07em"
        textTransform: "uppercase"
    table-head:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "14px"
        fontWeight: 700
        fontStretch: "85%"
        letterSpacing: "0.02em"
    wordmark:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "21px"
        fontWeight: 800
        fontStretch: "125%"
        letterSpacing: "0.06em"
        textTransform: "uppercase"
    numeral:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "76px"
        fontWeight: 800
        fontStretch: "62%"
    mono:
        fontFamily: "IBM Plex Mono, ui-monospace, Menlo, Consolas, monospace"
        fontWeight: 400
rounded:
    none: "0px"
    control: "3px"
borders:
    hairline: "1px"
    frame: "2px"
    rule: "3px"
    cap: "4px"
components:
    masthead:
        backgroundColor: "{colors.hull}"
        textColor: "{colors.on-hull}"
        borderColor: "{colors.ink}"
        typography: "{typography.label}"
    button-primary:
        backgroundColor: "{colors.buff}"
        textColor: "{colors.ink}"
        rounded: "{rounded.control}"
    button-primary-hover:
        backgroundColor: "{colors.buff-deep}"
        textColor: "{colors.ink}"
    button-default:
        backgroundColor: "{colors.plane}"
        textColor: "{colors.ink}"
        borderColor: "{colors.ink}"
        rounded: "{rounded.control}"
    table-head:
        backgroundColor: "{colors.ink}"
        textColor: "{colors.plane}"
        typography: "{typography.table-head}"
    table-row:
        borderColor: "{colors.line}"
        rounded: "{rounded.none}"
    code-block:
        backgroundColor: "{colors.code-bg}"
        textColor: "{colors.code-text}"
        borderColor: "{colors.code-edge}"
        typography: "{typography.mono}"
        rounded: "{rounded.control}"
    code-inline:
        backgroundColor: "{colors.deck}"
        textColor: "{colors.ink}"
        borderColor: "{colors.line}"
        rounded: "{rounded.control}"
    callout:
        backgroundColor: "{colors.deck}"
        textColor: "{colors.ink}"
        borderColor: "{colors.ink}"
        borderWidth: "{borders.frame}"
        rounded: "{rounded.control}"
    callout-tip:
        borderColor: "{colors.tip}"
    callout-warning:
        borderColor: "{colors.warning}"
    callout-danger:
        borderColor: "{colors.danger}"
    badge:
        backgroundColor: "{colors.deck}"
        textColor: "{colors.ink}"
        borderColor: "{colors.line}"
        typography: "{typography.label}"
        rounded: "{rounded.control}"
    hero:
        backgroundColor: "{colors.hull}"
        textColor: "{colors.on-hull}"
    footer:
        backgroundColor: "{colors.code-bg}"
        textColor: "{colors.code-soft}"
---

# Design System: Strait

## Overview

**Creative North Star: "Sister Ships"**

The docs wear a ship's livery. The hull's colour fills the masthead and the home page's opening. Below the waterline, the page is superstructure white, with the deck as the second plane, a black band, and the funnel's buff for the one action. Strait and Tack are sister ships: they share the token names and the rules, and differ in the hull and the tints that follow it. Strait's hull is sea green.

Colour comes in whole planes, not in accents. Type is one family, Archivo, used across its width axis: wide for the name, a little wide for headings, normal for reading, narrow for markings. Code sits on the black hull in both themes.

At night the planes turn dark and the ink turns light. The hull and the buff stay.

**Key Characteristics:**

- Whole planes of colour: hull, white, deck, black band, buff.
- One action colour, the funnel's buff, with ink text on it.
- One family across its width axis; IBM Plex Mono for code.
- Heavy headings; short labels in narrow capitals, as stencilled on plate.
- One radius of 3px for controls, code and callouts. Tables and planes are square.
- Plain English: the answer first, in short sentences.

## Colors

Every colour is a token in `docs/.vitepress/theme/tokens.css`, named `--ship-` and its role. `style.css` maps VitePress's own variables to them.

| Token | Day | Night | Used for |
|---|---|---|---|
| `hull` | `#0d5c50` | same | The masthead, the home page's opening, the numerals of the home page by day. |
| `hull-deep` | `#08443b` | same | The search box and the switch on the masthead. A table's head at night. |
| `on-hull` | `#f3fbf6` | same | Text on the hull and on the band. The focus ring on every dark surface. |
| `on-hull-soft` | `#c4e3d9` | same | Secondary text and lines on the hull. |
| `plane` | `#f9faf6` | `#0e1513` | The page. |
| `deck` | `#ebf0ea` | `#162019` | The second plane: callouts, a word of code, badges, the current page in the sidebar. |
| `line` | `#d2dad3` | `#2c3a36` | Hairlines: table rows, dividers. Never the only mark of a state. |
| `ink` | `#101a17` | `#eef4ee` | Text, rules, a table's head by day. |
| `soft-ink` | `#4f5d58` | `#a9b8b1` | Secondary text. |
| `buff` | `#edb23c` | same | The primary button, the active nav item's bar, the title mark, the closing plane of the home page, keywords in code, the route in use in the home drawing. |
| `buff-deep` | `#dc9d1f` | same | The primary button on hover. |
| `link` | `#0d5c50` | `#7fd4bf` | Links, the current page in the sidebar, the word "Check:", the focus ring on a plane. |
| `link-hover` | `#08443b` | `#a9e6d6` | A link on hover. |
| `code-bg` | `#101a17` | `#080d0b` | The ground of a code block, and the footer. |
| `code-text`, `code-soft`, `code-accent` | `#e8f0ea`, `#97a8a1`, `#edb23c` | same | Code, its comments, its keywords. |
| `code-edge` | `#101a17` | `#5a6a64` | The line round a code block. At night it shows against the dark plane. |
| `band` | `#101a17` | `#073a32` | The dark field of the home page. Black by day, the hull's deep green at night. |
| `band-soft`, `band-line` | `#c0cdc6`, `#2c3a36` | `#c4e3d9`, `#1f6a5c` | Secondary text and lines on the band. |
| `tip` | `#1b6e45` | `#5cc98d` | Green: a tip. |
| `warning` | `#a85400` | `#f0a24f` | Orange: a warning. |
| `danger` | `#b3261e` | `#ff8a78` | Red: split brain, data loss, a destructive step. |

The masthead's band and text on buff are `#101a17` in both themes.

### Named Rules

**The One Action Rule.** Buff is the only action colour. Text on it is ink. There is no second accent.

**The Plane Rule.** A colour fills a whole region or marks one thing. There is no glow and no blur. The one gradient is the soft edge of a code block whose line runs past it.

**The Red Means Danger Rule.** Red is for danger only. Warnings are orange. Every state also carries its word, so it reads without colour.

**The Black Hull Rule.** A block of code sits on the black hull in both themes.

## Typography

**Family:** Archivo Variable, with its width axis, for everything but code. **Mono:** IBM Plex Mono, 400 and 600. Both are open-licensed and self-hosted from the `@fontsource` packages: no font is fetched from another host.

| Role | Weight | Width | Size | Used for |
|---|---|---|---|---|
| Wordmark | 800 | 125% | 21px, capitals, tracked 0.06em | "Strait" on the masthead. There is no logo. |
| Page title | 800 | 106% | 28px to 42px, line height 1.1 | The first heading of a page, with the title mark under it. |
| Heading | 750 | 106% | 27px and 20px; 24px for the first below 720px | Section headings, in sentence case, tracked -0.015em. |
| Home headline | 800 | 112% | 40px to 78px | The home page's opening. |
| Home chapter | 800 | 108% | 32px to 56px | The heading of each chapter of the home page. |
| Body | 400 | 100% | 16.5px, line height 1.72 | Reading text, at most 32rem wide. |
| Link | 550 | 100% | as its text | Underlined, 1.5px, thicker on hover. |
| Marking | 700 | 78% | 13.5px, capitals, tracked 0.07em; 14.5px in the nav | Nav links, sidebar groups, a callout's kind, badges, the outline's title. |
| Table head | 700 | 85% | 14px, tracked 0.02em | A column's name, in the case it was written in. |
| Check | 750 | 85% | 0.86em, capitals, tracked 0.07em | The word "Check:" under a runbook step, in the link colour. |
| Numeral | 800 | 62% | 76px; 56px below 561px | The hop numbers on the home page. |
| Mono | 400, 600 | | 0.86em in text | Code, commands, field names. |

### Named Rules

**The Markings Rule.** Capitals are only for short labels, set narrow and tracked 0.07em. Headings and reading text stay in sentence case. A table's head is never in capitals: a unit such as µs and a case-sensitive name must read as written.

**The Width Rule.** Hierarchy comes from width and weight within the one family: wide for the name, narrow for markings and numerals. No second family is added for display.

## Layout

VitePress's layout: the masthead, the sidebar, the page, and its outline.

| Part | Rule |
|---|---|
| Masthead | The hull's colour on every page, 68px high, ending on a 4px black band. On the home page it runs into the opening, and the band comes at the waterline. From 768px to 1047px its links sit closer and the search shows no key. |
| Page | Reading text keeps to 32rem. Tables, code and diagrams take the whole column, which is 768px beside the outline from 1440px. |
| Page title | Ends on the title mark: a buff bar 64px by 10px under a 4px ink cap. |
| Section | A second-level heading starts on a 1px line, 56px below what came before. Straight under the title it has no line. |
| Anchor | A heading reached by its address clears the masthead: 88px, and 140px below 1280px. |
| Home page | Planes in order: hull, a waterline of white then black, white, black band, deck, buff. Each chapter has a heavy heading at the left and its matter at the right, and is one column below 901px. |
| Phone | Below 768px the nav is a menu, and its links are markings. Below 960px the sidebar is a drawer, and a closed drawer is out of the Tab order. Every control of the bars is 44px tall or more. |
| Footer | On the black hull. On a page with a sidebar it keeps to the column right of the sidebar. |

## Elevation & Depth

Flat. Depth comes from the planes and from lines.

| Shadow | Where |
|---|---|
| `0 24px 48px -12px` in `#101a17` at 55% | The sheet with the drawing on the home page's opening. |
| `0 10px 28px` in `#101a17` at 18% | What VitePress floats: menus and the search box. |

There is no other shadow, and no blur behind the masthead.

## Motion

| Motion | Where |
|---|---|
| The drawing steps through its three moments, 9 seconds each | The home page. A button pauses it, and three buttons reach each moment. |
| Colour of a route and of a member, 0.4s | The home drawing, between moments. |
| Colour and arrow on hover, 0.2s to 0.3s | Buttons on the home page. |
| VitePress's own | Menus, the sidebar's drawer, the theme switch. |

With `prefers-reduced-motion`, the drawing starts paused and the transitions are off. Nothing pulses.

## Shapes

| Shape | Radius |
|---|---|
| Buttons, code blocks, a word of code, callouts, badges, the search box, menus, the current page in the sidebar, focus rings | 3px, the token `ship-radius` |
| Tables, planes, the title mark | 0 |

## Components

### Buttons

The primary button is buff with ink text in an ink frame, and darkens on hover. The others are the plane with an ink frame and ink text, and take the deck on hover. On the hull, the primary button has no ink frame, the second has a frame in `on-hull`, and both turn `on-hull` on hover.

### Tables

Set as a timetable: a head band, hairline rows, no frame, figures that line up. A table fills the column. The head is ink by day and `hull-deep` at night, so no band is brighter than the page.

| Case | Rule |
|---|---|
| A first column of short labels | Heavier. `markdown.ts` marks the table `labels`. |
| A table with an empty head | Shows no head. Its first row starts on a 3px rule of the head's colour. |
| A word of code in a cell | May break anywhere. A short word, of 28 characters or fewer, does not break while the table has room. |
| Below 1280px | Less padding in each cell. |
| Below 720px | No columns. Each row is a record: its cells one under the other, each after its column's name. `markdown.ts` gives each cell that name, and each part of the table its role for a screen reader. |

A table never scrolls the page sideways.

### Code

A block is light text on the black hull in a 1px edge, with comments soft and keywords in buff. A line that runs past the block leaves a soft edge where it goes on. Below 720px a shell block wraps, and the next line of a wrap is set in by four characters. A word of code in the text is ink on the deck, in a hairline frame.

### Callouts

A deck plane in a 2px frame of the kind's colour: green for a tip, orange for a warning, red for danger, ink for information. The kind is said in a word, set as a marking on the kind's colour. The title and the text are ink. A callout that guards a runbook step comes before that step.

### Badges

Ink on the deck in a hairline frame, as a marking. The word carries the state.

### Navigation

The wordmark, lettered wide. Nav links and sidebar groups are markings. The active nav item has a 6px buff bar under it, and a nav link turns buff on hover. In the phone's menu the current section is on the deck, in the link colour, after a buff square. The current page in the sidebar is on the deck, in the link colour, heavier. A long entry of the outline wraps.

### Focus

A 3px ring, 2px clear of the control: the link colour on a plane, `on-hull` on the masthead, the footer, a code block and the dark planes of the home page. A control as tall as the masthead takes the ring inside.

### Diagrams

Mermaid diagrams take the tokens: boxes on the plane with ink frames and text, the deck for groups, ink lines. A diagram keeps the width it was laid out at, and a narrow column scrolls it sideways.

### Home drawing

Two sites on the black hull, with the link between their gateways. The route in use is buff, and the rest stays quiet. The link is solid in every moment. Where the sheet is narrower than 580px the sites stack, and the controls stay in view above the drawing. A key names the lines.

### Not found

"No page at this address." as a page title with the title mark, the address as a word of code, one line of help, and buttons to the Guide, Operations and Reference.

## Contrast

Every text pair meets WCAG AA at rest: 4.5:1 for text, 3:1 for a mark that is not text, by the WCAG 2 formula from the tokens.

| Use | Pair | Needs | Day | Night |
|---|---|---|---|---|
| Text | `ink` on `plane` | 4.5:1 | 16.95 | 16.57 |
| Text on the deck | `ink` on `deck` | 4.5:1 | 15.38 | 14.99 |
| Secondary text | `soft-ink` on `plane` | 4.5:1 | 6.59 | 8.96 |
| Secondary text on the deck | `soft-ink` on `deck` | 4.5:1 | 5.98 | 8.11 |
| Links, the focus ring on a plane | `link` on `plane` | 4.5:1 | 7.52 | 10.63 |
| Links on the deck, the current page | `link` on `deck` | 4.5:1 | 6.83 | 9.62 |
| Link hover | `link-hover` on `plane` | 4.5:1 | 10.54 | 13.19 |
| Text on the hull | `on-hull` on `hull` | 4.5:1 | 7.49 | 7.49 |
| Secondary text on the hull | `on-hull-soft` on `hull` | 4.5:1 | 5.76 | 5.76 |
| Text in the search box | `on-hull` on `hull-deep` | 4.5:1 | 10.49 | 10.49 |
| Text on buff | `#101a17` on `buff` | 4.5:1 | 9.33 | 9.33 |
| Text on a hovered primary button | `#101a17` on `buff-deep` | 4.5:1 | 7.52 | 7.52 |
| Code | `code-text` on `code-bg` | 4.5:1 | 15.30 | 16.86 |
| Comments in code, the footer | `code-soft` on `code-bg` | 4.5:1 | 7.13 | 7.86 |
| Keywords in code | `code-accent` on `code-bg` | 4.5:1 | 9.33 | 10.29 |
| A table's head | `plane` on `ink`; at night `on-hull` on `hull-deep` | 4.5:1 | 16.95 | 10.49 |
| Text on the band | `on-hull` on `band` | 4.5:1 | 16.87 | 12.01 |
| Secondary text on the band | `band-soft` on `band` | 4.5:1 | 10.82 | 9.24 |
| Buff on the band | `buff` on `band` | 4.5:1 | 9.33 | 6.65 |
| A tip's kind | `plane` on `tip` | 4.5:1 | 5.96 | 8.98 |
| A warning's kind | `plane` on `warning` | 4.5:1 | 5.09 | 8.80 |
| A danger's kind | `plane` on `danger` | 4.5:1 | 6.24 | 8.07 |
| A warning's frame | `warning` on `deck` | 3:1 | 4.62 | 7.96 |
| The active nav item's bar | `buff` on `hull` | 3:1 | 4.14 | 4.14 |
| The edge of a code block | `code-edge` on `plane` | 3:1 | 16.95 | 3.24 |

One pair is below 4.5:1: a nav link on hover is `buff` on `hull`, 4.14:1. It is a hover state of text that reads at 7.49:1 at rest.

## Do's and Don'ts

### Do:

- **Do** take every colour, face and radius from `docs/.vitepress/theme/tokens.css`, by its role.
- **Do** fill whole planes with colour, and keep buff for the one action.
- **Do** put ink text on buff, and `on-hull` text on the hull and the band.
- **Do** give every state its word as well as its colour.
- **Do** keep markings short, and headings in sentence case.
- **Do** keep a table to the page at every width, and check a new page at 390px and at 1440px, by day and at night.
- **Do** write the answer first, in short sentences of everyday English.

### Don't:

- **Don't** add a second accent colour or a second display family.
- **Don't** add a glow, a blur, a gradient beyond the edge of a code block, or a shadow beyond the two listed.
- **Don't** use red for anything but danger.
- **Don't** set text in `line`, or in buff on the hull at rest.
- **Don't** set a table's head in capitals.
- **Don't** set a marking over a heading as decoration.
- **Don't** load a font from another host.
- **Don't** use em-dashes in any copy.
