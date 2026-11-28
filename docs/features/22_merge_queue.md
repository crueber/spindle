# 22 — Merge queue

> Status: **shipped** (the Merge queue backlog card, 2026-11-28). Owner of the
> `merge_group` dependency that `PAR-016` (the trigger) left to it.
> Authority: this doc for the queue's storage, ordering, ref lifecycle,
> recovery, API and pages. The merge task and its gates are
> [`03_pull_requests.md`](03_pull_requests.md) §5; the trigger and run
> contract are [`docs/threads.md`](../threads.md) §6.10 and
> [`docs/WORKFLOWS.md`](../WORKFLOWS.md) §3.7; checks are
> [`05_checks_statuses.md`](05_checks_statuses.md) §6.

A maintainer enqueues an open pull request for a base branch. The queue's head
computes the merge commit it would land, publishes it as the documented
read-only ref

```
refs/heads/gh-readonly-queue/<base>/pr-<number>-<sha>
```

which starts exactly one `merge_group` run (PAR-016). When the run and the
base ref's `require_checks` union are green, the entry lands **through the one
merge task** — every protection gate re-runs, no bypass. A failure, conflict,
refusal, timeout or dequeue ejects the entry with a named reason; the pull
request stays open and the temporary ref is deleted. A restart resumes the
head (delete the stale ref, prepare again, run again) and never wedges.

## 1. Scope and non-goals

**In scope.** Per-base-branch ordered queues; priority; enqueue/dequeue by a
maintainer (and dequeue by the PR author); pause/resume; one running entry per
base; the temporary merge ref; the `merge_group` hand-off; the required-checks
gate on the checked merge commit; landing through `pull-merge`; ejection with a
reason and notification; the audit trail; restart/lease recovery; the JSON API
and the pages.

**Out of scope (named, not silently dropped).**

- **Batching.** One entry is checked and landed at a time; a batch ref
  (`pr-<a>-<b>-<sha>`) is not produced. The documented ref shape names one PR,
  and landing one PR per run keeps the failure attribution exact.
- **Speculative validation of the next entry.** The second entry starts only
  after the first lands or ejects (the contract's ordering).
- **Squash/rebase queue entries.** The temporary ref must name a merge commit
  (the tree the base receives); a base whose `merge_methods` forbids `merge`
  refuses the entry rather than testing a different shape.
- **A second merge path.** The landing call is `pulls.Service.LandQueuedMerge`,
  which runs `runMerge`. The queue owns no merge logic.

## 2. Storage

One JSON document per repository:

```
repos/<o>/<r>/merge_queue.json
```

```jsonc
{
  "version": 7,                       // CAS'd doc version (monotonic)
  "repo": "owner/name",
  "queues": [                         // one per base branch that has ever had work
    {
      "base": "main",                 // plain branch name; may contain slashes
      "paused": false,
      "paused_by": "", "paused_reason": "", "paused_at": "",
      "next_seq": 4,                  // monotonic enqueue order
      "entries": [ /* Entry, active only, stored in enqueue order */ ]
    }
  ],
  "recent": [ /* terminal Entry outcomes, newest first, bounded 50 */ ],
  "audit": [ /* AuditEntry, append order, bounded 200 */ ],
  "updated_at": "2026-11-28T12:00:00Z"
}
```

An `Entry` carries `number`, `base`, `title`, `author`, `enqueuer`, `priority`,
`seq`, `state`, the timestamps, the prepared
`ref`/`merge_sha`/`base_sha`/`head_sha`/`strategy`, the `claim_id`/`claimed_at`
lease, `attempts`, and the terminal `reason`. The recorded enqueue credential
lives in a sibling `credentials` map (`base#number`) — deliberately NOT on
`Entry`, so no API or page projection can emit it (it carries the enqueuer's
email and verified groups); the landing task re-checks the role against it.

**Effective order** is derived, never stored: priority descending, then `seq`
ascending (stable FIFO inside a priority). `position` is a read-model field
(`json:"-"`), so the stored document never carries a stale position.
Enqueuing appends; dequeuing removes; a terminal entry moves to `recent`.

Every mutation is one CAS: read the freshest document, apply the delta, write
with `IfVersion` (`PutCreate` when absent), retry on a precondition failure
(bounded at 8). Nothing is cached in process memory; a restart re-reads the
bucket. **The document is the only lock.**

## 3. Ordering rule (stated, not implied)

**Priority descending, then FIFO by enqueue sequence.** Enqueue is
maintain-only, so priority is a maintainer's ordering tool: `priority` 0…100,
default 0, higher runs first. Entries of equal priority run in the order they
were enqueued. A re-enqueue after a terminal outcome starts a fresh `seq` (it
is a new request), and replacing a terminal row in `recent` is handled by
`recent`'s one-row-per-PR rule.

## 4. The ref lifecycle

1. **Claim.** A worker CASes the head (`pending` → `validating`), writing a
   fresh `claim_id` (`<process-instance>-<nanos>-<counter>`) and `claimed_at`.
   Every later write verifies its own claim, so a stale worker can neither land
   nor eject an entry it no longer owns.
2. **Prepare.** `pulls.PrepareQueueMerge` runs the same §5 recipe as the merge
   task: re-resolve base/head, bridge fork objects, refuse draft/closed/merged,
   compute mergeability (conflicts and up-to-date are named refusals), apply
   the base's merge-method and up-to-date gates, trial-merge, `commit-tree`
   with parents `[base, head]`, and publish the documented ref **with its
   objects atomically** (the WAL pack + txn path). A leftover ref from a
   crashed attempt is deleted first; the create is deterministic.
3. **Hand-off.** The queue calls its `RefPublished` seam after the publish.
   Composition binds it to the same Threads push hook a client push uses, so
   the hook recognises the queue-ref shape and starts exactly one
   `merge_group` run (PAR-016). The queue never implements run creation.
4. **Validate.** The worker polls (default 10 s), reading only the
   `merge_group` runs created at/after the ref publish (a watermark-scoped
   list, not the repository's run history):
   - a `merge_group` run on the prepared tip that ends `failure`, `cancelled`,
     `skipped` or `timed_out` ejects immediately with the run id and state;
   - `checks.RequiredChecksProgress` on the prepared tip (the same gate
     `runMerge` applies): all green ⇒ land; a decided non-green state ⇒ eject
     naming the offenders; pending/missing ⇒ wait;
   - no run within `RunGrace` (5 min) ⇒ eject ("the base branch needs a
     workflow with `on.merge_group`");
   - `Timeout` (1 h) ⇒ eject, naming the wait.
   With no required contexts the merge_group run itself must end green — a
   repository that configured no protection still gets its CI verdict.
5. **Land.** The entry becomes `landing` under its claim; `LandQueuedMerge`
   takes the `(repo, pull-merge)` task slot (joining a human merge first),
   then runs `runMerge` with the prepared parents pinned and the required
   checks read on the **prepared merge commit's sha**. Every gate still runs:
   role, policy parse, merge method, up-to-date, required reviews, required
   checks, protected-ref CAS. A moved base or head refuses ("base/head moved
   while queued"); a policy refusal, a check shortfall or a CAS loss ejects
   with the task's own reason. There is no bypass and no second publish path.
   **The landing credential is a recorded delegation**, never user-supplied:
   the enqueue principal is captured server-side in the document's
   `credentials` map and re-resolved against `access.json` at landing, so
   revoking membership ejects the entry; a host-admin (`Admin`) flag captured
   at enqueue is bounded by the entry's lifetime and a maintainer can always
   dequeue it. The map is never part of an `Entry`, the API or a page.
6. **Clean.** The temporary ref is deleted on **every** exit path: landed,
   ejected, cancelled, superseded, and restart recovery. A failed delete keeps
   `ref` on the row and the reconcile pass retries; a row never silently loses
   its cleanup debt.

**Advertisement.** The refs live under `refs/heads/` (the documented shape) but
are not branches: the v0 advertisement and the default v2 `ls-refs` omit them,
the branch list and branch count omit them, and a commit page never names one.
An explicit `ref-prefix refs/heads/gh-readonly-queue/` still returns them, so a
deliberate `git fetch origin refs/heads/gh-readonly-queue/...` works.

## 5. Failure, ejection and notification

Every non-landed outcome is terminal for the entry, keeps the PR open, records
the named reason in the entry, the recent list and the audit trail, and emits a
notification (`merge_queue_ejected` / `merge_queue_dequeued` / `merge_queue_landed`
on the pull notification family). Reasons are the underlying service text where
one exists (conflicts, merge method, up-to-date, required checks, moved parents,
task error), so a person reads the actual gate that refused rather than "failed".

Dequeue is allowed while `pending` or `validating`; a `landing` entry is
already committed and is refused (409). Pause stops starting the next entry; an
entry already validating/landing continues to its verdict.

## 6. Concurrency and recovery

- **One head per base.** The effective order has exactly one active first
  entry; the worker claims it. One worker goroutine per repository serialises
  all of that repository's base queues (stronger than the contract requires),
  and the document CAS is the cross-process arbitration.
- **No lock across I/O.** The service holds only its in-memory worker-slot
  mutex (a map of repo → running, with a kick flag to avoid a lost wake-up);
  every store read, git subprocess and WAL publish happens without a lock.
- **Claim/lease.** A claim carries the process instance + a timestamp. A
  reconcile pass resets an active entry whose claim is older than `ClaimLease`
  (15 min) — deleting its temp ref, clearing the prepared shas and returning
  it to `pending`, so the entry is re-prepared and re-checked. Work is
  repeated, never skipped.
- **Restart.** The first maintainer pass after a process starts calls
  `ReconcileRepoForce`: it resets claims from *other* process instances
  immediately (time cannot distinguish a dead peer from a live one, and
  repeating work is the safe side of that choice), while never stealing a
  claim this same process owns. The doc CAS revokes the old worker's claim, so
  at most one worker can land. `MaxAttempts` (3) bounds replays; past it the
  entry is ejected with "recovered N times without completing".
- **Orphans.** The reconcile pass also deletes any temp ref left on a terminal
  row (or on an active row a crash disowned) and clears the row's `ref`.

## 7. API

All routes are on the repository lane, both `api` and `api-browser`, JSON
success and plain-text errors, `Cache-Control: no-store` (the document moves
with every step). Role floors: **maintain** for enqueue/pause/resume (the merge
action's floor), **read** for list/entry; dequeue also admits the pull request's
author and its enqueuer.

| Method | Path | Body / result |
|---|---|---|
| `GET` | `/{o}/{r}/api/merge-queue` | the repo view: `queues[]` (with positions), `recent[]`, `audit[]`, `paused_any` |
| `POST` | `/{o}/{r}/api/merge-queue/enqueue` | `{base, number, priority?}` → `202 {entry}` |
| `POST` | `/{o}/{r}/api/merge-queue/dequeue` | `{base, number}` → `200 {entry}` (cancelled) |
| `POST` | `/{o}/{r}/api/merge-queue/pause` | `{base?, reason?}` → `204` (empty base = every existing queue) |
| `POST` | `/{o}/{r}/api/merge-queue/resume` | `{base?}` → `204` |
| `GET` | `/{o}/{r}/api/pulls/{num}/merge-queue` | `{entry}` with `position`, or `404` |

Enqueue refuses a missing/wrong base, a closed/merged/draft PR, a duplicate
active entry (409), a priority outside 0…100, and an unwired pulls service
(503). Errors map 401/403/404/409/422/503 like every feature surface.

## 8. UI

- **PR panel** (`partials/collab_queue_panel.html`, rendered in the PR status
  card): the base, a state chip with its text label (`queued · position N`,
  `validating`, `landing`, `landed`, `ejected`, `cancelled`), the named reason,
  the checked merge commit's short sha, a `remove from queue` control for a
  maintainer/author, an `add to merge queue` control for a maintainer on an
  open PR, and a link to the base's queue page. A paused queue says so.
- **Queue page** (`GET /{owner}/{repo}/queue`, the `Queue` tab beside Pulls):
  one section per base with the paused banner and pause/resume controls; the
  ordered entries (position, PR link, state chip, author, age, reason, merge
  commit, remove control); a maintainer enqueue form (number + priority); the
  recent outcomes and the audit tail (actor, action, base, PR, detail).
- **Accessibility and style.** The page uses the shared `.card`/`.card-header`/
  `.chip`/`.btn`/`.warn-line` classes and semantic tokens only. Every state is
  text in its chip (never colour-only); the tab links, form controls and
  buttons are keyboard reachable in document order; the PR panel is a labelled
  section inside the status card; no page-level horizontal pan at 390 px (the
  audit rows wrap, the section lists are vertical cards). Verified rendered at
  1280 and 390 px.

## 9. Tests

- `internal/mergequeue`: ordering/position, enqueue/dequeue/pause/resume
  authorization and refusals, both-entries-in-order, priority, failing checks,
  run failure/cancel/timeout, no-run grace, prepare/land failures, merged
  outside the queue, closed/draft while queued, dequeue cleanup, restart
  recovery + attempt limit + orphan refs, claims/claim loss, worker kick.
- `internal/pulls`: `QueueCandidate`, `PrepareQueueMerge` (documented ref +
  pack + refusals), `LandQueuedMerge` (parent pinning, the queue-head gate),
  `DeleteQueueRef`.
- `internal/git`: queue refs are omitted from the clone advertisement and the
  default `ls-refs`, and visible to an explicit prefix.
- `internal/checks`: `RequiredChecksProgress` pending/decided/green split.
- `internal/threads`: `MergeGroupRuns`/`MergeGroupRunsAfter` read exactly one
  commit's merge-group runs, newest first, and the watermark bounds the list.
- `internal/webui`: queue page rendering (states, reasons, audit, pause/enqueue
  controls, anonymous affordances), the PR panel, POST redirects and errors,
  the Queue tab.
- `internal/e2e`: two PRs enqueue → the first runs `merge_group` → lands → the
  second advances; a failing check ejects with the reason; restart resumes;
  dequeue cancels and deletes the ref.

## Decisions & deviations

- **D1 — Priority + FIFO, not FIFO-only.** The card asks for priority; the
  contract permits a documented rule. Higher priority first, stable FIFO within
  a priority. Enqueue is maintain-only, so the knob is not contributor-reachable.
- **D2 — The temporary ref is a merge commit, strategy `merge` only.** The ref
  must name the tree the base receives; a squash/rebase tip is a different
  shape. A base whose `merge_methods` forbids `merge` refuses the entry.
- **D3 — Landing pins both parents and gates checks on the prepared tip.**
  `MergeInput` gained three internal fields (`ExpectedBaseSHA`,
  `ExpectedHeadSHA`, `QueueHeadSHA`); HTTP callers never set them. The merge
  task refuses a moved parent and reads `require_checks` on the queue head
  (the checked merge commit) instead of the PR head. Both are the same
  base+head tree, so protection is enforced, not bypassed; the merge method,
  up-to-date, required reviews, policy parse and CAS gates are unchanged.
- **D4 — One document per repository, not per base.** One CAS key keeps
  recovery to one GET and avoids a base index; contention is human-rate. A
  queue is still per base (`queues[]`), and pause/resume default to all
  existing bases (a newly used base starts unpaused).
- **D5 — Dequeue admits the PR author.** The contract says "by
  author/maintainer"; the entry records both the PR author and the enqueuer,
  and either may cancel while the entry is not landing.
- **D6 — Restart prefers repeated work.** A boot pass forcibly recovers claims
  from other process instances (never its own); at most one landing wins by the
  doc CAS and the merge task's write-once outcome.
- **D7 — Queue refs are hidden from clone advertisements.** They remain
  fetchable by an explicit ref prefix. This keeps `git clone`'s branch list
  truthful without breaking a job that deliberately reads the queue ref.
- **D8 — No batch entries.** The documented ref shape names one PR; batching
  is a future extension, not a silent omission.
