# 16 — Moderation

> **Status:** all four milestones **shipped** — M1 (blocks) 2026-11-21, M2 (hide/minimise +
> conversation locks) 2026-11-21, M3 (reports & queues) 2026-11-22, M4 (audit completeness, rate
> limits, spam signals) 2026-11-22. This document is the normative contract for the as-built
> behavior (keys, gate interfaces, enforcement/projection points, refusal text, audit family, lock
> composition, report lifecycle, rate-limit classes, spam signals) and closes
> [`PLAN-V2.md`](../PLAN-V2.md) §1.
>
> Reads first: [`README.md`](README.md) P1/P5/P6 primitives, `docs/go/14_extensibility.md` §14.2
> (frozen object families), `docs/go/13_concurrency.md` §3 (CAS is the lock), and
> `docs/features/01_identity_permissions.md` §4 (roles) / §5.5 (the block gate summary).

## 1. Scope

A signed-in user can **block another user** (directional, self-service). The blocked principal can no
longer interact with the blocker's content in repositories owned by the blocker's **user namespace**:
comment, react, open/update/merge a pull request, review, comment on a review thread, resolve a review
thread, add/remove review requests, assign, or upload an attachment. The blocker may no longer invite
the blocked principal. A blocked principal's mentions and interactions produce **no notification**
for the blocker. Reads, stars, watches and forks are unaffected — a block is **not** access control;
visibility governs reads (`01` §4.1).

Nothing else changes: a block never mutates `access.json`, rosters, policy, or any notification.
The blocked user is **never told** a block exists (`PLAN-V2` §M.12 decision (b)).

Out of scope in §1–§11 (later sections own them): hiding/minimizing content (§10), conversation
locks (§10), abuse reports and their queues (§11), rate limits (§12), spam signals (§13), audit
completeness (§14). Out of scope entirely: instance-level suspension.

## 2. Data model

### 2.1 Block lists (identity scope)

| Key | Kind | Schema |
|---|---|---|
| `users/<principal>/blocks.json` | CAS'd, overwritable family | `{"version":1,"blocked":["carol"],"updated_at":"RFC3339"}` |

- `blocked` holds normalized principal **names** (the Forgejo #370 username, lowercased/trimmed),
  sorted, unique, capped at `moderation.BlockLimit` = 500. Exceeding the cap is `invalid` (400).
- Create-on-first-block (`PutCreate` when the object is absent), then full-document CAS with the
  standard ≤ 5-attempt loop; exhaustion is `conflict` (409) — `01` §4 discipline. There are no
  per-block objects, so no LIST is ever needed.
- A missing object reads as the empty list (synthesized `{version:1, blocked:[]}` — the
  `access.json` synthesis pattern). An unparseable document or an unknown `version` **fails closed**
  (the gate errors → consumers refuse with a 503-class failure), never as "empty".
- `moderation.Service` keeps an in-process, version-stamped parse cache: every gate read is a
  conditional GET (`IfNoneMatch` = the cached CAS version), so a cached entry avoids only the JSON
  parse. Freshness is the store's, never a TTL. Cost per gate: one control-plane GET, asserted by
  `TestBlockGateAddsNoExtraReadWhenCached`.

### 2.2 Audit family (host scope)

| Key | Kind | Schema |
|---|---|---|
| `moderation/audit_state.json` | CAS'd allocator | `{"next_seq":N}` |
| `moderation/audit/<seq:012x>.json` | **Create**-only immutable | `{"seq","at","actor","action","target","reason","context"}` |

- M1 writes `action:"block"|"unblock"` with `actor` = the blocker and `target:"user:<principal>"`.
- One record per **successful** action: a repeated block/unblock (no state change) writes nothing.
  The record is created after the block CAS commits; a failed audit write is logged
  (`moderation.Service.Logger`) and never reverts the block (honest gap, the notify activity-log
  convention). An allocator reserve whose record Create fails leaves a seq gap — allowed, never
  rewritten.
- The family is the sink later milestones and the transfer feature write into: M2 adds
  `hide|unhide|lock|unlock`, M3 `report_resolve|report_dismiss`, T.4 `transfer`.
- Reads are exact-key (`AuditEntryAt`); `AuditSince` is the cold admin viewer's cursor read and stops
  at the first gap. No hot path lists this prefix.

### 2.3 What is deliberately NOT here

No per-repo block list (a block is a person relationship), no org blocks (an org-owned repo moderates
through its own moderation doc — M2), no block state in `access.json`, no notification on
block/unblock.

## 3. The gate contract (anti-bypass)

The rule is enforced **inside the service verbs**, never in a handler or a page filter, so the JSON
lanes and the rendered forms cannot diverge. Each consumer package defines the narrow interface it
needs (the `ChecksGate` house style, `14_extensibility.md` §14.3), and composition satisfies every
one with the single `*moderation.Service`:

| Consumer | Interface | Methods |
|---|---|---|
| `internal/issues` | `ModerationGate` | `BlockedFromRepo(ctx, owner, repo, actor) (bool, string, error)` |
| `internal/pulls` | `ModerationGate` | same |
| `internal/review` | `ModerationGate` | same |
| `internal/identity` | `BlockGate` | `BlockedBy(ctx, blocker, subject) (bool, error)` |
| `internal/notify` | `BlockGate` | `BlockedBy(ctx, blocker, subject) (bool, error)` |

### 3.1 Semantics

- **Namespace**: a block applies only when the repository owner is a **user namespace** and the
  actor is the blocked principal. `IsOrg(owner)` true → the block does not apply (an org's repos are
  the org's moderation surface). The owner is never blocked out of their own namespace.
- **Direction**: A blocks B restricts B acting in A's repositories only. A acting on B's content is
  untouched.
- **Host admins** are exempt (they act as operators; the exemption lives in each consumer helper,
  which holds the `Principal` flags).
- **Nil gate** = feature off → writes allowed (absence of the feature must never break a write).
- **Gate error** fails closed: the verb returns a 503-class error, never a silent allow.
- **Refusal**: every gated verb returns its package's `ErrForbidden` wrapping
  `identity.InteractionRefused` = `"not permitted in this repository"`. The string names neither the
  block nor the blocker, so a blocked user cannot distinguish it from a permissions refusal.

### 3.2 Enumerated enforcement points

| Package | Gated verbs |
|---|---|
| `issues` | `CreateIssue`, `AddComment`, `AddReaction`, `UploadAttachment`, `patchThread` (behind `PatchIssue`/`PatchThreadMetadata` — assign/label/state) |
| `pulls` | `OpenPR`, `UpdatePR`, `AddComment`, `StartMerge`, `UpdateBranch`, `DeleteHead` |
| `review` | `SubmitReview`, `DismissReview`, `OpenThread`, `AddThreadComment`, `ResolveThread`, `UnresolveThread`, `AddRequests`, `RemoveRequests` |
| `identity` | `CreateOrgInvite`, `CreateRepoInvite` (inviter blocked the subject) |
| `notify` | the fan-out filter in `emit` (see §3.3) |

**Explicit exemptions** (the plan itself states them, and `internal/moderation/gateverbs_test.go`
pins them with a reason): `issues.RemoveReaction` (removing one's own reaction is a personal undo,
the #594 precedent — it is not even lock-gated), labels/milestones (repo configuration under the
triage/write role floors), `pulls.StartFork` and `social.Star`/`Unstar`, watches
(`notify.SetWatch`), and personal notification state (`ReadAll`, `SetState`).

### 3.3 Notification filter

`notify.Service.Blocks` is consulted once per resolved target **before** the activity append:
a notification whose recipient has blocked the acting principal is dropped (mentions, subscribed,
assigned and review-requested alike). Because filtering precedes `appendActivity`, the overflow
`notify-fanout` path cannot reintroduce a suppressed recipient. A probe error drops that one
recipient and logs (silence is the fail-closed side); the remaining recipients and the activity
record are unaffected. The filter is idempotent — it maps the already-deduped target set 1:1, so it
never duplicates a notification.

### 3.4 The drift canary

`TestEveryMutationVerbIsGated` (package `moderation`) parses the five collaboration packages with
`go/ast` and collects every exported `*Service` method that takes a principal (`auth.Principal`, or a
string parameter named `principal`/`who`). Every collected verb must appear in one closed table as
`gated`, `delegating` (to a verb that is itself gated), or `exempt` with a written reason; a gated
entry must actually call `requireNotBlocked`. A new mutation verb, a removed gate, or a stale table
row fails the test. This is the `TestExposedCoversRoutes` style applied to gate coverage.

## 4. API

Top-level twins (`/api/v1` + `/api-browser/v1`), `no-store` bodies, plain-text errors, `[]` never
null. Registered by `moderation.Handler` on the `server.ExtraRoutes` chain (Seam 1) and discovered
via `ExposedTemplates` + `api.RegisterExposed` (the #272 rule).

| Method + path | Auth | Request → response |
|---|---|---|
| `GET /api/v1/users/{principal}/blocks` | self or host admin | → `{"version":N,"blocked":[…]}`; 404 unknown principal |
| `PUT /api/v1/users/{principal}/blocks/{subject}` | self or host admin | → `200 {"version":N}`; 409 already blocked; 400 unknown/self/invalid subject |
| `DELETE /api/v1/users/{principal}/blocks/{subject}` | self or host admin | → `200 {"version":N}`; 404 not blocked |

The list is private: any other authenticated caller gets 403 ("not your block list"); anonymous gets
401.

## 5. UI

`GET /{owner}` (the profile page) owns the control — no new page family:

- **Other user, signed in**: a `.link` text action, "Block @owner" / "Unblock @owner", posting to
  `POST /{owner}/block` / `POST /{owner}/unblock`, plus one muted line stating the consequence
  (comment/review/react/open refused; mentions not notified). Hidden for anonymous viewers, for
  yourself, for orgs, and when the moderation seam is unwired (no button that cannot save).
- **Your own profile**: a `Blocked (N)` card lists the blocked principals with an `unblock` `.link`
  per row (an unauthenticated or other viewer never sees it). "Nobody blocked." when empty.
- Both POST handlers run the shared write preflight (same-origin, signed-in) and the page mirrors the
  API gate: self-block and org targets are 400; refusals are the API's status and text.

`docs/go/12_web_ui.md` lists the routes. One `.primary` per page is preserved (the block control is a
`.link`); the card and the action follow `docs/style-guideline.md` §2/§4b (accessible name, keyboard
reachability, ≥ 24 px target, text-not-colour-only).

## 6. Permissions

| Action | anonymous | read | write | maintain | repo admin | host admin |
|---|---|---|---|---|---|---|
| Read own block list | 401 | ✓ | ✓ | ✓ | ✓ | ✓ (any list) |
| Block/unblock (own list) | 401 | ✓ | ✓ | ✓ | ✓ | ✓ (any list) |
| Interact in a blocker's user repo while blocked | 401 | 403 | 403 | 403 | 403 | ✓ |
| Receive a notification from a blocked actor | — | suppressed | suppressed | suppressed | suppressed | suppressed |

## 7. Concurrency & cost

- Block/unblock: one whole-document CAS (≤ 5 attempts) + one allocator CAS + one Create. Human rate.
- Gate: one conditional GET of `users/<owner>/blocks.json` per interaction (plus `IsOrg`'s one HEAD on
  the owner kind) — the same control-plane class as the visibility read; a cache hit skips the parse
  only. No LIST anywhere.
- No new locks; no lock is held across a store call (13 §2). The only mutex guards the parse cache and
  is released before every store call.

## 8. Tests

- `internal/moderation`: block/unblock idempotency (state and audit), validation (self, unknown,
  cap), CAS retry/exhaustion, corrupt/unknown-version fail-closed, direction, org exemption, cache
  revalidation (unblock is visible immediately), one-GET cost, audit records/gap/pagination, the full
  HTTP matrix on both lanes, discovery both-way, and the drift canary.
- Consumers: one gate-matrix test per package (issues, pulls, review), the identity invite gate, the
  notify mention/filter tests (suppression, no duplicates, fail-closed probe), the page-control
  round-trip and the page-refusal-equals-API arm (webui).
- `-race` mandatory; ≥ 95 % coverage per package (`make cover`).

## 9. Milestones (closed)

- M1 Blocks, M2 Hide/lock, M3 Reports & queues and M4 Audit completeness + rate limits + spam
  signals all shipped; §12–§14 are M4's contract. The named follow-ups M4 records rather than
  silently closes: the audit-family extension for identity access/role grants, and transfer's T.4
  record (both §14.2).

## 10. Hide/minimise and conversation locks (M2)

### 10.1 Data model

| Key | Kind | Schema |
|---|---|---|
| `repos/<o>/<r>/meta/moderation.json` | CAS'd (frozen overwritable family) | `{"version":N,"locked":false,"locked_by":"","locked_at":"","lock_reason":"","locks":{"thread:<num:06x>":{…}},"hidden":{…},"updated_at":"RFC3339"}` |

- `version` is the document **revision** (the API's concurrency token): absent reads as 0, the first
  write produces 1, every committed write increments it. The store's opaque CAS version is the real
  precondition; the revision makes a stale client PUT a 409 without exposing an ETag format.
- `hidden[target]` = `{"by","at","reason"}` with `reason` ∈ `spam|abusive|off-topic|other` (default
  `other`). The record is **evidence**: the entry is created on hide and removed on unhide; its
  `by`/`at`/`reason` are never rewritten in place (change it by unhide → hide).
- Target grammar (one form per content family, lowercase hex, minimum widths growing naturally):
  `thread:<num:06x>:event:<seq:012x>` (issue/PR event bodies — `seq` 0 is the opened event, i.e. the
  description), `review:<num:06x>:<seq:012x>` (review bodies), and
  `review_thread:<num:06x>:<tid>:<seq:012x>` (review-thread comments).
- **Lock modes.** Every lock carries a `mode`: `all` (the default — the conversation accepts no new
  content from anybody, host admins included) or `collaborators` (actors at role ≥ **write** on the
  normal P6 ladder keep commenting; read and anonymous viewers are refused). The field is additive
  (`omitempty` on the keyed entry, `lock_mode` on the flat envelope): a document written before modes
  existed reads as `all`, so the original semantics are unchanged (law 5). `LockThread` takes the mode,
  refuses an unknown one (400), and treats re-locking an already-locked conversation in the *other*
  mode as a mode switch that keeps the first moderator's `by`/`at`/`reason` (evidence is never
  rewritten); `UnlockThread` removes the mode with the lock. `Apply` accepts a mode-only delta on a
  kept entry or on the flat envelope, charges it the same **maintain** floor as any other lock change,
  and refuses an invalid mode before writing.
- **Host admins under a mode.** `all` refuses them exactly like everyone else (the #594 no-override
  decision, unchanged). `collaborators` admits them, because the mode's rule is "write and up" and a
  host admin resolves to admin on the effective ladder — the mode narrows the audience, it does not
  invent a second exception list.
- **Lock composition.** `locks` holds one entry per conversation (`thread:<num:06x>`), because the
  document is repo-scoped while a lock is conversation-scoped. The plan's flat
  `locked/locked_by/locked_at/lock_reason` fields are retained as the repo-wide envelope; `ThreadLock`
  is true when **either** arm is set. The M2 UI writes only the keyed entry. A lock composes
  additively with the closed/merged conversation lock: either refuses writing, and unlocking a
  moderator lock never reopens a closed thread. Neither writer touches the other's object — this
  document is never a thread header, so the P3 header CAS keeps exactly one writer.
- Absent document ≡ `{version:0,locked:false,hidden:{},locks:{}}` (the `access.json` synthesis
  pattern). A corrupt document or an unknown future version **fails closed**: every projection
  refuses rather than serving content it cannot prove visible.
- Unhide removes entries; nothing else evicts (growth is bounded by human moderation volume, the same
  rationale `PLAN-V2` §M.2 records).
- The document joins the frozen overwritable family in `docs/go/14_extensibility.md` (14 §14.11
  rule 2) in this revision.

### 10.2 Verbs and floors

| Verb | Floor | Notes |
|---|---|---|
| `Hide(target, reason)` | triage | idempotent (already hidden → no write, no audit); 400 unknown grammar/reason/target |
| `Unhide(target)` | triage | idempotent (not hidden → no write, no audit) |
| `LockThread(num, reason, mode)` | maintain | idempotent per mode; a mode change keeps the first moderator's reason; unknown mode 400 |
| `UnlockThread(num)` | maintain | idempotent |
| `Apply(doc)` (PUT) | per changed field | hidden-map deltas triage; lock deltas (keyed, flat, or a mode-only change on a kept entry) maintain; stale revision 409; unknown/absent target 400 |

Floors live **inside** the verbs (never in a handler), so the page forms and the JSON API cannot
disagree; host admins always pass, anonymous is a real 401. `Apply` diffs the posted document against
the current one and takes the **stricter** floor per changed field; it stamps `by`/`at` itself (a
client cannot forge the moderator of record) and writes one audit record per changed family.

Target existence is probed by exact-key HEAD in the owning family (issues event, review, review-thread
event) through a composition-owned `TargetProbe`; the moderation package never learns another
feature's key layout. No LIST runs on any path.

The JSON surface is the plan's one-writer pair on both lanes:
`GET /{o}/{r}/api[-browser]/moderation` (read-gated; 200 with the zero document when absent) and
`PUT …/moderation` (full document + `version`). A present `Origin`/`Referer` must name the request
host (browser-lane CSRF); an absent one is a token client.

### 10.3 Every projection (page **and** JSON)

Redaction happens in the **read verbs**, so every consumer of those verbs inherits it and no
projection re-implements the rule. Each package defines the gate it needs (consumer-owned, like M1's)
and satisfies it with the one `*moderation.Service`:

| Surface | Page | JSON | Where redaction happens |
|---|---|---|---|
| Issue/PR conversation timeline | `partials/collab_timeline.html` | `GET …/issues/{num}`, `GET …/issues/{num}/events`, `GET …/pulls/{num}` | `issues.GetThread`, `pulls.GetPR` |
| PR description (its opened event) | `pages/collab/pull.html` | `GET …/pulls/{num}` (`description_hidden`) | `pulls.GetPR` |
| Review bodies | `pages/collab/pull.html` review list | `GET …/pulls/{num}/reviews`, `…/reviews/{seq}` | `review.ListReviews`, `review.GetReview` |
| Review-thread comments | `partials/collab_threads.html` (htmx fragment) | `GET …/pulls/{num}/threads/{id}` | `review.GetThread` |
| SSE collab stream | invalidation frames only, never a body | `/{o}/{r}/api/collab/stream` `moderation` kind | frame carries `action` + `state` (`hidden|visible|locked|unlocked`) |
| Notifications / webhooks / listings | titles and refs only — no comment body exists in these shapes | same | verified: no body is projected, so nothing to redact |

Wire shape: a hidden event/review/comment carries `hidden:{by,at,reason}` and **omits `body`** on the
wire to a non-moderator; a moderator (triage+) receives the original body alongside the marker, and
the page renders it behind "Show original". The page never receives a body it may not show — there is
no CSS hide. `ThreadView`/`PullView` also expose `locked`/`lock_mode`/`lock_reason` so the page and
JSON agree on the composer state, and each consumer enforces the mode in its own lock gate
(`internal/issues`, `internal/pulls`, `internal/review`): `all` refuses everyone, `collaborators`
admits role ≥ write. The ETag folds the document revision (`-m<rev>` on issues; `.mod<rev>` on pulls)
so a hide/unhide/lock can never 304 a pre-moderation body; the views are `private, no-cache`.

### 10.4 UI

- **Hidden card**: a muted `card border-dashed` marked `role="status"` replaces the body — "Hidden by
  a moderator · `<by>` · `<when>` · `<reason>`"; moderators get "Show original" (the body), "Unhide",
  and the `.link`/popover "⋯ → Hide comment/review/reply" control on visible rows. No `.primary` is
  added.
- **Lock**: a `.chip` "Locked" in the conversation header is visible to **everyone** (the
  owner-visible indicator), the header carries a maintain+ "Lock" popover — a **mode** select ("Who may
  still comment": *Nobody (all)* / *Collaborators (write and up)*) plus the reason, both shown to
  everyone — and an "Unlock" button while locked. A `role="status" aria-live="polite"` banner renders
  "Locked by a moderator: `<reason>`" or "Locked to collaborators: `<reason>`"; under the latter a
  write+ viewer keeps a live composer (the banner says so) while read and anonymous viewers see the
  disabled composer with that reason. Changing a mode on an already-locked conversation is unlock →
  lock from the page (the service also accepts the direct switch for API/PUT clients).
- Page actions: `POST /{o}/{r}/issues/{num}/moderation/{hide|unhide|lock|unlock}` and the
  `/pull/{num}/moderation/…` twins, same-origin, floors mirrored (triage hide, maintain lock) and
  re-enforced by the service. The page gate mirrors the API gate: an anonymous viewer sees neither
  the lock control nor the moderation affordances.

### 10.5 Audit and SSE

- `hide`/`unhide`/`lock`/`unlock` append to the M1 family (`moderation/audit/<seq:012x>.json`) with
  `actor` = the moderator, `target` = `repo:<owner>/<name>`, `reason`, and a `context` naming the
  exact content target or thread, the **mode**, and the **previous state**
  (`;mode=all|collaborators;prev=visible|hidden|unlocked|locked`; PUT deltas add `scope=repo` for the
  flat envelope and one record per changed family; a mode switch records `prev_mode=<old>`). Post-commit
  and best-effort, exactly like M1: an audit outage is logged, never a phantom revert.
- Each committed verb publishes one `moderation` frame on the one repo collaboration stream:
  `{kind:"moderation", action:"hidden|unhidden|locked|unlocked|apply", state:"hidden|visible|locked|unlocked",
  num?, seq?, thread_id?, actor, at}`. Frames are invalidation + state, never bodies.
- Hide/lock produce **no** user notification in M2 (the plan reserves `report_filed`/moderation
  classes for M3); the author whose content was hidden learns it from the rendered marker, and the
  record is the audit trail.

### 10.6 Tests

`internal/moderation` covers target grammar, hide/unhide/lock/unlock idempotency and CAS
retry/exhaustion, both lock modes (default `all`, a mode switch that keeps the evidence, an invalid
mode refused, a mode-only `Apply` delta charged the maintain floor, the mode in the audit context), the floor matrix (anonymous/read/triage/write/maintain/admin), `Apply` diff floors,
stale revision, invalid/absent targets, the audit record's actor/target/prev-state, the frame state,
the cache-clone invariant and corrupt/unknown-version fail-closed. Consumers cover the projection
matrix (body absent for a reader, present with the marker for a moderator — page **and** JSON), the
lock's per-verb refusal (comments, reactions, reviews, thread content, host admin included), and
block+lock composition. `webui` covers the rendered hidden card, the moderator expand/unhide, the
lock banner/pill/disabled composer and the page-action floors. `-race`; ≥ 95 % per package.

## 11. Reports and queues (M3)

### 11.1 Data model

| Key | Kind | Schema |
|---|---|---|
| `repos/<o>/<r>/meta/moderation/reports/<id:012x>.json` | **Create**-only immutable | `{"id","repo","kind","target","reporter","reason","note","created_at"}` |
| `repos/<o>/<r>/meta/moderation/reports/index.json` | CAS'd (P4 hot window) | `{"version","open":[…],"resolved_recent":[…],"compacted_through","next_id","updated_at"}` |
| `moderation/reports/index.json` | CAS'd (host scope) | the same window with `repo` added to every row; rows are `(repo,id)` pointers |

- **The record is evidence.** It is created once and never rewritten; `state`, `resolved_by`,
  `resolved_at`, `action` and `resolve_note` are compensating fields on the **index card**, so a
  resolution is a new index revision, not an edit to the report (the invitations discipline). A
  terminal report is never reopened; file a new report instead.
- **Index card** = `{id,kind,target,reporter,reason,note,created_at,state,resolved_by,resolved_at,
  action,resolve_note}`. `state` ∈ `open|resolved|dismissed`; `action` ∈ `hidden|locked|none` (the
  terminal action that was applied). Open cards are never evicted; the resolved tail is bounded by
  the ~256 KiB cap (`compacted_through` names the newest evicted id).
- **Ids** are per-repo, allocated by `next_id` inside the same CAS that appends the open card, and
  rendered `%012x` in the record key / `compacted_through`. The API/UI spell a report id as a decimal
  integer. A failed evidence Create is compensated by one CAS that removes the just-added card
  (ids are not reused), so a retry is never a phantom duplicate.
- **Filing order**: repo index CAS (allocate + append) → record Create → global append (best effort).
  The index is the visibility commit point; the record is the evidence copy; a failed global append
  is a logged gap the compaction task repairs (never a phantom revert).
- **Compaction** follows P4: every committed write trims the oldest resolved cards until the
  marshaled document fits `ReportIndexLimit` (256 KiB); the global window additionally starts the
  `moderation-reports-compact` task (Seam 5, `(moderation, kind)` single-flight) when it is at the cap.
  The task is the **only** LIST of the family: it walks `repos/<o>/<r>/` prefixes, reads each repo
  index and rewrites the global window (open rows are the union; terminal rows keep the recent tail).
  No request path ever LISTs.
- Absent windows read as empty (`version 0`, `next_id 1`, `[]` slices, never null); corrupt documents
  fail closed (503-class). No new lock and no in-process mutex: one CAS loop per document (≤5
  attempts → 409), and the duplicate/terminal checks run **inside** the mutator so a retry
  re-evaluates them against the freshly read document.

### 11.2 Target grammar and the duplicate rule

The filing request is structured (`{kind, target:{num,seq?,tid?,principal?}}`), and the **service**
builds and validates the canonical key — clients never post a raw hide target:

| Kind | Target key | Notes |
|---|---|---|
| `thread` | `thread:<num:06x>:event:<seq:012x>` | issue/PR comment or description (`seq` 0 = opened event) |
| `review` | `review:<num:06x>:<seq:012x>` | review body |
| `review_thread` | `review_thread:<num:06x>:<tid>:<seq:012x>` | review-thread comment |
| `user` | `user:<principal>` | report-only (no content to hide/lock) |

Reasons are the M2 enum (`spam|abusive|off-topic|other`); the note is optional (≤ 2000 bytes). The
target must exist in its owning family (the M2 `TargetProbe`; a user target goes through the profile
probe). **409** when the same reporter already has an **open** report for the same target; a resolved
or dismissed report does not block a new filing. The grammar/existence/kind failures are 400.

### 11.3 Verbs and floors

| Verb | Path | Floor |
|---|---|---|
| File | `POST …/api[-browser]/moderation/reports` | any authenticated principal with read |
| List repo queue | `GET …/api[-browser]/moderation/reports?state=&after=&n=` | repo admin |
| Resolve | `POST …/api[-browser]/moderation/reports/{id}/resolve` `{action,note?}` | repo admin |
| Dismiss | `POST …/api[-browser]/moderation/reports/{id}/dismiss` `{note?}` | repo admin |
| List global queue | `GET /api/v1/moderation/reports?state=&after=&n=` | host admin |
| Resolve (twin) | `POST /api/v1/moderation/reports/{owner}/{repo}/{id}/resolve` | host admin |
| Audit viewer | `GET /api/v1/moderation/audit?after=&n=` | host admin |

- `state` ∈ `open` (default) | `resolved` (terminal rows; `dismissed` is accepted as a synonym) | `all`.
  Repo rows page by ascending id (`after` = the last id); global rows are newest-first by
  `(created_at, repo, id)` and each row carries an opaque `cursor` (the `after` value of the next page).
- `action` ∈ `hidden|locked|none`. `hidden` calls the M2 `Hide` verb for the reported target (with
  the report's reason), `locked` calls `LockThread` for its conversation, `none` records the
  dismissal only. The effect runs **before** the index CAS — both M2 writers are idempotent, so a lost
  CAS leaves the report open for a clean retry and never marks content resolved that was not acted on.
  A user report has no content: `hidden`/`locked` are 400, `none` works.
- Every resolve path writes one audit record: `report_resolve` (hidden or locked) or
  `report_dismiss` (none), with `target:"repo:<owner>/<name>"`, the report's reason, and a context of
  `report=<id:012x>;action=<action>;target=<key>`. A `hidden` resolve also writes the M2 `hide`
  record (each changed surface has its own evidence). The dismiss endpoint is the same verb with
  `action:"none"` — one wire shape, the audit verb is the only spelling difference.
- A terminal report resolves to 409; an unknown id (or one compacted out of the window) is 404. The
  audit viewer is the durable history (the record is immutable; the window is a cache).
- The page twins call the same verbs: `POST /{owner}/{name}/settings/reports/{id}/resolve|dismiss`
  (repo admin) and `POST /admin/reports/resolve` (host admin; owner/repo/id/action are form fields
  because a literal multi-segment `/admin` path would collide with the repo wildcard patterns).

### 11.4 Reports UI

- **Report affordance**: every comment card, review body and review-thread reply carries a `.link`
  "Report" `<details>` disclosure (reason radios + optional note; full-width at ~390 px). It posts
  the structured form to `POST /{owner}/{repo}/issues|pull/{num}/report`, which calls the filing verb
  (the same one the JSON lane uses); the confirmation renders the report id as the page notice.
  Anonymous viewers get no affordance, and a replayed anonymous POST is gated to the login
  interstitial. A refusal is rendered inline with the API's status (400/409/503).
- **Repo reports tab** (`Settings → Reports`, partial `partials/settings_repo_reports.html`,
  dispatched from `pages/settings/repo.html`): admin-only (`MinRead: admin`; the nav entry is not
  rendered below admin). Open reports render in the canonical list anatomy
  (`pages/collab/issues.html`) with the reason/note meta line and a per-row resolve form
  (`hidden`/`locked`/`none` radios + note); the resolved tail renders below with the decision, the
  actor and the note. Empty states are honest ("No open reports" / "No resolved reports in the hot
  window"), a nil seam renders "not wired", and nothing page-level is a `.primary` except the
  resolve/filter idiom. The tab and both admin pages compose `docs/style-guideline.md` §2 (card /
  list / eyebrow vocabulary) and §4b (accessible names, ≥24 px targets, text-not-colour-only state
  chips); the canonical list reference is `pages/collab/issues.html`, as the plan requires.
- **Host queue** (`GET /admin/reports`): the global window with repo links, the conversation link,
  a `?state=` filter and the same resolve twin; paging follows the row cursor. **Audit viewer**
  (`GET /admin/audit`): seq-cursor pages in a standard `overflow-x-auto` table (`seq`, when, actor,
  action, target, reason, context). Both pages gate through the Administration area's one access rule
  and render the area's designed denied view otherwise.

### 11.5 Notifications and SSE

- Filing publishes one `moderation` frame with `action:"report_filed"` and the payload
  `{num?,seq?,tid?,by,at}` (no state, no body/note).
- `report_filed` notifies the repository admins (the P6 admin set: direct bindings, team members with
  admin, org owners) with reason `moderation`. The notification's title is a generic "A new moderation
  report needs review" line; the emission's arguments contain only the conversation pointer and the
  reporter, so **no reported content (note, target, body) can reach a notification**. A repo with no
  resolvable admin notifies nobody (the report still reaches the host queue). Resolving/dismissing
  emits the M2 frames for its effect (hide/lock) and no extra notification.

### 11.6 Tests

`internal/moderation` covers: record Create-only + index-first compensation, the duplicate-open 409,
the target grammar and existence probes, the full floors matrix (anonymous 401, reader 403 on list/
resolve, reporter 201, repo admin resolve, host admin twin), every resolve action (hide through the
M2 read verb, lock, dismissal) and its audit verb, the already-terminal 409, global-window projection
and cursor paging, concurrent id allocation across two services over one store, the compaction
boundaries (trim + rebuild + task start/error), notification/SSE payloads, and both HTTP lanes'
method/route matrix. `webui` covers the tab and both admin pages (render + 403 + nav absence), the
page resolve twins, and the comment/review report affordance (render, post, confirmation, duplicate
409, anonymous gate). `cmd/spindle`'s wiring guard asserts the new settings/admin seams are bound.
`-race`; ≥ 95 % per package.

**Rendered proof (M3).** The four surfaces were driven in a real browser at desktop (1440×900) and
~390×844, in dark, light and the `ocean` palette: one `h1` per page, no page-level horizontal pan
(`document.documentElement.scrollWidth === clientWidth`), the report `<details>` form full-width at
390 px, stacked resolve forms, and the audit table scrolling inside its `overflow-x-auto` wrapper.
The drive reported no application console errors or exceptions; htmx was excluded from that one
pass because the headless binary used for the check does not define `XPathEvaluator` (its real
behaviour is covered by the webui/node suites).

## 12. Rate limits (M4)

### 12.1 Config and classes

`[rate_limit]` in `spindle.toml` (reflective schema — no hand-written row; `docs/go/11_config_cli.md`):

| Key | Default | Class |
|---|---|---|
| `enabled` | `false` | the whole feature; a zero-config boot changes nothing |
| `collab_write` | `"30/min"` | issue/PR comments, review submits and dismissals, reactions, review-thread opens and comments, review requests (add/remove) |
| `issue_open` | `"10/h"` | opening an issue or a pull request |
| `report` | `"5/h"` | filing an abuse report |
| `auth_fail` | `"10/min"` | rejected credential attempts (per address) |
| `trust_proxy` | `false` | key anonymous clients on the first `X-Forwarded-For` hop |

A rate is `N/s`, `N/min` or `N/h` with `N > 0`: a token bucket of capacity `N` refilled continuously
over the window. Validation is fail-closed and names the key. **Every key is restart-required** (the
buckets are built at boot and the process does not reload config), which is what `/admin/config`'s
restart list reports.

### 12.2 Keying, state, refusals

- **Key** = `(class, principal-or-address)`. A signed-in caller keys on the lowercased principal
  name (`u:<name>`), so a shared address cannot spend another signed-in client's budget; an
  anonymous caller keys on the socket address host (`a:<host>`, the port is not part of the key).
- **`X-Forwarded-For` is ignored unless `trust_proxy` is true**, and then **only the first hop** is
  read. The trust assumption is explicit: enabling it states that a reverse proxy in front of
  spindle **overwrites** (never appends to) the header. Otherwise a client mints unlimited limiter
  keys by varying it, which is why the default is off.
- **State is instance-local memory by design** (PLAN-V2 §M.7, L4): a limiter is a control-plane
  cache like the render LRU. It is deliberately **not** bucket state — durability would throttle
  nobody after a restart and turn every rate decision into a cross-instance CAS. Consequence, stated
  plainly: with N instances behind a proxy the effective limit is up to N× the configured rate.
- **Bound**: one mutex-guarded map keyed `class\x00key`; keys idle past 10 minutes are swept (clock
  driven, at most once a minute) and a hard **10 000-key** ceiling evicts the oldest activity first
  (ties by key), so the surviving set is a function of the input sequence, never of map iteration
  order. No store call, no lock across a handler.
- **Refusal**: `429` plain text `too many requests` with `Retry-After: 60`. Neutral: no state, no
  principal existence, no repository existence.
- **`auth_fail`** is consumed where a credential is *rejected* (never for a missing credential, a
  403 authorization decision or a 503 outage). A credential-bearing request (or `/_auth/callback`,
  `/_auth/tokens`) from an address that exhausted its failure budget is refused at the middleware
  with the 429 *before* another verification runs. Session cookies are not pre-checked: a stale
  cookie resolves to anonymous, and refusing browsers over another client's failures on a shared
  address is the one lockout a limiter must not create.

### 12.3 Enforcement points

One middleware in the server's fixed chain (`docs/go/06_server_http.md` §2.2 #10), innermost, so it
sits above every handler and **no verb can skip it** (PLAN-V2 §M.6.4). Classification is by method +
path shape and covers **both lanes** by construction: `POST /{o}/{r}/issues/{n}/comments` and
`POST /{o}/{r}/api[-browser]/issues/{n}/comments` share one bucket, so a page form and its JSON twin
cannot diverge. **Git pushes and reads never classify** — the CI-storm guarantee — and neither do
moderator actions (`…/moderation/{hide,lock}`), state changes (`…/close`, `…/merge`) or the host-admin
queue.

### 12.4 Tests

`internal/server`: per-class and per-lane budgets (`TestRateLimitPerClassAndLane`), the 429 +
`Retry-After` body (`TestRateLimitRetryAfter`), anonymous address keying incl. the port,
`trust_proxy` off/on incl. first-hop-only (`TestRateLimitTrustProxy`), disabled = allow
(`TestRateLimitDisabledAllowsEverything`), 150 git requests through an enabled limiter
(`TestRateLimitNeverThrottlesGit`), the auth-fail class (`TestRateLimitAuthFail`), the key bound and
the idle sweep on an injected clock (`TestRateLimiterBoundAndSweep`), refill/peek arithmetic
(`TestRateLimiterRefillAndPeek`), the closed class table (`TestClassifyWrite`), and the budget
assertion that keying adds **zero** store operations (`TestRateLimitNoStoreReadForKeying`).
`internal/config`/`internal/setup`: spellings, keyed errors, env overlay, page render + row metadata +
restart list. `-race`; ≥ 95 % per package.

## 13. Spam signals (M4)

Advisory only. The scorer runs after a **landed** collaboration write (the same middleware: a
rejected write is never observed), keyed per principal:

- A ring of the principal's last **256** bodies, one SHA-256 each (the body itself is never retained).
- The same body **≥ 3 times inside 24 h** pre-files **exactly one** report through the M3 filing tail
  (`appendReport`): kind `user`, target `user:<principal>`, reason `spam`, reporter the reserved,
  non-authenticating name `spam-signal`, note carrying the repetition count — never the body.
- Anti-duplication has two layers: the per-body cooldown (a filed body cannot file again inside the
  window; its observations are consumed) and the M3 duplicate-open rule on `(reporter, target)`
  (one open spam report per user+repo until a moderator resolves it).
- Bodies shorter than **40 bytes** are not scored: "+1", "thanks" and "LGTM" are what a busy reviewer
  writes three times a day, and a queue full of them is worse than no scorer.
- **Anonymous callers are never scored** (no principal; a fork run never reaches this path with a
  non-anonymous collaboration principal), and there is no API: the only observable effects are the report and
  `spindle_spam_flag_total{repo}` (incremented by the server when the observer reports a filing).
- Advisory means advisory: no block, no role change, no automatic hide or lock — a machine verdict
  never removes content (PLAN-V2 §M.1).

Tests (`internal/moderation/spam_test.go`): below/at threshold, the 24 h window on a fake clock, one
report per burst, the open-report guard, anonymous no-op, the short-body floor, the 256-entry ring
and the scored-principal bound. The server side (`TestSpamObserverRunsOnLandedWrite`,
`TestCollabBodyExtraction`) pins the extraction (form `body`/`note`; JSON `body`/`body_text`/`note`/
`text`), the landed-only rule, the anonymous skip and the counter series.

## 14. Audit completeness (M4)

### 14.1 The sweep

`internal/moderation/auditcanary_test.go` is the sibling of the block-gate canary: it walks the
moderation package's AST and requires every exported `*Service` verb that mutates state to call the
audit writer (`s.audit`/`s.appendAudit`), delegate to a verb that does, or carry an explicit
allow-list row **with a reason**. A violation names the verb. A negative-control test
(`TestAuditCanaryHasTeeth`) proves the sweep rejects a synthetic gap, so a green run is evidence about
the checker and not only about today's tree.

Allow-listed moderation verbs: `FileReport` and `NoteBody` (the report record itself is the evidence;
the audit list names terminal report actions), `CompactReports` (maintainer rebuild: no decision, no
actor).

### 14.2 The administrative table

A second fixed table covers mutations outside the package. Each row is verified by finding the audit
call in the named function, or allow-listed with the reason and its owner. A third table,
`auditFamilies`, pins the five required families (config save, token mint/revoke, admin grant, repo
transfer, moderation) to the function that must carry the call, and its negative test drops each
family's anchor in turn:

| Mutation | Where | Status |
|---|---|---|
| Config save | `webui.adminConfigSubmit` → `AdminDeps.AuditWrite` → `moderation.RecordAdmin` (`config_save`, target `instance:config`, detail = restart list + backup) | **audits** |
| Block / unblock / hide / unhide / lock / unlock | `internal/moderation` | **audits** |
| Report resolve / dismiss | `internal/moderation.ResolveReport` | **audits** |
| Token mint (`/_auth/tokens`) | `internal/server` | allow-listed: a self-service credential for oneself; revocation is `session_secret` rotation, i.e. a config save, which audits |
| Token revoke | — | no verb exists (HMAC tokens; rotation is the revocation), covered by the row above |
| Identity access / role grants (`PutAccess`, `SetMember`, `RemoveMember`, `SetTeamMember`, `RemoveTeamMember`) | `internal/identity` | **audits** (`access_grant`, `org_member_grant|revoke`, `team_member_grant|revoke`; actor via `identity.WithActor`, `system` for the CLI) — landed 2026-11-22 |
| Repo transfer (`TransferRepo` / `transferRun`) | `internal/identity` | **audits** (`transfer` at `repo:<destination>`, written after verification — T.4) — landed 2026-11-22 |
| Packages GC trigger (`StartGC`) | `internal/packages` | **audits** (`packages_gc` at `instance:packages`, one record per dry run / apply / join) — landed 2026-11-22 |

The audit record is written **after** the committed change and never reverts it (the M1 rule); a
failed write is a logged gap. In the restricted boot there is no moderation service and no verified
principal — the config save there is the recovery path and records nothing (§14.2's table says so).

### 14.3 Rendered proof (M4)

`/admin/config` was driven in a real browser at desktop (1440×900) and ~390×844, dark and light,
from a build of this tree (`make web` first): the `rate_limit` card renders all six rows with their
notes and examples, `trust_proxy` in the collapsed Advanced group; exactly one `h1`; the header, nav,
main and footer landmarks and the skip link are present; `document.documentElement.scrollWidth ===
clientWidth` at both widths (no page-level pan); every rate control is tab-reachable (`tabIndex` 0,
no positive tabindex anywhere) and has a label; measured text contrast ≥ 5:1 in both themes; the
console carried no application errors. The one exception the page reports in this environment is the
vendored htmx bundle's `XPathEvaluator` probe (the same headless-binary limitation M3 recorded), not
application code; htmx's real behaviour is covered by the webui/node suites.

## Decisions

- **No notification on block/unblock, and the blocked user is never told (2026-11-21, M1).** Rationale:
  notifying the blocked principal is hostile and invites retaliation; the refusal text
  (`identity.InteractionRefused`) is deliberately indistinguishable from a permissions refusal, and
  blocking is a private person-level fact (`PLAN-V2` §M.12 open question (b), answered: never).
- **Blocks apply to user-namespace repositories only; org repos are unaffected (M1).** Matches Forgejo
  and `PLAN-V2` §M.5; an org's moderation surface is its own doc (M2). Host admins are exempt because
  they act as operators.
- **The gate lives in the verbs, and the canary is source-shaped (M1).** A handler-level or UI-level
  filter is bypassable by the other lane; a behavioural test can only prove the verbs we remembered.
  The AST canary makes an unguarded new verb a red test.
- **The block list is a whole-document CAS with no per-block objects (M1).** A block is human-rate
  (the `members.json` reasoning, `01` §3), so one CAS is correct; ≤ 500 entries bound the document.
  Rationale for the cap: keeps the conditional GET and the parse small, and a person-level relationship
  cannot plausibly exceed it.
- **Audit is Create-only and post-commit (M1).** The record is evidence, never mutable state; a failed
  audit write is a logged gap, never a phantom revert of the block (the notify activity-log precedent).
  M4 owns the completeness sweep.
- **`users/<p>/blocks.json` joins the identity family; `moderation/audit*` is a new host prefix (M1).**
  Frozen-list amendment recorded in `docs/go/14_extensibility.md`; no Rust-era key is touched (L5).
- **Locks are per-conversation entries in the repo document, and the plan's flat fields stay as the
  repo-wide envelope (M2).** Rationale: `meta/moderation.json` is repo-scoped, but the product locks
  one conversation; a flat boolean would lock every thread in the repository. Keeping the pinned
  fields lets the API express a whole-repo freeze without a second object, and `ThreadLock` is the
  disjunction. The cost is one CAS object per repo at human rate — the `meta/` precedent.
- **Redaction lives in the read verbs, and moderators receive the body (M2).** Rationale: a page-only
  or handler-only filter is bypassable by the other lane; one verb feeds both lanes, so the projection
  matrix is a test of the verb. Moderators need the evidence to moderate, and the alternative (a
  separate "reveal" endpoint) would be a second reader to keep gated. Redaction is server-side: a
  non-moderator's response omits the body entirely, so no CSS/JS hide can be defeated.
- **The PR description is its opened event, so its hide target is `thread:<num>:event:0` (M2).**
  Rationale: the description is conversation content (`pr.json` mirrors the opened event's body for
  the read model); targeting the opened event keeps one grammar and one projection rule instead of a
  second `description:` family. `seq` 0 is therefore legal in the grammar while `num` remains ≥ 1.
- **A lock refuses host admins too; hide and lock are separate floors (M2).** Rationale: matches the
  v1 no-override lock decision — an admin's comment would otherwise reopen the fight the lock exists
  to stop — while hiding is a reversible content action the triage role already performs elsewhere.
  Unlock is maintain-gated, so a triage moderator cannot undo a maintainer's lock.
- **The frames carry state, not bodies (M2).** The collab stream is an invalidation transport (08's
  contract); putting a hidden body — or a hidden body's absence — into a frame would make the stream
  a second projection to keep redacted. `action` + `state` is enough for a page to revalidate and for
  a live region to announce.
- **A failed projection read fails closed (M2).** If the moderation document cannot be read, the
  timeline refuses (503-class) rather than serving a pre-redaction body; the same rule M1 applies to a
  corrupt block list. Availability loses to the redaction guarantee.
- **A report record is Create-only; terminal state lives on the index card (M3).** Rationale: the
  record is evidence — a rewritten "resolved by" field would be a second, mutable truth about what
  happened. The index card is the queue's state and the window may compact it away; the immutable
  record plus the audit entry remain the durable history. The index-first commit makes a filing
  visible in one CAS (no orphan record), and a failed evidence Create is compensated by removing the
  card so the caller's retry cannot turn into a phantom duplicate.
- **The global window is a projection, not a second truth (M3).** It is written best-effort by every
  filing/resolution (cross-repo contention at human rate, the `meta/repos.pb` precedent) and rebuilt
  by `moderation-reports-compact` from the per-repo indexes — the only LIST in the family. A request
  path therefore never lists, and a lost global append is a logged gap the task repairs rather than a
  reason to fail a report the repo already accepted.
- **Resolve applies the M2 effect before the report CAS (M3).** Both effects (`Hide`, `LockThread`)
  are idempotent, so a lost CAS leaves the report open and the retry converges; the alternative
  (mark resolved first) could show "resolved" for content that was never hidden. The extra audit
  record for the hide/lock is deliberate: each changed surface carries its own evidence.
- **The report id is decimal on the wire and `%012x` in the key (M3).** The record family follows the
  plan's fixed-width key (`<id:012x>.json`); the API's `id` is the JSON integer a form can carry.
  `compacted_through` uses the same fixed-width spelling so an id comparison is lexical and numeric.
- **The Settings tab is its own partial plus one dispatch line (M3).** The old mega-partial is being
  split; the reports tab lands as `partials/settings_repo_reports.html` and the page's dispatch line,
  so the two changes merge without touching the file the split owns.
- **The host resolve form is body-addressed (M3).** A literal `/admin/reports/{owner}/{repo}/{id}/
  {action}` path overlaps the repo family's wildcard POST patterns and ServeMux refuses ambiguous
  patterns; `POST /admin/reports/resolve` with owner/repo/id/action fields has one shape and no shim.
- **A `report_filed` notification cannot carry reported content by construction (M3).** The
  notifier's arguments are the conversation pointer, the reporter and the time — there is no field
  for a note, target or body — and the title is a fixed generic line. The test asserts the stored
  object has no content keys, so a future field addition has to change the signature deliberately.
- **Rate limiting is middleware, not a verb check (2026-11-22, M4).** PLAN-V2 §M.6.4 puts it above the
  handlers; the class table is therefore one gate for both lanes and cannot be forgotten by a new
  verb the way a per-verb call could. The trade-off — a route the classifier does not recognize is not
  limited — is the safe direction (a missing limit degrades to today's behaviour; a wrong match would
  throttle a legitimate write class), and `TestClassifyWrite` pins both the matched and the
  must-not-match shapes.
- **The limiter lives in `internal/server`, the product contract in this document (M4).** The server
  may not import feature packages (law 8), and the limiter is HTTP-transport state with no store, no
  principal and no role: it belongs to the layer that owns the route tree and the middleware order.
  The classes, defaults and semantics are specified here so the two cannot drift silently.
- **`enabled` defaults off; every `[rate_limit]` key is restart-required (M4).** The plan's open
  question (c) answered: an upgrade must not start refusing writes. Restart is honest because the
  buckets are built at boot and the process has no config reload — a limiter that reported a saved
  rate it never applied would be a config lie, so the key appears in the restart list.
- **`auth_fail` counts rejections, and only pre-checks machine credentials (M4).** Counting is where a
  credential is actually refused, so a successful login never spends the budget and a 403/503 never
  counts as a failed login. The pre-check deliberately skips session cookies: a stale cookie is not a
  failure (it resolves to anonymous) and refusing browsers because another client failed on the same
  address is the one lockout that a limiter must not create. The cost — an attacker keeps the CPU
  cost of the requests that are still within budget — is bounded by the budget itself.
- **Spam signals ride the collab-write class instead of a per-verb call (M4).** The scorer needs the
  body, and the one place both lanes hand over the text is the middleware that already owns the
  class; a per-verb call would have been four call sites (comments, reviews, thread comments,
  reactions) in three packages, each able to drift. Extraction is deliberately narrow (form
  `body`/`note`, JSON `body`/`body_text`/`note`/`text`) and its failure mode is "not scored", never a
  parse error page.
- **The scorer's target is the principal, not the comment (M4).** The middleware observes the write
  *before* it exists, so there is no conversation sequence to name; a `user:` report is the honest
  target for "this account is repeating the same body", it needs no existence probe (the author is
  authenticated), and it keeps the pre-filing inside the M3 verb's request grammar. The reporter is a
  reserved name rather than a synthetic admin, so the duplicate rule and the queue tell the truth
  about who filed it.
- **A filed spam burst is consumed and cooled down (M4).** Without the per-body cooldown plus the
  consumed ring, a body posted six times would file twice (3 + 3). The cooldown is the window, so a
  fresh burst after 24 h may file again; while the first report is still open the M3 duplicate rule
  refuses it anyway — the two layers cover restart and second-instance races.
- **The audit sweep is source-shaped, with teeth (M4).** A behavioural test can only prove the verbs
  we remembered to call (the M1 canary's reasoning); the AST walk plus the negative-control test is
  what makes a *new* mutation verb a red build. Administrative verbs outside this package get the
  same treatment through a fixed table, and the ones M4 cannot reach (identity access grants,
  transfer) are recorded here as owned follow-ups instead of assumed.
