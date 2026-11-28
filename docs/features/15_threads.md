# 15 — Threads: CI runs

> **Usage reference:** [`docs/threads.md`](../threads.md) is the user guide (workflow keys, runner
> flags, triggers, checks, troubleshooting). This file is the design / as-built record.

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
  `internal/threads/workflow` (`gopkg.in/yaml.v3`, the one package family allowed to import it —
  AGENTS law 1, amended 2026-11-20; the one-way importer `internal/threads/importer` is the other
  member, amended 2026-11-26 for PAR-002). Unknown keys are rejected, not ignored, and rejection is
  **per file**: the error belongs to that workflow only and is shown in the UI/API (§2.1). No
  cross-forge file fallback (`.github/`, `.forgejo/` are not read) — one documented location.
- **Accepted workflow syntax (v1):** `on` as a string, a list, or a mapping with
  `push.branches/tags/paths` (and the `*-ignore` twins), `pull_request.branches/paths/types` (types
  filtered against the emitted lifecycle set, validated at parse time), `release.types`,
  `issues.types`, `issue_comment.types`, `workflow_dispatch.inputs` (typed
  string/choice/boolean/number/environment with required/default/options) and
  `schedule.cron` (+ `timezone`); `concurrency`
  (group + cancel-in-progress); `permissions` (mapping or read-all/write-all); job `runs-on`
  (scalar or list), `container` (image or mapping), `services`, `env`, `if`, `needs`,
  `timeout-minutes`, `continue-on-error`, `strategy.matrix` (a simple product) with `fail-fast`, and
  a job-level `uses`/`with` (reusable workflow). Anything else is a parse error for that file.
- **Triggers v1 (Wave 2 extends the vocabulary):** `push` (branches/tags/paths), `pull_request`
  (every emitted lifecycle action, with `types` filtering that closed vocabulary), `release`,
  `issues`, `issue_comment`, `workflow_dispatch` (manual, from the UI/API), `schedule` (cron,
  evaluated by the maintainer role).
- **Jobs:** `runs-on` labels matched against registered runners; `container:` image override;
  `services:` (sidecar containers); `steps:` with `run:` and `uses: actions/checkout` (our own
  checkout action, see §4); `env`, `if`, `needs`, `timeout-minutes`, `continue-on-error`, and a step
  `id:` (the `steps.<id>.outcome`/`.conclusion` key, and the output producer; §4.1), job `outputs:`
  and step/job output capture through `$SPINDLE_OUTPUT` (§4.1).
- **Artifacts:** `uses: actions/upload-artifact` / `actions/download-artifact` move files between
  jobs of one run (§2.5), with a size/file cap, the run's retention window (a shorter per-artifact
  `retention-days` allowed), a ZIP download and a page list.
- **Not v1:** remote marketplace actions (local composite actions and `docker://` images are
  supported; a remote `owner/repo@ref` is refused with its reason), matrix expansion beyond a simple
  `strategy.matrix` product, **caching actions** (`actions/cache`), OIDC cloud credentials, reusable
  workflows, environment protection rules.

## 2. Server side (bucket-native)

Everything is objects; no queue server.

```
repos/<o>/<r>/threads/runs/<run_id>.json          run header (commit, event, workflow blob ref, state)
repos/<o>/<r>/threads/runs/<run_id>/jobs/<jid>.json  job record (labels, attempt, state, timing, steps)
repos/<o>/<r>/threads/runs/<run_id>/jobs/<jid>/log/<seq>  log chunks (append-only, 256 KiB frames)
repos/<o>/<r>/threads/queue/<enqueued>.json       pending job (labels, attempt, deadline) — CAS claim
threads/queue/index.json                          global dispatch hint (CAS'd, bounded; a hint, never authority)
threads/runners/<id>.json                         instance runner registry (labels, capacity, last seen)
threads/runners/tokens/<id>.json                  one-time registration token (hash only, TTL)
threads/runners/credentials/<id>.json             runner credential (hash only, revocable)
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

**Triggers (as wired).** `push`, `pull_request`, `workflow_dispatch`, `schedule` and (Wave 2)
`release`, `issues` and `issue_comment` create runs:

| event | workflow revision | `trusted` |
|---|---|---|
| `push` (client push) | the pushed commit | `true` |
| `push` (PR merge, server-side publish) | the merge commit | `true` |
| `pull_request` (any emitted lifecycle action), same repository | the head commit | `true` |
| `pull_request` (any emitted lifecycle action), fork | the **base branch tip** (resolved `refs/heads/<base>`) | `false` |
| `workflow_dispatch` | the resolved ref's commit | `true` |
| `schedule` (maintainer cron) | the default branch tip | `true` |
| `release` | the default branch tip (the run reports the tag commit) | `true` |
| `issues` / `issue_comment` | the default branch tip | `true` |

`pull_request` fires on `opened`, `synchronize`, `reopened`, `closed` (including a merge), `edited`,
`ready_for_review`, `converted_to_draft` and the metadata actions; the PR edges are emitted by
`internal/pulls` next to its
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

`pull_request.types` is a real filter over a closed vocabulary. The pulls `PullEvents` seam emits
`opened`, `synchronize`, `reopened`, `closed` (including a merge), `edited`, `ready_for_review` and
`converted_to_draft`; the shared issues thread sink supplies `labeled`, `unlabeled`, `assigned`,
`unassigned`, `milestoned` and `demilestoned`. `internal/threads/workflow` validates `types` against
that set at parse time (an unsupported name, or `types: []`, is that file's parse error naming the
supported set) and `Workflow.Fires`/`SelectFiles` take the delivered action, matching a run only when
it is listed. `CreateRun` requires a pull_request `Action` and rejects one outside the set, so no
caller can create a run for an edge no service emits. An absent `types` keeps the default: every
emitted action.

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
  "runs_on": "self-hosted, linux, docker",
  "container": "golang:1.27",
  "needs": ["lint"],
  "steps": [{"name": "test", "run": "go test ./..."}, {"uses": "actions/checkout"}],
  "spec": {
    "version": 1,
    "workflow": "CI",
    "runs_on": ["self-hosted", "linux", "docker"],
    "container": {"image": "golang:1.27", "env": {"CGO_ENABLED": "0"}},
    "services": {"redis": {"image": "redis:7", "ports": ["6379:6379"]}},
    "env": {"GOFLAGS": "-mod=readonly"},
    "if": "github.ref == 'refs/heads/main'",
    "timeout_minutes": 15,
    "continue_on_error": false,
    "permissions": {"contents": "read"},
    "strategy": {"matrix": {"go": ["1.26", "1.27"]}, "fail_fast": true},
    "concurrency": {"group": "ci-main", "cancel_in_progress": true},
    "outputs": {"digest": "${{ steps.test.outputs.digest }}"},
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
the merged `permissions`, `strategy.matrix`/`fail-fast`, `concurrency`, the job's `outputs:`
templates (step/needs output references are resolved at execution, §4.1), a job-level `uses` +
`with` (reusable workflow — accepted and carried, execution is pass B/C), and every step field
(`id/name/run/uses/with/env/if/shell/working-directory`). `permissions`, matrix expansion/fail-fast
and concurrency groups are **carried, not enforced** in pass A — there is no job token or scheduler
yet; the version bump (never a silent field change) is how a runner refuses an unknown shape.
`job_id` is `j-<slug>` (a collision gets `-2`), so it is a stable, URL-safe key. `Name` is the YAML
`name`, falling back to the map key. The checks mirror reads the workflow label from
`spec.workflow`, so the record alone determines the context.

Two server-written fields on the job record describe a job that did not run or did not conclude
cleanly: `reason` (why — a false job-level `if:`, a blocked `needs:`, an unsupported condition; the
run page renders it) and `tolerated: true` (a `failure` whose `continue-on-error: true` the run fold
ignores — the state stays `failure`, so "failed but tolerated" is distinguishable from "succeeded").
A successful job may also carry `outputs` (the map the runner reported from its `outputs:` templates,
§4.1); it is absent on a job that did not succeed, because a dependent job must never read a failed
dependency's outputs.

**State vocabulary** (runs and jobs): `queued | running | success | failure | cancelled | skipped`.
T1 creates everything `queued`; `UpdateJob` (the T2 runner's write surface, exercised by T1 tests)
CAS-updates one job, mirrors the checks record, and folds the run header (`foldRunState`: any live
job keeps the run running; all terminal → failure if any failed, cancelled if any cancelled, else
success — `skipped` and `tolerated` failures are neutral).

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
failure that skipped them is already the run's failure; round 3); a failed job whose
`continue-on-error: true` was tolerated is neutral too (round 4: the run conclusion follows the other
jobs, while the job record keeps `failure` + `tolerated`); all green ⇒ `success`. A cancelled
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
threads/runners/<id>.json                           instance runner registry (labels, capacity, last seen)
threads/runners/tokens/<id>.json                    one-time registration token (hash only, TTL)
threads/runners/credentials/<id>.json               runner credential (hash only, revocable)
threads/queue/index.json                            global dispatch hint (CAS'd, bounded; never authority)
repos/<o>/<r>/threads/secrets/<name>.json           sealed secret envelope (never a value)
repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/log/<attempt>-<seq>   immutable log frames
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

**Runner registry.** Runners are **instance-scoped** (2026-11-22,
docs/PLAN-INSTANCE-RUNNERS.md): a **host administrator** mints a registration token at
`/admin/runners`; the runner exchanges it once (15-minute TTL) for a long-lived instance
credential. The registry record (`threads/runners/<id>.json`) carries name, labels, capacity, state,
declared version, last seen and the current lease; the credential record
(`threads/runners/credentials/<id>.json`) carries only its hash and stays revocable (a revoked
runner is a real 401 on its next call). Every protocol response carries `X-Spindle-Runner: 1`.
Labels are `runner ⊇ job` over `spec.runs_on` (lowercased, sorted, deduped); a job with no labels
matches any runner (the parser requires `runs-on`, so this only guards old records).

**Attempt tokens.** `start` returns a short-lived `wjt_` token: an HMAC-SHA256 over a base64url
JSON payload `{repo, run, job, attempt, runner, scopes[], exp}`. The signature is verified locally
(no store read) and every write additionally compares the token's attempt with the job record's
current attempt — a superseded attempt is a 409 by identity, never by timestamp. Scopes are
`log`, `complete`, `cancel`, `read`; a start call requires the runner token (an attempt token there
is a clear 401).

**Logs.** `log` stores one frame per `(attempt, seq)` at `…/log/<attempt:03d>-<seq:019d>` (Create;
412 = already stored, so a retry is idempotent). A frame is a **record**, not a bare chunk:
`{time, stream, kind, text, step?, name?, conclusion?, exit_code?, duration_ms?}`. `time` is the
runner's RFC 3339 capture time, `stream` is `output` or `system`, and `kind` names the structural
marker (`step_start`, `step_end`, `group_start`, `group_end`, `truncated`); a frame with no `kind` is
captured output. Frames are capped at 256 KiB, masked before the write (see §5.3), and read back by a
prefix LIST (a human path). A frame the server refuses after its retries is counted by the runner and
stated in the next successful frame (or at close), and a sequence gap is ALSO rendered by the reader
as `log frames X..Y missing` — a dropped frame is never silent. The viewer attaches over SSE:
`GET …/api/threads/{run}/jobs/{job}/log` with `Accept: text/event-stream` replays stored frames as
`event: log` and tails new ones every second, ending with `event: done` when the job is terminal (a
`?accept=json` or plain request answers one bounded JSON window instead). The run page renders one
panel per job and the `threadlog.js` island consumes that stream (the same fetch-stream transport as
every other island; no new transport, no EventSource). The island dedupes by `(attempt, seq)`,
re-bases its cursor when a re-run changes the attempt, renders a group as a native `<details>` that
collapses when it closes, stamps each line start with the frame's time, and on a 401 waits for
re-authentication and resumes from the same cursor instead of dying.

**Whole-run log download (PAR-190).** `GET …/threads/{run}/logs.txt` and `…/logs.zip` (the API twins
`GET …/api/threads/{run}/logs.txt|logs.zip`) assemble EVERY job's current-attempt log under the
repository read gate: markers become readable lines, a sequence gap is named, a job with no output
still gets an entry saying so, and the zip carries `logs/<job-id>-<slug>.txt` plus `manifest.txt`.
One job entry is capped at 16 MiB with the cap named in the entry — a visible cap, never a silent
truncation.

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

- **Syntax (parse time).** `schedule: - cron: "…"` accepts the documented five-field subset:
  `*`, a number, a range `a-b`, a step `*/N`, `a-b/N` or `n/N`, comma lists, month/weekday names,
  the `@daily`-family macros, `L`/`L-N`/`NW`/`DL`/`D#n`, and a per-entry `timezone:` (IANA, UTC
  when absent). Field ranges are minute 0–59, hour 0–23, day-of-month 1–31, month 1–12,
  day-of-week 0–6 with `7` as Sunday's alias. An unknown macro/name, a six-field expression with
  seconds, or an invalid timezone is **that file's parse error** (`workflow.ParseCron` in
  `internal/threads/workflow/cron.go`), so the file's other triggers never silently stop working and
  a schedule can never be silently dead. A bare `on: schedule` with no entries and an empty
  `schedule: []` are errors too. Day-of-month and day-of-week follow the classic Vixie rule (both
  restricted ⇒ either matches).
- **Evaluation (`threads-schedule`, Seam 5).** A maintainer sweeper
  (`internal/threads/schedule.go`, `cmd/spindle/threads_sweep.go`) resolves the default branch (one
  `git rev-parse HEAD` + one `symbolic-ref`), discovers the workflow files at that commit (the same
  per-file discovery the UI uses), and for every `(path, cron)` entry computes the **most recent
  tick at or before now** (`Cron.PrevIn` on the entry's zone). The watermark object
  `repos/<o>/<r>/threads/schedule.json` holds `"<path>|<cron>|<timezone>" → the local wall-clock
  slot` of the last fired tick. A slot is the scheduled local minute, so a DST fall-back hour (two
  instants, one local minute) fires once, and a spring-forward local time (which does not exist)
  has no slot at all.
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
- **Runners too.** A `revoked` instance runner past the window and a `pending` registration whose
  15-minute token expired unused are deleted (registry record + credential record); a live runner is
  never touched. The family is instance-wide, so one process-wide hourly gate keeps the
  per-repository retention pass from LISTing it once per repository. Attempt tokens are stateless HMACs, so there is nothing to sweep for them — expiry and
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
- **Platform scopes beyond git (2026-11-28).** The same effective map gates `packages` (the job token
  is exchanged at `/v2/token` for a short-lived, repository-scoped registry token: `read` ⇒ pull,
  `write` ⇒ pull+push) and `checks`/`statuses` (the check-run/status surface of its own repository,
  `read`/`write`). Each is checked from `JobPerms` at the credential boundary, never from a repository
  role, and a job token cannot mint another credential. The closed vocabulary and the refused names
  are in docs/threads.md §5.3.1.
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

### 2.5 As implemented (2026-11-21) — run artifacts

The remaining CI gap: every pipeline that publishes a build was excluded (`uses:
actions/upload-artifact` failed the step with "there is no marketplace"). Artifacts now pass files
between jobs of one run.

**Workflow surface (runner/app/plan).** Two built-in actions, `actions/upload-artifact` and
`actions/download-artifact`, with a closed `with:` vocabulary — `name`, `path`,
`if-no-files-found`, `retention-days` (upload) and `name`, `path` (download). Any other key is a
**named step error** (`unsupported with key "retention_day"`), never a silent no-op; that matters
because a typo otherwise looks like a working workflow. `path` accepts an exact file, a directory
(recursive), a glob (`*`/`?` within a segment, `**` across segments, `[...]` classes) or `.`, matched
case-sensitively against workspace-relative paths; a glob that matches a directory includes its
files. Only regular files are uploaded (symlinks are not followed or archived), so a pattern can
never publish bytes from outside the workspace. `if-no-files-found` is `warn` (default), `error` or
`ignore`; `retention-days` is 0 or 1–90. The job image must carry `find` and `tar` (busybox does;
the default image does) — a missing tool is a named failure.

**Transport.** The job container never reaches the object store. Upload: the runner runs
`docker exec --workdir <ws> <container> find . -type f`, matches the patterns in Go, then one
`docker exec -i <container> tar -cf - -C <ws> -- <paths…>` whose stdout it parses as a tar; each
regular entry is PUT to the server with the attempt token (`wjt_`,
`PUT …/attempt/{n}/artifacts/{name}/blobs/{index}?path=…&size=…`) and committed with
`POST …/attempt/{n}/artifacts`. Download: the runner GETs the manifest and each blob and writes one
tar to `docker exec -i <container> tar -xf - -C <dest>`; nothing touches the runner's disk. Both
directions ride the existing authenticated job channel, so no new credential, no presigned URL and
no container→store network path exists.

**Storage.** New families under the run (15 §2 pattern; documented in `02_storage_protobuf.md`
§2.1's note because it is a new bucket family):

```
repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/manifest.json          Create-only; the commit point
repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/blobs/<attempt:03d>-<index:06d>   immutable bytes
repos/<o>/<r>/threads/artifact-expiry/<expires-unix:019d>-<run>-<name>.json        short-window index
```

The manifest is `{name, run, job, attempt, files:[{path,size,sha256,index}], total_size,
retention_days, created_at, trusted}`. Blobs carry the attempt in their key, so a retried job can
never overwrite what a previous attempt wrote. **Artifact names are unique per run**: the manifest is
`PutCreate`, so a second upload of one name is a `409` (`artifact "…" already exists in run …`) and
the step fails — never a silent replace that would let a later job rewrite an earlier job's output.
Blobs become visible only when the manifest lands (an interrupted upload leaves blobs the run's
retention deletes, never a half-listed artifact). The server computes each blob's SHA-256 while
receiving it — a client digest is never trusted — and finalize verifies the uploaded set with one
prefix LIST (count and per-index size), so a manifest never points at a missing or short blob.

**Caps.** `threads.max_artifact_bytes_per_file` (default 256 MiB) and
`threads.max_artifact_bytes_per_run` (default 2 GiB, the sum of every artifact of the run, enforced
on upload, finalize and download) plus a fixed 1000-file cap. The server is authoritative; the
runner learns the caps in its start answer (`StartView.artifacts`) and refuses over-cap input before
streaming it. Every refusal names the cap and its config key (`413`).

**Retention.** An artifact expires with its run (`threads.retention`), so `threads-gc` deleting the
run deletes its manifest and blobs (`deleteRun` → `deleteArtifact`, manifest last). A
`retention-days` may only **shorten** that window: expiry is `min(run expiry, created +
retention-days)`, computed on read so a longer value cannot extend it. A shortening gets a row in
the repo's expiry index at finalize; the retention pass reads that index with **one** prefix LIST
and reclaims the artifact early (manifest, blobs, index row). The read path also treats an expired
artifact as absent (404/not listed) immediately, so correctness never waits for the hourly pass.
`GCResult` reports `artifacts` and `artifact_bytes`.

**Access.** List and download are repository read-gated exactly like the run page; the download route
(`GET /{o}/{r}/threads/{run}/artifacts/{name}`, ZIP with `Content-Disposition`) resolves the run under
the **URL's** repository, so it can never act as a cross-repo read — a fork PR run's artifacts live in
the base repository, exactly like its logs, and a fork's own lane sees no such run (404). The job
channel's attempt token carries a new `ScopeArtifact`: it may upload into and read artifacts of its own
run only (a blob index outside the manifest is a 404), never another run or another repository.
Artifact **contents are the user's data** (never rewritten); secret masking applies to the step's log
lines, including the artifact name echoed there.

**UI + API.** The run page renders an **Artifacts** section above the jobs: one row per artifact with
its name (the download link), size, file count, age and expiry; links only, so the page keeps its
one-primary rule. `GET …/api/threads/{run}/artifacts` returns the same list, and `RunView` (the run
page and `GET …/api/threads/{run}`) carries `artifacts`.

**Verified.** Unit: manifest/key layout, name uniqueness, incomplete-upload refusal, record
validation, caps, retention clamps and the expiry index, the permission matrix, blob digests and
job-scoped reads, ZIP contents, GC reclaims (`internal/threads/artifacts_test.go`); planning, glob
expansion, `if-no-files-found`, caps and both tar streams (`internal/threads/runner/artifacts_test.go`);
the page rows and the download headers/bytes (`internal/webui/threads_artifacts_test.go`). Real
containers: a two-job upload→download run asserting the bytes, the API list, the page link, the ZIP
download, and `threads-gc` after the window removing the blobs and the list; a fork PR run uploading
an artifact that the base repository can read and the fork's lane cannot
(`internal/e2e/threads_artifacts_test.go`). Rendered at 1440×900 and ~390×844:
`scrollWidth === clientWidth`, artifact links present, no primary added (the page's approve control
is its only primary, and only on a held run).

### 2.6 As implemented (2026-11-23) — job OIDC tokens

Full design: [`docs/PLAN-THREADS-OIDC.md`](../PLAN-THREADS-OIDC.md). Summary of the stored and wire
shapes:

| Concern | As built |
|---|---|
| issuer | `server.public_url` (trimmed), else the request origin (development). No new config key. |
| discovery | `GET /.well-known/openid-configuration`, `GET /.well-known/jwks.json` — hand-rolled JSON, `public, max-age=60`, open (a provider has no spindle credential). |
| signing key | `threads/oidc/signing-key.json` = `{version, active_kid, keys:[{kid, alg:"RS256", private_key_pem, created_at, retired_at?}]}`. Generated on first use with `PutCreate`; a 412 re-reads the winner. Automatic rotation at 90 days (CAS); retired public keys stay in the JWKS for one hour. `kid` is a public-key thumbprint. **In-process cache** of the active private key + rendered JWKS: cold = 1 exact-key GET, warm JWKS/exchange = 0, 60 s TTL for a peer's rotation, local rotation cached immediately, errors never cached, single-flighted on a cold burst. |
| request token | `wot_<64 hex>`, hash-only in `repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/id-token/<attempt:03d>.json` with the CAS'd rate window. Bound to (repo, run, job, attempt); lifetime = the attempt token's (timeout+10 min, 2 h default, ≤ 6 h). |
| exchange | `POST /api/v1/threads/jobs/{ref}/attempt/{n}/id-token` with `{"audience":"…"}` → `{"value":"<JWT>"}`. 10 requests/minute/attempt (`429` + `Retry-After`). |
| claims | `iss, sub, aud, exp, iat, nbf, jti, repository, repository_owner, ref, sha, workflow, job, run_id, run_attempt, event_name`; RS256, 5-minute lifetime, `sub = repo:o/r:ref:<ref>`. |
| gate | trusted run **and** effective `id-token: write`; `write-all`/`all` do not grant it (explicit-only, fail closed); fork runs never get a token and cannot exchange one. A terminal job or superseded attempt refuses. |

The private key is never in a page, an API answer, a job payload or a log line; the request token is
delivered by environment-name only (the value is in the docker client's environment), never in
`docker` argv. The bucket is the trust root for the key, which is stated in the plan and in the user
guide.

### 2.7 As implemented (2026-11-23) — deployment environments

Full design: [`docs/PLAN-THREADS-ENVIRONMENTS.md`](../PLAN-THREADS-ENVIRONMENTS.md). Summary:

| Concern | As built |
|---|---|
| workflow | `jobs.<key>.environment:` = name or `{name, url}`; name `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`, url absolute http(s). A job without one is unchanged (zero record reads). |
| record | `repos/<o>/<r>/threads/environments/<name>.json` = `{name, required_reviewers[], secrets[], created_by, created_at, updated_at, version}` (CAS; the record is the commit point). |
| reviewers | validated at save: at least **write** in the repo (repo role or instance admin), deduped/lowercased/sorted, ≤ 10. An unresolvable name is `400` naming it. |
| gate | a run with a job naming a reviewed environment is `pending_approval` with `approval_kind: environment` and `environments: [...]`; jobs written, checks `pending`, **nothing enqueued**. A listed reviewer (write+) or a repo admin approves/denies; a second decision is `409`; no reviewer ⇒ pending forever (never auto-approves). |
| decide verbs | approve: header CAS to `queued` + per-job deployment `approved` (who/when) + enqueue; deny: deployment jobs fail with `deployment to environment "x" denied by <who>`, other jobs continue. Re-run re-applies the gate and clears the stamps. |
| secrets | `repos/<o>/<r>/threads/environments/<name>/secrets/<S>.json` — the repository envelope (AES-256-GCM) with AAD bound to (repo, environment, name). The record's `secrets` list is the allowlist, so resolution is exact-key GETs, never a LIST. Injected only into a trusted job that names the environment **and** whose deployment is approved/not-required; **override repository secrets by name**; both sets masked. |
| fork | never trusted ⇒ no environment secrets; a fork actor holds no write role ⇒ cannot be listed and cannot self-approve; a reviewed environment on a fork run stays pending (or fails closed). |
| deployment record | on the **job** (`deployment`: environment, url, state `not_required|pending|approved|denied|failed`, approved_by/at, denied_by/at) — no new family, deleted with the job. |
| surfaces | Settings → Environments (maintain; a new `settings_repo_environments.html` partial + one dispatch line), the run page (environment, state, decider, time, URL — never a secret), and both JSON lanes (`…/threads/environments[/{name}[/secrets[/{secret}]]]`). |
| audit | `environment_create|update|delete`, `environment_secret_set|delete`, `environment_approve|deny`, target `environment:<name>`, never a value. |
| config | none: availability follows `server.secrets_key` / `server.auth.session_secret`; without one the secret endpoints `503` and the record still works. |

## 3. Runner protocol (our own, documented)

JSON over HTTPS, cookie/token authenticated, small and versioned (`X-Spindle-Runner: 1`):

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/threads/runners/register` | one-time registration token → runner id + long-lived token |
| POST | `/api/v1/threads/runners/declare` | version + labels (`linux`, `docker`, `spindle-1`) |
| POST | `/api/v1/threads/runners/fetch` | long-poll (≤ 30 s) for a job matching the labels; returns 204 when idle |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/start` | attempt-scoped job token issued; marks running |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/log` | append a log frame (idempotent by sequence) |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/complete` | conclusion + step results + job outputs |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/cancel` | acknowledge cancellation |
| POST | `/api/v1/threads/jobs/{id}/attempt/{n}/artifacts` | commit an artifact manifest (files, sizes, retention-days) |
| PUT | `/api/v1/threads/jobs/{id}/attempt/{n}/artifacts/{name}/blobs/{i}` | upload one file (`?path=&size=`; the server computes the digest) |
| GET | `/api/v1/threads/jobs/{id}/attempt/{n}/artifacts/{name}` | read an artifact manifest of the **same run** |
| GET | `/api/v1/threads/jobs/{id}/attempt/{n}/artifacts/{name}/blobs/{i}` | read one file of the same run |

Identity is **per attempt**: a requeued job gets a new attempt number and token; writes carrying a
superseded attempt are rejected by identity, not by timestamp. Fetching *is* the heartbeat (the
long-poll), so there is no separate liveness ping.

### 3.1 As built (pass B1)

| Method + path | Auth | Body → answer |
|---|---|---|
| `POST /api/v1/threads/runners/register` | the registration token in the body | `{token, name?, labels?, capacity?}` → `201 {runner_id, token, labels}` (exactly once; 401 otherwise; no repository) |
| `POST /api/v1/threads/runners/declare` | `Bearer wrt_…` | `{version, labels, capacity}` → `200 {runner_id, state, labels, capacity, lease_seconds}` |
| `POST /api/v1/threads/runners/fetch?timeout=` | `Bearer wrt_…` | empty → `200` job payload (global claim), or `204` when the poll (≤ 30 s, default 20) expires |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/start` | `Bearer wrt_…` | empty → `200 {token, expires_at, repo, run_id, job_id, attempt}` |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/log` | `Bearer wjt_…` | `{seq, time, stream, kind, text, step?, name?, conclusion?, exit_code?, duration_ms?}` → `200 {seq, state, cancel, lease_until?}` |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/complete` | `Bearer wjt_…` | `{conclusion, steps[], outputs?}` → `200 {repo, run_id, job_id, state, run_state}` |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/cancel` | `Bearer wjt_…` | empty → `200 {state, run_state}` (the ack) |
| `POST /api/v1/threads/jobs/{ref}/attempt/{n}/artifacts` | `Bearer wjt_…` | `{name, files:[{path,size,sha256,index}], retention_days?}` → `201` artifact view (409 when the name exists in the run) |
| `PUT /api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/blobs/{i}?path=&size=` | `Bearer wjt_…` | the file bytes → `201 {index, size, sha256}` (the server's digest) |
| `GET /api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}` | `Bearer wjt_…` | → `200` manifest (same run only) |
| `GET /api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/blobs/{i}` | `Bearer wjt_…` | → `200` bytes + `X-Spindle-Artifact-Path` (an index outside the manifest is 404) |

`{ref}` is the **opaque job reference** the fetch payload carries (`job_ref`, `jr_` + base64url of
`owner/repo/run/job`): the protocol table's `{id}` cannot be a bare job id, because two runs may both
own a job called `build`. The path's ref and attempt must match the token's claims (defence in
depth). The fetch payload is self-contained: repo/run/job ids, attempt, commit, event, branch,
workflow, trust class, `spec`, the check context, `server_url` (the canonical origin the job exposes
as `GITHUB_SERVER_URL`/`GITHUB_API_URL`, PAR-111), `secrets` + `mask` (trusted runs only),
`secrets_denied`, the folded `needs_outputs` of a dependent job (§4.1), and the lease. Declaring
labels is how a runner says what it *is*; fetching is how it says it is *alive*; both are one write
per call at most.

**Operator surface (host admin).** `POST /api/v1/admin/runners/tokens` mints an instance
registration token (shown once) with the runner's **repository policy** (`allow_all`, or up to 50
`owner/name` / `owner/*` patterns), `GET /api/v1/admin/runners` lists the fleet (including each
runner's policy and fork trust class), `POST /api/v1/admin/runners/{id}/policy` changes one
registered runner's policy in place (audited as `runner_policy`; only `allow_all`/`repos` are
rewritten, a revoked runner is refused, and its current job keeps running), and
`DELETE /api/v1/admin/runners/{id}` revokes one; the repo
Settings → Runners tab is deleted. The policy is copied to the registry record at registration and
enforced by the claim filter, and the runner can never declare it; a record with no policy (written
before tenancy) is offered nothing, and a policy change is one audited edit on the fleet row. The
policy is tenancy,
not isolation: a runner host is still trusted with the code its policy lets reach it, and
`docs/threads.md` §8.5 is the deployment recipe for that boundary.
`GET/PUT/DELETE …/api/threads/secrets[/{name}]` is the
write-only secret store (the GET lists names/timestamps and `{enabled}`, never a value). Cancel and
rerun are `POST …/api/threads/{run}/cancel` and `…/rerun` (repo write), with `spindle threads
cancel|rerun OWNER/REPO RUN` as their CLI twins. Runner administration has no CLI twin: the CLI's
machine credentials cannot administer (AGENTS law 9), so `/admin/runners` is the only mint/list/revoke
surface (host-admin-gated; the page and the API share the gate). `/admin/runners` and Settings →
Secrets are the same service calls the JSON API uses.

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
  reach the daemon API and therefore the other containers, so it is for trusted repositories only:
  the caveat lives in the runner documentation next to `--capacity` (§4.1/§5), not in Settings
  (the runner registry records labels/state/last-seen, not the operator's declared capacity). Labels
  route the pools apart (`runs-on: trusted` vs `runs-on: untrusted`).
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
| `--server` | `SPINDLE_SERVER` | `http://127.0.0.1:8080` | spindle origin (explicit flags win; without one the runner uses the server `spindle auth login` recorded, then falls back to the default) |
| — | `SPINDLE_RUNNER_TOKEN` | — | the `wrt_` runner credential (wins over the file) |
| `--token-file` | `SPINDLE_RUNNER_TOKEN_FILE` | — | 0600 file holding the credential; `--register` writes it |
| `--register` | — | — | one-time `wrg_` registration token to consume, then serve |
| `--capacity` | `SPINDLE_RUNNER_CAPACITY` | `1` | concurrent jobs (one container, network and volume each) |
| `--labels` | `SPINDLE_RUNNER_LABELS` | `self-hosted,linux,docker` | declared labels, exact and comma-separated (matched against `runs-on` as a subset: the runner must declare every label the job names) |
| `--name` | `SPINDLE_RUNNER_NAME` | — | display name recorded at registration |
| `--docker` | `SPINDLE_RUNNER_DOCKER` | `docker` | the docker CLI binary |
| `--image` | `SPINDLE_RUNNER_IMAGE` | `alpine/git:latest` | job image when the workflow names none (must carry `git`: the checkout runs inside the container) |
| `--image-prefix` | `SPINDLE_RUNNER_IMAGE_PREFIX` | off | registry mirror prefix (`host[:port]/path`); host-less images are rewritten under it, Docker Hub's implicit `library/` is not inserted |
| `--image-prefix-all` | `SPINDLE_RUNNER_IMAGE_PREFIX_ALL` | off | also rewrite registry-named images, replacing their host (requires `--image-prefix`) |
| `--image-pull` | `SPINDLE_RUNNER_IMAGE_PULL` | `missing` | `docker create --pull=` on the job and every service (`missing\|always\|never`; `docker create --help` must list `--pull`) |
| `--image-require-digest` | `SPINDLE_RUNNER_IMAGE_REQUIRE_DIGEST` | off | refuse an unpinned image at plan time (job and services), naming the image |
| `--allow-host-docker` | `SPINDLE_RUNNER_ALLOW_HOST_DOCKER` | off | acknowledge a host-root-exposing daemon |
| `--allow-fork-runs` | `SPINDLE_RUNNER_ALLOW_FORK_RUNS` | off | execute fork pull-request code on this runner (disposable host only; the server never offers one otherwise) |
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
reach the host's 127.0.0.1). The credential is the **attempt token** (`wjt_`) resolved by the server
chain to an unprivileged `ci:<runner>` principal and supplied to git through an in-container
credential helper (`$SPINDLE_JOB_TOKEN`, passed as `docker exec --env NAME` so it never appears in
argv). Every job — trusted or fork — gets exactly this credential, but it is **not read-only by
construction**: the token's scope follows the job's `permissions:`, so a job that declares
`contents: write` receives a `push`-scoped token and can push (and `contents: read` — the default —
cannot: its push is refused at the server and lands no ref, which
`internal/e2e/threads_runtime_test.go` proves end to end). A fork PR run is still untrusted: no
secrets, and whatever its workflow says about permissions applies to the fork's branch only.

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
`github.*`, `matrix.*`, `inputs.*`, `env.*`, `steps.<id>.outcome`, `steps.<id>.conclusion`, `!`,
`==`, `!=`, `&&`, `||`, parentheses) and an unsupported expression **fails the step with the parse
error** rather than skipping it silently.
`${{ matrix.<axis> }}` is substituted in step names, scripts, `if`, env, shell and
working-directory (and in the concurrency group at expansion). `continue-on-error` on a step keeps
the job green but records the failure; a failed step without it skips the remaining steps (each
recorded `skipped`) and fails the job. `timeout-minutes` on a step terminates it by killing the job
container — the only reliable in-container kill — and the step fails with `timed out after …`; the
job's own `timeout-minutes` cancels the job (the terminal state reads **`cancelled`**, exactly like
an operator cancel, because the attempt's context is what expired).
**`uses:` is the built-in set, a repository-local action, or a `docker://` image.** The built-ins are
`actions/checkout`, `actions/upload-artifact` and `actions/download-artifact`; `./<path>` resolves an
action.yml at the run commit and `docker://image` runs that image (docs/threads.md §3.4a, §8 decision
of 2026-11-22). A remote marketplace action (`owner/repo@ref`) fails the step with "unsupported
action … there is no marketplace"; a job-level `uses` (reusable workflow) fails the job with a clear
message.

**Step `id:` (round 4, 2026-11-21).** A step may declare `id:` (GitHub-style: letters, digits, `_`
and `-`, not starting with a digit; unique within its job — a duplicate is that file's parse error
naming the job and the id). The runner records the step's raw `outcome`
(`success|failure|cancelled|skipped`) and its `conclusion` (the outcome after the step's
`continue-on-error`, i.e. a tolerated failure's conclusion is `success`), so a later step can write
`if: steps.lint.outcome == 'failure'`.

**Step and job outputs (2026-11-21).** Every step gets `SPINDLE_OUTPUT` (alias `GITHUB_OUTPUT`) at
`/tmp/spindle-output-<n>`; a step with an `id:` may append `name=value` or `name<<DELIM` heredoc
lines to it, and the runner reads the file after the step **succeeds** (a failed, cancelled or
skipped step contributes nothing). Parsing is strict (a bad line, an unterminated heredoc, an
invalid name, or a value over 64 KiB is a named step failure), duplicates keep the last value, and
the collected map is the `steps.<id>.outputs.<name>` context. A later step substitutes those values
in `name`/`run`/`if`/`shell`/`working-directory`/`env`/`with`; substitution is fail-closed (an
unknown step id or an unpublished output fails the step naming the reference), while the native
`if:` expression reads a known step's unpublished output as empty and an unknown id as an error.
A job's `outputs:` templates are evaluated at job end (only for a successful job; an unresolvable
template fails the job naming it) and reported on `complete`; the server validates the caps again
and stores them on the job record. A dependent job's payload carries `needs_outputs`, folded at
claim time from the dependency records (only successful dependencies contribute; a matrix
dependency is the union of its cells, last cell in job-id order winning a collision), so
`${{ needs.<job>.outputs.<name> }}` resolves in its steps. Caps: 50 outputs/step, 100/job,
64 KiB/value, 1 MiB/job. Outputs are **data, not masked**: the log line that echoes one is masked
by the existing frame masking, but the value itself crosses the job boundary verbatim.

**A job-level `if:` is evaluated by the server (round 4).** It is decided **before the job is
claimable**, with the same subset plus the job contexts that exist (`github.*`, `matrix.*`,
`inputs.*`, `env.*`, `needs.<job>.result`, and `success()`/`failure()`/`cancelled()` folded from the
dependency results). A job with no `needs:` is decided at creation (and again on approval/rerun): a
false condition writes the job `skipped` with the condition in `reason` and **never enqueues it**. A
dependent job is decided by the `needs:` cascade once every dependency is terminal, and the claim
probe repeats the decision if a cascade did not run. A false condition is `skipped` (neutral in the
run fold); an expression outside the subset **fails the job** with the error in `reason` (never a
silent skip). A non-empty `if:` that evaluates TRUE lets the job run even when a dependency failed
(`if: always()`, `if: needs.build.result == 'failure'`); with no `if:`, the default is strict — every
dependency must have succeeded. `continue-on-error: true` on a job does **not** rewrite its record: a
failing job stays `failure` and gains `tolerated: true`, the run conclusion follows the other jobs,
and fail-fast does not fire from it. The check mirror keeps the job's real state (`failure`), so a
required check on that context still blocks a merge — toleration is a run-level fold, not a check
result. `permissions` is applied to the **attempt token** (the job's actual credential, §5,
`internal/threads/permissions_test.go`); it does not scope a `GITHUB_TOKEN` because there is none.

**`needs:` ordering is enforced by the queue (round 2, 2026-11-21; skipped state round 3;
job-level `if:` round 4).** A job is claimable only when every `needs` entry is terminal; until then
its queue entry is skipped without consuming the probe's claim budget (so a dependent enqueued ahead
of its dependency never starves it). Once the dependencies are terminal the job's own `if:` decides
(§ the job-level `if:` note above): with no `if:` a dependency that failed, was cancelled or was
skipped terminalises the dependent as **skipped** (round 3: the state vocabulary gained `skipped`;
GitHub reads a dependent that never ran as skipped, and a genuinely cancelled job still reads
`cancelled`), wave by wave, so the skip is transitive and needs no runner poll; with an `if:` the
condition gets the final say. A dependency record that cannot be read skips the dependent (fail
closed, reason on the record). A `needs` on a matrix job means **every cell**, and
`needs.<job>.result` folds the cells (failure beats cancelled beats skipped beats success). The
runner knows nothing about ordering: `internal/threads/needs.go` owns the gate, the cascade runs at
the end of every terminal job transition, and a terminal job wakes the fetch hub so a dependent is
claimed immediately. The workflow parser rejects a `needs` name that is not a job (`ErrBadNeeds`).

**Logs, cancel, completion.** One frame writer per attempt: a single flusher goroutine drains one
typed record queue (output bytes, runner system lines, step/group/truncation markers), preserving the
order the job wrote them; stdout and stderr share one comparable writer, so `os/exec` gives both
descriptors one pipe and the child's write order is the log's order. Output splits at 32 KiB (the
server's cap is 256 KiB), `seq`s increase monotonically, every known secret value is masked (and step
error text too) and each frame is POSTed; a retry is idempotent by sequence, and a frame the server
rejects after three attempts is counted, stated in the next successful frame (or at close) and visible
as a sequence gap in the reader. A 25 s idle heartbeat frame keeps the claim lease alive and is the cancel channel: a log
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
can cancel exactly the siblings). A job `name` that itself names a matrix axis is filled per cell
and does not also gain the suffix (`name: cell-${{ matrix.v }}` renders `cell-a`), so the rendered
name and the `threads/<workflow>/<job>` check context agree. The product is capped at 256 cells (a
parse-time-looking error: `ErrUnprocessable`). `fail-fast` (default on) cancels the other cells when
one fails, through the same terminal path the operator's cancel uses. **Concurrency groups** are
enforced on the claim path:
`repos/<o>/<r>/threads/concurrency/<slug>-<hash>.json` names the run holding the group; a claim
CASes that record (one in-flight run per group per repo), gives its queue entry back when another
live run holds it, and retries on the next 2 s probe; a holding run's sibling jobs pass (a
workflow-level group belongs to the run), and `cancel-in-progress` cancels the holder through the
existing cancel path (the holder is a *running* job, so it is signalled and the group frees when its
runner acks — at most one heartbeat interval, 25 s). A terminal fold releases exactly the groups on
the run header, and an orphaned holder (a folded run that never released) is stolen by the next
claim. The group is a **template**, not literal text: `${{ github.ref }}`, `github.ref_name`,
`github.repository`, `github.workflow`, `github.event_name`, `github.sha`, `github.run_id`,
`matrix.<axis>` (job level; each cell its own group) and `inputs.<name>` expand at run creation
`internal/threads/workflow/template.go`, one expander for the parser, the server and the runner
(`internal/threads/runner/expr.go` aliases it). It fails closed: an unknown/undeclared reference is
that file's parse error naming the key and the supported set, and a supported reference with no
value for the run fails creation with `ErrUnprocessable` — never a literal or an empty key segment.

**Evidence.** `scripts/b2-manual-e2e.sh` (two jobs, checks green) and `internal/e2e/threads_runner_test.go`
(three scenarios: two-job success + SSE logs + checks; a failing step with a `require_checks` merge
blocked; a fork PR with no secrets, the base-branch workflow and a rejected push with the checkout
token) run a real runner against a live server. `scripts/b2-scale.sh 20 20` runs 20 capacity-1
runners against a 20-cell matrix: 20/20 jobs success, 20 unique queue sequences, all attempt 1,
zero leftover containers, ~7 s wall time on a 10-core host.

## 5. Security model (non-negotiable)

1. A job container is root inside its own namespace; with a shared DinD daemon, jobs on that daemon
   can see each other. Therefore **capacity 1** is the default, capacity > 1 is an explicit operator
   choice documented with the flag (`--capacity`, §4.1 — the runner declares it and the registry
   records it for the admin page; it is never a repository setting), and scale-out (many capacity-1
   runners) is the documented path to 20/100 concurrent jobs for untrusted code.
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
6. **A job image is code, and the server can gate it.** `threads.image_allow_prefixes` (literal
   `host[:port]/path` prefixes; empty = unrestricted) and `threads.image_require_digest`
   (`@sha256:` required) are enforced where a workflow becomes a job, over `container.image` and
   every `services.*.image`, for every trust class. A refusal is a failed job with a `reason`
   naming the image and the allowlist, a red check, and no secret material; an unevaluable policy
   refuses (fail closed). This is the control that closes the same-repository-PR image hole: that
   run reads its workflow from the PR head and receives the repository's secrets, so it could
   otherwise name any image. The runner's own `--image` is the operator's declaration and is not
   governed by the allowlist — pin it by digest in the runner's unit or container definition.
   A mirror an operator points the runner at (`--image-prefix`, `docs/threads.md` §8.6) is a
   **code-execution trust root** exactly like Docker Hub; the allowlist names upstream images, not
   the mirror, and digest pinning is the containment for a compromised mirror.
7. A job OIDC token is a second, narrower credential: it is issued only for a **trusted** run whose
   effective permissions say `id-token: write`, has a five-minute lifetime, is audience-scoped by
   the caller, and can only assert the identity of its own run/job. Fork runs never receive a request
   token and cannot exchange one. The signing key is bucket-resident (the bucket is the trust root)
   and its private material is never served. See §2.6 and `docs/PLAN-THREADS-OIDC.md`.

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
| `/admin/runners` | host admin: mint an instance registration token (copy-once panel), list name/id/labels/capacity/state/last seen/current job, revoke. Settings → Runners is deleted (2026-11-22). |
| Settings → Secrets | add/replace (name + password field), list `name / •••••• (set) / updated`, delete. When the keyring is absent the panel explains `server.secrets_key` instead of offering a form. |
| Run page | the job graph (PAR-191: dependency levels, matrix cells with their check and duration, `after` edges, blocked dependents), cancel run / re-run buttons (write role only — the affordance, with the service as the gate), per-job step results, one live-log panel per job (`data-threadlog` + URL; the island replays then tails), and the whole-run log download (`.txt` / `.zip`). |
| CLI | **`spindle runner serve`** only (pass B2: the runner process itself — flags/env in §4.1; `--register` bootstraps an instance credential after a browser mint, SIGTERM stops cleanly). There are no `token|list|revoke` verbs: the CLI exposes no admin commands (static machine credentials never administer; a user `wgt_` token re-derives its owner's admin verdict on the API). |
| JSON API | §3.1: the runner protocol plus the admin runner/secret routes, cancel and rerun, and the log window (`GET …/jobs/{job}/log`, SSE or `?accept=json`). |

The full v1 surface (live logs, cancel/rerun, runners, secrets, settings) is below.

- **Repository tab:** `Threads` (replaces the old "Actions" naming) — run list with commit, event,
  status, duration; a run page with jobs, live logs (SSE via the existing task stream), cancel and
  rerun.
- **Settings:** runners (register/revoke, labels, capacity, state, last seen — capacity is declared by
  the runner, never a repo setting), secrets (write-only). (Runners moved to `/admin/runners` on
  2026-11-22.) The fork-approval
  policy and the retention window are instance settings (`threads.approve_first_time_forks`,
  `threads.retention`, 11_config_cli.md §2), shown and edited by /admin/config with every other key — not
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

### 6.4 As implemented (2026-11-21) — artifacts on the page

The run page gains an **Artifacts** section between the run header and the job list (15 §2.5): one
row per artifact with the name as the download link (native `download`), its human size, file count,
age and expiry. It is links only — the page's one-primary rule is unchanged (the approve control
remains the only primary, and only on a held run). The rows compose the shared card/link/muted
classes and wrap at narrow widths; verified at 1440×900 and ~390×844 with
`scrollWidth === clientWidth` and no primary button inside the section.

| Where | What |
|---|---|
| Run page | the Artifacts section (above the jobs), ZIP download links |
| Download route | `GET /{o}/{r}/threads/{run}/artifacts/{name}` → `application/zip`, `Content-Disposition: attachment`, `Cache-Control: no-store`; the service resolves the repository read gate and expiry (a non-sentinel failure is a 503, an unknown/expired artifact a 404) |
| JSON API | `GET …/api/threads/{run}/artifacts` (`{artifacts: [...]}`); `artifacts` on `GET …/api/threads/{run}` |
| Config | `threads.max_artifact_bytes_per_run`, `threads.max_artifact_bytes_per_file` (the Configuration page's schema picks them up from the struct tags; defaults 2GiB/256MiB) |

## 7. Tests (definition of done)

- **Artifacts (2026-11-21):** `internal/threads/artifacts_test.go` (manifest record + key layout,
  name uniqueness, incomplete-upload refusal, per-file/per-run caps, retention clamping and the
  expiry index, the read permission matrix, server-computed digests and job-scoped reads, ZIP
  contents, run-delete and expiry GC reclaiming blobs), `internal/threads/runner/artifacts_test.go`
  (plan/`with` validation and named errors, glob expansion incl. `**`/directories, `if-no-files-found`
  semantics, caps, both tar streams), `internal/webui/threads_artifacts_test.go` (the page rows,
  links only, ZIP headers and bytes, the unwired 503) and `internal/e2e/threads_artifacts_test.go`
  (real containers: upload → `needs:` download asserting bytes; API list; page link; ZIP download;
  `threads-gc` after the window removes blobs and list; a fork PR run's artifact readable in the base
  repo and a 404 through the fork's lane).
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
  step timeout, frame ordering/masking/cancel, cleanup on success/admin/config-failure/panic/cancel/SIGTERM,
  backoff) plus `internal/e2e/threads_runner_test.go` (real runner + docker: two-job success with SSE
  logs and checks; a failing step with a `require_checks` merge blocked; a fork PR with no secrets, the
  base workflow and a rejected push). `scripts/b2-scale.sh` is the 15 §6 shape (20 runners × 20 jobs).
- **Job `if:` / `continue-on-error` and step `id:` (round 4):**
  `internal/threads/jobif_test.go` (a false condition skipped at creation and never queued with the
  reason on the record; `needs.<job>.result` picks the branch; a true condition runs after a failed
  dependency; an unsupported condition fails the job; the matrix cell context; a tolerated failure
  leaves the run green while the job stays `failure`+`tolerated`; fail-fast does not fire; approval
  and rerun re-decide), `jobif_internal_test.go` + `queue_gate_test.go` (nil/edge contexts, the need
  fold, the probe's own decision and its fail-closed reads), `expr_test.go` (`steps.<id>.outcome` /
  `.conclusion`, a known step's unset output, `needs.<job>.result`), `internal/threads/workflow/stepid_test.go`
  (accepted shapes, leading digit/dot/space rejected, a duplicate names job and id, the same id in two
  jobs is legal), `internal/threads/runner/stepids_test.go` (plan carries the id; a later step's `if:`
  branches on outcome and conclusion; the skipped remainder is recorded) and
  `internal/e2e/threads_jobsem_test.go` (a real runner: the `if: false` job never starts, the
  tolerated failure's record says so, the run is success).
- **Step and job outputs (2026-11-21):** `internal/threads/runner/outputs_test.go` (env-file parsing:
  `k=v`, heredoc, CRLF, duplicate last-wins, equals-in-value, blank lines/empty file; bad line,
  invalid name, empty delimiter, unterminated heredoc → named errors; per-step count and per-value
  caps; the collection argv; the substitution sites — name/run/`if`/env/working-directory/`with`;
  fail-closed references; a failed step's file never read; the per-job byte cap; job-output
  evaluation), `internal/threads/workflow/outputs_test.go` (parsed `outputs:`; unknown step id,
  unknown axis, unsupported context, bad name; a self/forward/unknown step reference is a parse
  error; the strict filler; the caps), `internal/threads/outputs_test.go` (the `if:` contexts,
  an unset output vs an unknown id, `needs` outputs, the matrix union/last-cell fold, a failed
  dependency exposing none, Complete storing outputs and materializing `needs_outputs` in the next
  payload, an oversized report refused, a rerun clearing outputs) and
  `internal/e2e/threads_outputs_test.go` (real containers: step A writes `$SPINDLE_OUTPUT`, step B
  gates on it and substitutes it, the job's `outputs:` reach a dependent job's `needs.<job>.outputs`
  assertion, a failed step's output is absent and its job fails closed, a secret-bearing output
  crosses the boundary verbatim while its log echo is masked).
- **Security matrix:** fork PR gets no secret and a read-only token; a job cannot read another
  repository's secrets; logs are masked; capacity-1 runners reject a second claim.
- **Rendered:** run page live logs at desktop and ~390 px; no page-level horizontal scroll.

**Operator approval (2026-11-27):** the six decisions recorded below — D-22 (user-scope secrets
declined), D-23 (workflow container credentials refused), D-24 (bash default shell), D-25 (`case`
removed from the expression acceptance), D-26 (digest-pinned stock images) and D-27 (SMTP notifier,
off by default) — are approved as implemented. The effect of declining each is recorded in
`21_parity_master.md` §1.12.

- **NEW (2026-11-27) — the expression function set is the platform's real set (D-25), and `case` is
  not a function.** The parity row's capability/acceptance named `case` alongside the real functions,
  but neither GitHub nor Forgejo defines `case()` (and this engine never did); inventing it would be
  semantics, not parity. The row is amended to success/failure/always/cancelled (canceled accepted),
  startsWith/endsWith/contains/format/join, toJSON/fromJSON and hashFiles, with one conformance case
  per function in `TestExpressionFunctionSetMatchesPlatform`. `case(...)` stays a named
  parse/evaluation error; `hashFiles`'s in-container walk remains PAR-041.

- **NEW (2026-11-27) — stock labels resolve to digest-pinned upstream images (D-26).** The catalogue
  (`threads.StockImages`) is public upstream images pinned by manifest digest (ubuntu 24.04, node 22,
  python 3.13, Go 1.25, Rust 1, docker 27-cli). A workflow that names a stock label and no
  `container.image` receives that pinned image in its job payload; a runner's own
  `label:docker://…` mapping still applies (PAR-171); with no mapping, an explicit
  `container.image` wins over the stock catalogue. The server never
  invents an image outside the catalogue, `spindle runner images --label` refuses an unknown label by
  name, and spindle still publishes/mirrors nothing (air-gapped hosts use `--image-prefix`).

- **NEW (2026-11-27) — failure email is a stdlib SMTP notifier, off by default (D-27).** The third
  notification channel uses `net/smtp` + `crypto/tls` only (no dependency) and is enabled by
  `threads.smtp.host`. It shares the inbox's failure-only event filter and rides the inbox record's
  deterministic key, so a message is sent only when that record is first created (one per subscriber
  per run). The body carries the same facts (workflow, branch, run link) and is masked with the run's
  job mask values; if the mask list cannot be resolved the email is skipped rather than sent
  unmasked. Credentials are refused over a plaintext connection. The config keys and bounds are in
  `docs/go/11_config_cli.md`; the operator-facing description is `docs/threads.md` §9.1a.

- **NEW (2026-11-27) — the default step shell is bash (D-24), and a log frame is a record.**
  (a) **Default shell.** A workflow written for GitHub or Forgejo runs a step with no `shell:` under
  bash on Linux; spindle documented `sh -e`. The corpus execution tier proved the cost: the real
  `weave-publish-github` workflow uses a `<<<` here-string and could not execute, so the "runs
  unmodified after conversion" acceptance line was unreachable. The default is now
  `bash --noprofile --norc -eo pipefail -c`, with `shell: sh` still available, and an image without
  bash falls back to `sh -e` with one log line (the same fallback an authored `shell: bash` always
  had). (b) **Literal-block env values travel verbatim.** The planner flattened authored `env:` values
  to one line, which turned the corpus's multi-line `CHECKOUT: |` script into a broken `git`
  invocation. An env value is data; only HTTP header values are single-line. (c) **`Authorization:
  token` is Bearer.** Forgejo/GitHub spell a CI token that way, and the corpus fetch idiom uses it;
  it is the same credential, not a second authentication path. (d) **A log frame is a record.**
  `{time, stream, kind, text, …}` lets the reader render step boundaries and `::group::` folds and
  name a sequence gap; a bare text chunk made the page guess and let a dropped frame disappear.
  (e) **A non-root job gets a writable HOME** (`/tmp/spindle-home`): docker's default `HOME=/` made
  `git config --global` fail for every corpus workflow, and an authored `env: HOME` still wins.

- **NEW (2026-11-23) — deployment environments reuse the approval state machine, run-scoped.**
  (a) **One hold, not a second state machine.** A run naming a reviewed environment is created in the
  existing `pending_approval` state; `approval_kind`/`environments` make the hold name the environment
  (page, API, checks description) and the decide verbs branch on the kind. A per-job deployment queue
  would duplicate the CAS/enqueue/fold machinery for one page and one button. Consequence, written
  down: a run with two environment jobs holds for the **intersection** of their reviewer sets
  (a repo admin always may). (b) **Environment hold outranks the fork hold.** Both are one hold; the
  stricter rule (a named reviewer set) wins, so a fork run naming a reviewed environment waits for
  that environment's reviewers instead of the fork gate's generic maintainer — and it can never
  approve itself, because a fork actor has no write role. (c) **Deny fails the deployment, not the
  run.** Cancelling the whole run erased the build that produced the artifact; the deployment jobs
  fail with a named reason and the rest continue. (d) **The record's `secrets` list is an allowlist.**
  It makes resolution exact-key GETs (no LIST on a claim path) and gives "listed but absent" a
  defined, safe meaning (skip) instead of an empty secret. (e) **A separate AAD, not a repository
  secret under another name.** Binding (repo, environment, name) means a ciphertext cannot be
  replayed as a repository secret or under another environment, and no read path can confuse the two
  families. (f) **Maintain floor, not admin.** A maintainer is the role that ships; pushing
  environment CRUD to admin-only would make every deployment wait for an admin to add a reviewer.
  (g) **Fail closed on an undefined environment** rather than treating an unknown name as "no
  deployment": a typo must be visible, not a green run that deployed nowhere. (h) **No config key** —
  secrets availability is the existing sealing-key rule.

- **NEW (2026-11-23) — job OIDC tokens: instance-signed, bucket-keyed, explicit `id-token: write`
  only.** (a) **The issuer is `server.public_url`, not a new key.** It is already the operator's
  declaration of the externally reachable origin (the checkout uses it), and adding a second URL
  setting would let a token's `iss` disagree with the address a provider fetched discovery from.
  The request-origin fallback exists for development and is documented as unsuitable behind a proxy.
  (b) **One instance key in the bucket, CAS-created and auto-rotated.** Per-repository keys would
  multiply objects and rotation races while every key still lives in the same trust root; the claims
  carry the repository. The bucket-is-a-trust-root consequence is written down rather than papered
  over. (c) **`write-all` does not grant `id-token: write`.** An OIDC token can become a cloud
  identity, so it is granted only by its own key — a fail-closed deviation from GitHub's shorthand
  semantics (the existing `all` handling only widens `contents`). (d) **A separate hash-only request
  token, not the job token.** The job token authorises log/complete/cancel and (with
  `contents: write`) push; handing that broader credential to the exchange would make every exchange
  a potential control-plane write. The request token is bound to one attempt, stored hash-only,
  expires with the job and is rate-limited per attempt. (e) **Bucket-backed rate window.** The
  counter rides the same CAS that verifies the token, so it is correct across instances and needs no
  process state; the only cost is one extra write per exchange on a path that is already rare. The
  known imprecision (a racing pair can overshoot) is documented. (f) **No `jti` replay registry.** A
  provider enforces `aud`/`exp`; a replay of the *request token* mints a fresh token and is bounded
  by the rate limit, and there is no per-token deny list in v1. (g) **GitHub spelling aliases are
  provided deliberately** (`ACTIONS_ID_TOKEN_REQUEST_*` alongside `SPINDLE_*`, same values): Threads
  executes GitHub-shaped workflows, so a step that already performs GitHub's OIDC dance works
  unchanged. (h) **The token is delivered by environment-name, never argv**, matching the job
  token/secrets rule; a `docker://` action receives neither the token nor the URL. The runner masks
  it in its own log stream (best effort, the documented masking caveat applies).

- **NEW (2026-11-22) — Actions beyond `checkout`: local composite actions and `docker://` images,
  resolved at run creation; remote actions are deliberately deferred.**
  (a) **Resolution happens at run creation, from the run commit, and the definitions are embedded in
  the job payload** (`Spec.Actions`, keyed by repository-relative directory). This is the only place
  that satisfies both requirements at once: the image gate must see every image an action names
  *before anything is enqueued* (a runner-side resolution would start the job container before the
  gate), and the run must execute the definition at its own commit (the runner has no repository
  reader, so it cannot fetch another ref). The run page expands the same definitions for display
  (`Job.Steps` carries `parent`), so what the page shows and what the runner executes come from one
  resolution. (b) **An earlier step editing the action file changes nothing for this run.** Because
  the definition is pinned at creation, `SPINDLE` executes the pinned `run:` scripts and metadata;
  the next run at a new commit reads the new file. The scripts are still repository code with the
  same trust as a workflow script step (job token + repository secrets), which is documented rather
  than sandboxed: a compromised action file is a compromised job. (c) **Composite expansion is a
  tree, executed in place.** The plan keeps the calling step as one node with its expanded steps,
  because failure/`if:`/`continue-on-error`/outputs semantics are then the same recursion as a job's
  step list; the display list is flattened with the caller's name as the parent. Inputs become
  `INPUT_*` (uppercase, spaces→underscores), the caller's `env:` applies to every child, `with:` keys
  are validated against the declaration (a typo is refused, the built-in actions' rule), and
  `steps.<id>` inside the action is its own namespace. (d) **Depth 10 and cycle detection are
  enforced twice** — at creation (the job fails with a reason naming the chain; nothing is written
  for the runner) and in the runner's expansion (a hostile payload must not loop it). (e) **Docker
  actions are their own containers on the job network**, with the workspace volume at the same path,
  no host socket/privileged/capabilities/mounts, and **no token or secret environment** — the image
  is third-party code, so the fork invariant (read-only, no secrets) is structural rather than a
  property of the run's trust class. A composite's `run:` steps keep the normal run-step environment
  (they are repository code, like a script). (f) **The image gate is one gate.** `checkJobImages`
  walks inline `docker://` steps, local docker actions and docker steps nested in composites and
  calls the same `checkImage`; the reason names the step (and the nested chain) and the image, and
  the job fails before enqueue. The runner applies the same `--image-prefix`/`--image-pull`/
  `--image-require-digest` at plan time (before the network or any container). (g) **Remote
  marketplace actions stay refused, with the reason stated**: fetching and trusting an arbitrary
  repository is a separate security decision (pinning, mirroring, review, revocation, supply-chain
  policy) and this slice does not pretend to make it. An unknown `uses:` keeps the "there is no
  marketplace" step error. (h) **Docker action outputs are copied out of the action container** (the
  workspace volume is shared; `/tmp` is not), so a docker action can publish outputs without making
  the job container read another container's filesystem. (i) **`runs.outputs` is composite-only**:
  a docker action has no steps to read output templates from, so the declaration is a parse error
  rather than an empty value at run time.
- **NEW (2026-11-21) — `on.pull_request.types` is parsed, validated and enforced (the audit's
  accepted-and-ignored filter).** (a) **The vocabulary is the emitted set, and it is closed.** The
  pulls service emits exactly `opened`, `synchronize` and `reopened` through its `PullEvents` seam;
  a close is a notify/SSE transition only, and no label event exists (`internal/pulls`). So
  `types: [closed]` and `types: [labeled]` are **rejected at parse time** rather than emitted:
  making close a CI edge would let a closing PR start work (and would silently widen every existing
  bare `pull_request:` workflow to fire on close), which is the opposite of the documented "a close
  never starts work" rule. The per-file error names the supported set, so the workflow author sees
  why, and one bad file still disables only itself. (b) **`types: []` is a parse error.** An empty
  filter matches nothing; the parser's standing rule is that a workflow which can never fire is a
  typo, not a feature (the same reason cron is validated), and the error tells the author to omit
  the key. (c) **An absent `types` keeps the old default: all three emitted actions.** An absent or
  null filter is not a filter. (d) **The matcher takes the action, and the action is required.**
  `Workflow.Fires(event, branch, action)` and `SelectFiles` filter after the branch match, and
  `CreateRun` rejects a pull_request whose `Action` is missing or outside the set (`ErrInvalid`
  naming the vocabulary) instead of quietly writing a run no `types` filter could ever match. The
  fork/trust rules are untouched: the action only selects workflow files; a fork PR still reads the
  base-branch file untrusted with no secrets. Pinned by
  `internal/threads/workflow/prtypes_test.go` (every type × every emitted action, the default, the
  rejected spellings) and `internal/threads/prtypes_test.go` (run creation filtered per action, the
  per-file unsupported-type error, the rejected callers).
- **NEW (2026-11-21) — Threads round-2 audit: concurrency groups are templates that fail closed,
  and a matrix job name renders its cell.**
  (a) **A group expression resolves; it never degrades to literal text.** `${{ github.ref }}` in
  `concurrency.group` used to stay literal, so every branch hashed to one repository-wide key and
  `cancel-in-progress: true` let a feature-branch push cancel `main`'s run. The supported set is
  closed — `github.ref`, `github.ref_name`, `github.repository`, `github.workflow`,
  `github.event_name`, `github.sha`, `github.run_id`, `matrix.<axis>` (job level only: a
  workflow-level group expands once per run, before any matrix) and `inputs.<name>`; there is no
  `spindle.*` expression context. The scanner/expander lives in
  `internal/threads/workflow/template.go` (the package `threads` may import), wrapped by
  `internal/threads/group.go`, aliased by `internal/threads/runner/expr.go`; the step substitutions
  (`FillContext`) now ride the same scanner, so one `${{ matrix.go }}` means one thing everywhere.
  (b) **Fail closed at both moments.** A reference that cannot exist at all (unknown context or
  `github.<field>`, an undeclared matrix axis or dispatch input, an operator, an unterminated span)
  is the FILE's parse error, reported in the workflow list with the key and the supported set — the
  house rule for every unsupported key, and it keeps a typo from becoming a repository-wide lock. A
  supported reference with no value for THIS run (an optional dispatch input with no default on a
  push) fails run creation with `ErrUnprocessable`; only a declared `default` is used outside a
  dispatch. Both are better than a silent literal, which is a correctness bug in a lock.
  (c) **A job name is filled, not validated.** `name` is display-only, so `FillTemplate` replaces
  what it can and leaves an unresolved reference visible instead of failing an otherwise runnable
  job. A name that names a matrix axis already distinguishes its cells, so the `(v, …)` suffix is
  not appended (GitHub's rule): `name: cell-${{ matrix.v }}` renders `cell-a`, and the check context
  `threads/<workflow>/cell-a` agrees with the rendered name. Before this the name rendered
  `cell-${{ matrix.v }} (a)`.
- **NEW (2026-11-21) — The checkout URL prefers `server.public_url`, and a checkout failure is
  named.**
  (a) **`server.public_url` wins over the protocol request's origin for the checkout base.** The
  request origin is whatever the runner called, which may be a browser-only canonical host
  (`walgit.localhost:<port>`) that does not resolve inside a job container; the configured public
  URL is the operator's declaration of an address containers can reach. Empty keeps the request
  origin (unchanged behaviour, and reverse proxies still work through `X-Forwarded-Proto`); a value
  that is not an absolute http(s) URL is ignored rather than poisoning the clone URL. The runner
  never rewrites a non-loopback name to a guessed IP — only the existing loopback →
  `host.docker.internal` rule survives, and both ends of that rewrite are stated in the log.
  (b) **A checkout failure is named, not left as `exit 128`.** The generated checkout script keeps
  git's own output (the evidence) and adds one diagnostic naming the URL tried, the in-container
  reachability requirement and the setting that fixes it, with an extra paragraph when git reported
  a name/connection error. The runner also states the resolved base once at job start. Both are
  script/plan text, so the runner image needs nothing new; the unit tests execute the generated
  script under a real `sh` with a stub git, and the e2e drives a real container against an
  unresolvable `server.public_url`.

- **NEW (2026-11-21) — The runtime image ships the docker CLI for `spindle runner serve`.**
  The docker CLI goes into the ONE runtime image instead of a second runner image. The server and
  the runner are the same binary, so a separate image would duplicate the whole build for one ~30 MB
  CLI and add a second tag to publish, mirror and pin; the CLI is inert for a server-only
  deployment. Nonroot is unchanged — the socket is granted at `docker run --group-add`, and
  `--allow-host-docker` is still required for the host socket. The container contract runs
  `docker --version` inside the published image, so the runner path cannot regress silently.

- **NEW (2026-11-21) — Threads round 4: job-level `if:` / `continue-on-error` applied, step `id:`.**
  (a) **The job `if:` is decided by the server, at the earliest point its contexts are facts.** A
  job with no `needs:` is decided at run creation (and again on approval/rerun) — nothing can change
  its answer, so a false condition writes `skipped` with the condition in `reason` and the job is
  **never enqueued**; a dependent is decided by the `needs:` cascade once every dependency is
  terminal (`needs.<job>.result` is then a fact, folded over a matrix dependency's cells), and the
  claim probe repeats the decision so a cascade that did not run cannot leak a job. Doing it in the
  runner would have made a false job wait for a poll and would have hidden the `needs` result from
  the decision; doing it only at creation would have evaluated `needs.*` before it had a value. The
  evaluator moved to `internal/threads/expr.go` (package `threads`) so the server and the runner
  share one parser — `internal/threads/runner/expr.go` is now a type/function alias. A non-empty
  `if:` that is true runs the job even after a failed dependency; with no `if:` the default stays
  strict, which is GitHub's rule and keeps every existing workflow's meaning.
  (b) **A tolerated failure is marked, never rewritten.** `continue-on-error: true` on a job leaves
  its record `failure` and sets `tolerated: true`; `foldRunState` ignores a tolerated failure and
  `Complete` skips fail-fast for it. Rewriting the state to `success` would erase the distinction the
  operator needs; changing the *check* mirror would let a required check on that context pass on a
  failed job, so the mirror keeps the real state and only the run fold tolerates it.
  (c) **A step `id:` is accepted and addressable.** The parser validates the GitHub identifier shape
  and rejects a duplicate within a job (case-insensitively, because `steps.<id>` lookups are) as that
  file's parse error naming the job and the id. The runner records `outcome` (raw) and `conclusion`
  (after `continue-on-error`) per id, which is what `if: steps.x.outcome == 'failure'` reads; a
  skipped remainder is recorded under its id too. **`steps.<id>.outputs.*` parses and reads empty**:
  v1 has no step-output capture (no `$GITHUB_OUTPUT` parsing), and resolving an unknown context to an
  error would fail real workflows that merely *inspect* an output, so the documented limitation is an
  empty value (`needs.<job>.outputs` behaves the same). **Superseded 2026-11-21 by the outputs
  decision below:** outputs are captured (`$SPINDLE_OUTPUT`), field substitution is fail-closed, and
  only the native `if:` keeps the "known step, unset output reads empty" rule.
  (d) **`spindle auth login` state backs the runner's server resolution.** `runner serve` calls the
  same `resolveServer` as every other CLI command (flag → `SPINDLE_SERVER` → login state → default),
  so §4.1 records the fallback instead of claiming the state is unused. (e) **Capacity is a runner
  flag, not a registry field.** The registry records labels/state/version/last-seen; the earlier
  claims that Settings shows a capacity caveat were wrong and are removed (the caveat lives with
  `--capacity` in §4.1/§5). (f) **A job timeout reads `cancelled`, a step timeout reads `timed out`.**
  The job context's deadline cancels the attempt exactly like an operator cancel; only the step's own
  deadline produces `timed out after …` (it kills the container). The earlier doc said both reported
  `timed out`; the code is unchanged and the doc now matches it. (g) **The CLI covers cancel/rerun.**
  `spindle threads cancel|rerun` call the served API verbs (`POST …/threads/{run}/cancel|rerun`); the
  run body carries no per-job state in the answer — the page/API read is how a caller polls it.
  (h) **Every checkout is not read-only.** The attempt token's scopes are the job's `permissions:`
  (`contents: read` default, `contents: write` grants `push`), and a job token reads its own
  repository past the identity gate (`gateRepoRead`), so private-repository checkouts work. The two
  earlier claims (read-only by construction; the private-repo deferral) are corrected in place,
  with the e2e that proves them named in §4.1.
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
  `wct_` shape) to an unprivileged `ci:<runner>` principal whose scopes are the job's:
  `permissions: contents: read` (the default) yields a read-only token and a push is refused at the
  server; `contents: write` yields a `push`-scoped token, so the checkout is **not** read-only by
  construction (the e2e pushes with it, `internal/e2e/threads_runtime_test.go`). The runner bearer
  (`wrt_`) is claimed the same way, shape-only, because a token-mode or oidc-mode chain would
  otherwise 401 every protocol call before the runner handler ran; the one call that carries no
  bearer (`runners/register`, a body token) is the single runner-protocol path exempt from the oidc
  anonymous-write assert. **Private repositories work:** `gateRepoRead` (`internal/server/bind_api.go`)
  grants a job token read on exactly its own repository before the identity gate, whatever the
  `ci:<runner>` principal's membership (a job token is repository-scoped by construction and never
  reaches another repo).
  (c) **The runner owns the job container's lifetime** (`--entrypoint /bin/sh` + a keep-alive
  command), as GitHub's runner does; a workflow `container.entrypoint`/`command` is carried but not
  applied, while `env`/`ports`/`volumes`/`options` are. Resource limits therefore come from the
  runner/daemon, not the workflow (the §4 bullet "the workflow may lower them" is deferred with the
  same reason: there is no per-job limit surface yet). (d) **Service ports are published on loopback
  ephemeral host ports** in addition to the job network alias: it is the only readiness probe that
  needs no tooling inside the image. One `services.<id>` container, its alias and its ports per job,
  torn down with the job. There is no workflow healthcheck field in v1; an image `HEALTHCHECK` is
  honoured when present. (e) **Step timeouts kill the job container.** `docker exec` cannot kill the
  in-container process tree portably, so a step that exceeds its `timeout-minutes` terminates the
  container and the step fails with `timed out after …`, which is also the only honest way to stop a
  wedged process. The job's own `timeout-minutes` is the attempt context's deadline: it cancels the
  job (containers removed, remaining steps recorded `cancelled`) and the job reads `cancelled`, the
  same terminal state an operator cancel produces — a consumer that wants "timed out" to be
  distinguishable reads the step result. (f)
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
  (a) **Runners are repository-scoped** (superseded 2026-11-22 — see the instance-runner decision
  below). The registration token is minted in the repository's Settings → Runners and the runner
  only ever sees that repository's queue. An instance-wide runner would need a global dispatch index
  over every repository, which is exactly the repo-wide LIST the claim path forbids; the per-repo
  head keeps the claim at one probe and one CAS. Labels route *pools*, not tenants. (b) **The queue entry is the truth, `head.json` is a bounded dispatch
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
  (≥ 32 bytes when set) and is masked in /admin/config like every other secret. (g) **Cancel reaches a busy
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
- **NEW (2026-11-21) — run artifacts: built-in actions, a manifest commit point, and the decisions
  behind them.** (a) **Two built-in actions, not a marketplace.** `actions/upload-artifact` and
  `actions/download-artifact` are runner-side built-ins; a `with:` key outside the documented set is
  a **named step error**, because a silently ignored `retention_day` typo is a workflow that looks
  right and behaves differently. (b) **The runner is the transport, the container is not a client.**
  The container is listed with `find` and moved with `tar` through `docker exec`; the runner PUTs
  bytes over the existing authenticated job channel with the attempt token. No presigned URL, no new
  credential, no bucket network path from untrusted code, no second protocol. (c) **The manifest is
  the commit point, and a name is unique per run.** Blobs are immutable and keyed with the attempt;
  the manifest is `PutCreate`. A second upload of one name is a `409` and fails the step rather than
  replacing what an earlier job published (a replace would let a later job rewrite the input an
  earlier job was trusted with). (d) **The server computes digests.** A client-supplied SHA-256 is
  never trusted; the server hashes while receiving and finalize verifies the uploaded set (one
  prefix LIST) so the manifest cannot point at a missing/short blob. (e) **Retention is the run's,
  and `retention-days` only shortens it.** Expiry is `min(run expiry, created + days)` on read, so a
  longer value cannot extend; a shortening gets one repo-level expiry-index row, reclaimed by one
  prefix LIST in the retention pass (never a per-run enumeration on a request path). The read path
  treats an expired artifact as absent immediately, so GC lag is never a data leak. (f) **Same-run
  only.** `download-artifact` cannot name another run/workflow: the attempt token's new
  `ScopeArtifact` is scoped to its own run, and the reader route resolves the run under the URL's
  repository, so artifacts can never act as a cross-repo read (a fork run's artifacts live in the base
  repository, like its logs). (g) **Contents are the user's data.** Masking stops at the step's log
  line (including the name echoed there); spindle does not rewrite uploaded bytes — documented
  plainly so nobody assumes a secret written into a file is scrubbed. (h) **A ZIP for the browser,
  per-file blobs for the runner.** `archive/zip` (stdlib) streams the download with
  `Content-Disposition`; the runner reads the manifest's blobs directly so it never needs `unzip` in
  the container (only `tar` and `find`). (i) **The artifact caps are config, not constants**
  (`threads.max_artifact_bytes_per_run` / `_per_file`, plus a fixed 1000-file cap), advertised in
  the start answer so the runner refuses over-cap input before streaming it; every refusal names the
  cap and its key.
- **NEW (2026-11-21) — the API lane does not gzip `application/octet-stream`.** The artifact blob
  route surfaced a general bug: the api-lane gzip wrapper compressed every response, including opaque
  binary streams, deleting `Content-Length` and handing a runner chunked bytes it could not verify
  against the manifest. `compressWriter` now bypasses `application/octet-stream` alongside SSE,
  images and precompressed assets (18 `internal/server` tests unchanged; the e2e proves the runner
  sees the recorded length).
- **NEW (2026-11-21) — step and job outputs are the GitHub file mechanism, fail closed, and are
  data (not masked).** (a) **`$SPINDLE_OUTPUT`, GitHub's format.** Every step gets
  `SPINDLE_OUTPUT` (alias `GITHUB_OUTPUT`) at `/tmp/spindle-output-<n>` inside the job container —
  not the workspace, so a `git clean` cannot erase another step's file and the checkout gains no
  untracked path. `name=value` and `name<<DELIM` heredocs are parsed exactly as GitHub documents;
  anything else is a named step failure (a silently dropped output is the bug this feature exists to
  remove). Duplicates keep the last value. (b) **Only a successful, identified step contributes.**
  The runner reads the file after the step succeeds, and only when the step declared an `id:`
  (an anonymous step has no `steps.<id>` key). A failed/cancelled/skipped step's file is never read,
  so its outputs are unset rather than empty-but-present. (c) **Substitution fails closed; the
  native `if:` reads empty for a known step.** `${{ steps.x.outputs.y }}` / `${{ needs.j.outputs.y }}`
  in name/run/`if`/shell/working-directory/env/`with` is strict: an unknown id/job or an
  unpublished name fails the step naming the reference, because an empty substitution silently
  changes what a script does. Inside a native `if:` expression a known step's unpublished output
  reads empty (so `== ''` checks keep working) and an unknown id is an error — the two positions
  differ because one is data flow and the other is a predicate. (d) **Job outputs are evaluated by
  the runner, stored by the server, and never reported by a failed job.** A job's `outputs:`
  templates resolve against its collected step outputs at job end; an unresolvable template fails
  the job (named), and the server ignores an output map from a non-successful conclusion and
  re-validates the caps before storing. (e) **A matrix dependency exposes the union, last cell
  in job-id order.** GitHub's matrix-outputs rule is run-order dependent; spindle's fold is
  deterministic: `needs.<job>.result` is the worst cell, `needs.<job>.outputs.<name>` is the union
  of the successful cells with the last cell (job-id order) winning a collision, and any non-success
  cell means the dependency exposes nothing at all. (f) **The dependent payload carries the folded
  map.** The job spec is written at run creation, before a dependency can finish, so `needs_outputs`
  is materialized at claim time (the same fold the server's job-`if:` uses) and the runner substitutes
  it. (g) **The caps are fixed constants, not config.** 50 outputs/step, 100/job, 64 KiB/value,
  1 MiB/job, named refusals at parse and at Complete: a runaway job must not grow a job record
  without bound. (h) **Outputs are data; masking stays on log lines.** A value written into an
  output is stored and substituted verbatim (a secret in an output reaches every dependent job), so
  the docs state GitHub's caveat plainly; the log line that echoes it is still masked by the
  existing frame masking, which the e2e pins. Putting a secret in an output is the workflow's
  mistake, not a masking promise.
- **NEW (2026-11-22) — an image source is a code-execution trust root, and the runner can be
  pointed at a mirror.** (a) **The trust statement is documented, not softened.** Whatever bytes a
  job image's tag resolves to run as the workflow with the repository's secrets in the environment,
  so a mirror (an OCI registry proxy, e.g. Artifact Keeper) is exactly as powerful as Docker Hub.
  `docs/threads.md` §8.6 carries the L0 recipe (`daemon.json` `registry-mirrors`, the mirror's
  Docker Hub Remote repository with `oci_trusted_bearer_realms`, `AK_DEFAULT_DOCKER_MIRROR_REPO`,
  TLS/`insecure-registries`) and states plainly that a compromised mirror means arbitrary code as
  the runner's daemon. Nothing in spindle trusts the mirror or proxies to it: the integration is
  documentation plus optional runner flags. (b) **L1 is flag-only.** `--image-prefix`,
  `--image-prefix-all`, `--image-pull` and `--image-require-digest` change the plan and the docker
  argv, never a server record, so they are unit-testable without a daemon (`plan_test.go` plus
  `image_test.go`). A reference with no registry (docker's rule: first segment without `.`/`:`, not
  `localhost`) is a Docker Hub reference and moves under the prefix; a registry-named reference is
  untouched unless `--image-prefix-all`, which **replaces** the host (the original host is not
  carried in the path, so one mirror repository per upstream is the operator's choice of prefix).
  (c) **Docker Hub's implicit namespace is not rewritten.** `golang:1.27` →
  `<prefix>/golang:1.27`, never `<prefix>/library/golang:1.27`: docker (and the mirror's Hub proxy)
  apply `library/` themselves on the single-segment spelling, and inserting it here would target a
  path a proxy repository does not serve. This is asserted in the rewrite matrix, including the
  explicit `docker.io/library/golang` case. (d) **`--pull=` is always in the argv**, with `missing`
  as the default spelling of docker's own behaviour: the effective policy is then one observable
  argv instead of an absent flag, and `always` is the only way a moved tag reaches CI (`never` is
  the audit shape). It requires a docker CLI that accepts `docker create --pull` (19.09+; the docs
  pin 23+). (e) **A create failure is named.** Three classes — unreachable (DNS/connection/TLS),
  401/credential refused, 404/manifest unknown — each print a `spindle:` diagnostic after docker's
  own text, carrying the attempted reference, the mirror rewrite, the fallback state (with a prefix
  there is none, by design) and the fix, exactly the shape of the checkout diagnostic (§4.4). A
  non-image create failure (a bad mount) gets no image diagnosis: the classifier only speaks when
  docker's text matches a class. (f) **Credentials are never argv.** A mirror token lives in a 0600
  `$DOCKER_CONFIG` file in the runner's environment; the plan asserts no flag the policy adds can
  carry one, and the job container never sees it (the pull happens in the daemon).
- **NEW (2026-11-22) — `threads.image_allow_prefixes` + `threads.image_require_digest`: the
  server-side image gate (L2 of `PLAN-ARTIFACT-KEEPER.md`).** (a) **Where.** `createOne` applies it
  to the final job spec — `container.image` **and** every `services.<id>.image` — before anything
  is enqueued, so it covers every trigger class identically: same-repo push, same-repo PR (the hole
  this closes: a PR head's workflow runs with the repository's secrets and could name any image), a
  fork PR (untrusted, but the same check) and a dispatch. The runner is never asked to re-apply it
  and the allowlist never enters the job spec: the server is the trust root (L3 stays rejected).
  (b) **The failure shape is an existing one.** The job record is written `failure` with `Reason`
  naming the image, the field it came from and the allowlist (or the digest rule), the check mirror
  is red, and the run folds to failure — no schema change, no new job state. The reason carries no
  secret material: the policy never reads a credential. (c) **Matching is literal, at a boundary,
  and Docker-Hub-aware.** An entry is a `host[:port]/path` literal; a reference matches when it
  starts with the entry and the next byte is `/`, `:` or `@` or the string ends (the `/` boundary is
  deliberate: the documented `docker.io/library` example must allow
  `docker.io/library/golang:1.27`). A host-less reference is *also* matched in its canonical
  `docker.io[/library]/…` spelling, because that is the same reference under docker's grammar —
  `docker.io/library` therefore covers `golang:1.27`. Never an alias across registries: the
  canonical form only ever names docker.io. (d) **Fail closed.** An entry that cannot be parsed at
  evaluation time refuses the job with a reason saying the policy could not be evaluated; empty
  list = unrestricted (the compatibility default), and `image_require_digest` is independent of the
  allowlist (an allowed prefix can still be a mutable tag). (e) **A skipped job is not refused.**
  A job a job-level `if:` already decided `skipped` pulls no image, so the gate leaves the skip (and
  its reason) alone; a job with a false `if:` that only resolves in the `needs:` cascade is refused
  at creation like any other. (f) **Surface.** Two `[threads]` keys with compiled defaults, named
  validator errors keyed to `threads.image_allow_prefixes[N]`, and two `adminFieldMeta` rows, so the
  Configuration page renders, saves, coerces and restart-lists them with no template work (the
  schema is reflection over the struct). Both are restart-required because the policy is read where
  a run is created. Duplicate entries are tolerated (harmless under set-like matching) rather than
  dropped: a validator never mutates its input. (g) **A rerun re-applies the gate.** `RerunRun`
  creates a new attempt of an existing job without re-creating it, so a policy tightened after the
  run was created could otherwise be bypassed by clicking re-run: the same check runs before the
  requeue, a refused job is marked `failure` with the named reason, and a run with nothing left to
  run folds to failure. No new job state and no re-parse of the workflow.

- **NEW (2026-11-22) — instance-scoped runners (supersedes the pass-B1 `(a)` decision).**
  (a) **The unit is the instance, not the repository.** A runner is registered once with a token a
  **host administrator** mints in the browser at `/admin/runners`; only that mint
  authorizes it, and the runner may be offered **any** repository's jobs, including private ones.
  A repo admin cannot mint and cannot see the fleet (403); an anonymous caller gets 401. The trust
  boundary is stated in `docs/PLAN-INSTANCE-RUNNERS.md` §1: the admin mint authorizes the runner
  host, and fork runs remain read-only with no secrets. (b) **The global index is a hint.** The
  pass-B1 objection (an instance runner needs a global dispatch index, which would mean a LIST on
  the claim path) is answered by a CAS'd candidate document (`threads/queue/index.json`) appended at
  enqueue, bounded at 10k rows, and cleaned on claim/terminal transitions. It never grants a claim:
  a global fetch picks the oldest matching repositories from it and runs the **existing per-repo
  claim loop**, where the queue-entry CAS is the only authority. A stale row is skipped and the
  `threads-queue-reconcile` maintainer task (the only place the index LISTs) rebuilds it from the
  authoritative entries. A lost append costs a reconcile pass, never a job. (c) **No shim, and CI
  stops between merge and re-registration.** The repository registry and `users/…/tokens/threads/…`
  credential families are removed from the code; old objects are unread and not migrated. Rollout:
  merge → a host admin mints an instance token at `/admin/runners` → the CI runner is upgraded and
  restarted with
  `spindle runner serve --register <token> --labels …` → CI resumes. The old credential is unknown,
  so the running CI runner must be updated in the deploy window; this is documented in the plan and
  `docs/threads.md` §2.2/§8. (d) **Capacity is declared, not configured in settings.** The registry
  gains a capacity field filled by `declare` (bounded 1–64; 1 stays the isolation-preserving
  default). The admin page shows it; it is still an operator flag, never a repo setting. (e) **The
  admin surface is audited.** Mint and revoke write one host-audit record each
  (`runner_token_mint`, `runner_revoke`, target `runner:<id>`) through the moderation audit family;
  audit failure is logged and never reverts the committed change. (f) **Deferred: runner groups.**
  Named pools with an explicit repository allowlist are a tenancy feature; labels stay an execution
  match and the instance mint stays the authorization until those rules are decided.

- **NEW (2026-11-22) — container-option allowlist, fork-run opt-in, and the runner trust boundary
  (security hotfix).** (a) **Workflow argv is an allowlist, not a pass-through.**
  `container.options`/`volumes` and `services.<id>.options`/`volumes` reach `docker create` as
  workflow-controlled argv, and a job container is a *sibling* on the runner's daemon, so
  `--privileged`, a host namespace, `--cap-add`, `--device`, `--security-opt` or `-v /:/host` is host
  root without any container escape. The allowed set is resource/confinement only: `--cpus`,
  `--memory`/`-m`, `--memory-swap`, `--shm-size`, `--ulimit nofile=…`, `--init`, `--read-only`,
  `--user`/`-u`, `--workdir`/`-w`; every other flag and **every workflow-supplied volume** is
  refused. Enforced at run creation/rerun (a failed job naming the field and option, red check, no
  enqueue — the image-policy shape) and again by the runner at plan time (`spindle:` diagnostic, no
  container created), so a bypassed/older server cannot reach a daemon. The message states the
  allowlist, never the policy's absence. (b) **Fork runs need an explicit opt-in.** A runner records
  `allow_fork_runs` at register/declare (flag `--allow-fork-runs`, env
  `SPINDLE_RUNNER_ALLOW_FORK_RUNS`, default false). The global queue-hint row and the per-repository
  claim both refuse a fork job to a runner without it, and the runner refuses one it is handed
  anyway (defence in depth, reported as a failed attempt). No eligible runner means the job **stays
  queued**, never fails. The per-repository queue-entry CAS remains the claim authority. (c) **Trust
  boundary documented plainly.** `docs/threads.md` §8.7 states that a runner with the docker socket
  is root-equivalent infrastructure (sibling containers on the node daemon), that only
  admin-registered runners exist, that fork code belongs on an opted-in disposable host, and that a
  container escape on a shared host is residual risk this change does not remove (the isolation card
  owns daemonless/per-job daemons; the socket mount is unchanged). (d) **Labels are exact and
  subset-matched.** `--labels` is comma-separated, stored verbatim (case-folded, de-duplicated), and
  a job runs only on a runner declaring *every* label of its `runs-on`; the default set is
  `self-hosted,linux,docker`. Forgejo's hosted-runner labels (`docker, ubuntu-latest, linux_amd64`)
  overlap that in one label, so Forgejo-shaped examples (`runs-on: [self-hosted, linux]`,
  `ubuntu-latest`) never matched and every doc/test example now names the real set or a matching
  subset.

- **NEW (2026-11-26) — Actions parity wave 1b/1c: token aliases, server addresses, command files,
  hosted labels, checkout inputs** (`docs/features/21_parity_master.md` PAR-110/111/114/170/073/090/091,
  D-4/D-12). (a) **One token, three spellings.** The attempt credential is also delivered as
  `GITHUB_TOKEN`/`FORGEJO_TOKEN` and resolves as `${{ secrets.GITHUB_TOKEN }}`; nothing new is minted,
  so scopes and lifetime cannot diverge. An authored `env:` value for those names wins (a workflow
  publishing with its own PAT keeps it — `publish-github.yml` does), and the secret name is reserved
  to the job token. Delivery stays name-only and the token joins `mask`. (b) **Addresses come from
  the configuration, never a literal host.** The fetch payload carries `server_url` (the same
  resolved base as the checkout: `server.public_url`, else the request origin with
  `X-Forwarded-Proto`), and the runner exports `GITHUB_SERVER_URL`,
  `GITHUB_API_URL` (`<origin>/api/v1`) and `FORGEJO_SERVER_URL`; `${{ github.server_url }}` resolves
  to the same value. The closed `github.*` vocabulary (wave 1a) gains exactly `server_url` and
  `api_url` for this — the parser accepts them in every alias spelling, the runner substitutes them
  in step fields, and a **job-level** `if:` still reads them empty (the server's context predates
  the fields). This supersedes the earlier "no hosted labels" note above for the hosted Linux
  spelling only: `ubuntu-latest`/`ubuntu-24.04|22.04|20.04` now expand at enqueue onto the
  `self-hosted` + `linux` set (PAR-170) — a replacement that still requires every expanded label, so
  an alias cannot widen a runner's declared capabilities; `windows-*`/`macos-*` stay unmapped as the
  D-15 non-goal, and the image remains the operator's `--image` (label→image is PAR-171). (c) **The
  environment default is recorded, not changed** (PAR-114/D-12): no `permissions:` means
  `contents: read` and a push is refused with a 403 naming that scope, diverging from Forgejo's
  implicit write. (d) **Job-level `continue-on-error` keeps GitHub semantics** (PAR-056/D-4): the job
  record stays `failure`+`tolerated`, the run fold and `needs.<job>.result` read success, dependents
  run — Forgejo's "ignored" is the recorded divergence. (e) **Command files are per-step paths in
  the job container**, collected after the step and layered onto the NEXT step's `docker exec`
  (env) or a `sh -c` wrapper (PATH prepend) — never the runner's process environment, so concurrent
  jobs cannot see each other; values are one `NAME=VALUE`/directory per line (no heredoc: GitHub
  documents none for these files), protected names are refused by name (runner namespaces, `CI`,
  `PATH`, startup hooks), caps are enforced, and a `docker://` action receives none of it (same rule
  as secrets). (f) **Checkout inputs grow to `ref`/`path`/`fetch-tags`** with an explicit, pinned
  argv and `--no-tags` made explicit; every unsupported documented input is refused by name with
  its alternative rather than silently ignored.

- **NEW (2026-11-26) — the importer is a one-way converter, not a second discovery path.** PAR-002
  (`docs/features/21_parity_master.md` §2.3): `.forgejo/workflows/` and `.github/workflows/` stay
  unread by the engine (PAR-001); `spindle threads import` reads them once, on an operator command,
  and emits `.spindle/workflows/<name>.yml`. (a) **Refuse, never guess.** Every construct the target
  language cannot express is a report entry *and* a `# UNTRANSLATED:` comment at its own site, and
  the command exits non-zero even though the file is written; `--apply` refuses to commit a partial
  conversion without `--allow-partial`. A step or job with no translation is replaced by one that
  fails with the reason, so a lossy conversion can never look green. (b) **The target grammar has one
  implementation.** Every generated file is validated with `workflow.Parse`; a failure is an importer
  error naming the construct, never a warning. The translator walks the parsed YAML node tree (not a
  second schema) so comments, key order and anchors survive; anchors are materialized and mechanical
  rewrites (`needs: test` → `needs: [test]`, scalar branch filters → lists, `runs-on: docker` → the
  default labels, `github.event.inputs.x` → `inputs.x`, `$GITHUB_SERVER_URL` → `$SPINDLE_SERVER`,
  `${{ secrets.X }}` in a `run:` → `${X}`) are report *notes*. (c) **Read-only by default.**
  `--apply` creates a new branch (`spindle/import-<platform>-<timestamp>`), writes and commits there,
  and stops: no push, no force, never the current branch, and it refuses a dirty work tree so an
  operator's uncommitted work cannot be folded into the conversion. (d) **Untrusted input.** Source
  files are text with a 1 MiB cap; discovery and output paths are resolved and required to stay inside
  the repository (a symlink out of it is refused); the source directory is never written. (e) **The
  yaml.v3 boundary widens by exactly one package.** `internal/threads/importer` imports it for the
  same reason `internal/threads/workflow` does — both parse a YAML workflow document (AGENTS law 1,
  amended in the same change); no other package may.
- **NEW (2026-11-26) — implicit supersede is one CAS'd head record per (workflow file, ref), not a
  scan.** (PAR-150, `docs/threads.md` §3.1a.) (a) **Why a head record.** "Cancel the run of the
  commit this push replaces" needs the previous run of the pair, and a run-family LIST is a hot-path
  scan law 4/6 forbid. One small record (`threads/supersede/<wf>.<ref>.json`) names the newest run;
  the writer that advances it is told what it displaced, and a writer that finds a *newer* id there
  has itself been superseded and cancels itself. That single forward-only rule (run ids are
  time-ordered) closes the read-then-cancel window without a lock. (b) **Adoption is after the run
  header, cancellation after the enqueue.** The header stays the run's commit point, and a head id
  can never name a run whose header is not visible; applying the cancel last means a run this
  creation superseded is always inspectable before it is terminated. (c) **The two concurrency rules
  are reconciled explicitly, not stacked.** A run that declares a `concurrency:` group neither
  adopts a head nor is implicitly cancelled: the group is the workflow's explicit serialisation
  choice (PAR-152), and implicitly cancelling its holder would make `cancel-in-progress: false` a
  lie. Implicit supersede is the group-free default; the group is the override. (d) **One cancel
  path.** Operator cancel, supersede, `cancel-in-progress` and the approval deny all call
  `cancelRun`, which is idempotent and keeps the first reason (`cancel_reason`/`cancelled_by`/
  `superseded_by` on the header, the reason on a superseded run's queued jobs). (e) **Budget.** A
  group-free push/PR run adds one GET + one conditional PUT of the head, issued in its own goroutine
  concurrently with the checks mirror and the enqueue; dispatches, schedules, PR `opened`/`reopened`
  and every grouped workflow add none, and the cancel is the pre-existing terminal path paid only
  when an older live run exists.
- **NEW (2026-11-26) — the parity matrix has two modes, because Wave 1 lands with planned gaps.**
  (PAR-201, `docs/go/15_testing.md` §6.8.) §9 of the parity master plan says the gate fails on a
  `missing` row; taken literally that makes `make parity` red for every later wave, so the CI gate
  enforces *integrity* (green needs an existing test, every gap planned, the plan/matrix id sets
  agree, the render fresh) and `make parity-closed` enforces the plan's *closure* rule (no `missing`
  row). The closure mode is the definition of done, not a per-PR gate. The corpus harness mirrors
  the same discipline: a file blocked by an open row must fail with its documented refusal, and the
  entry flips to the strict shape assertion in the change that closes the row.
- **NEW (2026-11-26) — Actions parity wave 1d: one substitution pass in every step site**
  (`docs/features/21_parity_master.md`; docs/threads.md §3.6, docs/WORKFLOWS.md §6.2). (a) **One
  value set per step.** The runner feeds `workflow.TemplateValues{GitHub, Matrix, Inputs, Env,
  Secrets, Steps, Needs}` to `workflow.Substitute` for `name`, `run`, `if`, `shell`,
  `working-directory`, `env:` values, `with:` values, a composite action's `outputs:` and a job's
  `outputs:` — replacing the earlier matrix/inputs-only path plus the two ad-hoc cases (a
  whole-value step `env:` secret and the two server addresses). `github.*` is the closed field set
  captured from the runner's built-ins before any authored `env:` overlay (so `env: GITHUB_SHA: …`
  cannot change `${{ github.sha }}`); `env.*` is the authored layer at the site, the step's own
  values resolving against the layer below them; `secrets.*` is the job's secret map plus the
  reserved token aliases. (b) **The payload carries the actor.** `FetchResult.Actor` (the run's
  `actor`) is exposed as `GITHUB_ACTOR` and `${{ github.actor }}`, so a step `if:` and a step field
  read the same value a job-level `if:` reads on the server. (c) **Empty vs error.** An absent
  supported value expands empty (an unset env name, a missing secret, no actor, no origin); an
  undeclared `steps.<id>.outputs.<name>` / `needs.<job>.outputs.<name>` fails the step naming the
  reference. The plan-time preview defers output spans (nothing has run yet) and the executor
  resolves the authored template once, immediately before the step runs, so a substituted value is
  never rescanned. (d) **Masking follows substitution.** Every secret the plan can substitute joins
  the stream's mask list at job start (`Streamer.addMask`, mutex-guarded), so an embedded
  `${{ secrets.X }}` spliced into a script prints `***` exactly like a variable delivered from the
  plan's environment. A whole-value step `env:` entry keeps the stronger by-NAME delivery (no
  secret in argv).
  (e) **A tag run's ref reaches the container.** The fetch payload carries the run's full `ref`
  (`refs/heads/main`, `refs/tags/v1.2.3`), and the runner derives `GITHUB_REF`/`SPINDLE_REF` and
  `GITHUB_REF_NAME` from it: a tag run has no branch, so the old branch-only derivation left both
  variables empty while the server-side `github.*` was already correct. A pull request keeps its
  BASE branch (the payload's `refs/pull/<n>/head` is not a job's identity — the same rule the OIDC
  token's `ref` claim applies), and a payload with neither a ref nor a branch answers empty, never
  `refs/heads/`. `GITHUB_REF_TYPE` is deliberately not invented: `ref_type` is not in the closed
  vocabulary.
