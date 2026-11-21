# 15 — Threads: CI runs

> **Status:** specified for v1 (2026-11-20). Not implemented. Replaces the DRAFT
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
  amended 2026-11-20). Unknown keys are rejected, not ignored. No cross-forge file
  fallback (`.github/`, `.forgejo/` are not read) — one documented location.
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
(no empty run record). A file that does not parse fails the whole creation visibly
(`422`/logged error) and writes nothing — a workflow that cannot execute never enqueues a job.

| event | workflow revision | `trusted` |
|---|---|---|
| `push` | the pushed commit | `true` |
| `pull_request`, same repository | the head commit | `true` |
| `pull_request`, fork | the **base branch tip** (`opts.BaseCommit`, else resolved `refs/heads/<base>`) | `false` |
| `workflow_dispatch` | the resolved ref's commit | `true` |

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

**Job record** — `repos/<o>/<r>/threads/runs/<run_id>/jobs/<job_id>.json`, Create-then-CAS:

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
  "attempt": 1,
  "trusted": true,
  "state": "queued",
  "created_at": "…", "updated_at": "…", "started_at": null, "completed_at": null,
  "version": 1
}
```

`job_id` is `j-<slug>` (a collision gets `-2`), so it is a stable, URL-safe key. Steps are copied
into the record: a runner must not need the workflow file to start a job. `Name` is the YAML
`name`, falling back to the map key.

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
capped at 40 chars). Job states map to the statuses enum: `queued`/`running` → `pending`,
`success` → `success`, `failure`/`cancelled` → `failure` (a cancelled required check must never
unblock a merge). The write goes through `checks.Service.RecordStatus`, the trusted in-process
reporter (05 §3 note): same validation/storage/index/fan-out as an external report, no credential
check, and **not reachable over HTTP**. A Threads job therefore gates a merge exactly like an
external CI report, with no gate code change. A mirror failure at creation (e.g. the commit is not
yet resolvable in the serving copy) surfaces as an error after the run header exists; the job's
next state transition re-mirrors, and the run page still shows the job (the header is the run's
truth). Fork PR heads are bridged by the existing pulls fork→base fetch before this path runs
(T3/T4 wiring, not T1).

**Writes and concurrency.** Job records are written before the run header; the header is the run's
commit point, so an interrupted creation leaves invisible orphan jobs (swept by `threads-gc`)
rather than a half-visible run. Run ids are minted per creation and every create is `PutCreate`;
job/fold transitions are the canonical CAS retry loop (13_concurrency.md §3). A run get is one
exact-key GET plus one bounded prefix LIST over that run's jobs; the run list is one prefix LIST
behind a page request (cursor = last run id, `n` default 50, max 200) — no repo-wide LIST on any
hot path.

**Task kind.** `threads-gc` is registered by composition (Seam 5). Its body is a documented
no-op in T1: log chunks and retention windows do not exist yet; T3 owns the sweep the name
reserves.


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

## 5. Security model (non-negotiable)

1. A job container is root inside its own namespace; with a shared DinD daemon, jobs on that daemon
   can see each other. Therefore **capacity 1** is the default, capacity > 1 is an explicit operator
   choice with the caveat shown in the UI, and scale-out (many capacity-1 runners) is the documented
   path to 20/100 concurrent jobs for untrusted code.
2. Fork PRs: base-branch workflow, no secrets, read-only token, and an approval gate for first-time
   contributors (the run is created in `waiting` and an admin approves).
3. Secrets are scoped to the repository and exposed only to trusted runs; they are masked in logs
   (best effort, documented as such) and never persisted in the run record.
4. Egress from job containers follows the same policy surface as the rest of spindle
   (`internal/egress`), with a per-runner default-deny option.
5. A leaked job token is scoped to one attempt and one repository, expires with the job, and cannot
   push to protected refs (it is a `ci:` principal, not the triggering user).

## 6. UI and API

### 6.1 As implemented (T1)

| Method + path | Auth | Request → response |
|---|---|---|
| `GET /{o}/{r}/api/threads?after=&n=` | repo read | `{runs: [Run], more}` — newest first, cursor `after` = last run id, `n` default 50 / max 200 |
| `GET /{o}/{r}/api/threads/{run}` | repo read | `{run: Run, jobs: [Job]}`; unknown run `404` |
| `POST /{o}/{r}/api/threads` | repo **write** | `{ref, workflow?}` → `201 {runs: [Run]}`; `workflow` is a basename or path (empty = every `workflow_dispatch` file); unknown file/ref `404`, bad body `400` |

Both lanes (`/api` and `/api-browser`), `no-store` on every GET, plain-text errors, RFC 3339,
arrays `[]` never null, anonymous-denied reads a real 401 with `WWW-Authenticate: Bearer`. The
handler is the Seam 1 `server.ExtraRoutes` chain (`Handle` reports false for non-threads paths) and
its two templates are registered in `/api/v1` discovery (`api.RegisterExposed`) from composition.

Page (server-rendered, `internal/webui`; `12_web_ui.md` §2): `GET /{o}/{r}/threads` lists runs
(workflow, event/branch, commit link, state, `untrusted` chip, time) with an internal-scroll tab
strip entry **Threads** and the shared empty state; `GET /{o}/{r}/threads/{run}` shows the header
(workflow path/commit/digest/actor, trust class) and each job with its runs-on, check context and
steps. Read-only in T1: dispatch is the JSON API, cancel/rerun arrive with the T2 runner protocol.

The full v1 surface (live logs, cancel/rerun, runners, secrets, settings) is below.

- **Repository tab:** `Threads` (replaces the old "Actions" naming) — run list with commit, event,
  status, duration; a run page with jobs, live logs (SSE via the existing task stream), cancel and
  rerun.
- **Settings:** runners (register/revoke, labels, capacity), secrets (write-only), workflow policy
  (fork approval on/off).
- **JSON API/SDK:** runs, jobs, logs (paged), cancel/rerun, runners, secrets — the same gates as the
  pages.

## 7. Tests (definition of done)

- **Protocol contract:** register → declare → fetch → start/log/complete, with attempt supersession,
  idempotent log frames, cancel, and auth failures (expired/foreign attempt tokens).
- **Scheduler:** trigger matching, label matching, required-check integration, fork trust-class
  selection (base-branch workflow + no secrets), approval gate, cron evaluation, concurrency caps.
- **Executor:** a fake `docker` CLI asserting the exact argv shape for create/exec/cp/rm, workspace
  volume lifecycle, service network teardown, timeout kill; a real Docker-in-Docker e2e that runs a
  two-step workflow when a daemon is present (skip, not fail, otherwise — the same rule as the git
  e2e).
- **Security matrix:** fork PR gets no secret and a read-only token; a job cannot read another
  repository's secrets; logs are masked; capacity-1 runners reject a second claim.
- **Rendered:** run page live logs at desktop and ~390 px; no page-level horizontal scroll.

## 8. Decisions & deviations

- **NEW (2026-11-20) — T1 as-built record/selection decisions.** (a) Run ids are
  time-ordered (`%019d-nanos` + random) so the cursor list needs no index projection and
  run creation pays no extra CAS write; (b) the run header is created LAST (jobs first), making it
  the run's commit point and orphans invisible/sweepable; (c) the checks mirror uses the existing
  statuses family via `checks.Service.RecordStatus` (one shared validation/storage path, no
  HTTP-reachable bypass) rather than a new check-runs family — 05 §1 already deferred check-runs,
  and required-check gating is identical; (d) contexts are `threads/<workflow>/<job>`, lowercased
  and slugged because the checks grammar forbids spaces; (e) cancelled jobs map to `failure`, not
  success, so a cancelled required check cannot gate a merge open; (f) a bare `push:` /
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
