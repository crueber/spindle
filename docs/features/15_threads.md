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

- **Workflow files:** `.spindle/workflows/*.yml` at the commit being built. No cross-forge file
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
  filesystem without an explicit `--allow-host-docker` acknowledgement. Capacity defaults to **one
  job per runner** (ephemeral runners are the cheap isolation win); operators may raise it and accept
  the shared-daemon caveat.
- Resource limits per job container (cpu/memory/pids) come from runner config, with the workflow able
  to lower them, never raise them.

## 5. Security model (non-negotiable)

1. A job container is root inside its own namespace; with a shared DinD daemon, jobs on that daemon
   can see each other. Therefore **capacity 1** is the default and shared capacity is an explicit
   operator choice.
2. Fork PRs: base-branch workflow, no secrets, read-only token, and an approval gate for first-time
   contributors (the run is created in `waiting` and an admin approves).
3. Secrets are scoped to the repository and exposed only to trusted runs; they are masked in logs
   (best effort, documented as such) and never persisted in the run record.
4. Egress from job containers follows the same policy surface as the rest of spindle
   (`internal/egress`), with a per-runner default-deny option.
5. A leaked job token is scoped to one attempt and one repository, expires with the job, and cannot
   push to protected refs (it is a `ci:` principal, not the triggering user).

## 6. UI and API

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
- **NEW — capacity 1 by default.** Shared DinD is an explicit operator decision, not a default.
