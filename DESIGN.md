# DESIGN.md — working on the spindle UI and CSS

> How to build, style, and extend the UI so every page looks like one product.
> Stack context: **server-rendered pages** (Go `html/template` in `internal/webui`) + **vendored htmx**
> + small vanilla-ES islands, Tailwind v4 (CSS-first), dark-by-default — the decisions of record are
> `DEVIATIONS.md` D-WEB-14/D-WEB-15; the normative spec is `docs/go/12_web_ui.md` §2.

## 1. Non-negotiables

1. **No CDN, ever.** No external font, script, or image URLs. Fonts are the system stack; icons are unicode glyphs or inline SVG. Everything ships in the binary (`web/dist/` is embedded).
2. **Dark mode is the default.** `<html class="dark">` ships in `internal/webui/templates/base.html`. Never use `prefers-color-scheme` for the default — the theme is a deliberate user choice (`web/src/islands/theme.js`, persisted in localStorage under `spindle-theme`). Light styling is the base layer, `dark:` variants carry the dark theme (the custom variant is declared in `web/src/app.css`).
3. **No TypeScript, no framework, no client data layer.** Pages are Go templates; behavior is vanilla-ES islands (`web/src/islands/`); htmx is vendored (`web/vendor/`). No new npm dependencies without a written amendment (AGENTS.md Law 1) — the dev budget is exactly `vite`, `@tailwindcss/vite`, `tailwindcss`, `esbuild`, and there are no runtime npm dependencies.
4. **The dogfood rule.** A page handler calls the same Go service the JSON API calls (`api.Env` seams), never an HTTP loopback to our own API. The `repos.js` SDK remains for external consumers, agents and app code; pages do not load it.
5. **User bytes render only through the Go renderers.** `{{template}}`-escaped text by default; markdown, unified diffs, commit graphs and syntax highlighting go through `internal/webui/{markdown,diff,graph,highlight}.go` (`md`, `mdrepo`, `diff`, `graph`, `hl`). Never pass user-derived bytes through `template.HTML`, and never add a client-side sanitizer.

## 2. Files and build

```
internal/webui/templates/base.html          document shell — <html class="dark">, header/footer, {{template "content"}}
internal/webui/templates/partials/*.html    shared blocks, parsed into every page set
internal/webui/templates/pages/<group>/*.html  ONE file per page, defines "content"
internal/webui/*.go                         page handlers, PageData/Chrome, renderers
internal/pages/seam.go                      the provider seam internal/server mounts
internal/server/bind_ui.go                  pageOr404 / uiOr mounts (no page markup lives here)
web/src/app.css                             ALL styling: Tailwind import + tokens + component classes
web/src/islands/*.js                        behavior islands → web/dist/ui.js (esbuild)
web/vendor/htmx*.js                         vendored htmx 2.0.4 (+ SSE extension), copied to dist/
web/sdk/src/*.js                            dependency-free SDK → web/dist/repos.js (esbuild)
web/dist/                                   build output (gitignored except .keep) — embedded by the Go binary
```

`make web` = stylesheet (`vite` + `@tailwindcss/vite` → `app.css`) + islands (`esbuild` → `ui.js`) +
vendored htmx copy + SDK bundle (`repos.js`); `make build` depends on it. Page HTML is `no-cache` +
strong ETag; the stable page assets (`app.css`, `ui.js`, `htmx*.js`, `concepts/*.gif`) are the same
class; only the content-addressed `assets/*` lane is immutable. Templates hardcode `/_ui/…` — there is
no asset manifest to update.

## 3. Styling rules (web/src/app.css)

**Tokens live in `@theme`** (`--font-sans`, `--font-mono`). Colors come from Tailwind's palette and are used consistently:

| Role | Dark | Light | Utility pattern |
|---|---|---|---|
| Page background | `zinc-950` | `zinc-100` | on `<body>` (`base.html`) |
| Surface (cards) | `zinc-900/70` | `white` | `.card` |
| Borders | `zinc-800` | `zinc-200` | `.card`, `.data-table` |
| Primary text | `zinc-200` | `zinc-900` | on `<body>` |
| Muted text | `zinc-400` | `zinc-500` | `.muted` |
| Accent / links / active | `emerald-400`–`600` | `emerald-600`–`700` | links, `.btn.primary`, active tab |
| Danger | `red-400/600` | `red-600` | `.err-line`, `.tray-entry` |
| Warning | `amber-400/600` | `amber-600` | `.warn-line`, busy pills |

**Component classes** (in `@layer components`) are the vocabulary — USE THEM, don't re-invent (the
canonical-reference table is `docs/style-guideline.md` §2):

- `.card` — any panel. Compose with padding utilities (`p-4`) and `mt-4` between cards.
- `.btn` / `.btn.primary` — actions. One `.primary` per view (Save/Run).
- `.pill` — compact status/selector; `.chip-*` — tiny state labels ("open", "draft", "prerelease", booleans).
- `.input` — every text/select/textarea. Width via utilities (`md:w-72`, `md:max-w-md`).
- `.setup-row` / `.setup-label` / `.setup-examples` — the setup form: label column LEFT, control RIGHT, tight `gap-x-5 gap-y-1` gutter; one column on phones. `.setup-label` is bound to the control (`for`/`id`, `aria-labelledby` for the bool switch). `.setup-examples` (+ `.setup-ex`, `.setup-note` renders the field's working example under the label — it is real markup, never a placeholder, so it stays visible while typing; examples live in `internal/webui/settings.go` (`settingsFieldMeta`) and are asserted to validate by the setup tests. **Auth is its own card** after `server`; auth rows are mode-conditional, the card shows the copyable OIDC redirect URI (`<public_url|origin>/_auth/callback`) in oidc mode, and its **Test OIDC setup** button posts to `/setup/auth-test` (`hx-post`, a rendered fragment — never JSON).
- `.data-table` — **the table class. Never name anything `.grid`** — it collides with Tailwind's `display: grid` utility and silently breaks the table (this shipped once).
- `.code-view`, `.markdown-body`, `.diff-add/.diff-del/.diff-hunk`, `.tok-*` — rendering surfaces (filled by the Go renderers).
- `.muted`, `.tabular`, `.err-line`, `.warn-line`, `.progress`, `.tray` — chrome helpers.

**Dark/light discipline:** style the LIGHT look in the base utilities and add `dark:` for dark — the shipped default is dark, so every component class already carries both halves. Never hardcode a color that only works in one theme without the counterpart.

## 4. Layout and responsiveness

- The app shell is `flex min-h-screen flex-col` (`base.html`); `main` is `mx-auto w-full max-w-6xl px-4 py-6`. Pages NEVER set their own max-width — one container owns the rhythm.
- **Phone (≤ 640px) is a first-class target.** Rules that keep it working:
  - Every WIDE table (≥ 5 columns or long content) lives inside `<div class="overflow-x-auto">…</div>`
    — rows stay single-line (`.overflow-x-auto .data-table td { whitespace-nowrap }`) and the wrapper
    scrolls. 2-column KV tables may sit bare.
  - Repo header and button rows use `flex flex-wrap` + `gap-*` — never a fixed row.
  - Tab/tab-pill bars use `flex flex-wrap gap-1.5` — pills wrap, never squeeze (the site and repo nav strips are the deliberate exception: they scroll inside the strip, §F4 of the style guideline).
  - Form inputs are `w-full` with `md:max-w-md`/`md:w-72` caps on desktop.
  - Nothing may force `document.scrollWidth > innerWidth` — test at 390px.
- Breakpoints in use: default = phone, `md:` = ≥768 desktop, occasionally `sm:`. Don't add more.

## 5. Page and interaction conventions (htmx + islands)

- **URL state replaces client memory.** View state is a query parameter the handler reads:
  `?graph=1`, `?doc=<name>`, `?view=code`, `?all=1`, `?state=`/`?after=` on notifications,
  `?skip=`/`ref`/`path` pagers. Selected lines ride `#L<n>[-L<m>]`. Nothing is persisted locally except
  the theme.
- **Pages are GET/HEAD only.** Writes are sibling POST routes that `ParseForm`, run the same-origin
  check, call the service, then `303` back to the page (or `HX-Redirect` for an htmx post). Validation
  errors re-render the page with 400/422 and the submitted values.
- **htmx is for partial updates**, not navigation: `hx-post` + `hx-target` + `hx-select` swap a
  fragment; navigation stays plain links. SSE events trigger refetches
  (`hx-trigger="ui:sse-… from:body"`).
- **The SSE transport is an island** (`web/src/islands/sse.js`), never the htmx SSE extension: it
  needs cookies, headers, JSON payloads and a 401 signal, so it keeps a fetch stream with coalesced
  invalidation and backoff.
- **Island lifecycle:** `web/src/islands/index.js` boots idempotently on `DOMContentLoaded` and after
  every `htmx:afterSwap`/`afterSettle`; each island marks the nodes it took over
  (`data-island-ready`) so a swap never double-binds. Islands own: theme, menu popovers, copy, textarea
  autogrow, line selection, SSE, typed danger confirms, submit keys.
- **Errors are server-rendered.** A failed read renders the error page (or the honest "not wired"
  card); a missing repo/ref/path is a real 404 HTML page, never a 200 with a client-rendered error.
  Destructive actions need a typed confirmation re-validated server-side
  (`data-danger-confirm`/`data-danger-expected`; the island only arms the button).

## 6. Adding a page (checklist)

1. Create `internal/webui/templates/pages/<group>/<name>.html` defining `content`; shared blocks go in
   `templates/partials/`. Register the route in that group's `mount*` function (`internal/webui/*.go`,
   `registerGroup` from `init()`).
2. Add the handler: resolve the principal, apply the same gate as the JSON twin, call the service,
   build a view struct, `s.page(w, r, p, status, "group/name", title, data)`.
3. Style with the §3 component classes + Tailwind utilities; dark + light both covered.
4. Check at 1280px AND 390px in a real browser (AGENTS.md ladder step 8): no horizontal overflow, tables scroll inside their cards, tabs/pills wrap.
5. `pnpm --dir web run build` must pass (app.css is generated from the templates it scans —
   `@source "../../internal/webui/templates"`), and `make test` covers the handler; island logic needs
   `node --test web/test/unit/*.test.js`.

## 7. Known-shape notes (server payloads the UI must defend against)

- `settings/describe`: `maintenance.this_host.serves` / `maintains` are **booleans**, `roles` is a list — render booleans as yes/no, never `.join()` them.
- `settings/effective` returns **TOML text** (`application/toml`) — it is rendered verbatim, not JSON-parsed.
- Sha-addressed payloads (tree/blob/commits/commit) carry `ref: ""` by design — the page takes the display ref from the resolve step.
