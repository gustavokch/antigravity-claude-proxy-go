---
name: Signal Box
description: Railway signal-box console for the antigravity-go-proxy. Enamel and ink; lamps are the only colour.
colors:
  ground: "#e3e4dc"
  panel: "#f1f1ea"
  sunk: "#d6d7ce"
  sunk-2: "#cbccc2"
  ink: "#1b1c19"
  ink-2: "#3f413a"
  ink-3: "#52544b"
  rule: "#b4b5ab"
  rule-strong: "#8d8f85"
  plate: "#1b1c19"
  plate-ink: "#f1f1ea"
  track: "#1b1c19"
  track-empty: "#3a3b36"
  lamp-clear: "#fbf6dc"
  lamp-caution: "#e8a20f"
  lamp-danger: "#d0371f"
  lamp-ok: "#2f8a4c"
  lamp-off: "#6f7168"
  lamp-housing: "#1b1c19"
  lamp-unlit: "#55574f"
  danger: "#b02a1a"
  danger-fill: "#c3301c"
  danger-wash: "#f3d9d2"
  caution: "#7a5200"
  caution-wash: "#f4e6bf"
  ok: "#25683a"
  ok-wash: "#d6eadb"
  focus: "#e8a20f"
  selection: "#f2cf6b"
typography:
  display:
    fontFamily: "Barlow Semi Condensed, Barlow, system-ui, sans-serif"
    fontSize: "2.75rem"
    fontWeight: 700
    lineHeight: 1
    letterSpacing: "-0.01em"
  headline:
    fontFamily: "Barlow Semi Condensed, Barlow, system-ui, sans-serif"
    fontSize: "1.375rem"
    fontWeight: 700
    lineHeight: 1.1
    letterSpacing: "0"
  title:
    fontFamily: "Barlow Semi Condensed, Barlow, system-ui, sans-serif"
    fontSize: "1.125rem"
    fontWeight: 700
    lineHeight: 1.15
    letterSpacing: "0"
  subtitle:
    fontFamily: "Barlow Semi Condensed, Barlow, system-ui, sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 700
    lineHeight: 1.2
    letterSpacing: "0"
  body:
    fontFamily: "Barlow, system-ui, -apple-system, Segoe UI, sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 400
    lineHeight: 1.45
    letterSpacing: "normal"
  body-sm:
    fontFamily: "Barlow, system-ui, -apple-system, Segoe UI, sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 400
    lineHeight: 1.45
    letterSpacing: "normal"
  button:
    fontFamily: "Barlow, system-ui, -apple-system, Segoe UI, sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 600
    lineHeight: 1
    letterSpacing: "0.01em"
  label:
    fontFamily: "Barlow Semi Condensed, Barlow, system-ui, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "0.03em"
  plate:
    fontFamily: "Barlow Semi Condensed, Barlow, system-ui, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 700
    lineHeight: 1.15
    letterSpacing: "0.04em"
  tag:
    fontFamily: "Barlow Semi Condensed, Barlow, system-ui, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "0.02em"
  mono:
    fontFamily: "IBM Plex Mono, ui-monospace, SF Mono, Menlo, monospace"
    fontSize: "0.92em"
    fontWeight: 400
    lineHeight: 1.45
    letterSpacing: "normal"
rounded:
  sm: "2px"
  xs: "1px"
  lamp: "50%"
spacing:
  s1: "4px"
  s2: "8px"
  s3: "12px"
  s4: "16px"
  s5: "24px"
  s6: "32px"
  s7: "48px"
components:
  button-primary:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.ground}"
    typography: "{typography.button}"
    rounded: "{rounded.sm}"
    padding: "0 14px"
    height: "36px"
  button-primary-hover:
    backgroundColor: "{colors.ink-2}"
    textColor: "{colors.ground}"
  button-default:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    typography: "{typography.button}"
    rounded: "{rounded.sm}"
    padding: "0 14px"
    height: "36px"
  button-default-hover:
    backgroundColor: "{colors.sunk}"
    textColor: "{colors.ink}"
  button-danger:
    backgroundColor: "{colors.danger-fill}"
    textColor: "#ffffff"
    typography: "{typography.button}"
    rounded: "{rounded.sm}"
    padding: "0 14px"
    height: "36px"
  button-danger-hover:
    backgroundColor: "{colors.danger}"
    textColor: "#ffffff"
  button-ghost:
    backgroundColor: "transparent"
    textColor: "{colors.ink-2}"
    typography: "{typography.button}"
    rounded: "{rounded.sm}"
    padding: "0 14px"
    height: "36px"
  button-ghost-hover:
    backgroundColor: "{colors.sunk}"
    textColor: "{colors.ink}"
  input:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    typography: "{typography.body}"
    rounded: "{rounded.sm}"
    padding: "0 10px"
    height: "36px"
  plate:
    backgroundColor: "{colors.plate}"
    textColor: "{colors.plate-ink}"
    typography: "{typography.plate}"
    rounded: "{rounded.sm}"
    padding: "3px 7px 2px"
  tag-muted:
    backgroundColor: "transparent"
    textColor: "{colors.ink-3}"
    typography: "{typography.tag}"
    rounded: "{rounded.sm}"
    padding: "1px 6px"
  tag-caution:
    backgroundColor: "{colors.caution-wash}"
    textColor: "{colors.caution}"
    typography: "{typography.tag}"
    rounded: "{rounded.sm}"
    padding: "1px 6px"
  tag-danger:
    backgroundColor: "{colors.danger-wash}"
    textColor: "{colors.danger}"
    typography: "{typography.tag}"
    rounded: "{rounded.sm}"
    padding: "1px 6px"
  tag-ok:
    backgroundColor: "{colors.ok-wash}"
    textColor: "{colors.ok}"
    typography: "{typography.tag}"
    rounded: "{rounded.sm}"
    padding: "1px 6px"
---

# Design System: Signal Box

## Overview

**Creative North Star: "The Lit Signal Box"**

The console is a railway signal box that runs a local proxy. The request route is drawn as a lit track diagram across the top bar: client, then proxy, then each gateway in precedence order, with account lamps on each gateway. Views are numbered levers in a frame on the left. Labels are engraved traffolyte plates. The metaphor does work: an exhausted quota is a track occupied in red, a blocked gateway is a danger lamp, and a healthy console reads as unlit enamel with a few clear lamps.

The surfaces are enamel. Day is a pale grey-green box; night is a near-black box. Nothing floats, nothing glows, nothing is glass. Depth comes from 1px hairlines, 1.5px ink borders and tonal steps between ground, panel and sunk enamel, not from shadow. Density is operator-grade: each view has one deciding figure at poster size (44px), everything else sits at reading size, and tables use tabular figures so columns of quota can be compared at a glance.

Confirmed rejections: the dark sidebar with stat cards, provider hues (purple Claude, blue Google, cyan Kimi), gradients, glow or blur on surfaces, and rounded-lg/xl/full corners. Motion is short and rare: lamps and track fills change in 160 to 400 ms, and nothing loops except loading indicators.

**Key Characteristics:**
- Flat enamel surfaces with a 2px radius; no shadow at rest.
- Colour is a lamp: clear, caution, danger, off. Green only marks a completed action.
- Traffolyte plates (Barlow Semi Condensed, capitals) name providers, sources and view levers.
- Track circuits show quota on one fixed 0 to 100% scale; an exhausted pool fills its whole circuit red.
- A route mimic in the top bar shows the request path and the reason each signal shows its aspect.
- Two boxes, one token set: day and night resolve from `data-sb-theme` on `<html>`.

**The Settle Rule.** Lamps and track fills change in 160 to 400 ms with the shared ease curve. Nothing loops except an in-flight spinner and the first-load skeleton.

## Colors

Palette strategy: enamel and ink, with lamps as the only saturated colour. Neutrals carry every surface and all text. Hue appears only as a lamp or a state wash that names a real state, never as a provider identity.

The frontmatter holds the **day box** values; they are normative for tooling. The **night box** values live in the sidecar (`extensions.nightTheme`) under the same token names. Both boxes resolve through `var(--sb-*)`, and components never hard-code hex. The resolved theme is the stored preference (`day`, `night` or `system`), written to `<html data-sb-theme>` before first paint.

### Primary
- **Engraved Ink** (#1b1c19 day / #ecebe2 night, `ink`): all text, track bars, the filled primary button, frame and dialog borders. It is the only accent the console has.
- **Traffolyte Plate** (#1b1c19 / #ecebe2, `plate`) with **Plate Lettering** (#f1f1ea / #151613, `plate-ink`): the client node, lever numbers, route readout, toasts and plate labels.

### Secondary (lamps)
- **Clear Lamp White** (#fbf6dc / #fff3c9, `lamp-clear`): healthy, route set, quota left.
- **Caution Lamp Yellow** (#e8a20f / #f2b630, `lamp-caution`): limited, low, degraded, needs attention soon.
- **Danger Lamp Red** (#d0371f / #ff5a3c, `lamp-danger`): blocked, exhausted, invalid, error, destructive.
- **Completed Lamp Green** (#2f8a4c / #5fbf7d, `lamp-ok`): a completed action only.
- **Off Lamp Grey** (#6f7168 / #4a4b44, `lamp-off`) and **Unlit Glass** (#55574f / #151613, `lamp-unlit`): disabled or not configured. The `lamp-off` class draws Unlit Glass.

### Tertiary (state text and washes)
- **Danger Text** (#b02a1a / #ff7a5c, `danger`), **Danger Fill** (#c3301c / #d23a22, `danger-fill`), **Danger Wash** (#f3d9d2 / #3a1d16, `danger-wash`).
- **Caution Text** (#7a5200 / #f2b630, `caution`), **Caution Wash** (#f4e6bf / #33290f, `caution-wash`).
- **Completed Text** (#25683a / #6cc487, `ok`), **Completed Wash** (#d6eadb / #16301f, `ok-wash`).
- **Focus Amber** (#e8a20f / #f2b630, `focus`), **Selection** (#f2cf6b / #6b5312, `selection`).

### Neutral
- **Enamel Ground** (#e3e4dc / #151613, `ground`): page and main area.
- **Working Panel** (#f1f1ea / #1d1e1a, `panel`): tables, forms, panels, dialogs, controls.
- **Sunk Enamel** (#d6d7ce / #10110e, `sunk`): frame, hover, toolbars, code blocks.
- **Pressed Enamel** (#cbccc2 / #282924, `sunk-2`): pressed buttons and selected rows.
- **Secondary Ink** (#3f413a / #c9c8bc, `ink-2`) and **Label Ink** (#52544b / #a2a195, `ink-3`): body secondary, labels, metadata, placeholders.
- **Hairline** (#b4b5ab / #363731, `rule`) and **Control Grey** (#8d8f85 / #57584f, `rule-strong`): dividers and control borders.
- **Track Bar Ink** (#1b1c19 / #050505, `track`), **Unlit Track Cell** (#3a3b36 / #2c2d28, `track-empty`), **Lamp Housing** (#1b1c19 / #4a4b44, `lamp-housing`).

Measured contrast (WCAG relative luminance, from these values): Engraved Ink on Working Panel 15.1:1 day and 14.0:1 night; Label Ink on Working Panel 6.7:1 day and 6.4:1 night; Label Ink on Pressed Enamel 4.75:1 day; Danger Text on Working Panel 5.8:1 day; Caution Text on Working Panel 6.1:1 day; Completed Text on Working Panel 5.9:1 day.

### Chart series
Chart.js reads `--sb-chart-1` to `--sb-chart-8` at runtime. These are an ink ramp (#1b1c19 to #a3a59a day), never a hue; series that share a level are told apart by dash pattern and legend. Grid is `chart-grid` (#c4c5bb day, #2e2f2a night); ticks are `chart-tick`.

### Named Rules
**The Lamp-Only Colour Rule.** Hue appears only as a lamp or a state wash that names a real state. Everything else is ink on enamel. Providers, families and brands are told apart by plates and text, never by colour.

**The Green-Means-Done Rule.** Green (`lamp-ok`, `ok`, `ok-wash`) marks a completed action such as a saved setting or a passed test. Healthy is clear white, not green.

## Typography

**Display Font:** Barlow Semi Condensed (fallback Barlow, system-ui, sans-serif). Plates, headings, labels, the deciding figure.
**Body Font:** Barlow (fallback system-ui, -apple-system, Segoe UI, sans-serif). Set on `body`.
**Label/Mono Font:** IBM Plex Mono (fallback ui-monospace, SF Mono, Menlo, monospace). Code-like data only.

Files are served from `fonts/`: Barlow 400, 500 and 600 (the 600 file covers 600 to 700); Barlow Semi Condensed 600 (covers 500 to 600) and 700 (covers 700 to 800); IBM Plex Mono 400 and 500 (covers 500 to 700). Type sizes: 12, 13, 15, 18, 22 and 44px (`--sb-text-xs` to `--sb-text-fig`).

**Character:** a signal-box engraving voice. Condensed capitals for anything that names or labels, a plain grotesque for sentences and values, and monospace only where the operator copies or compares a value.

### Hierarchy
- **Figure** (Barlow Semi Condensed 700, 44px, line-height 1, tabular numerals, -0.01em): the one deciding number of a view. 36px under 720px. `.figure`; `.figure-md` is 26px for secondary numbers. The unit beside it is 0.45em in Label Ink.
- **Headline** (Barlow Semi Condensed 700, 22px, line-height 1.1): view titles, `h1.view-title`.
- **Title** (Barlow Semi Condensed 700, 18px, line-height 1.15): section titles, `h2.section-title`, empty-state titles.
- **Subtitle** (Barlow Semi Condensed 700, 15px, line-height 1.2): panel titles, `h3.panel-title`.
- **Body** (Barlow 400, 15px, line-height 1.45, max 72ch on paragraphs): sentences and row descriptions.
- **Body small** (Barlow 400, 13px): tables, field text, view subtitles, section descriptions, notices and buttons (buttons are 600).
- **Label** (Barlow Semi Condensed 600, 12px, letter-spacing 0.03em, uppercase, Label Ink): table headers, field keys, readout keys.
- **Plate** (Barlow Semi Condensed 700, 12px, letter-spacing 0.04em, uppercase, line-height 1.15): `.plate`, tags use the same family at 600 without capitals.
- **Mono** (IBM Plex Mono, 0.92em of the parent): IDs, emails, URLs, keys, log lines, JSON, `kbd`, `pre.codeblock` at 13px. Tables and values use tabular figures (`.tnum`, `.num`).

### Named Rules
**The Deciding Figure Rule.** Each view has exactly one `.figure`. Everything else is reading size.

**The Plate Is For Names Rule.** Plate capitals name a provider, a source, a stable short code, a lever, or a state word in a tag. Plates are not for sentences and are not for every label.

**The Mono Is Data Rule.** IBM Plex Mono is for values the operator copies or compares. It is never used for prose or for field labels, except code-like names such as environment variables.

## Layout

The shell is one CSS grid. The top bar is 64px (`--sb-top-h`) with a 2px ink bottom rule. The lever frame is 208px (`--sb-frame-w`) on sunk enamel. The main area fills the rest and scrolls on its own. The top bar holds the brand cell (the frame's width), the route strip (`.mimic`, flexible, scrolls sideways with a hidden scrollbar), and two icon actions at the right.

The view container is `.view`: max width 1480px, padding 24px 32px 48px. Views stack a head (`.view-head`: title and subtitle, actions at right, 1px hairline below, 24px margin), then `.section` blocks separated by 32px. Section heads use 8px and 16px gaps; panel head and foot are 12px by 16px, panel body is 16px, rows are 12px by 16px. Tables use 10px by 12px cells, 6px by 10px in `.sb-table-dense`.

Spacing scale (`--sb-s1` to `--sb-s7`): 4, 8, 12, 16, 24, 32, 48px. Button heights are 36px default, 32px `.btn-sm`, 28px `.btn-xs`, 44px `.btn-lg`. On coarse pointers every button is at least 44px tall.

**Responsive breakpoints (as built):**
- Above 1100px: full lever frame with labels, brand text visible.
- At or below 1100px: the frame becomes a 72px rail. Labels, handles and brand text hide; numbers sit in centred levers; the lever lamp moves to the top-right corner.
- At or below 900px: `.grid-2` and `.grid-3` collapse to one column.
- At or below 720px: top bar 56px; the frame is removed and the shell becomes one column with a five-lever bottom bar (`.sb-frame-mobile`, five equal columns, 56px minimum height, safe-area inset). Route segments shrink to 14px and node labels hide, leaving lamps and numbers. `.view` padding becomes 16px 16px 32px and `.figure` 36px. Toasts sit 72px above the bottom and span the width.
- At or below 640px: modals take the viewport width less 16px; `.view-full` padding 16px by 12px; the dashboard quota row becomes two columns; the trend chart is 180px tall.
- At or below 520px: log lines stack the timestamp into a 4.75rem column.
- Column visibility: secondary table columns (`.acc-second`, `.mdl-second`, `.setm-md-up`, `.set-cloud-wide`) show only at 768px and up. Settings controls take fixed widths at 720px and up (range 240px, select 220px) and the settings grid goes to two columns.
- Tables always sit inside `.table-wrap`, which scrolls sideways. Table headers stick to the top of the scroll area.

Grid helpers in `signalbox.css`: `.stack` (16px), `.stack-sm` (8px), `.stack-lg` (32px), `.cluster` (wrapping row, 8px), `.cluster-lg` (16px), `.split`, `.grid-2`, `.grid-3`, `.grid-auto` (minimum column 260px), `.rule`, `.grow`.

### Named Rules
**The Frozen Utility Rule.** `css/style.css` is a frozen Tailwind/DaisyUI build with no config. Markup may use only its layout utilities (display, flex, grid, gap, padding, margin, width, height, overflow, hidden, and the responsive variants that exist in that file). Every colour, type and component rule is hand-written in `css/signalbox.css` against `var(--sb-*)`. A class missing from `style.css` does nothing, so check it exists before using it. The legacy colour and component rules in `style.css` (`btn-action-*`, `standard-table`, `status-pill-*`, `bg-space-*`, `text-neon-*`) are not to be used in markup.

## Elevation & Depth

Flat by default. Surfaces sit on the page through tonal steps (ground, panel, sunk), 1px hairlines and 1.5px ink borders on controls and dialogs. No resting element carries a shadow. Shadows appear only on layers that float above the page: modal boxes, menus, toasts and the route readout. Focus is shown with a ring, not a shadow.

### Shadow Vocabulary
- **Floating layer** (`box-shadow: 0 18px 48px -12px rgba(27,28,25,0.35), 0 2px 6px rgba(27,28,25,0.12)` day; `0 18px 48px -12px rgba(0,0,0,0.7), 0 2px 6px rgba(0,0,0,0.5)` night): modal boxes, `.menu` and `.sb-menu`, `.sb-toast`, `.mimic-readout`. The single token is `--sb-shadow-dialog`.
- **Focus ring** (`box-shadow: 0 0 0 4px var(--sb-ink)` day, `0 0 0 4px var(--sb-sunk)` night, with a 2px focus outline at 2px offset): `:focus-visible` only.
- **Inset rules** (`inset 0 0 0 1px` on plates, tags, lever numbers, route nodes; `inset 0 1px 0` hairlines on the current lever): drawn outlines, not shadows.

### Named Rules
**The Flat-At-Rest Rule.** Surfaces carry no shadow at rest. Lift is a hairline or an ink border. A shadow appears only on a layer above the page.

**The Device Gradient Rule.** Surfaces, buttons and text never use gradients. Gradients are allowed only to draw a device: a lamp's lit lens (radial), the 5% graduation marks on a track (repeating linear), the dotted off-segment in the route, and the two chevrons of a select.

## Shapes

Every control, panel, plate, tag, notice, dialog, toast and menu uses a 2px radius (`--sb-radius`). Small inner parts use 1px: track bars, checkboxes, the toggle knob, plate numbers, lever handles, route numbers, skeletons, slider thumbs. Lamps and radio dots are circles (50%). There are no rounded-lg, rounded-xl or rounded-full corners.

Borders encode role. 1px Hairline (`rule`) for dividers and panel edges. 1.5px Engraved Ink for buttons, segmented controls, route nodes, dialogs and table header rules. 1px Control Grey for inputs, notices and tags. A 2px ink rule closes the top bar. Dashed borders always mean disabled.

The geometry is rectilinear: rectangles, straight rules, bars and a pivoted lever handle. Circles are reserved for lamps.

## Components

Each component below is a short character line followed by its shape, colour assignment and states. Sidecar snippets for the main primitives are in `.impeccable/design.json`.

### Buttons
- **Shape:** 2px radius, 1.5px Engraved Ink border, 36px tall (`.btn-sm` 32px, `.btn-xs` 28px, `.btn-lg` 44px), padding 0 14px, 13px Barlow 600. Icon-only `.btn-icon` is square at the same height and needs an `aria-label`.
- **Default (`.btn`):** Working Panel fill, Engraved Ink text. Hover: Sunk Enamel fill, ink border. Active: Pressed Enamel fill.
- **Primary (`.btn-primary`):** Engraved Ink fill, Enamel Ground text; hover Secondary Ink. One per panel or dialog.
- **Danger (`.btn-danger`):** Danger Fill with white text (#fff, literal); hover Danger Text. Only for a confirm-destructive action.
- **Quiet destructive (`.btn-danger-quiet`):** transparent, Control Grey border, Danger Text; hover Danger Wash.
- **Ghost (`.btn-ghost`):** no border, Secondary Ink text; hover Sunk Enamel and Engraved Ink text.
- **Focus:** `:focus-visible` shows the 2px focus outline at 2px offset.
- **Disabled:** dashed Hairline border, Label Ink text, transparent fill, not-allowed cursor.
- **Busy:** `aria-busy="true"` blocks pointer events; an inline `.spinner` shows while the request is in flight.
- **Groups (`.join`):** adjacent buttons share one border (-1.5px overlap) with square corners.
- **Transitions:** background, colour and border colour over 160ms.

### Tags
- **Style:** outlined by default: 1px Control Grey border, transparent fill, Secondary Ink text, 20px minimum height, padding 1px 6px, 12px Barlow Semi Condensed 600.
- **Variants:** `tag-ink` (plate face), `tag-danger` (Danger Wash, Danger Text), `tag-caution` (Caution Wash, Caution Text), `tag-ok` (Completed Wash, Completed Text, completed actions only), `tag-muted` (Label Ink text, Hairline border).
- **Use:** status words beside a lamp or a count, for example `tag-danger` for a dead gateway or `tag-caution` for a cooldown.

### Plates
- **Style:** Traffolyte plate: Engraved Ink face, Plate Lettering text, 12px capitals in Barlow Semi Condensed 700, padding 3px 7px 2px, 2px radius.
- **Variants:** `plate-outline` (transparent face, ink inset ring), `plate-num` (centred, minimum 1.6em, for lever and gateway numbers), `plate-danger` (Danger Fill, white text), `plate-caution` (Lamp Caution Yellow face with near-black text).
- **Use:** provider and source names (family, gateway), lever numbers, stable short codes.

### Lamps
- **Shape:** a 12px circle, lit lens in the lamp colour inside a Lamp Housing ring (16px as `.lamp-lg`; 8px in route nodes). The lens is what makes white read as lit on pale enamel.
- **Aspects:** `lamp-clear`, `lamp-caution`, `lamp-danger`, `lamp-ok`, `lamp-off` (Unlit Glass).
- **Placement:** always beside text that names the state. Colour is never the only signal.
- **Motion:** background colour over 160ms.

### Track Circuit (quota bar)
- **Shape:** 8px tall flat bar (`.track-sm` 5px in tables, `.track-lg` 12px for a view's main quota), 1.5px Track Bar Ink border, 1px radius, unlit cells in Unlit Track Cell, a graduation mark every 5%.
- **Fill:** `--fill` percent. `data-level` selects the cell colour: clear (default), caution, danger, off. `data-empty="true"` with danger fills the whole circuit red ("track occupied").
- **Threshold marker:** `.track-mark`, a 2px Lamp Caution Yellow post overhanging the bar by 4px at the top and bottom.
- **Level rules as built:**
  - Remaining quota (dashboard family circuits, models, accounts pools): danger at 0%, caution below 20%, else clear.
  - Used utilisation (accounts reset windows): danger above 80%, caution above 50%, else clear.
  - Health score (accounts health inspector, a separate metric): clear above 70, caution above 50, else danger.
- **Pairing:** every circuit carries its number as text (`42%`) and its reset time.
- **Motion:** fill slides over 400ms; cell colour over 160ms.

### Route Mimic (signature, top bar)
- **Shape:** a horizontal line of nodes: Client (plate face, non-interactive), Proxy (aspect from connection: connected is clear, connecting is caution, otherwise danger; focusable with `role="status"`), then one node per gateway in effective precedence order.
- **Node:** 32px tall, 1.5px Engraved Ink border, 2px radius, Working Panel fill, 13px plate capitals, a plate-capital precedence number, the gateway label, and up to eight account lamps in a track-coloured housing with `+n` overflow.
- **Segments:** 28px by 4px track bar between nodes (14px under 720px). An off gateway draws a 2px grey dotted segment.
- **States:** `data-state="off"` gives a dashed Control Grey border and Label Ink text. `data-state="danger"` gives a Lamp Danger Red border with an inset ring. Clear and caution keep the ink border; their lamps carry the signal.
- **Readout:** hovering or focusing a node opens a fixed plate-face readout (max 320px, floating shadow) with the gateway's title and reason lines. The node's aria-label carries the same text.
- **Interaction:** a gateway node is a button that opens its view on click.
- **Aspect rules as built:**
  - Account: disabled is off; status ok is clear; invalid is danger; any other status (rate limited, cooldown) is caution.
  - Gateway, pooled accounts: none enabled or none ready is danger; some ready is caution; all ready is clear.
  - Keyed gateway: disabled is off; missing key is danger; otherwise clear.
  - OpenRouter balance: under $1 is danger, under $5 is caution.
  - Custom endpoints: none is off; otherwise clear.

### Lever Frame (signature, primary navigation)
- **Shape:** a 208px frame on Sunk Enamel. Each lever is 44px minimum height with 16px side padding and holds a handle, a number plate and a label in Barlow 600 15px.
- **Handle:** a 10px by 24px bar on a pivot, leaning back 22 degrees when off and forward 22 degrees in ink when pulled.
- **Number:** a 24px by 22px plate with a 1px inset ring; keys 1 to 5 select the views.
- **Pulled (`aria-current="page"`):** Working Panel fill, hairlines above and below, the number as an engraved plate, and a 1px panel cut on the right edge joining the lever to the page.
- **Hover:** Pressed Enamel fill.
- **Groups:** views (Dashboard, Models, Accounts) and tools (Logs, Settings) separated by a hairline.
- **Motion:** handle angle and colours over 160ms.

### Inputs and Fields
- **Field:** a column of label (`.field-label`, 13px 600 Engraved Ink), control, then help (`.field-help`, 12px Label Ink) or error (`.field-error`, Danger Text).
- **Style:** 36px tall (`.input-sm` 32px, `.input-xs` 28px), 1px Control Grey border, 2px radius, Working Panel fill, 15px Barlow (13px in the small sizes), padding 0 10px. Mono for IDs, keys and URLs (`.input.mono`, 13px).
- **Hover:** border Label Ink.
- **Focus:** border Engraved Ink with a 2px Focus Amber outline at 0 offset.
- **Disabled:** Sunk Enamel fill, dashed Hairline border, Label Ink text.
- **Invalid:** the error text names the problem; a field whose pattern does not compile takes a Danger border.
- **Textarea:** minimum 80px, 8px by 10px padding.
- **Select:** 30px right padding; two 5px ink chevrons drawn with gradients (a device, see the Device Gradient Rule).
- **Search:** `.sb-search` places a 16px icon 9px in from the left and pads the input 32px.
- **Toggle (`.toggle`, `.sb-switch`):** a 40px by 22px lever switch with a 1.5px Label Ink border on Sunk Enamel. The 15px square knob is Label Ink when off and slides 18px to Plate Lettering on Engraved Ink when on. `.toggle-sm` is 34px by 20px.
- **Checkbox (`.checkbox`):** 18px square, 1px radius, Engraved Ink fill and an enamel tick when checked. Indeterminate is a bar.
- **Radio:** 18px circle, Engraved Ink dot with a Working Panel ring when checked.
- **Range (`.range`):** 4px Engraved Ink track with a 12px by 22px Working Panel thumb and 2px ink border.
- **Coarse pointers:** toggles gain a 10px focus offset.

### Segmented Control and Tabs
- **Segmented (`.seg`):** an inline group with a 1.5px Engraved Ink outline and 2px radius; segments split by Control Grey rules. Selected (`aria-pressed="true"`, `aria-selected`, or `.is-on`) is Engraved Ink fill with Enamel Ground text. Used for filters.
- **Tabs (`.sb-tabs`, `.sb-tab`):** 42px tall, 15px Barlow 600, Label Ink when idle, Engraved Ink when selected with a 2px underline bar inset 8px. `.tab-count` is an 11px counter that inverts when selected. Content below a view-level tab bar starts 24px lower.

### Notices
- **Style:** a flex row: a lamp of the matching state, a body (title 600 and a paragraph) that grows, and an optional action button at the end. 1px Control Grey border, 2px radius, Working Panel fill, 13px text.
- **Variants:** `notice-danger` (Danger Wash, Danger Text border), `notice-caution` (Caution Wash, Caution Text border), `notice-ok` (Completed Wash, Completed Text border), `notice-info` (Working Panel, Control Grey border).
- **Placement:** trouble notices render above the quota circuits on the Dashboard and sit at the top of their view. Role `status`.

### Panels, Rows and Readouts
- **Panel (`.panel`):** Working Panel fill, 1px Hairline border, 2px radius. `.panel-head` (12px by 16px, hairline below), `.panel-body` (16px), `.panel-foot` (12px by 16px, hairline above). Never nest a panel in a panel.
- **Rows (`.rows`, `.row`):** a panel ruled into rows with 12px by 16px padding; text (`.row-text`, title 600 and `.row-desc` 13px Label Ink) on the left, control on the right.
- **Readout (`.readout`):** a grid of key and value cells with hairline dividers. `.readout-key` is a label; `.readout-val` is an 18px plate-face figure.
- **Disclosure (`details.disclosure`):** a 1px hairline frame, a summary with a chevron that turns 90 degrees when open, a 16px body.

### Tables
- **Shape:** `.table-wrap` (horizontal scroll) around `.sb-table` (13px, tabular numerals). Headers are Label style with a 1.5px Engraved Ink underline and sticky top. Rows are divided by Hairline. Hover tints the row with Sunk Enamel at 55%.
- **Numeric cells:** `.num` is right aligned with tabular numerals.
- **Row states:** `tr.is-blocked` washes the cells with Danger Wash at 70%. `tr.is-off` sets Label Ink text and dims its track to 55% opacity.
- **Columns:** the columns shown are the ones the operator's decision needs. Secondary columns hide below 768px.

### Dialogs
- **Scrim:** `dialog.modal` covers the page with `--sb-scrim` (27,28,25 at 45% day; black at 60% night), no blur.
- **Box:** `.modal-box`, Working Panel fill, 1.5px Engraved Ink border, 2px radius, floating shadow, maximum height `100dvh - 48px` with internal scrolling.
- **Parts:** `.modal-head` (title and an icon close button with `aria-label`), `.modal-body` (24px padding), `.modal-action` (right-aligned, wraps).
- **Small screens:** under 640px the box is the viewport width less 16px.

### Menus, Tooltips and Toasts
- **Menu (`.menu`, `.sb-menu`):** Working Panel, 1.5px Engraved Ink border, 2px radius, floating shadow, 4px padding. Items have 1px radius and Sunk Enamel hover.
- **Tooltip (`.tooltip`):** Plate face, 12px text, maximum 280px.
- **Toast (`.sb-toast`):** bottom right, Plate face and Plate Lettering text, a lamp in a plate-coloured housing, 280px to 440px wide, floating shadow. Enters over 200ms and leaves over 150ms. Under 720px it spans the width above the lever bar.

### Empty, Loading and Error States
- **Empty (`.empty`):** a left-aligned column with 32px by 24px padding: `.empty-title` (18px plate face), `.empty-desc` (13px Secondary Ink, maximum 60ch), `.empty-actions` (one action that fills the gap). The description says why the view is empty.
- **Loading:** `.skeleton` is a Sunk Enamel block with 1px radius that pulses opacity between 1 and 0.55 over 1.4s on first load only. `.spinner` is a 14px ring in `currentColor` turning over 0.8s, shown only while a request is in flight.
- **Error:** a `notice-danger` with a danger lamp, stating what failed and what to do.
- **Reduced motion:** transitions and animations are cut to one near-instant step. The spinner stops and holds at 60% opacity.

### Code and Keys
- **Code block (`pre.codeblock`):** Sunk Enamel fill, 1px Hairline border, 2px radius, 13px mono, horizontal scroll.
- **Key (`kbd`):** 20px minimum height, bordered, mono.

### Named Rules
**The Calm-At-Rest Rule.** A healthy row is plain ink. Trouble gets a lamp, a tag and a wash, and where the view supports sorting it sorts first.

**The Lamp Beside Words Rule.** A lamp always sits next to text that names the state. Colour is never the only carrier of meaning.

## Do's and Don'ts

### Do:
- **Do** route every colour through a token: `var(--sb-*)`. Known literals that remain: `#fff` on Danger Fill (buttons and `plate-danger`), `#b22d18` for the night danger-button hover, and `#1b1c19` on the caution plate.
- **Do** pair each lamp or tag with the word that names its state.
- **Do** give each view one `.figure` at 44px and keep everything else at reading size.
- **Do** keep healthy rows plain ink; reserve `tr.is-blocked`, danger tags and danger lamps for trouble.
- **Do** express every quota as a `.track` on the fixed 0 to 100% scale, with its percentage and reset time as text.
- **Do** use a 2px radius, 1px hairlines for dividers, 1.5px ink borders for controls, and dashed borders for disabled.
- **Do** keep motion between 160ms and 400ms, and respect `prefers-reduced-motion` (already global).
- **Do** wrap any table that can overflow in `.table-wrap`.
- **Do** draw icons from the outline SVG set: 24px viewBox, 2px stroke, `currentColor`.
- **Do** check that a layout class exists in `style.css` before using it; put all other styling in `signalbox.css`.

### Don't:
- **Don't** add a gradient, glow, blur or decorative shadow to a surface. Gradients are for lamp lenses, track graduations, the route's off-segment and the select chevrons only.
- **Don't** colour a family, provider or brand. Use a plate and text.
- **Don't** use green for a healthy state. Green marks a completed action only.
- **Don't** nest a `.panel` inside a `.panel`.
- **Don't** set prose or field labels in mono. Mono is for IDs, emails, URLs, keys, log lines and JSON.
- **Don't** use Tailwind colour classes, DaisyUI colour modifiers, `rounded-lg`, `rounded-xl`, `rounded-full`, `shadow-*`, `backdrop-blur`, or any `bg-space-*`, `text-neon-*`, `text-gray-*` or `text-white` class in markup or in JS strings that build classes.
- **Don't** add a hue to tell chart series apart. Use the ink ramp and dash patterns.
- **Don't** hand-write a colour or type value in a view. If a value is missing, add a token to `css/signalbox.css` in both boxes.
