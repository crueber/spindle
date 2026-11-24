# PLAN-THEMES — a first-class theme system for the spindle UI

> Status: **plan, not implementation.** Written 2026-11-21 against `main` (`17158e0`).
> Scope: 5 new selectable themes (2 high-contrast, 3 expressive) on the existing
> Precision Dark foundation, the token contract that makes them possible, the
> selection/persistence plumbing, and the verification gate. No code in this
> change; every value below is measured, not asserted.
>
> Ground truth read for this plan: [`web/src/app.css`](../web/src/app.css)
> (the `--pd-*` blocks, `@theme` ramp retunes, the `.dark` class variant),
> [`internal/webui/templates/base.html`](../internal/webui/templates/base.html)
> (`<html lang="en" class="dark">` — no bootstrap script),
> [`internal/webui/templates/partials/header.html`](../internal/webui/templates/partials/header.html)
> (the `data-theme-toggle` button),
> [`web/src/islands/theme.js`](../web/src/islands/theme.js) (the toggle + its
> localStorage persistence), [`docs/style-guideline.md`](style-guideline.md) §1
> (normative palette), [`docs/AUDIT-V1.md`](AUDIT-V1.md) (amber/emerald
> semantics), `AGENTS.md` law 1 + the Tailwind-only rule.
> The contrast method below reproduces the ratios documented in
> `app.css`/§1 F2 to within rounding (they were measured against *surface*;
> this plan gates on the *worse* of canvas/surface and says which).
>
> **Revised 2026-11-21** after the independent review
> (`/var/tmp/themes-review/review.md`, re-runnable audit scripts alongside),
> which recomputed all 231 gate-matrix pairs across the seven themes with
> **zero contrast failures** and found the implementation holes folded in
> here: the Go-emitted check-dot path, two dead tokens, three ramp-table
> bugs, missing conversions, and six factual corrections. Palette values are
> unchanged except three doc-level ramp fixes in §5.1 (a swapped terminal
> step, an added hc-light step, a dropped inert hc-dark step).

---

## 0. Recommendation up front

- **Add 5 themes** — `hc-dark`, `hc-light` (higher-contrast variants of the two
  defaults) and `paper`, `ocean`, `terminal` (expressive). That is the
  4–6-theme ask (5 new selectable themes); the menu offers them **plus the
  existing `dark`/`light`** (which must stay selectable so an expressive user
  can come home) and a `match system` entry — 7 theme ids in all.
- **Default stays `dark`** — the product's identity (guideline §1 F1); a
  first-visit user with no stored choice and no OS preference gets dark.
  A user whose OS *declares* a preference gets that side (§7).
- **Order:** token contract + conversions + the contrast gate test (W1),
  selection plumbing (W2), the five palettes (W3: **hc-dark and hc-light
  first — need before mood — then ocean, paper, terminal**), docs + rendered
  proof (W4). ≈ 2.5 dev-days total; detail in §10.
- **The current toggle does persist** (`localStorage["spindle-theme"]`), but it
  has a **flash of the wrong theme**: `base.html` ships `class="dark"` and the
  module that applies the stored choice executes after first paint. The plan
  folds the fix (§7.3). It also found **measured AA failures in the current
  themes** — six rows' worth in light, one in dark (§2) — folded fixes, not
  new-theme work.
- **W1 input (explicit dependency):** the parallel accessibility pass will
  hand over a measured contrast list
  (`/var/tmp/wt-a11y/a11y-contrast-findings.md`); it had **not been written
  when this plan or its review finished**. When it lands, its rows are folded
  into the §2 baseline and the §8.4 inventory *before any palette value is
  finalised* — anything it lists beyond §2 arrives as W1 conversions the same
  way the review's tree findings did. The plan does not silently depend on
  that file: §2 is complete and self-consistent without it.

---

## 1. The token contract

### 1.1 What exists today

`web/src/app.css` defines the semantic roles as CSS custom properties —
`:root` = light, `.dark` = dark (lines 83–132) — plus a second family of
graph-lane variables (`--graph-0..7`, `--graph-ring`, lines 655–676) and the
`@theme` retunes of the raw Tailwind `zinc`/`emerald`/`amber` ramps
(lines 16–78) that the ~50 inline ramp utilities in templates inherit.

| Token | Role |
|---|---|
| `--pd-canvas` | page background |
| `--pd-surface` | panes, cards, popover ground |
| `--pd-raised` / `--pd-raised-2` | the two elevation steps (inputs, hover rows / pressed rows) |
| `--pd-line` / `--pd-line-strong` | hairline border / control border |
| `--pd-text` / `--pd-text-2` / `--pd-muted` / `--pd-dim` | primary, secondary, metadata, decorative-only |
| `--pd-accent` / `--pd-accent-hover` / `--pd-on-accent` | action: primary button, focus ring, active branch |
| `--pd-ok` / `--pd-warn` / `--pd-danger` | state: open-passing / draft-degraded / closed-failing |
| `--pd-add-bg` `--pd-add-fg` `--pd-del-bg` `--pd-del-fg` | diff rows |
| `--pd-sel` / `--pd-sel-edge` | blob/diff line selection |
| `--pd-shadow` | popover elevation |

### 1.2 What is missing for a first-class theme system

| Gap | Evidence | Token to add |
|---|---|---|
| **Merged / prerelease chip colours live outside the contract.** `.chip-merged` / `.chip-prerelease` (app.css 310–311) use the *default* Tailwind `--color-violet-400` / `--color-sky-400`, which `@theme` never retunes — a theme cannot reach them. | (measured failures in §2) | `--pd-merged`, `--pd-merged-bg`, `--pd-prerelease`, `--pd-prerelease-bg` |
| **Chip grounds are computed, not tokens.** Chips tint their ground with `color-mix(in srgb, var(--pd-ok) 16%, transparent)` — a theme cannot raise tint strength for high contrast, and the ground is untestable. | app.css 307–313 | `--pd-ok-bg`, `--pd-warn-bg`, `--pd-danger-bg` (solid; replace the five `color-mix` grounds) |
| **Syntax keyword colour is a raw ramp.** `.tok-kw` uses `--color-violet-400` (app.css 559); strings/numbers/comments correctly reuse `--pd-ok`/`--pd-accent`/`--pd-muted`. | measured 2.72:1 on light code blocks (§2) | `--pd-kw` |
| **Code background is implicit.** `markdown pre` / `.code-view` use `--pd-canvas`, `markdown code` uses `--pd-raised` — no seam a theme could specialise. | — | `--pd-code-bg` (defaults to `--pd-canvas` per theme) |
| **Link colour has no seam.** `.link` and markdown anchors are `--pd-text` + underline (correct per F3); a theme may never need more, but the rule should be spelled as a token, not an inline choice. | — | `--pd-link` (set to the theme's text value in every shipped theme; underline stays the differentiator) |
| **The setup callout is raw `sky-*`.** `.setup-callout` (app.css 341) is the one cool-blue panel; on `paper` it would fight the warm character. | — | `--pd-info-bg` (border becomes `--pd-line-strong`; body text unchanged) |
| **State markers bypass tokens entirely.** The notification unread edge/dot use `border-l-emerald-500` / `bg-emerald-500` unpaired across themes (`pages/public/notifications.html` 31, 42); check dots use `bg-emerald-500 / bg-red-500 / bg-red-700 / bg-amber-400 / bg-zinc-400…` (`partials/collab_checks.html` 5). | — | no new tokens: convert to `--pd-ok` / `--pd-danger` / `--pd-warn` / muted (§8.4) |
| **Focus ring** is `--pd-accent` by rule (`@layer base :focus-visible`). Keep it a rule, not a token: the ring IS the action accent in every theme, so ring contrast is gated as accent-vs-canvas (§9). | — | (none) |
| Elevation steps, border, muted, diff add/remove, selection **already exist** and need no new tokens. | — | — |

### 1.3 The rules (binding for every theme)

1. **A theme may only set tokens.** A theme block sets the complete
   `--pd-*` set (completeness asserted by the gate test) and overrides
   `--color-*` ramp variables. No theme may carry a component rule, a
   `dark:`-variant exception, a per-page override, or a hex literal outside its
   own block + the swatch partial (§7.4). "Change the shape and delete the old
   shape" (AGENTS preamble) applies: no theme aliases.
2. **The `--pd-*` variables are the only colour references components may use**
   (guideline F2, unchanged). The ramp variables exist so the ~50 existing
   inline utilities follow the theme without per-template edits — the built
   stylesheet already compiles them to variable references
   (`.bg-zinc-100{background-color:var(--color-zinc-100)}` in `web/dist/app.css`),
   so a later `[data-theme]` block wins by source order at equal specificity.
3. **Amber means action survives re-huing.** The accent may take a theme's hue
   (paper: terracotta; ocean: azure), but it may appear *only* where action
   appears today: `.btn.primary`, `.btn-active` border/text, `:focus-visible`
   outline, `.input:focus` ring, `.brand::before` (the one sanctioned
   decorative mark, guideline §2), active tab edge, selection edge, checkbox
   `accent-color`, `tok-num`. A theme must not paint navigation, links, chips,
   or decoration with it. `--pd-warn` stays the amber-family *state* (draft,
   degraded) in every theme — accent and warn being cousins is the status quo
   (dark `#ffb224` vs `#e0a95e` today) and is covered by the redundancy rules (§6).
4. **Green is state, in every theme.** `--pd-ok` keeps the emerald family
   everywhere (cross-theme muscle memory: open/passing/diff-add is green on
   every git host). This is why **terminal keeps the amber accent** instead of
   a phosphor-green one: a green action accent would collide with the ok/state
   green that sits beside it on every issue and PR page. The terminal
   character comes from the world (green-cast canvas, phosphor-grey text), not
   from repainting the action (§3, `terminal` row).
5. **Links are ink** (`--pd-link` = `--pd-text` in all shipped themes) with the
   existing 35–40 % underline that solidifies on hover (F3). No theme makes
   links the accent.
6. **The state→role vocabulary is fixed** (guideline §6): ok = open/passing,
   danger = closed/failing, merged = merged, warn = draft/degraded,
   prerelease = pre-release. Themes retune *values*, never reassign roles.

---

## 2. Honest gaps found in the current palettes (measured, folded into W1)

Measured with the WCAG 2.x relative-luminance formula (§9.1); the method
reproduces the documented pairs (dark text 15.63:1 vs the doc's 15.6; light
on-accent 5.43 vs 5.4; the doc's text-2/muted figures are against *surface* —
7.8/4.6 reproduce as 7.80/4.62 there). An independent review (2026-11-21)
recomputed every number in this section exactly, plus the 231-pair gate matrix
with zero failures — the tables below are twice-measured. Rows the parallel
a11y pass adds when its findings file lands (§0) fold in here the same way.

| # | Finding | Measured | Fix (folded) |
|---|---|---|---|
| 1 | **Light `chip-merged`**: default `violet-400 #a78bfa` as 10 px chip text on its tint | **2.19:1** | new tokens `--pd-merged #6d28d9` / `--pd-merged-bg #e7e1f4` → **5.57:1** |
| 2 | **Light `chip-prerelease`**: `sky-400 #38bdf8` on its tint | **1.76:1** | `--pd-prerelease #0c6291` / bg `#d8e9f0` → **5.31:1** |
| 3 | **Light syntax keywords**: `.tok-kw` violet-400 on white code blocks | **2.72:1** | `--pd-kw #6d28d9` → **7.10:1** |
| 4 | Light `chip-open` / `chip-draft` on their tints | 4.34 / 4.30:1 (10 px text, AA needs 4.5) | grounds lightened: `--pd-ok-bg #dbe8e2` → **4.62**, `--pd-warn-bg #eee2ca` → **4.61** |
| 5 | **Light `muted` on cards** (`--pd-muted` is used on `--pd-surface`: table headers, card-meta) | 4.45:1 | `--pd-muted #6a6f74` → **4.73** (white: 5.07) |
| 6 | **Dark 11 px setup notes**: `dark:text-zinc-500` is the *light* theme's muted value on the dark canvas | **3.96:1** | dark ramp step zinc-500 → `#79838b` → **4.89:1** |
| 7 | **Light 11 px setup notes**: `text-zinc-400 #9aa0a6` on white | **2.64:1** | light ramp step zinc-400 → `#6a6f74` → **5.07:1** |
| 8 | Non-text UI boundaries (WCAG 1.4.11, 3:1): control border `--pd-line-strong` on canvas | dark **1.63:1**, light **1.56:1** | *Documented deviation for the two defaults* — the hairline control border is the Precision Dark identity and the button is identified by its label; the HC themes gate this at 3:1 (§4.3/§4.4 measure **3.30/3.25**). Recorded, not silently kept. |

Also recorded: `--pd-dim` carries blob/diff gutter numbers (clickable anchors,
11.5 px) at dark **2.61:1** / light 3.04:1 — decorative-by-intent today, gated
at 4.5:1 in the HC themes (they measure 4.99/5.04) and stated as a deviation
in the defaults.

---

## 3. The theme set

| id | side | character | why it earns a slot |
|---|---|---|---|
| `dark` *(existing, default)* | dark | graphite + amber action, terminal-native density | the identity (§1 F1); unchanged except the folded §2 fixes |
| `light` *(existing)* | light | the same language on paper-white | parity for daylight/office; gains the §2 fixes |
| `hc-dark` **new** | dark | the same graphite/amber, stronger | not a new identity: body/secondary/button text ≥ 7:1 (17.71/12.32/11.77), control borders ≥ 3:1 (WCAG 1.4.11); muted 8.67; `dim` stays a documented deviation at 4.99 — gated, not raised to 7 (§10.5-2) |
| `hc-light` **new** | light | the same light, stronger | same, on the light side (19.70/12.58/7.77; borders 3.25; `dim` 5.04 documented the same way) |
| `paper` **new** | light | warm cream paper, sepia ink, terracotta action | long-form reading: READMEs, reviews, issue threads. The warm cast reads as a document, not a dashboard; the expressive move is the *world*, the semantics stay stock |
| `ocean` **new** | dark | deep navy, slate surfaces, azure action | a dark theme whose cast is calm rather than graphite — the most requested "not another grey dark mode"; blue is the lowest-fatigue dark hue for long sessions |
| `terminal` **new** | dark | green-cast CRT world, phosphor-grey text, amber action kept | the git-native romance: a phosphor world without breaking rule §1.3-4 (green stays state) |

**Rejected:** a 4th expressive theme (violet "dusk") — the merged/prerelease
vocab already occupies violet/sky, so an accent there would collide with state
chips; and a green-*accent* terminal (§1.3-4). Revisit only with a state-role
change, which is a guideline amendment, not a palette.

---

## 4. Palettes (token → hex, with measured contrast)

Gates: **4.5:1** for all text ≤ 18 px (chips are 10 px, notes 11 px, code 11.5 px
— all "small text" under WCAG 2.2), **3:1** for non-text UI (focus ring, control
borders in HC, selection edge, state markers), **≥ 7:1** body/secondary/button
text in the two HC themes. Every table row was computed with the §9.1 formula;
"worst" names the theme's tightest gated pair.

New tokens introduced in §1.2 appear here per theme. Alpha-tint tokens
(`add-bg`/`del-bg` on dark sides) are specified as `rgb(r g b / a)` and the
table lists the computed solid over canvas for review; the gate tests both the
canvas and surface blends.

### 4.1 `dark` — existing default (reference; changed cells folded from §2)

| token | value | | token | value |
|---|---|---|---|---|
| canvas | `#0f1113` | | accent | `#ffb224` |
| surface | `#14181b` | | accent-hover | `#ffc862` |
| raised / raised-2 | `#1c2125` / `#22282c` | | on-accent | `#151004` |
| line / line-strong | `#232a2e` / `#323a3f` | | ok / ok-bg | `#4ec9a4` / `#1d3431` *(solid of the 16 % tint)* |
| text | `#e6eaec` | | warn / warn-bg | `#e0a95e` / `#393227` |
| text-2 | `#a3adb4` | | danger / danger-bg | `#e06c6c` / `#352528` |
| muted | `#79838b` | | merged / merged-bg | `#a78bfa` / `#2e2d43` |
| dim | `#525c63` | | prerelease / prerelease-bg | `#38bdf8` / `#1a3643` |
| link | `#e6eaec` | | add-bg / add-fg | `rgb(78 201 164 / .12)` (= `#172724` on canvas) / `#b7e8d8` |
| code-bg / kw | `#0f1113` / `#a78bfa` | | del-bg / del-fg | `rgb(224 108 108 / .12)` (= `#281c1e`) / `#f2c3c3` |
| sel / sel-edge | `#2a2214` / `#ffb224` | | info-bg | `#10202b` |
| shadow | `0 8px 24px rgb(0 0 0 / .45)` | | | |

Key pairs: body/canvas **15.63** · body/surface 14.75 · text-2 8.28 ·
muted/surface **4.62** · link 15.63 · on-accent **10.51** · accent/canvas (ring)
**10.49** · chip ok 6.43 / warn 6.03 / **danger 4.51** / merged 4.91 / pre 5.93 ·
add-fg **11.50** / del-fg 10.49 (over canvas) · table header (muted/surface)
4.62 · kw 6.95.
**Worst gated pair: chip-danger 4.51:1** — passes AA exactly; headroom note in §10.

### 4.2 `light` — existing (reference; ✱ = folded fix from §2)

| token | value | | token | value |
|---|---|---|---|---|
| canvas | `#ffffff` | | accent | `#9a5b00` |
| surface | `#f7f7f6` | | accent-hover | `#7a4600` |
| raised / raised-2 | `#efefed` / `#e6e6e3` | | on-accent | `#ffffff` |
| line / line-strong | `#e2e2df` / `#cfcfca` | | ok / ok-bg | `#0f7355` / `#dbe8e2` ✱ |
| text | `#17181a` | | warn / warn-bg | `#8a5a10` / `#eee2ca` ✱ |
| text-2 | `#55585d` | | danger / danger-bg | `#b42323` / `#ecd5d4` |
| muted | `#6a6f74` ✱ | | merged / merged-bg | `#6d28d9` ✱ / `#e7e1f4` ✱ |
| dim | `#8a8f94` | | prerelease / prerelease-bg | `#0c6291` ✱ / `#d8e9f0` ✱ |
| link | `#17181a` | | add-bg / add-fg | `#e8f6f0` / `#0b5a43` |
| code-bg / kw | `#ffffff` / `#6d28d9` ✱ | | del-bg / del-fg | `#fdf0f0` / `#8f2020` |
| sel / sel-edge | `#fdf0d0` / `#b26a00` | | info-bg | `#e8f1f8` |
| shadow | `0 8px 24px rgb(0 0 0 / .12)` | | | |

Key pairs: body/canvas **17.77** · text-2 7.14 · muted/surface **4.73** ·
link 17.77 · on-accent **5.43** · accent/canvas (ring) **5.43** · chip ok
**4.62** / warn **4.61** / danger 4.69 / merged **5.57** / pre **5.31** ·
add-fg **7.38** / del-fg 7.89 · table header 4.73 · kw **7.10**.
**Worst gated pair: chip-warn 4.61:1.**

### 4.3 `hc-dark` — new (same identity, stronger)

| token | value | | token | value |
|---|---|---|---|---|
| canvas | `#0f1113` | | accent | `#ffc23e` |
| surface | `#14181b` | | accent-hover | `#ffd06e` |
| raised / raised-2 | `#1c2125` / `#22282c` | | on-accent | `#151004` |
| line / line-strong | `#39434b` / `#5b6871` | | ok / ok-bg | `#5fd6b0` / `#1a3a33` |
| text | `#f4f8fa` | | warn / warn-bg | `#e8ba7c` / `#3f3626` |
| text-2 | `#c8d2d9` | | danger / danger-bg | `#f08484` / `#3d2427` |
| muted | `#a6b1ba` | | merged / merged-bg | `#b9a3fc` / `#332b4d` |
| dim | `#7d8992` | | prerelease / prerelease-bg | `#6cc8f8` / `#15394a` |
| link | `#f4f8fa` | | add-bg / add-fg | `rgb(78 201 164 / .16)` (= `#192e2a`) / `#c8f2e2` |
| code-bg / kw | `#0f1113` / `#c0b0ff` | | del-bg / del-fg | `rgb(224 108 108 / .16)` (= `#302021`) / `#f8d4d4` |
| sel / sel-edge | `#33290f` / `#ffc23e` | | info-bg | `#14242f` |
| shadow | `0 8px 24px rgb(0 0 0 / .55)` | | | |

Key pairs: body/canvas **17.71** · text-2 **12.32** · muted **8.67** ·
on-accent **11.77** · accent/canvas **11.75** · line-strong/canvas **3.30** ·
dim/surface (gutter anchors) **4.99 — documented deviation: gated at 4.5, not
raised to 7, exactly like the defaults' line-strong/dim deviations (§10.5-2)**
· chips 5.63–6.91 · add-fg **11.77** · table header 8.18 · kw 9.82.
**All ≥ 7:1 body/secondary/button targets met; worst gated pair: chip-danger 5.63:1.**

### 4.4 `hc-light` — new

| token | value | | token | value |
|---|---|---|---|---|
| canvas | `#ffffff` | | accent | `#7a4600` |
| surface | `#f7f7f6` | | accent-hover | `#5c3400` |
| raised / raised-2 | `#efefed` / `#e6e6e3` | | on-accent | `#ffffff` |
| line / line-strong | `#c8c8c3` / `#8f8f89` | | ok / ok-bg | `#0c5c44` / `#cde5db` |
| text | `#0a0b0c` | | warn / warn-bg | `#6f4a0c` / `#e6dcc4` |
| text-2 | `#2f3438` | | danger / danger-bg | `#9c1c1c` / `#f3d4d4` |
| muted | `#4d5358` | | merged / merged-bg | `#5b21b6` / `#e2dcf3` |
| dim | `#656b70` | | prerelease / prerelease-bg | `#0c5f8a` / `#d2e8f0` |
| link | `#0a0b0c` | | add-bg / add-fg | `#dcf0e8` / `#08392a` |
| code-bg / kw | `#ffffff` / `#5b21b6` | | del-bg / del-fg | `#fae8e8` / `#701616` |
| sel / sel-edge | `#fceebc` / `#7a4600` | | info-bg | `#e3edf5` |
| shadow | `0 8px 24px rgb(0 0 0 / .12)` | | | |

Key pairs: body/canvas **19.70** · text-2 **12.58** · muted **7.80** ·
on-accent **7.77** · accent/canvas **7.77** · line-strong/canvas **3.25** ·
dim/surface **5.04 — documented deviation, same rule as hc-dark (§10.5-2)** ·
chips 5.48–6.74 · add-fg **10.85** · table header 7.27 · kw 8.98.
**All ≥ 7:1 body/secondary/button targets met; worst gated pair: chip-prerelease 5.48:1.**

### 4.5 `paper` — new (warm light)

| token | value | | token | value |
|---|---|---|---|---|
| canvas | `#f9f3e7` | | accent | `#9c4a12` |
| surface | `#f2ecdc` | | accent-hover | `#7d3a0e` |
| raised / raised-2 | `#ebe2cd` / `#e2d8bf` | | on-accent | `#ffffff` |
| line / line-strong | `#ded2b6` / `#a8935f` | | ok / ok-bg | `#175c43` / `#dbe7d7` |
| text | `#241d10` | | warn / warn-bg | `#7a4a10` / `#ecdfc0` |
| text-2 | `#5a5040` | | danger / danger-bg | `#a02222` / `#f2ddda` |
| muted | `#6a5f4a` | | merged / merged-bg | `#5b21b6` / `#e5def0` |
| dim | `#94876a` | | prerelease / prerelease-bg | `#0c5f8a` / `#d4e6ea` |
| link | `#241d10` | | add-bg / add-fg | `#dcead5` / `#123a28` |
| code-bg / kw | `#fdfaf1` / `#5b21b6` | | del-bg / del-fg | `#f4e0d8` / `#701c1c` |
| sel / sel-edge | `#f2e3ba` / `#9c4a12` | | info-bg | `#e3ecef` |
| shadow | `0 8px 24px rgb(31 24 6 / .14)` | | | |

Key pairs: body/canvas **15.10** · text-2 7.15 · muted/surface **5.32** ·
link 15.10 · on-accent **6.17** · accent/canvas (ring) **5.59** · chips 5.40–6.86 ·
add-fg **10.09** / del-fg 8.82 · table header 5.32 · kw 8.61.
**Worst gated pair: chip-prerelease 5.40:1.**

### 4.6 `ocean` — new (cool dark)

| token | value | | token | value |
|---|---|---|---|---|
| canvas | `#0c1420` | | accent | `#6cb6ff` |
| surface | `#101a29` | | accent-hover | `#93c8ff` |
| raised / raised-2 | `#16222f` / `#1c2a3a` | | on-accent | `#081120` |
| line / line-strong | `#1e2a3c` / `#33415a` | | ok / ok-bg | `#55cf9f` / `#12332b` |
| text | `#e0eaf4` | | warn / warn-bg | `#e6b577` / `#3a3324` |
| text-2 | `#a6b8cc` | | danger / danger-bg | `#ef7d7d` / `#3c2730` |
| muted | `#7d8ea6` | | merged / merged-bg | `#b9a3fc` / `#302b4a` |
| dim | `#56657e` | | prerelease / prerelease-bg | `#a5e3f7` / `#16324a` |
| link | `#e0eaf4` | | add-bg / add-fg | `rgb(85 207 159 / .12)` (= `#152a2f`) / `#bce8d8` |
| code-bg / kw | `#0c1420` / `#b0a6f7` | | del-bg / del-fg | `rgb(239 125 125 / .12)` (= `#27212b`) / `#f4c8c8` |
| sel / sel-edge | `#1a2c4d` / `#6cb6ff` | | info-bg | `#14304a` |
| shadow | `0 8px 24px rgb(0 0 0 / .50)` | | | |

Key pairs: body/canvas **15.17** · text-2 9.11 · muted/surface **5.24** ·
link 15.17 · on-accent **8.79** · accent/canvas (ring) **8.60** · chips 5.18–9.39 ·
add-fg **11.16** / del-fg 10.41 · table header 5.24 · kw 8.48.
Prerelease is the **pale** cyan `#a5e3f7` deliberately: the accent azure and a
sky-400-grade prerelease would be near-identical (§6.2); pale-vs-azure separates
at ΔE 29 (protan) / 32 (deutan) / 18 (tritan).
**Worst gated pair: chip-danger 5.18:1.**

### 4.7 `terminal` — new (green-cast dark, amber action kept)

| token | value | | token | value |
|---|---|---|---|---|
| canvas | `#0a0f0c` | | accent | `#ffb224` |
| surface | `#0f1611` | | accent-hover | `#ffc862` |
| raised / raised-2 | `#161f18` / `#1b2620` | | on-accent | `#151004` |
| line / line-strong | `#1c271f` / `#2f4032` | | ok / ok-bg | `#57d6ab` / `#12352a` |
| text | `#d9e8dc` | | warn / warn-bg | `#e0a95e` / `#3a3323` |
| text-2 | `#a4b8a9` | | danger / danger-bg | `#e87474` / `#3a2426` |
| muted | `#7d9182` | | merged / merged-bg | `#b9a3fc` / `#302b4a` |
| dim | `#576858` | | prerelease / prerelease-bg | `#6cc8f8` / `#16324a` |
| link | `#d9e8dc` | | add-bg / add-fg | `rgb(87 214 171 / .12)` (= `#13271f`) / `#b8ead2` |
| code-bg / kw | `#0a0f0c` / `#b0a2f0` | | del-bg / del-fg | `rgb(232 116 116 / .12)` (= `#251b18`) / `#f2c6c6` |
| sel / sel-edge | `#292211` / `#ffb224` | | info-bg | `#14261f` |
| shadow | `0 8px 24px rgb(0 0 0 / .50)` | | | |

Key pairs: body/canvas **15.21** · text-2 9.21 · muted/surface **5.46** ·
link 15.21 · on-accent **10.51** · accent/canvas (ring) **10.72** · chips 4.92–7.40 ·
add-fg **11.76** / del-fg 10.95 · table header 5.46 · kw 8.53.
**Worst gated pair: chip-danger 4.92:1.**

---

## 5. Ramp retunes (the mechanism the themes ride)

Templates still carry ~50 inline ramp utilities (`bg-zinc-100
dark:bg-zinc-800`, `accent-amber-500`, `border-zinc-200 dark:border-zinc-800`,
`text-red-700 dark:text-red-400`, `bg-emerald-500`, `border-sky-300`…). The
built CSS references them through variables, so **per-theme ramp overrides in
the same `[data-theme]` block re-skin all of them with zero template edits**
(except the conversions in §8.4, which exist for semantic reasons, not
mechanical ones).

### 5.1 `zinc` per theme (the surface/border/text ramp)

Current (shared both sides): `50 #f7f7f6 · 100 #e8ebec · 200 #e2e2df ·
300 #cfcfca · 400 #9aa0a6 · 500 #6f7378 · 600 #55585d · 700 #323a3f ·
800 #232a2e · 900 #14181b · 950 #0f1113`.

| theme | steps overridden | why |
|---|---|---|
| `dark` | 500 → **`#79838b`** | the only dark-side text step that failed (11 px notes, §2-6: 3.96 → **4.89**) |
| `light` | 400 → **`#6a6f74`** | the only light-side failing text step (§2-7: 2.64 → **5.07**) |
| `hc-dark` | 400 `#c2ccd4` · 500 `#a6b1ba` · 600 `#7d8992` · 700 `#3e4850` · 800 `#2c363e` | text steps 11.61/8.67:1; avatar pair (700 bg × 50 text) **8.72:1**; clone-cmd ground visible. *(Revised: the earlier zinc-300 override is dropped — step 300 is inert on the dark side, where the `.dark`-scoped scrollbar-thumb rule consumes 700 — and it broke the composed ramp's monotonicity.)* |
| `hc-light` | 200 `#b4b4ae` · 300 `#9a9a93` · 400 `#6a6f74` · 500 `#4d5358` · 600 `#2f3438` · **700 `#262c30`** | text steps 5.07/7.80/12.58; fieldset borders (zinc-200) **2.08:1** (documented: grouping hairlines, the token `line-strong` carries the 3:1 control boundary); *(revised: 700 is overridden so the composed ramp stays monotonic through the new, darker 600 — step 700 is the avatar ground, consumed on both sides).* |
| `paper` | 50 `#faf4e8` · 100 `#f0e9d6` · 200 `#ddd2b4` · 300 `#c3b58c` · 400 `#6f6444` · 500 `#5b5032` · 600 `#463d22` · 700 `#3a3117` · 800 `#2e2612` · 900 `#191407` · 950 `#100c04` | the sand ramp; text steps **5.30 / 7.20 / 9.74** |
| `ocean` | 50 `#eef3fa` · 100 `#dbe6f2` · 200 `#b8c9de` · 300 `#93a9c6` · 400 `#8fa3bd` · 500 `#6d80a0` · 600 `#4a5c78` · 700 `#2c3d58` · 800 `#1c2942` · 900 `#111c2e` · 950 `#0b1320` | navy-graphite; text steps **7.16 / 4.62**; avatar pair **9.83:1** |
| `terminal` | 50 `#eaf2eb` · 100 `#d7e6d9` · 200 `#b6cdb9` · 300 **`#8fa89a`** · 400 **`#8ba592`** · 500 `#6d8477` · 600 `#4c5f53` · 700 `#2c3a2f` · 800 `#1d2a20` · 900 `#131b15` · 950 `#0c120d` | green-graphite; text steps **7.27 / 4.80**; avatar pair **10.49:1**. *(Revised: the original table inverted 300/400 — 400 sat lighter than 300; the swap restores monotonicity and moves the gated text step to 7.27:1.)* |

Only the *active side's* steps matter (light-side themes never fire `dark:`
utilities and vice-versa), but full ramps are specified so the inert half can
never surprise. The identity-avatar pair (`bg-zinc-700 text-zinc-50
ring-zinc-600`, `partials/header.html` 36 — unpaired, fires on both sides) is
gated per theme (§9.1 pair 20).

**Monotonicity is gated per side, not per whole ramp** (a theme inherits the
other side's steps, so a whole-ramp check would reject correct tables):
dark-side themes assert their **composed 400–950** strictly darkens; light-side
themes assert their **composed 50–600** (hc-light also 700, since it overrides
it). The avatar steps (50, 700) are covered by pair 20 regardless of side.
Every table above passes its scoped assertion as written.

### 5.2 The other ramps

- **`emerald`, `amber`**: after §8.4's conversions no template *or Go* utility
  references them (grep-guarded by the gate test, §9.1 pair 25 — Go included
  precisely because `checkStateDot` was emitting them), and the `@theme`
  retunes of both become dead code — delete both retunes in the same change
  (pre-1.0 law: no dead shapes). The semantic tokens carry the families.
- **`red`** (form errors `text-red-700 dark:text-red-400`, the only remaining
  paired red use): keep Tailwind defaults; they pass on every canvas —
  `#b91c1c` on white 6.47 / on paper cream **5.85**; `#f87171` on dark canvas
  6.84 / on ocean 6.68. Gated as pair 22.
- **`sky`, `violet`**: unused after §8.4 (callout + kw convert to tokens); no
  `@theme` entries needed.
- **Graph lanes** `--graph-0..7`: light-side themes reuse the existing
  700-grade set, dark-side the 400-grade set (both already measured ≥ 3:1 on
  their canvases — the weakest light lane `#4d7c0f` on paper cream **4.52:1**;
  the weakest 400-grade lane `#fb7185` measures **6.87:1** on ocean and
  **7.18:1** on terminal). Lanes are continuity
  markers, not semantic colour — the graph's meaning is topology, so CVD
  safety is structural. Gate: each lane ≥ 3:1 vs canvas (§9.1 pair 21).

---

## 6. Colour-blindness safety

### 6.1 The binding rule: no state is hue-only

Every state signal must carry a non-hue channel. This is mostly already true
and becomes a **gate**, not a hope:

| Signal | Non-hue channel (existing → required) |
|---|---|
| open / closed / merged / draft / prerelease chips | uppercase mono **text label** (OPEN / CLOSED / …) — universal today; the gate test asserts every chip class is always rendered with a label (template grep: `chip-` without adjacent text = review failure) |
| required-check pass / fail / pending / skipped dots | **shape, added in W1** to the *whole* dot path — the shared template block `collab_check_dot` (`partials/collab_checks.html` 5) **and the Go emitter `checkStateDot` (`internal/webui/collab.go` 1280–1298)** whose classes reach `collab_check_pill` (`collab_checks.html` 2, used by `pull.html` 172 and `check_detail.html` 9) and the **dot-only** `partials/repo_check_pill.html` 10 (commits page — there the dot *is* the visual signal; the anchor's `title`/`aria-label` carries the accessible name): success = filled circle, failure/error = **filled diamond** (45° rotation), pending/in-progress = hollow circle (in-progress keeps its pulse), cancelled/skipped = hollow + dim. One renderer, one class map — §8.4-2 deletes the Go duplicate. Most dot surfaces already render the state as an adjacent `chip-neutral` text (verified: `checks.html` 35, `collab_check_contexts` 14, `commit.html` 74, `threads.html` 129, `thread_detail.html` 72); the dots are decoration there, and the shape change makes the at-a-glance read survive CVD on every surface, dot-only ones included |
| diff added / removed | the **sign column** (`+`/`−`, `select-none`, ahead of the line numbers — guideline §8) plus the row tint; sign column must never be removed (it is the load-bearing channel per §6.2) |
| label chips | the label **text** (user-chosen colours never encode meaning alone) |
| form errors | the error **text** (`.err-line`, inline `role="alert"`); colour is redundant. Two `err-line` sites lacked the role (`partials/admin_config_rows.html` — the config row errors and the auth-test fragment); PLAN-ADMIN M2 added `role="alert"` to both, so W1 (§8.4-12) only keeps it pinned |
| unread notification | the dot is decorative; the row text + `aria-label="unread"` carries it (also gains the shared dot shapes) |
| links | **underline** (F3), never colour alone |

### 6.2 The measured check per palette

Dichromacy simulation (Machado et al. 2009, severity 1.0, linear RGB) + CIELAB
ΔE76; ΔE < 10 read as confusable, 10–30 moderate, > 30 distinct. Luminance
separation (the hue-independent channel) is the WCAG ratio between the two
swatches. Values are estimates for plan-level review; the binding guarantees
are §6.1's channels, which is why confusable rows below are acceptable **only**
with their channel intact.

Findings that hold in **every** palette (the shape of the problem is fixed by
the vocabulary, not by any palette choice):

- **diff add-fg vs del-fg is protan/deutan-confusable** (ΔE 4–13 everywhere;
  luminance separation only 1.1–1.2×). The sign column is not decoration — it
  is the channel. (This is also true of GitHub's diffs; it is why ours already
  has one.)
- **ok vs prerelease is tritan-confusable** (ΔE 8–9 everywhere). Prerelease
  chips appear only on release pages and are labelled.
- **ok vs danger under protan/deutan is only moderate** (ΔE 15–36, luminance
  separation 1.1–1.6×) — never rely on it: chips are labelled, dots get shapes
  (W1).
- **accent vs warn is same-family** in every theme that keeps amber as action
  (dark, light, both HC, terminal, paper): under simulation the dark-side
  amber themes (dark, hc-dark, terminal) hold **ΔE P 31.0 / D 28.2 / T 5.0** —
  distinct under protan/deutan, confusable under *tritan* — while the
  light-side amber set (light, hc-light, paper) is broadly confusable
  (**P 0.6–4.4 / D 5.2–9.1 / T 8.5–9.1**); ocean's azure-vs-amber is the one
  theme where they fully separate (ΔE 76/86/72). Sanctioned: warn never
  renders as a control; draft chips are labelled; the primary button is filled.

Per-theme notable rows (full 9-pair × 3-CVD matrix goes into the gate
harness as documentation):

| theme | confusable pairs (ΔE < 10) | mitigation (per §6.1) |
|---|---|---|
| dark | add/del fg (P/D), ok/pre (T), accent/warn (T) | sign column; prerelease labelled; warn never a control |
| light | warn/danger (D 3), accent/warn (P/D/T), ok/pre (T) | chip labels; filled-vs-outline; prerelease labelled |
| hc-dark | add/del fg (P/D), ok/pre (T), accent/warn (T) | as dark; shapes stronger (dot set) |
| hc-light | warn/danger (D 2), accent/warn (P/D/T), add/del (P 9) | as light |
| paper | warn/danger (D 3), accent/warn (P 4/D 9), add/del (P 5) | as light |
| ocean | add/del fg (P 8/D 6) | sign column; accent/warn **distinct** here |
| terminal | add/del fg (D 4), accent/warn (T 5) | sign column; labels |

**The one deliberate cross-theme muscle-memory choice:** ok stays
green-family in all seven themes (including terminal, where the world is also
green-cast but the ok chip/`tok-str` value `#57d6ab` separates from the
phosphor body text `#d9e8dc` by luminance **1.42×** — the tightest such pair
in the set, against dark's 1.70× and ocean's 1.59× — and always carries its
label).

---

## 7. How a theme is selected

### 7.1 Resolution order (per page load, before first paint)

```
stored explicit choice (localStorage["spindle-theme"], a valid id)
  → else prefers-color-scheme: dark ? "dark" : "light"
    → else (no matchMedia / no preference) "dark"     ← the product default
```

A stored `"dark"`/`"light"` from the current toggle is already a valid id —
**migration is a no-op** for existing users. A stored `"system"` (or a cleared
key) opts back into resolution step 2, and while in system mode a
`matchMedia` change re-resolves live (the island subscribes only in that mode).
If the owner would rather the product always *introduce itself* dark (first
visit ignores the OS), that is a one-line flip in the boot script — flagged in
§10.5 rather than decided here, because §7's order is what "match system"
means.

### 7.2 Persistence: `localStorage` (decision + justification)

Keep the existing mechanism — `localStorage["spindle-theme"]`, now holding a
theme id — and extend it. **Not** a cookie, **not** a server-side profile
setting:

1. **The product is anonymous-first.** Public repo browsing and `auth none`
   instances have no principal to hang a preference on; a server profile would
   cover only the signed-in minority and still need a client store for
   everyone else — two sources of truth for one value.
2. **AGENTS law 8:** feature state must never live in the bucket/WAL without a
   schema decision. A server-side theme pref is a schema decision
   (`owners/<owner>/profile.json` extension) + an API + a write path for a
   purely presentational, per-display choice.
3. **Zero round trips and zero new gates** — the theme is applied before the
   first paint client-side; a cookie would burn a request header on every asset
   fetch for no gain, and a server profile would need a read-before-render seam
   the page provider does not have today.
4. **Forward-compatible:** if cross-device sync is ever wanted, the server
   profile *seeds* `data-theme` at render time and localStorage stays the
   per-device override (precedence: localStorage > server-rendered attr >
   system > dark). The mechanism below does not preclude it; building it now
   would.

### 7.3 No flash of the wrong theme (the folded fix)

Today `base.html` ships `<html lang="en" class="dark">` and the stored choice
is applied by `web/src/islands/theme.js` when the **module** executes — after
first paint. A light-mode user gets a dark flash on every page. And CSP
(`internal/httpsec/httpsec.go`: `script-src 'self'`) forbids the usual inline
bootstrap snippet. The fix rides the constraint:

- New first-party file `web/src/theme-boot.js` (~15 lines: read
  `localStorage`, fall back to §7.1, set `documentElement.dataset.theme` and
  toggle the `.dark` class) built by the **existing esbuild step as a second
  entry point** to `web/dist/theme-boot.js` — no new dependency, no bundler
  change beyond the entry list.
- **Storage-denied path (explicit):** the boot script keeps the current
  `theme.js` try/catch around every storage access (private mode, blocked
  storage). A read failure resolves exactly like an absent value —
  `prefers-color-scheme` → else dark — and a write failure simply does not
  persist: the theme still applies for the page, as the current toggle
  already behaves.
- `base.html` gains `<script src="/_ui/theme-boot.js"></script>` immediately
  after the stylesheet link — a classic parser-blocking script from `'self'`,
  so it runs before first paint and CSP is untouched. The `type="module"`
  island bundle keeps its `defer` semantics.
- The markup keeps `class="dark"` as the no-JS/no-storage floor, so the
  no-script path renders the product default, never an unstyled flash.

### 7.4 The selection surface

The single header toggle button (`partials/header.html` `data-theme-toggle`)
becomes a **theme menu** — the shared `menus.js` popover contract (§4 of the
guideline: trigger inside `data-menu`, `role="menu"`, Esc/arrow keyboard,
outside-click), replacing the two-state button:

```
match system            ◐
──────────────────────
dark                    ▪ swatch
light                   ▪ swatch
dark · high contrast    ▪ swatch
light · high contrast   ▪ swatch
──────────────────────
paper                   ▪ swatch
ocean                   ▪ swatch
terminal                ▪ swatch
```

- Static template partial `partials/theme_menu.html` — **no Go handler
  changes**: the theme list is fixed markup; the island marks the active entry
  (`menuitemradio` + `aria-checked`, §8.2's model) from localStorage. Mono
  lowercase labels, `.btn` trigger with the moon glyph, one `.primary`-free
  popover (menu items are never primaries).
- Each row's **swatch** is three inline-style chips (canvas / surface /
  accent). The literals live in exactly one place (this partial) and the gate
  test pins them to the token tables (§9.1 pair 23) so the preview can never
  drift from the palette.
- **Width cap, stated honestly:** the `max-width: calc(100vw - 1rem)` bound
  is today the *enumerated* rule at `app.css` 439–441 (`.clone-body,
  .ref-drop, … .tray`) — it does **not** already apply to every floating
  `.card`, and `menus.js` does no JS capping. The theme-menu panel therefore
  needs the bound, and the plan makes it **structural** while this area is
  open: the `.card.absolute, .card.fixed` rule (app.css 427–430, the #405
  opaque-panel precedent) gains the same `max-width`, which also closes the
  latent gap for every future popover (§8.4-8).
- 390 px: the menu rides that structural cap; verified in W3's rendered smoke.

### 7.5 The two high-contrast themes are first-class entries, not a modifier

Decided: **no "increase contrast" flag.** A `theme × contrast` modifier would
multiply 7 palettes into 14 combinations, double the gate matrix, and complicate
the menu for a benefit nobody has measured. Instead:

- `hc-dark`/`hc-light` are ordinary menu entries (they are also the natural
  landing for users who today flip `prefers-contrast` in their OS).
- `@media (prefers-contrast: more)` maps **system mode only** to the HC theme
  of the resolved side (dark → `hc-dark`, light → `hc-light`). An explicit
  stored choice always wins, so the mapping never surprises a user who picked
  a theme by hand. This is one rule in the boot script + one in the island's
  live re-resolve; no CSS duplication.

### 7.6 `prefers-color-scheme` today

There is none, deliberately (guideline F1: class-based variant, "the toggle is
a deliberate user choice"). This plan keeps the class-based `@custom-variant
dark` untouched — the `.dark` class remains the luminance-side switch for the
~50 `dark:` utilities — and adds system *resolution* in the boot script
(§7.1). CSS `prefers-color-scheme` media queries are still never used for
palettes: the token blocks are discrete `[data-theme]` sets and the script
picks the id once, before paint.

---

## 8. Implementation plan

### 8.1 File structure (CSS)

```
web/src/app.css
  @import "tailwindcss";
  @source ...                       // unchanged
  @custom-variant dark ...          // unchanged: .dark stays the side switch
  @theme { zinc / fonts / radius / text }   // emerald+amber retunes DELETED (§5.2)
  :root   { --pd-* light  }         // unchanged role; gains the §2 fixes + new tokens
  .dark   { --pd-* dark   }         // unchanged role; gains new tokens
  [data-theme="hc-dark"]  { --pd-*… 35 tokens; --color-zinc-* overrides; color-scheme via .dark }
  [data-theme="hc-light"] { … }
  [data-theme="paper"]    { … }
  [data-theme="ocean"]    { … }
  [data-theme="terminal"] { … }
```

Rules that make this robust:

- **Every non-default theme block sets the complete token list** — no partial
  themes, no token bleed from `.dark` defaults (completeness asserted by the
  gate test, §9.1 pair 24). `.dark` continues to carry the default dark set;
  `[data-theme="hc-dark"]` (later in source order, equal specificity) fully
  overrides it, and the boot script keeps `.dark` present on dark-side themes
  so every `dark:` utility still fires.
- `data-theme="dark"` / `"light"` need no blocks (`.dark` / `:root` are them).
- The existing `:root { color-scheme: light } .dark { color-scheme: dark }`
  rule already covers every theme via the `.dark` toggle — no change.
- Blocks are authored plain CSS in `app.css` (~35 tokens + ramp steps each,
  ≈ 300 added lines); no build-mechanism change, no new dependencies
  (AGENTS law 1 untouched — no CSS-in-JS, no runtime theming library).

### 8.2 JS wiring

| file | change |
|---|---|
| `web/src/theme-boot.js` | **new** (§7.3): resolve §7.1, set `dataset.theme` + `.dark`; ~15 lines, classic script, second esbuild entry |
| `internal/webui/templates/base.html` | add the `<script src="/_ui/theme-boot.js">` tag after the stylesheet link |
| `web/src/islands/theme.js` | rewrite: `apply(id)` sets `data-theme` + side class; menu open/close rides `menus.js`; `menuitemradio` + `aria-checked` sync; store id or `"system"`; subscribe to `matchMedia` + `prefers-contrast` changes only while unresolved-in-system-mode; idempotent under htmx swaps as today (an island must never re-apply the theme to a subtree — F1) |
| `internal/webui/templates/partials/theme_menu.html` | **new** (§7.4): static menu + swatches |
| `internal/webui/templates/partials/header.html` | replace the `data-theme-toggle` button with the menu trigger |
| `web/package.json` (`build:islands`, and the `build` chain) | second esbuild entry point `src/theme-boot.js` → `dist/theme-boot.js`. *(Corrected: the entry list lives in `web/package.json`, not the Makefile — the Makefile only calls `pnpm --dir web run build`.)* |

### 8.3 Print and forced-colors

- **`@media print`**: one grouped block re-asserts the *light* token set on
  `:root, .dark, [data-theme]` (paper output regardless of screen theme),
  neutralises `--pd-shadow` and the translucent header, and hides the status
  strip + theme menu. The gate test asserts the print block matches the light
  block token-for-token (parse + compare) so it cannot drift.
- **`@media (forced-colors: active)`**: the app is already border-and-text
  driven, so mostly *get out of the way*: declare `forced-color-adjust: auto`
  (the default; stated so nobody "fixes" it later), and accept that chip/diff
  tints vanish — the labels (§6.1) and the sign column carry the semantics.
  One required check in W3's smoke: focus outlines remain visible (Windows
  HC draws them from the OS palette) and no rule paints `background` over a
  forced outline.

### 8.4 The conversion inventory (component pickup without per-component changes)

All shared-component edits, once — themes never touch components again:

1. **Chips** (app.css 301–313): the five tinted variants switch from
   `color-mix` grounds to `--pd-*-bg` tokens; `chip-merged`/`chip-prerelease`
   switch from `--color-violet-400`/`--color-sky-400` to `--pd-merged`/`--pd-prerelease`.
2. **Check dots — the WHOLE path, template and Go.** The dots are emitted from
   two places: the template block `collab_check_dot`
   (`partials/collab_checks.html` 5) *and* the Go helper
   **`checkStateDot` (`internal/webui/collab.go` 1280–1298)**, whose class
   strings reach `collab_check_pill` (`collab_checks.html` 2; consumers
   `pull.html` 172, `check_detail.html` 9) and the dot-only
   `partials/repo_check_pill.html` 10 (commits page). Converting only the
   template would leave Go emitting raw `bg-emerald-500 / bg-amber-400 /
   bg-red-500 / bg-red-700 / bg-zinc-400 / bg-zinc-500` — and §5.2 deletes the
   emerald/amber `@theme` retunes, so those utilities would silently fall back
   to Tailwind's *default* values (`#10b981`/`#fbbf24`, not the retuned
   `#2fae86`/`#ffc862`): an untested palette regression on PR panels, commit
   rows and check pages, invisible to a template-only grep. **Decision: one
   renderer.** Delete `checkStateDot` (pre-1.0: change the shape, delete the
   old shape); the pill view models carry the state string and render
   `{{template "collab_check_dot" .State}}` (the `repo_check_pill` anchor
   keeps its `title`/`aria-label` name); the block maps state → the §6.1
   shape vocabulary as **named classes** (`.dot`, `.dot-ok`, `.dot-fail`,
   `.dot-pending`, `.dot-progress`, `.dot-muted`) defined once in app.css on
   `--pd-ok`/`--pd-danger`/`--pd-warn`/muted — one class map, zero
   Go-emitted colour strings. Pinned strings updated:
   `internal/webui/collab_test.go` 723, 742 (page renders, `bg-red-500` →
   `dot-fail`) and 1488–1490 (the `checkStateDot` unit table test, deleted
   with the helper — the template mapping is covered by the page tests).
3. **Notification markers** (`pages/public/notifications.html` 31, 42):
   `border-l-emerald-500`/`bg-emerald-500` → `--pd-ok` spellings; the pinned
   `border-l-4 border-l-emerald-500` in `internal/webui/public_test.go` 683
   becomes the token spelling.
4. **Checkbox/radio accent** (~30 sites: `accent-amber-500` in
   `settings_setup.html`, `settings_repo.html`, `repo_wal_ops.html`,
   `forms/*.html`): → a shared `.check` class with
   `accent-color: var(--pd-accent)` (the markdown task-box rule app.css 497 is
   the precedent — same mechanism, one class).
5. **Avatar ring + tab edge + label ring** (`header.html` 36
   `hover:ring-amber-500`, `repo_tabs.html` 16 `border-b-amber-500!`,
   `release.html` 115 `peer-focus-visible:ring-amber-500`): →
   `var(--pd-accent)` spellings.
6. **Setup callout** (app.css 341): sky utilities → `--pd-info-bg` +
   `--pd-line-strong` border.
7. **Syntax** (app.css 559–562): `.tok-kw` → `--pd-kw`; `.tok-str`/`.tok-num`/
   `.tok-com` already use `--pd-ok`/`--pd-accent`/`--pd-muted`.
8. **Structural popover width cap** (app.css 427–430): the
   `.card.absolute, .card.fixed` rule gains
   `max-width: calc(100vw - 1rem)`, promoting the enumerated 439–441 rule to
   the default for every floating panel — the #405 opaque-panel precedent
   applied one step further, and the cap the new theme menu actually needs
   (§7.4). The enumerated rule stays as a harmless no-op for its named
   panels.
9. **Markdown blockquote / setup-row borders** (app.css 334–352, 516–517): the
   zinc utilities stay — they ride the per-theme ramp (§5.1).
10. **Dead-token consumers (the two tokens §1.2 adds would otherwise be
    unreferenced):** `.markdown pre` (app.css 510) and `.code-view`
    (app.css 536) backgrounds → `var(--pd-code-bg)` — without this, paper's
    distinct `#fdfaf1` code ground never renders; `.link` (app.css 229) and
    `.markdown-body a` (app.css 498) colour → `var(--pd-link)`. The gate's
    dead-token check (§9.1 pair 24) makes an unreferenced token a failure, so
    a token and its consumer land in the same change.
11. **Mirror-guide status lines** (`partials/repo_guide.html` 13, 63 —
    `text-amber-700 dark:text-amber-400`, mirror-sync result and invitation
    expiry): → `text-[color:var(--pd-warn)]` (the `divide-[color:var(--pd-line)]`
    arbitrary-value spelling is the in-tree precedent). They are warn text;
    the §9.1 pair-25 grep would fail the build on these rows if left.
12. **A11y riders (theme-independent, folded into W1 to avoid a second
    chrome-only pass):** (a) no skip link exists today (searched: none in any
    template) — add `<a class="skip-link" href="#app">` as the first focusable
    element in `base.html` + one `.skip-link` rule (ink on surface, appears on
    `:focus-visible`); (b) `role="alert"` on the two `.err-line` sites that
    lack it (`partials/admin_config_rows.html` — fixed with role="alert" in PLAN-ADMIN M2).

### 8.5 What deliberately does *not* change

No template gains a theme conditional; no component rule gains a
`[data-theme]` selector; no JS theming, no CSS variables set from Go; the
renderers (`markdown/diff/graph/highlight.go`) emit classes only, as today;
htmx fragment swaps inherit the `<html>` classes, as today (F1).

---

## 9. Verification plan

### 9.1 The gate: `web/test/unit/themes.test.js` (node `--test`, runs in `make test-web`)

Parses **`web/src/app.css`** (the `:root`/`.dark`/`[data-theme]` blocks +
`@theme` ramps), **`partials/theme_menu.html`** (swatch literals), and
computes every pair with the WCAG 2.x formula:

```
L = 0.2126·R′ + 0.7152·G′ + 0.0722·B′,  C′ = (C/255 ≤ 0.03928) ? C/12.92 : ((C/255 + 0.055)/1.055)^2.4
contrast(a, b) = (Llighter + 0.05) / (Ldarker + 0.05)
```

Validated against the shipped pairs first (dark body 15.63 vs documented 15.6;
light on-accent 5.43 vs 5.4) — the test asserts the validation pairs too, so a
formula regression is itself a failure.

The pair matrix (per theme; gate in brackets). Pairs 1–22 are contrast; 23–26
are the structural assertions:

1. text / canvas and / surface — **4.5** (HC themes: **7**)
2. text-2 / canvas, / surface — **4.5** (HC: **7**)
3. text-2 / raised-2 (the `.btn:hover` label) — **4.5**
4. muted / canvas, / surface — **4.5**
5. link / canvas — **4.5**
6. on-accent / accent (button label) — **4.5** (HC: **7**)
7. on-accent / accent-hover (the hover label) — **4.5**
8. accent / canvas, / surface, / raised (focus ring, btn-active border) — **3**
9. ok, merged, prerelease / canvas (markers, dots) — **3**
10. warn, danger / canvas **as text** (`.warn-line`, `.err-line`,
    `.btn.danger`, the converted guide lines) — **4.5**
11. each state / its `-bg` ground (chip text) — **4.5**
12. text-2 / raised (neutral chip) — **4.5**
13. add-fg / add-bg effective **over canvas and over surface** (alpha blends
    computed) — **4.5**; same for del
14. line-strong / canvas — **3** in HC themes, **1.5 documented deviation** in
    the defaults (§2-8; the deviation value itself is asserted so it cannot
    silently worsen)
15. sel-edge / canvas — **3**
16. dim / surface — **4.5** HC (gutter anchors; **documented, deliberately not
    raised to 7** — §10.5-2), **2.4 documented** defaults
17. kw, ok-as-tok-str, muted-as-tok-com, **tok-num (accent)** / code-bg — **4.5**
18. text / info-bg — **4.5**
19. ramp text steps / canvas — dark-side zinc-400/500, light-side
    zinc-400/500/600 — **4.5**
20. avatar pair zinc-50 / zinc-700 (fires on both sides — `header.html` 36) — **4.5**
21. each graph lane / canvas — **3**
22. red-700 (light-side) and red-400 (dark-side) / canvas (form errors, the one
    sanctioned raw-ramp text) — **4.5**
23. swatch literals in `theme_menu.html` ≡ the theme's canvas/surface/accent
    tokens (drift pin)
24. completeness and liveness: every `[data-theme]` block sets the full token
    list; **every defined token is referenced by at least one rule outside the
    theme blocks (dead-token check — this is what forces §8.4-10's consumers
    to land with the tokens)**; ramp monotonicity **scoped per side**
    (dark-side composed 400–950, light-side composed 50–600, strictly
    darkening; §5.1); the print block matches the light block token-for-token
25. **the raw-ramp grep, templates AND Go**: zero `emerald-|amber-`
    colour-utility strings in `internal/webui/templates/**` **and
    `internal/webui/*.go`** — the server emits classes from Go
    (`checkStateDot` was the case this check exists for); plus the build check
    that `web/dist/app.css` still references `var(--color-zinc-…)` (the
    ramp-override mechanism the themes ride)
26. **theme-block purity and drift**: every `[data-theme=…]` block contains
    *only* custom-property declarations (no component properties, no nested
    rules or media queries); **no other selector in app.css mentions
    `[data-theme`** (§8.5's "no component-level theme conditional", made
    mechanical); and a **±0.05 drift tripwire** — each computed ratio must
    match §4/§5's documented value to ±0.05, not merely sit above gate, so a
    silent above-threshold palette edit fails the build.

Expected values are the tables in §4/§5 — the test fails on any pair that
regresses below gate, including the current-light defects if W1's folded fixes
are dropped. This is the merge gate: **no palette lands without the test**, and
the test is data-driven from the CSS itself (no duplicated palette literals in
the test).

### 9.2 Rendered smoke (per theme × {1440, 390})

Per AGENTS §2/§4 (real Chromium, both widths, console checked,
`documentElement.scrollWidth === clientWidth`):

- `/`, a repo tree page, the issues list (chips + primary), a commit with
  checks (dots), a diff view (add/del/sign), rendered markdown with code
  (kw/str/num/com), `/setup` (info callout + switches), the theme menu open.
- Set the theme via `localStorage["spindle-theme"]` before load; also drive
  the menu (click each entry, reload, assert no flash: screenshot at
  `document.createElement` timing is not enough — assert via
  `PerformanceObserver` paint entry order or a `requestAnimationFrame` probe
  that the `data-theme` attr was set before the stylesheet's paint; practical
  form: boot script sets a `data-theme-boot` marker synchronously, the smoke
  asserts it is present in the very first paint probe).
- Focus visibility: Tab to a control, screenshot the `--pd-accent` ring
  (§9.1 pair 8 guarantees its contrast).
- **Computed-style cascade assertion, one per theme** (pins the whole
  mechanism end-to-end): with `data-theme=ocean` set, assert
  `getComputedStyle(document.body).backgroundColor` equals the ocean canvas
  `#0c1420`; ditto body colour = `--pd-text`; and with a dark-side theme
  active, assert one known `dark:` utility (e.g. the clone-cmd ground)
  resolves to the theme's zinc-800 override — proving the boot script's side
  class, the `@custom-variant`, and the ramp overrides all compose.
- A11y floor per theme: focus ring visible, no page-level pan, chips
  labelled, dot shapes differ without hue (force `--pd-*` monochrome via CDP
  to eyeball-shape check the dots and sign columns).
- `forced-colors` once (any theme) and `print` once (any theme): rendered
  proof the §8.3 behaviour holds.

### 9.3 What "done" means

All pairs green in `themes.test.js` (`make test-web`), the rendered smoke set
above per theme, `make web` + `make fmt && make vet` clean, the
`docs/style-guideline.md` §1 amendment + `docs/go/12_web_ui.md` §2.2 note and
a `DEVIATIONS.md` entry (D-WEB-16: theme system) in the same changes (law 12).

---

## 10. Recommendation

### 10.1 The set

**Ship all 5 new themes** (`hc-dark`, `hc-light`, `paper`, `ocean`,
`terminal`) for a 7-entry menu + `match system` — `terminal` included by
decision (2026-11-21); its tightest pair (ok-text vs body, 1.42×) is covered
by labels and grounds, and its ramp typo is fixed in §5.1. A 4th expressive
theme stays rejected (§3), with the **warm dark (rust/ember)** recorded as the
first future candidate *once this contract has shipped and been load-tested
by the five* — its accent hue collides with nothing in the state vocabulary.

### 10.2 Default

**`dark`** — current behaviour for the no-signal case, and the menu's
highlighted recommendation; `prefers-color-scheme` refines it for first-time
visits whose OS declares a side (§7.1).

### 10.3 Order and effort

| wave | content | estimate |
|---|---|---|
| W1 | token contract (§1.2 tokens) **with their §8.4-10 consumers**, the whole §8.4 conversion inventory **including the Go `checkStateDot` path (§8.4-2) and its pinned-string updates**, the §2 folded fixes **plus any rows the a11y pass's findings file adds when it lands (§0)**, the a11y riders (skip link + the two `role="alert"` sites, §8.4-12), `themes.test.js` with the full pair matrix (§9.1) + validation pairs, rendered proof that default dark/light are visually unchanged (minus the fixes) | **~1 dev-day** |
| W2 | `theme-boot.js` + `base.html`, `theme.js` rewrite + `theme_menu.html` + `header.html` (+ the `web/package.json` entry), swatch pinning, print/forced-colors blocks, docs amendments | **~0.5 day** |
| W3 | the five palettes + ramp overrides from §4/§5 tables — **need before mood: `hc-dark`, `hc-light` first, then `ocean`, `paper`, `terminal`** — full rendered smoke ×7 themes ×2 widths as each lands | **~0.5–0.75 day** |
| W4 | evidence pass, mobile screenshots into the PR, doc cross-links (the print block from §8.3 may slide here if W2 runs long — it is the one deferrable item) | **~0.25 day** |

Per-theme effort (W3 view): each HC theme ≈ 1 h (data + gate), each expressive
theme ≈ 2 h (data + ramp + smoke review). The cost is concentrated in W1/W2,
which exist once.

### 10.4 Risks

- **Go-emitted colour utilities are the bug class this plan nearly shipped.**
  `checkStateDot` built `bg-emerald-500`-style strings in Go, invisible to a
  template-only grep, and §5.2's ramp deletion would have regressed them
  silently. The guard is §9.1 pair 25's Go-inclusive grep — treat weakening it
  as a review rejection.
- **A theme that fails the amber-action rule** (§1.3-3) — ocean's azure accent
  is the risky one: it is blue, and blue is also "default link" conditioning.
  Mitigations: links stay ink+underline (§1.3-5), prerelease was moved to pale
  cyan (§4.6), and the W3 review explicitly checks that azure appears *only*
  on primaries/active/focus. The mechanical part (accent appears only in the
  sanctioned component rules) is a review item, not a test — grep the built
  CSS for `--pd-accent` consumers and diff the list against app.css's current
  one.
- **Contrast at small text**: the chips are 10 px mono caps — the gate holds
  them to 4.5:1, and dark's chip-danger sits at **4.51** with no headroom; if
  a future state darkens that ground, the gate catches it, but reviewers
  should treat chip-danger/dark as "do not touch without re-measuring".
- **Ramp-override coupling**: the whole mechanism assumes utilities compile to
  variable references — that is true of the current build (§1.3-2) and
  asserted at build time (§9.1 pair 25); a Tailwind upgrade that inlines theme
  values (`@theme inline`) would break every theme silently.
- **The `[data-theme]` blocks are 5 × ~35 tokens of authored CSS** — a typo
  class of bug the completeness + purity checks (§9.1 pair 24/26) are for.
- **`prefers-contrast` auto-mapping surprise**: a user with HC OS settings gets
  an HC theme without asking — only in system mode, and the menu always wins
  (§7.5); state it in the menu's `match system` tooltip.
- **Print forcing light** will surprise a dark-theme user printing code —
  accepted (paper is paper); revisit if anyone prints enough to complain.

### 10.5 Decisions taken (owner, 2026-11-21)

1. **First-visit OS honouring — keep §7.1 as written.** A light-OS first-time
   visitor sees light; every no-signal case (no JS, no stored choice, no OS
   preference) still lands dark, and "match system" is honest from the first
   visit. The one-line flip to always-introduce-dark stays documented above
   for the record, but is *not* taken; if it ever is, "match system" becomes
   the only OS-following path and the menu tooltip must say so.
2. **Default `dim` — do not raise it.** The documented deviations stand
   (defaults 2.4:1 floor, HC themes 4.5:1 — deliberately *not* 7, recorded in
   §3/§4.3/§4.4 exactly like `line-strong`): `dim` is decorative by contract
   (guideline F2), its one borderline use (gutter anchors) has an accent hover
   state, and the HC themes serve the low-vision need. Revisit only on a real
   user complaint, not as precaution.
3. **No 4th expressive theme now.** The violet/dusk rejection stands
   (state-vocab collision, §3); the **warm dark (rust/ember)** is the
   recorded future candidate once this contract has shipped and been
   load-tested by the five (§10.1).

---

## 11. W1 as built (2026-11-21)

W1 landed the token contract, the §8.4 conversions, the folded contrast fixes
and the gate. Recorded here because §4/§5 are the *target* tables (W3); these
are the as-built values for the two shipped themes.

**Token contract (34 roles).** `:root`/`.dark` gained `--pd-ok-bg`,
`--pd-warn-bg`, `--pd-danger-bg`, `--pd-merged`, `--pd-merged-bg`,
`--pd-prerelease`, `--pd-prerelease-bg`, `--pd-link`, `--pd-code-bg`,
`--pd-kw`, `--pd-info-bg`. Consumers landed with them (§8.4-10): chips, `.tok-kw`,
`markdown pre`/`.code-view`, `.link`/`.markdown-body a`, `.setup-callout`. The
`emerald`/`amber` `@theme` retunes are deleted (§5.2); `themes.test.js`'s
dead-token check is what keeps a token from landing unconsumed.

**Folded fixes, measured before → after.** light chip-merged 2.19 → **5.57**,
light chip-prerelease 1.76 → **5.31**, light `.tok-kw` 2.72 → **7.10**, dark
11px setup notes 3.96 → **4.89** (ramp zinc-500), light setup notes 2.64 →
**5.07** (ramp zinc-400), light chip-open/draft 4.34/4.30 → **4.62/4.61**
(ground tokens). From the a11y findings file: visible `dim` text moved to
`--pd-muted` (footer, status-strip key, commit email/parents label, input
placeholder); `muted` itself moved so it passes on **raised**, not just
surface/canvas — dark `#79838b` → `#818b93` (4.20 → **4.68** on raised, 5.14
on surface), light `#6f7378` → `#666a6f` (4.15 → **4.73** on raised, 5.08 on
surface). §10.5-2 stands: `--pd-dim` itself is unchanged and its gutter use
remains the documented 2.4:1-floor deviation.

**One §5.1 doc bug fixed.** The light 400 → `#6a6f74` override inverted the
composed ramp through the unchanged 500 (`#6f7378` is *lighter* than
`#6a6f74`), which §5.1's own monotonicity assertion catches. Light 500 also
moves to `#676b70`, restoring strictly-darkening 50–600.

**The gate.** `web/test/unit/themes.test.js` parses `app.css`, validates the
WCAG formula against three documented pairs, then asserts: completeness of the
34-token contract in every theme block, liveness (dead-token), declaration-only
`[data-theme]` blocks and no `[data-theme]` in any other selector, every pair
of the matrix (text 4.5, UI 3, the two documented deviations at their floors),
±0.05 drift on the documented key pairs, per-side ramp monotonicity, the
raw-ramp grep over templates **and** `internal/webui/*.go`, and (when built)
that `web/dist/app.css` still references `var(--color-zinc-*)` and carries no
emerald/amber utility.

**Still W2/W3.** W2 owns selection: `theme-boot.js`, the `base.html` script
tag, the `theme.js` menu rewrite, `theme_menu.html` (§7.4, including the
swatch literals and pair 23), print + forced-colors blocks, the `D-WEB-16`
deviation entry. W3 owns the five palettes and their ramp tables; the gate's
`[data-theme]` checks and the drift table extend with them.

## 12. W2 as built (2026-11-21)

Selection is live with the two shipped palettes; a palette is now **data**.

**Resolution and painting.** `web/src/islands/themes.js` is the one registry —
`{id, side, swatch:{canvas,text,accent}}` per theme, plus the pure
`resolveTheme`/`resolveChoice`/`applyTheme` helpers. `web/src/theme-boot.js`
(second esbuild entry, IIFE, classic `<script>` right after the stylesheet) runs
`resolveChoice` over `localStorage["spindle-theme"]` → `prefers-color-scheme`
(→ the side's `hc-*` when the OS also asks for more contrast *and* that palette
is registered) → `dark`, then writes `data-theme` + the `.dark` side class on
`<html>` and a `data-theme-boot` first-paint marker. `base.html` ships
`<html lang="en" class="dark" data-theme="dark">` as the no-JS floor. Storage
denied resolves like an absent value; the island applies a clicked choice
directly, so a failed write still themes the page. `/_ui/theme-boot.js` is
registered in `uiAsset` (no-cache + ETag, stable name).

**The menu.** `partials/theme_menu.html` is server markup: an `.icon-btn`
trigger (`aria-label="Theme"`, `aria-haspopup="menu"`) over a `role="menu"`
panel of `role="menuitemradio"` + `aria-checked` rows — "Match system" first,
then one row per theme with a three-chip swatch. Per §7.4's intent, but stricter:
the swatch literals do **not** live in the partial; the island writes them from
the registry (CSSOM, no inline style attribute), so the template carries no
colour at all and the gate derives the expected values from `app.css`. The
island marks the current choice from storage, applies + persists on click,
closes back to the trigger, and subscribes to `matchMedia` changes only while
the choice is "Match system". Keyboard (Enter/Space opens to the first row,
arrows wrap, Home/End, Esc closes and returns focus, outside click, Tab out) is
`menus.js`'s existing contract; the panel rides the structural
`max-width: calc(100vw - 1rem)` cap.

**Print and forced colours** (`app.css`, last two rules). `@media print`
re-asserts the light token set on `:root, .dark, [data-theme]` — measured values,
not references — with the chip grounds / diff tints / selection / `--pd-info-bg`
dropped to `transparent`, `--pd-shadow: none`, the header opaque, and the theme
menu + status strip hidden; the gate compares it token-for-token against
`:root` with an explicit override list, and re-runs the text pairs on paper.
`@media (forced-colors: active)` declares no colour: `forced-color-adjust: auto`,
`--pd-shadow: none`, control borders, the diff gutter divider and link
underlines. Print is the one §10.3 deferrable item and landed in W2.

**Gate additions** (`web/test/unit/themes.test.js`). Registry ↔ CSS theme blocks
(side derived from the measured canvas, not trusted); swatch completeness and
per-chip equality with `--pd-canvas`/`--pd-text`/`--pd-accent`; menu keys ≡
registry + `system`, one row per key, no colour literal in the template;
`theme-boot.js` carries no palette and no hardcoded id, and `base.html` loads it
as a classic script between the stylesheet and the island bundle with the SSR
default present; resolution order incl. stored-over-OS and stale-id fallback;
storage-denial; `applyTheme` side-class behaviour (stub document); print
token-for-token mirror + paper contrast + declaration-only shape; forced-colours
no-colour assertion.

**Rendered proof** (headless Chromium 153 over CDP, desktop 1440 and 390):
stored `light` + OS `dark` paints `light` in the first-frame probe
(`data-theme`/`.dark`/body background already light at `readyState="loading"`),
stored `dark` + OS `light` the mirror; cold no-choice follows the OS; the menu
opens by keyboard, arrows/Home/End/Esc behave, selecting applies + persists and
returns focus; match-system re-resolves live on an OS flip; storage-denied
renders, selects, and only loses persistence; `documentElement.scrollWidth ===
clientWidth` at 390 with the menu open in both sides; print media renders the
paper palette from a dark choice (PDF captured) and hides the menu; forced
colours renders the OS palette with underlined links; console clean.

**W3 adds, with no code:** one `web/src/islands/themes.js` entry per palette
(id, side, swatch), one `[data-theme="…"]` token block + its ramp steps in
`app.css`, one row in `partials/theme_menu.html`, and the palette's documented
pairs in the gate's drift table. `hc-dark`/`hc-light` also light up
`prefers-contrast` automatically because `resolveTheme` maps through the
registry.

## 13. W3 as built (2026-11-21)

The five palettes are **data**, exactly as §12 promised: `web/src/app.css` has
one `[data-theme="…"]` block per palette (the complete 34-token contract + its
active side's zinc steps + `--graph-ring`), `web/src/islands/themes.js` has the
registry entry (id, side, swatch), `partials/theme_menu.html` has the row, and
`web/test/unit/themes.test.js` measures all seven. No new machinery: the boot
script, the island, and `theme.js` already read the registry.

**Token values: the §4.3–§4.7 tables, verbatim.** hc-dark/hc-light (§4.3/§4.4)
carry the identity at 7:1 text pairs (17.71/12.32/11.77 and
19.70/12.58/7.77) with 3.30/3.25 control borders and the documented `dim`
gutter deviation (4.99/5.04, gated at 4.5, not raised to 7 — §10.5-2). paper
(§4.5) is the warm cream world with the distinct `#fdfaf1` code ground (the
reason `--pd-code-bg` exists) and a terracotta action; ocean (§4.6) is deep navy
with azure action and the pale-cyan prerelease (`#a5e3f7`, ΔE ~29 from the
accent); terminal (§4.7) is the green-cast world with phosphor-grey text and
**amber kept as action** (a green accent would collide with the ok state on
every issue/PR page). §5.1 ramps are applied as written, including the review
fixes: hc-dark drops the inert 300 step, hc-light gains the 700 override, and
terminal's 300/400 are in the corrected order.

**Values changed from the plan: none.** Every palette value is §4/§5 verbatim.
Two *documented ratios* differ from the plan's rounded key-pair lines and were
re-measured from the alpha blends rather than copied: hc-dark `add-fg` on canvas
is **11.71** (plan 11.77) and ocean's is **11.11** (plan 11.16). Both pass and
are pinned at the measured value in the drift table; the palette bytes did not
move.

**One addition inside the blocks (not a new token): `--graph-ring`.** §5.2
pins which *lane* set each side reuses but says nothing about the graph halo,
which is `box-shadow: 0 0 0 2px var(--graph-ring)` around every lane dot. Left
to the side default, paper would draw a `#f7f7f6` halo on `#f2ecdc` cards. Each
palette block therefore sets `--graph-ring` to its own `--pd-surface`, and the
gate asserts `graph-ring === surface` for all seven (the defaults already
complied). It is a colour custom property in a declaration-only block, so §1.3
is unchanged.

**Print now resets the whole ramp.** W2's print block set only the light
400/500 steps because those were the only overrides; the palettes retune up to
eleven steps, so `@media print` re-asserts the full `@theme` ramp (50–950) plus
the composed light 400/500 and the light graph lanes. The gate compares every
step, not two, so no palette's ramp can survive into print.

**The gate, extended (still no exclusions).** `themes.test.js` composes each
theme from the real cascade (`@theme` → its side's base → its block), derives
the side from the measured canvas, and for **all seven**: the full pair matrix
with HC floors (body/secondary/button label 7:1, border 3:1, `dim` 4.5:1),
per-theme drift pins (±0.05; the drift roster must equal the painted roster),
per-side ramp monotonicity of each composed ramp, the print mirror on every
ramp step, plus three new structural rules: green stays the state family
(`--pd-ok` hue 140–180° in every palette) while the action accent stays out of
that band, links equal `--pd-text` in every palette, and `--graph-ring ===
--pd-surface`. The dark/light drift rows are unchanged; their two documented
deviations are now pinned too (border 1.63/1.56, `dim` 2.61/3.04), so the
shipped pair is provably untouched.

**Rendered proof** (headless Chromium 153 over CDP; desktop 1440×900 and
390×844; six pages — `/`, repo tree, PR conversation, PR files/diff, a commit,
`/admin/config` — per theme): `data-theme`, the pre-paint `data-theme-boot`
marker, the `.dark` side class and the computed body background/colour match
the palette in all 84 loads; exactly one `.primary` (or none on the diff page);
`document.documentElement.scrollWidth === clientWidth` at both widths; chips,
dots and diff rows render; `/admin/config`'s
`text-zinc-500 dark:text-zinc-400` example resolves to each theme's composed
ramp step (proving the side class, the `@custom-variant` and the per-theme ramp
override all compose); console clean. The theme menu was keyboard-driven once
per theme (Enter opens, arrows reach the row, Enter applies + persists, focus
returns), then reloaded: the boot marker and body background were already the
chosen theme, so persistence survives and paints before content. With no stored
choice and the OS asking for more contrast, system mode landed on `hc-dark` /
`hc-light` (§7.5 now has palettes to land on); the menu open at 390 px stayed
inside the viewport with no page pan. Screenshots:
`/var/tmp/sp-tmp/shots-w3/` (outside the tree). **Remaining: nothing for W3**;
the W4 evidence pass (mobile screenshots into the PR, doc cross-links) is
separate.
