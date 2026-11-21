# Style guideline — the spindle UI design language

> Status: **normative, not advisory.** This document codifies the app's
> de-facto design language with canonical references (the file and the
> ticket/doc that established each rule), read out of the current tree.
> It is binding for all UI work via the AGENTS.md §2 working rule
> ("The style guideline is binding for all UI work", Forgejo #533).
> Where this guideline conflicts with AGENTS.md law 1 (dependency budget)
> or the Tailwind-only rule, the law wins and this guideline is corrected.
>
> One source of truth: rules already normative in AGENTS.md or
> `docs/go/12_web_ui.md` are **referenced, not restated** — restatements
> drift. Section numbers below name the authoritative location.
>
> Since D-WEB-14/D-WEB-15 the UI is **server-rendered**: canonical references are
> Go templates/partials (`internal/webui/templates/…`), page handlers
> (`internal/webui/*.go`), vanilla-ES islands (`web/src/islands/*.js`) and the
> stylesheet (`web/src/app.css`). Line/selector numbers cite the tree at the time
> of writing; the cited file + ticket is the reference, not the line number.
> SPA-era `.jsx`/`web/src/lib` paths appear only as explicitly historical notes.

## 0. What lives where (authoritative locations)

- Stack, dependency budget, build, dark-by-default, no-TypeScript:
  AGENTS.md law 1 and `docs/go/12_web_ui.md` header note + §2.2.
- Page seam, gating parity, writes, URL state: `docs/go/12_web_ui.md`
  §2.1–§2.5/§2.9; implementation `internal/pages/seam.go`,
  `internal/server/bind_ui.go`, `internal/webui/*.go`.
- htmx usage and island lifecycle: `docs/go/12_web_ui.md` §2.6–§2.7;
  `web/src/islands/index.js`.
- Setup page rendering/validation/save contract:
  `docs/go/12_web_ui.md` §2.10; implementation
  `internal/webui/settings.go`, `internal/webui/templates/pages/settings/setup.html`,
  `internal/webui/templates/partials/settings_setup.html`.
- Repo chrome, ref/clone UI, WAL/tasks/settings page contracts:
  `docs/go/12_web_ui.md` §2.3, §2.9; implementation
  `internal/webui/templates/partials/repo_header.html`, `repo_tabs.html`.
- Server renderers (the XSS boundary): `docs/go/12_web_ui.md` §2.8;
  `internal/webui/{markdown,diff,graph,highlight}.go`.
- Mobile-viewport check, Tailwind-only composition, rendered-proof rule:
  AGENTS.md §2 working rules (Forgejo #533).

## 1. Foundations — “Precision Dark”

**Adopted 2026-11-20 (Forgejo: design-language review). Supersedes the
emerald-on-zinc language.** The comparison document that chose this direction is
`docs/design/DESIGN-LANGUAGE-PROPOSALS.md` (direction A, “Precision Dark”);
its wireframe is `docs/design/mocks/A/repo.html`. This section is the normative
instantiation: if a screen disagrees with it, the screen is wrong.

The language in one sentence: **a dense, flat, terminal-native tool — graphite
panes, one amber accent that means *action*, monospace for every identifier, and
colour otherwise reserved for state.**

**F1 — Dark is the shipped default; light is the base layer.**
`internal/webui/templates/base.html` carries `<html lang="en" class="dark">`;
the header toggle (`partials/header.html`, `data-theme-toggle`) is driven by
`web/src/islands/theme.js`, which persists the choice in localStorage
(`spindle-theme`, dark stays the default when storage is unavailable) and syncs
the `.dark` class. `web/src/app.css` wires it: Tailwind v4 CSS-first
(`@import "tailwindcss"`, no config file, no CDN) with the class-based dark
variant (`@custom-variant dark`, never `prefers-color-scheme`, because the
toggle is a deliberate user choice). Every shared class ships light as the base
and carries the dark theme in `dark:` variants. Because pages are server-
rendered, a swapped fragment inherits the class already on `<html>` — an island
must never re-apply the theme to a subtree. (AGENTS.md law 1, D-WEB-6/D-WEB-14;
`docs/go/12_web_ui.md` §2.2.)

**F2 — One token system, two themes.** Every surface, line and text colour comes
from the semantic variables below (`web/src/app.css`, `:root` = light,
`.dark` = dark). Components reference the variables; templates reference the
component classes or the Tailwind ramps. A hex literal in a template is a bug
(F2 previously; unchanged in force, retuned in value). Contrast is a gate, not a
preference: every pair in the table is measured, and the three text-carrying
pairs hold ≥ 4.5:1 in both themes.

| Token | Dark (default) | Light | Role |
|---|---|---|---|
| `--pd-canvas` | `#0f1113` | `#ffffff` | page background |
| `--pd-surface` | `#14181b` | `#f7f7f6` | panes, cards, table bodies |
| `--pd-raised` | `#1c2125` | `#efefed` | inputs, chips, hover rows |
| `--pd-raised-2` | `#22282c` | `#e6e6e3` | pressed / selected rows |
| `--pd-line` | `#232a2e` | `#e2e2df` | hairline borders (the default border) |
| `--pd-line-strong` | `#323a3f` | `#cfcfca` | control borders, dividers that must read |
| `--pd-text` | `#e6eaec` | `#17181a` | primary text (15.6:1 / 16.6:1) |
| `--pd-text-2` | `#a3adb4` | `#55585d` | secondary text (7.8:1 / 6.7:1) |
| `--pd-muted` | `#79838b` | `#6f7378` | metadata (4.6:1 / 4.8:1) |
| `--pd-dim` | `#525c63` | `#8a8f94` | decorative only — never text a user must read |
| `--pd-accent` | `#ffb224` | `#9a5b00` | **action**: primary button, focus, active branch |
| `--pd-accent-hover` | `#ffc862` | `#7a4600` | action hover |
| `--pd-on-accent` | `#151004` | `#ffffff` | text on the accent (10.5:1 / 5.4:1) |
| `--pd-ok` | `#4ec9a4` | `#0f7355` | open / passing / healthy |
| `--pd-warn` | `#e0a95e` | `#8a5a10` | draft / degraded |
| `--pd-danger` | `#e06c6c` | `#b42323` | closed / failing / destructive |

**F3 — Amber means *action*, never *link*.** Exactly one primary action per
page owns the accent (`pages/collab/issues.html` toolbar remains the reference).
On repository pages the masthead Clone is that action only when the page body
has no primary of its own; a page with New issue / New pull request / Save /
Comment / Create tag demotes Clone to a secondary `.btn` (the audit F1 nit —
two amber CTAs read as two equal actions). Links are ink: `--pd-text` with a
1 px underline at 40 % opacity that goes solid on hover and takes the accent on
focus. `.link` keeps its role as the one text-action treatment but is no longer
emerald. Amber on *state* is forbidden: an issue is open (`--pd-ok`), merged
(violet-free: `--pd-text-2` on `--pd-raised`), or closed (`--pd-danger`).

**F4 — Flat panes, hairline depth.** Radius never exceeds 4 px (`--radius-sm:
2px`, `--radius-md: 3px`, `--radius-lg: 4px`, `--radius-xl: 6px`). Cards carry
no shadow: a pane is `--pd-surface` + 1 px `--pd-line` on `--pd-canvas`.
Elevation is reserved for popovers, which are opaque (§4) and may cast
`0 8px 24px rgb(0 0 0 / .45)` in dark / `0 8px 24px rgb(0 0 0 / .12)` in light.

**F5 — Monospace is the identity.** Refs, branch names, paths, SHAs, counts,
sizes, timestamps, key/value labels and the status strip are monospace
(`--font-mono`). Prose — README bodies, issue descriptions, settings copy — is
sans. A page has exactly these two voices; using mono for prose is a bug, and
using sans for an identifier is a missed signal.

**F6 — Density with a floor.** The type ramp is retuned (`--text-xs` 11 px,
`--text-sm` 13.5 px, `--text-base` 15 px, `--text-lg` 16.5 px, `--text-xl`
19 px, `--text-2xl` 23 px, `--text-3xl` 28 px). Table rows are 34 px, list rows
44–56 px by content, the top bar is 46 px, the repo status strip 26 px.
Micro-labels are 10 px uppercase with `0.08em` tracking (`.eyebrow`). Nothing
carrying information is smaller than 11 px on a touch viewport.

**F7 — The a11y floor: every keyboard-reachable control shows focus.**
`web/src/app.css` (`@layer base :focus-visible`) paints a 2 px `--pd-accent`
outline at 1 px offset in both themes — amber on graphite is 10.5:1. Popovers
keep a native-button keyboard baseline (Enter/Space toggle, Tab walks controls,
Esc closes and returns focus — `web/src/islands/menus.js`, Forgejo #214);
decorative glyphs and inline SVG icons are `aria-hidden` so accessible names
are untouched (e.g. `partials/header.html`, Forgejo #495 pattern).

**F8 — Narrow viewports never pan the page.**
Layout changes are verified rendered at ~390px as well as desktop
(AGENTS.md §2). The established repairs: nav strips scroll internally instead of
wrapping (§2); wide tables scroll in place inside `overflow-x-auto` wrappers
(issue #275 — `pages/repo/tree.html`, `partials/settings_repo.html` and
siblings); the graph rail hides at ≤480px (app.css media query; §9);
`break-words` (not scroll wrappers) contains unbroken strings in wrapping `kv`
cells (issue #275). The repo status strip is hidden below `sm:`.

## 2. Shared vocabulary — `web/src/app.css` component classes

Each class below is the ONE idiom for its pattern. New UI composes these
instead of re-deciding. (Tailwind-only rule, AGENTS.md §2, Forgejo #533:
a new rule earns its place only as a shared pattern here, per the #405
opaque-popover precedent.)

**Panels and chrome.**

- `.card` (`web/src/app.css`) — the panel: `--pd-surface`, a 1 px `--pd-line`
  border and radius 4, no shadow (§1 F4). Floating `.card`s are opaque and are
  the only elements that carry `--pd-shadow` (§4).
- `.card-meta` (issue #277, re-tuned 2026-11-20) — meta rows are a flex row
  with gap + wrap so adjacent spans never concatenate; monospace 11 px in
  `--pd-muted` (§1 F5), with explicit ` · ` separators in the markup. Its SPA
  consumer (`web/src/pages/Pull.jsx`) is historical (D-WEB-15); the class is the
  shared idiom new list-card meta rows must use. (Note: `web/css/repo.css` is
  dead/unbundled — the live stylesheet is `web/src/app.css`.)
- `.card-header` (Forgejo #521) — the ONE title treatment
  (`text-sm font-semibold`) for sidebar + conversation cards (Mergeability,
  Reviewers, Checks, Merge, Reviews, Finish review, Files). Reference:
  `partials/settings_repo.html` (`<h3 class="card-header">…`) and
  `pages/collab/pull.html` (Comments, Reviews, Finish review, Files, Status).
  Composes with layout utilities; cards MUST NOT
  drift apart into per-card title styles.
- `.site-header`, `.brand` — header chrome and wordmark
  (`partials/header.html`). The wordmark is monospace with the 8 px amber block
  that gives the product a mark (§1 F5); it is the only place the accent is
  decorative rather than an action.
- `.site-nav a`, `.nav-link` (issue #238) — one link treatment for the primary
  nav and for nav-styled links in the right-aligned utility cluster (the API
  link sits `ml-auto`, left of the theme/menu cluster — `partials/header.html`).
- `.owner-tabs a` (Forgejo #437) — the vertical profile tab list
  (`partials/public_owner_tabs.html`, `pages/public/owner.html`): stacked
  full-width links in a bordered box, no scrolling, no wrapping.

**Nav strips scroll internally at narrow widths — never wrap, never shrink
to unreadability.** `.site-nav` (issue #273, `partials/header.html` +
app.css), `.repo-tabs` (issue #274, `partials/repo_tabs.html` + app.css),
`.owner-tabs` is the vertical variant (Forgejo #437, no scroll rules). The
strip's own scrollbar is hidden; links stay keyboard-reachable (Tab walks, strip
follows focus) and touch-scrollable; the truncated peek of the next tab is the
scroll affordance. (The SPA scrolled the active repo tab into view on
navigation; server-rendered pages arrive with the active tab already marked
`aria-current="page"`.)

**Controls.**

- `.btn` / `.btn.primary` / `.btn.danger` — **amber is the primary action**
  (§1 F3). `.btn` is a 26 px control on `--pd-raised`; `.btn.primary` is the one
  amber CTA; `.btn.danger` carries `--pd-danger`. Exactly ONE primary CTA per
  page; the issues-list toolbar is the reference (`pages/collab/issues.html`:
  Labels / Milestones `.btn`, then one `.btn primary` "New issue").
- `.btn-active` — toggled control state: amber border + amber text on the
  raised surface, never a filled button (state filters, the graph toggle, the
  star/watch on-state). References: `pages/collab/pulls.html`,
  `pages/repo/commits.html`, `pages/settings/org.html`, `repo_header.html`.
- `.link` (Forgejo #568, retuned 2026-11-20) — the ONE shared treatment for
  text-level actions that must read as links, not surrounding text. **Ink, not
  accent**: `--pd-text` with a 35 %-opacity underline that goes solid on hover
  (§1 F3); focus takes the amber ring through `:focus-visible`. (review dismiss/remove, thread
  resolve/expand toggles, staged remove, member remove, token revoke, webhook
  ping/deliveries/delete, back-links, check expand toggles, notification
  actions). Accent emerald both themes (the markdown-link language), hover
  underline + pointer affordance. New text-link controls reuse it instead of
  composing per-site accent/underline utilities (the #405 shared-rule
  precedent). Deliberately a class, never a bare `a` rule — scoped link styles
  and nav treatments keep their contexts. Button-shaped actions (composer
  Cancels, staged edit/remove) stay `.btn`, never `.link` (#566/#567).
- `.pill` — filter chips, ref/branch badges, the doc-tab idiom. Monospace by
  default: a pill holds a ref, a label or a filename (§1 F5).
- `.tab-badge` (issue #319, retuned 2026-11-20) — a count **chip**, not a
  bubble: monospace digits on `--pd-raised` with a hairline, so a count never
  competes with its tab label. A tab omits the badge when it has no
  count to show (`partials/repo_tabs.html` renders it only for a nonzero
  `Badge`; the owner tab's repository count is always meaningful).
- `.input` — text-field utilities live here AND ONLY here. Never put `.input`
  on a non-text control (Forgejo #533 — it stretched a checkbox full-width and
  scattered the Settings Features rows). Non-text controls use dedicated
  markup: real `<input type="checkbox">` in a Tailwind peer pattern with
  `role="switch"` + `aria-checked` where it is a setting switch
  (`partials/settings_setup.html`, `partials/settings_repo.html`).
- `.eyebrow` (2026-11-20) — the micro-caps label (10 px, `0.08em` tracking,
  monospace, `--pd-muted`). Every sidebar/section label that names a region
  rather than speaking a sentence uses it (§1 F6).
- `.mono` (2026-11-20) — the opt-in identifier treatment: refs, SHAs, paths,
  counts, sizes and key/value labels. Prose never takes it (§1 F5).
- `.kv` / `.kv-row` / `.kv-key` / `.kv-val` (2026-11-20) — the About-rail
  key/value stack (`pages/repo/tree.html`): mono keys left, values right.
- `.status-strip` (2026-11-20) — the repository status strip (§1 F6): fixed to
  the viewport bottom above `sm:`, 26 px, monospace, carrying branch, rev and
  health. Pages that render it are padded by
  `body:has(.status-strip) main`; it is hidden on touch viewports.
- `.icon` (Forgejo #465) — the one sizing rule for inline SVG: `1em` /
  `currentColor` (`web/src/app.css`); templates inline the glyph with
  `aria-hidden="true"` and no per-icon CSS, no margins — row spacing stays with
  the caller's gap (#463). The shared icon set is the `collab_icon` template
  block (`partials/collab_common.html`) plus inline SVGs in `header.html`.

**Settings nav: `.side-nav-*`** (issues #123/#276, `web/src/app.css`) — sections
with `side-nav-heading` (+ `-danger`), native buttons/links with
`aria-current="page"` as the selected style, the Danger Zone in its own
danger-styled section (`partials/settings_repo.html`, `pages/settings/org.html`).
Stacks full-width above the content below `sm:`, side-by-side rows at `sm:`–`lg:`,
sticky sidebar at `lg:`+.

**Webhook panel** (`partials/settings_webhooks.html`, WP6.2) — the one canonical
create-form + hooks table + per-hook deliveries table used by repo settings and
the org Webhooks tab. One `.primary` (create); row actions are `link text-xs`;
delete is a `<details>` with the typed-id confirmation (`data-danger-expected`)
and `.btn danger`; deliveries render in their own `overflow-x-auto` `data-table`
(status, `duration_ms`, at, redeliver-or-retained-note). Never fork this markup
per surface — pass the plumbing (`SettingsWebhookPanel`) instead.

## 3. Page anatomy — the issues list is the canonical reference

`internal/webui/templates/pages/collab/issues.html` is the reference
implementation for list pages (handler `internal/webui/collab.go`). Sibling
tabs (Pulls, Releases — Releases reworked to match in issue #270) mirror it;
new list pages copy its anatomy and cite it.

- **Heading + toolbar row:** `h2` in `text-xl font-semibold tracking-tight`,
  toolbar right-aligned via `ml-auto flex gap-2` — secondary `.btn` links first
  (Labels, Milestones with leading icons, Forgejo #495), exactly one
  `.btn.primary` CTA last ("New issue"). Feature-gated CTAs disappear when the
  feature is off (`NewIssueHref` is empty; the handler decides, the template
  only renders what it is given — fail closed). The empty-state action mirrors
  the same href (`collab_empty`).
- **Filter bar** (issue #232): labelled fields in a grid card —
  `card mb-3 grid grid-cols-2 … sm:grid-cols-4`, 2-up on phones → 4+action
  wide. Selects and inputs are seeded from the query (`state`, `assignee`,
  `labels`, `milestone`); submitting is a plain GET.
- **Deep-link honesty:** URL state is resolvable or visibly degraded, never
  dropped. A deep link to a deleted value stays visible — the milestone filter
  carries `MilestoneUnknown` and renders the raw submitted id as the selected
  option (issue #416 binding comment). Pending states do not exist in the
  server render: the data is already resolved when the page is written.
- **Row meta** reads count → milestone → time: the comment count is a
  decorative glyph (`aria-hidden`) with the number in an accessible
  `aria-label`/hover title (Forgejo #380-#381 pattern); the milestone renders
  only when set, as a truncating `.chip` link to the filtered list
  (`partials/collab_common.html` `collab_milestone_chip`); the stamp is
  `collab_time` over the shared `rel` helper (`internal/webui/helpers.go`).
- **The branch is chosen where it changes the view** (2026-11-20; supersedes
  #252's header pill). The repository masthead carries no ref pill — two
  representations of the same branch competed with each other. The Code page
  toolbar owns the branch selector (`pages/repo/tree.html`: a `.btn` trigger +
  the shared `menus.js` popover, one link per head, the current head marked in
  amber); an empty repository says `empty` in the masthead chips. The handler's
  resolved ref remains the one truth.
- **Clone honesty** (issues #37/#124): the clone menu
  (`partials/repo_header.html`) shows the server's advertised URLs verbatim;
  the transport pill label derives from the URL scheme, so pill and text never
  disagree. Copy affordance: readonly textbox + Copy button (`data-copy`, the
  copy island), transient `role="status"` indication.

### 3.1 Repository code page — the canonical repository surface

`internal/webui/templates/pages/repo/tree.html` (handler `repoTree` in
`internal/webui/repo.go`) is the reference for every repository page. Anatomy,
top to bottom:

1. **Masthead** (`partials/repo_header.html`): `owner / name`, visibility chip,
   description, a monospace meta line (branch and tag counts), then the quiet
   counters and — on pages with no body primary — the page's ONE `.btn.primary`
   (Clone). A page that owns a primary action (New issue, New pull request,
   Save, Comment, Create tag) renders Clone as a secondary `.btn` (F3). No ref
   pill (§3).
2. **Tab strip** (`partials/repo_tabs.html`): active tab = amber bottom border.
3. **Toolbar**: branch selector, monospace path breadcrumb (only below the
   root), and the directory filter (`data-filter` → `web/src/islands/filter.js`,
   client-side over the rows already on the page — never a fetch).
4. **Commit strip**: the newest commit on the ref — author initial, subject,
   author, relative age, short SHA — from the same `Commits` call the log page
   uses. It is chrome: a failed or empty read renders no strip, never an error.
5. **File table**: type icon, name (`.link`), monospace size, monospace age.
   No Unix mode column. The wrapper is `overflow-x-auto` (issue #275).
6. **Docs panel** (`partials/repo_doctabs.html`): `?doc=` links, README first.
7. **About rail** (`lg:` only): `.kv` rows built exclusively from data the
   chrome already carries — stars, forks, watching, branches, tags. Nothing is
   invented; a value the handler does not have is not rendered.
8. **Status strip** (`partials/repo_status.html`): branch, rev, health and the
   branch count, fixed to the viewport bottom above `sm:`. It says only what the
   page knows — there is no fabricated CI state.

## 4. Popovers

- **Opaque backgrounds are the STRUCTURAL default** (Forgejo #405,
  `web/src/app.css`): `.card.absolute, .card.fixed` render solid
  `bg-white / dark:bg-zinc-900` via a higher-specificity rule ordered after
  `.card` — new dropdowns are covered automatically, no per-menu hook class
  needed. (The enumerated hook classes stay in markup as harmless markers.)
- **Viewport bound** (issue #278, `web/src/app.css`): every absolute/fixed
  panel (`.clone-body`, `.ref-drop`, `.tasks-drop`, `.notif-drop`,
  `.reaction-drop`, `.label-drop`, `.milestone-drop`, `.close-drop`, `.tag-drop`,
  `.tray`) carries `max-width: calc(100vw - 1rem)`. Desktop widths untouched —
  the bound only caps. Deliberately unbounded: `.progress` (fluid `inset-x-0`),
  inline forms, invisible helpers.
- **Behavior contract** (`web/src/islands/menus.js`; the clone menu in
  `partials/repo_header.html` and the identity/create menus in
  `partials/header.html` are the references; label/milestone pickers follow it):
  the trigger lives inside the root (`data-menu` > `data-menu-trigger` +
  `data-menu-panel`), the panel is bounds-capped, outside-click closes via a
  document-level listener, Esc closes and returns focus, ArrowUp/Down/Home/End
  walk items, Tab out dismisses, `role="menu"`/`role="menuitem"` with
  `aria-expanded` on the trigger. The native `<details data-popover>` variant
  (issue filters, label/milestone editors) keeps the same visual rules and is
  JS-free.
- **`.scroll-slim`** (issue #115, `web/src/app.css`) for popover lists:
  thin themed scrollbars both engines, both themes.
- **Release tag combobox** (Forgejo #254) follows the same shape: `.tag-drop`
  popover, solid-panel + `.scroll-slim`, menu keyboard conventions,
  outside-click close.

## 5. Empty states

`.empty-state` / `.empty-state-compact` (issues #34/#35, `web/src/app.css`):
icon/title/hint composition, generous padding, dashed panel — rendered through
the shared `collab_empty` block (`partials/collab_common.html`; link actions
stay keyboard-focusable; `compact` for narrow sidebars; `role="status"`).
Zero-data renders "No X configured" + setup guidance, never a machine-internal
catch-all. The empty-repo guide (`partials/repo_guide.html`) and mirror waiting
state (`partials/repo_degraded.html` — mirrors NEVER show the push guide,
Forgejo #281) follow the same truthful-empty language. The exactly-ONE-CTA
pairing (toolbar primary + Empty action) is the issues pattern (issue #50 as
amended by #270).

## 6. State chips

`.chip-open` / `.chip-closed` / `.chip-merged` / `.chip-draft` /
`.chip-prerelease` / `.chip-neutral` (`web/src/app.css`) — the state→color
mapping is fixed (emerald / red / purple / amber / sky / zinc). Consumers:
`pages/collab/issue.html` (open/closed), the pull-state helper in
`internal/webui/collab.go` (open/closed/merged mapping),
`pages/collab/releases.html` (draft/prerelease), and the PR page's review
verdict chip (Forgejo #545 — APPROVED → chip-open, CHANGES_REQUESTED →
chip-closed, COMMENTED/dismissed/requested → chip-neutral,
REVIEW_REQUIRED/unknown → chip-draft). `.chip-neutral` is the no-signal chip for
states that carry no color meaning. New states extend the family; inline colors
are never used for state.

## 7. Forms

- **Setup rows** (`.setup-row` / `.setup-label` / `.setup-examples` /
  `.setup-note` / `.setup-callout`, `web/src/app.css`;
  `partials/settings_setup.html`): label column left, control right, tight
  gutter; stacks to one column on phones. Native `<details>` for collapsible
  Advanced groups (issue #168 — `.setup-advanced`; collapsed rows stay mounted
  so the save payload is complete).
- **Struct-slice row editors** (`.setup-rows` / `.setup-rows-legend`,
  `partials/settings_setup.html`; `internal/setup/rows.go`): a repeating value
  (static auth tokens, bundle strategies) is one bordered fieldset per element
  with the element's name as the legend, its cells in a one/two-column grid of
  labelled inputs/switch pairs, and a per-row "remove this row" checkbox; a
  blank "new row" fieldset is always last (fill it to add — no JS required).
  Secret cells use a `type="password"` input holding the mask; the save path is
  the same `Coerce` path as a scalar key.
- **Composers** (issues #34/#49): centered `max-w-2xl` page
  (`pages/forms/issue_new.html`, `pages/forms/release_new.html`), section
  fieldsets with per-field help, inline `role="alert"` error line — no native
  `required` bubble; the inline error is the single error channel. Autogrow and
  submit-key behavior are islands (`data-autogrow`, `data-submit-keys`:
  `web/src/islands/autogrow.js`, `submitkeys.js`).
- **Danger Zone** (`web/src/islands/danger.js`, issue #39; markup hooks
  `data-danger-confirm`/`data-danger-expected` + `data-danger-input`/
  `data-danger-submit`, e.g. `partials/settings_repo.html`,
  `partials/settings_org.html`): typed exact-match confirm. The SERVER requires
  the typed text to equal the expected `owner/name` exactly — case-sensitive,
  untrimmed (`settingsRepoWrite`: `confirm != owner+"/"+name`) — and re-checks
  it on submit. The island only arms the button (its trim is UI convenience,
  never the boundary); destructive actions are impossible without the server
  match.

## 8. Tables and code views

- **`.data-table`**: every non-`kv` data table sits in an `overflow-x-auto`
  wrapper so it scrolls in place (issue #275); rows inside such wrappers stay
  single-line via the `.overflow-x-auto .data-table td` nowrap rule. `kv`
  tables are deliberately unwrapped (cells wrap; `break-words` contains
  unbroken strings — §F4). A table whose cells are prose rather than values
  opts into `.data-table.wrap` (the effective-policy Effect summary): the
  shared rule turns nowrap off, declared after the wrapper rule so the wrap
  intent wins, and the card still carries `min-w-0` + the wrapper for 390 px.
  A scroll wrapper must live in a `min-w-0` ancestor: a grid/flex item's
  automatic minimum width is its content, so without `min-w-0` the wrapper
  stretches to the table and the PAGE pans instead of the table scrolling.
  A scroll wrapper that contains `.sr-only` (absolutely positioned) must also
  be `relative` — otherwise the span's containing block is the page, and its
  static position in a far column keeps `documentElement.scrollWidth` wide
  even though the table itself is clipped.
  The combination `relative overflow-x-auto` is the canonical wrapper for any wide table
  added inside a grid page (docs/features/14 §7).
- **`.tree-table`** (issue #223, `web/src/app.css`;
  `pages/repo/tree.html`): no header row, no type column — the mode lead char +
  icons carry the kind. Every column but the name hugs its content (`w-px` +
  nowrap); the name absorbs spare width; the size number/unit pair reads as one
  column. The mode column hides below `sm:` (`hidden sm:table-cell`).
- **`.blob-table`** (issue #243, `web/src/app.css`; `pages/repo/blob.html`):
  gutter number + code line share a per-line `<tr>` so the columns can never
  desync; same mono metrics throughout. Selection is the `selection` island
  (`web/src/islands/selection.js`): the gutter is a real `#L` anchor, the
  island toggles the shipped `line-hl` class and pushes a shareable hash —
  the page works with no JS. Amber tint + emerald gutter edge live in
  app.css (deliberately unlayered so they win).
- **Diffs** (issues #244/#544/#598; `internal/webui/diff.go` renders the typed
  rows, `partials/repo_diff.html` + `partials/collab_diff.html` lay them out):
  every row leads with its +/-/space **sign column** ahead of the line numbers
  (`select-none`), themed with the row (add green / del red via the shared
  `lineClass`, context muted). Exactly ONE gutter column (unified diff);
  the SPA's split/two-gutter `DiffTable` is historical. `.diff-add` / `.diff-del`
  row backgrounds apply per CELL (gutter + code both colored); line selection
  wins (`.diff-row.line-hl` is deliberately unlayered); no color literals in
  templates — colors live in `web/src/app.css` token composition (F2). File-
  scoped shareable hashes come from the same `#L` rule.
- **`.markdown-body`** (issue #182, `web/src/app.css`): designed prose covering
  everything the Go renderer emits (headings, code, tables, blockquotes,
  nested/task lists, hr, images, links); relative URLs resolve against the
  file's coordinates server-side (`internal/webui/markdown.go`) and `#N`/`PRN`
  autolinks to their threads (Forgejo #340), as do `@username` mentions
  (`linkifyMentions` was the SPA helper; the Go renderer owns the rule now,
  docs/features/06). **It fills its card** — README/blob preview, issue and PR
  bodies, comments, release notes, owner bios, commit bodies and doc tabs all
  use the full width of the column they sit in, the way GitHub's do. The base
  class carries no `max-width`/`width`/auto-margin measure cap; containment is
  per-media (images `max-width:100%; height:auto`, wide tables and `<pre>`
  scroll inside their own box), so the page never pans. A width cap on this
  class is a regression: `web/test/unit/markdown-width.test.js` pins it, and
  no template may pair `markdown-body` with a `w-*`/`max-w-*` utility. A parent
  that is a genuine column (the issue conversation column beside the details
  sidebar, a `max-w-2xl` form card) constrains the CARD; the body fills it.
- **Dates** (issues #133/#312): every timestamp renders through the ONE `rel`
  template helper (`internal/webui/helpers.go`) into a real `<time
  datetime="…" title="…">` — compact relative text, exact stamp and local wall
  time in the title, falsy → fallback with no `<time>`. Listing stamps ride the
  payload, never per-row fetches: `partials/public_repo_row.html` (issue #142)
  renders the listing's last-active stamp and the row's star count directly
  (issue #137 — the handler hands them over, placeholder-free).
- **Text helpers** (`.muted`, `.tabular`, `.err-line`, `.warn-line`,
  `web/src/app.css`).

## 9. Commit graph — the continuous-gutter rule

(Forgejo #506/#512 — `web/src/app.css` unlayered graph block,
`internal/webui/graph.go`, `pages/repo/commits.html`.)
Templates/SVG carry only `.gl-N` classes; lane colors live in `--graph-*` vars
(700-grade light, 400-grade dark). While graph-on, rows are margin-free
(`.commit-row, .commit-empty { margin: 0 }`) with padding-only separation:
row-level vertical padding is forbidden (it would sit outside the rail track and
break the gutter at every joint — each content column carries its own `py-2`
instead); `divide-y` row borders are dropped (the template applies them only
when the graph is OFF) and separation is an inset top edge on `.commit-main`
only (never the row's left edge), so the rail column stays a continuous strip.
The rail hides at ≤480px (rows fall back to 3 columns; parent info survives in
text). The graph is a `?graph=1` query link — a link is honest, a localStorage
preference would need JS.

## 10. Interaction and state conventions

- **Long work is never a silent spinner** (AGENTS.md law 7): tasks with
  unique ids, `(repo, kind)` single-flight, progress packets, attachable
  SSE streams. A page either polls a server-rendered fragment with htmx
  (`partials/collab_threads.html`: the merge-task fragment carries
  `hx-trigger="every 2s"` while running) or attaches to the event stream; there
  is no client state machine.
- **Error states are human-readable with retry** (`docs/go/12_web_ui.md`
  §2.4–§2.5): a failed read renders the error page (or an inline `.err-line`);
  a missing auxiliary record is missing data, not a crash — the handler omits
  the row (`tolerateMissing` semantics moved server-side); degraded reads warn
  inline via `.warn-line` (`partials/repo_degraded.html`, issue #209); no raw
  `TypeError` strings reach a user.
- **Anonymous writes route to `/login-required`** (Forgejo #502): the handler
  shapes every write affordance through `collabWriteHref` /
  `LoginHref` (`internal/webui/collab.go`); the page renders
  `/login-required?next=…&action=…` instead of a button that cannot work. The
  server re-checks authority on the write path (the URL has no privilege).
- **Event lifecycle** (issue #270): never touch an event's `currentTarget`
  after an `await` — capture the element synchronously first. Islands are
  idempotent and mark the nodes they own (`data-island-ready`); htmx swaps
  re-run `boot()` over the new subtree (`web/src/islands/index.js`).
- **State-derived visuals are spelled out per surface and state**: tab icons
  belong to the TAB's identity, never the viewed page; PR badges come from the
  server's state mapping (`internal/webui/collab.go`), not template-side
  conditionals that guess.
- **Listing sources ride the payload, never N fetches** (Forgejo #247):
  ordering, stamps, counts and badges (`mirror` pill —
  `partials/public_repo_row.html`, Forgejo #281) are built into the listing
  view models by the handler. No page issues a per-row probe.

## 11. Maintenance

A UI pattern becomes canonical ONLY by landing as a shared implementation
(an `web/src/app.css` class, a template partial under
`internal/webui/templates/partials/`, a page handler helper, or an island under
`web/src/islands/`) PLUS a guideline entry citing it in the SAME change. A
change that introduces a new pattern where a canonical one exists, or that
re-decides what this guideline settles, is rejected unless it either extends
this guideline in the same change (new canonical reference + rationale) or
carries an amendment in the relevant doc's "Decisions & deviations" section
(AGENTS.md law 12). This guideline never overrides AGENTS.md law 1 or the
Tailwind-only rule.

## Decisions & deviations from the Rust design

- **NEW (Forgejo #537) — this guideline.** The de-facto design language
  previously lived in `ui.css` comments, per-page JSX, scattered AGENTS.md
  working rules, and the cited tickets; it is now codified here with a
  canonical reference per rule, and made binding by the AGENTS.md §2
  amendment in the same change (law 12).
- **AMENDED (Forgejo #545) — `.chip-neutral` joins the state-chip family
  (§6).** Review verdicts with no color signal (COMMENTED, dismissed,
  requested) render zinc in both themes; the PR page's verdict mapping is the
  single mapping serving every verdict surface.
- **RE-POINTED (2026-11-20, D-WEB-14/D-WEB-15) — canonical references moved
  from the deleted SolidJS SPA to the server-rendered pages.** Every rule above
  stands; the file a rule cites is now the Go template/partial, the page
  handler, the island or `web/src/app.css`. Where the shipped shape changed
  materially, the rule says so in place (single unified diff gutter, server
  rows/count badges, `?graph=1` instead of client state); SPA paths are marked
  historical rather than silently deleted.
- **ADOPTED (2026-11-20) — “Precision Dark” replaces the emerald-on-zinc
  language.** Chosen from the three proposals in
  `docs/design/DESIGN-LANGUAGE-PROPOSALS.md` (direction A). Rationale: the brief
  was *functional differentiation*, and A is the direction that makes the
  product legible as a tool — dense rows, monospace identifiers, one amber
  action accent, a repo status strip. Two things carried over from the other
  proposals: B's ~72ch reading measure for rendered markdown (`.markdown-body`)
  — **SUPERSEDED 2026-11-21: the cap is removed; the markdown body fills its
  card (GitHub parity, §2)** — and C's rule that a shared class, not a page,
  owns shape. The raw Tailwind
  ramps (`zinc`, `emerald`, `amber`) are retuned in `@theme` so the hundreds of
  inline utilities in templates adopt the palette without a per-file edit; the
  semantic `--pd-*` roles are what components use. Nothing in the component
  vocabulary was removed — `.card`, `.btn`, `.link`, `.chip*`, `.pill`,
  `.data-table` keep their names and roles, so the migration is a retune, not a
  rewrite.
