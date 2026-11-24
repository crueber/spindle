# a11y-contrast-findings.md — the colour defect list for the themes pass

Measured on the rendered app (real headless Chromium, computed colours with
alpha composited onto the effective background) at commit `feat/accessibility`,
in **both** themes, on 2026-11-21. Every ratio is foreground : background as
WCAG 2.x computes it. Requirements: **4.5:1 body text** (small text; the app's
smallest text is 10–13px, never "large"), **3:1 UI boundaries and non-text
state** (WCAG 1.4.11).

The non-colour defects found in the same audit (skip link, one-h1 + markdown
demotion, table scopes/labels, target sizes, `aria-busy`, reduced motion, the
chip cascade bug) are **fixed in this branch**; everything below is palette
work and is deliberately left to the themes pass.

## Fixed in this branch, but colour-related (context)

- **State chips rendered neutral in BOTH themes** (cascade bug, now fixed):
  `.chip` was declared after the bare `.chip-open/-closed/-draft/-merged`
  variants in `web/src/app.css`, so every `class="chip chip-open"` element
  computed `color: var(--pd-text-2)` and `background: var(--pd-raised)` —
  measured live: `chip-open` computed `rgb(163,173,180)` on `rgb(28,33,37)`.
  The fix adds compound `.chip.chip-*` selectors (the `.btn.btn-active`
  precedent). The ratios below for chips are the **intended** (now active)
  palette.

## A. Text contrast failures

| # | Element / where | Dark ratio | Light ratio | Needs | Token to move | Suggested value (passes both themes) |
|---|---|---|---|---|---|---|
| A1 | `--pd-dim` text: footer, `.blob-num`/`.diff-num` line numbers, `.status-strip .key`, `.kv-key` | **2.77** (canvas) | **3.26** (canvas) | 4.5 | `--pd-dim` used for text | Either move these text uses to `--pd-muted`, or split the token: dark `#7a838a`, light `#6a6f74` (4.5:1 on surface; more headroom on canvas) |
| A2 | `.input::placeholder` (`--pd-dim` on surface) | **2.61** | **3.04** | 4.5 | `.input::placeholder` color | Use `--pd-muted` directly (dark `#7e8890+`, light `#696d72-`); placeholder is still text the user reads |
| A3 | `--pd-muted` on **raised**: `.diff-hunk` line, `.card-meta` | **4.20** | **4.15** | 4.5 | `--pd-muted` | dark `#818b93`, light `#666a6f` (both ≥4.5 on raised, ≥4.6 on surface) |
| A4 | `--pd-muted` on **surface**: `.eyebrow`, `.empty-hint`, `.setup-note` | 4.62 (passes) | **4.45** | 4.5 | `--pd-muted` | same values as A3 |
| A5 | chip-merged: raw `--color-violet-400` `#a78bfa` on its 18% tint | 5.30 (passes) | **2.32** | 4.5 | introduce `--pd-merged` (dark/light pair) | dark keeps `#a78bfa`; light `#6d28d9` (5.25 on its tint, 7.1 on white) — mirror the graph-lane pattern (light lanes are already 700-grade) |
| A6 | chip-prerelease: raw `--color-sky-400` `#38bdf8` on its 18% tint | 6.44 (passes) | **1.86** | 4.5 | introduce `--pd-prerelease` | dark keeps `#38bdf8`; light `#075985` (5.65 on its tint; `#0369a1` = 4.54 also passes) |
| A7 | `.tok-kw` (syntax keywords): raw `--color-violet-400` on canvas | 6.95 (passes) | **2.72** | 4.5 | `.tok-kw` (share `--pd-merged`) | dark `#a78bfa`, light `#7c3aed` (5.7 on white) |

Everything else measured passes with headroom: body 15.6/17.8, links 15.6/17.8,
`.btn` 7.1/6.2, `.btn.primary` 10.5/5.4, `.err-line` (danger) 5.9/6.6,
`.warn-line` 9.0/5.9, chips open/closed/draft 4.6–7.0 both themes, diff
add-fg 11.5/7.4, del-fg 10.5/7.9, `.tok-str` 9.2/5.8, `.tok-num` 10.5/5.4,
selection 13.0/15.7.

## B. UI boundary failures (WCAG 1.4.11 — 3:1)

| # | Boundary | Dark | Light | Needs | Note |
|---|---|---|---|---|---|
| B1 | `.input` / `.btn` border (`--pd-line-strong`) vs its background | **1.63** (vs canvas) / **1.40** (vs raised) | **1.56** / **1.36** | 3.0 | The form-control boundary is the load-bearing one: the user must see where the field is. Suggested `--pd-line-strong`: dark `#5c656b`, light `#8f908c` |
| B2 | Decorative card hairlines (`--pd-line` vs surface) | **1.23** | **1.21** | — | Decorative panel edges are **exempt** (not needed for identification). Keep the hairline; only controls must meet 3:1 |
| B3 | Focus ring (`--pd-accent` vs canvas) | 10.5 | 5.4 | 3.0 | passes both themes |
| B4 | Line-selection edge (`--pd-sel-edge`) | 10.5 | 4.24 | 3.0 | passes |
| B5 | Check-state dots (ok/warn/danger vs canvas) | 5.9–9.2 | 5.8–6.6 | 3.0 | pass as non-text colour |

## C. Colour-only state (WCAG 1.4.1) — needs a design decision

- **Commit check pills are dot-only** (`partials/repo_check_pill.html`): state
  is a coloured dot (`checkStateDot`); the accessible name lives on the
  anchor's `title`/`aria-label`, but a *sighted hue-blind* reader gets pass /
  fail / pending / neutral as hue alone. Recommend one shape channel (filled =
  pass, ring = pending, ✕ = fail, hollow = none) or a two-letter label.
  Deliberately not changed here: it is the one place the minimal "dot IS the
  label" design language and the palette pass must move together.
- **Everything else already carries a non-colour signal**: state chips carry
  text (Open/Closed/Merged/Draft), diffs carry the `+`/`-` sign column, label
  chips carry the name, milestone chips carry icons. Verified in the audit.
- **Table row hover** (`.data-table tbody tr:hover`) is a background change
  with no focus/link affordance change — acceptable (rows are not controls),
  but the themes pass should not make hover the only signal for the active
  row anywhere new.

## D. Misc

- `prefers-reduced-motion` was absent — now added globally in
  `web/src/app.css` (transitions + progress easing die under the media
  query). The themes pass must not add animations outside this rule.
- `autocomplete="new-password"` is used on `/setup` secret inputs — correct
  (prevents browser autofill of saved passwords); keep it.
- `<html lang="en">`, per-page titles, avatar `alt=""` (decorative, adjacent
  name), concept GIF alts (detailed), `<time>` titles: all verified present.
