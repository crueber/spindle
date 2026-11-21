# 15 — Threads: CI runs

> **Status:** specified for v1 (2026-11-20). **Passes landed:** T1 (run/job records + checks
> mirror), A (2026-11-21: PR/merge triggers, versioned `Job.Spec`, parser breadth), B1
> (2026-11-21: runner protocol, queue + CAS claims, attempt tokens, log frames + SSE attach,
> secrets, cancel/rerun, runner/secrets settings UI, the `spindle runner` mint verb), **B2
> (2026-11-21: the executor + the runner binary — `spindle runner serve`, one container per job,
> services, in-container checkout, `docker exec` steps, matrix expansion, concurrency groups,
> fail-fast, fork safety, green/blocked checks end to end; §4.1)** and **round 2 pass 1
> (2026-11-21: prompt PR head-push delivery + forward-only head ordering, `needs:` gating,
> `workflow_dispatch.inputs` + the Run workflow control, `in_progress`/`cancelled` check states and
> run/job durations; §2.1, §4.1, §6.1, §8)** and **round 2 pass 2 (2026-11-21: `schedule:` cron
> evaluation on a persisted watermark, retention/GC, `permissions:` → a scoped job token enforced on
> the git path, the first-time-contributor approval gate, run-list filters, runner attribution and
> the commit checks panel; §2.3, §5, §6.3, §8)** and **round 3 (2026-11-21: per-repository
> retention cadence, job-token push suppression (`GITHUB_TOKEN` rule), a `skipped` job/check state
> for `needs:`-blocked dependents, and a minute-aligned cron gate; §2.4, §8)**. Replaces the DRAFT
> `12_runner.md` as the v1 feature spec (that document stays as the earlier design record).
> **Provenance: clean room.** This design was derived from public documentation (Forgejo/Gitea
> Actions user and admin docs, the workflow-syntax docs, OCI/Docker documentation) and from
> black-box observation of a published runner's behaviour. No implementation code was read or
> copied from Forgejo, Gitea, act_runner or `nektos/act`; every mechanism below is our own
> specification of an observed protocol shape, not a port.

Threads are spindle's CI: a workflow file in the repository, jobs executed in containers, results
reported into the checks surface that already gates merges (`05_checks_statuses.md`). The execution
model is the well-trodden Docker-in-Docker one: a runner process that talks to a Docker daemon and
launches a container per job — the same shape as Forgejo's runner, so operators can reason about it
with knowledge they already have.

## 1. Scope

- **Workflow files:** `.spindle/workflows/*.yml` at the commit being built, parsed by
  `internal/threads/workflow` (`gopkg.in/yaml.v3`, the one package allowed to import it — AGENTS law 1,
  amended 2026-11-20). Unknown keys are rejected, not ignored, and rejection is **per file**: the
  error belongs to that workflow only and is shown in the UI/API (§2.1). No cross-forge file
  fallback (`.github/`, `.forgejo/` are not read) — one documented location.
- **Accepted workflow syntax (v1):** `on` as a string, a list, or a mapping with
  `push.branches/tags`, `pull_request.branches/types`, `workflow_dispatch.inputs` (typed
  string/choice/boolean with required/default/options) and `schedule.cron`; `concurrency`
  (group + cancel-in-progress); `permissions` (mapping or read-all/write-all); job `runs-on`
  (scalar or list), `container` (image or mapping), `services`, `env`, `if`, `needs`,
  `timeout-minutes`, `continue-on-error`, `strategy.matrix` (a simple product) with `fail-fast`, and
  a job-level `uses`/`with` (reusable workflow). Anything else is a parse error for that file.
- **Triggers v1:** `push`, `pull_request` (opened/synchronize/reopened), `workflow_dispatch`
  (manual, from the UI/API), `schedule` (cron, evaluated by the maintainer role).
- **Jobs:** `runs-on` labels matched against registered runners; `container:` image override;
  `services:` (sidecar containers); `steps:` with `run:` and `uses: actions/checkout` (our own
  checkout action, see §4); `env`, `if`, `needs`, `timeout-minutes`, `continue-on-error`.
- **Not v1:** a marketplace of third-party actions, matrix expansion beyond a simple
  `strategy.matrix` product, caching actions, artifacts UI beyond download links, OIDC cloud
  credentials, reusable workflows, environment protection rules.

## 2. Server side (bucket-native)

Everything is objects; no queue server.

```
repos/<o>/<r>/threads/runs/<run_id>.json          run header (commit, event, workflow blob ref, state)
repos/<o>/<r>/threads/runs/<run_id>/jobs/<jid>.json  job record (labels, attempt, state, timing, steps)
repos/<o>/<r>/threads/runs/<run_id>/jobs/<jid>/log/<seq>  log chunks (append-only, 256 KiB frames)
repos/<o>/<r>/threads/queue/<enqueued>.json       pending job (labels, attempt, deadline) — CAS claim
users/<principal>/tokens/threads/<id>.json        runner registration (hash only, revocable)
```

- A run is created by the server when a matching event lands (push/PR/schedule/dispatch), reading the
  workflow file **from the repository at the selected commit** — never from the runner.
- **The server selects the workflow revision per trust class.** For a same-repository `push` and a
  same-repository `pull_request`, the head commit's file runs. For a **fork** `pull_request`, the
  *base branch's* file runs, with no secrets and a read-only token (GitHub's `pull_request` vs
  `pull_request_target` distinction, minus the unsafe variant). This decision is server-side policy;
  a workflow cannot opt into the other class.
- A job is claimed by a runner with a CAS on its queue object; the claim returns the job payload
  (workflow job, container spec, env, secrets for trusted runs, job token) — the runner never lists.
- Job state transitions write the same check-run records the checks gate already consumes, so a
  required-check rule works with Threads jobs unchanged.
- Secrets are decrypted only for the claiming attempt and only for trusted runs; they are never
  written into the run/job records or logs.

### 2.1 As implemented (T1, 2026-11-20)

Server-side only: there is no runner protocol, container execution, secret, or log streaming in
this milestone. `queue/`, `log/` and the runner token family are **not written**; the record
shapes below leave room for them without a migration.

**Selection and run creation.** `internal/threads` reads the workflow files with stock git
(`git ls-tree -r`, `git show <sha>:<path>` — 04_git.md §14), parses them with
`internal/threads/workflow`, and matches the event with `Workflow.Fires(event, branch)`. One run is
created per matching file; a repository with no files, or none that fire, is a **silent no-op**
(no empty run record). Parsing is **per file**: a file that does not parse is recorded with its own
error and creates no run, while every other file still runs and still creates its runs (the
all-or-nothing failure mode measured in the 2026-11-21 audit is gone). The per-file error is
surfaced by the Threads page and `GET …/api/threads/workflows`.

**Triggers (as wired).** `push`, `pull_request`, `workflow_dispatch` and (round 2, pass 2)
`schedule` create runs:

| event | workflow revision | `trusted` |
|---|---|---|
| `push` (client push) | the pushed commit | `true` |
| `push` (PR merge, server-side publish) | the merge commit | `true` |
| `pull_request` opened/synchronize/reopened, same repository | the head commit | `true` |
| `pull_request` opened/synchronize/reopened, fork | the **base branch tip** (resolved `refs/heads/<base>`) | `false` |
| `workflow_dispatch` | the resolved ref's commit | `true` |
| `schedule` (maintainer cron) | the default branch tip | `true` |

`pull_request` never fires on close. The three edges are emitted by `internal/pulls` next to its
existing notify/SSE lifecycle seams (`PullEvents`); the merge commit rides `BranchPublished`, the
hook the merge task calls after its base-ref publish — server-side publishes never enter the client
push pipeline, so without it a merge built nothing. Composition binds both to the same
`CreateRun` path the client `onPush` hook uses (`threadsRun`), never a second implementation.

**Delivery is prompt, and head moves are forward-only (round 2, 2026-11-21).** A landed client push
now wakes the events bridge inline (the same non-blocking `Wake` the `POST /_events/notify` route
uses), in the one `pushPipeline` both transports share. Without it the pulls sink only saw a head
push on the next sweep (`events.sweep_interval`, default **5 minutes**), so a PR's `synchronize` run
could lag by minutes; measured end to end with a one-hour sweep configured, the run appears in
**53 ms**. The WAL event carries its own ordering (`_walgit.seq` + the event's pre-image `old`): the
pulls sink stores the last applied sequence on the PR (`head_seq`) and
- ignores a delivery at or below that sequence (a replay or an older event), and
- applies a non-fast-forward move only when the event's pre-image **is** the head we hold (a real
  force-push) — never a rewind from a stale delivery, and never from a live ref read (which carries
  no ordering and stays forward-only by ancestry).

The `pull_request` `synchronize` edge fires only when the head actually moved, so a duplicate
delivery creates no second run for a commit the PR has left. `internal/e2e/threads_triggers_test.go`
pins the latency, the forward-only head and the no-duplicate-run property.

A bare `push:` / `pull_request:` key (no value) is a real trigger matching every branch; the
parser records key presence (`PushSet`/`PullRequestSet`) because a null YAML value decodes to a
nil pointer. `schedule` never creates a run here (the maintainer evaluates cron in T4).

**Run record** — `repos/<o>/<r>/threads/runs/<run_id>.json`, JSON, Create-only when created
(state transitions CAS-update it), RFC 3339 UTC:

```json
{
  "id": "1732104000000000001-a1b2c3",
  "repo": "owner/name",
  "commit": "cb38da1…",
  "event": "push",
  "branch": "main",
  "ref": "refs/heads/main",
  "workflow": "CI",
  "workflow_path": ".spindle/workflows/ci.yml",
  "workflow_commit": "cb38da1…",
  "workflow_digest": "<sha256 of the file bytes>",
  "trusted": true,
  "state": "queued",
  "actor": "push",
  "attempt": 1,
  "created_at": "2026-11-20T12:00:00Z",
  "updated_at": "2026-11-20T12:00:00Z",
  "started_at": null,
  "completed_at": null,
  "version": 1
}
```

`run_id` is `%019d-<6 hex>` (UTC unix-nanos, a dash, a random suffix): lexical order is creation
order, which is what the cursor-paged list relies on. `workflow_commit` is the revision the file
was read at — it differs from `commit` on a fork pull request, which is the audit point for the
trust decision. `workflow_digest` pins the exact file content.

**Job record** — `repos/<o>/<r>/threads/runs/<run_id>/jobs/<job_id>.json`, Create-then-CAS. The
`spec` object is the **authoritative execution payload** (schema version 1): the runner starts the
job from it and never reads the workflow file. `runs_on`/`container`/`steps` beside it are a
projection kept for the run page and the checks mirror:

```json
{
  "id": "j-build",
  "run_id": "1732104000000000001-a1b2c3",
  "repo": "owner/name",
  "key": "build",
  "name": "build",
  "context": "threads/ci/build",
  "runs_on": "ubuntu-latest",
  "container": "golang:1.27",
  "needs": ["lint"],
  "steps": [{"name": "test", "run": "go test ./..."}, {"uses": "actions/checkout"}],
  "spec": {
    "version": 1,
    "workflow": "CI",
    "runs_on": ["ubuntu-latest"],
    "container": {"image": "golang:1.27", "env": {"CGO_ENABLED": "0"}},
    "services": {"redis": {"image": "redis:7", "ports": ["6379:6379"]}},
    "env": {"GOFLAGS": "-mod=readonly"},
    "if": "github.ref == 'refs/heads/main'",
    "timeout_minutes": 15,
    "continue_on_error": false,
    "permissions": {"contents": "read"},
    "strategy": {"matrix": {"go": ["1.26", "1.27"]}, "fail_fast": true},
    "concurrency": {"group": "ci-main", "cancel_in_progress": true},
    "steps": [{"name": "test", "run": "go test ./...", "shell": "bash", "working_directory": "."}]
  },
  "inputs": {"env": "staging"},
  "attempt": 1,
  "trusted": true,
  "state": "queued",
  "created_at": "…", "updated_at": "…", "started_at": null, "completed_at": null,
  "version": 1
}
```

`inputs` is the resolved `workflow_dispatch` map (round 2; absent for push/PR runs). The run header
also records the same map, so an operator can audit what a manual run was told to do.

`spec` carries what the workflow file said: the label set (list `runs-on` preserved), the mapped
container, every service, workflow+job env merged (job wins), the job `if`/timeout/continue-on-error,
the merged `permissions`, `strategy.matrix`/`fail-fast`, `concurrency`, a job-level `uses` +
`with` (reusable workflow — accepted and carried, execution is pass B/C), and every step field
(`name/run/uses/with/env/if/shell/working-directory`). `permissions`, matrix expansion/fail-fast and
concurrency groups are **carried, not enforced** in pass A — there is no job token or scheduler yet;
the version bump (never a silent field change) is how a runner refuses an unknown shape.
`job_id` is `j-<slug>` (a collision gets `-2`), so it is a stable, URL-safe key. `Name` is the YAML
`name`, falling back to the map key. The checks mirror reads the workflow label from
`spec.workflow`, so the record alone determines the context.

**State vocabulary** (runs and jobs): `queued | running | success | failure | cancelled`. T1
creates everything `queued`; `UpdateJob` (the T2 runner's write surface, exercised by T1 tests)
CAS-updates one job, mirrors the checks record, and folds the run header (`foldRunState`: any live
job keeps the run running; all terminal → failure if any failed/cancelled, else success).

**Checks mirror.** Every job owns one record in the existing statuses family
(`checks/<sha>/<context>.json`, 05 §2) keyed by its **check context**:

```
threads/<workflow>/<job>          e.g. threads/ci/build
```

The checks grammar admits no spaces, so the prose `threads / <workflow> / <job>` is spelled with
slashes; both segments are lowercased and slugged to `[A-Za-z0-9._-]` (invalid runs → `-`, each
capped at 40 chars). Job states map to the statuses enum: `queued` → `pending`, `running` →
**`in_progress`**, `success` → `success`, `failure` → `failure`, `cancelled` → **`cancelled`**
(round 2, 2026-11-21: 05's enum gained both values. Only `success` is green, so a cancelled *or*
in-flight required check still never unblocks a merge — the gate code is unchanged). The write goes
through `checks.Service.RecordStatus`, the trusted in-process reporter (05 §3 note): same
validation/storage/index/fan-out as an external report, no credential check, and **not reachable
over HTTP**. A Threads job therefore gates a merge exactly like an external CI report, with no gate
code change. A mirror failure at creation (e.g. the commit is not yet resolvable in the serving copy)
surfaces as an error after the run header exists; the job's next state transition re-mirrors, and the
run page still shows the job (the header is the run's truth). Fork PR heads are bridged by the
existing pulls fork→base fetch before this path runs (T3/T4 wiring, not T1).

**Duration.** The run and job wire records (API and page) carry a derived `duration_seconds`:
`started_at → completed_at`, or `started_at → now` while live. It is computed at read and never
stored, so no record shape or CAS write changed.

**Run state.** The fold treats the terminals distinctly: any failed job ⇒ `failure`; otherwise any
cancelled job (an operator cancel or a fail-fast sibling) ⇒ `cancelled`; skipped jobs are neutral (the
failure that skipped them is already the run's failure; round 3); all green ⇒ `success`. A cancelled
run therefore reads "cancelled" everywhere (run page, list, API) instead of being called a failure,
and a `needs:`-blocked dependent reads `skipped` (job, check and run page) instead of cancelled.

**Writes and concurrency.** Job records are written before the run header; the header is the run's
commit point, so an interrupted creation leaves invisible orphan jobs (swept by `threads-gc`)
rather than a half-visible run. Run ids are minted per creation and every create is `PutCreate`;
job/fold transitions are the canonical CAS retry loop (13_concurrency.md §3). A run get is one
exact-key GET plus one bounded prefix LIST over that run's jobs; the run list is one prefix LIST
behind a page request (cursor = last run id, `n` default 50, max 200) — no repo-wide LIST on any
hot path.

**Task kind.** `threads-gc` is registered by composition (Seam 5). T1's body was a documented
no-op; since round 2, pass 2 it applies the retention window and repairs the queue (§2.3), as two
sweepers under the one kind (queue every 5 min, retention hourly per repository — round 3), and
`threads-schedule` evaluates cron.


### 2.2 As implemented (pass B1, 2026-11-21) — runner protocol, queue, logs, secrets

```
repos/<o>/<r>/threads/queue/<seq>.json              queue entry (Create, CAS-claimed) — 15 §2
repos/<o>/<r>/threads/queue/head.json               dispatch head (CAS'd pending window)
repos/<o>/<r>/threads/runners/<id>.json             runner registry (labels, state, last seen)
repos/<o>/<r>/threads/secrets/<name>.json           sealed secret envelope (never a value)
repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/log/<attempt>-<seq>   immutable log frames
users/<principal>/tokens/threads/<id>.json          runner credentials (hash only, revocable)
```

**Queue and claim.** A job that a runner can execute has one entry object per *enqueue*
(`queue/<seq>.json`, seq = the run-id layout: unix nanos + 6 hex, so lexical order is enqueue
order). The entry carries the label set **and the dependency ids a `needs:` job waits on** (round 2),
so eligibility costs one bounded GET per dependency and never a LIST. `queue/head.json` is a CAS'd
**dispatch head** carrying the bounded pending window (512 seqs): fetching probes ONE object and
walks the window, so the claim path never LISTs. A claim is a
CAS `PutUpdate` on the *entry* (version from the probe): exactly one runner wins, the loser
re-probes. The claim records `runner`, `attempt`, `claimed_at` and `lease_until`; a claimed entry
stays in the window until it finishes — that is how a lapsed lease is noticed. Deal: the entry is
the truth, the head is a projection; a stale seq fails its CAS and is trimmed, and `threads-gc`
requeues lapsed claims by hand. A requeue is a **new attempt**: the job record's `attempt` advances
first (identity authority), then the entry returns to `queued` at the same attempt number, so the
old attempt's token is rejected by identity on the next write.

**Lease and liveness.** `lease = max(job timeout + 5m, 15m)`, capped at 30m. Log frames renew it
(coalesced: one CAS per minute at most). `threads-gc` and a probe that meets a lapsed entry requeue
it; after `MaxQueueAttempts` (3) the job fails instead of cycling. Fetching is the heartbeat for an
idle runner (the long-poll), and `last_seen_at` refreshes at most once a minute.

**Runner registry.** Runners are **repository-scoped**: an admin mints a registration token in
Settings → Runners; the runner exchanges it once (15-minute TTL) for a long-lived runner token. The
registry record (`runners/<id>.json`) carries name, labels, state, declared version and last seen;
the credential record (`users/<principal>/tokens/threads/<id>.json`) carries only hashes and stays
revocable (a revoked runner is a real 401 on its next call). Every protocol response carries
`X-Spindle-Runner: 1`. Labels are `runner ⊇ job` over `spec.runs_on` (lowercased, sorted, deduped);
a job with no labels matches any runner (the parser requires `runs-on`, so this only guards old
records).

**Attempt tokens.** `start` returns a short-lived `wjt_` token: an HMAC-SHA256 over a base64url
JSON payload `{repo, run, job, attempt, runner, scopes[], exp}`. The signature is verified locally
(no store read) and every write additionally compares the token's attempt with the job record's
current attempt — a superseded attempt is a 409 by identity, never by timestamp. Scopes are
`log`, `complete`, `cancel`, `read`; a start call requires the runner token (an attempt token there
is a clear 401).

**Logs.** `log` stores one frame per `(attempt, seq)` at `…/log/<attempt:03d>-<seq:019d>` (Create;
412 = already stored, so a retry is idempotent). Frames are capped at 256 KiB, masked before the
write (see §5.3), and read back by a prefix LIST (a human path). The viewer attaches over SSE:
`GET …/api/threads/{run}/jobs/{job}/log` with `Accept: text/event-stream` replays stored frames as
`event: log` and tails new ones every second, ending with `event: done` when the job is terminal (a
`?accept=json` or plain request answers one bounded JSON window instead). The run page renders one
panel per job and the `threadlog.js` island consumes that stream (the same fetch-stream transport as
every other island; no new transport, no EventSource).

**Cancel and rerun.** Cancel marks every job: an unclaimed job goes `cancelled` immediately, a
running one carries `cancel_requested` until its attempt acknowledges (`cancel`, or the flag on a
log/complete answer — the busy runner's control channel). Rerun refuses while a job is running,
then resets every job to `queued` at `attempt+1` (clearing timings, cancel flag and queue seq),
bumps the run header and re-enqueues. Both are repo-write operations; the page buttons and the
`POST …/cancel` / `POST …/rerun` API are the same service calls.

**Checks.** The mirror keeps its `threads/<workflow>/<job>` context strings and the statuses enum:
a claim writes **`in_progress`** (round 2: 05's enum gained the value, so a consumer can tell "nobody
has it" from "a runner is executing it"), `complete`/`cancel` map success → `success`, failure →
`failure`, cancelled → **`cancelled`**. Only `success` is green, so every other state (including
cancelled and in-flight) still blocks a required check. A job that is never claimed stays `pending`.

### 2.3 As implemented (round 2, pass 2, 2026-11-21) — schedule, retention, permissions, approval

```
repos/<o>/<r>/threads/schedule.json                 cron last-fired watermark (CAS'd)
repos/<o>/<r>/meta/merged_authors.json              merged-PR author projection (pulls; the gate's evidence)
```

#### `schedule:` — cron evaluated by the maintainer

The parser validates cron; the **maintainer evaluates it**. Both halves are new:

- **Syntax (parse time).** `schedule: - cron: "…"` accepts the documented five-field UTC subset:
  `*`, a number, a range `a-b`, a step `*/N`, `a-b/N` or `n/N`, and comma lists of those. Field
  ranges are minute 0–59, hour 0–23, day-of-month 1–31, month 1–12, day-of-week 0–6 with `7` as
  Sunday's alias. Anything else — month/weekday **names** (`MON`), `@daily` macros, `L`/`W`/`#`,
  six fields with seconds — is **that file's parse error** (`workflow.ParseCron` in
  `internal/threads/workflow/cron.go`), so the file's other triggers never silently stop working and
  a schedule can never be silently dead. A bare `on: schedule` with no entries and an empty
  `schedule: []` are errors too. Day-of-month and day-of-week follow the classic Vixie rule (both
  restricted ⇒ either matches).
- **Evaluation (`threads-schedule`, Seam 5).** A maintainer sweeper
  (`internal/threads/schedule.go`, `cmd/spindle/threads_sweep.go`) resolves the default branch (one
  `git rev-parse HEAD` + one `symbolic-ref`), discovers the workflow files at that commit (the same
  per-file discovery the UI uses), and for every `(path, cron)` entry computes the **most recent
  tick at or before now** (`Cron.Prev`). The watermark object
  `repos/<o>/<r>/threads/schedule.json` holds `"<path>|<cron>" → unix second` of the last fired tick;
  an entry fires only when that tick is strictly greater.
- **The watermark CAS is the commit point (never double-fire).** Firing is a CAS `PutUpdate` of the
  watermark; the winner then creates the run. Two instances sweeping the same minute race on one
  version — the loser reads the advanced watermark and creates nothing — and a **restart** (a fresh
  process with no memory) re-reads it, so a tick is never re-fired. The write happens **before** the
  run is created, so a crash between the two loses one scheduled run rather than duplicating one
  (at-most-once is the fail-safe direction); a failed run creation consumes the tick and is logged,
  never retried in a spin. A watermark that cannot be read or written is an error, never a fire.
  The sweep prunes entries no longer present in any workflow file and refuses more than 512 entries.
- **Cost, and what the run looks like.** One exact-key GET (the watermark) plus one bounded file list
  per pass; no LIST on any hot path. The run is `event: schedule`, `actor: schedule`,
  `trusted: true`, `branch`/`ref` = the default branch, and `workflow_commit` = the default tip.
  There is **no separate "run now" verb**: the manual path is already `workflow_dispatch` (the Run
  workflow control), which is a different event with different semantics (a ref choice and typed
  inputs), so reusing it keeps one manual path instead of two.
- **Cadence.** `threads-schedule` is due once a wall-clock minute per repository (cron's own
  granularity), through the maintainer's `(repo, kind)` single-flight task path with progress
  packets. The gate compares **minute boundaries**, not a full minute measured from the previous
  pass: a duration gate measured from a pass at 08:37:57 reads "a hair early" at 08:38:57 and lost
  tick 08:38 entirely (round 3, §2.4). Manual start joins the pass rather than racing it
  (`cmd/spindle/threads_sweep.go`).

#### Retention and GC (`threads-gc`)

- **Window.** `threads.retention` (default **720h = 30 days**, 11_config_cli.md). A run is deleted
  only when it is **terminal** and its `completed_at` (or `created_at`, for a run that never started)
  is older than the window. A queued/running/`pending_approval` run is never touched, whatever its
  age.
- **What goes with it.** The run header, every job record, every log frame of every attempt, and the
  run's queue entries (`Job.QueueSeq` exactly; an unclaimed run's entries are found by one bounded
  LIST over the queue family in the maintenance pass — never on the claim path). The dispatch head
  drops the dangling sequences on its next visit.
- **The checks are independent, and that is why this is safe.** Checks live in `checks/<sha>/
  <context>.json` (05 §2) and are owned by `internal/checks`; the merge gate, the checks page and the
  PR panel read that family only. Threads retention **never deletes a check record**, so deleting an
  old run neither un-greens nor un-blocks a merge — the required check is still there and still
  authoritative. The only loss is the run page's log/step history (a check description naming a
  deleted run id links to a 404). `TestRetainKeepsChecksForLivePR` pins it.
- **Runners too.** A `revoked` runner past the window and a `pending` registration whose 15-minute
  token expired unused are deleted (registry record + credential record); a live runner is never
  touched. Attempt tokens are stateless HMACs, so there is nothing to sweep for them — expiry and
  completion revocation are checks at verification time (§5).
- **Idempotent, bounded, narrated.** At most 200 runs and 200 runner records per pass (the rest is
  the next pass's work), progress packets, and a summary notice with the counts
  (`runs/jobs/logs/queue/runners/credentials/freed`). A second sweep over an unchanged repository
  deletes nothing. The retention pass runs at most once an hour **per repository** on the maintainer
  (a per-repository in-memory gate, so one interval sweeps every repository; round 3 fixed a
  process-wide slot that let only the first repository of each hour retain), while the queue-repair
  half keeps its own 5-minute cadence (§2.4).

#### `permissions:` → a scoped job token

- **Scopes derive from the merged spec.** The effective permission map is the job spec's
  `permissions:` (workflow merged with the job, job wins) over the instance default
  (`threads.default_permissions`, default `read`): `all`/`read-all`/`write-all` set the baseline and a
  specific key overrides it. **A fork run is always `contents: read` regardless of what the workflow
  declares** — the trust class wins (§2/§5.2). An unrecognised level never widens access.
- **The token.** The attempt token (`wjt_`, §3.1) — the existing credential, not a second type — gains
  the `push` scope exactly when `contents` resolves to `write`. It is minted at `start` (the
  credential half of the job payload), embeds `{repo, run, job, attempt, runner, scopes, exp}`, is
  short-lived (the job's timeout + 10 minutes, capped at six hours) and is reported back in both the
  fetch payload (`permissions`, `scopes`, `checkout.read_only`) and the start answer, so the job
  states its own authority.
- **Enforcement is on the git path, not just the payload.** The credential resolves (Seam 2) to an
  unprivileged `ci:<runner>` principal carrying **one repository grant**: read on the run's
  repository always, push only with the `push` scope (`auth.Principal.JobRepo/JobPush`). The server
  honours exactly that grant: `upload-pack` on its repository is allowed even for a principal that is
  not a member, `receive-pack` is allowed only with the push scope and only to that repository, and
  the push policy still applies on top (a protected ref refuses it exactly like any other non-owner —
  a job token is never the triggering user). No other repository, no settings API, no admin.
- **Revoked at completion.** The resolver re-reads the job record (one exact-key GET, only for a job
  token) and rejects a token whose job is terminal or whose attempt was superseded — the same
  identity authority every protocol write uses; an expired token is rejected before any store read.
- **A token push never retriggers workflows (round 3, §2.4).** GitHub suppresses workflow runs for a
  push made with `GITHUB_TOKEN`; spindle does the same for a push authenticated by a Threads job
  token. The push lands normally (the refs move, a push mirror may sync), but the push funnel marks
  it `byJob` (`auth.Principal.JobRepo`), the Threads on-push hook creates **no run** from it, and the
  events-bridge wake is skipped, so it cannot wake a `pull_request` `synchronize` either. The WAL
  event still carries the `ci:<runner>` pusher, and the pulls sink treats such an event as
  refresh-only (the PR head moves; no `synchronize` run) — so the periodic sweep cannot resurrect the
  run the push path suppressed. Without this a `contents: write` workflow that pushes loops forever
  (the round-3 audit's double `push` run).
- **Every step receives it as `$SPINDLE_JOB_TOKEN`** (by NAME, through the docker client's
  environment — never in argv or `.git/config`), so a write job can push from a script step with the
  documented credential helper and a read job's push fails at the server.
- **Shallow-clone limitation (documented, legible).** A job that pushes must clone with
  `fetch-depth: 0`: a shallow clone's push carries the `shallow` protocol section, which spindle's
  receive-pack v1 does not advertise. The server refuses it on the git wire with an ERR packet naming
  the cause and the remedy (`fetch-depth: 0`, `git fetch --unshallow`) and logs the refusal with the
  repo and principal, so it reads as a fixable workflow mistake rather than a bare `HTTP 400`.
  `internal/e2e/threads_runtime_test.go` pins the working shape. Advertising `shallow` is a
  follow-up, not a silent failure.

#### First-time-contributor approval gate

The rule is precise (and §5.2 repeats it): a run is created in **`pending_approval`** — its jobs
written, its checks mirrored as `pending`, but **nothing enqueued**, so no runner can see it — when
ALL of:

1. the event is `pull_request` and the head is a **fork** (a same-repository branch author already has
   write access; their run is trusted and never gated);
2. the actor is **not** `maintain`/`admin` in the repository and not a configured instance admin
   principal (a static token flagged `admin` is recognised by name; an OIDC admin known only by
   email/domain/group is not resolvable from a PR event's actor name and may be gated once — the gate
   is an approval prompt, not a lockout);
3. the actor has **no previously merged pull request in this repository** — "merged" means a PR whose
   merge task landed and whose *thread author* is this principal, read from pulls'
   `merged_authors.json` projection (one exact-key GET). It is authorship of the PR that counts, never
   a commit's author field, an email or a host-level write flag. The projection is written when a
   merge lands and self-heals from the PR index on a miss (one bounded scan); a PR merged before the
   projection existed and already aged out of the index is the one case that can gate once more;
4. the gate is enabled (`threads.approve_first_time_forks`, default true).

A maintainer then **approves** (`POST …/api/threads/{run}/approve`, or the run page's *approve*
button, maintain role): the header CASes `pending_approval → queued`, records `approved_by/at`, and
the jobs are enqueued through the same `enqueueJob` the normal creation path uses; two concurrent
approvals enqueue once (the CAS decides). Or **denies** (`…/deny`): `denied_by/at` is stamped and the
operator cancel path cancels the queued jobs, so the run is `cancelled` and can never execute; a
later approve is a `409`. A fold triggered by an unrelated transition cannot release the gate. An
unreadable evidence source or a missing registry **holds** the run (fail closed).
`internal/e2e/threads_runtime_test.go` proves it live: with a real runner polling throughout, a held
run stays `pending_approval` with unstarted jobs and no claim, approving executes it, denying cancels
it.

### 2.4 As implemented (round 3, 2026-11-21) — retention scope, token-push suppression, `skipped`

Round-3 verification (`docs/AUDIT-V1.md`, Threads) found two FAILs and three smaller correctness
gaps. This section is the normative statement of the four rules that close them; the audit results
live in `docs/AUDIT-V1.md`.

**Retention cadence is per repository, owned by the scheduler (not the service).** `Retain` has no
clock of its own. `cmd/spindle/threads_sweep.go` runs two `threads-gc` sweepers — queue repair every
5 minutes, retention hourly — and each carries a gate **keyed by repository**
(`map[repo]time.Time`). One maintainer pass over the repositories therefore sweeps every repository
whose window elapsed; the round-3 defect was a single process-wide `retentionGate` field on the one
`Service`, so the first repository of each hour consumed the slot and every other repository's
terminal runs and logs lived forever (observed: a 3-hour-old run with `retention = 2h` survived six
restarts on a six-repo instance). Requirements this fixes, and why each holds:

- **Every repository is swept within the interval.** The gate is per repo; a fresh pass treats each
  repo independently. `TestRetentionSweepsEveryRepository` (the acceptance gate) is two expired
  repositories + one young on a single service, one pass, and asserts both expired ones go while the
  young one stays.
- **A restart does not starve a repository.** The gate is in memory, so a restart forgets it and
  repeats a pass for every repository — the safe direction (an extra pass, never a skipped one).
  `TestRetentionRestartsDoNotStarveEitherRepository` runs six restart cycles and asserts both
  repositories are retained each time.
- **Two instances never double-delete.** Both may run the same repository concurrently; every step is
  an exact-key delete that tolerates an already-gone object (a no-op on every backend), and the run
  LIST is read-only, so the loser frees nothing and neither pass fails.
  `TestRetentionTwoInstancesDoNotDoubleDelete` runs both concurrently over one shared store and then
  asserts the next pass deletes zero.
- **The per-pass bounds stay.** 200 runs / 200 runner records per pass, unchanged; the rest is the
  next pass's work.

**A job-token push creates no workflow run (`GITHUB_TOKEN` rule).** GitHub does not start workflow
runs for a push made with `GITHUB_TOKEN`; spindle now records the same distinction. The push funnel
(`internal/server/bind_ssh.go`) computes `byJob` from the job grant (`JobRepo`, the scope `JobPush` having let the push through) and hands it
to the on-push hook: the push still lands (the refs move; a push mirror may still synchronize), but
Threads creates no `push` run, and the events-bridge wake is skipped so no immediate
`pull_request synchronize` follows. The WAL ref event's `pusher` (`ci:<runner>`) marks the event for
the pulls sink, which still applies the head move but does not fire the `synchronize` trigger — so a
later events sweep cannot create the run either. Proven by
`internal/server` (`TestJobTokenPushIsMarkedAndDoesNotWake`),
`cmd/spindle` (`TestJobTokenPushCreatesNoRun`: a job push produces nothing, the human push right
after produces one run) and `internal/pulls` (`TestJobTokenRefEventNeverSynchronizes`).

**A blocked dependent is `skipped`, not `cancelled`.** The state vocabulary gained `skipped` for
jobs (and a run folded only from skipped jobs still folds to `success`, matching GitHub: skipped is
neutral). The `needs:` gate terminalises a dependent whose dependency failed, was cancelled **or was
skipped** as `JobSkipped` (transitively, wave by wave); `job.Skipped` maps to the checks state
`skipped` (`internal/checks`, non-green — only `success` satisfies a required check), and the run
page renders the same state. An operator cancel and a fail-fast sibling keep `cancelled`; a run whose
own jobs were genuinely cancelled still folds to `cancelled`. `terminal()` covers both. Proven by
`internal/threads/needs_test.go` (job state and checks mirror), `internal/checks/states_test.go`
(enum, rollup precedence, the required-check gate refuses it) and `internal/webui` (`runDot`).

**The cron gate follows the wall-clock minute; the first tick comes from the watermark or the current
boundary.** The schedule sweeper's gate is minute-aligned: it is due as soon as the clock enters a new
minute after the last pass, instead of requiring a full cadence measured from the previous pass (a
pass at 08:37:57 then read "a hair early" at 08:38:57, the next ran at 08:39:07 and tick 08:38 was
lost forever). The tick itself is always computed from the sweep's own `now` (`Cron.Prev(now)` = the
current boundary for a per-minute cron) and gated by the persisted watermark: with a watermark
present the next eligible tick is the first boundary strictly after the stored one; on a first run
(no watermark) the current boundary is eligible immediately (catch-up for a daily schedule that
started moments late) — never a tick sampled from a pre-pass clock reading that lands in the past. A
restart re-reads the watermark, so it cannot re-fire and cannot skip. Proven by
`TestScheduleSweeperFirstTickIsTheNextMinute` (08:37:57 fires tick 08:37; the very next pass at
08:38:00 is due and writes tick 08:38 to the watermark, not 08:39) and the existing
`TestThreadsScheduleSweeperFiresOnce` / `schedule_test.go` (no double fire across restarts and
instances).

## 3. Runner protocol (our own, documented)

JSON over HTTPS, cookie/token authenticated, small and versioned (`X-Spindle-Runner: 1`):

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/threads/runners/register` | one-time registration token → runner id + long-lived token |
| POST | `/api/v1/threads/runners/declare` | version + labels (`linux`, `docker`, `spindle-1`) |
| POST | `/api/v1/threads/runners/fetch` | long-poll (≤ 30 s) for a job matching the labels; returns 204 when idle |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/start` | attempt-scoped job token issued; marks running |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/log` | append a log frame (idempotent by sequence) |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/complete` | conclusion + step results |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/cancel` | acknowledge cancellation |

Identity is **per attempt**: a requeued job gets a new attempt number and token; writes carrying a
superseded attempt are rejected by identity, not by timestamp. Fetching *is* the heartbeat (the
long-poll), so there is no separate liveness ping.

### 3.1 As built (pass B1)

| Method + path | Auth | Body → answer |
|---|---|---|
| `POST /api/v1/threads/runners/register` | the registration token in the body | `{token, name?, labels?}` → `201 {runner_id, repo, token, labels}` (exactly once; 401 otherwise) |
| `POST /api/v1/threads/runners/declare` | `Bearer wrt_…` | `{version, labels}` → `200 {runner_id, repo, state, labels, lease_seconds}` |
| `POST /api/v1/threads/runners/fetch?timeout=` | `Bearer wrt_…` | empty → `200` job payload, or `204` when the poll (≤ 30 s, default 20) expires |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/start` | `Bearer wrt_…` | empty → `200 {token, expires_at, repo, run_id, job_id, attempt}` |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/log` | `Bearer wjt_…` | `{seq, text}` → `200 {seq, state, cancel, lease_until?}` |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/complete` | `Bearer wjt_…` | `{conclusion, steps[]}` → `200 {repo, run_id, job_id, state, run_state}` |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/cancel` | `Bearer wjt_…` | empty → `200 {state, run_state}` (the ack) |

`{ref}` is the **opaque job reference** the fetch payload carries (`job_ref`, `jr_` + base64url of
`owner/repo/run/job`): the protocol table's `{id}` cannot be a bare job id, because two runs may both
own a job called `build`. The path's ref and attempt must match the token's claims (defence in
depth). The fetch payload is self-contained: repo/run/job ids, attempt, commit, event, branch,
workflow, trust class, `spec`, the check context, `secrets` + `mask` (trusted runs only),
`secrets_denied`, and the lease. Declaring labels is how a runner says what it *is*; fetching is how
it says it is *alive*; both are one write per call at most.

**Operator surface (repo admin).** `POST …/api/threads/runners/tokens` mints a registration token
(shown once), `GET …/api/threads/runners` lists the repository's runners, `DELETE
…/api/threads/runners/{id}` revokes one; `GET/PUT/DELETE …/api/threads/secrets[/{name}]` is the
write-only secret store (the GET lists names/timestamps and `{enabled}`, never a value). Cancel and
rerun are `POST …/api/threads/{run}/cancel` and `…/rerun` (repo write). `spindle runner
token|list|revoke OWNER/REPO` is the CLI twin, and Settings → Runners / Secrets are the same
service calls (admin-gated, the page and the API share the gate).

## 4. Execution (Docker-in-Docker)
- **One container per job, steps via `docker exec`.** Not one container per step: the workspace, the
  installed toolchain and the network namespace must persist across steps, which is what workflow
  authors expect.
- **Workspace:** a per-job named volume mounted at the workdir; checkout runs **inside the job
  container** with `git` and the job token, so no host path is ever shared and the classic
  DinD-path problem does not exist. The runner never mounts the host filesystem into the job.
- **Services** get their own network per job, reachable by their `services.<id>` hostname; the job
  network is torn down with the job.
- **Runner deployment:** the runner itself runs as a container. It reaches a Docker daemon that is
  either a per-runner `docker:dind` sidecar or a daemon socket explicitly provided by the operator —
  **never a host socket by default**, and it refuses to start if it would expose the host root
  filesystem without an explicit `--allow-host-docker` acknowledgement.
- **Capacity is a runner setting, and the system has no ceiling.** `capacity` defaults to **1** but a
  runner accepts `--capacity 20` or `--capacity 100`; it keeps that many jobs in flight, each in its
  own container, network and workspace volume on the daemon it was given. The server has no central
  worker counter: every job is claimed by a CAS on its own queue object, and `fetch` is a long-poll
  returning one job per call — a capacity-N runner runs N concurrent fetches. That makes both axes
  horizontal: 100 concurrent jobs can be one runner at capacity 100, a hundred ephemeral runners at
  capacity 1, or any mix.
- **Two deployment shapes; the trade-off is isolation, not scale.**
  *Scale-out (recommended default):* many short-lived runners at capacity 1, one job per daemon. Jobs
  cannot see each other; 100 jobs means 100 runners, registered in bulk from tokens the operator
  mints. *Throughput:* one runner at capacity 20–100 sharing a daemon — every container on it can
  reach the daemon API and therefore the other containers, so it is for trusted repositories only and
  the UI shows that caveat next to the capacity field. Labels route the pools apart
  (`runs-on: trusted` vs `runs-on: untrusted`).
- **Server-side scale:** the queue is append-only objects and a claim is a per-job CAS write (no
  lock, no LIST on the claim path), so adding runners adds throughput linearly. Fairness is FIFO by
  enqueue sequence within a label set; a per-repository concurrency cap is a scheduler setting
  (default unlimited).
- Resource limits per job container (cpu/memory/pids) come from runner config, with the workflow able
  to lower them, never raise them.

### 4.1 As implemented (pass B2, 2026-11-21) — the executor and the runner binary

**The command.** `spindle runner serve` is the runner process (same binary, no store access). It
reads its origin from `--server`/`SPINDLE_SERVER`, its credential from `SPINDLE_RUNNER_TOKEN` or
`--token-file` (or bootstraps with `--register wrg_…`, which consumes a registration token and
writes the 0600 token file), declares its version and labels, then keeps `--capacity` fetch loops
in flight (default **1**). Every worker long-polls `fetch` (5–30 s), claims one job, executes it and
repeats; errors back off 1 s → 30 s, an instant 204 sleeps 250 ms so a proxy can never turn the
poll into a tight loop, and a rejected credential (`401`) stops the process with exit 1. SIGTERM
cancels the in-flight jobs: each job context dies, its containers are removed, and the attempt is
reported `cancelled` on a fresh context. **The runner refuses to start on the default
`/var/run/docker.sock`** unless `--allow-host-docker` acknowledges that the daemon exposes the host
root filesystem (15 §4); point `DOCKER_HOST` at a per-runner `docker:dind` for the isolation shape.

**Configuration** (flags ⊕ environment; there is no `[runner]` section in `spindle.toml`, because a
runner has no data dir and no store — it is a protocol client):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--server` | `SPINDLE_SERVER` | `http://127.0.0.1:8080` | spindle origin (the `spindle auth login` state is not used) |
| — | `SPINDLE_RUNNER_TOKEN` | — | the `wrt_` runner credential (wins over the file) |
| `--token-file` | `SPINDLE_RUNNER_TOKEN_FILE` | — | 0600 file holding the credential; `--register` writes it |
| `--register` | — | — | one-time `wrg_` registration token to consume, then serve |
| `--capacity` | `SPINDLE_RUNNER_CAPACITY` | `1` | concurrent jobs (one container, network and volume each) |
| `--labels` | `SPINDLE_RUNNER_LABELS` | `self-hosted,linux,docker` | declared labels (matched against `runs-on`) |
| `--name` | `SPINDLE_RUNNER_NAME` | — | display name recorded at registration |
| `--docker` | `SPINDLE_RUNNER_DOCKER` | `docker` | the docker CLI binary |
| `--image` | `SPINDLE_RUNNER_IMAGE` | `alpine/git:latest` | job image when the workflow names none (must carry `git`: the checkout runs inside the container) |
| `--allow-host-docker` | `SPINDLE_RUNNER_ALLOW_HOST_DOCKER` | off | acknowledge a host-root-exposing daemon |
| `--fetch-timeout` | — | `20` | long-poll seconds (1–30) |

**One container per job, steps via `docker exec`.** Per attempt (names are a hash of repo/run/job/
attempt, so cleanup needs nothing but the payload): `docker network create sp-n-…`, one
`docker volume create sp-v-…` mounted at `/spindle/workspace`, one container per service
(`--network-alias <services.id>`, its env/volumes/options), then the job container
(`--add-host host.docker.internal:host-gateway`, `--workdir`, the workspace volume, the merged
job/container env, `--entrypoint /bin/sh` plus the runner's own keep-alive command — the image's
entrypoint/command are accepted but the runner owns the container's lifetime, as GitHub's runner
does). **Service readiness** polls `docker inspect` state (honouring an image `HEALTHCHECK`) and,
when the service declares ports, TCP-dials each port published on a loopback *ephemeral* host port
(concurrent jobs never collide; the job reaches the service by its alias and container port).
`unhealthy`/`exited`/`dead` fails the job immediately.

**Checkout runs inside the container** (`actions/checkout`), never on the host: `git init`,
`git remote add origin <url>`, `git fetch --depth=1 origin <sha>`, `git checkout FETCH_HEAD`. The
URL comes from the fetch answer (derived from the request's host and `X-Forwarded-Proto`, so a
proxy works; a loopback origin is rewritten to `host.docker.internal` because the container cannot
reach the host's 127.0.0.1). The credential is the **attempt token** (`wjt_`, read scope) resolved
by the server chain to an unprivileged `ci:<runner>` principal and supplied to git through an
in-container credential helper (`$SPINDLE_JOB_TOKEN`, passed as `docker exec --env NAME` so it never
appears in argv). Every job — trusted or fork — gets exactly this credential, which is what makes
the checkout read-only by construction.

**A job that pushes must check out full depth.** The default checkout is shallow
(`git fetch --depth=1 origin <sha>`), and **a shallow clone cannot push**: its `git push` opens with the
protocol's `shallow <oid>` section, which receive-pack v1 does not advertise, so the server refuses it.
This is the same trap as GitHub Actions (where the default checkout depth also refuses a push); the
server does not change the default, it tells you why. Any job whose step runs `git push` must make its
checkout full-depth:

```yaml
      - uses: actions/checkout
        with:
          fetch-depth: 0
```

`fetch-depth: 0` drops `--depth` from the in-container `git fetch`, so the push has the history it
needs (`internal/threads/runner/plan.go`; pinned by `internal/e2e/threads_runtime_test.go`). The
server's refusal names the cause and this remedy, and a rejected push is logged with
`repo`/`principal` and the remedy, so the job log shows what to fix (closing-audit Observation 2).

**Steps.** Each step is `docker exec --workdir <ws>[/<working-directory>] --env … <container>
<shell>`. Environment layering is job env → container env → step env, plus the runner's built-ins
(`CI`, `GITHUB_*` including `GITHUB_SHA`/`GITHUB_REF`/`GITHUB_EVENT_NAME`/`GITHUB_WORKSPACE`, the
`SPINDLE_*` aliases, and `MATRIX_<AXIS>`), with repository secrets added last per step (by NAME,
values in the docker client's environment). `shell:` maps to `sh -e -c` (default), `bash -eo
pipefail -c`, `python3 -c`, `pwsh -command`, `cmd /c`; anything else fails the step explicitly.
`if:` evaluates the documented subset (`success()`, `failure()`, `always()`, `cancelled()`,
`github.*`, `matrix.*`, `env.*`, `steps.<id>.outcome`, `!`, `==`, `!=`, `&&`, `||`, parentheses) and
an unsupported expression **fails the step with the parse error** rather than skipping it silently.
`${{ matrix.<axis> }}` is substituted in step names, scripts, `if`, env, shell and
working-directory (and in the concurrency group at expansion). `continue-on-error` on a step keeps
the job green but records the failure; a failed step without it skips the remaining steps (each
recorded `skipped`) and fails the job. `timeout-minutes` on a step (and on the job) terminates the
step by killing the job container — the only reliable in-container kill — and reports `timed out`.
**`uses:` is the built-in set only: `actions/checkout`.** Anything else fails the step with
"unsupported action … there is no marketplace"; a job-level `uses` (reusable workflow) fails the job
with a clear message. Accepted-but-not-enforced: `permissions` (carried on the spec; there is no
GITHUB_TOKEN to scope yet).

**`needs:` ordering is enforced by the queue (round 2, 2026-11-21; skipped state round 3).** A job is
claimable only when every `needs` entry is terminal **success**; until then its queue entry is
skipped without consuming the probe's claim budget (so a dependent enqueued ahead of its dependency
never starves it). A dependency that failed, was cancelled or was skipped terminalises its dependents
as **skipped** (round 3: the state vocabulary gained `skipped`; GitHub reads a dependent that never
ran as skipped, and a genuinely cancelled job still reads `cancelled`), wave by wave, so the skip is
transitive and needs no runner poll. A `needs` on a matrix job means **every cell**. The runner knows
nothing about ordering: `internal/threads/needs.go` owns the gate, the cascade runs at the end of
every terminal job transition, and a terminal job wakes the fetch hub so a dependent is claimed
immediately. The workflow parser rejects a `needs` name that is not a job (`ErrBadNeeds`).

**Logs, cancel, completion.** One frame writer per attempt: a single flusher goroutine merges
stdout/stderr, splits at 32 KiB (the server's cap is 256 KiB), assigns monotonically increasing
`seq`s, masks every known secret value (and masks step error text) and POSTs each frame; a retry is
idempotent by sequence, and a frame the server rejects after three attempts is dropped with a
warning. A 25 s idle heartbeat frame keeps the claim lease alive and is the cancel channel: a log
answer with `cancel: true` cancels the job context immediately, so a cancelled step dies even
mid-command. `complete` posts the job conclusion plus per-step results (number, name, conclusion,
exit code, duration) on a fresh context, so a SIGTERM-time cancellation still reports. **Cleanup runs
on every path** — success, failure, setup error, cancel, timeout and panic — via a `defer` that uses
a fresh 90 s context: `docker rm -f` the container and each service, `docker network rm`,
`docker volume rm -f`. `scripts/b2-scale.sh` asserts zero leftover `sp-` containers after a
20-runner run.

**Matrix and concurrency (server side).** `strategy.matrix` expands at run creation into one job
record per combination, axes in **alphabetical order** (the spec's matrix is a Go map; the job ids
and checks contexts only need determinism), named GitHub-style `build (1.27, amd64)` with their own
`j-…` id, check context and `Spec.Matrix` cell (`Spec.MatrixGroup` names the product so fail-fast
can cancel exactly the siblings). The product is capped at 256 cells (a parse-time-looking error:
`ErrUnprocessable`). `fail-fast` (default on) cancels the other cells when one fails, through the
same terminal path the operator's cancel uses. **Concurrency groups** are enforced on the claim
path: `repos/<o>/<r>/threads/concurrency/<slug>-<hash>.json` names the run holding the group; a
claim CASes that record (one in-flight run per group per repo), gives its queue entry back when
another live run holds it, and retries on the next 2 s probe; a holding run's sibling jobs pass (a
workflow-level group belongs to the run), and `cancel-in-progress` cancels the holder through the
existing cancel path (the holder is a *running* job, so it is signalled and the group frees when its
runner acks — at most one heartbeat interval, 25 s). A terminal fold releases exactly the groups on
the run header, and an orphaned holder (a folded run that never released) is stolen by the next
claim.

**Evidence.** `scripts/b2-manual-e2e.sh` (two jobs, checks green) and `internal/e2e/threads_runner_test.go`
(three scenarios: two-job success + SSE logs + checks; a failing step with a `require_checks` merge
blocked; a fork PR with no secrets, the base-branch workflow and a rejected push with the checkout
token) run a real runner against a live server. `scripts/b2-scale.sh 20 20` runs 20 capacity-1
runners against a 20-cell matrix: 20/20 jobs success, 20 unique queue sequences, all attempt 1,
zero leftover containers, ~7 s wall time on a 10-core host.

## 5. Security model (non-negotiable)

1. A job container is root inside its own namespace; with a shared DinD daemon, jobs on that daemon
   can see each other. Therefore **capacity 1** is the default, capacity > 1 is an explicit operator
   choice with the caveat shown in the UI, and scale-out (many capacity-1 runners) is the documented
   path to 20/100 concurrent jobs for untrusted code.
2. Fork PRs: base-branch workflow, no secrets, and a token whose scopes are `contents: read`
   whatever the workflow declares; plus an approval gate for first-time contributors — the run is
   created in **`pending_approval`** (jobs written, nothing enqueued) and a maintainer (maintain+)
   approves or denies it (§2.3). "First-time" means authorship of a pull request that has never been
   merged in this repository; maintainer-authored runs and merged contributors' runs skip the gate.
   Same-repository PRs are not gated (their author already has write access).
3. Secrets are scoped to the repository and exposed only to trusted runs; they are masked in logs
   (best effort, documented as such) and never persisted in the run record.

   **As built (B1).** A secret is `repos/<o>/<r>/threads/secrets/<name>.json` =
   `{name, sealed, created_by, created_at, updated_at, version}` where `sealed` is
   `base64(nonce || AES-256-GCM ciphertext)`. The key is derived with **HKDF-SHA256** (stdlib
   `crypto/hkdf`) from `server.secrets_key` when set, else `server.auth.session_secret` — two
   purpose-separated keys come out of it, one for sealing and one for signing attempt tokens. The
   AAD binds each ciphertext to `(repo, name)`, so a sealed value cannot be replayed under another
   repository or another name. With neither key configured the store **fails closed**: the secrets
   API answers 503 ("set server.secrets_key or server.auth.session_secret"), attempt tokens are
   signed with a process-local key (valid exactly as long as the jobs they authorise), and a loud
   warning is logged at boot. The value is decrypted for two consumers only: the **trusted** job
   payload (a fork run gets `secrets: {}` and `secrets_denied: true`) and the log masker. Masking is
   server-side, applied to every log frame and every step result before it is stored, and is
   **best effort by construction**: exact substring replacement of each value (values shorter than
   4 bytes are skipped), so a runner that encodes, splits or derives a value defeats it. The API
   never returns a value; the UI renders `•••••• (set)`.
4. Egress from job containers follows the same policy surface as the rest of spindle
   (`internal/egress`), with a per-runner default-deny option.
5. A job token is scoped to one attempt and **one repository**, expires with the job (job timeout +
   10 minutes, capped at six hours) and is **revoked at completion** (the resolver rejects a token
   whose job is terminal or whose attempt was superseded). It reads its own repository and pushes to
   it only with the `push` scope (`permissions: contents: write`); it cannot push to protected refs
   (it is a `ci:` principal, not the triggering user, so the push policy applies unchanged) and it
   grants nothing in any other repository. A leaked fork token has no push scope at all.

## 6. UI and API

### 6.1 As implemented (T1)

| Method + path | Auth | Request → response |
|---|---|---|
| `GET /{o}/{r}/api/threads?after=&n=` | repo read | `{runs: [Run], more}` — newest first, cursor `after` = last run id, `n` default 50 / max 200 |
| `GET /{o}/{r}/api/threads/workflows?commit=` | repo read | `{commit, workflows: [{path, name, digest, triggers, inputs?, error?}]}` — `commit` defaults to the default branch tip; an empty repository is an empty list; a broken file is one entry with `error`; `inputs` is the declared `workflow_dispatch` schema (`[{name, type, description?, required, default?, has_default?, options?}]`, sorted by name) |
| `GET /{o}/{r}/api/threads/{run}` | repo read | `{run: Run, jobs: [Job]}`; unknown run `404` |
| `POST /{o}/{r}/api/threads` | repo **write** | `{ref, workflow?, inputs?}` → `201 {runs: [Run]}`; `workflow` is a basename or path (empty = every `workflow_dispatch` file); unknown file/ref `404`, bad body `400`, an input that does not match the declared schema **`422` naming the input** |

**Dispatch inputs (round 2, 2026-11-21).** `inputs` is a map of input name → scalar (string, boolean
or integral number; a nested value is `400`) and is accepted only with an explicit `workflow` (the
one file whose schema applies; otherwise `422`). Each value is validated against the declared
`workflow_dispatch.inputs`: unknown names, a missing `required` input, a `choice` outside its
`options`, and a non-boolean `boolean` are `422` naming the input. Declared defaults fill omitted
optionals, a boolean normalizes to `true`/`false`, and the resolved map is recorded on the run header
(`inputs`) and in every job spec (`spec.inputs`), where the runner substitutes `${{ inputs.<name> }}`
in step names/scripts/env/if/shell/working-directory and exposes `inputs.<name>` to `if`. An
inputless dispatch (no `inputs` key) keeps working exactly as before.

Both lanes (`/api` and `/api-browser`), `no-store` on every GET, plain-text errors, RFC 3339,
arrays `[]` never null, anonymous-denied reads a real 401 with `WWW-Authenticate: Bearer`. The
handler is the Seam 1 `server.ExtraRoutes` chain (`Handle` reports false for non-threads paths) and
its two templates are registered in `/api/v1` discovery (`api.RegisterExposed`) from composition.

Page (server-rendered, `internal/webui`; `12_web_ui.md` §2): `GET /{o}/{r}/threads` lists runs
(workflow, event/branch, commit link, state, `untrusted` chip, time, duration) with an internal-scroll
tab strip entry **Threads** and the shared empty state, and above them a **Workflows** section listing
every discovered file at the default branch tip — name, path, trigger chips, and the per-file parse
error (anchored `#workflow-<slug>`; one broken file never hides the others or the run list).
`GET /{o}/{r}/threads/{run}` shows the header (workflow path/commit/digest/actor, trust class,
duration) and each job with its runs-on, check context, state, duration and steps; when the run's
workflow file currently fails to parse, the page links to that anchored error.

**Run workflow (round 2, 2026-11-21).** A write-capable caller gets a **Run workflow** control on the
Threads page (the page's one `.btn.primary`; `docs/style-guideline.md` §1 F3): a workflow select
listing only files that declare `workflow_dispatch`, a ref field pre-filled with the default branch,
and the selected workflow's declared inputs as typed controls (a `select` for `choice` and
`boolean`, a text field for `string`, required marked, defaults pre-filled). Choosing another
workflow is a plain `GET ?dispatch=<path>` re-render (no client JavaScript), and the form posts to
`POST /{o}/{r}/threads/dispatch`, which calls the **same** `DispatchRun` the JSON API uses and
redirects to the created run. A validation failure re-renders the page with the service's message
inline (naming the input) and the submitted values intact; a read-only caller never sees the control,
and the service still enforces the write role. The previous dead `/api#threads` link is gone.

### 6.2 As implemented (pass B1)

| Where | What |
|---|---|
| Settings → Runners | mint a registration token (copy-once panel), list id/name/labels/state/version/last seen, revoke. Admin only (the tab is hidden below admin; a reader's POST is the service's 403). |
| Settings → Secrets | add/replace (name + password field), list `name / •••••• (set) / updated`, delete. When the keyring is absent the panel explains `server.secrets_key` instead of offering a form. |
| Run page | cancel run / re-run buttons (write role only — the affordance, with the service as the gate), per-job step results, and one live-log panel per job (`data-threadlog` + URL; the island replays then tails). |
| CLI | `spindle runner token OWNER/REPO [--labels a,b] [--name N]`, `spindle runner list OWNER/REPO`, `spindle runner revoke OWNER/REPO ID`, and **`spindle runner serve`** (pass B2: the runner process itself — flags/env in §4.1; `--register` bootstraps a credential, SIGTERM stops cleanly). |
| JSON API | §3.1: the runner protocol plus the admin runner/secret routes, cancel and rerun, and the log window (`GET …/jobs/{job}/log`, SSE or `?accept=json`). |

The full v1 surface (live logs, cancel/rerun, runners, secrets, settings) is below.

- **Repository tab:** `Threads` (replaces the old "Actions" naming) — run list with commit, event,
  status, duration; a run page with jobs, live logs (SSE via the existing task stream), cancel and
  rerun.
- **Settings:** runners (register/revoke, labels, capacity), secrets (write-only). The fork-approval
  policy and the retention window are instance settings (`threads.approve_first_time_forks`,
  `threads.retention`, 11_config_cli.md §2), shown and edited by /setup with every other key — not
  per-repo repository settings (one policy per instance keeps the gate's evidence a single
  projection).
- **JSON API/SDK:** runs (filtered), jobs, logs (paged), cancel/rerun/approve/deny, runners,
  secrets — the same gates as the pages.

### 6.3 As implemented (round 2, pass 2)

| Where | What |
|---|---|
| Run list | **Filters** (workflow · state · actor) as a plain GET form: the narrowing is URL state, so it is shareable and the back button works; a filtered empty result says the filter matched nothing (never "no runs yet") and offers a clear link. Each row also shows the actor. The controls sit in the canonical filter-bar grid card (2-up on phones, 4-up from `sm`) with labelled fields, so every `.input` has a real width — round 3 fixed a flex/min-w-0 row that rendered them 28–50 px wide (12_web_ui.md). |
| Run page | For a held run: the reason ("first-time contributor's fork pull request … has not executed") and, for maintain+, **approve run** (the page's one primary action) / **deny**; after a decision, `approved by …` / `denied by …` with the time. Per job: **runner attribution** (`runner <name>`) and the job's declared `permissions` map. |
| Commit page | A **Checks** panel below the message: the combined state and one row per context (state dot, context, description, creator, time), with a link to the full checks page — the same rows the checks page and PR panel render (`collab_check_contexts`). Omitted when nothing ever reported. |
| Threads page | Unchanged from pass 1 except the filters and the actor column; a held run shows the `pending_approval` state chip. |
| JSON API | `POST …/api/threads/{run}/approve`, `POST …/api/threads/{run}/deny` (maintain+; `409` on a second decision), `?workflow=&state=&actor=` on `GET …/api/threads`, `permissions`/`scopes` in the job payloads, `runner`/`runner_name` on job records, `approval` on the run header projection. |

## 7. Tests (definition of done)

- **Protocol contract:** register → declare → fetch → start/log/complete, with attempt supersession,
  idempotent log frames, cancel, and auth failures (expired/foreign attempt tokens).
- **Scheduler:** trigger matching, label matching, required-check integration, fork trust-class
  selection (base-branch workflow + no secrets), approval gate, cron evaluation, concurrency caps.
  **As built (round 2):** `internal/threads/needs_test.go` (linear chain, diamond, failed dependency
  skips downstream, cancelled dependency, unrelated jobs in parallel, enqueue-order starvation,
  matrix fan-in, entry projection) and `internal/e2e/threads_triggers_test.go` (PR head push →
  `synchronize` run in <5 s with a one-hour sweep, forward-only head, no duplicate run).
- **Schedule (round 2 pass 2; gate round 3):** `internal/threads/workflow/cron_test.go` (tick math
  incl. `*/N`, ranges, `n/N`, lists, Vixie dom/dow, Sunday `7`, UTC, no-tick expressions, every
  unsupported spelling rejected at parse time), `internal/threads/schedule_test.go` (fires once per
  tick, no double fire across a simulated restart, two instances sweeping concurrently create exactly
  one run, a disabled/unscheduled workflow fires nothing, corrupt watermark fails closed, watermark
  pruning), `cmd/spindle/threads_sweep_test.go` (the sweeper kinds + per-repo due gates + all three
  bodies, and `TestScheduleSweeperFirstTickIsTheNextMinute`: a fresh start at 08:37:57 fires the
  current boundary and the very next minute-aligned pass fires 08:38, never 08:39) and
  `internal/e2e/threads_runtime_test.go` (a live server's maintainer fires a cron workflow once and
  not twice in a minute).
- **Retention (round 2 pass 2; scope round 3):** `internal/threads/gc_test.go` + `round2_*_test.go`
  (old terminal deleted with its jobs/logs/queue entries, young kept, running kept, cancelled ages
  out, second pass no-op, checks survive for a live PR, dead runners removed and live ones kept, and
  every store-failure path fails closed) and `cmd/spindle/threads_sweep_test.go` **per-repository
  cadence**: `TestRetentionSweepsEveryRepository` (two expired repos + one young in one interval — the
  acceptance gate), `TestRetentionRestartsDoNotStarveEitherRepository` (six restarts) and
  `TestRetentionTwoInstancesDoNotDoubleDelete` (one shared store, concurrent passes).
- **Token-push suppression (round 3):** `internal/server/pushmirror_test.go`
  (`TestJobTokenPushIsMarkedAndDoesNotWake`: a `JobPush` principal lands, is marked `byJob`, and
  wakes nothing; a human push right after wakes the bridge), `cmd/spindle/threads_test.go`
  (`TestJobTokenPushCreatesNoRun`) and `internal/pulls/triggers_test.go`
  (`TestJobTokenRefEventNeverSynchronizes`).
- **`skipped` state (round 3):** `internal/threads/needs_test.go` (failed/cancelled/skipped
  dependency → `skipped` dependents, checks mirror `skipped`, genuine cancel keeps `cancelled`,
  transitive fold), `internal/checks/states_test.go` (enum accepted, rollup precedence, required-check
  gate refuses it) and the run-page dot projection.
- **Permissions and the job token (round 2 pass 2):** `internal/threads/permissions_test.go` (default
  scopes, `contents: read` narrowed, `contents: write` → `push`, `write-all` baseline + override,
  unknown level ignored, fork cannot escalate, start/fetch report the map, completion/expiry
  revocation) and `internal/e2e/threads_runtime_test.go` (a real container pushes with
  `contents: write` and is refused with `contents: read`, proven by the refs on the server).
- **Approval gate (round 2 pass 2):** `internal/threads/approval_test.go` (held run claimable by
  nobody, maintainer/merged/same-repo exemptions, fail-closed evidence, approve → claimable, deny →
  cancelled, conflict on a second decision, the projection) plus told-live
  `internal/e2e/threads_runtime_test.go` with a real runner polling throughout.
- **Executor:** a fake `docker` CLI asserting the exact argv shape for create/exec/cp/rm, workspace
  volume lifecycle, service network teardown, timeout kill; a real Docker-in-Docker e2e that runs a
  two-step workflow when a daemon is present (skip, not fail, otherwise — the same rule as the git
  e2e). **As built (B2):** `internal/threads/runner` unit tests (plan/argv, `if`, continue-on-error,
  step timeout, frame ordering/masking/cancel, cleanup on success/setup-failure/panic/cancel/SIGTERM,
  backoff) plus `internal/e2e/threads_runner_test.go` (real runner + docker: two-job success with SSE
  logs and checks; a failing step with a `require_checks` merge blocked; a fork PR with no secrets, the
  base workflow and a rejected push). `scripts/b2-scale.sh` is the 15 §6 shape (20 runners × 20 jobs).
- **Security matrix:** fork PR gets no secret and a read-only token; a job cannot read another
  repository's secrets; logs are masked; capacity-1 runners reject a second claim.
- **Rendered:** run page live logs at desktop and ~390 px; no page-level horizontal scroll.

## 8. Decisions & deviations

- **NEW (2026-11-21) — Threads round 3: per-repository retention, token-push suppression, `skipped`,
  and the minute-aligned cron gate.**
  (a) **Retention cadence belongs to the scheduler, keyed by repository.** The round-2 gate was one
  field on the process's single `Service`, so the first repository of each hour consumed the slot;
  the fix is a per-repo map in `cmd/spindle/threads_sweep.go` (a `threads-gc` queue sweeper at 5 min
  and a `threads-gc` retention sweeper at 1 h, same task kind, separate cadences). An in-memory gate
  is enough: a restart repeats a pass for every repo, which is the safe direction, and the deletes are
  idempotent so two instances cannot corrupt anything — a persisted watermark would add a store round
  trip per repo per pass for no safety gain. Per-pass bounds (200 runs / 200 runners) are unchanged.
  (b) **A job-token push never triggers a workflow** (`GITHUB_TOKEN` parity). The marker is the
  existing `JobRepo`/`JobPush` principal, carried as `byJob` on `server.Options.OnPush`; the Threads
  hook creates nothing and the inline events wake is skipped, and the WAL event's `ci:` pusher is
  mapped by composition to `pulls.RefEvent.ByJob`, which refreshes a PR head but never fires
  `synchronize`. Suppressing only the immediate wake would leave the periodic sweep able to resurrect
  the run, so both paths are covered. Push mirrors still fire: a mirror is not a workflow run.
  (c) **`skipped` is a first-class job/check state, not a cosmetic relabel.** GitHub reads a
  `needs:`-blocked dependent as skipped, distinct from an operator cancel; the shared vocabulary
  gained it, `terminal()` covers it, the checks enum gained `StateSkipped` with a precedence slot
  below `cancelled` (both non-green), and the run fold treats skipped jobs as neutral so a run whose
  only non-green job was skipped still folds green. An actual cancel — operator, fail-fast sibling,
  denied approval — keeps `cancelled`.
  (d) **The cron due gate is minute-aligned and the tick always comes from the sweep's own clock.**
  Measuring a full cadence from the previous pass made a pass at 08:38:57 "a hair early" and lost
  tick 08:38; comparing wall-clock minute boundaries removes the sub-second race without shortening
  the cadence. The fired tick is `Prev(now)` gated by the persisted watermark (the first boundary
  strictly after the stored one), and with no watermark the current boundary is eligible at once —
  never a tick sampled before the pass started, which could land in the past.
- **NEW (2026-11-21) — Threads round 2, pass 2: schedule, retention, the job token and the approval
  gate.**
  (a) **Cron syntax is validated by the parser, evaluated by the maintainer.** A cron the scheduler
  cannot evaluate would otherwise be a silently dead trigger, which is worse than a visible parse
  error; the documented subset (`*`, numbers, ranges, steps, lists — no names, macros or modifiers)
  is deliberately narrow, and everything else fails that file with its own message. Evaluation is a
  maintainer task (`threads-schedule`), never a request path.
  (b) **The watermark CAS is the commit point and it is written before the run.** Two instances
  racing the same tick converge on one run; a restart re-reads it and never re-fires. Writing first
  makes a crash lose one run rather than duplicate one — at-most-once, chosen because a duplicate
  scheduled deploy is worse than a missed one (the missed tick is visible in the log and the next
  tick fires normally).
  (c) **No separate "run now" for schedules.** `workflow_dispatch` already exists with a ref choice
  and typed inputs; a second manual path with different semantics would be two things to keep
  consistent, so the Run workflow control is the manual trigger for a repo and a schedule is only
  ever the clock.
  (d) **Retention deletes runs, never checks.** The checks family is 05's and is what the merge gate,
  the checks page and the PR panel read; Threads owns run/job/log/queue objects only. That is why an
  old required check still gates a merge after its run aged out, and why retention needs no
  reference scan against live PRs. `threads.retention` has no "keep forever" spelling: an unbounded
  CI history is a storage bug, not a feature (an operator who wants it sets a very large window).
  (e) **The job token is the attempt token, extended — not a second credential type.** Its scopes are
  derived from `permissions:` (over the instance default, forked runs pinned to read), carried inside
  the signed payload, and enforced by the existing Seam 2 credential resolver plus two explicit git
  gates (`upload-pack`/`receive-pack`). The grant is a repository-scoped field on the principal
  (`JobRepo`/`JobPush`) rather than a role, because a job is not a user: it must never inherit host
  write, admin, or membership in another repository, and the push policy must keep applying.
  Completion revocation is a job-record read at verification, because the token is stateless by
  design (no token family to sweep — the reason retention has nothing to expire).
  (f) **Every step receives `$SPINDLE_JOB_TOKEN` by name.** The B1 plan only exposed it to the
  checkout step; a `contents: write` job that wants to push needs it in its own script step. The
  value still travels in the docker client's environment (never argv, never `.git/config`), and a
  read-only job's push is refused server-side, so exposing the variable is not an escalation.
  (g) **The approval gate is enforced by the queue, and its evidence is a projection.** A held run's
  jobs exist (page, checks mirror, PR panel stay coherent) but are not enqueued, so `fetch` cannot
  see them; approval runs the same enqueue path as a normal creation. The "has this author merged
  here?" answer is pulls' CAS'd `merged_authors.json`, written at merge and self-healing from the PR
  index on a miss — one exact-key GET instead of a LIST or a per-PR pr.json walk. An unreadable
  evidence source or an unwired registry **holds** the run: an unknown contributor must not execute.
  (h) **Shallow clones cannot push (documented, with a legible refusal).** receive-pack v1 does not
  advertise the `shallow` capability, so a job that pushes must clone with `fetch-depth: 0`; the
  checkout section shows the exact `with:` snippet, the e2e pins the working shape, and the receive
  path refuses a shallow push with an ERR packet that names the cause and the remedy (plus a Warn log
  carrying repo/principal) instead of the generic `malformed push request`. Advertising `shallow`
  is a follow-up, not a silent failure.
  (i) **Run-list filters are server-side URL state** (`?workflow=&state=&actor=`), so the narrowed
  list is shareable and the back button works; a filtered page scans at most 1000 headers so one
  request can never become a repo walk. Runner attribution is stored on the job at claim time (id +
  display name) so the page needs no registry read, and the commit page reuses the checks service's
  combined view rather than inventing a second status read.

- **NEW (2026-11-21) — Threads round 2, pass 1: prompt PR delivery, `needs:` gating, dispatch
  inputs + UI, check-state fidelity.**
  (a) **The push path wakes the events bridge**, in `pushPipeline` (the one function both the HTTP
  and SSH transports use) right after a landed push. `events.Bridge.Wake` is the existing mechanism
  (non-blocking, coalescing, sweep as backstop), so no new delivery path was invented: the same
  `_events/notify` route operators use is simply also called by the producer. Only landed pushes
  wake it — a refused push produces no ref event.
  (b) **Head ordering is the event's own, never a clock.** `pr.json` gains `head_seq` (the WAL log
  sequence of the last applied ref event). A delivery at or below it is stale and cannot move the
  head; a non-fast-forward move is applied only when the event's pre-image is the head we hold (a
  genuine force-push). Live ref reads carry no ordering and stay forward-only by ancestry — which
  is also why the read path can no longer rewind a head (the audited `0bacfe12 → 361812b9`). The
  `synchronize` trigger fires only on an applied move, so a replay creates no duplicate run.
  (c) **`needs:` lives in the queue, not the runner**, and a blocked dependent is terminalised as
  `cancelled` rather than gaining a new `skipped` state: the state vocabulary is shared with the
  checks mirror and the run fold, and a skipped job must never read green. The cascade is driven by
  terminal job transitions (no scheduler, no sweep), and the claim path re-checks eligibility so a
  lost cascade write still cannot start a dependent early. `needs` names are validated at parse
  time, so a missing dependency only exists in legacy records (treated as blocked, never as
  "ready").
  (d) **`inputs` are validated against the workflow's declared schema, not coerced silently**; a
  nested/non-scalar value is `400` (a client bug) while a schema mismatch is `422` naming the input
  (a workflow-author contract). Inputs require an explicit `workflow` because two files may declare
  different schemas — dispatching "every file" with inputs would need a per-file union nobody can
  predict. The resolved map rides the run header and every job spec, and the runner substitutes
  `${{ inputs.* }}` with the same rules as matrix cells (no new expression language; `if` gained
  one context).
  (e) **The checks enum gained `in_progress` and `cancelled`** (05 §2), replacing the "pending with
  started_at" workaround: consumers can now distinguish queued/executing/concluded, and a cancelled
  job is not called a failure. The merge gate is unchanged in behaviour — it requires `success`, and
  both new values are non-green by construction. The run fold also reports `cancelled` instead of
  `failure` when nothing actually failed.
  (f) **Duration is a read model.** `duration_seconds` is computed when a run/job is served, never
  stored or CAS'd, so the bucket shapes the round-A/B passes fixed do not change and an old record
  renders correctly.
  (g) **The Run workflow form is server-rendered with no client JavaScript**: the workflow choice is
  a `GET ?dispatch=<path>` re-render, and the submit posts to a page route that calls the same
  `DispatchRun` as the JSON API. One dispatch implementation, one gate (the service's write check),
  errors inline on the page the user is looking at.

- **NEW (2026-11-21) — Threads pass B2: the executor and the runner binary as built.**
  (a) **`spindle runner serve` is a subcommand of the one binary**, not a second `cmd`: deployment
  already ships one image, and the runner shares the protocol types (`threads.FetchResult`, `Spec`)
  so the two ends of the wire cannot drift; it reads flags ⊕ environment (table in §4.1) and has no
  `spindle.toml` section because it owns no data dir and no store. (b) **The checkout credential is
  the attempt token**, resolved by the server's Seam 2 `ExtraCredential` hook (beside the checks
  `wct_` shape) to an unprivileged `ci:<runner>` principal. This is why every checkout is read-only:
  a fork run has no push credential because no run has one. The runner bearer (`wrt_`) is claimed
  the same way, shape-only, because a token-mode or oidc-mode chain would otherwise 401 every
  protocol call before the runner handler ran; the one call that carries no bearer
  (`runners/register`, a body token) is the single runner-protocol path exempt from the oidc
  anonymous-write assert. **Deferred:** a private repository whose read gate excludes a non-member
  `ci:` principal still cannot be cloned by a job — a repo-scoped read grant for the job principal
  is the follow-up; public repositories (and `anonymous_read` instances) work today.
  (c) **The runner owns the job container's lifetime** (`--entrypoint /bin/sh` + a keep-alive
  command), as GitHub's runner does; a workflow `container.entrypoint`/`command` is carried but not
  applied, while `env`/`ports`/`volumes`/`options` are. Resource limits therefore come from the
  runner/daemon, not the workflow (the §4 bullet "the workflow may lower them" is deferred with the
  same reason: there is no per-job limit surface yet). (d) **Service ports are published on loopback
  ephemeral host ports** in addition to the job network alias: it is the only readiness probe that
  needs no tooling inside the image. One `services.<id>` container, its alias and its ports per job,
  torn down with the job. There is no workflow healthcheck field in v1; an image `HEALTHCHECK` is
  honoured when present. (e) **Step timeouts kill the job container.** `docker exec` cannot kill the
  in-container process tree portably, so a timed-out step (or job) terminates the container; the
  job fails with `timed out`, which is also the only honest way to stop a wedged process. (f)
  **Matrix axes expand in alphabetical order**, because `strategy.matrix` decodes to a Go map and the
  YAML declaration order is lost — the property that matters is determinism across servers (the job
  ids are bucket keys and the names are check contexts). Names follow GitHub's `name (v1, v2)`
  convention, each cell gets its own id/context/`Spec.Matrix` cell, the product is capped at 256, and
  `fail-fast` is enforced on failure through the operator's cancel path. (g) **Concurrency is
  enforced at claim time, keyed by a per-group holder record**, not at run creation: the claim CAS is
  the arbitration the queue already uses, a blocked job gives its entry back and re-probes (no
  scheduler state), and `cancel-in-progress` reuses the existing cancel path — a running holder is
  signalled and the group frees when its runner acknowledges (≤ 25 s), so a container is never
  abandoned behind the operator's back. `release` happens on the terminal run fold; an orphaned
  holder is stolen on the next admission. (h) **`if` is a documented subset with explicit errors**,
  never GitHub's full expression language: an unrecognised function/context fails the step with the
  parse error so an author sees that spindle did not understand the condition, instead of a step
  silently skipping. `permissions` is carried (there is no GITHUB_TOKEN to scope) and the doc says so
  in §4.1. (i) **The log frame writer is also the control channel**: a 25 s heartbeat frame (empty
  text) renews the lease and polls the cancel flag, so a step that prints nothing is still
  cancellable; the buffer is bounded and a rejected frame is dropped after three idempotent retries.
  (j) **`scripts/b2-*.sh` are the evidence harnesses** for the doc's claims (manual e2e and scale),
  next to the automated `internal/e2e` scenarios; they are operator-runnable and bounded by a
  timeout.
- **NEW (2026-11-21) — Threads pass B1: the runner protocol, queue and secrets as built.**
  (a) **Runners are repository-scoped.** The registration token is minted in the repository's
  Settings → Runners and the runner only ever sees that repository's queue. An instance-wide runner
  would need a global dispatch index over every repository, which is exactly the repo-wide LIST the
  claim path forbids; the per-repo head keeps the claim at one probe and one CAS. Labels route
  *pools*, not tenants. (b) **The queue entry is the truth, `head.json` is a bounded dispatch
  projection** (512 pending seqs). The claim CAS is on the entry, so concurrent claims have exactly
  one winner regardless of the head; a stale head seq fails its CAS and is trimmed, and the gc
  rebuilds the window from the entry family. A claimed entry stays listed until it finishes — the
  head is also how a lapsed lease is noticed without a sweep. (c) **Attempts are the identity.** A
  requeue bumps the job record's `attempt` before the entry returns to `queued`; every attempt write
  compares its token's attempt with the job's current one, so a superseded runner is rejected by
  identity (409), never by a timestamp. The `wjt_` token is an HMAC over an embedded
  `{repo, run, job, attempt, runner, scopes, exp}` payload, verified locally; `start` takes the
  runner token (the call that mints the attempt credential), every other verb takes the attempt
  token. (d) **The protocol's `{id}` is an opaque `job_ref`** (`jr_` + base64url of
  `owner/repo/run/job`): job ids are only unique within a run, and the protocol paths are
  top-level. (e) **Log frames are keyed `(attempt, seq)`** at `…/log/<attempt:03d>-<seq:019d>`, so a
  retry is idempotent (Create, 412 = already stored) and a new attempt cannot collide with its
  predecessor's frames. The viewer reuses the SSE envelope writer and the run page's island reads it
  through the existing fetch-stream transport. (f) **Secrets are AES-256-GCM under an
  HKDF-derived key** (details in §5.3): write-only API, AAD-bound to `(repo, name)`, trusted runs
  only, masked best-effort in logs and step results, and **unavailable (503) when neither
  `server.secrets_key` nor `session_secret` is set** — an ephemeral sealing key would make stored
  ciphertexts unreadable after a restart. `server.secrets_key` is a new optional config key
  (≥ 32 bytes when set) and is masked in /setup like every other secret. (g) **Cancel reaches a busy
  runner through its own writes**: the cancel flag rides the log/complete answers (and `start`), and
  the runner acks with `cancel`; an unclaimed job is cancelled immediately. Cancel and rerun are
  repo-write operations served from the JSON API *and* the run page. (h) **Rerun refuses while a
  job is running** — abandoning a live container behind the operator's back is a surprise, so the
  operator cancels first. (i) **The checks enum stays frozen** (05 §2 has no `in_progress`): a claim
  mirrors `pending` with `started_at` set, which is what the merge gate needs, and the description
  carries the progress. Context strings are unchanged. (j) The **container executor, checkout,
  services, matrix expansion and concurrency-group enforcement** were pass B2 and landed on
  2026-11-21 (§4.1); B1 ended at the protocol boundary (a runner that claims a job, replays logs and
  reports a conclusion with no container is a valid B1 client).
- **NEW (2026-11-21) — Threads pass A: triggers, versioned spec, parser breadth, per-file
  isolation.** (a) `pull_request` fires on opened/synchronize/reopened and never on close, emitted
  from `internal/pulls` beside the existing lifecycle seams (`PullEvents`); a head push that moves
  an open PR's head is the synchronize edge, so the trigger has a real commit and does not depend on
  the client retelling the event. (b) The PR merge task calls `BranchPublished` after its base-ref
  publish, and composition routes it into the **same** `CreateRun` path as a client push — server-side
  publishes bypass `onPush` by construction (see `bind_ssh.go`), and a merge that built nothing was
  the audit's highest-impact trigger gap. Update-branch publishes a head update, which the WAL event
  bridge already delivers as a synchronize, so it deliberately does not double-fire. (c) The job record
  carries a versioned `spec` (version 1) with container mapping, services, merged env, job
  `if`/timeout/continue-on-error, permissions, matrix/fail-fast, concurrency, job `uses`/`with` and
  every step field; the old convenience fields stay a projection, and the checks mirror reads the
  workflow label from the spec (same context string, so no gate churn). (d) Parsing is per file: a bad
  file carries its own `error` in discovery and the API/UI, and disabled only its own workflow —
  the previous all-or-nothing failure meant one unsupported key silenced every workflow in the repo.
  (e) The workflow list is **derived from git at request time**, not persisted as a bucket projection:
  it cannot go stale, costs one bounded `ls-tree` plus one `show` per file on a human-scale page, and
  keeps pass A free of a new state family (law 4). (f) `permissions`, matrix expansion/fail-fast and
  concurrency are accepted and carried but **not enforced** until the runner and scheduler exist
  (pass B); the parser rejects unknown keys rather than pretending to honour them.
- **NEW (2026-11-20) — T1 as-built record/selection decisions.** (a) Run ids are
  time-ordered (`%019d-nanos` + random) so the cursor list needs no index projection and
  run creation pays no extra CAS write; (b) the run header is created LAST (jobs first), making it
  the run's commit point and orphans invisible/sweepable; (c) the checks mirror uses the existing
  statuses family via `checks.Service.RecordStatus` (one shared validation/storage path, no
  HTTP-reachable bypass) rather than a new check-runs family — 05 §1 already deferred check-runs,
  and required-check gating is identical; (d) contexts are `threads/<workflow>/<job>`, lowercased
  and slugged because the checks grammar forbids spaces; (e) cancelled jobs never read green (today
  a distinct `cancelled` conclusion, amended in round 2 — at T1 the enum had no such value and they
  mapped to `failure`), so a cancelled required check cannot gate a merge open; (f) a bare `push:` /
  `pull_request:` key is a real trigger (presence flags in the parser), matching CI-author
  expectations. (g) `threads-gc` is registered now and its body is T3 (no logs yet).
- **NEW (2026-11-20) — "Threads" names spindle's CI.** Forgejo calls it Actions; the mechanism is
  the same (workflow file, container jobs, checks results), the name is ours.
- **NEW — our own runner protocol, JSON over HTTPS.** The reference runner uses an undocumented
  RPC; a documented, versioned JSON surface is smaller to implement, easier to debug, and keeps the
  clean-room boundary unambiguous.
- **NEW — one container per job (steps via exec), corrected from `12_runner.md` §1.2.** One container
  per step cannot preserve a workspace or a toolchain across steps.
- **NEW — checkout inside the job container.** Removes host path coupling and is the standard shape.
- **NEW — server selects the workflow revision per trust class**; fork runs never read the fork's
  workflow file and never receive secrets.
- **NEW — capacity 1 by default, no ceiling.** A runner may run 20 or 100 jobs; scale-out with
  capacity-1 runners is the isolation-preserving way to reach those numbers for untrusted code.
