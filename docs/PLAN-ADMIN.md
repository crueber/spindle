# PLAN-ADMIN — “Administration” as a product area

> Status: **implementing — M1 + M2 + M3 + M5 landed; M4 landed in
> `feat/admin-access`** (2026-11-21). Written 2026-11-21 against
> `main` (`bebd11c`, worktree `plan/administration`). Every file, route, template,
> control and role string below is named so a reviewer can check the plan against the
> tree.
>
> **As built (2026-11-21, `feat/admin-area`):** M1 — `/setup` deleted (no redirect) and the
> area mounted at `/admin`, `/admin/config` (+ `access`/`maintenance` read pages),
> `mountSetupOnly` + 503 copy re-pointed, `ShowAdmin = openMode || p.Admin`, `admin` reserved
> as an owner name, and the whole-tree label sweep landed (docs, README, AGENTS, DEVIATIONS
> D-UX-5, compose, Dockerfile, templates, Go) with the machine surfaces untouched. M2 — the
> shared shell (`partials/admin_shell.html`), `admin.go` extraction + type renames
> (`SettingsSetup*` → `AdminConfig*`, `SettingsDeps.Setup*` → `AdminDeps`), the Overview
> (instance/health/maintainer/config-status cards + the server-computed first-run checklist),
> `server.HealthFacts` + the composition `wireAdminPages` binding, and the gating matrix are
> in. **As built (M3, `feat/admin-config-lift`):** section anchors (`#<section>` + the
> `#server.auth` alias; `setup.AnchorOf` is the total key→section map), the `.pill` section
> strip with changed counts and the `sectionWhy` line per card, `?q=` server filtering
> (progressive: the filter island applies the same predicate live and opens Advanced groups
> while a query is present), `?changed=1` over `FromFile`, `?reset=<key>` (form-only reseed to
> the compiled default spelling, saved only by Save), the TOML disclosure labelled as a **preview of
> the saved document** (`setup.TOMLView`: the **saved** configuration's canonical bytes with non-empty
> secrets masked — unsaved edits are not reflected, and it is hidden in restricted mode; the copy says
> the editor is where changes are made and Save writes those edits to that path, because "— read-only" read to
> an operator as "Save cannot write this file"), error
> navigation (the `role="alert"` summary links each row, `aria-invalid`, the Advanced group
> opens, a small focus island lands on the offending control), and the leaveguard island
> (`beforeunload` + one in-page link confirmation + the Save dirty indicator/live status).
> Search/filter, reset, anchors, TOML, error-navigation and guard tests are in
> `internal/webui/admin_config_lift_test.go`, `internal/setup/{anchor,tomlview}_test.go`,
> `cmd/spindle/admin_config_toml_test.go` and `web/test/unit/{filter,leaveguard}.test.js`.
> Rendered proof also fixed a pre-existing grid-label defect the lift exposed: a `.grid`
> label collapsed its input to 10 px, so stacked captions now use `flex flex-col`
> (style-guideline §7). **As built (M5, `feat/admin-maintenance`):** the Maintenance page is
> the full read view of §3.5 — the maintainer card (running/not-running/unwired/restricted
> state words, `server.roles` + `placement.maintain*` + `maintenance.*` facts, the live
> `MetricsSnapshot` with the last pass's duration, and the task-kind table where the canonical
> kinds from `maintain.Kind*` render "no data yet" until their first run and `follow` aggregates
> `FollowRoundsByOutcome`), the event-bridge card (configured/not from `events.webhook_url` +
> `[[events.sinks]]`, masked secret, sweep cadence, events-role state, and the honest
> "per-repo cursors, no instance aggregate, no redeliver mutation" note pointing at the
> `/metrics` series), the storage/cache card (effective-config rows; one `config.CacheSpace`
> statfs for the filesystem holding `cache.dir` with a bounded `role="progressbar"` meter and
> the boot guard's budget-exceeds warning restated), health endpoints with their current
> answers (`/healthz` version, `server.HealthFacts` for `/readyz`, `/metrics` unevaluated), and
> the per-repo ops links (no cross-repository trigger is added — a follow-up card owns those).
> Tests: `internal/webui/admin_maintenance_test.go` (three states + facts + template a11y),
> `cmd/spindle/webui_maintenance_test.go` (the task-kind projection) and the config
> `CacheSpace` case. **Correction to §3.2/§3.5 as written:** `Maintainer.Metrics()` records the
> last pass's **duration** (`LastPassNanos` is `now−start`, not a wall clock) and exposes no
> "when" and no next-due time. Both the Overview and the Maintenance card therefore state the
> pass duration and the configured cadence; the earlier "last pass age via `rel`" wording was
> wrong (it rendered 1970 as "56y ago"). **As built (M4, `feat/admin-access`):** the Access page
> is the auth truth after OIDC-only (PLAN-OIDC-ONLY §3.2): six read-only cards — the OIDC sign-in
> facts with the copyable redirect URI, **Your access** (this session's verified groups beside the
> configured admin groups, the intersection verdict, and the claim/group/sign-in-again fix when
> they do not meet), the admission rules and their consequence, the static-token table (principal,
> scope, masked credential origin, no timestamps — never a secret; static tokens can never
> administer, and no API route mints or revokes one), the discovery probe (reusing
> `POST /admin/config/auth-test` into an `aria-live` region; Save never requires it), and the
> session facts (TTLs, masked secrets, admin re-derived per request). Each fact links to its
> `/admin/config#row-<key>` (M3 anchors). `pages.Auth.SessionInfo` did not need extending: the
> verified groups already ride `auth.Principal.Groups`. The shared denied view no longer names the
> configured admin groups or the issuer — a signed-in non-admin sees only their own groups plus
> the file/env recovery path. Tests: `internal/webui/admin_access_test.go`; docs:
> `docs/go/12_web_ui.md` §2.10 + its Decisions entry.
>
> Ground truth read for this plan: `internal/setup/setup.go` + `rows.go`
> (BuildSchema/Coerce/Save/RestartKeys/ProbeAuth — **149 keys, 19 groups**,
> measured, not asserted: see Appendix A), `internal/webui/settings.go`
> (`mountSettings`, the `Setup*` seam, `settingsFieldMeta`, the Essentials/
> Advanced partition), `internal/webui/templates/pages/settings/setup.html` +
> `partials/settings_setup.html`, `internal/server/setup.go` (`setupAccess`),
> `internal/server/router.go` (`mountSetupOnly`), `internal/server/health.go`,
> `internal/maintain/metrics.go` (`Maintainer.Metrics()`), `internal/config/firstrun.go`
> (`MarshalTOML`, `SaveSetup`, `LoadSetupBase`), `internal/webui/render.go`
> (`Chrome.ShowSetup`), `partials/header.html`,
> `docs/style-guideline.md` (binding), the accessibility rules in `AGENTS.md`
> §2, `docs/PLAN-THEMES.md`, `docs/go/12_web_ui.md` §2.10,
> `docs/go/06_server_http.md` §3.4/§3.5, `docs/features/01_identity_permissions.md`,
> and `README.md` (quick start).

---

## 0. Recommendation up front

Rename the **“Setup”** section to **“Administration”**, give it a real home
(`/admin`), and lift it from one 149-key form into a four-page area composed
from the seams that already exist. The configuration editor — today’s whole
`/setup` page — stays **one editor** (the byte-identical, whole-file save
contract is preserved exactly) and gains the affordances that make 149 keys
navigable: section anchors, a key search, a changed-only view, per-field reset,
and a “view the TOML that Save would write” disclosure.

**The URL moves: `/setup` is deleted, not aliased** (pre-1.0 law: no aliases,
no shims). The restricted/first-run path — the one case that must stay
reachable — becomes `/admin` + `/admin/config`, served by the setup-only route
set exactly as `/setup` is today.

### Page map

| Page | Route | Purpose (one line) | Data source |
|---|---|---|---|
| **Overview** | `GET /admin` | The landing page: instance state, health, first-run checklist, version | boot state + config + `Maintainer.Metrics()` + readiness facts (new read-only seam, §8.2) |
| **Configuration** | `GET/POST /admin/config`, `POST /admin/config/auth-test` | The whole 149-key schema editor (today’s `/setup`, lifted) | `internal/setup` — unchanged |
| **Access** | `GET /admin/access` | Who can sign in, who is a host admin, OIDC discovery check | schema values (`server.auth.*`) + `setup.ProbeAuth` |
| **Maintenance** | `GET /admin/maintenance` | Operational state: maintainer loop, event bridge, storage facts, health links | `Maintainer.Metrics()` + config facts (read-only) |
| **Directory** | `GET /admin/directory`, `POST /admin/directory/{deactivate,reactivate}` | The instance users and organizations, with deactivate/reactivate (§3.7 — the §8.6 follow-up card, landed) | identity reads (`users/`, `orgs/`) + the one audited account transition |

### Nav, before → after

| | Before | After |
|---|---|---|
| Site nav (anonymous, auth `none`/defaults) | `explore · setup` | `explore · admin` |
| Site nav (signed-in host admin) | *(no link)* | `explore · admin` — the area is a destination, not a secret |
| Account menu (admin) | `… Keys · Invitations · **Setup** · Log out` | `… Keys · Invitations · **Administration** · Log out` |
| Site nav (signed-in non-admin, real auth) | *(nothing)* | *(nothing — the area is hidden, never shown-then-403’d; the org-tabs precedent)* |
| Page `<title>` / h1 | `Setup` / `Setup` | `Administration` / per-page h1 (§3.1) |

Rejected page ideas (runners/secrets/automation/about as their own pages, and
why) are in §1.3 and Appendix B.

**Effort:** ≈ 6.5 dev-days across five milestones, each independently green
(§8.4). **Nothing blocks on the theme work** (PLAN-THEMES W1–W4); two small
a11y riders already assigned to W1 are noted in §8.5 so the plans don’t collide.

---

## 1. Naming and IA

### 1.1 The name

The area is called **Administration**; its URL stem is `/admin`; its nav label
is lowercase `admin` (the site-nav voice — `explore`, `setup` today, is
lowercase); its account-menu entry is `Administration`. “Setup” is retired as a
user-facing word: the word describes first-run, and the surface it named is
used every day. Where prose needs the first-run meaning, say **first run** or
**restricted mode**, not “setup”.

### 1.2 Where it lives

- **Site nav** (`partials/header.html`): one `admin` link, shown when the
  visitor can actually use it. Today’s rule is
  `ShowSetup: !signedIn && (mode == "" || mode == "none")`
  (`internal/webui/render.go:79`) — a link that disappears the moment the
  dangerous mode ends, and never appears for the admins who need it daily.
  New rule: **`ShowAdmin: p.Admin`** (PLAN-OIDC-ONLY §1.6 amendment: `openMode`
  no longer exists — `server.auth.mode` is deleted, and the restricted boot is
  the only open state, where `/admin` is the only surface anyway). The header
  link is a plain link; the area side-nav (§3.1) is what marks the current page
  with `aria-current`.
- **Account menu** keeps an `Administration` entry for `Chrome.Admin`
  (identity-scoped convenience; both links go to `/admin`).

### 1.3 The sub-areas — and what does NOT belong

The area owns **instance-scoped** surfaces only. The repo already has a
correct, role-gated home for repository and organization administration
(`/{o}/{r}/settings/*`, `/{owner}/settings`); moving those here would break the
“page gate mirrors the API gate” rule (AGENTS field lesson) and orphan them
from the repo/org context that explains them.

| Proposed | Verdict | Rationale |
|---|---|---|
| Configuration (the schema) | **In** — `/admin/config` | The instance’s own knobs; today’s `/setup`. |
| Access (admins/OIDC/tokens) | **In** — `/admin/access` | Instance identity: who authenticates, who administers. Read view over `server.auth.*` + the discovery probe. |
| Maintenance (ops status, health) | **In** — `/admin/maintenance` | Instance operational state. The *status* belongs here; the per-repo *ops triggers* (WAL panel) stay in repo settings (below). |
| Instance “About/version” | **Folded into Overview** — no separate page | A page with a version string and nothing else is dead weight; the Overview carries it. |
| Runners & secrets (CI) | **Out — stays per-repo** | Runners register against a repo’s CI token; secrets are repo-scoped store docs (`docs/features/15_threads.md` §6). An instance fleet view needs a cross-repo list seam that does not exist — filed as a follow-up card (§8.6), not bundled. |
| Repo/org webhooks | **Out — stays repo/org** | `SettingsWebhookPanel` is per-owner plumbing (WP6.2); an instance list would need a new aggregation seam — follow-up card. |
| The events *bridge* | **In — Maintenance card** | `events.webhook_url/secret/sweep_interval` are instance config; the card reports configured/not + sweep cadence. Keys are edited once, on Configuration (§4). |
| Packages GC | **Out of milestones** | GC is a maintainer pass over per-repo registries (`internal/packages/gc.go`); an instance “run now” needs a new task kind + seam — follow-up card. The `packages.*` limits stay in Configuration. |
| Identity administration (users/orgs directory) | **Out of milestones** | No instance-wide directory seam exists; identity admin today IS the Access page (static tokens, OIDC admin mapping). A directory is a feature (law 8 schema decision) — follow-up card. |
| Audit/events listing | **Out of milestones** | There is no instance audit log. Org Activity is org-scoped. Do not fake one — follow-up card only if the owner wants the seam. |
| Per-repo WAL ops, danger zones, mirrors, policies | **Out — stays repo settings** | They are (repo, kind) single-flight tasks gated by repo roles (law 7); the repo settings tabs are their contract. |

### 1.4 The URL decision (and the bootstrap path)

**Move, then delete. No redirect.**

- `GET /setup` → `GET /admin/config`; `POST /setup` → `POST /admin/config`;
  `POST /setup/auth-test` → `POST /admin/config/auth-test`.
- `/setup` is **removed** in the same milestone. Pre-1.0 forbids aliases and
  “still accepted for” branches (AGENTS preamble); a 301 would be a shim with
  a heartbeat. In-tree references (22 files under `docs/`, plus `README.md`,
  `AGENTS.md`, `DEVIATIONS.md`, the compose example, `Dockerfile`, the publish
  workflow templates, and the Go files listed in §8.1 — enumerated in §9) are
  rewritten in the same commit, so nothing in the shipped tree can produce the
  old URL.
- **The restricted/first-run path stays stable and even shorter:** the
  setup-only route set (`internal/server/router.go mountSetupOnly`) serves
  `GET /admin`, `GET/POST /admin/config`, `POST /admin/config/auth-test` plus
  today’s `/_ui/*`, `/healthz`, `/readyz`, `/services/public/install.sh`; the
  503 fallback copy becomes `walgit: configuration is invalid — open /admin to
  fix it`. “Stable” means: one guessable URL, always registered, in every
  boot mode — which `/admin` now is. Tests assert the full mountSetupOnly
  matrix (§8.3).
- Route mechanics: literal routes already beat the `/{owner}` wildcard; the
  overview/config/access/maintenance routes are registered like today’s
  `/setup` lines (`router.go:172–174`), and **`"admin"` joins
  `reservedOwnerNames`** (`internal/webui/public.go:191`) — the same
  documented trade as `explore`/`how-it-works`: an owner literally named
  “admin” loses their `/admin` profile page; their git/API paths are unaffected.

### 1.5 What keeps its name (machine surfaces)

`/api/v1/setup*` (the frozen §3.4 JSON contract), `internal/setup` (package),
and `/services/setup.json` keep their names. They are machine surfaces whose
wire names are documented contracts (law: wire identifiers keep Rust-era
names); renaming them buys nothing and churns `docs/go/07_api.md` and every
client. The docs gain one sentence each: *“the setup API is the machine
surface of `/admin/config`.”* The **Area** is Administration; the **API** keeps
its historical handle — exactly like `walgit.v1`.

---

## 2. The first-run experience

The front door for a new operator is the product’s first impression. Today it
is one 149-row form with a Save button. The lift gives it a narrative.

### 2.1 Zero-config first run (no config file)

`spindle` boots on defaults (`boot.Mode == "defaults"`): auth `none`, open
everywhere. What the operator sees at `/admin`:

1. **The first-run OIDC onboard card** (only in the restricted-absent boot —
   PLAN-OIDC-ONLY §2.2 replaces the checklist), styled with the existing
   `.setup-callout`/`.setup-row` idioms: issuer, allowed email domain, client
   id, client secret, session secret (with a generated copyable suggestion) and
   admin group, plus the copyable redirect URI and a live **Test**
   (discovery + JWKS). Its one primary is **Save configuration**, which posts
   only those keys and never dials the IdP. A normal boot never renders it.
   `FirstRun` comes from the boot state (`restricted ∧ file absent`), so an
   env-configured instance with no file does not show it.
3. The rest of the Overview renders as always (instance card, health card).

### 2.2 Invalid-config (restricted) mode

`mountSetupOnly` serves `/admin` + `/admin/config*` only. The banner on both
pages keeps today’s semantics, reworded to the area’s voice:

> **Restricted mode — the configuration file is invalid.** Until a fixed
> config is saved and the server restarted, only `/admin`, `/healthz` and
> `/readyz` answer; everything else returns 503. Fix the errors below,
> Validate, Save, restart.

`/readyz` answers **200** `{"status":"restricted","surface":"admin"}` in this
mode (not 503): the instance is ready for the surface it serves, so a
health-gated ingress routes it and `/admin` is reachable — the 2026-11-22
incident was a 503 here, and the recovery UI was gated out
(`docs/go/06_server_http.md` §10.2 carries the ingress contract). `/healthz`
stays liveness; `/` and every non-admin path stay 503.

The boot-errors table (today’s “Current validation errors” card) stays on
`/admin/config`, and each row gains a link to its section anchor (§4). `/admin`
(overview) also renders in restricted mode — from the same `FileState`/errors
data, with degraded cards for the seams not wired there (the established
“not wired” pattern, `settings.go` WIRING header) — so the operator lands on a
page that explains the state, not a bare form.

### 2.3 Establishing the first administrator (no setup token)

Already decided in the tree and kept exactly: there is **no setup-token escape
hatch** (`internal/server/setup.go`; `docs/features/01_identity_permissions.md`
D-note: `SPINDLE_SETUP_TOKEN` has no effect). The first admin is whoever the
operator names in config, and the flow is now written down in one place:

- **OIDC only**: a signed-in member of a group in
  `server.auth.admin_groups` (default `spindle-admin`, read from
  `server.auth.groups_claim`). `admin_emails`/`admin_domains` and
  `admin = true` token rows were deleted by PLAN-OIDC-ONLY — there is no second
  path. Lockout recovery is the file/env edit (§2.4), never a paste form.

### 2.4 Before any admin can sign in

The access rule is unchanged (`setupAccess`): open while no valid config, in
restricted mode, or auth `none`; otherwise an authenticated host admin. The
**forbidden state** is a designed page, not a bare 403: the existing restricted
card (`setup.html` `Denied` branch) becomes the area’s shared denied view with
recovery copy — signed-in non-admin (the configured group, their live groups,
and “ask an admin”, linking the Access page), signed out (the sign-in link),
and always the verbatim file/env recovery plus “there is no unauthenticated
back door”. Unauthenticated callers get the
API’s status (401) so credentials are never trainable into a 200 (AGENTS law
9); the page renders the sign-in affordance.

---

## 3. Page anatomy — one shell, four pages

**No new design language.** Every control composes the canonical classes:
`.card` + `.card-header` (panels), `.setup-row`/`.setup-label`/
`.setup-examples`/`.setup-note`/`.setup-callout` (config rows, guideline §7),
`.btn`/`.btn.primary` (one primary per page), `.eyebrow` + `.mono`
(identifier voice), `.side-nav-*` (the area nav, guideline §2 — the settings
nav idiom), `.data-table` in `overflow-x-auto` + `min-w-0` wrappers (§8),
`.empty-state` (§5), `.chip` family for state, `.link` for text actions,
`.err-line`/`.warn-line` for errors, and the `danger.js` typed-confirm idiom
for anything destructive added later. Contrast comes from `--pd-*` tokens only.

### 3.1 The shell (all four pages)

- **Area nav** on the left at `lg:`, stacked full-width chip bar above content
  below `lg:` — exactly the repo-settings layout (`pages/settings/repo.html`
  lines 10–32): `Overview · Configuration · Access · Maintenance`, with
  `aria-current="page"`, heading `.side-nav-heading` “Administration”. No
  Danger section (no instance-destructive op exists yet; one added later goes
  in a `side-nav-heading-danger` section per the idiom).
- **Header**: `.eyebrow` “administration” (names the region, guideline §1 F6),
  then the page’s single `<h1>` (“Administration” on the overview;
  “Configuration”, “Access”, “Maintenance” on the others — the §4b
  one-h1 rule, promoted like today’s standalone `/setup`), then a one-line
  purpose (`text-sm` `--pd-text-2`), then a state chip row: **host admin**
  (`chip-neutral`) when signed in as admin, **first run** (`chip-draft`) while
  `FileState == "absent"`, **restricted** (`chip-closed`) in setup-only mode.
  State is never colour-only — the chips carry their text (AGENTS a11y rule).
- **Breadcrumbs: deliberately none.** The side-nav is the position indicator;
  there is no `owner/repo`-style coordinate hierarchy to breadcrumb, and the
  guideline has no breadcrumb idiom — introducing one would require a
  guideline extension for zero information gain (Appendix B). The eyebrow +
  side-nav carry the “you are in Administration” fact.
- **Result banners** (saved / validated / errors / notices) live at the top of
  the content column in the existing banner shape
  (`border-l-2 border-l-[var(--pd-ok|-danger)] p-3`), `role="status"` /
  `role="alert"`.
- 390 px: nav stacks (§7), rows go one-column (`.setup-row` already does),
  nothing new to decide per page.

### 3.2 P1 — Overview (`/admin`)

Purpose: *the state of this instance at a glance, and what to do next.*

| Section card | Contents |
|---|---|
| First run | §2.1 checklist card (absent-file state only) |
| Instance | mode (`first run`/`valid`/`restricted`), version (the value `/healthz` reports), the OIDC admin group + anonymous_read, store backend + bucket/root, `server.public_url`, active `server.roles`. A `.kv` table; every value monospace |
| Health | readiness verdict + warnings (the same facts `readyz` reports: warm-up pending, draining, `config:"defaults"` banner — new read seam, §8.2), links (`link` class) to `/healthz` and `/readyz` — links, never a fake dashboard |
| Maintainer | last pass duration, passes, checkpoint lag/age, missing/repair objects (from `Maintainer.Metrics()`; the metrics expose a pass duration, not a wall clock) — absent when the seam is unwired (restricted mode) |
| Configuration status | file state + count of boot validation errors, linking each error key to `/admin/config#<section>` |

Controls: none that write. This page is read-only; its one `.btn.primary` is
the first-run card’s “Open configuration” CTA (when first-run is showing);
otherwise the page has no primary (a read-only page with a fake primary is a
regression — F3).

### 3.3 P2 — Configuration (`/admin/config`)

Purpose: *edit every instance setting; validate and save the whole canonical
`spindle.toml`.*

This is today’s `/setup` body, lifted (§4 has the disclosure mechanics,
§5 the interactions). Sections render in schema order with anchors: `#server`
`#auth` `#store` `#cache` `#wal` `#maintenance` `#placement` `#compaction`
`#bundles` `#lfs` `#upstream` `#git` `#telemetry` `#events` `#releases`
`#import` `#attachments` `#packages` `#threads`. Each section card gains:

- a `.card-header` **h2 with the section name** (today’s bare `.eyebrow` h2
  becomes the card header idiom),
- a one-line **“why this matters”** note (`.muted text-sm`, from a new
  `sectionWhy` map in the page module — copy for all 19 in Appendix A.2),
- an `id` anchor + a per-section **row count chip** (`file` count vs defaults:
  “3 changed”).

The OIDC discovery card moves to `/admin/access` (it is an access concern);
the packages anonymous-write warning stays (it is configuration truth). A save
whose `setup.RestartKeys` list is non-empty renders the dedicated **Restart
required** card below the result banner (§5): the keys each link their
`#row-<key>`, and the card carries the typed-confirm `restart` action.

### 3.4 P3 — Access (`/admin/access`)

Purpose: *who can sign in to this instance, and who can administer it.*

> **As built (M4, `feat/admin-access`; §3.4 as written predates OIDC-only).** With
> `server.auth.mode` deleted, the "mode switch" row is gone: OIDC *is* the sign-in,
> administration is exactly *signed in ∧ verified `groups_claim` ∩ `admin_groups`*,
> and the page shows that truth. The built cards are:

| Section card | Contents |
|---|---|
| Sign-in (OIDC) | issuer, client id, `groups_claim`, `admin_groups`, `anonymous_read` — each a fact linked to its `/admin/config#row-<key>` — plus the computed redirect URI with its copy button (linked to `server.public_url`) |
| Your access (live) | the signed-in principal, the verified groups the IdP delivered this session, and the configured admin groups side by side, then the `p.Admin` verdict and — when the lists do not intersect — both lists and the concrete fix (check the claim name, correct the group value, ask the IdP admin, sign out and in again) |
| Admission rules | `allowed_domains`, `allowed_emails`, `write_domains` as read-only facts with consequences stated: an email must match one or nobody may sign in, and these never grant administration |
| Machine credentials | the static-token table (principal, write/read, masked credential origin or `token_env` name, created/revoked as "not tracked — the file is the record"), the fact that static tokens can never administer, the restart requirement, and the statement that no API route mints or revokes a static token (the page invents none; personal `wgt_` tokens are minted at `/_auth/tokens`) |
| Discovery test | the existing probe (`POST /admin/config/auth-test`) into an `aria-live` result; it reports the precise failing step and is never required for Save |
| Sessions | `session_ttl`, `access_token_ttl`, `session_secret`/`oauth_client_secret` set (masked, never shown), and that admin is re-derived per request — group removal at the IdP takes effect on the next sign-in, while editing `admin_groups` takes effect immediately |

Nothing here writes; all edits link into Configuration. One editor, many views
(§4 rule). **The denied view leaks nothing:** a non-admin sees their own verified
groups and the file/env recovery path, never the configured group names or the
issuer (those are admin-only facts).

### 3.5 P4 — Maintenance (`/admin/maintenance`)

Purpose: *is the machinery healthy, and what is it doing.*

| Section card | Contents |
|---|---|
| Maintainer | `MetricsSnapshot` rendered as a `.kv` table: passes, last pass duration, per-kind unit outcomes, checkpoint lag entries/age, missing/repair objects, follow rounds |
| Event bridge | configured (URL set? secret set — masked) + `sweep_interval`; “not configured” renders the `.empty-state` |
| Storage & cache | backend, bucket/prefix/root, cache mode + `max_bytes` — config facts, honestly labelled “from configuration”, linking to their editor anchors |
| Health endpoints | `/healthz`, `/readyz`, `/metrics` as `link`s (plain, machine-owned) |
| Restart | always available to a host admin: the typed-confirm `restart` control (§5). It posts `POST /admin/restart`; the process exits and its supervisor restarts it. |

Manual per-repo ops stay in repo settings (WAL tab); the card links to the
docs rather than inventing an instance-wide trigger (§1.3).

> **As built (2026-11-22, `feat/admin-gc-audit` — the §8.6 consolidated card's first two items).**
> The Maintenance page gains exactly ONE control, the packages GC card:
>
> - **Dry run first.** `POST /admin/maintenance/gc` with `op=dry-run` starts the existing
>   `packages-gc` task at **instance scope** (the reserved task-table repo key `instance`;
>   `internal/packages/gcadmin.go`) with `dry_run=1`. The pass walks every registry and counts
>   unreferenced blobs/bytes and expired upload staging — **nothing is deleted** — because a count
>   needs a LIST and law 4 forbids that on a request path. The copy says it is a count of the store
>   right now, not a promise. `PlanGC` is the same mark/sweep body with `apply=false`, so a dry run
>   and an applying pass cannot disagree about what "unreferenced" means.
> - **One-click apply behind a confirmation.** `op=apply` requires `confirm=apply`, typed into the
>   danger-zone confirmation field (`data-danger-expected`, the island disables the button until it
>   matches) with the same rule enforced server-side: a missing or wrong value is a 422 with a named
>   inline error, never a silent start. It starts the same task with `dry_run=0`; a running pass is
>   **joined**, not raced.
> - **Narration (law 7).** The POST 303s to `/admin/maintenance?gc=<id>`; the page renders the same
>   `data-task-id`/`data-task-stream` card the transfer/import cards use, streamed by
>   `GET /admin/maintenance/gc/stream?id=` through the API's one SSE envelope
>   (`api.PumpTaskStream`) — progress bars per registry, the task log tail, a terminal result.
> - **Last-run facts across restarts.** The pass writes `packages/gc_state.json` (bucket root,
>   overwrite JSON v1: `last` + `last_apply`, each `{at, dry_run, actor, task_id, repos,
>   unreferenced, bytes, uploads, ok, error}`). The card shows the most recent apply and the most
>   recent pass separately (a dry run can never masquerade as the last real run), with the failure
>   text when `ok=false`.
> - **Scheduled cadence, surfaced not duplicated.** The card states the maintainer's own numbers —
>   per-repo due gate `packages.DefaultGCInterval` (15m) and `DefaultBlobGrace` (10m) — instead of
>   adding a config key or a second sweeper.
> - **Audit.** Every trigger (dry run, apply, join) writes one `packages_gc` record at
>   `instance:packages` through `moderation.RecordAdmin`, naming the mode and the task id.
>
> The `/admin/audit` viewer gains server-rendered filters — `?action=` (verb, with a datalist of
> the family's vocabulary), `?actor=`, `?repo=` (`owner/name`, or `-` for instance-level
> targets), `?since=` (a date, an RFC 3339 stamp, or a window: `90m`/`12h`/`7d`/`2w`) — applied by
> `moderation.Service.AuditQuery` with exact-key reads only (no LIST). `?after=` remains the **scan**
> cursor, so paging survives filtering: every page link carries the filters and the cursor, a page
> that matched nothing reports "examined N" and continues, and the walk is bounded at 1,000 records
> per request. Bad input renders a named inline error and is ignored (the rest of the filter still
> applies) — never a 500. The viewer is `Cache-Control: no-store`.
>
> The audit family's two deferred rows land: identity's access grants (access.json),
> org/team membership grants and revokes write `access_grant` / `org_member_grant|revoke` /
> `team_member_grant|revoke` (actor from `identity.WithActor`, "system" for the CLI), and the
> transfer task writes `transfer` at `repo:<destination>` after verification. The M4 canary's
> allow-list rows for those are deleted, and a new family table pins the card's five required
> families (config save, token mint/revoke — mint self-service, revoke = session_secret rotation =
> config_save —, admin grant, repo transfer, moderation) with a negative test that drops each
> family's anchor in turn.
>
> **Rendered proof.** Both pages were driven in a real browser at desktop (1440×900) and ~390×844,
> dark and light, from a build of this tree (`make web` first, canonical host): one `h1` each
> (Maintenance / Audit log), the header/nav/main/footer landmarks and the skip link present, every
> control named in document order with no positive `tabindex`, and
> `document.documentElement.scrollWidth === clientWidth` at both widths in both themes (the audit
> table scrolls in its own `overflow-x-auto` wrapper). Measured contrast on the rendered computed
> colours: muted prose 5.14:1 dark / 5.08:1 light, chips 7.11 dark / 4.61 light, the apply button's
> label 5.43 light. The console carried no application errors; the one exception is the vendored
> htmx bundle's `XPathEvaluator` probe, the headless-binary limitation M3/M4 already recorded, not
> application code. The filter form and the typed confirmation were exercised by keyboard focus
> (label → input → submit), and the bad-input state (`?since=yesterday&action=two%20words&repo=acme`)
> renders three named inline errors at 200.

### 3.7 P6 — Directory (`/admin/directory`): the §8.6 follow-up card, as built

The last deferred administration surface. It landed last because it needed two
things the earlier milestones did not: an instance-wide identity read seam
(there was none — Access only ever read configuration) and a **schema
decision**, because deactivation changes whether a credential is accepted.

**Schema decision (law 8).** Disablement is stored on
`users/<username>/user.json` — the credential binding — as append-only fields:
`disabled`, `disabled_by`, `disabled_at`, `reason`, `tokens_revoked_at`
(RFC3339). Not on `profile.json`: a profile is presentation state the public
profile page reads, and a credential fact must not ride a document whose job is
to be shown around. `user.json` is the OIDC registry (every sign-in creates
it), it is extensible JSON with a CAS token (device `Version`), and the
credential boundary already resolves a username on every request — so the
authoritative read is one exact-key GET of the document the account IS, served
from a bounded in-process LRU on the warm path. The fields are additive
(law 5): an active account's bytes keep their pre-field shape and no key moves
(`docs/features/01_identity_permissions.md`, `docs/go/02_storage_protobuf.md`).

**Enforcement lives at the credential boundary, not on pages.**
`internal/server/auth.go`'s `accountGate` runs inside
`principalFromEmailGroups` — the one function every OIDC-derived credential
resolves through — after OIDC verification and username resolution:

- **disabled** → `403` with a neutral message (it names neither the
  administrator nor the reason);
- **a credential minted at or before `tokens_revoked_at`** → `401`
  (session and `wgt_` token wires carry their mint time);
- **an unreadable account state** → `503` (fail closed: "not disabled" is
  never the answer to a store error);
- **no lookup wired** = no directory on this build (the feature is off).

The read is not a per-request bucket round trip (law 6): the identity
service's `UserState` memoizes it in a bounded in-process LRU keyed by
username (4096 entries, 10 s TTL, negative entries included, errors never
cached; `internal/identity/account_cache.go`). A warm signed-in request costs
no store op; a cold/expired entry costs one exact-key GET. `SetUserDisabled`
invalidates the account after its CAS commit, so a local transition is enforced
on the next request; **the TTL is the cross-instance bound** — another instance
may serve a just-deactivated account for up to 10 s, while a stale disabled
entry after a reactivation only refuses sign-in until it expires (revoked
credentials stay revoked either way).

**Semantics, stated plainly (docs and tests agree):**

| Credential | Effect of deactivation | Effect of reactivation |
|---|---|---|
| OIDC sign-in (ID token / login callback) | refused (403) | works again |
| Browser session (`walgit_session`) | refused immediately (403 while disabled, 401 after the watermark) | the pre-deactivation session stays dead; sign in again |
| `wgt_` access token | refused (403 while disabled, 401 after the watermark); new mints are refused because the mint needs an authenticated principal | the old token stays dead; mint a new one |
| SSH key auth | refused (it resolves through the same gate) | works again |
| Static `server.auth.tokens`, CI/runner tokens | **unaffected** — machine credentials are configuration entries, never user-bound | unaffected |
| Org membership / ownership | **unchanged** — the page names the organizations a deactivated account still owns; transfer is out of scope | unchanged |

Reactivation clears the flag and keeps `tokens_revoked_at`: revoked
credentials are never restored.

**The page.** Host-admin only, one new nav entry (`Directory`, h1 "Users and
organizations"), two tabs over one bounded server-rendered page:

- **Users** — name order, `?q=` case-insensitive substring over username and
  display name, `?after=` cursor. The unfiltered list walks account names with
  a delimiter LIST until the page (25) fills; a search examines at most
  `identity.DirectoryScanBound` (120) accounts and says so, offering the
  cursor for the rest. Each row: username (linked to the profile page),
  display name, organization memberships with owner marked (one bounded
  organization walk for the whole page, never one per row), and the state as
  TEXT (`active` / `deactivated`, with who/when/why beneath it). The profile
  page link is the only identifier shown — no email, no token, no key.
- **Organizations** — name order, 25 per page with the same cursor, name
  (linked to the org page), display name, member count (one `org.json` +
  `members.json` read per row).
- **Actions** — deactivate behind the shared typed-confirmation island
  (`data-danger-confirm`, `data-danger-arm`, `data-danger-submit`) plus a
  required reason; the server enforces confirm == username, a non-empty
  reason and "not your own account" as a 422 with a named inline error, so a
  request without JavaScript cannot skip the confirmation. Reactivate is a
  plain same-origin POST. Both 303 back to `?q=<username>` so the row (and its
  ownership annotation) is what the operator sees.
- **Audit** — every real transition writes one `user_deactivate` /
  `user_reactivate` record at `user:<username>` through identity's audit seam
  (`moderation.RecordAdmin`), with the reason as the detail and the actor from
  `identity.WithActor`. A repeat of the same state writes nothing (no
  fabricated entry), and the M4 completeness canary carries the row
  (`internal/moderation/auditcanary_test.go`).
- **Empty/denied/unwired** — the shared four states (§3.6); the tables scroll
  in their own `overflow-x-auto` wrappers at ~390 px, and the page has one
  `.btn.primary` (search).

**Rendered proof (2026-11-22).** Driven from a build of this tree
(`make web` first, canonical host, headless Chromium over CDP with
`Emulation.setDeviceMetricsOverride`) at 1440×900 and ~390×844, dark and light,
both tabs, a deactivated row and its result banner: one `h1` each ("Users and
organizations"), the skip link and the four landmarks, exactly one
`aria-current="page"` (the area nav; the tab strip carries
`aria-current="true"`), one `.btn.primary` (search), and
`document.documentElement.scrollWidth === clientWidth` at both widths in both
themes — the users table (704 px min) scrolls inside its own wrapper
(838/838 desktop, 704/324 at 390 px), never the page. Measured contrast on the
rendered computed colours (WCAG AA needs 4.5:1 for text): muted prose 5.45:1
both themes, `deactivated` chip 4.51 dark / 4.69 light, `active` chip 6.43 /
4.62, the disabled-reason line 5.14 / 5.08, the deactivate summary 5.55 / 6.11,
h1 15.63 / 17.77, table links 14.75 / 16.57. No control that is not an inline
text link is under 24 px (the `deactivate…` summary measures 24 px). The
console carries only the vendored htmx bundle's `XPathEvaluator` probe — the
headless-binary limitation §3.5 already records, not application code. Keyboard:
the content order is tabs → search → row summary → reason → confirmation →
submit, the island arms the button only when the typed username matches, and
the server re-checks it (a wrong confirmation is a 422 with the named inline
error; the deactivate/reactivate round trip and both audit rows were exercised
on the live instance).

### 3.6 The four shared states

- **Forbidden** — the §2.4 denied view (the one page layout, mode-specific copy).
- **Unwired seam** — the existing “not wired on this build” card (a nil dep
  renders its panel degraded; the wiring-guard test extends to the new pages —
  the `TestSettingsWiringBindsEveryRenderedSeam` precedent).
- **Error** — banner + keyed inline `.err-line` rows (existing); the API’s
  503 page for store-level failures.
- **Empty** — `.empty-state` + guidance (e.g. “No static tokens configured —
  add one in Configuration”, linking the anchor); never a machine catch-all.

---

## 4. Progressive disclosure over a 149-key schema

The load-bearing rule: **the schema is edited in exactly one place.** Access
and Maintenance are read views; every edit affordance they show links to
`/admin/config#<anchor>`. Duplicating editors would fork the save path and
break the byte-identical save. (Rejected: per-area editing pages — Appendix B.)

1. **Essentials / Advanced** (unchanged mechanism): the `settingsFieldMeta`
   partition, collapsed `<details class="setup-advanced">`, auto-opened on a
   keyed error (`AdvancedHasErrors`). Collapsed rows stay mounted — the
   zero-edit save stays byte-identical.
2. **Section anchors**: every section card gets `id="<section>"`; the page
   gains an **anchor rail** — the side-nav area nav stays, and a second-level
   in-page nav (a `.pill` strip under the header, horizontally scrollable at
   390 px per the nav-strip rule) lists the 19 sections with their changed
   counts. Deep links from errors, the Overview, Access, Maintenance and the
   docs all use this one coordinate system.
3. **Key search**: one `<input>` (`.input`, `type="search"`,
   `aria-label="filter keys"`) above the form driving the **existing
   `data-filter` island** (`web/src/islands/filter.js` — client-side over the
   mounted rows, never a fetch, guideline §3.1). Rows hide on no-match
   (`hidden` attribute, focus-safe). While the query is non-empty the island
   sets `open` on every `details.setup-advanced` in the form (a filtered row
   inside a collapsed group is invisible otherwise). **As built (M3):** for
   no-JS degradation the same control is a GET form whose `?q=` the server
   honours with the identical predicate (trimmed, case-insensitive substring;
   the Go test mirrors the island's cases), and matching sections drop from
   the page. The URL is the authoritative filter; the island is a live
   convenience over the mounted rows and does not rewrite history (a transient
   filter is not state — §2.9 keeps *state* in URLs, and `?q=` is a view, not
   a document).
4. **Changed-only view**: `?changed=1` renders only rows with
   `FromFile == true` (the field already computes value ≠ default;
   `settingsPrepareField`). A count banner states “showing N changed of 149
   keys — show all” (a `link`, not a button trap). Server-side, one branch in
   the section builder. **As built (M3):** a named checkbox in the same GET
   form (`data-autosubmit` when JS is present) and the toolbar's "Showing N of
   TOTAL keys · M changed" line; a stored secret counts as set because its
   value cannot be compared to the default.
5. **Per-field reset**: each `FromFile` row gets a `reset` `link`
   (`?reset=<key>` — a GET that re-renders with that row’s `Value` seeded
   from `Default`, **nothing saved**: the `settingsRevert` precedent — a link
   is honest, a write is not). Save still writes the whole file.
6. **Key → location mapping**: `setup.AnchorOf(key) string` (tiny exported
   helper over BuildSchema: key → section id) consumed by (a) the boot-errors
   table rows, (b) the saved-restart key list, (c) `docs/go/11_config_cli.md`
   documents that every keyed CLI/validator error names
   `/admin/config#<section>`. With all keys on one anchored page this is
   mechanical and total.
7. **View the TOML Save would write**: a `<details>` at the top of the page
   (“The file Save writes”) rendering `config.MarshalTOML(base)` in a
   `code-view` block — the exact bytes of the next save, including the
   not-shown-in-rows state. This is the copy-paste-able artifact for
   operators who keep config in Ansible. **As built (M3):** `setup.TOMLView`
   over the file-visible base, with every non-empty secret value replaced by
   `setup.SecretMask` (secrets never leave the server — §5). The view is the
   **saved configuration**: unsaved form edits are deliberately not reflected
   (only Save moves the file, and a client TOML serializer would be a second
   renderer of the shape `internal/setup` owns). The disclosure states that,
   and is hidden while the file is invalid (no valid saved document exists).

The keyed-error contract is untouched: errors carry their key, mark their row,
open their group (`settingsApplySetupValues`), and never echo secrets.

---

## 5. Interaction rules

| Concern | Decision |
|---|---|
| Validation timing | **On submit only** (the existing Validate/Save two-step). No blur-validation island: a client copy of `config.ParseDuration`/`ParseByteSize` would be a second source of truth for the parsers (the exact drift the `EnumValues` single-table rule exists to prevent). A blur error the server disagrees with is worse than a submit round trip. |
| Save granularity | **One save for the whole document** (the whole form posts; the payload is complete because collapsed rows stay mounted). Per-section saves would either write partial files (dangerous — the file is the source of truth) or re-serialize the whole file anyway while pretending to be local. One primary: `Save`; `Validate` is the secondary `.btn`. |
| Dirty/unsaved guard | A new ~20-line island `web/src/islands/leaveguard.js`: `data-leave-guard` on the form; any `input` event inside arms `beforeunload`; submit/`hx-post` disarms it. No modal, no client state machine — the browser’s own guard, matching the no-JS-first composition of the rest of the area. |
| Restart signalling | Kept (post-save card + `?restart=` keys) and extended with the control the recovery loop needs. A save whose `setup.RestartKeys` list is non-empty renders a prominent **Restart required** card listing each key (linked to its `/admin/config#row-<key>`); **Maintenance** always offers a Restart card. Both post `POST /admin/restart` with the danger typed-confirm word `restart`. The server re-checks the word, audits `admin_restart`, flushes a "restarting" page, then invokes the injected `AdminDeps.Restart` seam (composition binds a graceful self-SIGTERM; the process's two-phase drain runs and the supervisor starts it again). It is **not** an in-process reload and the page says so: with `restart: no` the instance stays down. Bound in **both** boot modes — the restricted boot is exactly what a restart exists for — and one signal per process: the first request wins, a concurrent second is a no-op 409. |
| Snippets | Per-row `e.g.` examples (existing `settingsFieldMeta`), the OIDC redirect-URI copy button (existing `data-copy` pattern), and the §4.7 TOML view. |
| Reset to default | §4.5 (`?reset=<key>`, link-seeded, save-gated). |
| Secrets | Always masked (`SecretMask`), overwrite-only, never echoed on rejection, never in a URL — unchanged contract (`setup.ParseValues`, `settingsApplySetupValues`, `structSliceMaps`). The Access page renders the same masks from the same schema values. |
| Dangerous actions | None exist on the area today. Anything destructive added later (e.g. a packages-GC trigger) MUST use the `danger.js` typed-confirm + server re-check idiom (guideline §7 Danger Zone). The loud-warning cards (auth `none`, packages anonymous-writable) are the honest-danger pattern for configuration that is dangerous without being an action. |
| Same-origin | Every POST keeps the `settingsSameOrigin` check. |

---

## 6. Access and safety

- **One gate for the whole area**, and it is the existing one: the
  `SetupAccess` binding (`server.setupAccess`: open in defaults / restricted /
  auth-`none`; else an authenticated host admin). The three new read pages
  bind through the same seam — **the page gate mirrors the API gate**
  (`/api/v1/setup*` answers with identical status; a page must never leak what
  the API protects — AGENTS field lesson). The fallback
  (`authMode() != "none" && !p.Admin` → 403) is kept for unwired-build safety.
- **Non-admin visibility:** under real auth the nav link is hidden (never
  shown-then-403’d — the org-tabs precedent, Forgejo #348/#358), and a direct
  visit renders the designed denied view with the API’s 401/403 status (§2.4).
  Under auth `none` the pages are open **by design** (zero-config law 10) and
  carry the loud warning cards. 404 is never used to hide the area: the
  instance’s existence is not a secret from its own users, and the recovery
  copy is the product.
- **Secrets** never leave the server (§5). **Writes** are the existing two
  (`validate`, `save`) plus the discovery probe — no new write paths.

---

## 7. Mobile and accessibility

Per page, at ~390 px and at 200 % zoom, both themes, checked rendered
(headless drive at the canonical host — the redirect field lesson):

- The area nav stacks full-width above content (the repo-settings responsive
  rule); the section strip scrolls internally, never wraps (nav-strip rule).
- `.setup-row` is one column (label above control) — already the app.css
  behaviour; the anchor strip and search input go full-width.
- Every table (boot errors, metrics kv is exempt — `kv` wraps) sits in
  `relative overflow-x-auto` inside a `min-w-0` ancestor; no page-level pan
  (`documentElement.scrollWidth === clientWidth` asserted in the drive).
- Keyboard: skip link → area nav (Tab) → search → form rows in document
  order; no positive `tabindex`; the global `:focus-visible` ring carries
  focus (F7); `details` disclosures are native. The leave-guard and filter
  islands add no focus traps.
- Announcements: result banners are `role="status"`/`alert`; the auth-test
  fragment keeps `aria-live="polite"`; the filter island leaves DOM order
  stable (no live-region churn).
- Contrast is carried entirely by `--pd-*` tokens (F2) — no hex literal ever
  lands in an admin template. The 11 px note/`muted` text inherits the
  PLAN-THEMES W1 fixes (§8.5).
- Touch targets ≥ 24 px; the anchor pills and row reset links ride the
  `.btn`/`link` paddings (the §4b exception table applies to in-table links).

---

## 8. Implementation plan

### 8.1 Files

| File | Change |
|---|---|
| `internal/webui/admin.go` | **new** (M2): the area group — nav model, shell data (`AdminPage`), the overview/access/maintenance handlers, the new `AdminDeps` seam additions (`Health`, `Maintain`), `sectionWhy`. M2 also **moves** the setup page handlers out of `settings.go` into `admin.go` and renames the exported seam types (`SettingsSetupView` → `AdminConfigView`, etc.) with the `cmd/spindle/webui.go` + `serve.go` wiring updated in the same change (pre-1.0: one shape). |
| `internal/webui/settings.go` | loses the setup routes/handlers/data (moved); keeps repo/org/keys settings |
| `internal/webui/render.go` | `ShowSetup` → `ShowAdmin`, new visibility rule (§1.2) |
| `internal/webui/public.go` | `reservedOwnerNames` gains `"admin"` |
| `internal/server/router.go` | `/setup` lines → `/admin*`; `mountSetupOnly` serves the new paths; 503 copy |
| `internal/server/setup.go`, `setup_api.go` | unchanged logic; comments re-pointed |
| `internal/webui/templates/pages/admin/{overview,config,access,maintenance}.html` | **new** page templates (one page set each, per the pages-vs-partials glob rule) |
| `internal/webui/templates/partials/admin_config_rows.html` | the moved+renamed `settings_setup.html` (rows, advanced groups, row editors, auth-test fragment) |
| `internal/webui/templates/partials/admin_shell.html` | the shared shell partial (area nav, header, banners) |
| `partials/header.html` | nav + menu entries (§1.2) |
| `internal/setup/setup.go` | + `AnchorOf(key)`; nothing else changes |
| `web/src/islands/leaveguard.js` | **new** (§5) |
| `web/src/islands/index.js`, `web/src/islands/filter.js` | boot the guard; reuse the filter (no change unless the Advanced-open hook needs one flag) |
| `internal/maintain` | no change — `Metrics()` already exported |
| `cmd/spindle/webui.go`, `serve.go` | bind the new `AdminDeps.Health`/`Maintain` (composition-owned, fail-closed nil = degraded card) |
| `web/src/app.css` | **no change** (no new CSS: everything composes existing classes; the guideline gains the area entry, not the stylesheet) |

### 8.2 New seams (behind the one gate; one controlled write)

```go
// AdminDeps additions (composition binds; nil = degraded card, never a 500):
Health  func(ctx) AdminHealthView   // boot state, drain phase, prewarm pending,
                                    // warnings — the same facts readyz reports
Maintain func(ctx) maintain.MetricsSnapshot // Maintainer.Metrics(), nil-safe
Restart func()                      // the restart seam (§5): composition binds a
                                    // graceful self-SIGTERM; tests inject a fake
```

No new *write* seam exists anywhere in this plan beyond the restart signal.
(Concurrency: `Health`/`Maintain` are point-in-time atomic reads —
`Maintainer.Metrics()` is a mutex-guarded snapshot; nothing blocks, no lock
crosses a store call, satisfying law 3. `Restart` runs once per process, after
the response is flushed, behind the in-memory one-signal guard.)

### 8.3 Tests

- **Move/labels (M1):** every existing `/setup` test in
  `internal/webui/settings_test.go` (20 `TestSettingsSetup*` functions),
  `settings_setup_rows_test.go`, `coverage_settings_test.go`,
  `identity_ui_parity_test.go`; `internal/server` router tests
  (`x_gaps3/4_test.go` mountSetupOnly matrices, `router_test.go`); the
  restricted-mode lane in `internal/e2e/e2e_test.go` (lines 283–285 drive
  `GET /setup`). The `/api/v1/setup*` tests
  (`setup_api_test.go`, `setup_group_access_test.go`) are **unchanged**.
- **New gates:** per-page admin/open/denied matrix (mirroring the API status,
  the `settings_role_matrix_test.go` pattern); the
  `mountSetupOnly`-serves-`/admin*` matrix; `reservedOwnerNames` collision
  test; the wiring guard extended so no rendered admin seam is nil.
- **Anchors/search/changed/reset:** the filter island is DOM-tested via the
  template tests + the rendered drive; `?changed=1` / `?reset=` are handler
  tests (partition counts, seeded values, nothing-saved assertions).
- **The existing invariants stay asserted:** zero-edit byte-identical save,
  keyed errors open Advanced groups, secrets never echoed, backup file
  once-only, restart-key list, OIDC fragment. No budget assertion is weakened.
- **The restart control:** the gate matrix (401/403), a wrong/missing confirm
  word (422 + named inline error, no signal), the accepted request (seam called
  exactly once, `admin_restart` audited, the flushed "restarting" page),
  concurrent requests (one signal, the second 409), the nil seam (503), the
  Configuration card's key links and its absence on a no-op save, and the
  composition binding in both boot modes (tests never signal their process).
- **A11y:** the `a11y_test.go` landmark/heading suite extended to the four
  pages; the rendered drive (desktop + 390 px + keyboard pass) is part of
  every UI milestone’s definition of done (AGENTS §4.9).
- **Directory (§3.7):** the users list (order, cursor, the by-email family
  skipped, profile-only rows), search hit/miss over username and display name
  with the scan bound and its continuation, the organizations list with member
  counts, the one membership walk (owner marker, truncation), the deactivate
  confirmation matrix (missing reason / wrong confirmation / own account →
  422 with the seam untouched), the reactivate transition, the sentinel→status
  mapping (404/409/503), the gate matrix (anonymous 401, non-admin 403), the
  not-wired/empty/read-error states, `BindDirectory`, the audit records for
  both transitions, and the credential-boundary lane: a disabled account's
  session refused 403, its pre-deactivation session and `wgt_` token 401 after
  reactivation, a fresh mint accepted, machine tokens untouched, a lookup
  error 503, and the same-second (fail-closed) watermark boundary. The
  account-state cache has its own budget test (`countStore`): one exact-key GET
  cold, ZERO store ops across 50 warm lookups, a re-read after the 10 s TTL, a
  local deactivate invalidating immediately, negative entries cached, a fault
  never cached, and the LRU's capacity/eviction/TTL/invalidate arms. The e2e
  lane drives it against the in-test IdP: sign in, deactivate through the page,
  the live session refused, the callback refused, reactivate, old session still
  revoked, fresh sign-in works, and both audit rows present.

### 8.4 Milestones (each independently green)

| # | Milestone | Contents | Est. |
|---|---|---|---|
| M1 | **The move** | URLs (`/admin*`), nav labels, page titles, `reservedOwnerNames`, mountSetupOnly + 503 copy, all in-tree references (§9), test moves. Zero behavioural change beyond names. | 1 d |
| M2 | **Area shell + Overview** | `admin.go` extraction + type renames, `admin_shell.html`, the Overview page (instance/health/maintainer/config-status cards, first-run checklist), new seams + wiring guard, gates. | 1.5 d |
| M3 | **Configuration lift** | anchors + section strip, `.card-header` + `sectionWhy` copy, key search (filter island + Advanced-open), `?changed=1`, `?reset=`, TOML view, leaveguard island, restart-card links. | 2 d |
| M4 | **Access** | the page (sign-in method, host-admin directory, OIDC card + moved discovery form, sessions), denied-view rework into the shell. | 1 d |
| M5 | **Maintenance** | metrics/bridge/storage/health-link cards, empty states, the seam binding. | 1 d |

Each milestone: `make fmt && make vet && make test && make race && make cover`,
plus the rendered drive for UI milestones; `make e2e` for M1 (it touches the
restricted-mode lane).

### 8.5 Theme-work dependencies

**Nothing here waits on PLAN-THEMES.** All pages compose existing classes and
tokens, so W3’s palettes re-skin the area for free. Two riders already folded
into PLAN-THEMES **W1** touch files this plan renames — its §8.4-12
(`role="alert"` on the two `.err-line` sites in what becomes
`admin_config_rows.html`) and its §2-6/7 (11 px note contrast): whoever lands
second updates the file name in the other plan; both plans pin the same
behaviour, so order does not matter. New admin templates must not regress
either (no raw `err-line` without a role; no hex).

### 8.6 Follow-up cards (NOT in these milestones; each needs a new seam or a law-8 schema decision)

Instance runner-fleet view · instance webhook directory · users/orgs directory.

**Landed 2026-11-22:** packages “run GC now” (the instance-scope `packages-gc` task on the
Maintenance page, `internal/packages/gcadmin.go`) and the instance audit listing (`/admin/audit`
with filters, backed by the existing moderation audit family — no new listing seam was needed once
the family existed). Both are described as built in §3.5.

**Landed 2026-11-22: the users/orgs directory** (`/admin/directory`, §3.7) — the card that needed a
law-8 schema decision, taken explicitly there: deactivation lives on `users/<username>/user.json`
(append-only fields), is enforced by one `accountGate` at the credential boundary, and revokes
sessions and `wgt_` tokens permanently via a `tokens_revoked_at` watermark. Instance runner-fleet and
webhook-directory views remain deferred.

---

## 9. Migration and compatibility (pre-1.0: change in place)

**Deleted:** the `/setup` routes (page, POST, auth-test), the
`templates/pages/settings/setup.html` + `partials/settings_setup.html` names,
the `ShowSetup` chrome field, the “Setup” nav/menu/title strings, the
`walgit: … open /setup` 503 copy.

**Changed in the same commits (M1 owns the sweep):** `README.md` (quick start
lines 20/22/65), `AGENTS.md` (law 10 mentions `/setup`), `DEVIATIONS.md`
(new D-entry recording the move + rationale), and the 22 `docs/` files that
name the path:

- `docs/go/`: `01_overview`, `06_server_http`, `07_api`, `08_bundles`,
  `11_config_cli`, `12_web_ui`, `15_testing`, `16_packaging`, `17_ssh`,
  `README` (`.md` each)
- `docs/features/`: `01_identity_permissions`, `02_issues`,
  `10_git_import`, `14_packages`, `15_threads`
- `docs/style-guideline.md` — §0/§7 file references re-pointed + a new
  “Administration area” entry + a Decisions line (law 12)
- `docs/AUDIT-V1.md`, `MASTER_RUST_SPEC.md`, `PLAN-V1.md`, `PLAN-V2.md`,
  `PLAN-THEMES.md` — historical notes stay as history; only living route
  tables change

plus `Dockerfile`/`compose.standalone.yml`/`scripts/publish/*.yml` where they
name the path, and the Go files listed in §8.1.

**Unchanged (wire contracts):** `/api/v1/setup*`, `internal/setup` package
name, `/services/setup.json`, every bucket/TOML/protobuf shape, the save
byte-contract, `SecretMask`, the `setup.Field` JSON tags.

**Publish consequences:** the GitHub squash-publish
(`scripts/publish-to-github.sh`) ships whatever is tracked — the M1 doc/code
sweep rides the normal PR→`main`→publish flow; README’s “Run with docker
compose” path is verified against the new URL in M1’s rendered drive. No
`.gitignore` interaction.

---

## 10. Risks

1. **Restricted-mode regression** — a missed `mountSetupOnly` registration
   turns the recovery path into a 503 loop. Mitigated by the §8.3 route-set
   matrix test (all four verbs × both modes).
2. **Owner-name collision** — `/admin` reserves the name (§1.4); the same
   accepted trade as `explore`, documented in the router comment.
3. **The doc sweep is broad** (22 files under `docs/` + the three root files) —
   mechanical, but a missed operator-facing doc (README first) ships a wrong
   instruction. M1 greps `/setup` to zero in `docs/`, README and templates
   before review.
4. **Query composition** (`?changed=1` + `?reset=<key>` + the client filter)
   — a small matrix test pins each combination.
5. **New seams must fail closed** — `Health`/`Maintain` are nil-able; a nil
   seam renders a degraded card, never a 500; composition’s wiring guard
   asserts the real build binds them.
6. **a11y test churn** — the h1/eyebrow change rewrites pinned strings in
   `a11y_test.go`/`settings_test.go`; intentional, in-milestone.
7. **The leave-guard island is new JS** — smallest possible surface
   (`beforeunload` only), covered by the keyboard/drive pass; no framework,
   no state machine.

---

## Appendix A — every schema group → its destination

**Measured from `setup.BuildSchema(config.Defaults(), …)` on `main`
(`bebd11c`): 19 groups, 149 keys** (the task brief’s “146/18” is stale by
three keys and one group — `threads` was added since).

| Group (schema order) | Keys | Edited at | Read/status view |
|---|---|---|---|
| `server` | 20 | `/admin/config#server` | Overview instance card (listen, public_url, roles) |
| `auth` (`server.auth.*`) | 18 | `/admin/config#auth` | **Access** (whole page), Overview first-run step 3 |
| `store` | 12 | `/admin/config#store` | Overview instance card (backend), Maintenance storage card |
| `cache` | 15 | `/admin/config#cache` | Maintenance storage card |
| `wal` | 15 | `/admin/config#wal` | — (repo WAL tab remains the ops surface) |
| `maintenance` | 7 | `/admin/config#maintenance` | Maintenance maintainer card (loop facts) |
| `placement` | 4 | `/admin/config#placement` | Overview instance card (serve/maintain roles) |
| `compaction` | 7 | `/admin/config#compaction` | Maintenance (passes via metrics) |
| `bundles` | 11 | `/admin/config#bundles` | — |
| `lfs` | 4 | `/admin/config#lfs` | — |
| `upstream` | 4 | `/admin/config#upstream` | — |
| `git` | 9 | `/admin/config#git` | — |
| `telemetry` | 4 | `/admin/config#telemetry` | Maintenance health card (metrics link) |
| `events` | 3 | `/admin/config#events` | **Maintenance** event-bridge card |
| `releases` | 1 | `/admin/config#releases` | — |
| `import` | 8 | `/admin/config#import` | — |
| `attachments` | 1 | `/admin/config#attachments` | — |
| `packages` | 3 | `/admin/config#packages` | — (GC follow-up card would land here) |
| `threads` | 3 | `/admin/config#threads` | — (fleet follow-up card would land on Maintenance) |
| **total** | **149** | one editor | three read views |

### A.2 The 19 “why this matters” lines (`sectionWhy`)

`server` — How this instance answers HTTP: the listen address, request limits, and the URL it advertises to the world.
`auth` — Who can sign in, and who becomes a host admin — the front door for every other permission.
`store` — Where repository data actually lives — the bucket is the repository; everything else is a cache.
`cache` — Local disk warmth: what is cached, how large it may grow, and what evicts.
`wal` — The write-ahead log: batching, checkpoints and freshness — the durability and speed of every push.
`maintenance` — The background loop that keeps repositories small and healthy (checkpoints, fsck, follow).
`placement` — Which hosts serve and maintain which repositories in a multi-host deployment.
`compaction` — Repacking git objects into fewer packs as they accumulate.
`bundles` — Pre-generated clone bundles that make cold clones fast: schedules, retention, serving.
`lfs` — Large-file storage: enabling, size limits, and how objects are served.
`upstream` — The upstream origin this instance follows for read-only mirroring.
`git` — How the git subprocess layer behaves: engine, filters, object format, history packs.
`telemetry` — What the instance logs and exposes to metrics scrapers.
`events` — The instance’s outbound event webhook bridge.
`releases` — Upload size limit for release assets.
`import` — Bounds and SSRF rules for the repository import surface.
`attachments` — Image size limit for issue and pull-request attachments.
`packages` — Per-repository OCI registry limits — under auth `none` the registry is anonymous-writable; set credentials before exposing it.
`threads` — Instance CI policy: run retention, first-time-fork approval, default job permissions.

---

## Appendix B — rejected alternatives (with reasons)

- **Keep `/setup`, relabel only.** Rejected: the URL would say “first-run”
  forever while the area means daily operation; nav and address would
  disagree, which is exactly the dishonesty this repo’s style rules forbid.
  The move is one pre-1.0 sweep, then done.
- **Redirect `/setup` → `/admin/config` (301).** Rejected: pre-1.0 forbids
  aliases/shims; nothing in-tree may even *produce* the old URL after M1.
- **Per-subsystem editing pages (a “Store” page, a “WAL” page, …).**
  Rejected: each would fork the whole-document save (byte-identical contract
  applies to the *complete* file) and multiply the 149-key surface across
  URLs an operator must hunt. One editor + anchors + search + read views
  gives the same findability with one truth.
- **A “Runners & secrets” admin page.** Rejected for now: those are
  repo-scoped registries; an instance view needs a cross-repo list seam that
  does not exist. Inventing it inside this plan would bundle a feature with a
  redesign. Follow-up card instead (§8.6).
- **An “Automation” page (schedules/retention/webhooks/events).** Rejected:
  it would contain only config keys (edited once, on Configuration) and one
  bridge-status card — a fourth read view with two rows on it. The bridge
  status lives on Maintenance; the keys keep their anchors.
- **Breadcrumbs.** Rejected: no coordinate hierarchy exists to breadcrumb;
  the side-nav + eyebrow already state position; a new pattern requires a
  guideline extension for zero gain.
- **Client-side validation on blur.** Rejected: duplicates the parsers
  (`ParseDuration`/`ParseByteSize`/range checks) in a second language — the
  drift risk the `EnumValues` single-table rule exists to prevent.
- **A restart button that claimed an in-process reload.** Rejected as dishonest: there is no
  reload. The restart control (added later, §5) says what it does — the process **exits** and a
  supervisor (a container `restart:` policy, a Swarm service task, systemd `Restart=`) starts it
  again; with `restart: no` it stays down. The control is bound in the restricted boot too, because
  that is the recovery loop: save a valid file, then restart.
- **Renaming `/api/v1/setup*` / `internal/setup` / `/services/setup.json`.**
  Rejected: frozen wire contracts (§1.5) — the churn buys no operator value.
