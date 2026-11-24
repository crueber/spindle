# V2 feature plan: moderation, protection, ownership transfer, file edit

> **Status:** proposed (2026-11-21). Plans the next four features after the v1 release plan
> ([`PLAN-V1.md`](PLAN-V1.md)), which itself supersedes the README's old "not in v1" list. The four, in
> the order the owner asked for: **Moderation** (§1), **PR/merge protection — the gaps** (§2),
> **Ownership transfer** (§3), **File edit** (§4). §0 is the shared sequencing. This is a *plan*: no code
> lands with this document, but every section is written so an implementer re-decides nothing — keys,
> routes, role floors, argv, test names and doc obligations are named.
>
> **Every "already exists" claim was verified against the code** at `main` @ `17158e0`; the evidence
> appendix (§5) lists file paths. Where the code contradicts an older doc, the plan says so (§0.4).
>
> Standing constraints that bind all four (AGENTS.md laws, restated once so each section can cite by
> number): **L1** dependency budget (no new modules; anything needing one is an explicit amendment
> request); **L2** git is a subprocess, argv pinned in `docs/go/04_git.md`; **L3** concurrency rules of
> `docs/go/13_concurrency.md` (lock order, TryLock-only `rw` writes, no lock across a store call);
> **L4** the bucket is the repository (no durable state off the object store); **L5** bucket
> compatibility with the Rust implementation (key layout, protobuf numbers, JSON shapes, append-only
> proto); **L6** round-trip budgets; **L7** no silent waiting (tasks + SSE); **L8** features attach
> only through the seams of `docs/go/14_extensibility.md` (route providers, auth, policy effects, event
> sinks, task kinds, CLI) and never import upward or sideways into feature internals — the
> consumer-defined-interface pattern (`pulls.ChecksGate`, `Server.ReadGate`, `api.CreateOwnerGate`) is
> the house style; **L9** fail closed; **L10** zero-config first run; **L11** ≥95 % per-package
> coverage, `-race`, never weaken a budget assertion; **L12** docs change in the same commit. Pre-1.0
> means no backwards-compat shims inside spindle (the only compat is L5's bucket compat with Rust).

---

## 0. Sequencing

### 0.1 Dependency graph

```
File edit (F)  ──► none          (write funnel already exists: tags + merge precedents; PR open ships)
Moderation (M) ──► none hard     (identity/issues/review/notify all ship; T.4 wants M's audit family)
Protection (P) ──► none hard     (checks/review/pulls/policy ship; P's UI reuses the settings seams)
                 P.4 auto-merge and P.5 merge queue reuse the merge task + UpdateRefWithPack funnel
Transfer (T)   ──► M.4 (audit log entries) is the only soft edge
```

No feature gates another on code. The soft edges: transfer wants moderation's audit log (T
milestone 4), and every later feature's permission matrix should name the moderation block state
once (M) rather than three different spellings.

### 0.2 Recommended order: **F → M → P → T**

**First pick: File edit.** It is the only one of the four with zero new bucket families, zero
migration surface, and zero cross-family state: the commit *is* the git state, the PR (when the
editor proposes a branch) is the shipped pulls state. It exercises the server-side write funnel
(`git` plumbing → `Layer.Ingest` → `UpdateRefWithPack` publish) from a brand-new feature package,
which is exactly the pattern the merge-queue worker (P.5) will need — landing it first proves that
funnel is comfortable to call from outside `internal/pulls` before anything with durable state rides
it. It is also the most user-visible gap on the README backlog.

Then **Moderation**, because the block state is identity-level: protection's codeowner reviews and
transfer's accept flow both want a single answer to "is this principal blocked?" and M.3's audit
family is what T.4 writes into. Then **Protection**, whose engine changes are additive policy fields
(the registry is already open, Seam 3) and whose riskiest slice (merge queue) is last inside its own
milestones. Then **Transfer**, which touches every bucket family, wants M's audit log, and deserves a
quiet tree — its registry/catalog/fork-index fixes are the kind of work that should not race three
other features landing.

### 0.3 What this plan deliberately does not re-plan

Shipped and verified (do not re-implement; see §5 for evidence): branch protection *basics*
(`protect` + `required-reviews` effects, force-push/deletion restriction, bypass lists, push-role
bypasses from the UI, `require_checks` with the merge-time gate, stale-approval derivation, review
dismissal, the Protection settings tab), PRs end to end (open/draft/merge three ways/update-branch/
forks/sync-fork), checks end to end (statuses, CI tokens, combined view), the v1 **direct** repo
transfer (`POST …/api/transfer` with the `access.json` owner rewrite), tags, releases, threads,
packages, and the read side of file browsing (`GET …/tree|blob` API + `/tree /blob` pages).

### 0.4 Doc-truth bugs found while inventorying (fix with the first landing, law 12)

1. `PLAN-V1.md` §0 lists "ownership transfer" as *deliberately not v1* while `docs/features/
   01_identity_permissions.md` §5.4 documents the shipped v1 transfer (issue #358) and
   `internal/identity/transfer.go` implements it. PLAN-V1's §0 line is the stale one (its own §1
   audit table predates #358's landing); PLAN-V1 is a historical release record, so the fix is a
   one-line note there pointing at this plan, not a rewrite.
2. `docs/go/14_extensibility.md` §14.6 shows a built-in `jsonl` sink kind in its config example;
   `internal/events` implements only the webhook sink. Either implement `jsonl` (small; it would be
   the audit trail's free egress for WAL events) or mark the example as a design sketch. M.2 does not
   depend on it (the moderation audit family is its own objects), but the doc must stop promising a
   sink that does not exist.
3. **P.4's "free-form rule names were always legal" is false** — `internal/policy` pinned
   `ruleNameRe = ^[a-z][a-z0-9-]{0,62}$` (MASTER_RUST_SPEC line ~2031), so the planned ruleset names
   `protection:<slug>-push` were unparseable. Fixed with the P1 landing by widening the grammar to
   `^[a-z][a-z0-9:-]{0,62}$` (additive: every previously legal name stays legal; a `:`-named file is
   refused by an older reader, the same rolling-upgrade caveat §P.2 already records for effect
   fields). Recorded in `docs/features/03_pull_requests.md` Decisions.

---

## 1. Feature M — Moderation

**Package:** new `internal/moderation`. Parity bar: GitHub user blocks + minimized comments +
conversation locks + abuse reports with an admin queue + an action audit log + request rate limits;
Forgejo's block semantics (no interaction in the blocker's repos) as the reference where GitHub and
Forgejo differ.

### M.1 Problem & scope

A signed-in user gets:

- **Block a user** (directional, self-service). The blocked user can no longer interact with the
  blocker's content; reads are unaffected (blocks are *not* access control — visibility governs
  reads).
- **Hide/minimize** any conversation comment, review body, or review-thread comment, and **lock** an
  issue/PR conversation.
- **File an abuse report** on repo content and (report-only) on a user profile; repo admins and host
  admins get a **queue** with resolve/dismiss actions.
- A host-admin **audit log** of every moderation action.
- Operator-configurable **rate limits** on collaboration writes and authentication failures, and
  advisory **spam signals** that pre-file reports.

Out of scope (documented in the feature doc): instance-level user suspension/deletion (needs an
identity lifecycle decision), org-level blocks (personal blocks only — org-owned repos moderate via
the repo moderation doc), email-on-report (06 owns egress; operators wire SMTP themselves), automatic
hiding (signals are advisory, never act silently — L9's spirit: a machine verdict never removes
content by itself), and per-repo block lists (a block is a *person* relationship).

### M.2 Data model

Three object groups. New families join the frozen overwritable list in the same revision that adopts
them (14 §14.2 table + Decisions amendment, §14.11 rule 2).

**Blocks (identity scope, P1 `users/` family):**

| Key | Kind | Schema |
|---|---|---|
| `users/<principal>/blocks.json` | CAS'd (overwritable family) | `{"version":1,"blocked":["carol"],"updated_at":"RFC3339"}` — normalized principal **names** (the #370 username), sorted, unique, ≤ 500 |

- Create-on-first-block (CAS: GET-absent → `PutCreate`), then full-doc CAS with the standard
  ≤5-attempt loop (01 §4 discipline). No per-block objects; human-rate whole-doc writes (the
  `members.json` reasoning, 01 §3).
- Nothing else in identity changes: a block never mutates `access.json`, rosters, or policy.

**Repo moderation state (P1 `repos/<o>/<r>/meta/` family):**

| Key | Kind | Schema |
|---|---|---|
| `repos/<o>/<r>/meta/moderation.json` | CAS'd (overwritable family) | `{"version":1,"locked":false,"locked_by":"","locked_at":"","lock_reason":"","hidden":{…},"updated_at"}` |
| `repos/<o>/<r>/meta/moderation/reports/<id:012x>.json` | **Create**-only immutable | report record below; terminal state is a compensating field on the *index* row, the object is never rewritten (the invitations discipline: the record is evidence, not mutable state) |
| `repos/<o>/<r>/meta/moderation/reports/index.json` | CAS'd (P4 hot window) | `{"version":N,"open":[card…],"resolved_recent":[card…],"compacted_through":"00003a","next_id":42,"updated_at"}` |

- `hidden` map, target-key grammar (one form per content family, all lowercase, colons):
  - `thread:<num:06x>:event:<seq:012x>` — issue/PR conversation comment bodies (the shared issues
    family covers both kinds);
  - `review:<num:06x>:<seq:012x>` — review bodies;
  - `review_thread:<num:06x>:<tid>:<seq:012x>` — review-thread comments.
  Value: `{"by","at","reason":"spam"|"abusive"|"off-topic"|"other","note":""}`. Entries only ever
  appear via triage+ actions and leave via unhide; no eviction in v1 (growth is bounded by human
  moderation volume; the doc records the bound rationale like P2 does).
- **Lock is a field on the same doc**, not an event on the thread: the existing comment lock keys on
  thread state (`internal/issues` `threadLocked`), and the moderation lock must compose with it
  without a second writer fighting the P3 header CAS. Enforcement reads `moderation.json`; the
  timeline renders a `lock_changed` *display* row derived from the moderation doc's lock fields, and
  the lock action appends nothing to the immutable event log (moderation state is not conversation
  history — decided, so the event log never lies about who said what).

**Host scope (new top-level prefix `moderation/`, peer to `repos/` and `users/` per P1):**

| Key | Kind | Schema |
|---|---|---|
| `moderation/reports/index.json` | CAS'd (P4, overwritable family) | global queue hot window: `{"version":N,"open":[{repo,id,kind,target,reporter,reason,created_at}],"resolved_recent":[…],"compacted_through":…,"updated_at"}` — rows are (repo, id) pointers; full records stay per-repo |
| `moderation/audit_state.json` | CAS'd allocator | `{"next_seq":N}` — the `collab_state.json` shape (06 §1) |
| `moderation/audit/<seq:012x>.json` | **Create**-only immutable | `{"seq":N,"at":"RFC3339","actor":"jane","action":"hide"|"unhide"|"lock"|"unlock"|"block"|"unblock"|"report_resolve"|"report_dismiss"|"transfer"|"…","target":"repo:key"|"user:principal","reason":"","context":""}` |

- The global reports index is one bucket-root CAS object written by every repo report creation —
  cross-repo contention at human rate, the `meta/repos.pb` precedent, acceptable and documented.
  Compaction follows P4 exactly (256 KiB cap, `moderation-reports-compact` task kind, LIST fallback
  over per-repo index objects — the global LIST is an admin page, not a hot path).
- The audit log is the transfer feature's sink too (T.4): every action that changes who owns what or
  what content is visible writes one Create-only record. No LIST on read except the paginated admin
  page (`after=` seq cursor).

### M.3 API

Wire conventions per `docs/go/07_api.md` §2 (plain-text errors, `[]` not null, RFC 3339, both lanes
via the `server.ExtraRoutes` chain, discovery templates via `ExposedTemplates` +
`api.RegisterExposed` with both-way tests, the Forgejo #272 rule). Role strings are the P6 ladder.

Top-level twins (`/api/v1` + `/api-browser/v1`):

| Method + path | Auth | Request → response |
|---|---|---|
| `GET /api/v1/users/{principal}/blocks` | self or host admin | → `{"version":N,"blocked":[…]}`; 404 unknown principal (the profile-existence probe) |
| `PUT /api/v1/users/{principal}/blocks/{subject}` | self or host admin | → `200 {version}`; 409 already blocked; 400 subject unknown (profile probe) or self-block |
| `DELETE /api/v1/users/{principal}/blocks/{subject}` | self or host admin | → `200 {version}`; 404 not blocked |
| `GET /api/v1/moderation/reports?state=&after=&n=` | host admin | → `{reports:[…],more}` (global queue, no-store) |
| `POST /api/v1/moderation/reports/{owner}/{repo}/{id}/resolve` | host admin | `{action:"hidden"|"locked"|"none",note?}` → `200 {report}` (the repo twin delegates) |
| `GET /api/v1/moderation/audit?after=&n=` | host admin | → `{entries:[…],more}` (no-store) |

Repo-scoped `/{o}/{r}/api/…` (+ `/api-browser` twin):

| Method + path | Auth | Request → response |
|---|---|---|
| `GET …/moderation` | read (visibility-gated) | → the moderation doc minus nothing (locks/hidden are not secrets); 404 never — absent doc renders `{version:0,locked:false,hidden:{}}` |
| `GET …/moderation/reports?state=&after=&n=` | repo admin | → `{reports:[…],more}` |
| `POST …/moderation/reports` | any authenticated with read | `{kind:"thread"\|"review"\|"review_thread"\|"user", target:{num,seq?,tid?,principal?}, reason:"spam"\|"abusive"\|"off-topic"\|"other", note?}` → `201 {id, report}`; 400 bad target key; 409 duplicate open report by this reporter for the same target |
| `POST …/moderation/reports/{id}/resolve` | repo admin (or host admin via the top-level twin) | `{action:"hidden"\|"locked"\|"none",note?}` → `200` — `hidden` hides the target, `locked` locks the thread, `none` records dismissal-only; every path writes the audit entry |
| `POST …/moderation/reports/{id}/dismiss` | repo admin | `{note?}` → `200` (alias of resolve with `action:"none"` on the wire only in the audit verb; one endpoint shape, no shim) |

**Where hide/lock actually get written:** one mutating verb pair on the repo doc, so page and API
share it (the "one writer" rule):

| Method + path | Auth | Request → response |
|---|---|---|
| `PUT …/moderation` | triage+ **for hide/unhide deltas only**; maintain+ **when the lock fields change** (the handler diffs the posted doc against the current doc and applies the stricter floor per changed field — the one-writer shape, testable) | full doc incl. `version` → `200 {version}`; 409 stale; 400 unknown target key or target that does not exist (probe the thread/review objects) |

SSE (the repo collaboration stream, 06/08 §4): new event name `moderation`, actions
`hidden|unhidden|locked|unlocked|report_filed`, payload `{num?, seq?, tid?, by, at}`. Notifications:
`report_filed` notifies repo admins (reason `moderation`); block/unblock notifies nobody (a block is
not a social event, and notifying the blocked user is hostile — decided, documented).

Spam signals: no API; they run inside the collab-write handlers (M.6) and their only observable
effect is a pre-filed report Create plus a counter `spindle_spam_flag_total{repo}`.

### M.4 UI

All server-rendered (`internal/webui`), composing `docs/style-guideline.md` vocabulary — no new
patterns are needed, and the feature doc must cite the references instead of re-deciding:

- **Profile block control** on `/{user}` (`pages/public/owner.html` handler in `public.go`): a
  `.link` "Block user" / "Unblock" action in the owner header cluster, write-gated to signed-in
  non-self viewers; your own page shows a "Blocked (N)" disclosure listing blocked names with
  unblock `.link` rows (no new page family — the profile page owns it).
- **Minimized comment card** in `partials/collab_timeline.html` (and the review-thread variant in
  `partials/collab_threads.html`): the body is replaced by a one-line muted card — "minimized by
  `<by>` — `<reason>`" — with a triage+ `unhide` `.link`; the **body never reaches the browser**
  (server-side redaction; stronger than a CSS hide, and the XSS boundary stays the server renderer).
  The API timeline responses omit the body for hidden targets the same way.
- **Lock control** in the issue/PR conversation header: a `.btn` lock/unlock (maintain+ only),
  a lock badge row (`.pill`) rendered while locked, and the composer disabled-with-reason exactly like
  the existing merged lock (`lockState` in `internal/webui/collab.go` — reuse the pattern, add the
  moderation arm).
- **Report affordance** as a `.link` "Report" on every comment card and review body (authenticated
  read floor): opens an inline `<details>` form (reason radios + note field), posting to
  `POST …/moderation/reports`. Confirmation renders the report id.
- **Repo reports tab** under `/{o}/{r}/settings` (`partials/settings_repo.html` extension, admin
  gate): open/resolved tables in the canonical list anatomy (`pages/collab/issues.html` reference),
  per-row resolve controls (`hidden`/`locked`/`none` radios + note), states: empty ("No reports"),
  403 for non-admins (no tab rendered), and pending-lock display.
- **Host admin queue** at `/admin/reports` (new `pages/public/` page, host-admin gate — the
  `/setup`-style gate): global rows with repo links, filter `?state=`, resolve twin, and an audit
  viewer at `/admin/audit` (same gate, seq-cursor pages).
- **Mobile:** every new surface is a stacked form or a table in the standard `overflow-x-auto`
  wrapper; the report `<details>` form is full-width at ~390 px. Browser proof at desktop + 390 px is
  part of M.9.

### M.5 Permissions

| Action | anonymous | read | triage | write | maintain | admin (repo) | host admin |
|---|---|---|---|---|---|---|---|
| View repo (incl. moderation doc) | per `visibility` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Block/unblock (own list) | 401 | ✓ (self) | ✓ | ✓ | ✓ | ✓ | ✓ (any list) |
| File a report | 401 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Hide/unhide content | — | — | ✓ | ✓ | ✓ | ✓ | ✓ |
| Lock/unlock conversation | — | — | — | — | ✓ | ✓ | ✓ |
| List repo reports / resolve | — | — | — | — | — | ✓ | ✓ |
| Global queue + audit | — | — | — | — | — | — | ✓ |

**Blocked principal** (blocker = the user who owns the namespace being written to): comments,
reactions, review submits, review-thread comments, review-request additions, and issue/PR *opens*
in repos under the **blocker's user namespace** → `403` with the shared neutral text
`not permitted in this repository` (amended by the M1 landing: the original `403 blocked by <owner>`
would tell the blocked user a block exists, contradicting M.12 open question (b) — the answer is
never); invitations created by the blocker targeting the blocked principal → `403`; mentions of the
blocker written by the blocked principal produce **no notification**. Reads, stars, watches, and forks are unaffected (documented
decision: interaction-only, matching Forgejo). Org-owned repos: personal blocks do not apply (the
repo moderation doc is the org's tool). Host admins are never blocked-out (they act as operators;
documented).

Where each check lives (the one-gate rule, so page and API cannot diverge):

- Role floors live **inside the service verbs** (`internal/moderation`), and both the API handlers
  and the page handlers (`internal/webui/settings_seams.go` + `collab.go` write paths) call the
  verbs — never a reimplementation. The page layer only *hides affordances*.
- Block checks live in a `ModerationGate` interface defined **by each consumer package** and wired
  from composition (the `ChecksGate` precedent, L8):
  - `internal/issues`: `type ModerationGate interface { BlockedFromRepo(ctx, owner, repo, actor string) (bool, string, error) ; ThreadLock(ctx, o, r string, num int) (bool, string) }` — consulted by `AddComment`, `AddReaction`, thread-open verbs, `AddIssue/AddPR` (open), before any CAS.
  - `internal/review`: the same shape for `SubmitReview`, thread comments, review-request add.
  - `internal/identity` invite create: `BlockedBy(ctx, blocker, subject) (bool, error)`.
  - `internal/notify` mention fan-out: filter consults `BlockedBy`.
  Nil gate (feature off / not wired) = allow (fail-open here is correct: absence of the feature must
  not break writes; the *feature's* failures fail closed per L9). A composition guard test pins every
  rendered seam non-nil (the `TestSettingsWiringBindsEveryRenderedSeam` precedent).

### M.6 Enforcement points & anti-bypass tests

1. **Collab write verbs** (issues/review handlers, both lanes + the page forms that call the same
   verbs): gate at the top, after role resolution, before any CAS. Table-driven proof:
   `TestBlockedPrincipalCannotComment/React/Review/OpenThread/RequestReview` in each consumer
   package — one arm per verb, plus an arm proving the **page route** returns the same 403 when the
   API is called from the page form.
2. **Timeline projection** (`internal/webui` render + the API thread GETs): hidden targets are
   redacted server-side in both lanes. `TestHiddenBodyNeverReachesClient` asserts the absent body on
   page HTML and on the JSON payload.
3. **Lock** composes with the existing closed/merged lock: `TestModerationLockRefusesEveryone`
   (including admins — matches the v1 no-override lock decision) and `TestUnlockRestoresComments`.
4. **Rate limits** are middleware (M.7): no verb can skip them because they mount above the
   handlers. `TestRateLimit429BothLanes` + `TestRateLimitRetryAfter`.
5. **Audit completeness:** every service verb writes its audit record before returning —
   `TestEveryModerationVerbAudits` walks the verb table.

### M.7 Concurrency & cost model

- All moderation writes are CAS loops (≤5 attempts → 409/503 per the class of the parent verb); no
  new locks, no changes to 13 §2's list (L3). The global reports index and audit allocator are
  single-CAS objects at human rate (the `collab_state`/`next_num` reasoning).
- **Block check cost:** one conditional GET of `users/<owner>/blocks.json`, memoized in an
  in-process LRU stamped by the CAS version (the `access.json` LRU pattern, 01 §4.1). Worst case per
  collab write: one control-plane GET — same class as the visibility read, asserted in a budget test
  (`TestBlockGateAddsNoExtraReadWhenCached`).
- **Moderation doc read cost:** thread pages fold the doc into the existing thread fetch: +1
  conditional GET per conversation page (mutual-collab class, no-store). Hidden-map lookups are
  in-memory after that single read.
- **Rate limiter:** hand-rolled token bucket (L1: no module). In-process map keyed
  `(class, principal-or-IP)` under one mutex, idle-key sweep, 10k key bound. **Bucket state is
  instance-local memory and deliberately not bucket state** (L4 reasoning: a rate limiter is a
  control-plane cache like the render LRU; durability would throttle nobody after a restart
  anyway — document this in the feature doc). 429 plain text + `Retry-After: 60`; anonymous keys on
  the remote address (no X-Forwarded-For trust without an explicit `[rate_limit] trust_proxy`
  boolean — default false, documented).
- Config: new reflective `[rate_limit]` section (`internal/config`): `enabled` (default **false** —
  L10: zero-config boot changes nothing), `collab_write = "30/min"`, `issue_open = "10/h"`,
  `report = "5/h"`, `auth_fail = "10/min"`, `trust_proxy = false`. Setup schema + env overlay +
  size validation ride the existing reflective machinery; rows render on `/setup` per the row
  protocol. Classes cover issues/PR comments, reviews, reactions, reports, review-requests, and
  login failures — **not git pushes** (a CI push storm must not throttle; documented).
- Spam signal scorer: in-memory recent-body ring per principal (≤256 entries), one SHA-256 of the
  body; ≥3 repeats in 24 h → one pre-filed report + the rate-limit counter. Advisory only (M.1).

### M.8 Migration / compatibility

- Pre-1.0, no shims: nothing existing changes shape. All new families are additive; the frozen
  overwritable list amendment (14 §14.2 + Decisions) is part of the landing commit (L12).
- L5: no Rust-era key is touched — `users/`, `repos/…/meta/`, and the new `moderation/` prefix are
  spindle-side collaboration families the Rust implementation never wrote; policy.json is untouched
  by this feature.
- Existing threads render unchanged (absent moderation doc ≡ `{locked:false,hidden:{}}` on read —
  the access.json synthesis pattern).

### M.9 Tests

- `internal/moderation`: table-driven service tests (block add/remove/idempotence, hide target
  validation, lock floors, report lifecycle, audit write), httptest both lanes for every route with
  the full role matrix, ≥95 % coverage (`make cover`), `-race`.
- Consumer packages: the gate tables of M.6, plus the nil-gate composition guard.
- `internal/server`: rate-limit middleware tests (per class, per lane, Retry-After, anonymous key).
- e2e (`internal/e2e`, real git + real store): block → comment refused on both lanes; hide → body
  absent from a fresh browser fetch; lock → composer refused; rate limit → 429 burst.
- Budget assertions: block-gate LRU test (M.7); timeline +1-GET cap test.
- Browser proof: every M.4 surface at desktop and ~390 px, console clean, no horizontal pan
  (AGENTS §2 rule) — with the canonical-host caveat for loopback drives.

### M.10 Docs (same commit as each milestone)

`docs/features/16_moderation.md` (new, the normative contract — keys, grammar, gates, floors);
`docs/features/README.md` (table row + P9 amendment: moderation leaves the deferred list);
`docs/go/14_extensibility.md` (frozen-list amendment + Decisions); `docs/features/02/03/04`
(amended: the consumer-owned gate interfaces + lock composition); `docs/features/06` (report_filed
notification class + mention filter); `docs/go/06_server_http.md` (rate-limit middleware section);
`docs/go/11_config_cli.md` (`[rate_limit]`); `docs/go/12_web_ui.md` (page routes + redaction note);
`README.md` backlog tick on completion. No dependency amendment (M needs none).

### M.11 Milestones (each independently green)

1. **M1 Blocks** — `blocks.json` + profile UI + consumer gates + notify mention filter + audit
   family skeleton (block/unblock verbs audit).
2. **M2 Hide/lock** — `meta/moderation.json` + PUT verb + redaction in every timeline projection
   (page + JSON) + lock composition + SSE frames.
3. **M3 Reports & queues** — per-repo + global families, resolve verbs, settings tab, `/admin`
   pages, notifications.
4. **M4 Audit completeness + rate limits + spam signals** — `[rate_limit]` section + middleware,
   scorer, audit coverage sweep (this is the milestone transfer's T.4 depends on). **Landed
   2026-11-22, as built in §M.11d.**

### M.11a As built (M1 Blocks, 2026-11-21)

M1 landed as `internal/moderation` + consumer-owned gates; the normative contract is
[`features/16_moderation.md`](features/16_moderation.md). What shipped, against this plan:

- **State**: `users/<principal>/blocks.json` exactly as §M.2 (whole-document CAS, ≤5 attempts,
  sorted/unique/lowercased names, ≤500). No per-block objects, no LIST; gate reads are conditional
  GETs stamped by the CAS version (`TestBlockGateReadsOnce`). Absent reads as the empty doc; corrupt
  or unknown-version documents fail closed.
- **Audit family**: `moderation/audit_state.json` (CAS allocator) + `moderation/audit/<seq:012x>.json`
  (Create-only) exactly as §M.2, with `block`/`unblock` records. Written post-commit; a failed write
  is logged, never a phantom revert (M4 owns the completeness sweep).
- **Gates**: consumer-owned interfaces, one per package — `issues`/`pulls`/`review`
  `ModerationGate{BlockedFromRepo}`, `identity` and `notify` `BlockGate{BlockedBy}`. Enumerated verbs:
  issues `CreateIssue`, `AddComment`, `AddReaction`, `UploadAttachment`, `patchThread` (behind
  `PatchIssue`/`PatchThreadMetadata`); pulls `OpenPR`, `UpdatePR`, `AddComment`, `StartMerge`,
  `UpdateBranch`, `DeleteHead`; review `SubmitReview`, `DismissReview`, `OpenThread`,
  `AddThreadComment`, `ResolveThread`, `UnresolveThread`, `AddRequests`, `RemoveRequests`; identity
  `CreateOrgInvite`/`CreateRepoInvite`; notify's fan-out filter. Exempt with reasons: own-reaction
  removal (#594), labels/milestones (repo config), forks, stars, watches, personal tray state.
- **Anti-bypass canary**: `TestEveryMutationVerbIsGated` AST-scans the five packages for
  principal-taking `*Service` methods and requires each to be gated, delegating to a gated verb, or
  exempt with a documented reason; a gated entry must call `requireNotBlocked`.
- **Notify**: one `BlockedBy` probe per resolved target before the activity append, so the overflow
  fan-out path cannot reintroduce a suppressed recipient; probe errors drop that recipient (fail
  closed) and log. No duplicates; the reverse direction is untouched.
- **API/UI**: the §M.3 block twins (`GET`/`PUT`/`DELETE`, both lanes, discovery registered) and the
  §M.4 profile control (`POST /{owner}/block|unblock`, self-only "Blocked (N)" disclosure). The
  refusal text is neutral (M.5 amended above).
- **Cost**: gate = one conditional GET (+ `IsOrg`'s one HEAD on the owner kind); no new locks; no new
  task kind; no new module.
- **M2–M4 still own**: `meta/moderation.json` hide/lock + the `PUT …/moderation` one-writer verb and
  timeline redaction; reports (per-repo + global queues, resolve/dismiss, settings tab and `/admin`
  pages, `report_filed` notifications); `[rate_limit]` config + middleware, the spam scorer, and the
  audit-completeness sweep (transfer T.4's dependency). The audit family, the consumer-gate pattern
  and the profile surface are in place for them to extend without a re-plan.

### M.11b As built (M2 Hide/lock, 2026-11-21)

M2 landed as the repo moderation document + the projection/lock arms of the same consumer gates; the
normative contract is [`features/16_moderation.md`](features/16_moderation.md) §10. Against this plan:

- **State**: `repos/<o>/<r>/meta/moderation.json` (CAS'd whole-document: `hidden` target map with
  `{by,at,reason}`, per-conversation `locks`, and the plan's flat repo-wide lock envelope). The
  document `version` is the API revision token; absent reads as `{version:0,locked:false,hidden:{},locks:{}}`;
  corrupt/unknown versions fail closed. Target grammar implemented verbatim, with `seq` 0 legal (the
  opened event is the thread/PR description). Hide/unhide/lock/unlock are idempotent; the verbs write
  the M1 audit family with actor + target + `prev=` state, and publish one `moderation` SSE frame
  (action + state, never a body).
- **Why `locks` is keyed**: the plan pins a flat `locked` field on a repo-scoped document, but a lock
  is conversation-scoped. The flat fields stay as the repo-wide envelope; `ThreadLock` is the
  disjunction; the UI writes the keyed entry (Decisions in 16 §10).
- **One writer**: `PUT /{o}/{r}/api[-browser]/moderation` (full doc + version; hidden deltas triage,
  lock deltas maintain, stale 409, unknown/absent target 400, same-origin) and the page actions
  `POST …/issues|pull/{num}/moderation/{hide|unhide|lock|unlock}` call the same service verbs.
- **Projections** (page + JSON, one rule): issue timeline (`issues.GetThread`), PR timeline and
  description (`pulls.GetPR`), review bodies (`review.ListReviews`/`GetReview`), review-thread
  comments (`review.GetThread`), the `moderation` SSE frame. Redaction is server-side (body omitted
  below triage; moderator sees the marker + original), and ETags fold the document revision so a hide
  cannot 304 a stale body. Notifications/webhooks/listings carry no comment body; verified, nothing
  to redact.
- **UI**: hidden card + moderator "Show original"/"Unhide"/hide popover; lock chip visible to all,
  maintain+ lock/unlock control, reason banner, disabled composers (comment, review, thread reply).
- **Floors**: hide/unhide triage, lock/unlock maintain, host admin always, anonymous 401; locks refuse
  host admins too (the #594 no-override decision) and compose with closed/merged.
- **Still open**: M3 (reports + queues + `report_filed` notifications) and M4 (`[rate_limit]`, spam
  scorer, audit-completeness sweep). No re-plan needed — the gates, the audit family and the one-writer
  verb are the seams they extend.

### M.11c As built (M3 Reports & queues, 2026-11-22)

M3 landed as the reports family + the two queues; the normative contract is
[`features/16_moderation.md`](features/16_moderation.md) §11. Against this plan:

- **State**: `repos/<o>/<r>/meta/moderation/reports/<id:012x>.json` (Create-only immutable evidence)
  and its `…/reports/index.json` CAS'd hot window, plus the host-scope
  `moderation/reports/index.json` projection (rows are `(repo,id)` pointers). Terminal state is a
  compensating field on the index card; the record is never rewritten. Ids are per-repo
  (`next_id`, allocated inside the append CAS) and the evidence Create is compensated on failure.
  Absent windows read empty; corrupt ones fail closed; duplicate-open filings are 409; every
  mutation is one bounded CAS loop (no new locks).
- **Compaction**: the P4 256 KiB cap. Every committed write trims the oldest resolved cards inline
  (open cards are never evicted, `compacted_through` advances), and the global window starts the
  `moderation-reports-compact` task at the cap. The task — the family's only LIST — rebuilds the
  global window from the per-repo indexes; no request path lists.
- **Verbs**: file (authenticated read), repo list/resolve/dismiss (repo admin), global list/resolve
  twin and audit viewer (host admin), on both lanes. `hidden` calls the M2 `Hide` writer, `locked`
  the M2 `LockThread` writer, `none` records dismissal only; the effect runs before the report CAS
  (idempotent retry), every path audits (`report_resolve`/`report_dismiss`), and a terminal report is
  409. `GET /api/v1/moderation/audit` is the seq-cursor viewer over the M1 family.
- **SSE/notify**: `moderation` action `report_filed` with `{num?,seq?,tid?,by,at}` (no state), and a
  `report_filed` notification to the repo admins (reason `moderation`) whose signature has no field
  for reported content.
- **UI**: the `.link` Report `<details>` form on every comment card / review body / thread reply
  (page endpoint `POST …/issues|pull/{num}/report`, confirmation renders the id), the admin-only
  Settings → Reports tab (new partial + one dispatch line; per-row hidden/locked/none resolve and the
  resolved tail), and the host `/admin/reports` + `/admin/audit` pages under the existing area gate.
- **Still open**: M4 only — `[rate_limit]` config + middleware, the spam scorer (it files through
  the M3 verb), and the audit-completeness sweep (transfer T.4's dependency).

### M.11d As built (M4 Audit completeness + rate limits + spam signals, 2026-11-22)

M4 landed as the `[rate_limit]` middleware, the advisory spam scorer and the audit-completeness
sweep; the normative contract is [`features/16_moderation.md`](features/16_moderation.md) §12–§14.
Against this plan:

- **Config**: the reflective `[rate_limit]` section with exactly the pinned keys and defaults
  (`enabled = false`, `30/min`, `10/h`, `5/h`, `10/min`, `trust_proxy = false`). `config.ParseRate`
  is the one parser (`N/s|N/min|N/h`, positive); a nonsense value is a validation error naming the
  key, and env (`SPINDLE__RATE_LIMIT__*`), the setup schema rows, page rendering and coercion ride
  the existing machinery. **Every key is restart-required** — the buckets are built at boot and the
  process does not reload config — so the restart list carries them (decided, documented in
  `docs/go/11_config_cli.md`).
- **Limiter**: hand-rolled token buckets (no module) in `internal/server/ratelimit.go`, mounted as
  the innermost middleware so both lanes share one gate per class. Key = principal when signed in,
  else the socket address; `X-Forwarded-For` is ignored unless `trust_proxy`, then first hop only.
  One mutex-guarded map, clock-driven idle sweep, hard 10k-key bound with deterministic eviction.
  429 plain text + `Retry-After: 60`, neutral. Classes: `collab_write`, `issue_open`, `report`,
  `auth_fail`; **git pushes and reads never classify** (a test drives 150 git requests through an
  enabled limiter). State is instance-local memory by design (a control-plane cache, not bucket
  state).
- **Spam signals**: an in-memory per-principal ring (≤256 bodies, one SHA-256 each); the same body
  ≥3 times in 24 h pre-files exactly one report through the M3 filing tail (kind `user`, reporter
  `spam-signal`, reason `spam`, the repetition count in the note — never the body), with a per-body
  cooldown and the M3 duplicate-open rule as the two anti-double-file layers, plus
  `spindle_spam_flag_total{repo}`. Bodies under 40 bytes and anonymous callers are not scored; it
  never blocks or changes a floor.
- **Audit completeness**: `TestEveryModerationMutationVerbAudits` walks the moderation AST (every
  mutating `*Service` verb audits, delegates to one that does, or is allow-listed with a reason) and
  `TestEveryAdministrativeMutationAudits` verifies a fixed table (config save → `AuditWrite` →
  `RecordAdmin`; token mint allow-listed; identity role grants and the v1 transfer allow-listed with
  their owning follow-up, T.4). A negative-control test proves the sweep rejects a synthetic gap.
  The Administration area's config save is the family's first non-moderation caller.
- **Landed after M4 (2026-11-22, `feat/admin-gc-audit`, the PLAN-ADMIN §8.6 card):** the two
  allow-listed rows are gone. Identity writes `access_grant` (`PutAccess`), `org_member_grant|revoke`
  (`SetMember`/`RemoveMember`) and `team_member_grant|revoke` (`SetTeamMember`/`RemoveTeamMember`),
  attributed by `identity.WithActor` at the handlers (the CLI records `system`); the transfer body
  writes `transfer` at `repo:<destination>` after verification. The packages-GC trigger writes
  `packages_gc`. A third canary table, `auditFamilies`, pins this plan's five required families
  (config save, token mint/revoke — mint is self-service, revocation IS the config save's
  `session_secret` rotation —, admin grant, repo transfer, moderation) to the function that must
  carry the audit call, with `TestAuditFamiliesHaveTeeth` dropping each family's anchor in turn.
  `/admin/audit` gained the filters its viewer needed (verb, actor, repo or `-`, since) as plain
  query params that preserve the `after=` cursor.

### M.12 Risks & open questions

- **Gate coverage is the whole feature.** A missed collab verb is a block bypass; the mitigation is
  that the gates are consumer-owned interfaces *inside the verbs*, plus a coverage test that greps
  the collab packages for mutation verbs and asserts each consults a gate (a drift canary, the
  `TestExposedCoversRoutes` style).
- **Hidden-map growth** on a heavily moderated repo: bounded by human actions; revisit if a real
  repo hits 10k entries (compaction would move old entries to a cold LIST-backed object — a named
  future, not planned).
- Open questions for the owner: (a) should personal blocks apply in **org** repos where the blocker
  is an owner (Forgejo says no, this plan says no — confirm); (b) should the blocked user be told
  (this plan: never, no notification); (c) default-on rate limits with generous numbers vs
  default-off (this plan: off, L10 bias — confirm).

---

## 2. Feature P — PR / merge protection: the gaps

**Packages touched:** `internal/policy` (additive fields), `internal/pulls`, `internal/review`,
`internal/checks` (none for statuses themselves), `internal/webui` (settings tab rework), new
`internal/codeowners` (P.3). Parity bar: GitHub branch protection *rulesets*, CODEOWNERS review
requirements, strict up-to-date checks, merge methods per branch, auto-merge, merge queue.

### P.0 Inventory — what already ships (verified against the code, §5)

| Capability | Where | Status |
|---|---|---|
| Protected refs: deny create/update/delete/force-push, per-rule bypass (incl. `team:`/`role:` expansion) | `internal/policy/effect_protect.go`; evaluated at receive-pack step 4 (`internal/server` pushPipeline, both transports). The merge task does NOT apply it (`internal/pulls/policy.go checkMergeTarget`, `merge.go` step 4): `restricts`/`bypass` govern direct pushes only | **ships** |
| Required checks (1–32 contexts, union across rules, bypass-independent at merge) | `protect.require_checks` (parsed `effect_protect.go:101-124`), merge-time gate `pulls/checks.go` at `merge.go` step 4 (`ChecksGate`) | **ships** |
| Required reviews (min approvals, non-author rule #586, stale derivation, dismissal events, most-restrictive combination) | `required-reviews` effect; `internal/review` dismiss + `CheckRequiredReviews` at `merge.go:189` | **ships** |
| Review approval states (approve/request-changes/comment), last-review-wins rollup, stale badge | `internal/review` (`review_summary`) | **ships** |
| Force-push/deletion protection, push-role bypasses, one protection form writing the validated policy | Settings → Branch protection (`internal/webui/settings.go:3171-3350`): two managed rules `protection-push`/`protection-reviews`, refs list, approvals 0–50, dismiss-stale, checks list, roles→`role:<o>/<r>:<role>` bypass, block-force/block-delete | **ships** |
| Merge methods merge/squash/rebase, delete-branch, update-branch, draft refusal, comment lock | `internal/pulls/merge.go` | **ships** (per-request strategy, no per-branch restriction) |
| Threads runs reported as required checks | `internal/threads` → checks | **ships** |

**Gaps (this feature):** multiple named rulesets; CODEOWNERS path owners; strict up-to-date checks;
per-branch merge methods; auto-merge; merge queue; signed-commit presence requirement. Everything
below is *additive to the shipped surface* — no re-plan of the above.

### P.1 Problem & scope

A repo admin can define **several named protection rulesets**, each with its own ref patterns and
gate configuration, entirely from the UI (raw JSON stays the advanced escape hatch). Required
reviews can demand **codeowner approval** for touched paths. Merges onto protected refs can demand
an **up-to-date head** and can restrict the **allowed merge methods**. A PR author can arm
**auto-merge**. A maintain+ user can enqueue a PR into a **merge queue** that lands entries in
order, re-running every gate at dequeue time. Pushes to protected refs can require **signed
commits** (presence-level).

Out of scope: GitHub "recently pushed" heuristics, ruleset *status* dates/history (policy.json is
the record), per-context strictness (rule-level only, matching GitHub), merge-queue batching of
multiple PRs per attempt (one-at-a-time — see P.7), and CODEOWNERS *notification* routing (only the
review *requirement* is planned; requesting reviewers from CODEOWNERS on PR open is a named
follow-up, not planned here).

### P.2 Data model

**The enforcement language stays `policy.json`** (frozen envelope, Seam 3 open effect registry).
All engine changes are additive optional fields inside existing effects — allowed by 14 §14.12's
field rule, with the same rolling-upgrade caveat the `require_checks` Wave-05 amendment recorded
(an old binary fails closed on files it cannot parse; deploy fleet-wide before repos adopt).

`protect` gains:

```json
"require_up_to_date": true,          // bool, default false — merge-time only
"merge_methods": ["merge","squash"],// subset of merge|squash|rebase, non-empty, default absent = all
"require_signed": true              // bool, default false — push half: signature presence on old..new
```

`required-reviews` gains:

```json
"require_codeowners": true           // bool, default false — merge-time codeowner gate
```

Parse rules (fail closed, L9): unknown keys stay parse errors; `merge_methods` rejects duplicates,
unknown values, and empty lists; all three default-off when absent. Combination rules, declared per
14 §14.5: `require_up_to_date` and `require_signed` are **all-of** across matching rules the actor
does not bypass; `merge_methods` is the **intersection** across matching rules (absent = universe, so
intersection with an absent rule is a no-op).

**New durable state (CAS'd overwritable families, frozen-list amendments in the landing commit):**

| Key | Kind | Purpose |
|---|---|---|
| `repos/<o>/<r>/meta/merge-queue.json` | CAS'd | `{"version":N,"next_seq":M,"entries":[{seq,num,by,at,state:"queued"\|"merging"\|"failed"\|"landed",attempt}],"updated_at"}` — FIFO by `seq`; only queued/merging entries persist (landed/failed rows compact to a bounded recent tail of 25) |
| `repos/<o>/<r>/pulls/<num>/pr.json` additive field `auto_merge` | CAS'd | `{"enabled":bool,"by":"jane","strategy":"merge","delete_head":bool,"armed_at":"RFC3339"}` — omitted reads disabled (field-additive, the `draft` #613 precedent) |

CODEOWNERS itself is **repo content** (`CODEOWNERS` / `.spindle/CODEOWNERS` / `docs/CODEOWNERS` at
the PR's base ref, first found wins, GitHub precedence) — no bucket state. Rulesets are *not* a new
object either: a ruleset is a named pair of policy rules (P.4).

L5: `policy.json` is a Rust-era shared file; additive *optional* keys keep Rust-side round-tripping
(the Wave-05 precedent for `require_checks`; the caveat is documented in the feature doc, and golden
fixtures in `testdata/` gain one fixture exercising each new field).

### P.3 API

Everything rides existing endpoints except the queue and the auto-merge field:

| Method + path | Auth | Request → response |
|---|---|---|
| `PUT …/policy` | admin (unchanged) | now accepts the new fields; `400` + reason on any parse failure (unchanged semantics, wider grammar) |
| `PUT …/pulls/{num}` | write (author) or triage+ | gains optional `auto_merge: {enabled, strategy?, delete_head?}` — arming validates the strategy against `merge_methods` for the base ref (409 names the allowed set); arming an already-satisfiable PR **merges immediately** (GitHub parity) via the normal merge task; disarming sets `enabled:false`; merged/closed PRs → 409 |
| `POST …/pulls/{num}/merge-queue` | maintain+ | enqueue (draft/closed/merged → 409 like merge; already-queued → 200 idempotent) → `202 {task:{id}}` (the worker's task record); also the disarm verb with `{remove:true}` |
| `GET …/pulls/{num}/merge-queue` | read | → `{entry:{seq,state,by,at}, position}` or `404` |
| `GET …/merge-queue` | read | → `{entries:[{num,seq,state,by,title?}],head_seq}` (the queue page + tab badge) |

SSE: queue state changes ride the repo stream as event name `merge_queue` (actions
`enqueued|dequeued|failed|landed|removed`, payload `{num,seq}`); the existing `pull` stream events
cover auto-merge fires (they *are* merges). No new top-level routes; discovery templates register per
the #272 rule with the both-way coverage tests.

### P.4 Rulesets (UI rework, zero engine change)

`policy.json` already supports N rules; only the tab is single-ruleset today. The Protection tab
(`internal/webui/settings_seams.go`) becomes:

- a list of rulesets (`.card` rows: slug, refs, summary chips built from `settingsProtectionView`),
  each row edit/delete (`data-danger-expected` typed confirm for delete), plus one `.btn.primary`
  "New ruleset";
- a per-ruleset form at `/{o}/{r}/settings/protection/{slug}` with **all** fields of P.2/P.3/P.5:
  refs patterns, approvals + dismiss-stale, require_codeowners, checks (1–32), require_up_to_date,
  merge_methods checkboxes (default all), require_signed, push-role bypass multi-select,
  block-force/block-delete;
- rule naming: `protection:<slug>-push` and `protection:<slug>-reviews`, both carrying the same
  `match.refs`. **Migration is a read-time mapping, not a compat shim** (pre-1.0 in-place change,
  law "no shims" respected because no code path accepts the old spelling specially — the mapping
  *is* the read): rules named `protection-push`/`protection-reviews` (today's managed names,
  `settings.go:3180-3183`) present as the ruleset `default`; the first save of that ruleset rewrites
  them under the new names. The raw editor and `PUT …/policy` are unaffected (free-form rule names
  were always legal).
- Slugs: `[a-z0-9-]{1,39}` (the org-slug grammar); duplicates refused inline; the effective-policy
  read-only view (already shipped) keeps rendering every rule.

### P.5 Enforcement points (the server choke points)

Everything is enforced where the decision is observable — the honesty rule of 14 §14.5:

1. **Receive-pack** (both transports, `pushPipeline` policy step): `require_signed` — after ingest,
   `git log --format=%G? <old>..<new>` (new argv, law L2, added to `docs/go/04_git.md` with the
   per-commit exit list); any commit whose flag is not in `{G, E}` (good/expired-presence; `B`
   malformed and `N` none reject) → REJECT listing the first ten offenders. Bypass lists apply as
   always. No extra store round trips (local git spawn only) — asserted in a budget test.
2. **Merge task, step 1** (`internal/pulls/merge.go runMerge`): `require_up_to_date` —
   `git merge-base --is-ancestor <base_sha> <head_sha>` false → refuse with narration "head is
   behind base — update the branch first" (the `update-branch` hint links the button). Strategy
   validation moves from blind `validateStrategy` to also consulting `merge_methods` for the base
   ref (409 names the allowed set).
3. **Merge task, step 4 gates** (the existing trio at `merge.go:169/178/189`): `require_codeowners`
   joins them as a fourth call site — the review gate's `CheckRequiredReviews` grows the codeowner
   half (P.7), consulting `internal/codeowners` (new package, no deps — L1 clean):
   - resolve the CODEOWNERS blob at the **base ref** via `RepoView.Blob` (first-found precedence);
   - parse (hand-rolled, GitHub grammar subset: comments, patterns with `*`/`**`, leading `/`
     anchor, trailing `/` directory match, `!` negation, **last-match-wins**); unparseable lines are
     skipped (GitHub parity — CODEOWNERS is repo content, not policy.json, and fails open on bad
     lines);
   - match the PR's changed paths (the existing diff file list), group files by owner set, resolve
     `@user` → principal and `@org/team` → team members (the identity expansion seam reuse),
   - require ≥1 **fresh, non-author, surviving** approval per owner-set (codeowner approvals also
     count toward `min_approvals`, GitHub parity);
   - **fail closed on absence:** `require_codeowners` with no resolvable file → refuse "CODEOWNERS
     not found at base" (a rule demanding owners must find owners; documented divergence from
     GitHub, which fails open — the L9 bias).
4. **Merge-queue worker** (`internal/pulls/mergequeue.go`, new): a per-repo single-flight loop in the
   mirror/forksweep shape (own goroutine, woken by enqueue, `context`-terminated on drain — L3/L7),
   popping the FIFO head and driving the *existing* merge machinery **as the enqueuing principal**
   (their gates, their review state — no magic bypass principal; the bot-principal alternative is an
   open question, P.12). At dequeue: re-run all step-1..4 gates fresh; if the entry is `behind`,
   auto-run update-branch first (existing task body), then merge; on any refusal → entry `failed`,
   narrated on the task stream, `merge_queue` SSE frame, next entry proceeds against the new base.
   Base-moved-underneath (publish CAS 409): re-verify once then fail the entry loudly — the queue
   never force-publishes (the merge task's rule).
5. **Auto-merge triggers:** after their CAS commits, the checks report handler (`internal/checks`
   service) and the review submit handler (`internal/review`) call a consumer-owned
   `AutoMergeProbe interface { Try(ctx, owner, repo string, num int) }` (wired to `internal/pulls`
   from composition, the `ChecksGate` shape — L8): it loads `pr.json.auto_merge`, re-evaluates every
   gate, and starts the merge **as the arming principal** if all pass. Disarm on: merge/close
   (terminal, automatic), head moved (the `pulls` sink's `refreshHead` clears `enabled` with a
   timeline note), manual `PUT`, or queue enqueue (a queued PR is not auto-merged).
6. **The UI cannot bypass anything:** every write above is a server verb; the merge button, queue
   button, and arm toggle only *post* to them. Proof tests: `TestProtectionUIIsNotTheGate` family —
   with gates unmet, `POST …/merge`/`merge-queue`/`auto_merge` arm all refuse while the pages render;
   with `require_up_to_date` set, a direct `git push` of new commits to the base ref is *still*
   allowed (push half ignores it, GitHub parity) while the merge refuses.

### P.6 Permissions

Merge floor stays `maintain` (03 §5). Queue enqueue `maintain`. Auto-merge arm: `write` (author) or
`triage`+ — but the *fire* runs the merge as the armer, so the maintain floor re-applies at fire
time; a demoted armer's auto-merge fails narrated (documented). Ruleset editing: repo `admin`
(policy PUT class, unchanged). Signed/merge-method/up-to-date checks apply to every principal that
does not carry the rule's bypass (host admin included — the policy engine is principal-flag-blind
on purpose; documented, matches today's `protect`).

### P.7 Concurrency & cost model

- Queue doc: single CAS per transition (enqueue, dequeue, fail, land); the worker holds no locks
  across store or git calls (L3), takes no `syncMu/packMu/rw`, and re-reads the queue doc inside
  each CAS loop. Worker + merge task share the `(repo, "pull-merge")` single-flight? No — the queue
  worker drives merges *through* `StartMerge`, which joins the merge single-flight; the worker
  itself is `(repo, "pull-merge-queue")` so a manual merge and the worker serialize on the merge
  kind, never deadlock (documented lock-free argument in the feature doc).
- One-at-a-time queue semantics: entries land strictly FIFO; GitHub's multi-PR batch attempts are a
  documented non-goal (P.1) — the merge single-flight makes batching a re-plan, not an oversight.
- `require_signed` scan: one local git spawn per matching push — **no extra store round trips**
  (budget test pins store-request count unchanged); the push budget (≤5 requests) is untouched.
- Codeowner gate: runs inside the merge task (task path, not request path): +1 blob read +1 diff
  listing per merge — bounded, human-rate, L6-safe.
- Auto-merge probe: one `pr.json` read per checks report / review submit that targets an open PR —
  the checks handler already loads open-PR sidecars for the failure-notification lookup (05 §8
  implementation note), so the probe adds **zero** extra reads when `auto_merge` is absent (the field
  rides the sidecar it already fetched); asserted in `TestAutoMergeProbeAddsNoReadWhenDisarmed`.

### P.8 Migration / compatibility

Additive fields only; no existing data is rewritten. The ruleset rule-name mapping (P.4) is an
in-place rename on next save, not a migration. Golden fixtures gain coverage for each new field so
Rust-side round-tripping stays pinned (L5 + law 5's fixture rule).

### P.9 Tests

- `internal/policy`: parse tables for every new field (valid, invalid, defaults, duplicate/empty
  lists, combination declarations); load-time interaction unchanged (disjoint-bypass protect check
  untouched).
- `internal/codeowners`: grammar tables (anchoring, `**`, negation, last-wins, skip-bad-line,
  missing-file), owner resolution fakes.
- `internal/pulls`: gate ordering tests per P.5.2/3; queue worker tests with a fake git/store
  (dequeue, behind→update→merge, fail→next, CAS loss→re-verify, drain cancellation); auto-merge
  arm/fire/disarm tables; `-race` with `-count` stress on the worker loop.
- `internal/checks`/`internal/review`: the probe wiring + zero-extra-read assertions.
- e2e (real git): (a) protect `main` with `merge_methods:["squash"]` → a `merge` strategy POST
  409s while squash lands; (b) `require_up_to_date` → merge refused, update-branch then merge lands;
  (c) CODEOWNERS gate → owner approval required, non-owner approval insufficient, owner approval
  satisfies; (d) queue → two PRs land in FIFO order, a failing head fails only its entry;
  (e) auto-merge → red check turns green, merge lands with no human interaction; (f)
  `require_signed` → signed push accepted (test key fixture), unsigned rejected with the commit
  list.
- Budget: sim/e2e assertions per P.7 (store round trips per push unchanged; per-merge gate reads
  bounded).
- Coverage ≥95 % on `internal/policy`, `internal/codeowners`, `internal/pulls`; browser proof for
  the ruleset UI at 390 px.

### P.10 Docs

`docs/features/03_pull_requests.md` (queue + auto-merge + up-to-date + merge-methods sections and
Decisions); `docs/features/04_code_review.md` (codeowner gate + the amended non-goal line —
CODEOWNERS *routing* stays out, the *requirement* is now in); `docs/features/05_checks_statuses.md`
(auto-merge probe note); `docs/go/04_git.md` (law-2 amendment: `%G?` scan argv; update-branch reuse
note); `docs/go/14_extensibility.md` (frozen list: `meta/merge-queue.json`; effect-field amendments);
`docs/go/12_web_ui.md` + `docs/style-guideline.md` (only if the ruleset form needs a pattern the
guideline lacks — it should not; cite `.card`/`.side-nav-*`/typed-confirm instead);
`docs/PLAN-V1.md` §2.1 rows updated where they said "non-goal" for auto-merge/merge queue (they were
v1 non-goals; v2 plans them).

### P.11 Milestones

1. **P1 Rulesets UI** (no engine change; the mapping of P.4).
2. **P2 Merge methods + up-to-date** (two effect fields, merge-task call sites, UI checkboxes,
   enforcement tests).
3. **P3 CODEOWNERS gate** (new package + review-gate call site + UI checkbox).
4. **P4 Auto-merge** (`pr.json` field + probe wiring + arm/fire/disarm + UI toggle).
5. **P5 Merge queue** (worker + routes + page/PR controls + SSE).
6. **P6 Signed-commit presence** (push-half scan; smallest, last, and the one most likely to be
   deferred per P.12).

### P.12 Risks & open questions

- **Merge queue correctness under base movement** is the classic failure: every dequeue re-runs
  gates and re-verifies CAS; the failure mode is a *stalled* entry (loud narration), never a wrong
  publish — the tests in P.9(d) pin exactly that.
- **CODEOWNERS grammar fidelity** (GitHub's path matching has documented quirks): the subset in
  P.5.3 is tested against GitHub's public examples; anything ambiguous is skipped-with-warning
  (fail-open on content, the only fail-open in this feature, justified because it is repo content —
  and *presence* of the requirement still fails closed).
- **Merge as-enqueuer vs bot principal:** as-enqueuer means the enqueuer's own review state gates
  their entry (honest, no bypass magic); a `svc:merge-queue` principal (the 14 §14.5 sketch) would
  require operators to manage bypass lists. **Open question for the owner** — this plan picks
  as-enqueuer; say so to keep it.
- **Signed commits are presence-only** (`%G?`): "valid signature" needs a keyring/trust design (and
  possibly an L1 conversation about ssh-keyring formats). Presence-only is the honest enforceable
  core; the merge-commit exemption (server-made merges are unsigned) is documented — **open
  question:** is presence-only worth shipping, or defer until a server-signing-key design exists?

---

## 3. Feature T — Ownership transfer

**Packages touched:** `internal/identity` (the shipped `transfer.go` grows), `internal/store`
(registry invalidation), `internal/sizecatalog`, `internal/webui`, `internal/events` (nothing),
composition. Parity bar: GitHub transfer semantics (accept step for recipients, old-URL redirects,
numbers preserved) on top of the shipped v1 direct move.

### T.0 Inventory — what already ships (verified, §5)

`internal/identity/transfer.go` + `http_transfer.go` + the settings Danger-Zone verb
(`settings_seams.go:706,1211`): `POST /{o}/{r}/api/transfer` `{owner, repo?}` moves **every object
under `repos/<src>/<r>/`** by copy-then-delete (`PutCreate` per key, streamed, cleanup-on-409,
leftover-LIST race report), gated by repo admin on the source + `CheckCreateOwner` on the
destination; `access.json` is rewritten (drop `user:<src>`, add `user:<dst>`); invitations move with
the prefix; a corrupt `access.json` moves as bytes; org-delete's 409 names it. Everything
repo-scoped therefore already moves: refs/objects, issues/PRs (**numbering state `meta/next_num`
moves with the prefix — numbers never renumber**, GitHub parity, decided), packages
(`repos/<o>/<r>/packages/registry`), releases/assets, webhooks + cursors, checks + CI tokens,
threads runs/queue/secrets/schedule, LFS objects (`lfs/objects/<oid>` is prefix-relative), mirror +
push-mirror sidecars, policy, settings, collab-events.

**Gaps:** the move is synchronous (L7 violation for big repos); cross-referenced state **outside**
the prefix is not fixed (catalog rows, fork indexes, org-hook cursors, star/watch rows — analyzed
below); there is no accept step for recipients who are not already admins on the destination; there
are no old-path redirects; the local serving copy / `store.RepoRegistry` invalidation is not named in
the code; org destinations admit any member (the #346 creation rule), which is too loose for a
namespace *gift*; nothing writes an audit record.

### T.1 Problem & scope

Owner-initiated transfer of a repository between any owner kinds (user↔user, user↔org, org↔user,
org↔org; same-owner + new name = rename), with: destination eligibility and an **accept step** when
the initiator is not already admin on the destination; a narrated **task** for the move; consistency
fixes for every cross-reference outside the repo prefix; **redirects** from the old path for
pages/API; and full audit. Out of scope: bulk org migration ("transfer all repos of an org" — a CLI
loop over the single-repo verb, documented, not a new engine), org *rename* (slug is identity;
renaming `orgs/<org>/` is its own plan — the org-delete 409 already names the per-repo path),
transfer of a repo between *instances*, and git-smart-HTTP redirects (below).

### T.2 Data model

One new repo-scoped family, one new top-level family:

| Key | Kind | Schema |
|---|---|---|
| `repos/<o>/<r>/meta/transfer.json` | Create-then-CAS (overwritable family amendment) | `{"version":1,"state":"pending","to":{"owner":"acme","repo":"app"},"by":"jane","at":"RFC3339","expires_at":"RFC3339+48h","token":"<32-hex>"}` — terminal states are **delete-on-transition** (the invitations discipline) |
| `redirects/<owner>/<repo>.json` | **Create**-only (immutable until expiry) | `{"to":"acme/app","transferred_at":"RFC3339","expires_at":"+90d"}` — created by the transfer task, deleted by the maintainer sweep when expired (the one LIST the feature needs, on a maintenance pass, not a hot path) |

Both join the frozen overwritable/Create lists in the landing commit. Nothing else new: the task
record rides the core wal `TaskTable` (`repo-transfer` kind, the `repoimport.RegisterKind`
precedent); audit entries are M.4's `moderation/audit/<seq>.json` with `action:"transfer"` (if M has
not landed, T ships without audit and T.4 adds it — the soft edge of §0.1).

### T.3 API

| Method + path | Auth | Request → response |
|---|---|---|
| `POST …/api/transfer` | repo admin (source) + destination admission | `{owner, repo?}` → **202 + TaskRecord** when the initiator already holds admin on the destination (org owner / self / host admin — the direct path, today's semantics); **201 `{pending:{owner,repo,expires_at,accept_url}}`** when they do not (the accept path, T.5). 409 occupied destination; 404 unknown source; 422 a transfer is already pending |
| `GET …/api/transfer` | repo admin or the pending destination owner | → `{pending:…}` or `{}` |
| `DELETE …/api/transfer` | initiator or destination owner | cancel a pending transfer → `204`; 404/409 per state |
| `POST …/api/transfer/accept` | destination owner (user: that principal; org: **org owner** — the stricter floor of T.5) or host admin | → `202 + TaskRecord` (runs the same move task); 404 no pending; 409 expired/cancelled |
| `GET /{o}/{r}/…` (any repo page/API 404 path) | — | manifest-miss → one exact-key probe of `redirects/<o>/<r>.json` → **301** `Location: /<new-o>/<new-r>/…` (miss stays 404). Pages, the JSON API, and the SDK follow; **git smart-HTTP/LFS/bundle paths 404** — a moved remote is a client-side fix, and redirecting git would let a stale credential keep working against a surprise destination (documented divergence from GitHub, which redirects git; decided) |

The `POST` response-shape change (201→202/201-pending) is a pre-1.0 in-place change — no old shape
is kept (no-shims law); the settings form and SDK move in the same commit.

**Destination admission (eligibility):** the direct path keeps `CheckCreateOwner` (self, member org,
host admin — 01 §5.2) **plus** "already admin on the destination namespace": for a user
destination that means being that user (or host admin); for an org destination it means org owner.
The accept path exists precisely for everyone else. **Org destinations via the accept path require
an org OWNER's acceptance** — a deliberate divergence from member-may-create (#346): creation claims
a fresh prefix, a transfer is a *gift into the namespace* and the receiving side gets a veto.
Documented in the identity feature doc's Decisions.

### T.4 The move, made a task, and made complete

The synchronous loop in `TransferRepo` becomes task kind `repo-transfer` (single-flight
`(repo, kind)`, SSE attach, progress packets per phase — law L7), preserving today's copy-then-delete
discipline verbatim (it is correct: `PutCreate` arbitration, cleanup-on-412, leftover LIST). New
steps, in order around the existing move:

1. **Pre-flight** (cheap, request-path): gates of T.3, destination occupied-probe, pending-record
   Create (accept path).
2. **Move pass** (the existing code): LIST source prefix → stream-copy per key with the
   `access.json` rewrite → delete pass → leftover LIST (all unchanged).
3. **Cross-reference fixes (new, post-move, best-effort-but-narrated):**
   - `meta/repos.pb` catalog rows: CAS the bucket-root catalog (delete old-path row, add new-path row
     with the same size/activity fields; ≤5 CAS attempts then narrate "catalog busy — the sweep will
     heal it" and continue — the sweep's fold already reconciles rows against live repos, so a miss
     is bounded staleness, never corruption);
   - **fork child moved:** CAS-rewrite the row in the parent's `meta/forks.json` (exact key, one CAS;
     a miss writes nothing — the #457 ghost rule);
   - **fork parent moved:** enumerate children from the moved `meta/forks.json` (bounded, exact-key
     GETs) and CAS each child's `fork.json` `parent`/`root` pointers — **load-bearing**: the
     `internal/wal/forkread.go` pack fallback and the `forknet.go` GC walk follow these pointers, so
     a stale pointer breaks fork pack reads and GC liveness;
   - **org-webhook cursors:** when the source owner is an org, delete each org hook's
     `orgs/<org>/webhooks/cursors/<id>/repos/<old-repo>.json` (bounded by the org's hook list — an
     org-scope LIST, admin path); after the move the org hook pass sees the new name with no cursor
     and starts from the current head (documented: one-time re-delivery window, never a gap);
   - **stars/watches:** leave the user-scoped rows at the old path (`users/<p>/starred|watching/
     <old>…`) — they cannot be found without a users LIST (forbidden); the `/{user}/starred` page
     already skips rows whose manifest is missing (verified, G17), and the moved `social.json`
     counters carry forward verbatim. Documented, bounded residue — the same residue class as
     stars of a deleted repo, self-healing as users unstar/re-star at the new path.
4. **Redirect + cleanup:** Create `redirects/<old>.json` (only for same-instance renames/moves;
   skip if a redirect already points there — Create-412 = adopt), invalidate the local serving copy
   and the `store.RepoRegistry` entry for the old id (the delete-repo path's discipline — name the
   exact registry function in the milestone), audit entry, 201/202 completion outcome with a
   **warnings list**: `["oci namespace is now host/<new>…; re-push or re-pull by the new name",
   "threads runners must re-register against the new repo path", "update git remotes: <old> → <new>"]`.

### T.5 Pending transfers

`meta/transfer.json` Create-on-arm; the destination owner gets a notification (06 fan-out, reason
`moderation`-shaped class `transfer`) + a once-rendered accept URL (the invitations page pattern —
`webui.InvitePageURL` shape, shown once on arm, never re-displayed). Expiry (48 h) is enforced
lazily on read (the invitations precedent — a stale object answers 409 "expired or cancelled" and
the sweeper deletes it). Accept → the same task as the direct path. Cancel (either side) deletes the
record. A second arm while pending → 422. The repository is fully writable while a transfer is
pending (the move re-lists at task time; today's leftover-race report covers a mid-move push) — a
push landing after arm but before accept simply moves too, because the task re-LISTs. Documented.

### T.6 UI

- The existing Danger-Zone transfer form (`partials/settings_repo.html`) gains: pending state
  display (to/expires), cancel control, the accept link (once), and the task-progress card (the
  shared ops/SSE pattern from `partials/repo_wal_ops.html`).
- The destination owner's accept surface is an `/invitations`-style page block (the org-invitations
  tab precedent) + the raw API.
- Old-path pages 301 (T.3); the redirected page renders a one-line notice "transferred from
  `<old>`" for one visit (query param, no state).
- CLI: `spindle repo transfer <owner>/<repo> --to <owner> [--name <repo>]` (the `spindle repo`
  group exists per PLAN-V1 §2.10) with `--json` per the WP11 contract; accept/cancel verbs included.

### T.7 Concurrency & cost

- The task inherits the copy-then-delete arbitration (CAS is the lock — no new locks, L3). The
  catalog CAS is the one cross-repo object: human-rate, ≤5 attempts, self-healing residual.
- Redirect probe cost: **one exact-key GET on the manifest-miss path only** — zero cost for every
  existing repo (L6's "verification goes on the failure path"). Budget test:
  `TestRedirectProbeOnlyOnMiss`.
- The 90-day redirect TTL is swept by the maintainer loop (probe-by-expiry over the `redirects/`
  prefix — a maintenance LIST, allowed off the hot path).

### T.8 Migration / compatibility

No existing shape changes; `meta/transfer.json` and `redirects/` are new families (frozen-list
amendment). The `POST …/transfer` response shape changes in place (pre-1.0, no shim — SDK + page
move in the same commit). L5: both new families are spindle-only prefixes; no Rust-era key is
touched. Packages caveat documented: OCI image digests survive (content-addressed) but the
namespace `host/<owner>/<repo>` changes — pull paths and already-pushed tags resolve under the new
name only (warnings list carries it).

### T.9 Tests

- `internal/identity`: transfer-task tests (copy-then-delete preserved: concurrent-push leftover
  report, 409 cleanup, access rewrite, corrupt-access passthrough — all the existing
  `transfer_test.go` cases re-pointed at the task), pending lifecycle table (arm/accept/cancel/
  expire/second-arm), org-owner accept floor vs member refusal, catalog row rewrite + sweep-heal
  fallback, fork parent/child pointer rewrite (with a forkread probe proving pack fallback still
  resolves), org-cursor cleanup, redirect Create/adopt.
- `internal/api`/`internal/webui`: 301 tests for pages + API, 404 for git paths; role floors per
  T.3 on both lanes + the page forms.
- e2e: transfer a repo with issues/PRs/checks/releases/packages/threads/LFS objects, clone at the new
  path, verify LFS smudge works, old path 301s, stars page skips the dead row, a fork child still
  serves packs (the forkread fallback), CI tokens still work, threads runs render.
- Budget: redirect probe only-on-miss; the move task's store ops are 2 per key (unchanged,
  asserted).

### T.10 Docs

`docs/features/01_identity_permissions.md` (§5.4 grows: task, accept flow, org-owner floor,
redirects, cross-reference table, warnings); `docs/go/14_extensibility.md` (frozen list +
`repo-transfer` kind + Decisions); `docs/features/14_packages.md` (namespace-change note);
`docs/threads.md` (runner re-registration note); `docs/go/12_web_ui.md` (routes); `README.md`
backlog tick; `docs/PLAN-V1.md` §0 note per §0.4.

### T.11 Milestones

1. **T1 Task-ification + completeness** — the move becomes `repo-transfer`; catalog, fork
   parent/child, registry invalidation, serving-copy cleanup (all without new routes).
2. **T2 Redirects** — the `redirects/` family, miss-path probe, 301s, sweep.
3. **T3 Accept flow** — pending records, notification, accept/cancel, org-owner floor, form upgrade.
4. **T4 Org-cursor cleanup + audit + CLI + warnings** (depends on M.4 for the audit family).

### T.12 Risks & open questions

- **Cross-reference completeness is the whole feature.** The enumeration in T.4.3 was built by
  auditing every family *outside* `repos/<o>/<r>/` that can name a repo; the test list in T.9 is the
  guard. A missed family = a stale pointer (the fork-pointer class breaks GC/pack fallback). The
  forknet GC is fail-closed, so a miss degrades loudly, not silently.
- **Two owners, one truth:** a pending transfer does not lock the repo (writes continue); the
  accept-side owner could be surprised by a repo that grew since arm. Mitigation: the accept page
  shows the live summary (size, open PRs) at accept time — cheap, one summary GET.
- Open questions: (a) 48 h expiry + 90 day redirect TTL — confirm the numbers; (b) should the
  direct path be *removed* (everything goes through accept) for uniform auditability? This plan
  keeps direct-when-already-admin (GitHub parity, fewer round trips); (c) org rename is explicitly
  unplanned — confirm it stays out.

### T.13 As built (T1 Task-ification + completeness, 2026-11-21)

T1 landed as the `repo-transfer` task inside `internal/identity` (the shipped `transfer.go` grew;
no new package — the move, the access rewrite and the gates were already there). The normative
contract is [`features/01_identity_permissions.md`](features/01_identity_permissions.md) §5.4; the
storage rules are in [`go/02_storage_protobuf.md`](go/02_storage_protobuf.md) §2.1.1 and the task
table/registry additions in [`go/05_wal_engine.md`](go/05_wal_engine.md) §5.8, the page shape in
[`go/12_web_ui.md`](go/12_web_ui.md) §2.5. Against this plan:

- **Task-ified (§T.2/T.4.1):** `POST …/api/transfer` answers `202 {task_id, joined, task}`; the body
  rides the core table (kind `repo-transfer`, `(repo,kind)` keyed on the SOURCE), a second start
  joins the leader, narration is per-phase (copy/delete progress, family notices), and the terminal
  record is the only "done". Attach is findable while it runs (`GET …/api/tasks`, the WAL page's
  recent list) and streamable via `GET …/api/tasks/{id}` with SSE — **handle-free**, because the
  move quiesces the source before copying (the table gained `Subscribe`/`RunningFor`, `Broadcast.Close`;
  `Get` now returns a copy — two locks over one record was a real race the `-race` gate caught).
  The auth/role floors are the v1 ones verbatim (repo admin on the source, `CheckCreateOwner` on the
  destination).
- **Complete outside the prefix (§T.4.3, the T1 slice):** catalog row (`sizecatalog.RenameRow`,
  CAS, fields carried), fork child row and fork parent/child pointers (`pulls` implements the
  consumer-owned seam; `wal/forkread` and the forknet GC follow those pointers), the packages
  namespace (travels with the prefix; `packages.RepoMoved` drops the sweep clocks), the serving copy
  and listing cache (`(*wal.Registry).Invalidate` / `VerifyRepo`), plus the `access.json` rewrite
  from v1. The **manifest's `repo` field** is rewritten too — an unlisted gap in §T.4.3 and a real
  bug in v1: the serving-copy directory is derived from it.
- **Re-runnable (§T.5's bucket-truth rule):** identical destination keys are adopted, different ones
  are a foreign repository (409, source untouched), and the post-commit resume only deletes
  leftovers it can prove are reproduced. Ordering rule in §5.4: Create-only destination, destination
  manifest last after a source-manifest barrier, source manifest first out.
- **Verification (§T.4 in part):** manifest decodes and names the destination; refs replay at
  `LevelRefs`; the source manifest is gone; the catalog row (when one existed), every package key
  and the fork pointers are consistent — each failure names its piece and fails the task. A catalog
  CAS miss **fails** rather than warns (deviation, rationale in §5.4).
- **UI:** the Danger-Zone form starts the task and renders the shared task card (no redirect; the
  source page dies at the commit point) — `docs/go/12_web_ui.md` §2.5/§2.7.
- **Tests:** `internal/identity/transfer_task_test.go` (families, join, crash-between-families
  retry, crash-mid-move adopt, four named verification failures, pre-flight verdicts,
  residue-never-deleted), `internal/pulls/forkmove_test.go`, `sizecatalog` `TestRenameRow`,
  `packages` `TestRepoMovedDropsSweepBookkeeping`, `internal/api` `TestTaskAcrossLaneAfterMove`,
  `internal/wal` attach/close tests, the webui settings transfer card, and
  `internal/e2e/transfer_test.go` (real git + real fork + real OCI tag: the destination clones, the
  fork child still serves, the catalog row and the package prefix followed, the old prefix is empty,
  the old path answers 404, and a re-run converges).
- **T2–T4 still own:** `redirects/<owner>/<repo>.json`, the miss-path 301 probe and the expiry
  sweep; `meta/transfer.json` (pending/accept/cancel/expiry), the destination notification, the
  org-owner accept floor and the form's pending state; org-webhook cursor cleanup, the moderation
  audit record, the CLI verbs, and the user-facing warnings list (OCI namespace, runner
  re-registration, git remotes).

---

## 4. Feature F — File edit

**Package:** new `internal/webedit` (the `internal/tags` precedent: a small feature package that
owns one server-side write path). Parity bar: GitHub's web editor for a single file — create, edit,
delete — with commit message, branch choice (direct vs new branch + optional PR), and honest
conflict/binary/LFS guardrails.

### F.1 Problem & scope

A signed-in user with write access can, from the browser: **edit** an existing text file, **create**
a new file at a path, and **delete** a file — each landing as one commit authored by them and
published through the same WAL funnel as a push (so policy, events, Threads CI, checks, and the
events bridge all behave exactly as for a pushed commit). They choose: commit directly to a branch,
or to a **new branch** (created by the server) with an optional **immediate PR**.

Out of scope (documented): multi-file edits in one commit (GitHub's web editor is single-file; the
GitHub "git/commits" style multi-file JSON API is a named follow-up), file upload (binary asset
upload is the release-assets/packages surface, not the editor), rename as a first-class action
(rename = delete + create at the new path, two commits — documented divergence), directory
creation (implicit in creating a nested file), and editing LFS pointers (guard, not surgery — F.4).

### F.2 Data model

**None.** The commit is git state; the optional PR is the shipped `pulls` state; the branch is a ref.
This is the only feature in the plan with zero new bucket families and zero migrations — the plan
requires the frozen overwritable list to be amended **nowhere**. (Stated because a reviewer should
check: any bucket key appearing in this feature's code is a bug.)

### F.3 API

Repo-scoped, both lanes, discovered via `ExposedTemplates` + both-way tests (#272 rule). Errors
plain text; all writes `no-store`.

| Method + path | Auth | Request → response |
|---|---|---|
| `PUT /{o}/{r}/api/contents?ref=&path=` | write (push-equivalent, F.5) | `{message, description?, content: base64, expected_blob_sha? (required when the file exists), expected_branch_sha?, new_branch?, open_pr?}` → `201 {commit:{sha}, branch, pr?:{num}}`; `400` bad path/message/charset; `401` anonymous; `403` role/policy/block-free (push rules); `404` unknown ref; `409` branch or blob moved under you (names both shas); `413` over the editor cap; `415` binary or LFS-pointer target (F.4) |
| `POST /{o}/{r}/api/contents?ref=&path=` | write | create-new-file variant: same body, `expected_blob_sha` forbidden, `409` if the path exists (names the blob sha, no silent overwrite) |
| `DELETE /{o}/{r}/api/contents?ref=&path=` | write | `{message, expected_blob_sha (required), expected_branch_sha?, new_branch?, open_pr?}` → `200 {commit:{sha}, branch, pr?:{num}}`; `404` path absent |

Path validation: `git.ValidateRefName`-grade checks — no `..`, no leading `/`, no `.git` segment, no
NUL/control bytes, UTF-8, ≤ 512 bytes; message ≤ 64 KiB (the tag cap precedent) after trimming to
non-empty. `new_branch`: `refs/heads/<name>` grade validation, must not exist (the publish CAS
decides; 409 on race). `open_pr` requires `new_branch` (400 otherwise) and opens the PR via the seam
of F.6 with base = the edited `ref` and head = the new branch.

**The response's `pr` + a `Location` header** make the SDK/CLI one-call flows (`webedit` SDK group:
`repo.contents.put/create/remove`).

### F.4 Guardrails (server-side, never only in the UI)

- **Binary:** refuse edit/delete target when the existing blob is binary — detection: a NUL byte in
  the first 8 000 bytes (the same sniff the blob page uses to render "binary"). Creating a "file"
  whose base64 body contains NUL in the first 8 000 bytes → `415` (the editor is for text; binary
  upload is explicitly out of scope, F.1).
- **Size:** editor cap `webedit.max_bytes` (new reflective config, default 1 MiB; setup row). Load
  side: the edit page refuses blobs above the cap with a link to raw (the blob page already caps at
  2 MiB for `?raw`). Save side: `413` over the cap. The commit's pack stays O(KiB) above the cap
  … trivially inside `server.max_push_bytes`.
- **LFS:** refuse when the target blob *is* an LFS pointer (body starts with
  `version https://git-lfs.github.com/spec/v1`), and refuse create/edit at a path that the **root
  `.gitattributes`** marks `filter=lfs` (minimal hand-rolled parse: line → first token is the
  pattern, look for the `filter=lfs` attribute; glob subset shared with P.3's matcher or simpler —
  exact/dir-prefix only, documented subset; negations and macros ignored). `415` with guidance
  ("use git + LFS from a clone").
- **Conflict/precondition:** `expected_branch_sha` (the ref sha the editor rendered from) — mismatch
  → `409 branch moved under you (page: <a>, disk: <b>)` and nothing is written (the publish CAS also
  backstops this — the precondition is UX, the CAS is correctness, both tested). Editing requires
  `expected_blob_sha` matching the file's current blob; delete requires it too (409 mismatch names
  the current sha). The client is never allowed a blind overwrite.
- **No-op guard:** tree unchanged after the edit (identical content) → `422 nothing to commit`.

### F.5 The server-side write path (the funnel, exactly)

One synchronous request (the tags precedent: seconds of pool-bounded git, not a task — L7's
threshold is *long* work; a file edit is not). Steps, all argv added to `docs/go/04_git.md` in the
same change (law L2):

1. **Gate (before any git):** visibility/require_read; anonymous → 401 (the #502 rule);
   role = `write` via identity resolution **with the host `write` flag stripped and the self-namespace
   rule applied — the exact `CheckPush` contract of 01 §5.3** (this is a push, so the push rules
   govern: no `CheckRole` shortcut); then `policy` evaluation for `(principal, target ref, op)`
   — `protect` rules deny (bypass lists apply), and the `required-reviews` **push half** denies
   direct commits to review-gated refs exactly as a git push would (the honest interplay: editing
   `main` on a review-gated repo tells you to propose a branch). The evaluation is the
   `checkProtectedRef` shape (`internal/pulls/policy.go`), re-implemented against the consumer-owned
   interface, not imported from `internal/pulls` (L8).
2. **Resolve:** one refs sync (the standard conditional GET) → branch sha; compare with
   `expected_branch_sha` when present (409 above).
3. **Build the commit in the serving copy, plumbing only, no worktree, no branch movement:**
   - `git hash-object -w --stdin` (new blob; identity env per `internal/git` `TestMain` for any
     object-creating test);
   - a **request-scoped temp index** (scratch dir, cleaned on every exit path — the ingest §3.1
     discipline): `git read-tree <branch_sha>` → `git update-index --add --cacheinfo
     100644,<blob>,<path>` (create/edit) or `git update-index --force-remove <path>` (delete) →
     `git write-tree`;
   - no-op guard: `git diff-index --quiet <branch_sha>` against the new tree (exit 0 → 422);
   - `git commit-tree <tree> -p <branch_sha> -m <message>` with `GIT_AUTHOR_*` = the principal
     (the merge convention: author = principal, committer = server identity, wall clock).
4. **Pack + publish:** the `PackTip` argv (`<tip> --not --all`) → `Layer.Ingest` → one WAL entry:
   direct mode = ref update `old=<branch_sha> new=<tip>` via `RefPublisher.UpdateRefWithPack` (a
   PUSH entry — events, checks, Threads triggers all fire as if pushed, because it *is* a push);
   new-branch mode = ref create `old=zero`; optional PR via F.6.
5. **Round trips:** refs sync (1) + manifest conditional GET (1) + pack PUT + manifest CAS (2) —
   **≤ 5 store requests, inside the push budget (L6)**; asserted by an e2e budget test mirroring
   the push-budget assertions.

### F.6 The PR opener seam (L8, no sideways import)

`internal/webedit` defines (consumer-owned, the `ChecksGate` house style):

```go
type PROpener interface {
    OpenPR(ctx context.Context, owner, repo string, in OpenInput, actor auth.Principal) (num int, err error)
}
```

Composition wires `internal/pulls`' open service to it. `OpenInput` carries title/body defaults
(editable via optional request fields `pr_title`/`pr_body`, else "Edit `<path>`"/the message) — the
PR opens exactly as a human's `POST …/pulls` would, gates included (unresolvable refs → 422 passthrough).

### F.7 UI

Pages in the repo group (`internal/webui/templates/pages/repo/`), composing only guideline
vocabulary (`.card`, `.btn`/`.primary`, `.link`, `.pill`, `.input`, the composer's field stack from
`partials/collab_composer.html`):

| Route | Page | Notes |
|---|---|---|
| `/{o}/{r}/new/{branch}` | create file | path input + content textarea (mono, full width) |
| `/{o}/{r}/edit/{branch}/{path…}` | edit file | prefilled textarea; header shows blob sha + branch |
| `/{o}/{r}/delete/{branch}/{path…}` | delete | typed-name confirm (`data-danger-expected`), `.btn danger` |
| blob page (`pages/repo/blob.html`) | + "Edit" `.btn` + "Delete" `.link` | write-gated: hidden for anonymous (the `collabWriteHref`/login-interstitial pattern), disabled-with-reason on binary/LFS/oversize |
| tree page (`pages/repo/tree.html`) | + "Add file" control | links to `/new/{branch}` |

The commit form (shared partial): branch `<select>` (existing branches from `GET …/refs/branches`,
server-rendered) with an "or propose a new branch" text input that appears on toggle, commit
message + description, "Propose changes" as the one `.primary` (direct mode) — new-branch mode swaps
the CTA to "Propose changes" + an `open_pr` checkbox (default on, GitHub parity). States: loading
(server-rendered, no spinner spinners), forbidden (403 page + login interstitial for anonymous),
conflict (banner "branch moved — reload to merge the newer change" + a reload `.btn`, the exact
`expected_*` shas shown in `.mono`), binary/oversize/LFS (explanation cards per F.4). Mobile at
~390 px: single column, textarea full width, select full width, CTA pinned last — verified in
Chromium (the §2 field-lesson rule). The island needs nothing new: plain form posts + htmx-free
progressive submit (the submit-keys island already in `web/src/islands/` covers Cmd+Enter if wired —
optional, cite it).

### F.8 Permissions

| Action | anonymous | read | triage | write | maintain | admin |
|---|---|---|---|---|---|---|
| View editor pages / blob+tree | per visibility | ✓ | ✓ | ✓ | ✓ | ✓ |
| Create/edit/delete (commit) | 401 | 403 | 403 | ✓ (+ policy + push rules) | ✓ | ✓ |
| New branch + PR | 401 | 403 | 403 | ✓ (branch create + PR open floors compose) | ✓ | ✓ |

The floor is the **push** floor (`CheckPush`, 01 §5.3), not `CheckRole(write)` — a host-wide write
flag alone must not authorize an edit on someone else's repo (the #347 lesson), and the self/org
rules apply. Policy (`protect`, `required-reviews` push half) composes *after* the role floor.
Moderation blocks deliberately do **not** gate this feature (blocks govern conversation, not
git-equivalent writes — the M.5 decision, restated here so nobody re-litigates it).

### F.9 Concurrency & cost

- No locks (L3): the temp index is request-scoped scratch; git spawns run pool-bounded under the
  request context; the publish CAS arbitrates concurrent editors (two editors, one branch: the
  loser's CAS 409s with both shas — F.4's conflict UX covers it; no silent overwrite, no force).
- Cost per edit: ≤ 5 store requests (F.5.5, budget-asserted) + ≤ 7 local git spawns (resolve,
  hash-object, read-tree, update-index, write-tree, diff-index, commit-tree, pack-objects — eight,
  all pool-bounded, sub-second each). No task, no SSE of its own — the push event lands on the
  existing streams.
- No new task kinds, no new event names, no cursor families.

### F.10 Migration / compatibility

None (F.2). New argv are a law-L2 doc amendment to `docs/go/04_git.md` (read-tree /
update-index --cacheinfo / --force-remove / write-tree / hash-object -w --stdin / diff-index
--quiet), pinned in the same commit. L5 untouched (no bucket change at all).

### F.11 Tests

- `internal/webedit`: service tables (path/message/branch validation, binary/NUL sniff, LFS pointer
  + `.gitattributes` subset parse, no-op guard, preconditions, direct/new-branch/PR modes, policy
  refusal table with bypass arms, role tables with the flag-stripped push rule); httptest both lanes;
  page handler tests for the three pages + affordance gating (anonymous → interstitial href,
  read → no affordance).
- Real-git integration (the `internal/git` test layer, identity env pinned by its `TestMain`):
  create/edit/delete round trips — `git clone` after each shows the exact tree; nested-path edit
  rebuilds every ancestor tree correctly; delete of the last file in a directory removes the
  directory entry (plumbing does this naturally — assert it).
- e2e (`internal/e2e`, real store): the full funnel — edit `main` directly (push event emitted, a
  Threads workflow fires if configured); edit with a `required-reviews` rule on `main` → 403-shaped
  refusal with the propose-branch guidance; new branch + PR → merge it; two racing editors → the
  loser's 409; budget assertion of ≤ 5 store requests.
- Coverage ≥95 % on `internal/webedit`; browser proof for all F.7 states at desktop + ~390 px.

### F.12 Docs

`docs/features/17_file_edit.md` (new — the normative contract: routes, argv, guards, gates);
`docs/features/README.md` (row + P9 note); `docs/go/04_git.md` (law-2 argv amendment);
`docs/go/12_web_ui.md` (page routes); `docs/features/03_pull_requests.md` (the `PROpener` seam
paragraph — one line, it is a consumer, not a change to pulls); `README.md` backlog tick. No
dependency amendment (F needs none).

### F.13 Milestones

1. **F1 Engine** — the service + API verbs (no UI): gates, plumbing, publish, preconditions,
   real-git tests, budget test.
2. **F2 Pages** — the three pages + tree/blob affordances + all states + mobile proof.
3. **F3 PR mode + SDK/CLI** — `PROpener` wiring, `open_pr`, the `repo.contents.*` SDK group, and
   the `spindle` CLI verbs (`spindle file put/create/delete`, the WP11 `--json` contract).

### As built (2026-11-21)

F1–F3 landed together in `feat/web-file-edit` (the milestones were not split into separate
releases): `internal/webedit` (service + both-lane API + real-git plumbing), the three pages plus
blob/tree affordances, the `PROpener` wiring, the SDK group (`repo.contents.*`) and the CLI verbs
(`spindle file put|create|delete`). Normative contract: `docs/features/17_file_edit.md`.

Deviations from the prose above, all recorded in that doc:

- `diff-tree --quiet <base> <tree>` is the no-op guard (not `diff-index`), and the temporary index
  carries an empty `GIT_WORK_TREE` so `update-index --force-remove` runs against a bare repo.
- The publish runs the same on-push fan-out as a git push (push mirrors + Threads run creation),
  not only the events-bridge wake: an API edit must start configured workflows.
- A failed PR open after the branch published answers `503` naming the branch (the commit is real).
- The protected-branch refusal is a `403` naming the rule; the propose-branch field is on the same
  page, so no redirect is needed.
- The budget is the structural push budget (refs sync + manifest GET + pack PUT + manifest CAS ≤ 5
  store requests, plus one policy GET when a policy exists); the black-box e2e does not instrument
  process-internal store ops, so no separate black-box budget assertion ships.
- Multi-file editing, binary upload and rename-as-an-action are out of scope (unchanged).

### F.14 Risks & open questions

- **The plumbing set is the only technical risk**: `read-tree`/`update-index`/`write-tree` on a bare
  serving copy with a temp `GIT_INDEX_FILE` is a well-trodden path but new to this repo — the
  real-git tests of F.11 are the definition of done, and argv land in the doc first (law L2).
- **Anonymous/visibility edge:** the edit affordance must never render a working button for
  readers; the page gate mirrors the API's (the "page gate must mirror the API's gate" field lesson)
  — pinned by the affordance tests.
- Open questions: (a) default `open_pr` on for new-branch mode (this plan: on, GitHub parity —
  confirm); (b) should direct-to-protected-branch edits be refused with a *redirect* to the
  new-branch form pre-filled (this plan: yes — small UX, no state); (c) the 1 MiB editor cap number.

---

## 5. Evidence appendix — verification of every "already ships" claim

| Claim | Evidence (all at `main` @ `17158e0`) |
|---|---|
| Repo transfer ships (direct, owner-initiated) | `internal/identity/transfer.go` (move + access rewrite + race report), `internal/identity/http_transfer.go`, `docs/features/01_identity_permissions.md` §5.4, settings verb `internal/webui/settings_seams.go:706` |
| `protect` effect: restricts/bypass/`require_checks` | `internal/policy/effect_protect.go` (parse strict, `require_checks` 1–32, push-path-only evaluation) |
| `required-reviews` effect: min/stale/bypass, push half denies | `internal/policy/effect_required_reviews.go` |
| Merge-task gates (policy parse, checks, reviews) | `internal/pulls/merge.go` step 4 (`checkMergeTarget` — fail-closed parse, no `restricts`), then `checkRequiredChecksGate` (`pulls/checks.go`) and `Reviews.CheckRequiredReviews`; seams `internal/pulls/checks.go`, `internal/pulls/policy.go` |
| Protection settings tab (two managed rules, roles→bypass, force/delete blocks) | `internal/webui/settings.go:3171-3350` (`protection-push`/`protection-reviews`), view `settingsProtectionView`, round-trip test `internal/webui/settings_seams_test.go:1048+` |
| Checks: statuses, CI tokens, combined view, no-store classes | `internal/checks/{model,service,http,auth}.go`, `docs/features/05_checks_statuses.md` as-built notes |
| Server-side write funnel precedent (mktag → pack-objects → Ingest → Publish, `UpdateRefWithPack`) | `internal/tags/{tags,git,service}.go`, `docs/go/04_git.md` Decisions (§4.3 annotated-tag amendment, merge-tip pack argv), `internal/pulls/git.go:104-121` (`RefPublisher`) |
| Tree/blob read API + pages exist | `internal/api/tree.go`, `internal/api/blob.go`, `docs/go/07_api.md` §9.4/9.5, `internal/webui/templates/pages/repo/{tree,blob}.html` |
| Repo collab SSE stream + frames | `internal/notify/collab.go` (`GET …/api/collab/stream`), `internal/notify/http.go:365` |
| Notifications/webhooks activity log + fan-out seams | `internal/notify/{notify,activity,emit}.go`, `meta/collab_state.json` allocator |
| Threads state is repo-prefix-scoped (moves with transfer) | `internal/threads/keys.go` |
| Packages are repo-prefix-scoped (`repos/<o>/<r>/packages/registry`) | `internal/packages/packages.go:203` |
| LFS objects are repo-prefix-scoped (move with transfer) | `internal/server/bind_wal.go:81-82` (`lfsKey`) |
| Fork pack fallback follows child→parent pointers (transfer must rewrite) | `internal/wal/forkread.go`, GC walk `internal/maintain/forknet.go`, `docs/features/03` §7 decisions |
| Starred page skips dead rows (transfer residue is benign) | `internal/webui/public.go` G17 handler + `docs/features/07` star-record family |
| No rate limiting, blocking, hiding, reports, or audit log exist | greps over `internal/` (§1 of this plan's investigation): the only `Block`/`rate` hits are bundle/policy internals |
| `jsonl` sink not implemented (doc-truth bug §0.4) | `internal/events/sink.go` + `bridge.go` — only the webhook sink is registered |
| Merge queue / auto-merge / codeowners absent | PLAN-V1 §2.1 row ("non-goal"), README backlog rows, `internal/policy` has no such fields |

---

## 6. As built outside this plan: Threads run artifacts (2026-11-21)

Not one of the four planned features, but the largest remaining CI-parity gap and the first item on
the Threads audit's list, so it landed between plan sections. As built:

- **Actions:** `actions/upload-artifact` / `actions/download-artifact` are runner built-ins with a
  closed `with:` vocabulary (`name`, `path`, `if-no-files-found`, `retention-days`; download:
  `name`, `path`). Unknown keys are named step errors. `path` is a file, a directory, or a glob
  (`*`/`?` per segment, `**` across, `[...]` classes), matched against workspace-relative paths.
- **Transport:** the runner lists the container with `find` and moves bytes with `tar` over
  `docker exec`; uploads/downloads ride the existing authenticated job channel with the attempt token
  (a new `ScopeArtifact`, own run only). The container never reaches the bucket and nothing lands on
  the runner's disk.
- **Storage:** `threads/runs/<run>/artifacts/<name>/manifest.json` (Create-only, **the commit
  point**) + `blobs/<attempt>-<index>` (immutable; the server computes each SHA-256), and a
  `threads/artifact-expiry/…` index only for a `retention-days` that shortens the run's window.
  One artifact name per run (a second upload fails, never replaces).
- **Limits/retention:** `threads.max_artifact_bytes_per_run` (2 GiB) / `_per_file` (256 MiB), 1000
  files; expiry is `min(run expiry, created + retention-days)`, so per-artifact retention only
  shortens. `threads-gc` reclaims blobs and manifests with the run and honours the early index.
- **Access/UI/API:** repository read gate for list and ZIP download
  (`GET /{o}/{r}/threads/{run}/artifacts/{name}`, `Content-Disposition`, no cross-repo read; a fork
  run's artifacts stay in the base repo), `GET …/api/threads/{run}/artifacts`, and an **Artifacts**
  section on the run page (name link, size, count, age) — links only, one primary preserved.
- **Docs/tests:** `docs/features/15_threads.md` §2.5/§6.4/§7/§8, `docs/threads.md` §3.4/§10/§11,
  `docs/go/02_storage_protobuf.md` §2.1, `docs/go/06_server_http.md`, `docs/go/12_web_ui.md`; unit +
  webui tests and two real-container e2e scenarios (upload→needs→download with a GC restart; a fork
  PR run's artifact unreadable from the fork's lane). Still absent: `actions/cache` (its own card).
