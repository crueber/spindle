# V1 wrap-up plan

> **Status:** proposed (2026-11-20). Audits the README's v1 requirements against the shipped code and
> against GitHub's behaviour, then specifies the work that closes v1. This is a *release* plan; the
> feature contracts stay in `docs/features/` and the build order stays in `docs/go/README.md`.
> Relationship to existing docs: `docs/features/09_rollout.md` is the **as-built record** of the
> collaboration layer (waves A–E, landed 2026-09-04). It is not a v1 release plan and does not
> describe GitHub parity; where this document and 09 disagree, 09 is history and this is the plan.

## 0. Definition of done for v1

v1 ships when **every checkbox under "v1 release requirements" in `README.md` is true and each item
meets the GitHub-parity bar in §4**, with the verification in §6. "Parity" means: a user who knows
GitHub can do the same task in spindle through the web UI (or the documented API/SDK), and the
behaviour is specified and tested — not that every GitHub feature is copied.

**Scope decisions taken 2026-11-20** (they bound everything below): OCI/container is the only
package ecosystem in v1; release source archives and asset download counts are v1; **every
user-facing setting and policy must be editable in the UI** — raw JSON is not a v1 surface; the GCS
backend is removed rather than implemented; "sync fork" is v1; the registry reuses the existing
token/OIDC identity with repository-role scopes and introduces no second credential type.
**Amended the same day:** the **CLI** and **Threads** (CI — the feature formerly called Actions) are
**v1 requirements**, not v1.1; and README rendering must be **GitHub-compatible** (raw HTML that
GitHub allows must render the same way, safely).

Deliberately **not** v1 (README `Backlog`, and P9): code search, projects, wiki, insights,
moderation, sponsorships, forum, ownership transfer, SAML/SCIM, merge queue, auto-merge, stacked
PRs, Discussions, and — inside Threads — a third-party action marketplace, caches and OIDC cloud
credentials.

## 1. Audit — README v1 list vs the code (2026-11-20)

| # | Item | README | Reality | Evidence |
|---|---|---|---|---|
| 1 | git storage | ✅ | **shipped** | `internal/store` (fs/s3/memory), `internal/wal`, `internal/git`, smart v0/v2, LFS, bundles, SSH |
| 2 | issues | ✅ | **shipped** | `internal/issues` + API + pages + SDK; assignees are API-only in the UI |
| 3 | pull requests | ⬜ **stale** | **shipped** | `internal/pulls` + `internal/review`; open/get/diff/commits/comments/merge/merge-task/update-branch/head/forks/review-requests/threads; pages, SDK, e2e |
| 4 | tags | ✅ | **shipped (thin)** | `internal/tags` (light/annotated); no browse page, no delete |
| 5 | releases | ⬜ **stale** | **shipped** | `internal/releases`: draft/publish, prerelease, latest, assets (Range/ETag), pages, SDK, e2e |
| 6 | packages | ⬜ | **absent** | no registry of any kind (see §5.1 and `docs/features/14_packages.md`) |
| 7 | fork | ✅ | **shipped** | `internal/pulls/forks.go`, `forkexec.go` (manifest-sharing landed, #424); no "sync fork" |
| 8 | webhooks | ✅ | **shipped** | `internal/notify/webhooks.go` (HMAC, SSRF guard, retry, delivery ring); org hooks API-only |
| 9 | oidc | ✅ | **shipped** | discovery, PKCE, nonce, signed state, ID-token verify; no claim→team mapping |
| 10 | cli | *(was v1.1)* | **partial** | `cmd/spindle` has serve/compact/bundle/repo/wal/import/mirror/config; no identity, org, issue/PR/release, packages or threads commands, and no machine-readable output contract |
| 11 | threads (CI) | *(was "actions", v1.1)* | **absent** | no runner, no workflow discovery, no job execution; `docs/features/12_runner.md` was a DRAFT, superseded by `15_threads.md` |

Two README corrections follow from this table: **pull requests and releases are done** (ticked in this
change), and `packages` is the only unchecked v1 item with no implementation.

## 2. GitHub-parity matrix

Each row states GitHub's behaviour as the reference, what spindle does today, and the delta this plan
must close. Rows marked **—** are at parity already; they stay in the table as the acceptance record.

### 2.1 Pull requests

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Open from a branch, same repo | yes | yes | — |
| Open from a fork (cross-owner) | yes | yes (UI picker lists the fork network — **WP2.1 landed 2026-11-20**) | — |
| Draft / ready-for-review | yes | yes | — |
| Conversation, comments, timeline, reactions | yes | yes | — |
| Commits / files changed, unified + split, line comments, suggestions | yes | yes | — |
| Reviews: approve / request changes / comment; threads resolve | yes | yes | — |
| Required checks gate | yes | yes (policy) | — |
| Merge: merge commit / squash / rebase; delete branch after | yes | yes | — |
| Update branch (merge base into head) | yes | yes | — |
| Labels / milestone / assignees on the PR | yes | yes (sidebar + `PUT …/pulls/{num}` + SDK; list filters by label — **WP2.2 landed 2026-11-20**) | — |
| Branch protection rules UI (required reviews, status checks, who may push) | yes | yes (Settings → Branch protection writes the validated policy — **WP2.3 landed 2026-11-20**) | — |
| Auto-merge, merge queue, stacked PRs | yes | no | **non-goal** |

### 2.2 Releases

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Release on an existing tag, title, markdown notes | yes | yes | — |
| Draft / prerelease / latest | yes | yes | — |
| Assets: upload, download (Range/ETag), delete | yes | yes | — |
| Auto-generated release notes from merged PRs | yes | yes (autodraft) | — |
| Auto-built source archives (zip, tar.gz) | yes | yes (2026-11-20) | **WP3.1 landed** |
| Download counts per asset | yes | yes (2026-11-20) | **WP3.2 landed** |
| Draft that creates its tag on publish | yes | yes (2026-11-20) | **WP3.3 landed** |

### 2.3 Tags

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Lightweight + annotated tags | yes | yes | — |
| Tag list page with commit, date, author | yes | yes (`/tags`, 2026-11-20) | **WP4.1 landed** |
| Delete a tag (with policy check) | yes | yes (2026-11-20) | **WP4.2 landed** |
| Tag protection rules | yes | backlog (`protect` policy covers it) | **non-goal for v1** |

### 2.4 Fork

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Fork a repository (manifest-sharing, no copy) | yes | yes | — |
| Fork network listing | yes | yes | — |
| PR from a fork | yes | yes (**WP2.1 landed 2026-11-20**) | — |
| Sync fork from upstream | yes | yes (task, **WP5 landed 2026-11-20**) | — |

### 2.5 Issues

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Create/edit/close/reopen, comments, timeline, reactions | yes | yes | — |
| Labels, milestones | yes | yes | — |
| Assignees | yes | yes (sidebar control + form post — **WP6.1 landed 2026-11-20**) | — |
| Cross-references and closing keywords | yes | yes (linkify + refs) | — |
| Search/filter | yes | list filters (state/assignee/label/milestone) | **non-goal** (code search is P9) |

### 2.6 Webhooks

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Repo hooks: create, ping, list deliveries, delete | yes | yes | — |
| Secret + HMAC signature | yes | yes (`X-Walgit-Signature`) | — |
| Enable/disable, rotate secret in the UI | yes | yes (repo + org tabs — **WP6.2 landed 2026-11-21**) | — |
| Redeliver a delivery | yes | yes (repo + org — **WP6.2 landed 2026-11-21**) | — |
| Org-level hooks in the UI | yes | yes (org Webhooks tab — **WP6.2 landed 2026-11-21**) | — |

### 2.7 OIDC

| Capability | GitHub (as org SSO) | spindle today | Delta |
|---|---|---|---|
| Login via OIDC discovery + PKCE | — | yes | — |
| Allowlist enforcement | — | yes | — |
| Claim → user attribute mapping | yes | username/email only | **acceptable for v1** |
| Claim → org/team membership | yes | manual membership | **non-goal** (SAML/SCIM is P9) |

### 2.8 Git storage

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| Smart HTTP v0/v2, SSH, LFS, bundle-uri | yes | yes | — |
| Filesystem + S3 backends | — | yes | — |
| Archive download (`/archive/<ref>.tar.gz`) | yes | no | **WP3.1** (shares the archive writer) |

### 2.9 Threads (CI)

| Capability | GitHub Actions | spindle today | Delta |
|---|---|---|---|
| Workflow files in the repo, triggered by push/PR/schedule/manual | yes | none | **WP10** |
| Jobs in containers, steps with `run:`/`uses:`, services, matrix | yes | none | **WP10** |
| Results reported as checks (required-checks gate) | yes | checks exist, no producer | **WP10** |
| Fork PR safety: base-branch workflow, no secrets, approval | yes | none | **WP10** (server-side policy) |
| Runner deployment: Docker-in-Docker, labels, capacity | yes | none | **WP10** (capacity 1 default) |
| Third-party action marketplace, caches, OIDC cloud creds | yes | none | **non-goal for v1** |

Full specification: `docs/features/15_threads.md`.

### 2.10 CLI

| Capability | GitHub (`gh`) | spindle today | Delta |
|---|---|---|---|
| Server lifecycle (serve, config check/dump) | n/a | yes | — |
| Repository admin (create/list/info, policy, settings, WAL) | `gh repo` | yes | — |
| Identity, orgs, teams, tokens | `gh api` / `gh auth` | no | **WP11** |
| Issues, PRs, releases, checks (read + write) | `gh issue/pr/release` | no | **WP11** |
| Packages and Threads | `gh` + API | no | **WP11** |
| Machine-readable output (`--json`), stable exit codes, shell completion | yes | no | **WP11** |
| `spindle api` escape hatch (raw request, token from env) | `gh api` | no | **WP11** |

### 2.11 README rendering

| Capability | GitHub | spindle today | Delta |
|---|---|---|---|
| CommonMark + tables, task lists, autolinks, strikethrough | yes | yes | — |
| Raw HTML: `<details>/<summary>`, `<video>/<source>`, `<picture>`, `<kbd>`, `<sub>/<sup>` | yes (sanitized) | **no** — escaped or dropped | **WP12** |
| `align="center"` on block elements | yes | **no** | **WP12** |
| Relative links/images resolved in-repo | yes | yes | — |
| Safety: script/style/iframe/event handlers stripped, URL scheme allowlist | yes | n/a (no raw HTML) | **WP12** (the sanitizer is the boundary) |

Field evidence (2026-11-20, a live instance): a README using `<div align="center">`,
`<details>` and a `<video>`-style demo link renders as escaped text — GitHub renders all three.

## 3. Work packages

Each WP is one implementable unit with its own acceptance criteria (§6). Ordering is §4.

### WP1 — Packages: an OCI registry (the only new feature)

Full design: **`docs/features/14_packages.md`**. Summary: implement the OCI Distribution Spec over the
existing object store, namespaced per repository (`host/<owner>/<repo>`), authenticated with the
existing tokens/OIDC through a Docker token endpoint, with a repository Packages page, API + SDK, and
a maintainer GC pass. No npm/Maven/NuGet in v1 (Q1).

### WP2 — Pull-request parity gaps

- **WP2.1 Fork PRs from the UI (LANDED 2026-11-20).** The new-PR form's head selector offers the
  parent's live fork index (readable forks, grouped under "Forks") in addition to the base owner's
  repos; `?from_repo=` prefills, and the existing `OpenInput.Fork` path opens the cross-owner PR.
- **WP2.2 PR metadata (LANDED 2026-11-20).** `PUT …/pulls/{num}` carries labels/assignees/milestone,
  applied by the exported 02 `PatchThreadMetadata` (one writer, 03 never forks the rules); the PR
  sidebar renders the shared metadata card, the list filters by label, and the SDK exposes
  `pulls.setLabels/setAssignees/setMilestone`.
- **WP2.3 Branch protection rules, in the UI (LANDED 2026-11-20).** Settings → Branch protection
  edits required approvals, required status checks, who may push (roles) and force-push/deletion
  restrictions, writing the same validated policy document; the raw JSON editor is only an advanced
  disclosure, and the tab shows the read-only effective policy.

### WP3 — Release and archive parity (LANDED 2026-11-20)

- **WP3.1 Source archives.** `GET /{o}/{r}/archive/{ref}.{tar.gz,zip}` — server-rendered from git
  (`git archive` argv added to `docs/go/04_git.md`), streamed, ETag from the tree id, and linked from
  the release page and the repo page's "Download" affordance. Reuses one writer for releases.
- **WP3.2 Asset download counts.** A counter per asset updated on the byte route, shown on the
  release page and in the API. Cheap write: one CAS bump, best-effort, never on the ACK path.
- **WP3.3 Draft with a new tag.** Allow a draft release to declare `target` and create the tag on
  publish, matching GitHub's "choose a tag → create new tag on publish".

### WP4 — Tags (LANDED 2026-11-20)

- **WP4.1 `/tags` page** listing tag, target commit, date, author, and the release it backs.
- **WP4.2 Delete tag** (`DELETE /api/.../refs/tags/<tag>`) gated by the `protect` policy effect, with
  the page action and SDK method.

### WP5 — Sync fork (LANDED 2026-11-20)

A "Sync fork" action that fetches upstream and fast-forwards the fork's default branch when it is a
strict ancestor; otherwise it reports divergence and links to a PR. Uses the existing mirror/egress
machinery (`internal/egress`, `internal/mirror` presets) so SSRF rules are shared, and runs as a
`(repo, kind=sync-fork)` task with progress packets (law 7).

### WP6 — Issues and webhooks UI parity

- **WP6.1 Issue assignees in the UI (LANDED 2026-11-20)** — the sidebar control posts the same
  full-set PATCH the API accepts (candidates: collaborators at role ≥ triage).
- **WP6.2 Org webhooks tab (LANDED 2026-11-21)** — `/{owner}/settings?tab=webhooks` reuses the repo
  tab's canonical panel (create/ping/deliveries/delete), adds enable/disable + secret rotation to
  **both** tabs, and adds `redeliver` to both API surfaces (retained-body replay; a payload-less
  row answers 410 rather than faking a delivery). CLI: `spindle org webhook list|create|delete|
  ping|deliveries|redeliver`; SDK: `webhooks.redeliver` / `orgs.webhooks.redeliver`.

### WP7 — Storage surface truth (LANDED 2026-11-20)

The inherited project shipped an extra cloud backend that spindle does not build. It is gone:
`store.backend` (enum + validation), its config struct, the setup schema, the serve branch, its
event-envelope parser, the contract target and every doc/README reference. Supported backends are
**filesystem** (the zero-config default), **S3** (rustfs/MinIO) and **memory** (tests).
`DEVIATIONS.md` records the removal as a divergence from the inherited Rust project.

### WP9 — UI editability sweep (the "everything does" rule)

The scope decision makes UI editability a v1 property, so one pass audits every user-facing setting
and policy for an API-only path and closes what it finds. Full inventory and evidence:
`docs/AUDIT-V1.md` (round-2 "UI editability" area) and `/tmp/opencode/aud2-editability.md`.

**Inventory (2026-11-21, `main` @ `5c33f19`):** 145 server-config keys in 18 groups, the per-repo
settings surface, org settings, account settings and the Threads/runner surface were read from the
code and sampled in a live browser. **18 gaps: 2 P0, 3 P1, 8 P2, 5 P3.** The two P0s were the most
severe defects any audit found, because both surfaces *rendered* as if they worked: every org-settings
write answered 503 (`OrgWrite` was never composed), and `/setup` could not save (21 keys
unrepresentable, a forced save writing `[""]` into untouched lists).

| # | gap | surface | status |
|---|---|---|---|
| G1 | `/setup` could not save; 21 keys unrepresentable (2 struct slices, 2 uints, 17 empty lists); a forced save wrote `[""]` into untouched lists | `/setup` | **CLOSED** — whole-schema round-trip gate (`TestFormValuesWholeSchemaRoundTrip`), byte-identical zero-edit page save (`TestSetupPageZeroEditFormSavesByteIdentical`), unsigned counters, blank list = empty list, validation errors name their key |
| G2 | every org-settings write answered 503 (`SettingsDeps.OrgWrite` never bound) | org settings | **CLOSED** — `orgWriteFn` binds identity's verbs and re-checks `CheckOrgOwner`; a guard test fails if composition ever leaves a rendered seam nil |
| G3 | static auth tokens invisible/uneditable | `/setup` auth | **CLOSED** — row editor (principal, `token_env`, write, admin), secret masked and overwrite-only, add/rotate/revoke |
| G4 | `bundles.strategy` had no editor anywhere | `/setup` bundles | **CLOSED** — the same row editor over the `BundleStrategy` fields |
| G5 | org webhooks had no UI | org settings | **CLOSED** — Webhooks tab over the shared panel; redeliver on both scopes (retained body, same id/HMAC); `notify.OrgOwner` wired |
| G6 | repo webhooks could not be edited | repo settings | **CLOSED** — per-hook edit disclosure (URL/events/active) plus a first-class "rotate signing secret" (blank rotation is 422, never a silent keep) on the shared panel |
| G7 | team membership could not be removed | org Teams | **CLOSED** — per-member remove |
| G8 | team rename/description was API-only | org Teams | **CLOSED** — rename + description on the team card |
| G9 | repo collaborator invitations were API-only | repo Access | **CLOSED** — invite form (email or username + role), pending list with copyable accept link and cancel; non-admins never see capability tokens |
| G10 | user avatar upload was API-only | user profile | **CLOSED** — multipart upload card (type/size validated), initials fallback preserved |
| G11 | org avatar removal was API-only | org Profile | **CLOSED** — "Remove avatar" on the profile form |
| G12 | maintenance ops had no run control | repo WAL | **CLOSED** — per-op run form streaming progress over the existing SSE route, terminal task inline, no auto-retry |
| G13 | Threads dispatch was API-only | Threads | **CLOSED in `fix/threads-triggers`** — Run workflow control (workflow select, ref, typed `workflow_dispatch.inputs`), plus the `inputs` 400 and check-state fidelity fixes |
| G14 | per-repo `bundles.*`/`maintenance.*`/`compaction.*`/`upstream.*` were raw-TOML-only | repo settings | **CLOSED** — structured Scheduled tab with per-section "override host config" consent, bundle rows on the `/setup` row protocol, shared coercion/validation path |
| G15 | the policy document could not be removed | repo Push policy | **CLOSED** — typed-confirm Remove policy; an emptied editor removes instead of 400ing |
| G16 | Settings → WAL was a stub | repo settings | **CLOSED** — the real panel (facts + the shared ops card) |
| G17 | no page for a user's starred repositories | user | **CLOSED** — `/{user}/starred`, visibility-filtered, keyset paging |
| G18 | `/setup` UX debt: no essentials/advanced grouping, free-text enums, a keyless error on all 145 rows | `/setup` | **CLOSED** — keyless errors stay in the banner and errors carry their key; every section now splits essentials (open) from a collapsed `<details>` Advanced group driven by the one `settingsFieldMeta` table, the seven enum keys render `<select>`s over `internal/setup`'s `EnumValues` (empty/out-of-set values still representable), and the three dead metadata keys are gone |

**File-only by design** (recorded so the sweep is not re-opened): the data dir (it locates the config
file it edits), secret *values* behind `*_env` names (the names are editable, the values never enter
the file), the `SPINDLE__*` env overlay, and write-once Threads runner/job secrets. `server.listen` is
**not** file-only — it is an ordinary `/setup` row, which is why G1 mattered.

**Deferred, documented:** ~~an accept-link list for *org* invitations (org reads redact the token;
the small follow-up rides the org settings seam)~~ — **closed 2026-11-21**: the org invitations tab
is owner-gated like its API, renders the human accept link once on create (and on the created
pending row), shows the masked marker on later reads, and accepts through the same
`/invitations` page. ~~and the G18 remainder~~ — **closed 2026-11-21** (essentials/advanced
grouping + enum selects).

### WP10 — Threads (CI)

Full specification: **`docs/features/15_threads.md`**. Summary: workflow files under
`.spindle/workflows/`, runs created server-side from push/PR/schedule/dispatch **with the workflow
revision selected per trust class** (fork PRs run the base branch's file, no secrets, approval gate);
jobs claimed by runners over a documented JSON protocol with **attempt-scoped tokens**; one container
per job with steps via `docker exec` and checkout inside the container; Docker-in-Docker deployment
with **capacity 1 by default**; results written into the existing checks surface so the merge gate
works unchanged; a Threads tab with live logs, cancel and rerun.

Milestones: **T1** workflow discovery + run/job records + checks integration (no runner yet, manual
run state); **T2** runner protocol + executor with a fake docker CLI, including a configurable
`capacity` and concurrent claims; **T3** real DinD e2e + fork trust policy + secrets; **T4** UI/SDK +
schedule/cron + capacity controls + a **100-job scale test** (many capacity-1 runners, and one
capacity-100 runner). Capacity is a runner setting with no server ceiling; default 1.

### WP11 — Full CLI

Close the parity gaps in §2.10: `spindle auth`/`token`, `user`, `org`, `team`, `issue`, `pr`,
`release`, `checks`, `packages`, `threads`, plus `spindle api` (one authenticated request, token from
env/config). Every command gets `--json` (stable schema, documented), meaningful exit codes (`0 ok`,
`1 user error`, `2 usage`, `3 not found`, `4 auth`), and shell completion generated from the same
command table. The CLI is a client of the existing JSON API — no second source of truth, no direct
store access beyond the operator commands that already exist (WAL/fsck/compact).

### WP12 — GitHub-compatible README rendering

Close the gaps in §2.11 without weakening the XSS boundary. Design: keep the current CommonMark
pipeline, and add **raw-HTML passthrough through an allowlist sanitizer** built on
`golang.org/x/net/html` (already a dependency, law 1): parse the HTML fragment, walk the tree, emit
only allowed tags/attributes and http/https/mailto/relative URLs, drop scripts/styles/iframes/event
handlers/`javascript:`/`data:` (except `data:image/*` for images), and re-serialize the sanitized tree.
Allow GitHub's practical set — `details`, `summary`, `video`, `audio`, `source`, `picture`, `kbd`,
`sub`, `sup`, `mark`, `del`, `ins`, `abbr`, plus `align` (left/center/right) on block elements — and
keep an explicit test corpus of known-bad inputs the sanitizer must neuter.

Acceptance: the `loom` README renders like GitHub (centered header, `<details>` collapsing, an
embedded demo video), and the sanitizer corpus passes with no `template.HTML` bypass.

### WP13 — OIDC admin groups (setup access)

`/setup` (and the setup JSON API) must not be unlocked by a CLI/env secret. Global admin — and
therefore setup access — comes from a **signature-verified OIDC group claim**: `server.auth.admin_groups`
(default `["spindle-admins"]`) matched case-insensitively against the ID-token claim named by
`server.auth.groups_claim` (default `"groups"`, a JSON array of strings or a single string). The
`admin` flag stays independent of `write`; groups never bypass the `allowed_domains`/`allowed_emails`
admission rule; an empty `admin_groups` disables group admins; `admin_emails`/`admin_domains` keep
working (migration path). The `SPINDLE_SETUP_TOKEN` env var, the `?token=`/Bearer bypass, and the
page's token field are **deleted** (setting the env var has no effect). Group membership rides the
walgit session/token wire so a signed-in browser keeps group-derived admin, and the admin policy is
re-applied per request.

Acceptance: a signed ID token with an admin group opens `/api/v1/setup` (200) and the `/setup` page;
`groups: ["other"]` or an absent claim is 403; `groups_claim = "roles"` reads `roles`; case
differences match; `admin_groups = []` grants nothing; the setup page names the group rule rather
than a token; the config keys render in the setup schema. Docs: 06 §3.4/§8.4/§8.5/Decisions, 11,
15 §5.3 matrix, 16 D6, features/01 Decisions.

### WP8 — Documentation and checklist reconciliation (landed with this plan)

README checkboxes corrected; `03_pull_requests.md` status fixed; `docs/features/README.md` P9 amended
(packages is now in scope for v1, pointing at 14_packages.md); `09_rollout.md`'s deferred items
corrected (fork manifest-sharing landed); this plan linked from the README.

## 4. Sequencing

| Milestone | Contents | Why this order |
|---|---|---|
| **V1-alpha** | WP8 (docs truth), WP7 (storage truth — landed), WP1 skeleton: `/v2/` ping, blob/manifest PUT/GET, token auth, `docker push/pull` of a single-image repo | Packages is the largest risk; prove the protocol first |
| **V1-beta** | WP1 complete (tags/list, delete, GC, UI page, API/SDK, feature toggle), WP2.1, WP3.1 | A user can push an image, open a fork PR, and download a release tarball |
| **V1-rc** | WP2.2, WP2.3, WP3.2/3.3, WP4, WP5, WP6, WP9, **WP12** | Parity gaps that do not gate the headline features; WP9 proves the editability rule; WP12 makes READMEs render like GitHub |
| **V1-rc (large)** | **WP10 T1–T4** (Threads), **WP11** (CLI) | The two items that must ship for v1 per the 2026-11-20 amendment; WP10 is the critical path and should start immediately after V1-alpha |
| **V1** | Full ladder green, `docs/EVIDENCE.md` entry for the registry hot paths, README all-ticked, release button | Ship |

WP1 and WP3.1 are independent and can run in parallel; WP2 is independent of both; WP5 depends on
nothing in WP1–WP4 but should follow V1-beta so the egress machinery is settled.

## 5. Design notes that constrain every WP

- **The bucket is the repository (law 4).** Every WP above stores its state in the object store under a
  documented prefix; nothing on local disk or in the WAL except what the existing docs allow.
- **No LIST on a hot path.** The registry must probe by digest/tag, not enumerate. Tag listing is the
  one paged LIST and it is a user action, not a push path (`docs/features/14_packages.md` §5).
- **Round-trip budgets (law 6).** A `docker push` of a small image must fit the same discipline as a
  git push: parallel blob PUTs, one manifest CAS, no read-after-write verification on the happy path.
- **Tasks, not spinners (law 7).** WP5 and registry GC are task kinds with progress and SSE attach.
- **Seams only (law 8).** The registry attaches as a `RouteProvider` (`internal/server/bind_api.go`)
  plus a feature package; it must not import upward into `internal/server`.
- **Docs move with code (law 12).** Each WP updates its feature doc and appends decisions there.

## 6. Acceptance criteria

Common to every WP: `make fmt vet test cover test-web` green; `-race` on touched packages; rendered
Chromium proof at desktop and ~390 px for UI changes; the feature's API covered by httptest and its
user flow by e2e where a real client exists.

Per WP:

- **WP1** — the OCI conformance subset in `14_packages.md` §8 passes (push/pull by tag and digest,
  auth allow/deny per role, delete + GC); `docker push`/`pull`/`manifest inspect` work against a live
  instance; blob PUTs are parallel and the push adds no sequential read; the Packages page lists tags
  with pull commands; feature toggle off hides the page and 404s the routes.
- **WP2.1** — a cross-owner fork PR can be opened entirely from the UI.
- **WP2.2** — labels/milestone/assignees editable on a PR from the UI and the SDK; list filters use them.
- **WP2.3** — the three common protection rules are editable without hand-editing JSON, and each is
  enforced by a test (the policy effect already is).
- **WP3.1** — `git archive` output for a tag and a commit matches `git archive` run locally
  (byte-identical for tar with fixed metadata; documented flag differences for zip).
- **WP3.2** — counts increment on download and survive a restart (bucket, not memory).
- **WP3.3** — a draft with a new tag creates the tag at publish and never before.
- **WP4** — the tags page and deletion work with the policy check; deletion of a tag backing a release
  is refused with a clear message.
- **WP5** — fast-forward sync is idempotent and single-flighted; divergence is reported without
  rewriting history; SSRF rules match the mirror path.
- **WP6** — assignee editing and the org webhook tab pass the same tests as their API twins.
- **WP10** — the protocol contract, scheduler (including fork trust class), executor argv and the
  real Docker-in-Docker e2e pass; a fork PR runs with no secrets and a read-only token; a job's
  results appear as a required check and block the merge when failing; live logs render at 1440 and
  390 px.
- **WP11** — every §2.10 row has a command; `--json` output is schema-pinned by tests; `spindle api`
  works against the local server with a token from env; completion generates for bash/zsh/fish.
- **WP12** — the loom README's `<div align>`, `<details>` and video render like GitHub; the
  sanitizer corpus (script, iframe, event handler, `javascript:`, `data:` exfiltration, style
  injection) is neutralized; no page gains a `template.HTML` path outside the sanitizer.

## 7. Risks

1. **Registry scope creep.** "Packages" could mean five ecosystems. v1 is OCI only (Q1); every other
   ecosystem is a separate plan.
2. **Registry GC correctness.** Blobs are shared by manifests; deleting a tag must not delete a blob a
   second manifest references. GC runs as a sweep (no refcount LIST on delete) — specify and test it.
3. **Storage-surface drift.** Config, README, setup UI and the store doc must agree (WP7); a backend
   name accepted by validation but not buildable is a boot error waiting for the next operator.
4. **The editability rule expands scope.** "Everything is editable in the UI" can surface settings with
   no form today (WP9); each one costs an endpoint + a page section. The sweep bounds the list before
   the rc milestone, and anything it uncovers that is genuinely rare lands as a named follow-up rather
   than silently shipping as JSON-only.
5. **Doc truth.** The README checklist had drifted from the code. WP8 makes it the single status source
   and the plan links every claim to evidence.
6. **Threads runs untrusted code.** A CI runner is the highest-risk surface in the product: root in a
   container, a Docker daemon behind it, secrets nearby. The non-negotiables in `15_threads.md` §5
   (capacity 1 by default, base-branch workflow for forks, no secrets on fork runs, attempt-scoped
   tokens, no host socket by default) are v1 acceptance criteria, not aspirations.
7. **CLI surface sprawl.** WP11 must stay a client of the API; a command that reaches into the store
   directly becomes a second behaviour to keep correct.

## 8. Resolved scope decisions (2026-11-20)

| # | Question | Decision | Effect on this plan |
|---|---|---|---|
| 1 | Package ecosystems | **OCI/container only** | WP1 as written; npm/Maven/NuGet/RubyGems stay out |
| 2 | Release archives + download counts | **In v1** | WP3.1 and WP3.2 are required, not optional |
| 3 | Branch-protection UI | **In v1 — everything is editable in the UI** | WP2.3 required; new WP9 sweep enforces the rule |
| 4 | Legacy extra storage backend | **Removed** | WP7 landed: filesystem, S3 and memory only |
| 5 | Fork sync / code search | **Sync fork in v1; search stays deferred (P9)** | WP5 required; search unchanged |
| 6 | Registry auth | **Reuse existing token/OIDC, repo-role scopes** | WP1 as designed; no second credential type |
| 7 | CLI | **v1** (moved from v1.1) | new WP11; parity with the API surface + `--json` |
| 8 | CI | **v1, named Threads** (moved from v1.1 "actions"); Docker-in-Docker like Forgejo, clean-room | new WP10 + `docs/features/15_threads.md` |
| 9 | README rendering | **GitHub-compatible** (raw HTML through an allowlist sanitizer) | new WP12 |
| 10 | Setup access | **OIDC admin groups** (`spindle-admins` default), no env setup token | new **WP13**; `SPINDLE_SETUP_TOKEN` deleted |

Consequences worth stating plainly: v1 grows by WP2.3 (protection UI), WP9 (editability sweep) and the
release-archive work, and shrinks by the GCS backend. The README's "protection rules are backlog" line
is superseded by decision 3 and is updated when WP2.3 lands.
