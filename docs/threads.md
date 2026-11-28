# Threads: CI runs — usage reference

> This is the **user guide** for Threads: how to write a workflow, install a runner, and debug a
> failure. The design/as-built record — protocol, bucket record shapes, security reasoning and every
> decision with its rationale — is [`features/15_threads.md`](features/15_threads.md); read that one
> for the "why" and for the frozen wire shapes. Route and page details live in
> [`go/06_server_http.md`](go/06_server_http.md) and [`go/12_web_ui.md`](go/12_web_ui.md).

## 1. What Threads is

Threads is spindle's CI. A workflow file lives in the repository at `.spindle/workflows/<name>.yml`;
a matching event (a push, a pull request, a schedule tick or a manual dispatch) creates a **run**;
the run's **jobs** are matched to registered **runners**, and a runner executes each job in its own
container, reporting results back into the checks surface that already gates merges.

The mental model, from the outside in:

| Level | How many | What it is | Reported as |
|---|---|---|---|
| **Run** | one per workflow file per event | a workflow file pinned to one commit (path + commit + sha256 of the file) | the Threads page row / `GET …/api/threads` |
| **Job** | one per `jobs:` key, or per `strategy.matrix` cell | one container, one workspace volume, one runner claim | one check: `threads/<workflow>/<job>` |
| **Step** | one per `steps:` entry | one `docker exec` in that container (or the built-in checkout) | a step result on the run page and the job log |

Run-level and job-level are separate on purpose: a run is "what the file asked for at that commit",
a job is "one unit of work a runner claims and executes". A run's state is folded from its jobs
(any failure ⇒ `failure`; otherwise any genuine cancel ⇒ `cancelled`; `skipped` jobs and failures a
job's `continue-on-error` tolerated are neutral; all green ⇒ `success`), and a job that never ran
because its `if:` was false or its `needs:` dependency failed is `skipped`, not `cancelled` — with
the reason on the job record.

A workflow may be **trusted** or **untrusted**:

- trusted: `push`, same-repository `pull_request`, `pull_request_target` (when the repository enabled
  it), `workflow_dispatch`, `schedule`, `repository_dispatch`, `workflow_run` (unless its upstream
  was untrusted), `create`/`delete`, `deployment`/`deployment_status` — secrets of all three scopes
  are delivered and `permissions:` decides the job token's scopes;
- untrusted: a **fork** `pull_request` — the workflow file is read from the base branch, no secrets
  are delivered, the token is `contents: read` (unless the repository enabled `send_write_tokens`,
  D-13), and a first-time contributor's run waits for a maintainer's approval. A `workflow_run`
  whose upstream run was untrusted is held the same way (D-3).

Everything lives in the object store; there is no queue server and no local state that survives a
restart. Restarting the server loses nothing but warmth.

## 2. Quickstart

### 2.1 Add a workflow and push it

Create `.spindle/workflows/ci.yml` on a branch and push it. Only `.spindle/workflows/*.yml` and
`*.yaml` are read (one directory, no nesting, no `.github/` fallback), and each file is capped at
1 MiB.

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    steps:
      - uses: actions/checkout
      - name: test
        run: go test ./...
```

The push that introduces the file triggers the run itself: the workflow is read **from the pushed
commit**, so nothing else has to be arranged.

### 2.2 Start a runner

A run does nothing until a runner claims its jobs. Runners are **instance-scoped infrastructure**:
a token is minted by a **host administrator** in the browser at `/admin/runners`, together with the
runner's **repository policy** — every repository on the instance, or a bounded allowlist of
`owner/name` / `owner/*` patterns (§8.2). The admin mint is the authorization; the runner host is
trusted with the code its policy lets reach it, while fork runs stay read-only with no secrets. The
runner is the same `spindle` binary — no data dir, no store, just a protocol client.

Mint the token in the browser at **`/admin/runners`** ("Mint a registration token"): type a name,
then either tick **All repositories** or list the patterns (one per line or comma separated). The
policy is the admin's decision — the runner can never change it — and to change it later you edit it
on the fleet row ("change policy…"), which keeps the runner and its current job running.

```
# Registration token (shown once, expires 2026-11-21T12:15:00Z):
# wrg_…
# policy: acme/mono, acme/*
```

Minting, listing and revoking are **browser-only by design**: the CLI's API client authenticates
with static machine credentials, and a static token cannot administer (AGENTS law 9), so there are
no `spindle runner token|list|revoke` verbs. `spindle runner serve --register` is the operator step
after a browser mint.

The registration token is valid for **15 minutes** and is shown once. Exchange it for a long-lived
instance credential, then run the runner:

```sh
# one-time: consume the registration token, write a 0600 token file, then serve
spindle --server https://spindle.example.com runner serve \
  --register wrg_… \
  --token-file /etc/spindle/runner.token \
  --labels self-hosted,linux,docker --name build-1 --capacity 1

# afterwards the token file is enough
spindle --server https://spindle.example.com runner serve \
  --token-file /etc/spindle/runner.token \
  --labels self-hosted,linux,docker --name build-1 --capacity 1
```

**Upgrading from repository-scoped runners (2026-11-22).** There is no automatic migration: old
repo-scoped credentials are unknown to the instance registry. Mint a new token at `/admin/runners`,
upgrade the runner binary, restart it with `--register <token>`, and CI resumes. **CI is down
between the deploy and that re-registration**; plan the restart in the same window. A runner
registered before repository tenancy existed has no policy on its record: it is offered **nothing**
(never "everything"), the fleet shows it as *none — no repository allowed*, and the fix is one edit on
its fleet row (no re-registration, no downtime).

**Offline registration (PAR-180).** A file can replace the browser mint and the long command line.
`spindle runner serve --config /etc/spindle/runner.toml` reads one or more `[[runner]]` entries; each
entry has its own credential (therefore its own admin-set scope) so **one process serves several
scopes**, while the docker/image/container-grant settings stay process-wide:

```toml
[[runner]]
name = "org-build"
server = "https://spindle.example.com"
token_file = "/etc/spindle/org.token"      # 0600; --register writes it
labels = ["self-hosted", "linux", "ubuntu-latest:docker://ubuntu:24.04"]
capacity = 1

[[runner]]
name = "repo-gpu"
server = "https://spindle.example.com"
registration_file = "/etc/spindle/repo-gpu.wrg"  # one-time; consumed at start
labels = ["linux", "gpu"]
capacity = 1
ephemeral = true
max_cpus = 4
max_memory = "16GiB"
```

A credential file must be `0600` (`chmod 600`); a group/world-readable one is refused before the
runner dials. A literal `token`/`register_token` is accepted for a file with restrictive
permissions, but file paths are the documented form because they keep the secret out of argv and
logs. `--register-file` is the same idea for a single-runner start. The repository policy is still
the administrator's: nothing in this file can widen it.

`--server` is a global flag (it may sit before or after the subcommand) and falls back to
`SPINDLE_SERVER`, then to the server selected by `spindle auth login`, then to
`http://127.0.0.1:8080`.

**Docker is required.** The runner is not an executor library: it shells out to the `docker` CLI
(`--docker`, default `docker`) and drives a daemon named by `DOCKER_HOST`. If that daemon is the
host socket (unset `DOCKER_HOST`, or `unix:///var/run/docker.sock`), the runner refuses to start
until you pass `--allow-host-docker` — a daemon with the host socket is root-equivalent
infrastructure (§8.7). For untrusted code, point `DOCKER_HOST` at a per-runner `docker:dind` instead
and leave the flag off.

**Running the runner as a container.** The published image is the same one that serves spindle —
the runner is the same binary, and the image ships the `docker` CLI for exactly this purpose. Put
the runner next to the daemon it drives, mount the host socket, and hand a nonroot process the
socket's group:

```sh
docker run -d --name spindle-runner --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -v /srv/spindle/runner.token:/run/secrets/runner.token:ro \
  ghcr.io/crueber/spindle:latest \
  runner serve --server https://spindle.example.com \
    --token-file /run/secrets/runner.token \
    --labels linux,docker --capacity 1 --allow-host-docker
```

- **Group, not root.** The image runs as the nonroot `spindle` user (uid 1000), which is not in the
  host's `docker` group; `--group-add` with the socket's numeric GID is what lets it open the socket
  (GNU `stat`; on macOS use `stat -f '%g'`). The token file must be readable by that uid too
  (`chown 1000` it, mode `0600`, before mounting).
- **The acknowledgement stays explicit.** `--allow-host-docker` is still required for the host
  socket: job containers are siblings on that daemon, which can mount the host filesystem, so a
  container escape is root on the host. For untrusted code use a `docker:dind` daemon and set
  `DOCKER_HOST` instead (leave the flag off); then the job containers live behind *that* daemon.
- **Checkout reachability is unaffected by where the runner runs** — job containers are created by
  the daemon, so they must reach the checkout URL from *their* network (see §4.4). Do not point the
  checkout at the runner container's own hostname.

The job container needs `git` on `PATH` because checkout runs **inside** it; the default job image
is `alpine/git:latest` for exactly that reason (`--image` overrides it, and a `container:` in the
workflow overrides both).

### 2.3 Watch it

| Where | What |
|---|---|
| `/{owner}/{repo}/threads` | run list (workflow, state, event/branch, actor, commit, duration), filters, the **Run workflow** control, and the per-file **Workflows** section with parse errors |
| `/{owner}/{repo}/threads/{run-id}` | one run: the **job graph** (one node per workflow job, matrix cells grouped with their check and duration, `after` edges, blocked dependents), jobs, `runs-on`, runner attribution, check context, durations, step results, the **Artifacts** list (name, size, age, ZIP download), one live log panel per job, and the whole-run log download (`.txt` / `.zip`) |
| the commit page | a **Checks** panel with one row per context, linking to the full checks page |
| `GET /{owner}/{repo}/api/threads` | the same run list as JSON (newest first, cursor `after`, `n` default 50 / max 200) |
| `GET /{owner}/{repo}/api/threads/{run}/artifacts` | the run's artifacts (`id`, `name`, `files`, `total_size`, `created_at`, `expires_at`) under the same read gate as the run; a closed `retention-days` window is a 404 |
| `GET /{owner}/{repo}/api/threads/{run}/artifacts/{name}` | one artifact manifest (the same read gate) |
| `DELETE /{owner}/{repo}/api/threads/{run}/artifacts/{name}` | delete the artifact (a **write** role) and report `bytes_freed`; the blobs and the manifest both go |
| `GET /{owner}/{repo}/threads/{run}/artifacts/{name}` | the artifact as a ZIP download (`Content-Disposition: attachment; filename="<name>.zip"`, `application/zip`), streamed from the blobs at the artifact's `compression-level`; a reader's gate, never a cross-repository read |
| `GET /{owner}/{repo}/api/threads/caches` | the repository's cache entries and the effective caps (read gate) |
| `DELETE /{owner}/{repo}/api/threads/caches/{id}` | delete one cache entry (a **maintain** role, audited) and report `bytes_freed` |

The run id is in the page URL and in `spindle threads list acme/widgets`.

### 2.4 Migrating in a Forgejo or GitHub workflow

Threads only ever runs `.spindle/workflows`; it does not read `.forgejo/workflows` or
`.github/workflows` at run time (not as a fallback, not behind a flag). The migration is an explicit,
one-way conversion:

```sh
# Dry run: print the converted file, change nothing.
spindle threads import --from forgejo --repo .

# Review, then commit the conversion on a new branch.
spindle threads import --from forgejo --repo . --apply
```

The importer translates what the Spindle workflow language expresses (triggers, jobs, steps, the
three built-in actions, local actions, `docker://`, labels, `needs`, concurrency, permissions,
dispatch inputs, schedules, secrets and the documented env aliases) and **refuses to guess**: every
construct it cannot express is listed in the report and marked `# UNTRANSLATED:` at its own site in
the converted file, and the command exits non-zero even though the file is written. Nothing is
pushed, and the source directory is never edited. The full translation table, the refusal list and
the review workflow are in [`WORKFLOWS.md` §10](WORKFLOWS.md#10-migrating-from-forgejo).

**The migration banner (PAR-003).** Until the converted files are on the default branch, the
`/{owner}/{repo}/threads` page shows a banner: when the repository has `.forgejo/workflows` or
`.github/workflows` files and no `.spindle/workflows`, it lists the foreign files and prints the
exact command to convert them (`spindle threads import --from forgejo --repo .`, or `--from github`).
The banner reads those directories **for display only**, on that page request: nothing is parsed,
imported or run, no run is created by visiting the page, and the engine still has no code path that
reads a foreign directory (§3.6).

**Checking a converted file offline (PAR-199).** `spindle threads validate` runs the server's own
validator, so its messages are the ones a push (and the page's editor) would report:

```sh
spindle threads validate .spindle/workflows/ci.yml   # one file
spindle threads validate .spindle/workflows          # every *.yml/*.yaml file directly inside
spindle threads validate --repo .                    # ./.spindle/workflows of a checkout
spindle threads validate - < ci.yml                  # stdin
spindle threads validate --repo . --json             # the machine report
```

Exit **0** when every file parses, **1** when any file is invalid (each issue prints
`file:line: key: message`) or a target cannot be read, **2** for a usage error. `threads explain`
remains the neighbouring verb: it summarises what a valid file will do, while `validate` answers
whether it parses at all. The full contract (flags, report shape) is in
[`CLI.md` §5.4](CLI.md#54-threads-validate--offline-workflow-validation).

## 3. Workflow reference

> **Writing a workflow?** [`WORKFLOWS.md`](WORKFLOWS.md) is the authoring guide: compact key
> tables, recipes, the Forgejo migration cookbook and a troubleshooting tree. This section is the
> exhaustive reference it links into.

The parser is strict: **an unknown key is an error for that file**, never a silently ignored typo.
A file that does not parse creates no runs of its own and does not affect the other files — its error
is listed in the Threads page's **Workflows** section (anchored `#workflow-<slug>`) and in
`GET …/api/threads/workflows`.

### 3.1 Top level

| Key | Accepted | Default | Notes |
|---|---|---|---|
| `name` | string | file stem | display name; also the `<workflow>` segment of every check context |
| `on` | event name, list of names, or mapping | — | a file with no trigger parses but never fires; the event vocabulary is closed — `push`, `pull_request`, `pull_request_target`, `repository_dispatch`, `workflow_run`, `merge_group`, `create`/`delete`, `deployment`/`deployment_status`, `release`, `issues`, `issue_comment`, `pull_request_review`/`pull_request_review_comment`, `label`, `milestone`, `fork`, `watch`, `check_run`, `workflow_dispatch`, `workflow_call`, `schedule` (§3.2) |
| `run-name` | string with `${{ … }}` spans | the workflow `name` | the run title (PAR-057): rendered once at creation from `github.*` and `inputs.*`, one bounded line, shown by the run list, run page and API |
| `env` | map of string → string | empty | merged **under** the job's `env` (job wins); a YAML scalar value (number, bool) is coerced to its text |
| `defaults` | `{run: {shell, working-directory}}` | none | run-step defaults (PAR-054): job overrides workflow per key, step overrides both; both values are literals |
| `concurrency` | group name, or `{group, cancel-in-progress}` | none | one running and one pending run per group per repository (§3.1a.1); `cancel-in-progress` is a bool **or one `${{ … }}` expression** evaluated at creation (PAR-151); the group may use `${{ spindle.* }}`, `${{ matrix.* }}` (job level) and `${{ inputs.* }}` — see §3.6 |
| `permissions` | mapping, or `read-all` / `write-all` | instance default (`threads.default_permissions`, `read`) | the job token's authority: `contents`, `issues`, `pull-requests`, `deployments`, `packages`, `checks`, `statuses`, `id-token`; any other scope is a parse error naming the supported set (§5.3.1) |
| `enable-openid-connect` | bool | false | Forgejo's spelling for the OIDC grant (§5.5); same grant as `permissions: id-token: write` |
| `jobs` | map of key → job | required | at least one job |

### 3.1a Supersede: a newer push cancels the older run's work

A push to a ref and a PR `synchronize` start work for **one** commit. Without a
rule, two pushes in a row race each other (the classic double deploy). So
Threads does what Forgejo does: **the newer run supersedes the older one**.

- The pair an implicit supersede acts on is **(workflow file, ref)**, and the ref
  is the full name: `refs/heads/<branch>` for a branch push, `refs/tags/<tag>`
  for a tag push (§6.1), `refs/pull/<n>/merge` for a PR (its run ref;
  `refs/pull/<n>/head` when the merge conflicts). So a push on `main` never
  cancels a feature branch's run, a PR's run is never cancelled by a push to its
  base branch, and a branch and a tag of the same short name (`v1.2.3`) are
  independent — one never cancels the other. A tag re-pushed onto a new commit
  does supersede its own previous tag run, which is the same "newer push wins"
  rule.
- The **superseded run reads `cancelled`** and names its successor
  (`superseded_by` on the run header; the API's `cancel_reason` is
  `supersede`). A queued job of that run also carries the reason ("superseded by
  run `<id>`") so the run page says why it did not execute.
- A run that already **ended** (success, failure, cancelled, skipped) is not "in
  flight" and is never touched.
- `workflow_dispatch` and `schedule` never supersede: a dispatch is an explicit
  act, and a cron tick is the schedule's own edge. A PR `opened`/`reopened`
  registers itself as the newest run of its head ref but cancels nothing —
  reopening a PR is not a new commit.
- **A run that declares a `concurrency:` group is never implicitly
  cancelled**, and a run whose workflow declares a group does not implicitly
  cancel anything either. The group is the workflow's explicit serialisation
  choice (§3.1, and `cancel-in-progress: false` in particular), and silently
  cancelling the holder would make that key a lie. Implicit supersede is the
  *default for group-free workflows*; the group is the *override*. `PAR-150`
  and `PAR-152` are the two levels of one model, not two features.
- Only the newest run of a pair is registered: the head record is advanced
  forward-only on the run id, so under a burst of pushes exactly one run stays
  live and every loser names a successor that exists.

```text
push A ──► run 1 (queued)                 push B ──► run 2
                     head(ci.yml, refs/heads/main) = run 2
run 1: state=cancelled, cancel_reason=supersede, superseded_by=run 2
```

Cancellation is one path — operator cancel, supersede, `cancel-in-progress` and
an approval deny all go through it — so the meaning of "cancelled" cannot drift,
and it is **idempotent**: cancelling an already-cancelled run leaves the first
reason, the first actor and the completion time in place.

### 3.1a.1 Concurrency groups: one running, one pending (PAR-152)

A workflow that declares `concurrency:` replaces implicit supersede with a
group (above). The group record holds two slots: the **holder** (the one run
allowed to execute) and **one pending run** (GitHub's rule):

- A newer run that wants a held group becomes the pending run and **cancels the
  pending run it replaces**, so a burst of three pushes leaves one running and
  one pending — never two waiting, and never a queue of stale commits.
- `cancel-in-progress: true` cancels the holder (and any pending run) **when the
  new run is created**, not when a runner happens to poll; the new run then
  waits only for the fold to release the group.
- `cancel-in-progress` may be one `${{ … }}` expression (PAR-151), evaluated at
  creation from the documented contexts (`spindle.*`, `inputs.*`, and at job
  level `matrix.*`/`strategy.*`): `cancel-in-progress: ${{ spindle.ref_name != 'main' }}`
  protects the main branch while cancelling older branch runs. A non-boolean
  result fails run creation; the authored expression never reaches the record.
- `cancel-in-progress: false` sequences in trigger order: the holder finishes,
  the pending run starts, and a later run replaces the pending one. The run's
  header carries `pending_group` while it waits, which is what the run page and
  the run list render ("group X · waiting") instead of a silent spinner.
- A run held by the fork gate or an environment approval does **not** enter a
  group: it is not queued at all until it is approved, so a held deployment
  cannot cancel a running job merely by being created.
- The claim path refuses to offer a job while the holder is another live run or
  while a live run owns the pending slot ahead of it; the holder's terminal fold
  clears both slots (a cancelled pending run can never wedge the group).

### 3.1b Conformance: `make parity`

Threads accepts the Actions syntax and is checked against real workflows by a
committed corpus (`internal/e2e/testdata/parity/`, four Weave/Scriobh files with
a `SOURCE.json` stamp) and a living matrix
(`docs/features/parity/parity.json` → generated `matrix.md`). `make parity` runs
both, with no docker and no network:

- the **gate**: every `have` row must name a test that exists in the tree, a
  `missing`/`partial` row must be planned (a wave or a decision), a non-goal must
  name its signatory, the master plan's `PAR-` id set must match the matrix, and
  `matrix.md` must be a fresh render (never hand-edited);
- the **corpus parse/plan tier**: each converted file parses, exposes the
  documented jobs and step counts, and passes through the engine's real
  run-creation path; the trigger tier drives the corpus's real trigger shapes
  (a tags-only file fires on a matching tag and on nothing else). All four
  Weave/Scriobh files convert with **no refusals** today, and the matrix is
  **119 rows — 117 `have`, 0 `partial`,
  0 `missing`, 2 signed `non-goal`**. A file whose feature
  rows are not green yet must instead fail with the documented refusal (an
  `# UNTRANSLATED:` marker the importer emitted, or a parse error the harness
  declares) — so a gap is named, never inert.

`make parity-closed` adds the plan's §9 closure rule (no `missing` row at all)
and is the check to run when a wave closes, not on every PR. `make parity-exec`
is the opt-in execution tier: it boots a server and a runner, executes the
committed golden of a real publish workflow byte-unmodified, then builds,
pushes and pulls a small
image through a local `registry:2` with a daemonless builder (no docker socket
in the job). It requires `SPINDLE_PARITY_EXEC=1` and a docker daemon; when the
tier is requested, a missing daemon or image is a failure, never a skip.

### 3.2 `on`

| Trigger | Keys | What fires |
|---|---|---|
| `push` | `branches`, `branches-ignore`, `tags`, `tags-ignore`, `paths`, `paths-ignore` | a branch push (`refs/heads/*`) matching `branches`, a tag push (`refs/tags/*`) matching `tags`, and a changed-file set matching `paths`. The two ref filters are independent: `tags`-only does not fire on a branch, `branches`-only does not fire on a tag, and naming neither fires on both kinds. Branch and tag deletes never fire. A merged PR creates a `push` run for the base branch. |
| `pull_request` | `branches`, `branches-ignore` (target branch), `paths`, `paths-ignore`, `types` | the listed lifecycle edges on a PR whose target branch and changed files match; no `types` = every emitted action. Every edge's run ref is the server-computed merge commit (`refs/pull/<n>/merge`, `github.ref`, `GITHUB_SHA`), with the job's `GITHUB_REF` variable kept at the base branch; a conflicting merge falls back to the head |
| `pull_request_target` | same filters as `pull_request` | the same PR edges, but the **base branch's** workflow file runs with base secrets and a write token. Off per repository by default (D-2); enabling it is a maintain+ audited switch (`pull_request_target`), and a declarative head checkout needs the second switch (`pr_target_allow_head_checkout`) |
| `repository_dispatch` | `types:` (the dispatcher's `event_type` strings) | `POST …/api/threads/dispatches` with a write role; `github.event.client_payload.*` is the bounded payload |
| `workflow_run` | `workflows` (upstream names), `types` (`requested`, `completed`; default `completed`), `branches`, `branches-ignore` | another run's lifecycle edge; the upstream run's name/branch are the filter, the file comes from the default branch, and an untrusted upstream holds the follow-up for approval (D-3) |
| `merge_group` | `types` (`checks_requested`), `branches`, `branches-ignore` (target branch) | a push to the documented merge-queue ref `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>`; one run checks the queue head with the **base branch's** file, and a queue ref never fires `push` (§6.10) |
| `create` / `delete` | none | a branch or tag created/deleted (`ref_type` in the payload). A job-token push still starts nothing |
| `deployment` / `deployment_status` | none | a Threads deployment record is created (`deployment`) or changes state (`deployment_status`, states `pending`, `in_progress`, `failure`) |
| `workflow_dispatch` | `inputs:` (see below) | a manual dispatch from the UI/API |
| `workflow_call` | `inputs:`, `secrets:`, `outputs:` | a reusable workflow's typed interface; the key never starts a run itself (§3.4b) |
| `release` | `types:` | a release lifecycle action emitted by the releases service (§6.5) |
| `issues` | `types:` | an issue lifecycle action emitted by the issues service (§6.6) |
| `issue_comment` | `types: [created]` | a new issue **or PR conversation** comment (§6.6) |
| `pull_request_review` | `types:` (`submitted`, `dismissed`) | a review posted or dismissed by the review service; the run uses the PR's merge ref and the same fork trust rule as `pull_request` |
| `pull_request_review_comment` | `types: [created]` | a new inline review thread or reply (the review service's `OpenThread`/`AddThreadComment`/submitted review threads) |
| `label` | `types:` (`created`, `edited`, `deleted`) | a label **definition** changed by the issues service, not the `labeled` PR/issue metadata edge |
| `milestone` | `types:` (`created`, `opened`, `closed`, `edited`, `deleted`) | a milestone definition changed (a state flip is `closed`/`opened`; any other field is `edited`) |
| `fork` | none | a fork completed by pulls; the run is on the **original** repository, default branch, with `forkee` in the payload |
| `watch` | none (`action: started`) | a first star by social; a repeat star or an unstar emits nothing |
| `check_run` | `types:` (`created`, `completed`) | an **external** CI report on the checks surface. A Threads-mirrored check is the run itself and never re-fires this trigger |
| `schedule` | `- cron: "…"`, optional `timezone:` | the maintainer's cron sweep, **default branch only** |

**`on.pull_request.types`** — the filter selects the lifecycle edges that start a run. The
vocabulary is **closed to what the services emit**: `opened`, `synchronize`, `reopened`, `closed`
(including a merge), `edited` (title/body), `ready_for_review`, `converted_to_draft`, and the shared
thread-metadata actions `labeled`, `unlabeled`, `assigned`, `unassigned`, `milestoned`,
`demilestoned`. An absent (or null) `types` fires on every emitted action; a listed `types` is an OR
(`types: [opened, reopened]`). Two spellings are parse errors for that file, never silently dead
triggers:

- a type no service emits — `deleted`, `locked`, `review_requested` today — fails with
  `pull_request.types: unsupported type "deleted" (supported: …)`;
- `types: []` matches no action at all, so it fails with an explanation; omit `types` to fire on
  every edge.

**Filter dialect.** `branches`, `tags` and `paths` use the documented Actions patterns: `*` stops at
`/`, `**` crosses it, `?`/`+` quantify the preceding character, and an ordered `!` negates (the last
matching pattern decides). `branches-ignore`/`tags-ignore`/`paths-ignore` are exclusion lists;
naming both the include list and its ignore twin is a parse error.

**`paths` is the changed-file filter.** A docs-only push/PR does not fire `paths: ['src/**']`; a
mixed commit does. A push diffs its pre-image..new commit (a new ref uses the commit's own change
set); a PR diffs `base...head` (merge base). A path-filtered trigger never fires without a computed
diff (fail closed). The diff is one name-only `git diff` per event and is skipped entirely when no
discovered workflow declares a path filter — no store round trip is added.

```yaml
name: first look only
on:
  pull_request:
    branches: [main]
    types: [opened]   # skip synchronize and reopened
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
```

`on` accepts all three spellings, and a bare key is a real trigger:

```yaml
name: every push
on: push
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
```

```yaml
name: two events
on: [push, pull_request]
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
```

```yaml
name: filtered
on:
  push:
  pull_request:
    branches: [main]
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
```

**GitHub-only triggers (Wave 3).** Five events have no Forgejo twin and are
supported on their own terms:

- `repository_dispatch` — `POST /{owner}/{repo}/api/threads/dispatches` takes
  `{"event_type": "...", "client_payload": {…}}`; the caller needs the write role
  (GitHub's `contents: write`) and a Threads job token is refused. `event_type`
  matches `[A-Za-z0-9._-]{1,100}` and selects `on.repository_dispatch.types`.
  `client_payload` is bounded **before** any run exists: an object with at most
  10 top-level keys (`[A-Za-z0-9_-]{1,64}`), nesting at most 4 levels, scalar
  leaves, and 64 KiB encoded. It is exposed at
  `${{ github.event.client_payload.<key> }}` and in `GITHUB_EVENT_PATH`. The
  request runs the **default branch** only: a `ref` that does not resolve to the
  default branch tip is a 422, never a run from a branch another principal
  controls.
- `workflow_run` — filters `workflows` (upstream workflow **names**, exact and
  case-insensitive, not globs), `types` (`requested` when the upstream header
  commits, `completed` when it folds; **the default is `completed`**) and
  `branches`/`branches-ignore` on the upstream head branch. The follow-up reads
  its own file from the **default branch**, records `ParentRunID`/`ParentTrusted`
  and `ChainDepth`, and a chain stops at depth 3. A follow-up whose upstream run
  was **untrusted** (a fork PR) is created `pending_approval` with
  `ApprovalKind: workflow_run`: nothing starts, and cross-run artifact
  consumption is refused, until a maintainer approves (D-3). The artifacts API
  enforces that with `Run.State`/`ApprovalKind`; see §5.4.
- `create` / `delete` — emitted by the push hook for a branch or tag ref,
  carrying `ref` and `ref_type`. The workflow file is the default branch's, and
  a push authenticated by a Threads job token still starts nothing (the
  `GITHUB_TOKEN` loop rule).
- `deployment` / `deployment_status` — emitted when a job's deployment record is
  created or decided. `deployment_status.state` is `pending` (required
  reviewers), `in_progress` (approved) or `failure` (denied or failed). The file
  is read from the default branch, and the follow-up chain depth bounds a
  deployment workflow that deploys again.
- `pull_request_target` — D-2: the base tip's file with base secrets and a write
  token. Off per repository by default; `pull_request_target` on the Threads
  page (maintain+, audited) enables it. Inside it, an `actions/checkout` that
  names a `ref` or another `repository` is a **plan-time failure** naming
  `pr_target_allow_head_checkout` unless the second, separately audited switch
  is on. The run carries the `pr_target` badge, and the residual (a `run:` step
  can still fetch the head itself) is S5's.

**Merge-ref semantics.** Every `pull_request` edge — the code edges (`opened`, `synchronize`,
`reopened`, `closed`, `edited`, `ready_for_review`, `converted_to_draft`) and the metadata-only
edges (`labeled`/`unlabeled`, `assigned`/`unassigned`, `milestoned`/`demilestoned`) — has the run ref
`refs/pull/<n>/merge` and reports `GITHUB_SHA` as a server-computed merge commit of base
and head (`git merge-tree --write-tree` + `git commit-tree`, docs/go/04_git.md
§14a); `${{ github.ref }}` reads that run ref, while the job's `GITHUB_REF`
variable stays the base branch (§3.5). The issues service's metadata deliveries carry the PR number
into the same run-creation path as the lifecycle edges, so a merge-only failure goes red for either
kind. The workflow file is still
read from the head (same repository) or the base (a fork), so a fork run never
executes the contributor's workflow file. If the merge conflicts, or the git
reader cannot build one, the run falls back to the head commit and
`refs/pull/<n>/head` and says so in the log; the payload's
`pull_request.merge_commit_sha` is set only when a real merge commit exists.

**Named refusals.** The repository/check/deploy family has more names than
emittable events. Each of `status`, `check_suite`,
`registry_package`, `gollum`, `page_build`, `branch_protection_rule`, `public`,
`discussion`, `discussion_comment` and `pull_request_review_thread` is a **parse
error naming why it cannot fire** — never an accepted trigger that never runs.
`status` names the missing legacy
commit-status surface (this instance's checks are check runs, not statuses),
`check_suite` the missing app check-suite object (individual check runs are
emitted), and `discussion`(+comment) the missing discussion surface.
`merge_group` is no longer in this list: the trigger and its run contract are
implemented against the merge queue's documented ref shape (§6.10), and the
queue that creates those refs and lands the merge is
[`features/22_merge_queue.md`](features/22_merge_queue.md).

**The issue/review definition family.** `pull_request_review`,
`pull_request_review_comment`, `label` and `milestone` are emitted by the
service that owns the transition — never synthesized by the engine. Reviews and
inline comments run from the PR's merge ref with the same fork trust rule and
`pull_request` payload shape as the PR family; a fork PR's review run reads the
base workflow file and is untrusted. `label` and `milestone` are definition
events (triage), distinct from the `labeled`/`milestoned` PR/issue metadata
edges, and run the default branch's file. `fork` fires on the original
repository after the fork task commits, `watch` on a first star, and `check_run`
for an external CI report (`created`, then `completed` on a terminal state).
Threads' own mirrored check is the run itself and never re-fires `check_run`, so
a workflow cannot trigger a copy of itself.

**`workflow_dispatch.inputs`** — types `string` (default), `boolean`, `choice`, `number`,
`environment`:

| Field | Accepted | Notes |
|---|---|---|
| `description` | string | shown on the Run workflow form |
| `type` | `string` \| `boolean` \| `choice` \| `number` \| `environment` | anything else is a parse error |
| `required` | bool | a missing required input is `422` naming the input |
| `default` | scalar | carried as text; a boolean default reads `true`/`false` |
| `options` | list of strings | required for `choice`; a value outside the list is `422`. An `environment` input renders the repository's environment names as its picker and must name one |

`number` accepts a decimal number and keeps its authored text; `environment` is validated against
the repository's existing environments at dispatch time. `inputs.<name>` and
`github.event.inputs.<name>` read the same value for every type.

`${{ inputs.<name> }}` is substituted in step names, `run` scripts, step `env` values, step `if`,
`shell` and `working-directory`, and `inputs.<name>` is readable in `if`. A dispatch with `inputs`
must name exactly one workflow (the one whose schema applies).

**`on.schedule` — the supported cron subset.** Five fields, UTC unless the entry sets a
`timezone:`:

```
minute(0-59) hour(0-23) day-of-month(1-31) month(1-12) day-of-week(0-6, 7=Sunday)
```

- supported: `*`, a number, a range `a-b`, a step `*/N`, `a-b/N`, `n/N` (from `n` to the field
  maximum, every `N`), and comma lists of those;
- supported names and macros: `JAN-DEC`, `SUN-SAT` (`7` is Sunday), and `@yearly`/`@annually`,
  `@monthly`, `@weekly`, `@daily`/`@midnight`, `@hourly`;
- supported day modifiers: `L` (last day), `L-N`, `NW` (nearest weekday), `DL` (last weekday of the
  month) and `D#n` (the nth weekday);
- **not** supported (each is that file's parse error): a six-field expression with seconds, an
  unknown macro/name, and an invalid `timezone:`;
- an optional per-entry `timezone: <IANA name>` evaluates the expression on that local clock (absent
  means UTC). A spring-forward local time has no tick that day; a fall-back local time happens
  twice on the clock but is one watermark slot and fires **once**;
- day-of-month and day-of-week follow the classic Vixie rule: when both are restricted a day matches
  if **either** matches;
- `schedule:` with no entries, or a bare `on: schedule`, is an error.

```yaml
name: nightly
on:
  schedule:
    - cron: "0 3 * * *"
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
```

### 3.3 `jobs.<key>`

| Key | Accepted | Default | Notes |
|---|---|---|---|
| `name` | string | the job key | display name and the `<job>` segment of the check context |
| `runs-on` | label, or list of labels | required (unless the job is a reusable-workflow `uses`) | runner must have **every** label, case-insensitive. A label may be a `${{ matrix.<axis> }}`, `${{ inputs.<name> }}` or `${{ spindle.<field> }}` reference, expanded per cell before the job record is written (PAR-053); an empty resolution fails run creation. The `{group:, labels:}` mapping is refused by name. The hosted Linux labels (`ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, `ubuntu-20.04`) map onto `self-hosted` + `linux` before matching (§8.2); `windows-*`/`macos-*` are deliberately unmapped |
| `container` | image string, or mapping | runner's `--image` (`alpine/git:latest`) | mapping fields: `image`, `env`, `volumes`, `options`, `ports`, `command`, `entrypoint` — every one is **applied** (§4.1). `ports` publishes each container port on an ephemeral loopback host port; `entrypoint`/`command` own the container's process (the runner keeps it alive only when both are absent). `credentials` is **refused by name** (D-23): the registry pull identity is the runner operator's, never a workflow password. The image may be a `${{ matrix.<axis> }}`/`${{ inputs.<name> }}`/`${{ spindle.<field> }}` template, expanded per cell before the job record (PAR-030); a `container.env` value is literal |
| `services` | map of id → `image`/`env`/`ports`/`command` (Forgejo `cmd`)/`entrypoint`/`options`/`volumes` | none | one sidecar per entry, reachable at the id as a hostname; both `command` and Forgejo's `cmd` spelling set the argv tail, and `options` may carry the `--health-cmd`/`--health-interval`/`--health-timeout`/`--health-retries`/`--health-start-period` readiness probe (§4.2). Registry `credentials:` is **refused by name**: the pull identity is the runner's own (§8.6), and a workflow password would travel in the job payload and argv |
| `env` | map of string → string | empty | overrides the workflow `env` |
| `needs` | one job key, or a list of job keys | none | `needs: test` and `needs: [test]` are identical; must name existing jobs, no cycles; a matrix job's dependents wait for **every** cell |
| `defaults` | `{run: {shell, working-directory}}` | workflow `defaults` | the job's run-step defaults (PAR-054); a step's own key wins |
| `if` | expression string | implicit `success()`; with `needs`, every dependency must have succeeded | evaluated by the server before the job is claimable; `false` makes the job `skipped` (see §3.7). It may read `secrets.*`/`vars.*` (PAR-058): trusted runs read the values, untrusted runs read an empty secrets map |
| `timeout-minutes` | integer, or one `${{ … }}` expression | `360` | bounds the whole job (PAR-055); when it expires the job is cancelled, its containers removed and its conclusion reads `timed_out` (distinct from an operator `cancelled`). The expression resolves per cell from `github.*`/`matrix.*`/`inputs.*` and must be `1…360` |
| `continue-on-error` | bool, or one `${{ … }}` expression | false | a failing job stays `failure` (marked `tolerated`), but the run conclusion follows the other jobs; fail-fast does not fire from it, `needs.<job>.result` reads `success`, and dependents run. The expression is resolved **per matrix cell** at run creation from `spindle.*`/`matrix.*`/`strategy.*`/`inputs.*`, so `continue-on-error: ${{ matrix.experimental }}` tolerates exactly the cell that says true; a non-boolean result fails run creation (PAR-056). **Divergence (D-4):** Forgejo documents this key as *ignored*; Threads keeps GitHub's semantics because dropping a working behaviour would turn a green run red |
| `environment` | name string, or mapping `{name, url}` | none | the deployment environment this job targets (§5.6). The name must match `[A-Za-z0-9][A-Za-z0-9._-]{0,63}` and the environment must exist in the repository (an undefined one fails the job); `url` must be absolute http(s); a job without one is unchanged |
| `strategy.matrix` | axes, plus `include` / `exclude` lists of objects | none | product of scalar axes; `include` merges into cells whose axis values do not conflict (an object matching no cell becomes a new cell), `exclude` removes a base cell matching all its pairs; axes expand in name order; 256 cells max (§3.3a) |
| `strategy.fail-fast` | bool | `true` | cancels the sibling matrix cells when one fails |
| `strategy.max-parallel` | integer `1…256` | no per-matrix bound | how many cells of this matrix run at once (PAR-052); admission is in expansion order and the other cells wait queued |
| `steps` | list | required (unless `uses`) | see §3.4 |
| `concurrency` | group or mapping | workflow default | job-level wins |
| `permissions` | mapping or shorthand | workflow default | job-level wins; the closed scope vocabulary of §5.3.1 |
| `enable-openid-connect` | bool | workflow default | job-level grant; an explicit `id-token: none` wins |
| `uses` / `with` / `secrets` | reusable workflow | — | call a reusable workflow (local `./.spindle/workflows/<name>.yml`, or a pinned `owner/repo/...@<40-hex sha>`); its jobs join the run in place. The callee must declare `on.workflow_call`, whose typed inputs/secrets/outputs are the call's interface (§3.4b) |

**The server can refuse a job's images before any runner sees it.** Two `[threads]` keys are the
instance-wide image gate (`docs/PLAN-ARTIFACT-KEEPER.md` L2): `image_allow_prefixes` (a list of
literal `host[:port]/path` prefixes; empty = unrestricted, the default) and `image_require_digest`
(require `@sha256:` on those images). The check covers `container.image`, every
`services.<id>.image` and every `docker://` image a step or a local docker action names (§3.4a), for
every trigger class — a same-repository pull request runs a workflow file
from the PR head with the repository's secrets, so it can name any image unless the server says no;
a fork run takes the same check and merely never receives the secrets. A host-less reference is
matched both literally and in its canonical Docker Hub spelling, so `docker.io/library` allows
`golang:1.27` as well as `docker.io/library/golang:1.27`.

A refusal is a **failed job** with a `reason` naming the image and the allowlist (never a silently
disabled workflow, and never a secret): the run folds to `failure` and the check mirror is red. The
gate is re-applied on **re-run**, so a policy enabled after a run was created cannot be bypassed by
the re-run button. A job a job-level `if:` already skipped pulls no image and is not refused. The
policy governs workflow-named images only — a runner's own `--image` is the operator's declaration
and belongs in the runner's unit or container definition, pinned by digest. Both keys are
restart-required and live on Settings → Administration → Configuration (`threads.*`); a policy that
cannot be evaluated (an invalid entry) refuses the job rather than allowing it (fail closed).

The matrix product is capped at **256 cells**; each cell gets its own job id, check context and
`MATRIX_<AXIS>` environment, and is named `key (v1, v2)` — unless the job names a matrix axis
itself (`name: cell-${{ matrix.v }}` renders `cell-a`; §3.6). `strategy.max-parallel: N` admits at
most N cells of one matrix at a time (PAR-052): the admission slot is taken before the queue claim
and released when the cell finishes or requeues, so a six-cell matrix with `max-parallel: 2` never
has three cells running. It is a parse error outside `1…256` or without a matrix.

```yaml
name: Build
on:
  push:
    branches: ["main", "release/*"]
env:
  GOFLAGS: "-mod=readonly"
permissions:
  contents: read
jobs:
  test:
    name: test
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    timeout-minutes: 20
    concurrency:
      group: test-${{ matrix.go }}
      cancel-in-progress: true
    strategy:
      matrix:
        go: ["1.26", "1.27"]
      fail-fast: false
    services:
      postgres:
        image: postgres:17
        env:
          POSTGRES_PASSWORD: ci
        ports:
          - "5432"
    steps:
      - uses: actions/checkout
      - name: unit
        env:
          GO_VERSION: ${{ matrix.go }}
        run: |
          go version
          go test ./...
  publish:
    name: publish
    runs-on: linux
    needs: [test]
    permissions:
      contents: write
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - name: push the build branch
        shell: sh
        working-directory: .
        run: |
          git config user.name ci
          git config user.email ci@example.com
          date > build.txt
          git add build.txt
          git commit -q -m "ci: build"
          git push origin HEAD:refs/heads/ci-output
```

That file produces three jobs on a push: `test (1.26)`, `test (1.27)` (each check context
`threads/build/test-1.26` / `threads/build/test-1.27`) and `publish`, which waits for both.
`publish` may push because it declares `permissions: contents: write` **and** checks out full depth.

#### 3.3a `strategy.matrix.include` and `exclude`

The matrix is the product of the scalar axes plus GitHub's two lists. The expansion is fixed and
deterministic (the job ids and check contexts depend on it):

1. **Base:** the product of the non-empty axes (axes in name order, values in declaration order).
2. **`exclude`:** an object removes every base cell that matches **all** of its key/value pairs.
3. **`include`:** each object is merged into every **surviving base** cell whose axis values do not
   conflict with it (a key that is an axis must already hold the same value). Keys that are not
   axes are added, and may overwrite a value an earlier include entry added.
4. An include object that merges into no base cell becomes a **new** cell of its own, in
   include-list order. It is never merged into a cell an earlier include entry created, and an
   excluded cell is never resurrected.

A matrix made only of `include` entries (no axes) yields one cell per entry — the Scriobh release
shape:

```yaml
name: Cross build
on: push
jobs:
  build:
    runs-on: linux
    strategy:
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
    steps:
      - run: echo "${{ matrix.goos }}/${{ matrix.goarch }}"
```

Two cells, named `build (amd64, linux)` and `build (arm64, linux)` (the display order is axes in
name order, then include-only keys in name order — the values are the combination, not a promise
about authoring order). A list or object value inside an include/exclude
entry is a parse error, and the expansion is capped at **256 cells** (also a parse error).

```text
strategy:
  matrix:
    go: ["1.26", "1.27"]
    os: [linux, darwin]
    exclude:
      - { go: "1.26", os: darwin }   # one cell removed
    include:
      - { experimental: "true" }     # merged into every surviving cell
```

### 3.4 `steps`

| Key | Accepted | Default | Notes |
|---|---|---|---|
| `name` | string | `Step <n>` | `${{ matrix.* }}` / `${{ inputs.* }}` substituted |
| `run` | string | — | exactly one of `run` / `uses` |
| `uses` | `actions/checkout`, `actions/upload-artifact`, `actions/download-artifact`, `actions/cache` (`/restore`, `/save`), `./<path>` (a local action, §3.4a), `owner/repo[/path]@ref` / `https://host/owner/repo[/path]@ref` (a remote action, §3.4e), `docker://image[:tag\|@digest]` (§3.4a) | — | a remote reference is **default-deny**: it must resolve from this instance (or the one configured source), and a mutable ref outside the allowlist is a named refusal requiring a full commit SHA (§3.4e). A `setup-*` cache input uses the documented `actions/cache` recipe (§3.4d.1) |
| `with` | map of string → string | — | per action: checkout reads `fetch-depth`; the artifact and cache actions accept exactly the keys below (an unknown key fails the step — it is never ignored) |
| `shell` | `bash` (default), `sh`, `python`, `python3`, `pwsh`/`powershell`, `cmd`, or an arbitrary command template containing exactly one `{0}` (e.g. `dash -e {0}`) | `bash --noprofile --norc -eo pipefail` | the default is GitHub/Forgejo's Linux default (D-24); `sh` runs `sh -e -c`; `python`/`pwsh`/`cmd` get their documented argv; a custom template is split into argv words — never handed to a shell — and `{0}` is a script file written inside the container. An image without bash runs the step with `sh -e` and one log line says so (§4.1a). Anything else fails the step explicitly |
| `working-directory` | path | the workspace | relative paths join the workspace; absolute paths win |
| `env` | map of string → string | empty | overrides job and container env; `${{ }}` substituted |
| `if` | expression | implicit `success()` | the documented subset in §3.7; an unsupported expression **fails the step** with the parse error |
| `continue-on-error` | bool | false | the step may fail without failing the job; the result still records `failure` |
| `timeout-minutes` | integer, or one `${{ … }}` expression | the remaining job time | on expiry the step's container (a foreground step) or its own process group (a background step, §3.4c) is killed and the step concludes `timed_out` with `timed out after …` (PAR-055); the expression reads `github.*`/`matrix.*`/`inputs.*` and must be `1…360` |
| `id` | GitHub identifier: letters, digits, `_` and `-`, not starting with a digit | — | unique within the job (a duplicate is a parse error naming the job and the id); `steps.<id>.outcome` / `.conclusion` read it, and only a step with an id can publish outputs (§3.9) |
| `background` | bool | false | start the step asynchronously and continue; requires an `id`, and is accepted only on a `run`/`uses: docker://` step or a local docker action (§3.4c). The process runs in its own session inside the job container |
| `wait` | background step id, or `all` | — | a control step: wait for that background step (or every outstanding one) and fail if it failed |
| `wait-all` | bool | false | the boolean spelling of `wait: all` |
| `cancel` | background step id, or `all` | — | a control step: terminate the background step's process group (SIGTERM, then SIGKILL) |
| `parallel` | list of steps | — | run the members concurrently as one unit; a member is an ordinary `run`/`uses` step (nested groups and background/wait members are refused by name) |

A failed step (without `continue-on-error`) skips the remaining steps — each recorded as `skipped` —
and fails the job.

#### Artifact steps (`actions/upload-artifact` / `actions/download-artifact`)

Artifacts pass files from one job of a run to another inside the same run (a `needs:` dependency).
They travel over the authenticated job channel: the runner lists the job container's files, streams
them out as a tar through `docker exec`, and uploads each file with the job token; download is the
reverse (one tar streamed into `tar -xf -`). The job container never talks to the object store, and
nothing is written to the runner's disk.

```yaml
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: make dist
      - uses: actions/upload-artifact
        with:
          name: build-out
          path: |
            dist
            coverage/*.out
          if-no-files-found: error
          retention-days: 7
  publish:
    needs: [build]
    runs-on: linux
    steps:
      - uses: actions/download-artifact
        with:
          name: build-out
          path: in
```

| Key | Action | Accepted | Default | Notes |
|---|---|---|---|---|
| `name` | both | letters, digits, `.`, `_`, `-`; not starting with `.`/`-`; ≤ 255 bytes | `artifact` (upload); *every* artifact (download, v4) | **unique per run**: a second upload of one name fails the step with `artifact "…" already exists in run …` unless `overwrite: true` |
| `path` | upload | one or more patterns (newline- or comma-separated), relative to the workspace | required | see the pattern rules below |
| `if-no-files-found` | upload | `warn`, `error`, `ignore` | `warn` | `warn` logs a line and succeeds with no artifact; `error` fails the step; `ignore` says nothing |
| `retention-days` | upload | `0` (the artifact window) or `1`…`90` | `0` | the artifact's OWN lifetime: `created + retention-days`. It may outlive the run's `threads.retention` window (the two are separate, §9.2); an expired artifact is 404 to a reader before GC frees it |
| `overwrite` | upload | `true`, `false` | `false` | replaces an existing artifact of the same name **in the same run**. The runner opens the upload with the server (`prepare`) before streaming, so the old blob keys are freed first; without it a second upload is a conflict |
| `include-hidden-files` | upload | `true`, `false` | `false` | v4's default excludes any path whose segment starts with `.`; `true` includes them |
| `compression-level` | upload | `0` (store) … `9` | `6` | shapes the **ZIP download** only: stored blobs are always the raw bytes. `0` stores, `1`–`9` deflate at that level |
| `path` | download | a workspace-relative directory (absolute container paths are taken as-is) | the workspace | with `name`, the artifact's files land directly under it; without `name`, each artifact lands in `<path>/<name>/` unless `merge-multiple` |
| `pattern` | download | a glob over artifact NAMES (`dist-*`) | — | selects every artifact whose name matches; mutually exclusive with `name` and `artifact-ids` |
| `artifact-ids` | download | comma-separated positive integers | — | selects by each artifact's stable `id` (the list API and the run page show it) |
| `merge-multiple` | download | `true`, `false` | `false` | extracts every selected artifact into ONE tree instead of one directory per artifact. It needs `pattern` or `artifact-ids` (a single named artifact already lands in `path`) |
| `run-id` | download | another run's id in THIS repository | this run | **cross-run consumption requires an approval** (§3.4, *Cross-run artifacts*): the consuming run is held in `pending_approval` and a maintainer must approve it. A fork run and a job holding an organization/repository/environment secret are refused by name |

The v4 inputs that do not make sense here are refused **by name** rather than ignored:
`github-token` (the step already authenticates with the job token) and `repository` (an artifact can
only be read from its own repository).

**Supported `path` patterns** (case-sensitive, matched against workspace-relative slash-separated
paths):

- an exact file path (`dist/app.tar.gz`);
- a directory (`dist`): every regular file beneath it, recursively;
- a glob where `*` and `?` stay inside one path segment and `**` spans any number of segments
  (`dist/*.js`, `dist/**/*.js`, `dist/**/test-?.log`, `dist/[ab].txt`);
- `.` (the whole workspace).

A glob that matches a directory includes the files beneath it (the GitHub rule). Only regular files
are uploaded: symlinks are neither followed nor archived, so a pattern can never publish bytes from
outside the workspace. Absolute paths, `~`, and `..` segments are named step errors. The job image
must provide `find` and `tar` (busybox provides both; the default `alpine/git` image has them) — a
missing tool fails the step naming it.

**Limits** (configurable, enforced by the server and pre-checked by the runner; a refusal is a named
step error):

| Limit | Config key | Default |
|---|---|---|
| one file | `threads.max_artifact_bytes_per_file` | 256 MiB |
| all files of one artifact + every other artifact of the run (upload and download) | `threads.max_artifact_bytes_per_run` | 2 GiB |
| files per artifact | — (fixed) | 1000 |

**Trust and masking.** A fork pull request's run may upload artifacts exactly like a trusted run,
and they stay in the **base repository** (the run's repository) under the same read gate as its
logs; the run page marks the run `untrusted`. Secret masking applies to a step's LOG LINES,
including the artifact name echoed there — artifact **contents are the user's data** and are never
rewritten: a job that writes a secret into a file publishes that secret to every repository reader.
An authenticated repository reader (or an anonymous reader of a public repository) can list and
download; the attempt token can only read artifacts of its own run — or, with the approval below,
another run's in the same repository.

**Artifact API.** `GET …/api[-browser]/threads/{run}/artifacts` lists the run's live artifacts
(`id`, `name`, `files`, `total_size`, `created_at`, `expires_at`), `GET …/artifacts/{name}` returns
one manifest, and `DELETE …/artifacts/{name}` removes the artifact (a **write** role) and reports
`bytes_freed` — the blobs, the manifest and the expiry row, exactly what the retention sweep would
free. A retention window that has closed is a 404 on both reads even before GC runs. The run page
links the ZIP download (`compression-level` above); a manifest path that escapes the archive root is
refused on extraction and on the ZIP writer, so a corrupted record can never become a zip-slip.

#### Cross-run artifacts (`run-id`, D-3)

`download-artifact` may name another run of the **same repository** with `run-id`. That is the
untrusted-parent vector: bytes one run produced become input to a second run. The rule is a
**per-run maintainer decision**, never automatic:

1. At creation, a job whose steps name `run-id` is checked. A **fork** run is refused by name ("a
   fork run cannot download another run's artifacts"), and so is a job that references a repository
   or environment secret ("a secrets-bearing job cannot download another run's artifacts"). The
   built-in job token does not count as a secret; an `environment:` job does.
2. Any remaining consumer holds the whole run in `pending_approval` (`approval_kind: cross_run`).
   The hold IS the queue: no runner can claim a job until a maintainer (or admin) approves it.
3. Approving re-validates the same conditions and audits `threads.artifact.cross_run.approve`; every
   cross-run list/manifest/blob read is audited too. A re-run consumes again, so it is held again.
4. The source run must exist in the consuming run's repository. A cross-repository read is
   structurally impossible (the bucket prefix is the repository) and is refused by name if a client
   asks for one.

**The `workflow_run` hold (PAR-019).** A `workflow_run` consumer whose upstream run was untrusted is
held with `approval_kind: workflow_run`. While that hold is pending, a cross-run artifact download —
and attaching a `run-id` input, which is the same lookup — is refused with **409** naming the parent
run and `workflow_run approval pending`; it is never treated as "not approved yet" and never queues
work. The parent's approval is the same D-3 decision, so it releases the run and satisfies cross-run
consumption for that run; the dedicated `cross_run` hold remains the spelling for every other event.

A run that also needs an **environment** approval cannot carry the cross-run hold (one hold per run);
its cross-run step fails at download time naming the rule, so split it into its own workflow.

### 3.4a Local actions and `docker://` images

Beyond the built-in actions, a step may name a **repository-local action** or a **docker image**:

```yaml
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: setup
        name: toolchain
        uses: ./.spindle/actions/setup     # a directory in THIS repository
        with:
          version: "1.4"
      - run: make test
        env:
          TOOL: ${{ steps.setup.outputs.bin }}
      - uses: docker://alpine:3.20          # the image's own entrypoint
        with:
          who: world                        # → INPUT_WHO=world
      - uses: ./.spindle/actions/lint        # a local docker action
        id: lint
```

**Supported `uses` forms.** `actions/checkout`, `actions/upload-artifact`,
`actions/download-artifact`, `actions/cache` (+ `/restore`, `/save`) (the built-ins above),
`./<path>` (a local action), `owner/repo[/path]@ref` (a remote action, §3.4e) and
`docker://<image>[:tag|@digest]` (an image). A local action's path is any repository-relative
directory; `..`, absolute paths, backslashes and `:` are refused (the path is a git object path,
never a host path). Remote references are resolved, pinned and recorded at run creation (§3.4e);
they are never fetched by the runner and never from a host the operator did not configure.

**Resolution and pinning.** Local actions are resolved **at run creation, from the run commit** —
the same commit the job checks out. The parsed definitions are embedded in the job payload
(`Spec.Actions`), and the runner never reads a repository, so a run cannot be made to execute an
action file from another ref. The run page shows the expanded steps before anything runs. A
definition is *not* re-read when a step runs: if an earlier step edits `action.yml` in the workspace,
this run still executes the pinned definition (the next run, at a new commit, uses the new file).
What is **not** sandboxed is the action's `run:` scripts: they execute in the job container as
repository code, with the same trust as a workflow script step (the job token and secrets that a
script step receives). A compromised composite action file is a compromised job; review it
like a workflow file. A fork pull request resolves the local actions from the **PR head** (the
checked-out commit) and the run is untrusted: no secrets, no push credential — and a docker action
never receives either regardless (§5.2).

**Action metadata (`action.yml` / `action.yaml`).**

```yaml-action
name: Setup toolchain
description: install and configure
inputs:
  version:
    description: tool version
    required: true
    default: "1.0"
outputs:
  bin:
    description: installed path
    value: ${{ steps.install.outputs.bin }}
runs:
  using: composite
  steps:
    - id: install
      run: ./install.sh "$INPUT_VERSION"
      shell: bash
```

Accepted keys: `name`, `description`, `author`, `branding` (display-only, dropped), `inputs`
(`description`, `required`, `default`), `outputs` (`description`, `value` — a composite action only)
and `runs`. `runs.using` is required and is `composite`, `docker`, `node20` or `node24` (§3.4e);
`node12`/`node16` is a named end-of-life refusal, and anything else is refused by name. Unknown keys
are errors, as in workflow files. A `docker` action's `runs.image` is a `docker://` registry image or
a Dockerfile path inside the action (`image: Dockerfile`, built by the runner, §3.4e); `node`
actions declare `main`, and optionally `pre`/`post` with `pre-if`/`post-if`.

**Composite actions.** `runs.steps` expands **in place** in the job's step list (the run page shows
the inner steps, each carrying the calling step's name as its parent). Rules:

- each inner step has exactly one of `run` or `uses`; a `run` step **must** name a `shell:`
  (`sh`, `bash`, `python`/`python3`, `pwsh`/`powershell`, `cmd`, or the arbitrary `{0}` template —
  the same resolution as a workflow step, §4.1a);
- `uses` is `./<path>` (another local action), `docker://<image>`, or — inside a **fetched** remote
  composite — another remote reference (§3.4e), resolved recursively under the same depth cap. The
  built-in actions are refused inside a composite (checkout belongs in the job, not in an action).
  A **local** action's metadata still refuses a remote `uses:` at parse time: a local action is
  repository code and its marketplace dependencies must be explicit in the workflow;
- the caller's `with:` values become `INPUT_<NAME>` environment variables (uppercase, spaces →
  underscores: `dry run` → `INPUT_DRY_RUN`), with the action's declared `default`s applied; a `with:`
  key the action does not declare is a **named refusal** (a typo must not silently do nothing).
  Inside the action, `${{ inputs.<name> }}` is substituted in its step fields against the action's
  **own** declared inputs (an undeclared name is a parse error), and the `INPUT_<NAME>` environment
  variables are the shell-facing spelling of the same values;
- the caller's `env:` applies to every inner step, `INPUT_*` wins over it, and a nested action sees
  only its **own** `INPUT_*` names (an enclosing action's input names are not its interface); an
  inner `working-directory` is relative to the caller's (which defaults to the workspace);
- inner steps support `if`, `env`, `working-directory`, `continue-on-error` and `timeout-minutes`;
  an inner failure fails the calling step and skips the rest of the action (the caller's
  `continue-on-error` then decides whether the job tolerates it), and a failing inner step inside an
  action is `failure` for that action's later `if:` expressions;
- `steps.<id>` inside the action resolves the action's own steps (action-local ids are a separate
  namespace; a local id shadows a job id of the same name inside the action), and the action's
  `outputs:` mapping is evaluated from them and published under the **calling** step's id — so
  `${{ steps.setup.outputs.bin }}` works when the composite step has `id: setup`;
- `${{ github.action_path }}` is the composite action's **own** directory (PAR-074), and it is
  **per composite level**: for a step inside the action at repository directory `D`, the runner sets
  `github.action_path` to the absolute workspace path `<workspace>/D`. A nested local action's steps
  see the nested action's path, never the outer one; a workflow step, a job `if:` or an output
  template that reads it is a named parse error (the value exists only inside an action, so the
  field is accepted only in that scope).
- `outputs.<name>.value` may read only the action's own `steps.<id>.outputs.<x>` (never
  `github.action_path`, and never a workflow step id); the advertised `inputs.*`/`steps.*` scope is
  closed at parse time.
- nesting is limited to **10** action levels and cycles are refused, both at run creation (the job
  fails with a reason naming the chain) and again by the runner (it never trusts the payload, so a
  hostile spec cannot loop it). A nested remote action inside a fetched composite is resolved at run
  creation and embedded like any other; its steps see their own `github.action_path` (PAR-074).

**`docker://` actions.** The image runs as **its own container** on the job's network, with the job
workspace volume mounted at the same path and the step's working directory. It uses the image's own
entrypoint and command unless the action metadata names `runs.entrypoint`/`runs.args` (an inline
`docker://` step has no metadata, so it always follows the image). `with:` keys become `INPUT_*`.
Environment: the job/step environment plus `INPUT_*` — **never** the job token and never a repository
secret, so the fork invariant (read-only, no secrets) holds for third-party images. The container
gets **no host docker socket, no `--privileged`, no added capabilities**, and no user-supplied
mounts; the job's network is its network policy. A docker action's `$SPINDLE_OUTPUT` entries are
copied out of its container and published under the calling step's id (a docker action declares no
`outputs:` mapping — it has no steps to read one from; its output file *is* the output set). A step
that wrote nothing publishes nothing; an image whose `/tmp` cannot be read fails the step naming the
container (fail closed — a copy that silently found nothing would look like "no outputs"). A step
timeout removes the action container (the job container's process tree is not touched by a
third-party image).

**The image gate has no hole.** Every `docker://` image — an inline step, a local docker action, or a
`docker://` step inside a composite — goes through the **same** `threads.image_allow_prefixes` /
`threads.image_require_digest` policy as a job container (and the runner's own `--image-prefix`,
`--image-pull`, `--image-require-digest` apply at plan time). A refusal is a failed job with a
`reason` naming the step and the image, decided **before anything is enqueued**: no network, volume,
job container or action container is ever created for it.

**Not supported:** a remote `uses:` inside a **local** composite's metadata (the parser names it),
and action `pre`/`post` hooks on a composite action (they are a JavaScript-action feature, §3.4e).
A Dockerfile action is supported for remote actions and builds on the runner host (§3.4e).

### 3.4b Reusable workflows

A job may **call another workflow** instead of declaring steps — a file in the same repository, or
a pinned file in another repository on this instance:

```yaml
# .spindle/workflows/ci.yml
name: CI
on:
  push:
    branches: [main]
jobs:
  test:
    runs-on: linux
    steps:
      - run: go test ./...
  deploy:
    uses: ./.spindle/workflows/deploy.yml   # or owner/repo/.spindle/workflows/deploy.yml@<sha>
    needs: [test]
    permissions:
      contents: read
    with:
      target: production
    secrets:
      REGISTRY_TOKEN: ${{ secrets.REGISTRY_SECRET }}
  smoke:
    needs: [deploy]
    runs-on: linux
    steps:
      - run: curl "${{ needs.deploy.outputs.url }}"
```

The callee is a workflow file that declares `on.workflow_call`, the call's interface:

```yaml
name: Deploy
on:
  workflow_call:
    inputs:
      target: {type: string, required: true}
      dry-run: {type: boolean, default: false}
    secrets:
      REGISTRY_TOKEN: {required: true}
    outputs:
      url:
        value: ${{ jobs.release.outputs.url }}
jobs:
  release:
    runs-on: linux
    outputs:
      url: ${{ steps.publish.outputs.url }}
    steps:
      - id: publish
        run: echo "url=https://$TARGET.example" >> "$GITHUB_OUTPUT"
        env:
          TARGET: ${{ inputs.target }}
          TOKEN: ${{ secrets.REGISTRY_TOKEN }}
```

Its jobs are expanded into the same run, in place, before anything is written: each becomes its own
job record, check and log, named `<caller> / <callee>`. `needs:` inside the callee is rewritten to
the expanded keys, a callee job with no `needs:` inherits the caller's, and the caller's `if:` is
conjoined with every callee job's own condition. The run page therefore shows exactly what ran —
there is no hidden parent job.

Rules, all enforced (a parse error or a **failed job with a named reason**, never a silently ignored
key):

- **The callee must declare `on.workflow_call`.** Its typed inputs (`string`, `boolean`, `number`,
  with `required`/`default`) are the only `with:` names; an unknown name, a missing required input
  with no default, and a value that does not parse as its type are named refusals on the caller job.
  Declared defaults are applied, so `inputs.<name>` is always present for a declared input.
- **`with:` is expanded once at run creation** from `github.*` and the caller's own `inputs.*`. A
  `secrets.*`, `needs.*`, `matrix.*`, `env.*` or `runner.*` reference there is a parse error.
- **`secrets:`** is an explicit `callee-name: ${{ secrets.SOURCE }}` map (a literal is a parse error)
  or `inherit`. The callee's `on.workflow_call.secrets` is its closed set: `secrets.<name>` for an
  undeclared name is a parse error, and a `required` declaration the call does not pass is a named
  refusal. A same-repository callee with no `secrets:` block receives the run's resolved secret set
  (all three scopes, §5.1b; implicit inherit); a cross-repository callee with no block receives
  **none**.
- **`secrets: inherit` never crosses a repository boundary** (decision **D-6**): it is a named
  refusal. A cross-repository callee receives exactly the listed secrets, renamed to its own
  declarations; the claim's secret map is filtered to that set, so no other secret
  reaches the callee's process. A nested call only passes on secrets its own caller handed it: a
  name remapped at each level resolves transitively to the run repository's secret, and a callee
  cannot name a secret that never reached it.
- **A callee's `outputs:` are the caller job's outputs.** Each is a template over the callee's own
  jobs (`value: ${{ jobs.<id>.outputs.<x> }}`, and that job must declare the name), evaluated when
  the call's jobs finish; a later job reads `needs.<caller job>.outputs.<name>`. Reading a name the
  callee does not declare is a named refusal on the reading job. If any callee job does not succeed,
  the call exposes no outputs.
- **`permissions:` is downgrade-only**: every callee scope is capped at the caller job's effective
  scope (a scope the caller does not grant is `none` for the callee).
- A caller job is a *call*, not a runner job: `steps`, `runs-on`, `container`, `services`, `env`,
  `strategy`, `timeout-minutes`, `concurrency`, `environment` and `outputs` are refused by name.
- **Cross-repository `uses:`** is `owner/repo/.spindle/workflows/<name>.yml@<40-hex commit>`. The
  callee resolves only when its repository is readable without credentials on this instance
  (otherwise the refusal says so — that is the authority this engine does not carry yet), and the
  reference must be a full commit SHA: a branch or tag is refused because it changes under the run.
  Every cross-repository call writes one host-audit record (`reusable_cross_repo`: caller, callee
  `repo@commit`, job, run, the passed secret **names**) — never a secret value. A URL, a
  `docker://` image and a foreign workflow directory are refused.
- Nesting is limited to **4** reusable levels and a `(repository, path)` cycle is refused by name.
  The callee is read at the run's commit, so a later edit cannot change what a run executes.
- A callee that fails to read or parse is a named refusal on the caller's job — the run continues
  for the other jobs, like a bad local action.

Not supported today, each a named refusal rather than inert syntax: a matrix caller (`strategy:` on a
caller job), environment secrets inherited by a callee, and a `needs.<caller>.outputs.<name>`
reference to a nested call's job (read the outermost call's declared outputs).

### 3.4c Background, `wait`/`cancel` and parallel steps (PAR-080, D-7)

Long-running sidecars — a dev server, a watch build — can outlive their step, and independent steps
can run as one group.

```yaml-fragment
jobs:
  test:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: server
        name: dev server
        run: npm run dev -- --port 3000
        background: true
        timeout-minutes: 5
      - name: wait until it answers
        run: |
          for i in $(seq 1 30); do
            curl -fsS http://127.0.0.1:3000/health && exit 0
            sleep 1
          done
          exit 1
      - name: smoke
        run: npm run smoke
      - name: stop the server
        cancel: server          # optional: the job end kills it anyway
      - name: two independent linters
        parallel:
          - run: ./bin/lint-go
          - run: ./bin/lint-web
```

**The process-group rule.** A `background: true` step is started in its **own session and process
group** inside the job container (`setsid`), so a cancel, a timeout or the end of the job reaches the
command *and everything it forked* with SIGTERM, then SIGKILL. The runner verifies through `/proc`
that no live member survived and reports a process that did not stop as a job failure — a leaked
process is never silently inherited by the next job on a capacity-N runner. The image must provide
`setsid` (util-linux or busybox); a `background` step on an image without it fails by name rather than
starting an unkillable process.

**What happens when.** A background step keeps running across the steps after it until a step `wait`s
for it (`wait: <id>`, `wait: all`, `wait-all: true`) or `cancel`s it, or the job ends. The job end
kills every remaining background unconditionally, on every conclusion (success, failure, cancel,
timeout, panic), before the containers are removed. A background step that exits non-zero is surfaced
**at the wait** (the wait step fails with its exit code) and, if it was never waited for, at the job
end as a named failure — it is never dropped because the job moved on. A background that is still
running when the job ends is `cancelled` (cleanup), not a failure. A background's own
`timeout-minutes` kills its process group and reports `timed out after …` at the wait or job end.

**Output.** A background step's output is buffered and flushed as one block when the step is joined
(waited for, cancelled, or at job end), so a dev server cannot interleave unbounded output into later
steps. It is capped at 1 MiB per background step (a truncation line is logged), and a `parallel`
member at 4 MiB. Masking applies to both exactly as to a foreground step.

**Parallel groups** run their members concurrently and report the group as one unit followed by the
member rows, in declaration order; logs are buffered per member and flushed in the same order. The
group fails when a member fails without `continue-on-error` (one member's failure does not cancel its
siblings). Members share the workspace — two members writing one file race, which is the documented
GitHub behaviour and the same warning it carries. `$GITHUB_ENV`/`$GITHUB_PATH` writes from
background and parallel steps are applied in declaration order after the group/step is joined, never
by completion order.

**Parser.** The workflow parser accepts exactly the runner's model — `background` (bool),
`wait` (a background id or `all`), `wait-all` (bool), `cancel` (id or `all`) and `parallel`
(a non-empty list of ordinary steps) — and refuses the same invalid combinations by name at parse
time: `wait`/`wait-all`/`cancel` together, a control step with `run`/`uses` or an `id`, a
`wait`/`cancel` naming a step that is not a background started earlier, a background without an
`id`, a background on a built-in action, a `parallel` with `run`/`uses`, no members, a nested group,
or a background/control member, and any of these keys inside a composite action. A duplicate id is
the usual parse error; a self-reference is the control-step refusal. A local `uses: ./…` action is
accepted as a background form because only the action file (read at run creation) says whether it is
docker (allowed) or composite (refused by the runner at plan time).

### 3.4d Cache steps (`actions/cache`)

`actions/cache` (and its split forms `actions/cache/restore` and `actions/cache/save`) restores and
saves a directory tree across runs:

```yaml-fragment
- uses: actions/cache
  with:
    path: .npm                 # workspace-relative, one per line
    key: npm-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}
    restore-keys: |
      npm-${{ runner.os }}-${{ github.ref_name }}-
      npm-${{ runner.os }}-
```

| Key | Accepted | Notes |
|---|---|---|
| `path` | one or more workspace-relative paths (newline- or comma-separated) | **absolute paths and `~` are refused by name**: a tool's default cache (`~/.npm`, `~/.cache/pip`) lives in the container's ephemeral home, which the next job does not have. Point the tool at a workspace directory instead (the per-ecosystem recipes are in §3.4d.1) |
| `key` | ≤ 512 bytes, one line | required. A save of the same `(scope, key, version)` supersedes the earlier entry |
| `restore-keys` | up to 10 prefixes, one per line | tried in order after an exact miss, most recently saved match first, within the run's readable scopes |
| `fail-on-cache-miss` | `true`/`false` | `true` fails the step on a miss; the default (`false`) makes a miss a normal, silent-success restore |
| `lookup-only` | `true`/`false` | `true` restores nothing and saves nothing (it only reports `cache-hit`) |
| `enableCrossOsArchive` | refused | every Threads job is Linux; a cross-OS archive cannot be produced here |

The **version** is derived, not authored: sha256 of the sorted path list plus the archive format, so
changing the cached paths misses instead of restoring a tree that no longer matches. The archive is
a plain tar; the payload is stored **content-addressed** (sha256) and integrity-checked end to end on
restore. `actions/cache` restores at the step and saves **at the end of the job**, only when every
step succeeded; `actions/cache/save` saves immediately; `actions/cache/restore` never saves. A save
failure is logged and never fails a green job. A **miss is not a failure**.

The step publishes `cache-hit` (`"true"` only on an exact key match; a `restore-keys` fallback is
`"false"`), readable as `steps.<id>.outputs.cache-hit`.

**Scopes (D-10) — the security model.** A run's scopes are derived from its own header, never from a
request parameter:

| Run | Writes | Reads |
|---|---|---|
| trusted push/dispatch on the **default** branch | `default` | `default` |
| trusted push/dispatch on another branch | `branch:<name>` | its branch, then `default` |
| pull_request (same repository) | `pr:<n>` | `pr:<n>`, then its **base** branch (`default` when the base is the default) |
| pull_request (fork) | `pr:<n>:untrusted` | `pr:<n>:untrusted`, then its base branch |
| tag push (no branch) | — (read-only) | `default` |

Consequences, each pinned by a test: a **PR run can never write a base-readable entry**, a fork's
entry lives in the `:untrusted` half **no trusted run reads**, and **no PR reads another PR's**
cache. Keys may collide across scopes; the payload is content-addressed and integrity-checked, so a
collision can only select bytes the writer actually produced. A guessed digest is not readable
either: the payload must belong to an entry the run can see. A merged pull request's two scopes are
discarded when the merge lands (the pulls `closed`+`merged` event), and anything a merge misses ages
out through the retention window.

**Limits and eviction** (config keys in §9): `threads.cache_max_entry_bytes` (default 1 GiB, clamped
to the repo budget), `threads.cache_max_bytes` (the repository budget, default 5 GiB),
`threads.cache_max_entries` (default 256) and `threads.cache_retention` (default 7 days idle). A save
that would exceed the budget evicts **oldest-saved-first** inside the same index write; the entry
being saved is never the one evicted. The sweeper (`threads-gc`) also reclaims expired entries and
orphaned payloads (an upload whose index write lost a race), and reports `caches=… cache=…B` in its
per-family line.

**Bucket layout** (no state outside the object store):

```
repos/<o>/<r>/threads/caches/index.json          the repo index (the commit point, a CAS document)
repos/<o>/<r>/threads/caches/blobs/<sha256>      content-addressed cache payloads
```

Lookup and save are exact-key operations (one conditional GET of the index); the only prefix LIST in
the cache engine is the sweeper's orphan pass.

#### 3.4d.1 `setup-*` cache recipes (PAR-096)

`setup-node` / `setup-go` / `setup-python` are **remote actions**: when the instance policy admits
them (§3.4e), the server rewrites the step around this built-in cache pair — the cache restores
before the setup action and the deferred save runs at the end of a green job, with the ecosystem's
workspace-relative `env:` applied to the setup step. When the policy refuses the action, the refusal
quotes the exact replacement below rather than leaving an author to guess. Either way the **cache
half is built in**:

| Action | Replacement (a `run:`/`env:` step plus `actions/cache`) |
|---|---|
| `actions/setup-node` (`cache: npm`) | `env: npm_config_cache: .npm`; cache `path: .npm`, `key: npm-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}`, `restore-keys: npm-${{ runner.os }}-${{ github.ref_name }}-`. For yarn/pnpm point `YARN_CACHE_FOLDER`/`PNPM_STORE_DIR` at a workspace directory and cache that |
| `actions/setup-go` (`cache: true`) | `env: GOCACHE: .cache/go-build` and `GOMODCACHE: .cache/gomod`; cache both paths, `key: go-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}` |
| `actions/setup-python` (`cache: pip`) | `env: PIP_CACHE_DIR: .cache/pip`; cache `path: .cache/pip`, `key: pip-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}` |

`setup-java`, `setup-dotnet` and `setup-ruby` are refused by name with "this setup action's cache
layout is not one of the mapped ecosystems (npm/pip/go)". When the action itself resolves (§3.4e),
the server rewrites the step around the same `actions/cache` pair: the cache restores before the
setup action and the built-in action's deferred save runs at the end of a green job, with the
ecosystem's workspace-relative `env:` applied to the setup step. A `cache:` value this table does not
map, or a setup action that cannot resolve, is a named refusal quoting the recipe — never a silent
no-cache and never a skip.

### 3.4e Remote actions (PAR-070/071/072/074)

`uses: owner/repo[/path]@ref` (and the full-URL spelling `https://<configured-source>/owner/repo[/path]@ref`)
resolves a marketplace-style action. Nothing is fetched by the runner: the server resolves, pins and
embeds it at run creation, and the runner executes exactly those bytes.

**Where it resolves from.** By default **this instance only** — `owner/repo` is a repository on the
same spindle, read at the resolved commit with the same read-only git path as a workflow file. An
operator may configure ONE external source (`threads.actions.source`, a Forgejo/Gitea-compatible
base URL); when set, bare references resolve there and a full-URL reference must name that host. A
full URL with no configured source, or one naming another host, is a named refusal.

**Policy (default-deny, D-5).**

```toml
[threads.actions]
mode = "allowlist"          # deny | allowlist | open
allow = ["acme/tools", "actions/*"]   # this instance's repos; "host/owner/repo" for a source host
require_sha = true          # a mutable ref outside `allow` must be a full 40-char commit SHA
source = ""                 # empty = this instance only; else one https:// source
max_bytes = "16MiB"         # per action, bounded; 0 selects the default
```

- `mode = "deny"` refuses every remote action (an air-gapped instance).
- `mode = "allowlist"` (the default) permits a tag/branch only for a repository matching `allow`
  (`owner/repo`, `owner/*`, `*`); everywhere else a **full commit SHA** is required. A mutable ref
  from a non-allowlisted owner is refused at run creation with a reason naming the reference, the
  allowlist key and both remedies (pin the SHA, or add the repository).
- `mode = "open"` accepts mutable refs everywhere; resolution is still pinned and recorded.
- `require_sha = false` is an explicit weakening: the allowlist becomes advisory.

The fetched metadata is parsed by `internal/actions`' own strict decoder (the same approved
`gopkg.in/yaml.v3` module, no new dependency) rather than `internal/threads/workflow`'s local-action
parser, because the remote shape is wider (`node20`/`node24`, a Dockerfile `image:`, nested remote
`uses:`) and both parsers must stay strict in the same way. A local action's metadata is still read
by the workflow parser exactly as before.

There is no implicit credential anywhere in this path. A remote action never receives the job token,
the OIDC request token, or the secret set: only its declared `with:` inputs and its own
`env:` block (and an explicitly passed `${{ secrets.X }}` in `env:` is the declared interface). A
`docker://`/Dockerfile action gets no token at all, exactly like the local rule.

**Resolve → pin → verify.** A reference is resolved to a full commit SHA, the action directory's
files are fetched under a bounded size and file count, and the content is hashed (sha256 over mode,
path and bytes). Both the commit and the digest are written on the run header
(`action_loads`) and stated in the job log. The instance remembers commit → digest: if a source ever
serves different bytes under the same commit, the run is refused ("changed content under the same
commit") instead of treating it as a cache hit. Symlinks, submodules, path escapes and over-limit
content are refused by name. Revocation is removing the allowlist entry (and, for a mirror, deleting
its cached bytes): the next run refuses the reference.

**What a run records.** `Run.ActionLoads` is a flat, stable list — `source` (`instance` or the source
host), `repository`, `path`, `ref`, `commit`, `digest`, `using`, and `policy`
(`sha-pinned`/`allowlisted`/`open`). It is written once at creation, deduplicated across jobs, and
read from the run API/page. The operator-facing surfaces are therefore: the config above (rendered
in `/admin/config`), the run header, and the job `reason` when a reference is refused.

**Execution forms.**

- `composite` expands in place, exactly like a local composite (§3.4a): inputs, `${{ inputs.* }}`,
  per-step env layering, nested remote `uses:` (depth-capped, cycle-refused), output threading and
  `${{ github.action_path }}` all work. The action's files are materialized into
  `/spindle/remote-actions/<digest>` **outside the workspace volume**, so a checked-out repository
  cannot pre-create or symlink the path.
- `node20` / `node24` runs the action's `main` with the **job image's** Node runtime, declared as a
  minimum: `node20` needs Node ≥ 20, `node24` needs Node ≥ 24. The runtime is probed once per job;
  a missing or too-old runtime is a named step failure ("needs a Node 20 runtime … the step is
  refused, never skipped"). `INPUT_*`, `GITHUB_ACTION_PATH`, `$GITHUB_OUTPUT`/`$GITHUB_STATE` are
  provided; `pre`/`post` hooks run in the documented order (`pre` before `main`, `post` at the end of
  the job in reverse registration order, after a failed main, gated by `pre-if`/`post-if`). State the
  pre/main phase saves reaches the post hook as `STATE_<name>` (§3.9a) — the `actions/cache` contract.
  A cancelled job skips post hooks with a stated log line (there is no reliable container work after
  cancellation).
- `docker` runs a registry image (`image: docker://…`, digest rules unchanged, §3.4a) or **builds** a
  Dockerfile (`image: Dockerfile` or a path inside the action) on the **runner host** from exactly
  the fetched action files, then runs the resulting local image. The job container still never
  receives a docker socket (D-8); the built image is tagged from the job name, used once and removed
  at cleanup. The action content digest is the pin (there is no registry reference to digest-pin).
  `runs.args` supplies the image CMD; for an inline `docker://` step, `with.args` /
  `with.entrypoint` override the image's command/entrypoint (they are not inputs).
- Nesting is capped at **10** levels across local and remote composites; a cycle names the chain.
  A remote action's `uses:` may be another remote action, `docker://`, or a built-in; its `./<path>`
  is the caller repository's path (there is no implicit checkout of the action's own repository).

**Not available:** an action `pre`/`post` on a composite (a JavaScript-action feature), and a remote
`uses:` inside a **local** composite's metadata (named parse refusal).

### 3.5 `env` layering and built-ins



Precedence, lowest first: runner built-ins → workflow `env` → job `env` → container `env` → step
`env`; secrets are added last, by name. Every step sees:

| Variable | Value |
|---|---|
| `CI` | `true` |
| `GITHUB_ACTIONS` | `true` |
| `GITHUB_REPOSITORY` | `owner/repo` |
| `GITHUB_SHA` | the run's commit |
| `GITHUB_REF` | the triggering ref: `refs/heads/<branch>` for a push, `refs/tags/<tag>` for a tag push (the run has no branch then), the base branch for a pull request. A PR's run header and `${{ github.ref }}` are `refs/pull/<n>/merge` (§6.2); the job's `GITHUB_REF` stays the base branch — its document identity — and is not the supersede key (§3.1a) |
| `GITHUB_REF_NAME` | the short name of that ref: the branch, the tag (`v1.2.3`), or the PR's base branch. A tag run never reads the empty branch — `${{ github.ref_name }}` is the tag too |
| `GITHUB_EVENT_NAME` | `push`, `pull_request`, `pull_request_target`, `repository_dispatch`, `workflow_run`, `create`, `delete`, `deployment`, `deployment_status`, `workflow_dispatch`, `schedule`, `release`, `issues`, `issue_comment` |
| `GITHUB_EVENT_PATH`, `FORGEJO_EVENT_PATH`, `SPINDLE_EVENT_PATH` | `/tmp/spindle-event.json`: the bounded event payload written before the first step |
| `RUNNER_OS`, `RUNNER_ARCH`, `RUNNER_NAME`, `RUNNER_TEMP`, `RUNNER_TOOL_CACHE`, `RUNNER_ENVIRONMENT`, `RUNNER_DEBUG`, `HOME` | `Linux`, the container arch, the registered runner name, writable `/tmp/spindle-temp`, `/tmp/spindle-tool-cache` and `/tmp/spindle-home` (the job's home, so `git config --global` works as a non-root user), `self-hosted`, `0` |
| `SPINDLE_RUNNER_*` | the same runner values in spindle spelling |
| `GITHUB_WORKFLOW`, `GITHUB_JOB`, `GITHUB_RUN_ID` | workflow name, job id, run id |
| `GITHUB_ACTOR` | the principal that triggered the run (absent when the run has none); the same value `${{ github.actor }}` reads |
| `GITHUB_WORKSPACE` | `/spindle/workspace` |
| `SPINDLE`, `SPINDLE_REPO`, `SPINDLE_RUN_ID`, `SPINDLE_JOB_ID`, `SPINDLE_CHECK` | `true`, `owner/repo`, run id, job id, check context |
| `SPINDLE_REF`, `SPINDLE_SHA`, `SPINDLE_EVENT`, `SPINDLE_WORKFLOW`, `SPINDLE_TRUSTED`, `SPINDLE_WORKSPACE`, `SPINDLE_ATTEMPT` | the same values in spindle spelling |
| `SPINDLE_SERVER` | the origin the runner was pointed at |
| `GITHUB_SERVER_URL`, `FORGEJO_SERVER_URL` | the canonical spindle origin (no trailing slash): `server.public_url` when set, else the origin the runner reached (a reverse proxy's `X-Forwarded-Proto` included). Absent only when neither is configured |
| `GITHUB_API_URL` | `<GITHUB_SERVER_URL>/api/v1`, the JSON API root a workflow curls (§5.3) |
| `MATRIX_<AXIS>` | the matrix cell's value, uppercased axis (`MATRIX_GO`) |
| `SPINDLE_JOB_TOKEN` | the job token (§5.3), present in **every** step |
| `GITHUB_TOKEN`, `FORGEJO_TOKEN` | the **same** job token under GitHub's and Forgejo's spellings (§5.3). An authored `env:` value for either name wins, so a workflow that sets `GITHUB_TOKEN` to its own PAT keeps it |
| `GITHUB_ENV`, `FORGEJO_ENV`, `GITHUB_PATH`, `FORGEJO_PATH` | this step's command files (§3.10): what a step appends there reaches **later** steps of the job |
| `SPINDLE_ID_TOKEN_REQUEST_URL`, `SPINDLE_ID_TOKEN_REQUEST_TOKEN` | the job OIDC exchange URL and request token (§5.5); `id-token: write` on a trusted run only |
| `ACTIONS_ID_TOKEN_REQUEST_URL`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN` | the same two values under GitHub's spelling (§5.5) |
| `SPINDLE_OUTPUT` | the per-step output file (§3.9): `/tmp/spindle-output-<n>`, writable, initially absent |
| `GITHUB_OUTPUT` | the same path under the GitHub-compatible spelling |
| each organization/repository/environment secret | by its own name, trusted runs only; environment > repository > organization (§5.1) |

One reference has a stronger delivery rule than the environment:

- `${{ secrets.<NAME> }}` as the **whole** value of a step `env:` entry is delivered **by name**:
  the container gets the variable, the value travels in the runner's environment and never in argv,
  `docker inspect` or `.git/config`. An unknown secret (a fork run, a typo) is an **empty**
  variable, as on GitHub. Embedded in a larger string (`https://user:${{ secrets.T }}@host`) it is
  substituted like on GitHub — the value is necessarily part of the value it is spliced into — and
  it is masked in the log (§3.6).

`${{ github.server_url }}` and `${{ github.api_url }}` resolve to the same values as the
environment variables of §3.5 (both the `${{ github.x }}` and `${{github.x}}` spellings), and the
rest of §3.6's table resolves in every substituted site.

### 3.6 Where `${{ … }}` substitution applies

| Context | `matrix.*` | `inputs.*` | `steps`/`needs` outputs |
|---|---|---|---|
| step `name`, `run`, `if`, `shell`, `working-directory` | yes | yes | yes |
| step `env` **values**, `with` values | yes | yes | yes |
| a job's `outputs` **values** | yes | yes | yes (`steps` of the same job, `needs` dependencies) |
| job `name` | yes | yes | no |
| `concurrency.group` (job / workflow) | yes (job only) | yes | no (computed before any step runs) |
| `runs-on` labels | yes | yes | no |
| job/step `timeout-minutes` | yes | yes | no |
| `run-name` | no | yes | no |

**Every site has a documented context availability table (PAR-040)**, and a context outside it is
a named parse error, never an empty value:

| Site | Available contexts |
|---|---|
| step field / step `if:` | `spindle`, `runner`, `vars`, `matrix`, `strategy`, `job`, `inputs`, `env`, `secrets`, `steps`, `needs` |
| job `if:` | `spindle`, `vars`, `matrix`, `inputs`, `env`, `secrets`, `needs` (no `runner`, no `steps`, no `strategy`, no `job`) |
| job `name` | `spindle`, `vars`, `matrix`, `strategy`, `inputs` |
| job `concurrency.group` | `spindle`, `matrix`, `strategy`, `inputs` |
| workflow `concurrency.group` | `spindle`, `inputs` |
| job / workflow `concurrency.cancel-in-progress` | `spindle`, `inputs` (job level may also read `matrix`/`strategy`) |
| job `continue-on-error` | `spindle`, `matrix`, `strategy`, `inputs` |
| job `outputs` | `spindle`, `runner`, `vars`, `matrix`, `strategy`, `inputs`, `env`, `secrets`, `steps`, `needs` |
| `runs-on` / `container.image` / `timeout-minutes` | `spindle`, `matrix`, `strategy`, `inputs` |
| `run-name` | `spindle`, `inputs` |
| `defaults.run.*` | none (literals) |
| reusable caller `with:` | `spindle`, `inputs` |
| `workflow_call` output value | `jobs.<id>.outputs.<name>` only |

**`job` and `strategy` (PAR-039).** Two per-cell contexts complete the expression vocabulary:

- `job.status` is the running job's status — `success`, `failure` or `cancelled` — at the moment the
  step that reads it starts. It follows exactly the rule `success()`/`failure()` follows: a step
  failure tolerated by `continue-on-error` leaves the job a **success** (the step's own `outcome` is
  still `failure`, §3.7), and a cancel reads `cancelled`. It exists only in a step — its fields and
  its `if:` — because only then is there a job status to read; a job `if:`, a job name or a `run-name`
  that names it is a parse error.
- `strategy.job-index` (zero-based) and `strategy.job-total` describe the matrix cell; `fail-fast`
  is the declared flag (default `true`); `max-parallel` is the declared bound or, when the matrix
  declares none, the number of cells (spindle admits every cell up to the 256 cap). A **plain job** —
  no `strategy.matrix` — has an empty strategy context, so those four fields read the empty string
  (never the literal `${{ … }}` text); a matrix cell's values are fixed at run creation and are the
  same in its name, its lock key, its `runs-on`, its timeouts and its steps. `strategy` is not
  readable in the job's own `if:` (that condition decides whether the cell is created at all), and
  no strategy field may appear in a workflow-level `concurrency.group` (it expands once per run,
  before any cell exists).

**`runs-on` and `container.image` are the two structural fields a matrix may vary**
(PAR-053/PAR-030): a label or the image may contain whole `${{ matrix.<axis> }}`,
`${{ inputs.<name> }}` or `${{ spindle.<field> }}` spans, expanded per cell before the job record
is written; an empty resolution fails run creation, and the image gate only ever sees the real
tag. `services.<id>.image`, `environment` (its `name` and `url`), `defaults.run.*`, a
workflow/job/container/service `env` value and the job container's `command`/`entrypoint`
(PAR-077 — they are applied argv, so a span there would become argument text) are read before a
matrix cell exists and are copied verbatim — only a **step** `env:` value is substituted. A
`${{ … }}` span there is a **parse error naming the field**, never a literal that queues a job
against no runner or reaches the process environment unexpanded. Those refusals are deliberate (a
service image and the environment name are storage keys; `defaults.run` is literal by D-17), so
nothing is accepted-but-inert.

Every "yes" cell also accepts `spindle.<field>` / `github.<field>` / `forgejo.<field>` /
`forge.<field>` — the aliases are value-identical and share one closed field set (D-1, PAR-004):
`event_name`, `sha`, `ref`, `ref_name`, `ref_type`, `repository`, `repository_owner`, `workflow`,
`workflow_ref`, `workflow_sha`, `job`, `run_id`, `run_attempt`, `actor`, `triggering_actor`,
`head_ref`/`base_ref` (a pull request's branch names; empty on every other event), the two server
addresses `server_url`/`api_url` of §3.5, and — inside a composite action's own steps only —
`action_path`/`action_ref` (§3.4a). `actor_id`, `repository_owner_id`, `run_number` and
`ref_protected` are in the closed vocabulary but have **no value** in a spindle run: reading one is
a named error naming the reason (spindle has no numeric account ids, no per-workflow run counter,
and does not project branch protection), never the empty string. The set also accepts
`spindle.event.inputs.<name>`,
`env.<NAME>`, `secrets.<name>`, `spindle.event.<field>` (the documented payload projection:
`action`, `ref`, `before`/`after`, `repository.*`, `sender.login`, `pull_request.*`, `release.*`,
`issue.*`, `comment.*`), `runner.<field>` (`os`, `arch`, `name`, `temp`, `tool_cache`,
`environment`, `debug`),
`vars.<name>` (variables: environment over repository over organization, values masked in the log),
`job.status` and `strategy.<field>` (`job-index`, `job-total`, `fail-fast`, `max-parallel`, above).
The vocabulary is closed and checked at parse time: an unknown context or field, an undeclared
matrix key or dispatch input, or a malformed `secrets.<name>` is that file's error naming the
supported set — never a literal that reaches a shell. `runner.*` is a runner-site context (a step,
or a job output evaluated after the steps); `job.*` is readable only in a step; `strategy.*` is
readable wherever a matrix cell exists but not in the job's own `if:` (which decides whether the
cell is created); and `vars.*` and `secrets.*` are readable in a job-level `if:` as well as a step
(PAR-058). A reference outside its site's set is a parse error saying so.

The runner resolves a site's references in **one pass**: a substituted value is data, never a
template, so a secret whose body happens to contain `${{ github.ref }}` stays exactly that text.
The rule for a value that is not there is one of two:

- **an absent supported value expands empty.** An unset `env.NAME`, a secret the run does not have
  (a fork run, a typo), `spindle.actor` on a run with no actor, `head_ref`/`base_ref` outside a pull
  request, and `server_url`/`api_url` when no canonical origin is configured all read `''` — never
  the literal `${{ … }}` text.
- **a field with no value in this platform is an ERROR.** `actor_id`, `repository_owner_id`,
  `run_number` and `ref_protected` are accepted by the parser but cannot be valued by a spindle run;
  reading one is a named error stating why, never an empty string that silently changes a condition.
- **an undeclared `steps`/`needs` output is an ERROR.** `${{ steps.<id>.outputs.<name> }}` naming a
  step that is not one of the job's steps, or an output that step never published, fails the step
  with an error naming the reference (§3.9); `${{ needs.<job>.outputs.<name> }}` does the same for
  a dependency the job does not declare, or one that reported nothing. A cross-boundary value must
  never cross as an empty string.

`env.<NAME>` reads the authored environment layer at the site — runner built-ins → workflow `env` →
job `env` → container `env` → step `env`, with the step layer winning (`env: FOO: ${{ env.BAR }}`
inside a step reads the layer **below** that step, so it never reads a sibling or itself). Values a
step wrote to `$GITHUB_ENV` are runtime state for later steps, not part of the `env` context.

`secrets.<name>` is case-insensitive and includes the reserved token aliases
(`${{ secrets.GITHUB_TOKEN }}` / `${{ secrets.FORGEJO_TOKEN }}` are the job token, §5.3). A
**whole-value** step `env:` entry is still delivered by name (the value never reaches argv, §3.5);
an embedded reference is substituted and the value joins the log mask, so it prints as `***`.

The server-side expansions (job-level `if:`, job `name`, `concurrency.group`) read the same
`github.*` fields, `actor` included. A **job** `if:` may read `secrets.*` and `vars.*` (PAR-058):
a trusted run resolves the decrypted values, an untrusted run reads an empty secrets map (the
condition is false, not an error), and no value is written to the reason or the log. It does not
resolve the two addresses; a **step** `if:` reads `github.actor`, `github.server_url`/`api_url` and
`secrets.*` (§3.7).

A concurrency `group` also reads the run's ref contexts: `github.ref`, `github.ref_name`,
`github.repository`, `github.workflow`, `github.event_name`, `github.sha`, `github.run_id`. That is
the idiom `group: ci-${{ github.ref }}`, which gives each branch its own lock — without it, with
`cancel-in-progress: true`, a push on any branch would cancel `main`'s run. `env.*`, `steps.*`,
`needs.*`, `secrets.*`, `spindle.job`, `spindle.event.inputs.*` and operators are **not** supported
in a group (write `inputs.<name>` instead; `SPINDLE_*` are environment variables, and the canonical
`spindle.*` context — D-1, with `github.*`/`forgejo.*`/`forge.*` kept as value-identical aliases —
works in a group, so `group: ci-${{ spindle.ref_name }}` and `group: ci-${{ github.ref_name }}` are
the same lock).

A group is a lock, so its expansion **fails closed** — it never falls back to literal text:

- a reference that cannot exist at all (an unknown context, `github.<field>`, a matrix axis the job
  does not declare, a `workflow_dispatch` input the workflow does not declare, an operator, an
  unterminated `${{`) is a **parse error for that file**, naming the key and the supported set; the
  file is reported in the workflow list and runs nothing, like any other unsupported key;
- a supported reference with no value for **this** run (e.g. `${{ inputs.mode }}` on a `push`, when
  `mode` is declared without a default) fails run creation (`422`, the key named) rather than
  keying the group on an empty segment. A declared input's `default` does apply outside a dispatch.

A **job `name`** is filled with `matrix.*`, `inputs.*` and the same `github.*` fields. Its
references are validated at parse time like any other site; a validated reference whose value is
absent for this run stays visible in the rendered name (display-only, so it never fails an
otherwise runnable job). The matrix suffix follows GitHub's rule: a name that names a matrix axis
already distinguishes its cells, so `name: cell-${{ matrix.v }}` renders `cell-a` (and the checks
context is `threads/<workflow>/cell-a`), while `name: Build` renders `Build (a)`.

An unknown axis or input name in a step field is a **parse error for the file**, not a literal that
leaves the job to fail mid-run. A `${{ … }}` span in a step field may be one reference **or a whole
expression** (PAR-038): `env: {TARGET: "${{ matrix.env == 'prod' && 'p' || 'm' }}"}` renders `p` or
`m`, and `${{ github['sha'] }}` is the indexed spelling.

### 3.7 The expression subset (`if:`)

Supported in a **step** `if:`, in a substituted `${{ … }}` field, and (with the narrower context
set above) in a job `if:`:

```
success()  failure()  always()  cancelled()   (canceled() is accepted too)
startsWith(text, prefix)  endsWith(text, suffix)  contains(text, search)
format('{0}-{1}', a, b)  join(value, separator)  hashFiles(pattern, …)
toJSON(value)  fromJSON(string)   case(...) is NOT a function in this language
spindle.<field>  github.<field>  forgejo.<field>  forge.<field>
spindle.event.inputs.<name>  spindle.event.<field>
matrix.<key>  inputs.<name>  env.<NAME>  secrets.<NAME>  vars.<NAME>
job.status  strategy.job-index  strategy.job-total  strategy.fail-fast
strategy.max-parallel
spindle.action_path  spindle.action_ref   (a composite action's own steps only, §3.4a)
steps.<id>.outcome  steps.<id>.conclusion  steps.<id>.outputs.<name>
needs.<job>.result  needs.<job>.outputs.<name>
!  ==  !=  <  <=  >  >=  &&  ||  (  )
'literal'  "literal"  true  false  1  1.5
github['sha']   steps['build'].outputs['name']
needs.*.result  needs.*.outputs.<name>
steps.*.outcome  steps.*.conclusion  steps.*.outputs.<name>
```

The `spindle`/`github`/`forgejo`/`forge` fields are one closed set, identical for every spelling:
`event_name`, `sha`, `ref`, `ref_name`, `ref_type`, `repository`, `repository_owner`, `workflow`,
`workflow_ref`, `workflow_sha`, `job`, `run_id`, `run_attempt`, `actor`, `triggering_actor`,
`head_ref`, `base_ref`, `server_url`, `api_url`, `action_path`/`action_ref` (composite steps only).
`actor_id`, `repository_owner_id`, `run_number` and `ref_protected` are accepted by the parser but
have no value in a spindle run, so reading one is a **named evaluation error** naming the reason —
never the empty string. `format` uses
`{N}` argument indexes and `{{`/`}}` for literal braces. `secrets.<name>` reads the run's secret
map (empty on an untrusted run, never the literal text).

`toJSON(value)` renders compact JSON with `json.Marshal` semantics (no HTML escaping inside
strings) and object keys in sorted order, so one value always renders the same bytes. A string is
quoted, a number/boolean/null is itself, a filtered list is an array, and a **bare context**
(`github`, `forgejo`/`forge`, `runner`, `vars`, `matrix`, `inputs`, `env`, `secrets`, `job`,
`strategy`, `steps`, `needs`) is its object — the dotted `github` keys nest (`event.inputs.mode`
sits at `event.inputs.mode`), and a substitution whose whole span is a context renders the same
JSON. `fromJSON(string)` parses one JSON document into the expression model (object, array, string,
number, boolean, null); invalid JSON is a **named error**, never an empty value. A parsed object or
array is readable with the index syntax — `fromJSON(steps.x.outputs.payload).key == 'v'`,
`fromJSON('[1,2]')[0]` — and `contains()`/`join()` accept a parsed array. `case(...)` is not a
function in this language.

**Precedence**, tightest first: `!`, comparisons, `&&`, `||`; parentheses group. Comparisons are
non-associative.

- `==`/`!=` compare text case-insensitively (`'TRUE' == true`, `1 == '1'`); `<`/`<=`/`>`/`>=`
  compare numerically when both sides are numbers (`'2' < '10'`) and lexicographically otherwise
  (`'a' < 'b'`);
- `&&`/`||` are **value-returning** (PAR-038): `a && b` is `a` when `a` is falsy and `b`
  otherwise; `a || b` is `a` when `a` is truthy and `b` otherwise. `!` and comparisons return a
  boolean;
- an empty `if` means `success()`, so a plain step list runs in order;
- `success()`/`failure()` describe the **job** status: a step failure tolerated by
  `continue-on-error` does not flip them, and `job.status` (§3.6) reads the same fact;
- `steps.<id>.outcome` is the step's raw result (`success`, `failure`, `cancelled`, `skipped`,
  `timed_out`) and `steps.<id>.conclusion` is that result after the step's `continue-on-error` (a
  tolerated failure's conclusion is `success`). A step that did not run is recorded too. Step ids
  are case-insensitive;
- the documented filters produce a list in name order: `contains(list, value)` is a membership
  test and `join(list, sep)` renders it. Any other `*` position is a named error;
- `hashFiles(...)` walks the job workspace **inside the job container**: workspace-relative globs
  (`*`/`?` stay in one segment, `**` crosses, `**/` may match no directory, a leading `!`
  excludes), regular files only, deterministic ordering, cached for the current step and
  invalidated after a completed step. **No match is the empty string**; over **1000 files** or
  **100 MiB** is a named error. It is a named parse error in a job-level `if:` (there is no
  workspace before a runner is chosen);
- anything else — `case(...)` (not a function in this language), an unknown context or `github`
  field, an undeclared matrix key/input, a wrong argument count — is a **parse error for the file**
  (the same expression also fails the step or job at evaluation time if a stored workflow reaches
  it), never a silent skip;
- `steps.<id>.outputs.<name>` reads the output a step published through its output file (§3.9). A
  step that ran but did not publish that name reads empty, so `if: steps.x.outputs.y == ''` works;
  an `<id>` that is not a step of the job is an evaluation **error** naming it (a typo must never
  read as "not set").

A **job** `if:` is evaluated by the server, before the job is claimable, with the same subset plus
the job contexts: `needs.<job>.result` (the dependency's outcome, folded over a matrix job's cells),
`needs.<job>.outputs.<name>` (the dependency's reported outputs, §3.9), `github.*`, `matrix.*`,
`inputs.*`, `env.*`, `vars.*`, `secrets.*`, and `success()`/`failure()`/`cancelled()` folded from
the dependencies. A job with no `needs:` is decided when the run is created (and again on approval
or re-run), so a false condition is `skipped` immediately and never reaches a runner; a dependent
job is decided once every dependency is terminal. A false condition is `skipped` with the condition
in the job's `reason`; an expression outside this subset **fails the job** with the error, never a
silent skip. A job-level `if:` cannot read `steps.*` or `runner.*` (neither exists at the server's
decision point — the parse error names it); it **can** read `secrets.*` and `vars.*` (PAR-058):
a trusted run sees the decrypted values, an untrusted run sees an empty secrets map (the condition
is false, never an error), and nothing is written to the reason or the log. A true condition lets
the job run even after a failed dependency (`if: always()`,
`if: needs.build.result == 'failure'`); with no `if:`, every dependency must have succeeded
(`if: false` is the simplest way to disable a job without deleting it).

```yaml
name: Lint
on: [push]
jobs:
  lint:
    runs-on: linux
    if: github.ref == 'refs/heads/main'   # skipped on a feature branch
    steps:
      - id: vet
        run: go vet ./...
      - name: report a vet failure
        if: steps.vet.outcome == 'failure'
        run: echo "vet failed; see the step above"
  docs:
    runs-on: linux
    continue-on-error: true               # this job's failure does not fail the run
    steps:
      - run: make docs
```

### 3.8 A dispatch + schedule file

```yaml
name: Nightly
on:
  workflow_dispatch:
    inputs:
      environment:
        description: target environment
        type: choice
        options: [staging, production]
        required: true
        default: staging
      dry_run:
        description: skip the deploy
        type: boolean
        default: false
  schedule:
    - cron: "0 3 * * *"
permissions:
  contents: read
jobs:
  deploy:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: deploy
        if: inputs.dry_run != 'true'
        env:
          TARGET: ${{ inputs.environment }}
        run: |
          echo "deploying to $TARGET"
          # DEPLOY_TOKEN, if set in Settings → Secrets, is present in this step
          if [ -n "$DEPLOY_TOKEN" ]; then echo "token present"; fi
```

Scheduled runs pass no inputs, so `${{ inputs.environment }}` expands **empty** on the 03:00 tick
(an absent supported value is empty, §3.6) — a scheduled workflow should not depend on a dispatch
input, or should give it a `default` and read it from `env` only on dispatch. (The tick itself sees
`GITHUB_EVENT_NAME=schedule`.)

### 3.9 Step and job outputs

Outputs let one step hand a value to a later step, and one job hand a value to a job that `needs:`
it. spindle uses the GitHub file mechanism, so a script written for GitHub works unchanged.

**The step output file.** Every step gets `SPINDLE_OUTPUT` (and its alias `GITHUB_OUTPUT`) pointing
at a file in the job container: `/tmp/spindle-output-<step number>` (1-based). The file does not
exist until the step creates it — appending is the usual form:

```text
      - id: build
        run: |
          echo "digest=$(sha256sum dist/app.tar.gz | cut -d' ' -f1)" >> "$SPINDLE_OUTPUT"
          printf 'notes<<EOF\nbuilt by %s\nEOF\n' "$GITHUB_SHA" >> "$SPINDLE_OUTPUT"
```

The file lives in the job container (`/tmp`), not the workspace volume, so a `git clean` in the
checkout cannot erase another step's file and nothing shows up as an untracked path. The runner
reads it with `head -c` at step end, so the job image must provide `head` (busybox and any GNU
coreutils image do).

The format is GitHub's, exactly:

| Line | Meaning |
|---|---|
| `name=value` | one output; `value` is everything after the first `=` (it may contain `=`, and may be empty) |
| `name<<DELIMITER` | a heredoc: every following line up to a line that is exactly `DELIMITER` is the value, joined with `\n` |

A blank line outside a heredoc is ignored, a trailing CR is stripped (CRLF files work), and a
repeated name keeps the **last** value. Anything else — a line that is neither form, an invalid
name, an unterminated heredoc, a heredoc with an empty delimiter, or a delimiter longer than 128
bytes — is a **named step failure** (`step output file /tmp/spindle-output-1: line 3: …`), never a
silently dropped output. The delimiter is matched exactly on a line of its own, so a value that
contains the delimiter line ends the heredoc there: use a short random value (`EOF_$RANDOM`) rather
than a word your output may print. Names use the step-id charset (letters, digits, `_`, `-`; at most
128 bytes).

Only a step with an `id:` is addressable: the runner reads its file at step end and publishes the
values as `steps.<id>.outputs.<name>`. An anonymous step's file is never read (GitHub's rule).

**Reading a step output.** A later step substitutes a value anywhere `${{ … }}` is substituted for
matrix/inputs (§3.6) — `name`, `run`, `if`, `shell`, `working-directory`, `env` values and `with`
values:

```text
      - id: build
        run: make dist
      - name: publish
        if: steps.build.outputs.digest != ''
        env:
          DIGEST: ${{ steps.build.outputs.digest }}
        run: echo "publishing $DIGEST"
```

Substitution is **fail-closed**. A reference to a step id the job does not declare, or to an output
that step did not publish (because it failed, was skipped, or simply never wrote the name), fails
the step with a message naming the reference — it is never replaced by an empty string. (Inside a
native `if:` expression, §3.7, a known step's unpublished output reads empty, so `== ''` checks
work; an unknown id is still an error.) In practice: a step after a failure does not run (unless it
opts into `continue-on-error`), so the fail-closed error means "you read an output that does not
exist", which is almost always a typo or a step that was skipped.

In a step `if:` the native `steps.<id>.outputs.<name>` form is the idiom (§3.7). `needs.<job>.outputs`
is a job-level context: inside a step, a `${{ needs.… }}` span is substituted, but — like
`${{ matrix.… }}`/`${{ inputs.… }}` — the span must be a bare reference, so
`if: ${{ needs.build.outputs.flag }}` works for a boolean output while a comparison belongs in the
job-level `if:` (§3.7). The native `needs.<job>.outputs` form inside a step's expression is an error,
not an empty value.

**Job outputs.** A job declares what it exports, as templates over its own step outputs:

```yaml
jobs:
  build:
    runs-on: linux
    outputs:
      digest: ${{ steps.build.outputs.digest }}
    steps:
      - id: build
        run: echo "digest=abc123" >> "$SPINDLE_OUTPUT"
  publish:
    runs-on: linux
    needs: [build]
    if: needs.build.outputs.digest != ''
    steps:
      - run: echo "publishing ${{ needs.build.outputs.digest }}"
```

Rules:

- only a **successful** job reports outputs; a failed, cancelled or skipped job exposes none (a
  dependent's `needs.<job>.outputs.<name>` then reads empty). A job whose template names an output
  that was never published fails with a named error rather than exporting an empty value;
- a dependent job's step fields read `needs.<job>.outputs.<name>` (same substitution sites as step
  outputs), and its job-level `if:` reads it directly (§3.7);
- job outputs are computed by the runner at job end and stored on the job record by the server, so
  `GET …/api/threads/{run}` shows them (`outputs` on each job; the run page does not render them —
  they are data for the next job, not a report);
- a **matrix job's** outputs are folded for its dependents, just as its `result` is: `needs:<key>`
  means every cell. `needs.<job>.result` is the worst cell's state; `needs.<job>.outputs.<name>` is
  the **union** of the cells' outputs, with the **last cell in job-id order** winning a name
  collision. That is deterministic, unlike GitHub's run-order rule, and only a wholly successful
  dependency exposes anything;
- a re-run clears the previous attempt's outputs.

**Caps** (fixed; a refusal is a named step failure, and the server validates the reported map
again before storing it):

| Limit | Value |
|---|---|
| outputs per step | 50 |
| outputs per job (distinct names) | 100 |
| one value | 64 KiB |
| total name+value bytes per job | 1 MiB |

**Secrets: outputs are data, not masked.** Masking rewrites LOG LINES (§5.1). The value a step
writes into `$SPINDLE_OUTPUT` is not scanned or rewritten when it is collected, stored on the job
record, or substituted into a dependent step — a job that writes a secret into an output exposes
that secret to every job that `needs:` it (and to every repository reader of the run view). The log
line that echoes the value is still masked (the runner and the server both mask frames), so an
echoed output does not print the secret — but anything the dependent step does with the value
(a network call, a file, an `echo` through `base64`) is the workflow's responsibility. GitHub's own
caveat is the same: never put secrets in outputs; pass them as secrets to the dependent job instead.

### 3.9a Step summaries and workflow commands

**`$GITHUB_STEP_SUMMARY` (and `$SPINDLE_STEP_SUMMARY`).** Every repository-code step gets a file it
may append markdown to: `/tmp/spindle-summary-<step number>`. The runner reads it at step end — on
success **and** failure — and joins the pieces into the job's summary, which the run page renders
inside a collapsed *step summary* panel using the server's sanitizing markdown renderer (raw HTML
goes through the same allowlist as a README; a ```mermaid fenced block renders as a code block, not
as a diagram script).

```yaml-fragment
      - name: report
        run: |
          {
            echo "## Unit tests"
            echo ""
            echo "| package | result |"
            echo "|---|---|"
            echo "| store | pass |"
          } >> "$GITHUB_STEP_SUMMARY"
```

Limits: **1 MiB per step**, at most **20 step summaries per job**, and **4 MiB** total for the
joined summary. An oversized or unreadable summary fails a step that otherwise succeeded; on a step
that already failed it is only logged (the step's own failure is the useful message). A
`docker://` action does not receive the variable: its image is third-party code and the summary is
rendered on the job page.

**`$GITHUB_STATE`.** A JavaScript action's pre→post hand-off. The per-action
file is `/spindle/state/<n>` (outside the workspace volume, so a checked-out repository cannot
pre-create or redirect it) and the `pre`, `main` and `post` phases of ONE invocation share it: append
`NAME=VALUE` lines, one per line, exactly like `$GITHUB_ENV`.

```js
// dist/pre.js of an action that caches a key
const fs = require('fs');
fs.appendFileSync(process.env.GITHUB_STATE, `cache-primary-key=${key}\n`);
```

The runner reads the file at the end of each phase (success or failure) and gives the **post hook**
each saved value as `STATE_<name>` — the documented contract, and the one `actions/cache` and
`setup-node` use (`process.env["STATE_cache-primary-key"]`). Names may contain letters, digits, `.`,
`_` and `-` (the hyphen in a real cache key is deliberate); `=` and whitespace are refused by name.

- `::save-state name=NAME::VALUE` is the deprecated spelling of a file write, not a refusal: it feeds
  the same map, and the **file wins** a collision in one phase (the rule `$GITHUB_OUTPUT` uses against
  `::set-output::`).
- Caps: **1 MiB** of state per action invocation, **128** names, 64 KiB per value. An oversized or
  malformed file fails a phase that otherwise succeeded; on a phase that already failed it is logged.
- A state value can be a token, so it is delivered **by name**: the post hook's `docker exec` carries
  `--env STATE_<name>` and the value rides the docker client's environment. It never enters argv, and
  the `::save-state::` confirmation names the key only (`saved state "…" for the post hook`).
- `::save-state::` in a step that is not a JavaScript action is answered with a named line: there is
  no post hook to receive it.
- `docker://` actions and a **composite** action have no pre/post hooks in spindle, so neither gets a
  state file.

**Workflow commands.** A step's output lines of the form `::name key=value::message` are consumed by
the runner, GitHub-style:

| Command | Effect |
|---|---|
| `::error::`, `::warning::`, `::notice::` | an annotation (`file`, `line`, `endLine`, `col`, `endColumn`, `title` properties) stored on the job, listed on the run page, and counted in the commit's check description. The step is **not** failed by an error annotation — it is a message, exactly as on GitHub |
| `::group::` / `::endgroup::` | a log group marker; an unclosed group is closed at step end |
| `::add-mask::VALUE` | adds VALUE to this job's log mask, permanently: there is no unmask command, and nothing removes an entry. Values under 4 bytes are recorded but not applied (masking one character would redact the whole log). At most 128 masks per job |
| `::stop-commands::TOKEN` / `::TOKEN::` | stops/starts command parsing (used around untrusted output); while stopped, every line is plain text |
| `::set-output name=NAME::VALUE` | the deprecated alias for a step output; it joins `$GITHUB_OUTPUT`'s namespace, with the file winning a collision. A step without an `id:` discards it (with a warning) |
| `::debug::MESSAGE` | one debug line in the log |
| `::save-state name=NAME::VALUE` | the deprecated spelling of a `$GITHUB_STATE` write: it feeds the same map and the post hook receives `STATE_NAME`; in a non-action step it is a named line, not a silent drop |
| anything else (`::set-env::`, `::add-path::`, an unknown name) | a **visible warning** naming it; the original line stays in the log. Nothing is accepted-and-inert |

Escaping is GitHub's (`%25`→`%`, `%0A`/`%0D`→newline/CR in the message; properties additionally
`%3A`→`:` and `%2C`→`,`). The parser is the untrusted-input boundary: one line is buffered to at
most 64 KiB (a longer line is plain text and is never parsed mid-line), messages/titles/paths are
length-capped, at most 10 annotations per step and 50 per job are kept, and every payload is masked
before it is stored or printed. An annotation is data — the page renders its message escaped.

### 3.10 Command files: `$GITHUB_ENV` and `$GITHUB_PATH`

A step can extend the job's environment for the steps **after** it. Every repository-code step
(run steps, the built-in checkout, and composite-action inner steps) gets four variables pointing at
two files in the job container:

| Variable | File | What to write |
|---|---|---|
| `GITHUB_ENV`, `FORGEJO_ENV` | `/tmp/spindle-command-<n>-env` | one `NAME=VALUE` per line |
| `GITHUB_PATH`, `FORGEJO_PATH` | `/tmp/spindle-command-<n>-path` | one directory per line |

```yaml
name: env files
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - name: derive a tag
        run: |
          echo "TAG=v$(cat VERSION)" >> "$GITHUB_ENV"
          echo "$PWD/.tools/bin" >> "$GITHUB_PATH"
      - name: use it
        run: |
          echo "tag is $TAG"     # set by the previous step, not this one
          tool --version         # found via the prepended PATH
```

Rules, all of them enforced by the runner:

- **Later steps only.** A step's own writes are collected when it finishes, so the writing step does
  not see its own variable (`$TAG` above is empty in step 1) and every later step of the same job
  does. A step that writes and then fails still changes the environment for its successors.
- **No heredoc.** `$GITHUB_ENV` takes one `NAME=VALUE` per line (the value may contain `=`); the
  heredoc form belongs to `$GITHUB_OUTPUT` (§3.9) and is a named failure here. Blank lines are
  ignored, a trailing CR is stripped (CRLF files work), and a repeated name keeps the last value.
  Values are never evaluated — no `$VAR`, no globs.
- **PATH is appended to, never replaced.** Each directory written to `$GITHUB_PATH` is *prepended*
  to the next step's `PATH`, in write order, ahead of the image's own `PATH`. `PATH` itself cannot
  be set through `$GITHUB_ENV`.
- **Protected names are refused by name.** The runner owns `GITHUB_*`, `SPINDLE_*`, `FORGEJO_*`,
  `ACTIONS_*` and `RUNNER_*`, and refuses `CI`, `PATH`, and the process-startup hooks
  `LD_PRELOAD`, `LD_AUDIT`, `BASH_ENV`, `ENV`, `NODE_OPTIONS`, `IFS`, `PS4` and `SHELLOPTS`. A
  refusal is a named step failure that says which variable and why, never a silent drop.
- **The step's own `env:` wins.** A later step that declares the same name in its `env:` block keeps
  its value; otherwise the file's value overrides the workflow/job/container `env` layer.
- **Caps** (a named step failure): 100 added variables per job, 8 KiB per value, 64 KiB of added
  environment per job, 64 PATH entries of up to 4 KiB each.
- **No runner state, no concurrency leak.** The values ride the *next* step's `docker exec` (as
  `--env` elements) and a shell wrapper for `PATH`; the runner process environment is never
  mutated, and two concurrent jobs on one runner cannot see each other's files.
- **`docker://` actions receive none of it.** The image is third-party code and gets no job
  environment layer (the same rule as secrets); add the directory in a run step or bake it into the
  image.
- The job image must provide `head` and `sh` (busybox and any GNU coreutils image do): the runner
  reads the files with `head -c` at step end, and prepends `PATH` through `sh -c`.
- Nothing is masked when it is *collected*: a secret written into `$GITHUB_ENV` is delivered to
  later steps like any other variable. Its echoed log lines are masked (§5.1); never write a secret
  to these files to move it between jobs.

## 4. Containers, services and checkout

### 4.1 One container per job

Every job gets exactly one container, created before the first step and removed after the last —
on success, failure, setup error, cancel, timeout and runner panic alike. Steps are `docker exec`
invocations inside it, so the workspace, the installed toolchain and the network namespace persist
across steps.

**Who owns the container's process.** With neither `container.entrypoint` nor `container.command`,
the runner owns it: the image `ENTRYPOINT`/`CMD` is overridden with `--entrypoint /bin/sh` plus a
keep-alive command. When the workflow names either key, docker's own semantics apply and the workflow
owns the lifetime:

```yaml-fragment
    container:
      image: alpine:3.20
      entrypoint: /bin/sh
      command: -c "sleep infinity"
```

- `entrypoint` + `command`: `--entrypoint <first word>` with the remaining entrypoint words and the
  command as its arguments.
- `command` alone: the image's own `ENTRYPOINT` runs with the command as its arguments (docker's
  `CMD` override).
- Both forms accept GitHub's scalar or list spelling. A scalar is one command line and is split into
  argv words with quote awareness (`-c "sleep infinity"` is two words); a list element is one
  argument even when it contains a space.

The container must still be running when the first step starts, because steps are `docker exec`
calls. The runner checks it right after `docker start` and fails the job with a named reason when the
entrypoint/command exited (`the job container exited (code 0) before the first step:
container.entrypoint/command must keep a process running …`), instead of leaving the author with a
later "container is not running".

**`container.ports`** applies as an **ephemeral loopback publish** — `--publish 127.0.0.1::<port>`
for each entry — so the port is reachable from the job container's own loopback and from a service
on the job network, and two concurrent jobs never collide on a host port. A host-side port
(`8080:80`) is refused by name: a fixed host port would collide and open a port on the runner host.
The mapping docker actually made is stated once at job start (`spindle: job container port
8080/tcp -> 127.0.0.1:49153`).

The workspace is a per-job named volume mounted at `/spindle/workspace`. The runner mounts nothing
else; a workflow that wants a host path must say so with `container.volumes` (`- /host/path:/in/job`),
which is a bind mount running under the daemon's user — and only when the runner operator
allow-lists the source (`--container-valid-volume`, §8.7).

**Registry credentials are the runner's, not the workflow's.** `container.credentials` is refused by
name (D-23): a workflow-supplied password would travel in the job payload (the bucket) and in docker
create's environment. A private image pulls because the runner operator configured
`--registry-auth-file` (§8.6).

### 4.1a Shells

A step's `shell:` resolves at run time inside the container:

| Spelling | argv |
|---|---|
| absent (the default), `bash`, `bash {0}` | `bash --noprofile --norc -eo pipefail -c <script>` (D-24: GitHub/Forgejo's Linux default); if the image has no bash, the step runs `sh -e` and one log line says so |
| `sh`, `sh {0}` | `sh -e -c <script>` |
| `python` / `python3` | `python -c` / `python3 -c` (the image must provide the interpreter) |
| `pwsh` / `powershell` | `pwsh -command <script>` |
| `cmd` | `cmd /D /E:ON /V:OFF /S /C <script>` |
| anything else with exactly one `{0}` | the template split into argv words (no shell expansion), with `{0}` replaced by a script file written into the container (`/tmp/spindle-step-<n>`), e.g. `dash -e {0}` |

A named shell or template the runner cannot honour, and a custom template without exactly one
`{0}`, is a **named step failure** — never a guess and never a command appended to argv.

#### Docker in a job (PAR-081, D-8)

A job container has **no docker daemon and no socket by default**, and that is the documented parity
path, not a gap: `docker build`/`docker push` work only when the *operator* grants the runner a
daemon endpoint (§8.7). The supported way to build an image in a job is a **daemonless builder that
the job image provides** — kaniko, or buildah with `--storage-driver=vfs --isolation=chroot`:

```yaml-fragment
    container:
      image: gcr.io/kaniko-project/executor:debug
    steps:
      - uses: actions/checkout
      - name: build
        run: /kaniko/executor --context=dir://$GITHUB_WORKSPACE --dockerfile=$GITHUB_WORKSPACE/Dockerfile --destination=registry.example.com/acme/app:$GITHUB_SHA
```

Kaniko needs no privileges and no socket; it reads the Dockerfile and writes the image to a registry
(or `--no-push --tar-path=` for a local tar). Buildah additionally needs user namespaces or the
kernel/daemon configuration to allow them — test it on the runner host before moving a build there.
A step whose script invokes `docker` in a job without an endpoint fails with the remedy in its error
(exit 127 plus the daemonless line), and a workflow that asks for the socket itself — a
`volumes: ["/var/run/docker.sock:…"]` — is refused by the container policy. The operator grant is
`--docker-host` (§8.1): the job container alone receives the endpoint (a unix socket is bind-mounted
at the same path, a `tcp://` endpoint becomes `DOCKER_HOST`); services and `docker://` action images
never inherit it.

### 4.2 Services

Each `services:` entry becomes one sidecar container on the job's own network, with the entry's key
as its network alias — the job reaches it at `postgres:5432` (the *container* port, never the host
port). `env`, `volumes`, `options`, `command` and `entrypoint` are applied.

Readiness before the first step: the runner polls `docker inspect` (an image `HEALTHCHECK` is
honoured) and, for each declared port, TCP-dials it through an **ephemeral loopback** host port that
concurrent jobs cannot collide on. `unhealthy`, `exited` or `dead` fails the job immediately; no
service becomes ready within 60 seconds, and the job fails naming the service and its state.

A workflow-declared health probe is the documented way to gate on a database accepting queries —
`options` may carry docker's own `--health-*` flags (they run **inside** the service container, so
they are in-container knobs, not host capabilities):

```yaml-fragment
    services:
      postgres:
        image: postgres:17
        env:
          POSTGRES_PASSWORD: ci
        ports: ["5432"]
        options: >-
          --health-cmd "pg_isready -U postgres"
          --health-interval 10s
          --health-timeout 5s
          --health-retries 5
```

The runner reads the resulting health status through the same readiness wait: a service that stays
`starting` for the whole window fails the job naming it, and `unhealthy` fails it as soon as docker
reports it. A malformed or hostile health flag (`--health-cmd -x`, a non-duration interval, zero
retries) is refused by the container-option policy before the container is created; `--no-healthcheck`
is not a workflow-controlled flag. Forgejo's `cmd:` spelling is accepted as the service's argv tail.

**Registry credentials are the runner's, never the workflow's (PAR-078).** `services.<id>.credentials`
(and the container equivalent) is refused: it would put a registry password into the job payload in
the object store and into docker argv, which is host-visible. A private image is reachable because
the runner's operator configured its pull identity — the daemon's own `docker login`, `--image-prefix`
(§8.6), or `--registry-auth-file` (§8.1): a 0600 file of `registry username password` lines that the
runner logs in with once per start, passing each password on stdin (`docker login --password-stdin`)
into a private temporary `DOCKER_CONFIG`. The value never appears in argv, the job payload or a log
frame (it joins the job's mask list), and the temporary config is removed when the runner stops.

### 4.3 The network

One network per job, torn down with it. The job container can reach the host through
`host.docker.internal` (`--add-host …:host-gateway` is set), which is how a runner on the same host
clones over a loopback origin. A bridge-only host that does not resolve that name is reached at its
docker0 gateway (usually `172.17.0.1`). Whatever address the checkout uses, it must resolve and be
reachable from **inside** this network — never from the host's or the runner's own namespace (§4.4).

### 4.4 Checkout

`uses: actions/checkout` is the only built-in action. It runs **inside** the job container — `git
init -q`, `git remote add origin <url>`, `git fetch -q [--depth=N] --no-tags origin <sha|ref>`,
`git checkout -q FETCH_HEAD` — using the job token through an in-container credential helper
(`$SPINDLE_JOB_TOKEN` is passed as an environment variable name, never in argv or `.git/config`).

The clone URL is `server.public_url` when set, otherwise the origin of the protocol request the
runner made (its host, with `X-Forwarded-Proto` honoured). **The configured public URL wins** because
it is the operator's declaration of an address job containers can reach: the request origin can be a
browser-only canonical host (`walgit.localhost:<port>`) that resolves on the host and never inside a
container. A loopback origin is rewritten to `host.docker.internal` because the container cannot
reach the host's `127.0.0.1`; any other name is used as-is — the runner never guesses an IP.

**The URL must be reachable from inside the job container.** That is the one requirement a first-time
containerised setup trips over: `walgit.localhost`, a host-only DNS name, or a proxy address that only
the host resolves produces `fatal: unable to access …` and, before this rule, nothing else. Now:

- the runner states the resolved base once at job start, in the job log:
  `checkout from <url>, reachable inside the job container as <host:port>` (plus the payload URL when
  the loopback rewrite changed it);
- a checkout that fails on a name or connection error adds a **named diagnostic** after git's output
  (which is kept verbatim as the evidence): the URL tried, this reachability requirement, and the fix;
- the fix is `server.public_url` (Settings → Server → Advanced → `public_url`) set to an address
  job containers can reach, and/or restarting the runner with `--server` pointing at one. For a
  host-socket daemon that is `http://host.docker.internal:<port>`; for a bridge-only host, its docker0
  gateway (usually `http://172.17.0.1:<port>`). Choose an address that is also valid for browsers
  when you can, since `public_url` is shared by links and OIDC redirects; otherwise leave it empty
  and let `--server` decide the fallback origin, or run the runner against a public hostname.

**Inputs.** Four `with:` keys are honoured; any other key is **refused by name** (the step fails
with the key, the supported set and, where one exists, what to do instead) — never silently ignored:

| Input | Default | Exact effect on the argv |
|---|---|---|
| `fetch-depth` | `1` | `--depth=<N>` on the fetch; `0` (or `"0"`, `"00"`) omits `--depth` for a full clone. A non-numeric or negative value is a named failure |
| `fetch-tags` | `false` | `true` adds `--tags`; the default adds `--no-tags` explicitly, so the fetch never depends on git's config |
| `ref` | the run commit | the fetch target: `<ref>` replaces `<sha>` (`refs/heads/x`, a tag, or a SHA). Whitespace, a leading `-` and refs over 256 bytes are refused; the checkout is still detached at `FETCH_HEAD` and the success line names both |
| `path` | the workspace root | `mkdir -p <workspace>/<path>` then `cd` there before `git init`. A relative path only: absolute, `~`, `..` and NUL are refused. `GITHUB_WORKSPACE` still names the workspace root, so later steps start there unless they set `working-directory` |

The full argv, in order, for a `fetch-tags: true`, `fetch-depth: 3`, `ref: main`, `path: src` step:

```sh
mkdir -p '/spindle/workspace/src'
cd '/spindle/workspace/src'
git init -q
git remote remove origin >/dev/null 2>&1 || true
git remote add origin '<url>'
git -c credential.helper='!f() { echo username=ci; echo password=$SPINDLE_JOB_TOKEN; }; f' \
    fetch -q --depth=3 --tags origin 'main'
git checkout -q FETCH_HEAD
```

Refused today, by name, with the alternative: `submodules` (run `git submodule update --init
--recursive` in a step), `repository` (a cross-repository checkout needs its own credential — use
plain `git fetch` with a secret), `token`/`ssh-key` (the checkout authenticates with the job token),
`persist-credentials` (nothing is ever written to `.git/config`), `clean` (every job starts on a
fresh volume) and `sparse-checkout` (run `git sparse-checkout` after the checkout).

The default is a shallow clone; `fetch-depth: 0` (or
`"0"`) turns it into a full clone:

```yaml
name: full checkout
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - run: git log --oneline -3
```

**Any job whose step runs `git push` must use `fetch-depth: 0`.** The default checkout is shallow,
and a shallow clone cannot push: its `git push` opens with the `shallow` protocol section, which
spindle's receive-pack does not advertise. The server refuses it with an ERR packet naming the cause
and this remedy, and logs the refused push with the repository and principal.

The checkout credential is the job token, so the checkout's authority is the job's authority: a
`contents: read` job's clone cannot push (the server refuses with
`the job token has no push permission`), and a `contents: write` job's clone can. There is no
separate checkout credential.

## 5. Secrets and permissions

### 5.1 Repository secrets

Managed in **Settings → Secrets** (admin only) or the API — write-only, never readable back:

| Method | Path | Body / answer |
|---|---|---|
| `GET` | `/{o}/{r}/api/threads/secrets` | `{secrets: [{name, created_at, updated_at}]}` — names and timestamps, never a value |
| `PUT` | `/{o}/{r}/api/threads/secrets/{name}` | `{"value": "…"}` → `204` |
| `DELETE` | `/{o}/{r}/api/threads/secrets/{name}` | `204` |

- names match `[A-Za-z_][A-Za-z0-9_]{0,63}`; values are 1 byte – 64 KiB;
- a secret is exposed to **trusted** runs only, as an environment variable of its own name in every
  step of a job that resolves it (an environment secret only in the job that names the environment);
- a fork run gets none: its payload carries `secrets: {}` and `secrets_denied: true`;
- precedence is **environment > repository > organization** (§5.1b): an environment secret wins over a
  repository secret of the same name, which wins over an organization secret's;
- masking is server-side and **best effort**. The server and the runner share ONE rule
  (`threads.MaskValues`): a value of 4+ bytes is replaced wherever it appears, longest value first; a
  **multi-line** value is masked whole and line by line (a streamer that splits a frame cannot make a
  line leak); a value **shorter than 4 bytes** is replaced only as a whole token (bounded by
  non-identifier bytes, so `ab` in `crab` survives while `code=ab` is masked). An encoded, split or
  derived value still prints — there is no in-band redaction guarantee;
- masking covers every place a runner can echo a value before it is STORED: log frames, step results
  (the error text), workflow-command annotations and the step summary. The runner applies it as it
  streams; the server re-applies it on `AppendLog` and on `complete`, so a bugging or older runner
  cannot write a value into the bucket. A step output (`$SPINDLE_OUTPUT`, §3.9) stays data by design:
  it is stored on the job record and substituted into a dependent job verbatim, so never put a secret
  in an output and never echo one into an output file. Pass it as a secret to the dependent job
  instead;
- secrets are stored sealed (AES-256-GCM, bound to repository + name). Without
  `server.secrets_key` or `server.auth.session_secret` the store fails closed: the secrets API
  answers `503`, Settings → Secrets explains which key to set, and attempt tokens are signed with a
  process-local key (so a restart invalidates in-flight jobs' tokens).

### 5.1a Variables

Variables are non-secret configuration with the same storage and redaction posture as secrets (the
value is sealed, never returned, and masked in the log). Read one as `${{ vars.<NAME> }}`.

| Method | Path | Body / answer |
|---|---|---|
| `GET` | `/{o}/{r}/api/threads/variables` | `{variables: [{name, created_by, created_at, updated_at}], enabled}` |
| `PUT` | `/{o}/{r}/api/threads/variables/{name}` | `{"value": "…"}` → `204` (admin) |
| `DELETE` | `/{o}/{r}/api/threads/variables/{name}` | `204` (admin) |
| `GET`/`PUT`/`DELETE` | `/api/v1/orgs/{org}/threads/variables[/{name}]` | the same, org owner only |

- precedence: **environment > repository > organization** (the same order secrets take, §5.1b): a
  job that names an environment gets its variables over the repository's, which win over the
  organization's of the same name; only trusted jobs receive them;
- names match `[A-Za-z_][A-Za-z0-9_]{0,63}`; `GITHUB_`, `SPINDLE_`, `FORGEJO_` and `RUNNER_`
  prefixes are reserved and refused at save (a variable must not shadow the platform's own
  identity);
- resolution is two exact-key GETs on the claim path (organization then repository), plus the
  environment's own document when the job names one — never a LIST;
- `vars.*` is readable in a step field, a step `if:`, a job `name`, a job `outputs:` value and a
  **job-level `if:`** (PAR-058): the server resolves the values when it decides eligibility, so a
  false condition is not an error. It is not readable where no variable value is resolved —
  `runs-on`, a concurrency group, `run-name`, or a timeout — and a parse error names that site.

### 5.1b Organization secrets

The same write-only surface at the organization scope, for a credential several repositories share
(PAR-115). Managed in **Settings → Secrets** on the organization page (owner only) or the API:

| Method | Path | Body / answer |
|---|---|---|
| `GET` | `/api/v1/orgs/{org}/threads/secrets` | `{secrets: [{name, visibility, repositories, created_at, updated_at}], enabled}` — policy and timestamps, never a value |
| `PUT` | `/api/v1/orgs/{org}/threads/secrets/{name}` | `{"value": "…", "visibility": "all"\|"selected", "repositories": ["api"]}` → `204` |
| `DELETE` | `/api/v1/orgs/{org}/threads/secrets/{name}` | `204` |

- **Precedence is environment > repository > organization.** A repository secret of the same name
  wins over the organization's; an environment secret wins over both. A variable is never a secret and
  vice versa: `${{ vars.X }}` and `${{ env.X }}` cannot read one.
- **Repository policy.** `visibility: "all"` (the default) resolves in every repository the
  organization owns. `visibility: "selected"` resolves only in the listed repositories (bare names, or
  `org/name`); **an unselected repository gets nothing** — not an empty value, not the name.
- Only **trusted** runs resolve them, exactly like repository secrets; a fork run's payload carries
  none. Organization secrets are masked with the same rule (§5.1), and a secret's scope is recorded on
  the job as `secret_scopes` (name → `organization`/`repository`/`environment`) so the run page says
  WHERE a credential came from and never WHAT it was.
- Storage is ONE sealed document per organization (`orgs/<org>/threads/secrets.json`, bounded at 100
  entries) so a claim resolves the whole organization scope with a single GET — never a LIST over the
  org prefix and never one GET per secret. The envelope's AAD binds `org:<org>` + name, so an
  organization secret cannot be replayed as a repository secret (or the reverse) even when both hold a
  name.
- Authorization is the org ownership seam (`CheckOrgOwner`, host admin bypasses) on every read and
  write, and each write is audited by name and policy — never a value. A caller who is not an owner
  gets a real `403`; an unauthenticated caller gets `401`; neither is a `404` that would confirm or
  deny that a secret exists.
- **There is no user scope, deliberately.** A run executes as the REPOSITORY, not as a person: an
  actor is who triggered it, but the credential belongs to the repository. A per-user secret would let
  one contributor's personal credential be read by a workflow they did not write, which is exactly the
  confusion the scope model removes. This is the same decision variables took (§5.1a), and it is
  recorded as D-22.

### 5.2 The check every run passes through

| Run | Workflow read at | `trusted` | Secrets | Job token |
|---|---|---|---|---|
| `push` (client push or PR merge) | the pushed commit, or the merge commit | yes | yes | per `permissions` |
| `pull_request`, same repository | the head commit | yes | yes | per `permissions` |
| `pull_request`, fork | the **base branch tip** | no | none | `contents: read` always |
| `workflow_dispatch` | the resolved ref's commit | yes | yes | per `permissions` |
| `schedule` | the default branch tip | yes | yes | per `permissions` |

Trust is server-side policy: a workflow cannot opt into the other class, and a fork PR never runs
its own copy of the workflow file.

### 5.3 The job token

Every step receives `$SPINDLE_JOB_TOKEN`. It is scoped to **one attempt of one job in one
repository**:

- always: read of its own repository (so a job can clone even a private repository, whatever the
  identity gate says about the non-member `ci:<runner>` principal);
- the platform API authority is the **effective `permissions:` map** (§5.3.1) — the same map that
  adds `push` for `contents: write` is resolved once, at the credential boundary, into the minimum
  repository role those scopes imply. Nothing the map does not name is allowed: an action the map
  does not cover is a real `403` naming the missing scope, never a silent downgrade;
- `contents: read`, `none`, or no `permissions` at all means no push. A fork run is pinned to
  `contents: read` regardless of what the file declares;
- the push policy still applies on top, because the token is a `ci:<runner>` principal and never the
  triggering user: a protected ref refuses it exactly like any other non-owner;
- it expires with the job — the job timeout plus 10 minutes, two hours when the job declares no
  timeout, capped at six hours — and is revoked at completion (the server re-reads the job record and
  rejects a terminal or superseded attempt);
- a push authenticated by it **never starts a workflow run** (the `GITHUB_TOKEN` rule), so a
  `contents: write` workflow that pushes cannot rebuild itself forever. The push lands, mirrors may
  sync, and a PR head may move — with no `push` run and no `pull_request: synchronize` run.

#### 5.3.1 The scope matrix (PAR-112, D-11)

`permissions:` is a **closed vocabulary**, not an open map. A scope this server has no enforced
surface for is a **named parse error at push time**, naming the scope and the supported set — an
accepted-but-inert key would promise an authority the token does not carry (the bug this matrix
removes). The supported set is exactly what the token can do:

| Scope | Levels | What the token may do |
|---|---|---|
| `contents` | `read`, `write`, `none` | `read`: clone/fetch the run's repository. `write`: also push to it (the `push` git scope). |
| `issues` | `read`, `write`, `none` | `read`: read the repository and its issues/comments. `write`: the triage role — label/assign/close others' issues, manage labels and milestones. |
| `pull-requests` (`pull_requests` is an accepted alias) | `read`, `write`, `none` | `read`: read pull requests. `write`: the write role — open/update PRs and create releases. |
| `deployments` | `read`, `write`, `none` | `read`: list/get deployment records. `write`: create a deployment and append its statuses for the run's repository (§5.7). It never reaches the Threads operator surface (dispatch/cancel/approve). |
| `packages` | `read`, `write`, `none` | The OCI registry of the run's repository (docs/features/14_packages.md §4): `read` pulls images, `write` pushes and pulls. The job token is exchanged at `/v2/token` for a short-lived, repository-scoped registry token; it is never accepted as a registry bearer credential itself and never reaches another repository. |
| `checks` | `read`, `write`, `none` | The check-run/status surface (PAR-021): `read` reads the run's repository's check runs, `write` creates and updates them (the external-CI reporting path). Repository-scoped; it never manages CI tokens or any admin surface. |
| `statuses` | `read`, `write`, `none` | GitHub's legacy commit-status name. This server has one status/check-run surface, so it grants exactly what `checks` grants. |
| `id-token` | `write`, `none` | Exchange for a short-lived OIDC ID token (§5.5). `read` is not a level. |
| `read-all` / `write-all` (scalar, or the `all` key) | — | Sets the baseline for `contents`, `issues`, `pull-requests` only. **`write-all` never grants `id-token`** (S10), and it does not imply `deployments`, `packages`, `checks` or `statuses`: a job that wants a deployment, an image push or a check-run write names that scope. |

Refused **by name**, with the reason, because this server has no job-token surface for them:
`actions` (the Threads operator surface: dispatch, cancel/rerun, approve, environments/secrets — a
job that could restart its own run would loop, and dispatch would route around the job-push
suppression rule), `attestations`, `discussions`, `metadata`, `models`, `pages`,
`repository-projects`, `security-events`, `wiki`; any other spelling is an unknown scope. The
importer (`spindle threads import`) drops a refused scope and names it in the conversion report, so
a converted file always parses.

**How the platform API is scoped.** A job token is not a user and never becomes one: it has no
session, no host `write`/`admin` flag, and does not resolve through access bindings or org rosters.
Within its own repository its authority is the *minimum* role the scopes imply — `issues: write` ⇒
triage, `contents: write` or `pull-requests: write` ⇒ write, any read scope ⇒ read, and a token that
names only `id-token` (or `permissions: {}`) ⇒ no repo role. That role is recomputed from the job
record at every credential resolution, so a completed or superseded attempt stops working
immediately. The mapping never reaches `admin`, which is why a job token cannot mint another
credential (checks tokens, invites, repository settings all require admin/session authority), and it
never applies to another repository — a write there is a `403`.

Roles are ordered, so a higher tier carries the lower tier's actions: a `contents: write` job can also
label an issue, exactly as a write-role human can. Four exceptions are enforced beyond the tier:

- **Check runs are `checks`/`statuses`, not a repository role.** A `contents: write` job cannot forge
  a check context by virtue of its role: only a job token declaring `checks: write` (or `statuses:
  write`) may create or update a check run, and only in its own repository. The dedicated `wct_` CI
  token remains the other way an external CI reports (05 §3).
- **OCI packages are `packages`, not a repository role.** A job token is exchanged at `/v2/token` for
  a registry token whose scopes are capped at the job's own repository (`read` ⇒ pull, `write` ⇒
  pull+push); the `ci:<runner>` principal owns no registry role, and the job token is refused as a
  registry bearer credential. `contents: write` therefore cannot push an image.
- **Releases** are a `contents: write` capability specifically: `pull-requests: write` gives the
  write role but not the release surface.
- **The Threads operator surface** (dispatch a run, cancel/rerun, approve a held run, manage
  environments/secrets) never accepts a job token: those are the `actions: write` / maintain / admin
  capabilities expressed by the refused `actions` scope, and dispatching would let a job route around
  the job-push suppression rule.

**Divergence from GitHub, recorded:** spindle's own role model lets any reader comment on an issue
(`issues`/PRs are readable, commenting is a read-role action in 01 §5), so a job token carrying only
`contents: read` can also post a comment. Everything triage- or write-shaped (labels, assignment,
closing, merging, releases, pushes) still requires the matching scope. Tightening comments to
`issues: write` for job tokens alone would make the same credential behave differently from every
other reader; it is deliberately not done here.

Read it from the environment; never write it into argv or `.git/config`:

```sh
git -c credential.helper='!f() { echo username=ci; echo password=$SPINDLE_JOB_TOKEN; }; f' \
    push origin HEAD:refs/heads/ci-output
```

Use the same token against the platform API in a step (it is `$GITHUB_TOKEN` too):

```yaml
name: comment
on: [push]
permissions:
  issues: write
jobs:
  comment:
    runs-on: linux
    steps:
      - run: |
          curl -sS -X POST -H "Authorization: Bearer $GITHUB_TOKEN" \
            -H 'Content-Type: application/json' \
            -d '{"body":"built by CI"}' \
            "$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/api/issues/1/comments"
```

**Aliases: one token, three spellings.** Every repository-code step also receives the same
credential as `GITHUB_TOKEN` (GitHub's spelling) and `FORGEJO_TOKEN` (Forgejo's), and
`${{ secrets.GITHUB_TOKEN }}` resolves to it in any substituted site — that is what a Forgejo
workflow's `FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}` checkout line expects (§3.6). No
second token is minted: the alias is the same `wjt_` attempt credential, with the same scopes and
the same expiry, so "do not escalate by asking for another name" holds by construction. Details:

- an **authored** `env:` value for `GITHUB_TOKEN`/`FORGEJO_TOKEN` wins (a workflow that sets
  `GITHUB_TOKEN` to its own PAT — `publish-github.yml` does — keeps the PAT);
- the secret name `GITHUB_TOKEN` (and `FORGEJO_TOKEN`) is **reserved**: the job token always wins,
  and a stored repository secret with either name is ignored for the job (its value is still masked)
  — GitHub refuses to create such a secret, and a look-alike must never authenticate the checkout;
- delivery is by **name**: the value rides the docker client's environment, never argv, the create
  payload or `.git/config`, and it is masked in the log stream like a repository secret (an echoed
  `$GITHUB_TOKEN` prints `***`);
- a `docker://` action never receives it (third-party image, same rule as secrets);
- `${{ github.token }}` and `${{ forgejo.token }}` are **not** provided (there is no `github.token`/
  `forgejo.token` context here — use the environment variable); `${{ secrets.FORGEJO_TOKEN }}`
  resolves to the same value as `${{ secrets.GITHUB_TOKEN }}`.

**A fork run is read-only by default, and a per-repository switch can change
that (D-13, PAR-118).** A fork pull request is untrusted, so its job token is
pinned to `contents: read` and it gets no repository or environment secrets. A
maintainer (or admin) may turn on **`send_write_tokens`** for the repository —
on the Threads page or `PUT …/api/threads/settings` — and the next fork run's
token then carries the scopes its workflow declares. The decision is audited
(`repo_send_write_tokens`) and recorded on each affected run (`write_tokens` on
the run header, shown as a badge), so a reader can see that this run had write
access and why. It still never grants `id-token` (untrusted runs cannot exchange
one) and still never grants secrets. Off is the default; the switch is
`maintain`+.

**The default when a workflow declares no `permissions:` is `contents: read`** (PAR-114, D-12). This
is a recorded divergence from Forgejo, whose job token is implicitly write-capable and ignores
`permissions`: a Forgejo workflow that relies on the implicit write fails here with a real `403`
from the push gate — `the job token has no push permission (workflow permissions: contents: read)`
— rather than being silently upgraded. The default is the operator's decision
(`threads.default_permissions`, read or write); changing it is a configuration change, not a
workflow one. Unrecognised permission levels never widen access, and a fork run is pinned to
`contents: read` unless the repository's `send_write_tokens` switch (above) says otherwise.

### 5.4 The approval gate

A fork pull request is created in **`pending_approval`** — jobs written, checks mirrored as
`pending`, but nothing enqueued, so no runner can see it — when **all** of these hold:

1. the event is a fork `pull_request`;
2. the actor is not `maintain`/`admin` in the repository (and not a configured instance admin);
3. the actor has no previously **merged** pull request in this repository (authorship of a merged PR,
   never a commit's author field);
4. `threads.approve_first_time_forks` is `true` (the default).

A `maintain`+ user then **approves** (run page button, or
`POST /{o}/{r}/api/threads/{run}/approve`) — the run queues and runners may claim it — or **denies**
(`POST /{o}/{r}/api/threads/{run}/deny`), which cancels it without ever executing. A second decision
is `409`. If the evidence source cannot be read, the run is held (fail closed). A same-repository PR
is never gated: its author already has write access.

The same `pending_approval` state machine also carries an **environment** hold (§5.6): a run with a
job naming an environment that has required reviewers is created `pending_approval` with
`approval_kind: environment`, and only a listed reviewer (write or more) or a repo admin may decide
it. An environment hold outranks the fork hold on the same run (the stricter reviewer rule applies).

A **`workflow_run`** whose upstream run was untrusted (a fork PR) is the third hold kind
(`approval_kind: workflow_run`, PAR-019/D-3). Its own file comes from the default branch, but the
upstream run it follows executed attacker-controlled code; a maintainer must approve it before any
job starts. An approval is per run, never a repository-wide grant.

**Cross-run artifact rule.** The artifacts API enforces this hold at its own
authorization boundary (§3.4, "The `workflow_run` hold"): while
`ApprovalKind == "workflow_run"` is pending, a cross-run download — and attaching a `run-id` input,
which is the same lookup — is a **409** naming the parent run and
`workflow_run approval pending`, never "not approved yet" and never queued work. `ApproveRun` moves
the run to `queued`, so the check is "has this run been approved yet", not a stored grant;
`ParentTrusted == false` is the fact that put it there.

### 5.5 Job OIDC tokens (cloud identity)

A job can present a short-lived, audience-scoped ID token to a cloud provider — the Threads
equivalent of GitHub Actions' OIDC token. It is **off unless the job asks for it**, and both
spellings mean the same grant (PAR-119):

```yaml
jobs:
  deploy:
    runs-on: [self-hosted, linux, docker]
    permissions:
      id-token: write        # explicit; `write-all` deliberately does NOT grant it
    steps:
      - uses: actions/checkout
      - name: assume the deploy role
        run: |
          TOKEN=$(curl -sS -H "Authorization: Bearer $SPINDLE_ID_TOKEN_REQUEST_TOKEN" \
            -H 'Content-Type: application/json' \
            -d "{\"audience\":\"sts.amazonaws.com\"}" \
            "$SPINDLE_ID_TOKEN_REQUEST_URL" | sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
          …
```

The Forgejo spelling is accepted with zero edits:

```yaml
name: deploy
on: [push]
jobs:
  deploy:
    runs-on: [self-hosted, linux, docker]
    enable-openid-connect: true    # the job may exchange; trusted runs only
    steps:
      - run: echo deploy
```

`enable-openid-connect: true` (workflow or job level) grants the exchange; `permissions: id-token:
write` grants it too. An explicit `permissions: id-token: none` **denies** regardless of the
boolean, a fork run never qualifies, and `permissions: write-all` never implies it.

- **What the job gets.** `SPINDLE_ID_TOKEN_REQUEST_URL` + `SPINDLE_ID_TOKEN_REQUEST_TOKEN` (and the
  GitHub spellings `ACTIONS_ID_TOKEN_REQUEST_URL` + `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, same values) in
  every repository-code step. A `docker://` action gets neither. The URL is container environment;
  the token is passed by name, never in `docker` argv.
- **Exchange.** `POST` the request URL with `{"audience":"…"}`, or append `?audience=…` and send no
  body (GitHub's request-URL flow, PAR-120); both are the same contract. The answer is
  `{"value":"<signed JWT>"}`. **The audience is required** — there is no implicit default, because a
  token minted for a guessed audience is replayable at that service; a missing or empty audience is a
  `400` naming the field, and a control character or >512 bytes is refused. The exchange is limited
  to **10 requests per minute per job attempt** (`429` with `Retry-After`) and is bound to one
  attempt: a superseded attempt, a finished job, or a request token presented at another job's path
  is refused.
- **The token.** RS256, five-minute lifetime, signed with the instance key. The issuer is
  `server.public_url` (set it: providers fetch
  `<public_url>/.well-known/openid-configuration` and `<public_url>/.well-known/jwks.json`). The
  claim set is closed (a struct, so no user data can add a claim). **Subject templates:**

  | Run | `sub` |
  |---|---|
  | a job naming an environment | `repo:<owner>/<name>:environment:<environment>` |
  | `pull_request` | `repo:<owner>/<name>:pull_request` |
  | push / tag / dispatch | `repo:<owner>/<name>:ref:<full ref>` |

  An environment subject outranks the ref so an environment-scoped cloud role cannot be assumed from
  a branch; a PR subject is distinct from the base ref it builds, so a provider can tell a PR run
  from a `main` push. The environment name is parser-pinned to
  `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`, so it cannot inject a separator into the subject.

  | Claim | Value | Configurable? |
  |---|---|---|
  | `iss` | `server.public_url` | yes — instance config |
  | `aud` | what the job asked for | yes — per exchange, required |
  | `sub` | the table above | no (templates are fixed) |
  | `exp` / `iat` / `nbf` | 300 s lifetime | no |
  | `jti` | random per token | no |
  | `repository` / `repository_owner` | `owner/name` / `owner` | no |
  | `ref` | the full ref (`refs/heads/main`, `refs/tags/v1.2.3`; a PR run's **base** ref) | no |
  | `ref_type` | `branch` or `tag` | no |
  | `ref_protected` | `true` when a blanket `protect` rule covers the ref in the repository's `policy.json` | no — follows the policy |
  | `sha` | the built commit | no |
  | `workflow` | the run's workflow display name | no |
  | `job_workflow_ref` | `<owner>/<name>/<workflow path>@<ref>` | no |
  | `job`, `run_id`, `run_attempt`, `event_name` | the run identity | no |
  | `runner_environment` | `self-hosted` (there is no hosted fleet) | no |
  | `environment` | the environment name — **present only when the job names one** | no |

- **Who may ask.** Only a **trusted** run whose job's effective permissions grant `id-token` (either
  spelling above). A fork run never gets a request token and cannot exchange one; a request token is
  bound to one attempt of one job and expires with it; a finished job's token is refused. Configure
  the provider's trust policy against `iss`, `aud`, and the `sub`/`repository` claims.
- **The request token is a bearer credential.** It is stored hash-only server-side, delivered only to
  its own job, and masked in the runner's logs (best effort) — but a step that echoes or derives it
  defeats that. Never print it. The signed ID token is a stateless JWT: it cannot be made single-use
  without server-side state at the provider, so the short TTL and the per-attempt request token are
  the replay controls; `jti` is there for the provider's audit.

**Trust statement.** The signing key lives in the bucket (`threads/oidc/signing-key.json`) and is
generated independently of the session/attempt-token keys — a compromised session secret does not
sign ID tokens and vice versa. The bucket is therefore a trust root for this feature exactly as it
already is for every repository and secret: whoever can write the key object can mint tokens this
instance signs. There is one instance-wide key (rotated automatically every 90 days; old public keys
stay in the JWKS for an hour so in-flight tokens still verify), and no per-repository key. The
private material is never served by a page or an API.

### 5.6 Deployment environments

An **environment** is a repository-scoped deployment target with its own required reviewers and its
own secrets (`docs/PLAN-THREADS-ENVIRONMENTS.md`). A job targets one with `environment:`:

```yaml
jobs:
  ship:
    runs-on: [self-hosted, linux, docker]
    environment:
      name: production
      url: https://prod.example.test/     # optional; the run page links it
    steps:
      - uses: actions/checkout
      - run: ./deploy.sh
```

Managed in **Settings → Environments** (maintain or admin) or the API — the record names reviewers
and secret names, never a value:

| Method | Path | Body / answer |
|---|---|---|
| `GET` | `/{o}/{r}/api/threads/environments` | `{environments: [{name, required_reviewers, team_reviewers, wait_timer_seconds, prevent_self_review, deployment_branches, secrets, vars, created_at, updated_at, version}], enabled}` |
| `GET` | `/{o}/{r}/api/threads/environments/{name}` | `{environment: {…}}` |
| `PUT` | `/{o}/{r}/api/threads/environments/{name}` | `{"required_reviewers":["ada"],"team_reviewers":["acme/releasers"],"wait_timer_seconds":600,"prevent_self_review":true,"deployment_branches":["main","release/*"]}` → the environment |
| `DELETE` | `/{o}/{r}/api/threads/environments/{name}` | `204` |
| `GET` | `/{o}/{r}/api/threads/environments/{name}/secrets` | `{secrets: [{name, created_at, updated_at}], enabled}` |
| `PUT` | `/{o}/{r}/api/threads/environments/{name}/secrets/{secret}` | `{"value": "…"}` → `204` |
| `DELETE` | `/{o}/{r}/api/threads/environments/{name}/secrets/{secret}` | `204` |
| `GET` | `/{o}/{r}/api/threads/environments/{name}/variables` | `{variables: [{name, created_at, updated_at}], enabled}` |
| `PUT` | `/{o}/{r}/api/threads/environments/{name}/variables/{variable}` | `{"value": "…"}` → `204` |
| `DELETE` | `/{o}/{r}/api/threads/environments/{name}/variables/{variable}` | `204` |

- **A reviewer needs at least write access**, checked when the environment is saved: a name that
  cannot write is a `400` naming the reviewer, never a stored gate nobody can open. At most **six**
  reviewers and teams together, deduped and lowercased (GitHub's documented ceiling).
- **Team reviewers** are spelled `org/team` and must resolve through the team directory when the
  record is saved (an unknown or empty team is a `400`); the members at decision time are the
  approvers. A team that disappears after the record was saved fails the deployment closed with a
  named reason rather than holding it for nobody.
- **Every required approver must approve.** A listed reviewer's `POST …/approve` records that
  principal's approval on every gated deployment; the run is released only when nobody is left
  awaited (or a repo admin approves, which bypasses the reviewer set and is audited as such). A
  partial approval keeps the run `pending_approval` and the page names who still owes one — the
  verb is never a silent no-op.
- **Prevent self-review** refuses the principal who triggered the run, on both surfaces, with a
  `403` naming the rule. The same principal may still deny the run.
- **The wait timer** (0–360 minutes) starts when the deployment is created. Every approval can be
  in and the timer still holds the job: the claim path refuses to offer it before `wait_until`,
  which the page and the API show.
- **Deployment branches and tags** is an allowlist of exact names and `*` patterns, matched
  against the short branch/tag name and the full ref. Empty means every ref. A run from any other
  ref **fails the deployment job** with a reason naming the ref and the patterns — it is never
  held for an approval that could not help.
- **A job naming an undefined environment fails** with `environment "x" is not defined in this
  repository (create it in Settings → Environments)`. The rest of the workflow still runs.
- **Required reviewers hold the run.** The run is created `pending_approval` (`approval_kind:
  environment`, `environments: ["production"]`), its checks mirror `pending`, and **nothing is
  enqueued** — no runner can see the deployment. Only a **listed reviewer (holding write or more) or
  a repo admin** may approve (`POST …/api/threads/{run}/approve`) or deny (`…/deny`); approval stamps
  the job's deployment record with who and when and then queues the job once every required
  approver has approved (a partial approval is recorded and leaves the run pending; a second
  approval by a principal who has already approved is a no-op, and approving a run that is no
  longer pending is `409`). With **no available reviewer the run simply stays pending** — there is
  no timeout and no auto-approval. The approval hold is also a **claim-path** gate: the deployment
  record's `pending` state refuses a claim independently of the run header, so a stale queue entry
  can never hand a gated job to a runner before a reviewer decides.
- **Denied** fails exactly the deployment jobs, with `deployment to environment "production" denied by
  <who>`; the run's other jobs continue so the failure reads beside the build.
- **Secrets** are stored exactly like repository secrets (AES-256-GCM, write-only) but bound to the
  environment: only a job that names it, in a **trusted** run, with an **approved** deployment,
  receives them. They are injected as environment variables of their own name and masked best effort
  in logs. **They override repository secrets of the same name** (GitHub's rule); the rest of the
  repository set is still present.
- **Variables (PAR-117)** are the non-secret twin, stored with the same write-only/sealed posture
  this product chose for variables (the value never comes back from a read and is masked in logs).
  A trusted job that names the environment reads them as `${{ vars.NAME }}`; they **override
  repository and organization variables of the same name** (the environment is the most specific
  scope), and a name may never use a reserved prefix (`GITHUB_`, `SPINDLE_`, `FORGEJO_`, `RUNNER_`).
- **Deployments serialize per environment (PAR-136).** The first deployment to an environment holds
  the environment's slot; every later one is recorded in trigger order as `queued` with one visible
  reason naming the holder. A queued job is not offered to a runner, and an API deployment cannot
  move to `in_progress` before it is promoted. When the holder reaches `success`/`failure`/`error`/
  `inactive` (or its job ends), the next waiter is promoted — it loses its queue position (job) or
  moves `queued → pending` (API). A cancellation of a queued deployment removes it from the wait
  list. The queue is bounded at 100 waiters; a 101st is a named `409`, never a silent overflow.
- **A fork run never receives environment secrets**, cannot be listed as a reviewer (it has no write
  access) and therefore cannot approve its own deployment. A fork run naming an environment **with**
  reviewers stays pending until a reviewer decides — or fails closed if nobody does; naming one
  **without** reviewers runs like any fork job (no secrets).
- **Deletion while a run is pending** fails the deployment job closed (`… the environment no longer
  exists`) instead of releasing it, and a **re-run** is a new decision: the gate is re-applied.
- The run page shows the environment, `pending`/`approved`/`denied`, the decider and the time, the
  declared URL, who has approved so far, who is still awaited and the wait-timer deadline. It never
  shows a secret.
- **A pending deployment is visible, notifiable and cancellable** (PAR-135): the **run list** carries
  a badge naming the environment (`awaiting deployment · production`) and a `cancel` control for
  anyone with write access, the run page says who or what it waits for, and the required reviewers
  receive one notification per hold (a repo-level review request naming the run, its workflow and
  the environments). The audit family records every approval with the principal and whether it was
  partial. An operator who cannot approve can always stop the work without hunting for the page.

### 5.7 Deployment records and the deployments API

Every deployment — whether a workflow job named `environment:` or an external CI called the API —
is one record in the repository's deployment family:

```
repos/<o>/<r>/threads/deployments/<id>.json
repos/<o>/<r>/threads/environments/<name>/queue.json   the serialization slot
```

The record carries `environment`, `ref`, `sha`, `task`, `description`, the environment `url`, the
creator, the producing `run_id`/`job_id`, the latest `state`, the queue position/reason and a bounded
`statuses` history (newest last, at most 100 entries). The environment page lists them (the
`Deployments` section of each environment), and the API is the same read:

| Method | Path | Body / answer |
|---|---|---|
| `GET` | `/{o}/{r}/api/threads/deployments?environment=&after=&n=` | `{deployments: [{id, environment, ref, sha, state, queue_position?, queue_reason?, statuses: […]…}], more}` |
| `POST` | `/{o}/{r}/api/threads/deployments` | `{"ref":"refs/heads/main","environment":"production","description?","url?","log_url?","sha?","task?","transient_environment?","production_environment?"}` → `201 {deployment: …}` |
| `GET` | `/{o}/{r}/api/threads/deployments/{id}` | `{deployment: …}` |
| `POST` | `/{o}/{r}/api/threads/deployments/{id}/statuses` | `{"state":"in_progress","description?","environment_url?","log_url?"}` → `201 {deployment: …}` |

- **States** are GitHub's deployment-status vocabulary: `queued`, `pending`, `in_progress`,
  `success`, `failure`, `error`, `inactive`. A transition appends to the history; `success`,
  `failure`, `error` and `inactive` are terminal and release the environment slot.
- **Authorization is the boundary.** A repo `write` role may create/status/read. A workflow job token
  is accepted only for **its own repository** and only with `deployments` (`write` for create/status,
  `read` for list/get) — a `contents: write` token is refused, and another repository is a `403`.
  The job token never becomes admin and never reaches the Threads operator surface.
- **Environment binding fails closed.** The environment record must exist (an unknown name is a
  `404`), and the ref must pass the environment's deployment branch/tag policy (a forbidden ref is a
  `403` naming the patterns).
- **Pagination and retention.** The list is newest-first, cursor-paged with `after` (last id of the
  previous page) and `n` (default 50, max 200), and the maintainer's threads-gc pass deletes terminal
  records older than `threads.retention` (never a live or queued one).

### 5.8 Checks, annotations, badges and filters

- **Check annotations (PAR-192).** A report to `POST …/api/checks/statuses/{sha}` may carry
  `annotations: [{level: error|warning|notice, message, title?, file?, line?, end_line?, col?,
  end_column?, raw_details?}]` (at most 50, bounded text; an unknown level is a `400`). The set is
  stored with the check run, returned by every read (`GET …/checks/{sha}`, `…/checks/statuses/{sha}`,
  the PR panel and the commit page) and rendered escaped as text with its level, file and line.
  Annotations **never change the state**: the merge gate still reads the same
  `pending|in_progress|success|failure|cancelled|skipped|error`, and only `success` is green.
- **Check-context naming (recorded decision).** A Threads job's check context is
  `threads/<workflow>/<job>`, lowercased and slugged to the checks charset (§2.3). External CI names
  its own contexts; a required-checks rule (`require_checks`) unions contexts from every rule whose
  `match.refs` covers the PR's base ref (per-branch required checks), and a missing context is
  non-green — so a branch gate can require a Threads job and an external CI context in one rule.
- **Re-run from the PR panel.** The PR's Checks card names the Threads run that reported the PR's
  head/merge sha and offers `re-run checks` to a writer; the button posts the same
  `…/threads/{run}/rerun` verb the run page uses (the service re-checks the role). An external CI
  context has no run, so the control is absent rather than dead.
- **Workflow badge (PAR-193).**
  `GET /{o}/{r}/threads/badge.svg?workflow=<name-or-path>&branch=<branch>` renders the latest run's
  conclusion for that workflow on that branch (default branch when `branch` is omitted). It is an
  SVG for readers of the repository (public repositories answer anonymously), with `passing` /
  `failing` / `running` / `queued` written as text, never colour alone.
- **Run-list filters (PAR-194).** `GET …/api/threads` and the Threads page accept `workflow`, `state`,
  `branch`, `event`, `actor`, `after` and `n`. The filter scans at most 1000 run headers per page
  (`filterScanMax`), so a narrow filter over a large repository still answers one bounded request;
  `more` says whether the scan hit its bound. The page form is a plain GET, so the filter is
  shareable and the layout stays usable at 390 px.
- **Cancel granularity (PAR-195).** `POST …/threads/{run}/cancel` ends the whole run;
  `POST …/threads/{run}/jobs/{job}/cancel` (body optional `{"force":true}`, or `force=on` from the
  page) ends **one** job and leaves its siblings running. A non-forced running attempt receives the
  cancel signal and ends through the runner's ack; a forced cancel is terminal at once and a later
  runner `complete` is a `409` naming the state. The job record carries `cancelled_by` (the
  principal), `cancel_reason` and `force_cancelled`, and the first cancel wins (a retry never
  rewrites the actor).

## 6. Triggers in detail

### 6.1 Push

- the trigger set is GitHub's: `on.push.branches` filters branch pushes (`refs/heads/*`) and
  `on.push.tags` filters tag pushes (`refs/tags/*`), independently. A `tags`-only workflow does
  **not** fire on a branch push, and a `branches`-only workflow does not fire on a tag; a bare
  `on: push` fires on both. Branch and tag deletes create no run;
- exact names and `*` wildcards only (`release/*`): there is no `!` negation and no `**`; an
  absent or empty filter for the kind being delivered matches every ref of that kind;
- a landed tag create starts a run exactly like a branch push: the push hook accepts
  `refs/tags/…` (a tag delete or a notes ref still starts nothing) and the tag name selects the
  `tags` globs. A tag run has **no branch** — `github.ref` is the full `refs/tags/v1.2.3`,
  `github.ref_name` is `v1.2.3` (the tag name, never a branch), `github.event_name` stays
  `push`, and the run's stored ref is the full ref while its branch field is empty, so a tag can
  never be mistaken for a branch of the same name (the implicit supersede key is (workflow file,
  ref) with the prefix included, so `v1.2.3` as a branch and as a tag are independent runs);
- the workflow file is read at the pushed commit, so a push that changes both code and workflow uses
  the new workflow;
- a push authenticated by a job token creates nothing (§5.3), for branches and tags alike;
- a merge-queue ref (`refs/heads/gh-readonly-queue/…`) is never a branch push: it starts
  `merge_group` runs only (§6.10), whatever `on.push` says;
- a pull request **merge** creates a `push` run for the base branch (the merge commit), because the
  merge moves the base ref server-side.

### 6.2 Pull request

- fires on every lifecycle edge the services emit: `opened`, `synchronize`, `reopened`, `closed`
  (a plain close **and** a merge), `edited`, `ready_for_review`, `converted_to_draft`, plus the
  shared thread-metadata actions `labeled`, `unlabeled`, `assigned`, `unassigned`, `milestoned`,
  `demilestoned`;
- `on.pull_request.branches` filters on the **target** branch and `on.pull_request.paths` on the
  changed files (merge-base diff); `on.pull_request.types` filters the lifecycle edge (§3.2):
  absent = every emitted action, `types: [opened]` = only a new PR. The vocabulary is closed — an
  action no service emits is that file's parse error naming the supported set, so a filter can
  never name an edge that will not happen;
- same repository: the head commit's workflow file runs, trusted, with secrets; the run's commit is
  the server-computed merge commit of base and head (or the head when the merge conflicts), and its
  ref is `refs/pull/<n>/merge` (`refs/pull/<n>/head` on the fallback);
- fork: the **base branch's** workflow file runs at the base tip, untrusted, no secrets, read-only
  token, and possibly behind the approval gate (§5.4). The run header records both commits (`commit`
  = the merge/head commit being built, `workflow_commit` = where the file was read), so the trust
  decision is auditable;
- `synchronize` fires only when the head actually moved, so a replay or a stale delivery cannot
  create a duplicate run for a commit the PR has left;
- update-branch publishes a head update and is delivered as a normal `synchronize`, not a second
  event.

### 6.3 Manual dispatch

From the Threads page: **Run workflow** (write role only) — choose a workflow that declares
`workflow_dispatch` (the control is hidden when none does), type the ref (pre-filled with the default
branch) and fill the declared inputs (typed controls: `select` for `choice` and `boolean`, text for
`string`, required marked, defaults pre-filled). Or from the API/CLI:

```sh
curl -X POST -H "Authorization: Bearer $SPINDLE_TOKEN" -H "Content-Type: application/json" \
  -d '{"ref":"main","workflow":"nightly.yml","inputs":{"environment":"staging"}}' \
  https://spindle.example.com/acme/widgets/api/threads

spindle threads run acme/widgets nightly.yml --ref main --input environment=staging
```

Response `201 {"runs": […]}`. An unknown ref or workflow is `404`, a bad body `400`, and an input
that does not match the declared schema `422` naming the input. `inputs` requires an explicit
`workflow`; omitting `workflow` dispatches **every** file that declares `workflow_dispatch` at the
ref. The CLI takes repeatable `--input k=v` pairs (the value is checked against the declared type)
and an optional `--priority 0…10`, which orders the queue without bypassing any hold.

### 6.4 Schedule

The parser validates cron; the **maintainer** evaluates it.

- **UTC, five fields**, the subset in §3.2;
- the sweep reads workflow files at the **default branch tip** only, so a schedule on a feature
  branch never fires; the run is `event: schedule`, `actor: schedule`, trusted, on the default
  branch's commit;
- when a process with the `maintain` role is running, the sweep is due once per wall-clock minute
  (cron's own granularity). A process started with `server.roles = ["serve"]` and no maintainer runs
  no schedules;
- ticks are **at most once**: a per-file/per-cron/per-timezone watermark object is CAS-updated **before** the run
  is created, so two instances racing the same minute create one run, and a restart re-reads the
  watermark instead of re-firing. A crash between the watermark write and run creation loses that
  one tick (deliberate: a duplicate scheduled deploy is worse than a missed one); a failed run
  creation consumes the tick and is logged, never retried in a spin;
- on a first run with no watermark, the current minute's boundary is eligible immediately, so a
  daily schedule that starts moments late still gets today's tick.

Each entry can be **disabled or enabled without a commit** (`GET/POST …/api/threads/schedules…`,
`spindle threads schedule list|enable|disable|run`; audited), and **run now** fires a manual trigger
even while the entry is disabled. A disabled entry keeps its watermark; re-enabling fires on the
next due tick. A sweeper pass fires at most one run per entry and reports the coalesced count — a
backlog is never replayed (§9.1a).

### 6.5 Release

- `on.release.types` accepts exactly the actions the releases service emits: `created` (a draft),
  `published`, `edited`, `deleted`, `prereleased` (a prerelease published), `released` (a published
  prerelease's flag cleared) and `unpublished` (published → draft). An unemittable type is a parse
  error naming the set.
- The workflow file is read at the **default branch tip**; the run's `commit`/`GITHUB_SHA` is the
  release tag's commit and `GITHUB_REF` is `refs/tags/<tag>`.
- The payload carries `release.tag_name`, `name`, `draft`, `prerelease`, `target_commitish`,
  `author.login` and a capped `assets` list (name/size, 100 entries). `jq` at
  `$GITHUB_EVENT_PATH` reads the whole record.
- Publishing a release starts the run (§3.6); editing or deleting does too when the workflow lists
  that type.

### 6.6 Issues and issue comments

- `on.issues.types` accepts `opened`, `edited`, `closed`, `reopened`, `labeled`, `unlabeled`,
  `assigned`, `unassigned`, `milestoned`, `demilestoned` — exactly the edges the issues service
  emits. `on.issue_comment.types` accepts `created` (comment editing/deletion is not implemented;
  those types are parse errors).
- Both read the workflow file at the **default branch tip**, never at a branch the commenter
  controls; the run's commit is that tip. The actor is the commenter/issue actor.
- The payload carries `issue.number`, `title`, `state`, `user.login`, `labels`, and — for a comment
  — `comment.body` (bounded to 4 KiB) and `comment.user.login`. `github.event.action` agrees with the
  action in `$GITHUB_EVENT_PATH`.

### 6.7 Repository dispatch

`POST /{owner}/{repo}/api/threads/dispatches`:

```json
{"event_type": "deploy", "client_payload": {"environment": "prod", "sha": "…"}}
```

- **Gate:** write role (GitHub's `contents: write`). Anonymous is `401`, a read/triage role `403`,
  and a Threads job token `403` — a job cannot dispatch its way around the job-push suppression.
- **`event_type`:** required, `[A-Za-z0-9._-]{1,100}`; `on.repository_dispatch.types` selects it.
- **`client_payload`:** optional object, `null`-safe. At most 10 top-level keys named
  `[A-Za-z0-9_-]{1,64}`, nesting at most 4 levels, scalar leaves (string, bool, number, null), and
  64 KiB encoded. Over a bound the answer is `413`/`422` naming the bound; the payload is never
  silently truncated. It is exposed as `github.event.client_payload.<key>` (one level per value) and
  in `$GITHUB_EVENT_PATH`.
- **Where it runs:** the default branch only. An optional `ref` is accepted only when it resolves to
  the default branch tip (422 otherwise), so a token cannot dispatch a workflow file another
  principal controls.
- **Answer:** `202 Accepted` with `{"runs": [...]}` — the trigger is asynchronous, and the runs it
  started are listed.

The same service call is reachable from the Threads page (a repository_dispatch workflow's typed
types are shown on the workflow row, not a form).

### 6.8 Following another run (`workflow_run`)

```yaml
on:
  workflow_run:
    workflows: [Build]     # upstream workflow NAMES (not globs)
    types: [completed]     # requested | completed; omitted = completed
    branches: [main]       # upstream head branch
jobs:
  follow:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.workflow_run.head_sha }}"
```

The follow-up reads its **own** file at the default branch tip (never the upstream's commit), reports
the default branch tip as its `commit`, and filters on the upstream workflow's display name, the
lifecycle edge and the upstream head branch. The payload carries the upstream run's `id`, `name`,
`event`, `status`, `conclusion`, `run_attempt`, `head_branch`, `head_sha`, `workflow_id` and
`repository` — never an artifact URL or id.

- An upstream fork run is **untrusted**; the follow-up is created `pending_approval`
  (`approval_kind: workflow_run`) and no job starts until a maintainer approves (D-3). Its jobs never
  receive secrets on the strength of the upstream run.
- `ChainDepth` records the follow-up depth; a chain deeper than 3 is refused (logged, no run). Two
  workflows that follow each other terminate instead of spinning.
- A rerun of the upstream does not re-fire `completed` for the old attempt: the fold that reaches a
  terminal state is the edge.
- Budget: the `requested`/`completed` edges are emitted only when the revision the upstream run was
  created from declares `on.workflow_run` (the same discovery already paid at creation). A
  repository with no follower workflow therefore pays no extra read on either path.

### 6.9 Create, delete, deployment and deployment_status

- **`create` / `delete`** fire for a `refs/heads/*` or `refs/tags/*` ref created or deleted, with
  `ref`, `ref_type` (`branch`/`tag`), repository and sender in the payload. The file is the default
  branch's. A push authenticated by a Threads job token starts no create/delete run (the
  `GITHUB_TOKEN` rule), and a delete has no new commit, so the run reports the default branch tip.
- **`deployment`** fires when a job's deployment record is created; **`deployment_status`** fires
  when it is decided:
  - `pending` — the environment requires reviewers and the run is held;
  - `in_progress` — a reviewer approved and the deployment may start;
  - `failure` — denied, or the deployment was failed closed (a vanished environment, a policy
    refusal).
  The payload carries `deployment.id` (the job id), `environment`, `ref`, `sha`, `task` and
  `creator.login`, plus `deployment_status.state`/`description` on the status event. The file is the
  default branch's, and deployment events count against the same follow-up chain depth.

### 6.10 The merge queue (`merge_group`, PAR-016)

- **The ref is the hand-off.** A merge queue gives CI its work through a read-only git ref with the
  documented shape:

  ```
  refs/heads/gh-readonly-queue/<base-branch>/pr-<number>-<sha>
  ```

  The push hook recognises exactly that shape (the base may contain slashes, the number has no
  leading zero, the sha is a full lowercase 40- or 64-hex id). One such push creates **one**
  `merge_group` run; the same commit pushed as an ordinary branch or a tag creates none, and a queue
  ref itself never fires `on: push`. A ref that claims the `gh-readonly-queue/` prefix but does not
  match the shape creates **nothing at all** — not a branch build — and is logged with the expected
  shape.
- **The run's identity.** `github.ref` is the full queue ref, `github.ref_name` its short name
  (`gh-readonly-queue/main/pr-7-<sha>`), `github.sha` the ref's tip (the merge-group commit), and the
  run record keeps no branch (`run.branch` is empty) so a queue entry can never be mistaken for a
  branch push. `event` is `merge_group`. The `<sha>` segment in the ref name identifies the entry's
  pull-request head and is validated as a full lowercase object id; the tip is what CI checks (the
  same split GitHub documents).
- **The workflow is the base branch's.** Both sides of a queue entry are repository-owned, so there
  is no fork trust question and no contributor file is executed: the file is read at the base branch
  tip, the run is trusted, and secrets are available under the normal rules. `types` accepts
  `checks_requested`, the one activity the queue emits; `branches`/`branches-ignore` filter the
  target branch.
- **The payload** is a bounded projection with `action: checks_requested`, `number` (the pull
  request) and `merge_group.head_ref`/`head_sha`/`base_ref`/`base_sha`. Read it as
  `${{ github.event.merge_group.head_sha }}` or from `$GITHUB_EVENT_PATH`.
- **Whose checks gate the merge.** The run's jobs mirror onto the queue head sha exactly like any
  other job (§7), so a `require_checks` rule naming the workflow's contexts reads them with
  `CheckRequiredChecks` on that sha. The queue entry is mergeable only when those checks are green —
  the gate needs no merge-group-specific code.
- **The queue is real.** [`docs/features/22_merge_queue.md`](features/22_merge_queue.md) owns the
  product surface that pushes the documented refs and consumes the run: a per-base ordered queue
  (priority then FIFO), the temporary ref's full lifecycle (prepare, publish, delete on every exit
  path, restart recovery), the required-checks gate on the checked tip, and landing through the one
  merge task. This trigger remains the contract it hands off to; the queue implements no run
  creation or merge logic of its own.

### 6.11 Concurrency caps and queue reasons (PAR-153/PAR-156)

- A repository may set `concurrency_cap` (0 = unlimited, 1…500). When a run is created while that
  many runs are already live (queued or running, not approval-held), its jobs are written with
  pending checks but **not enqueued**, and the run header carries
  `hold_reason: "repository concurrency cap N reached"`. A live run ending releases the oldest held
  run (one LIST per terminal fold, only when a cap is configured). The cap is an admission
  heuristic, not a lock: simultaneous creations can overshoot by their own count, and the reason is
  visible rather than silent.
- An **organization** may set `concurrency_cap` (0…500) at
  `orgs/<org>/threads/settings.json` — `GET/PUT /api/v1/orgs/{org}/threads/settings`, org owner
  (host admin bypasses), audited. It bounds live runs across **every repository the organization
  owns**. Admission keeps one org index (`orgs/<org>/threads/cap.json`, deleted runs pruned on a
  bounded pass) so it never LISTs every repository; a run above the cap is held with exactly
  `hold_reason: "organization concurrency cap N reached"`, and a terminal fold in ANY org repository
  releases the oldest held run (cross-repository, also from the index). The settings projection
  reports `live`/`held` counts. Removing the cap releases every held run immediately. A user-owned
  namespace is never capped, and an organization with no
  cap pays no read. Like the repository cap, it is a steady-state admission heuristic, not a lock.
- Every queued job reports **exactly one** reason in `queue_reason` (API) and as a "queued · …" chip
  (page), in the order the read model checks them: the run's hold (approval, the repository
  `concurrency_cap`, or the organization `concurrency_cap`), CI pause ("CI is paused for this
  repository", PAR-157), a deployment queued
  behind an environment's serialization slot (naming the environment and queue position), a live
  `needs:` dependency, the pending concurrency group, a full matrix admission window
  (`waiting for a matrix slot (max-parallel N)`, PAR-052), then runner availability — "no runner is
  registered", "no runner matches labels …", "fork runs are disabled on every matching runner",
  "all matching runners are busy", "waiting for a matching runner" or a declared CPU/memory
  ceiling. The field is a read model (never stored), so it changes the moment the cause clears.

## 7. Checks and merge gating

Every job owns one check record keyed by its **context**:

```
threads/<workflow>/<job>
```

Both segments are lowercased and slugged to `[A-Za-z0-9._-]` (each capped at 40 characters). The
workflow segment is the YAML `name`, the job segment the job's display name — so a matrix cell reads
`threads/build/test-1.26` (the `(1.26)` suffix becomes `-1.26`). A job named `build (1.27, amd64)`
becomes `threads/ci/build-1.27-amd64`.

| Job state | Check state | Green? |
|---|---|---|
| `queued` | `pending` | no |
| `running` | `in_progress` | no |
| `success` | `success` | **yes** |
| `failure` | `failure` | no |
| `timed_out` (its own `timeout-minutes`) | `failure` (the checks enum has no timed-out value) | no |
| `cancelled` | `cancelled` | no |
| `skipped` (`needs:` blocked) | `skipped` | no |

Only `success` is green. A cancelled or in-flight required check therefore never unblocks a merge;
neither does a skipped one. A job that is never claimed stays `pending` forever (which is exactly
why a stalled runner blocks a merge rather than waving it through).

A `require_checks` push-policy rule consumes the context strings by name (Settings → Push policy,
`policy.json`):

```json
{
  "version": 1,
  "rules": [
    {
      "name": "main-needs-ci",
      "match": {"refs": ["refs/heads/main"]},
      "effect": {"protect": {"restricts": ["create", "delete"], "require_checks": ["threads/ci/test"]}}
    }
  ]
}
```

At merge time the union of matching rules' `require_checks` is checked against the PR head's statuses;
every context must be present **and** `success`, or the merge is refused with a message naming each
offender as `<context> (missing)` or `<context> (state)`. Threads writes through the same trusted
in-process reporter as any external CI report, so no gate code knows the difference.

A `merge_group` run (§6.10) mirrors onto the queue head sha, not a pull request head, because its
`github.sha` is the queue entry's head commit. A queue card that asks "may this entry land?" reads
the same `require_checks` union for that sha and gets the merge-group run's checks — the gate has no
merge-queue-specific branch, and queue creation itself is not part of it.

Checks are **independent of run retention**: deleting an old run never deletes its checks, so an old
green (or red) required check still gates. Only the run page's log/step history disappears.

The commit page shows the same rows as a **Checks** panel; the Threads run page shows each job's
context next to its state, plus the `reason` a job did not run (`if: …`, `needs: …`) and a
`· tolerated` marker on a failure its `continue-on-error` let through.

## 8. The runner

### 8.1 Flags and environment

`spindle runner serve` — flags ⊕ environment; there is no `[runner]` section in `spindle.toml`
(a runner has no data dir and no store; it is a protocol client).

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--server` (global) | `SPINDLE_SERVER` | login state, then `http://127.0.0.1:8080` | spindle origin; the checkout-origin fallback when `server.public_url` is unset (§4.4) |
| — | `SPINDLE_RUNNER_TOKEN` | — | the `wrt_` credential (wins over the token file) |
| `--token-file` | `SPINDLE_RUNNER_TOKEN_FILE` | — | 0600 file holding the credential; `--register` writes it |
| `--register` | — | — | one-time `wrg_` registration token to consume, then serve |
| `--register-file` | — | — | read the one-time `wrg_` token from a 0600 file (keeps it out of argv) |
| `--config` | — | — | TOML file of `[[runner]]` entries (offline registration, several scopes per process) |
| `--ephemeral` | `SPINDLE_RUNNER_EPHEMERAL` | off | one-job worker: exit after a job; the server deletes its record and credential (PAR-173) |
| `--max-cpus` | — | undeclared | declared CPU ceiling this runner offers (PAR-176) |
| `--max-memory` | — | undeclared | declared memory ceiling (`16GiB`); a larger request is queued with a named reason |
| `--capacity` | `SPINDLE_RUNNER_CAPACITY` | `1` | concurrent jobs (one container, network and volume each) |
| `--labels` | `SPINDLE_RUNNER_LABELS` | `self-hosted,linux,docker` | comma-separated labels the runner declares |
| `--name` | `SPINDLE_RUNNER_NAME` | — | display name recorded at registration |
| `--docker` | `SPINDLE_RUNNER_DOCKER` | `docker` | the docker CLI binary |
| `--image` | `SPINDLE_RUNNER_IMAGE` | `alpine/git:latest` | job image when the workflow names none; must carry `git` |
| `--image-prefix` | `SPINDLE_RUNNER_IMAGE_PREFIX` | off | registry mirror prefix (`host[:port]/path`): every host-less image is rewritten under it (§8.6) |
| `--image-prefix-all` | `SPINDLE_RUNNER_IMAGE_PREFIX_ALL` | off | also rewrite images that name their own registry (their host is dropped); requires `--image-prefix` |
| `--image-pull` | `SPINDLE_RUNNER_IMAGE_PULL` | `missing` | `docker create --pull` policy: `missing` (pull only an absent image, never refresh a tag), `always` (ask the registry every job), `never` (run only local images) |
| `--image-require-digest` | `SPINDLE_RUNNER_IMAGE_REQUIRE_DIGEST` | off | refuse an image without `@sha256:` at plan time (the job fails naming the image; services included) |
| `--allow-host-docker` | `SPINDLE_RUNNER_ALLOW_HOST_DOCKER` | off | acknowledge that `DOCKER_HOST` exposes the host root filesystem |
| `--allow-fork-runs` | `SPINDLE_RUNNER_ALLOW_FORK_RUNS` | off | execute fork pull-request code on this runner (only on a disposable host; §8.7). Refused in combination with ANY container grant (`--container-privileged`, `--docker-host`, `--container-network`, `--container-valid-volume`) |
| `--container-privileged` | `SPINDLE_RUNNER_CONTAINER_PRIVILEGED` | off | grant workflows `--privileged`/`--cap-add` on this runner; never combined with `--allow-fork-runs` (§8.7) |
| `--container-network` | `SPINDLE_RUNNER_CONTAINER_NETWORK` | off (per-job network) | a user-defined docker network jobs, services and docker actions join; `host`, `none` and namespace modes are refused; never combined with `--allow-fork-runs` |
| `--container-valid-volume` | — | off (every volume refused) | host path or `path.Match` glob a workflow `volumes:` source may bind; repeatable; never combined with `--allow-fork-runs` |
| `--docker-host` | `SPINDLE_RUNNER_DOCKER_HOST` | off (no endpoint) | `unix:///absolute/socket` or `tcp://host:port` exposed to the **job container only**; never combined with `--allow-fork-runs` |
| `--registry-auth-file` | `SPINDLE_RUNNER_REGISTRY_AUTH_FILE` | off | 0600 file of `registry username password` lines; the runner logs in with `--password-stdin` into a private temporary `DOCKER_CONFIG` (§8.6, PAR-078) |
| `--fetch-timeout` | — | `20` | long-poll seconds per fetch (clamped 1–30) |

The container knobs are the operator's, never the workflow's: a job cannot choose the network, the
valid-volume list or the docker endpoint, and **every** grant — `--container-privileged`,
`--docker-host`, `--container-network`, `--container-valid-volume` — is refused at startup when
combined with `--allow-fork-runs` (an untrusted fork must never reach a runner that widened its own
isolation). The runner validates every knob at startup and re-applies the resulting policy at plan
time, before any container, network or volume exists.

The four image flags are validated at startup: a prefix with a scheme or a trailing `/`, a
`--image-prefix-all` without `--image-prefix`, or an unknown `--image-pull` value exits 1 naming the
flag (and its environment variable), before the runner dials the server. The image flags only change
the plan and the docker argv, so they need no daemon to test. A runner that rewrites images is
covered in §8.6; a runner's own `--image` is the operator's declaration and is not subject to the
server's `threads.image_allow_prefixes` (§8.6 explains why).

Token prefixes are meaningful: `wrg_` registration (one-time, 15 minutes), `wrt_` runner
(long-lived, revocable), `wjt_` attempt (minted per job attempt, short-lived). A registered runner is
`pending` until it exchanges its registration token, `active` once it declares and fetches, and
`revoked` after `/admin/runners` revokes it — which is a real `401` on its next call.

There is deliberately **no repository-policy flag**: which repositories a runner may be offered is the
host administrator's decision at the mint and on the fleet row thereafter (§8.2), never a runner-side
setting.

### 8.2 What it does

On start the runner declares its version, labels and capacity, then keeps `--capacity` fetch loops in
flight. Each loop long-polls `fetch` (up to `--fetch-timeout` seconds); the claim is **global** — the
server's queue hint names the repositories with queued work, and the per-repository queue-entry
compare-and-swap remains the only authority — then it executes the job, reports the conclusion, and
repeats. A stale hint row is skipped and the reconciler repairs it; a job is never lost because a
hint write failed.
Fetching *is* the heartbeat. Errors back off exponentially (1 s → 30 s); an instant empty answer
sleeps 250 ms so a proxy cannot turn the poll into a tight loop; a rejected credential (`401`, e.g. a
revoked runner) stops the process with exit 1.

Labels are **exact, comma-separated strings**, declared at registration/declare
(`--labels self-hosted,linux,docker` is the default set; there are no wildcards and no regexes) and
matched as a **subset**: the runner must declare every label the job's `runs-on` names,
case-insensitive. `runs-on: [self-hosted, linux, docker]` is claimed only by a runner that has all
three; `runs-on: linux` is claimed by any runner carrying `linux`.

**Hosted labels map onto the spindle set** (PAR-170), so a corpus workflow written for Forgejo or
GitHub schedules without edits:

| Workflow writes | The queue matches on |
|---|---|
| `ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, `ubuntu-20.04` | `self-hosted` + `linux` |

The mapping is a *replacement*, and it only ever ADDS requirements to the runner's declared set: a
job that asks for `ubuntu-latest` still runs only where `self-hosted` and `linux` are both declared,
so an alias can never bypass a runner's capabilities, and a runner that declares only `linux` does
not silently accept it. There are no hosted runner *images* here (D-15); the **image** is the
runner's `--image`, the workflow's `container.image`, the runner's label mapping or — for the stock
labels below — the catalogue default.

**Stock toolchain labels (PAR-177, D-26).** A workflow that names one of these labels and no
`container.image` runs in the upstream image, pinned by manifest digest:

| `runs-on` label | Pinned image | Toolchain |
|---|---|---|
| `ubuntu-latest` | `ubuntu:24.04@sha256:562111b88caa79c71391c0c4d3c2114a7d5c35915c20561fd893cb1e07929e6b` | base Ubuntu 24.04 |
| `node` | `node:22-bookworm@sha256:7c400d78797d8cbe3f6bb21d2227b86af4cd24b0014cc9f62ff989cc16de988e` | Node.js 22 LTS + npm |
| `python` | `python:3.13-bookworm@sha256:ee1a30c36c5cf43df939eb664489fb167e1bbe07cd884ea694054f6aff4dd9d9` | CPython 3.13 + pip |
| `go` | `golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437` | Go 1.25 |
| `rust` | `rust:1-bookworm@sha256:75d26c23c24cc6ea1cdabf0714a6bff8fd7bf3248716043fd7ae86dac7f41b94` | Rust stable + cargo |
| `docker` | `docker:27-cli@sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c` | docker CLI + buildx (daemonless, §4.1a) |

`spindle runner images` prints the table and the exact `--labels` line; `spindle runner images
--label go` resolves one label and **refuses an unknown label by name** (a typo never falls back to
another toolchain). A runner's own `label:docker://…` mapping remains the operator's image for that
label (PAR-171) and still applies; with no mapping, an explicit `container.image` wins over the
stock catalogue. The image resolves when the job is fetched, so a digest refresh needs no workflow
edit. spindle publishes and mirrors none of these
images; an air-gapped instance points `--image-prefix` at its mirror (§8.6).

**Label → image (PAR-171).** A runner may declare Forgejo's `name:backend` spelling:

```text
--labels self-hosted,linux,ubuntu-latest:docker://node:lts
```

The label part (`ubuntu-latest`) is still an ordinary label — the job's `runs-on` must match it,
and the hosted expansion above still applies — and the image part becomes the job's container when
the workflow named **no** `container.image` of its own (a workflow's explicit image always wins).
The server injects the image into that job's payload and echoes the declaration as
`label_images`, so the runner log states what it served. `lxc://…` and `host` are refused at
declare time with a named error (the D-15 non-goals); any other backend spelling is refused too. Deliberately **withheld**: `windows-*`, `macos-*` (the D-15 Windows/macOS non-goal — there is
no spindle platform to map them to) and the architecture variants (`ubuntu-24.04-arm` would silently
run an amd64 image). Those labels pass through unmapped and a job naming only one stays queued: the
run page shows its `runs-on`, and the queue shows the unmatched label set (the single named
queue-reason surface is PAR-156).

Do not copy a Forgejo/GitHub hosted-runner label *set* onto `--labels`: a Forgejo runner declares
`docker, ubuntu-latest, linux_amd64`, which overlaps Spindle's `self-hosted, linux, docker` in only
some labels. Declare the spindle set the runner actually provides (`self-hosted`, `linux`, `docker`)
and let the alias table translate `ubuntu-latest`. A job with a label no runner declares stays
queued — the run page shows its `runs-on`, and `/admin/runners` shows the declared sets.

#### Repository tenancy (admin-set, never runner-set)

A runner is offered only the repositories its **policy** allows. The policy is set with the token,
by the host admin, at `/admin/runners`:

- **All repositories** — the instance-wide fleet shape: every repository, private ones included.
- **An allowlist** — up to 50 patterns, each `owner/name` (one repository) or `owner/*` (every
  repository of that owner). A pattern list that resolves to nothing is refused at the mint: a token
  that could never be offered a job is an operator mistake, not a quiet default.

The policy is recorded on the registration token and copied to the registry record at registration;
the fleet table shows it per runner (`all repositories`, the pattern list, or *none — no repository
allowed* for a record written before tenancy existed, which is offered nothing). Enforcement is in
the claim filter, before labels: the global queue hint skips a row whose repository is outside the
policy, the per-repository claim loop refuses the repository, and `claimEntry` re-checks under the
claim CAS — so a stale or hostile hint row can never hand a repository to a runner the admin did not
allow. The filter reads the policy from the registry record on every protocol request, which is why
an admin edit takes effect on the runner's next fetch. A job with no eligible runner stays queued.

A fork pull-request job needs **both**: the repository allowed *and* `--allow-fork-runs` on the
runner. A runner **cannot** widen its policy: `register` and `declare` carry labels, capacity and the
fork opt-in only, and an unknown field (`allow_all`, `repos`) is a `400`; the runner logs the policy
it operates under at startup and that is all it can do with it.

To change a policy after the mint, edit it on the runner's fleet row at `/admin/runners`
("change policy…"): the admin write rewrites only `allow_all`/`repos` on the registry record, is
refused for a revoked runner, and is audited as `runner_policy` (actor, `runner:<id>`, the resulting
policy). The runner does not restart and a job it is already running is untouched — the claim filter
reads the policy from the registry record each time it claims, so the **next** fetch honours the
edit (a fetch already long-polling keeps the record it loaded, at most 30 s — the same window revoke
has). The JSON API mirrors both writes: `POST /api/v1/admin/runners/tokens` with
`{"name": "build-01", "repos": ["acme/mono", "acme/*"]}` or `{"allow_all": true}` (host admin only),
and `POST /api/v1/admin/runners/{id}/policy` with the same `{allow_all, repos}` body.

Tenancy decides **which** repositories reach a host. It is not isolation (§8.5, §8.7): a runner with
a docker socket is root-equivalent on that host whatever its allowlist says.

### 8.3 Capacity and isolation

- `--capacity 1` is the default and the isolation-preserving shape: one job at a time per daemon.
- `--capacity N` keeps N jobs in flight on one daemon. Those jobs share that daemon's kernel and its
  container network, so they are **not** isolated from each other: it is for **trusted** repositories
  only. Capacity is a runner flag, not a repository form field (the admin fleet shows the value the
  runner declared), and the runner is the thing that must be trusted.
- Job containers do **not** mount the docker socket: the runner process holds it (or the daemon
  `DOCKER_HOST` names) and creates the containers. That is also why a shared daemon stays a shared
  failure domain — a container escape, or a daemon-level bug, is root on the host, and every job on
  that daemon shares the exposure (§8.7).
- For untrusted code, scale **out**: many capacity-1 runners, each with its own `docker:dind` (or its
  own daemon). Jobs cannot see each other. A hundred jobs means a hundred runners; there is no
  server-side ceiling and no central worker counter — every claim is a per-entry CAS, so throughput
  grows linearly and fairness is FIFO by enqueue order within a label set.
- Concurrency groups cap a *run* (not a runner): one running and one pending run per group per repository (§3.1a.1).
- A repository's `concurrency_cap` (§6.11) caps *live runs per repository* (0…500), holding new runs
  with a named reason; an organization's cap (same bound, org owner audited) caps live runs across all
  its repositories. Both are admission heuristics, not locks, and both name the one reason a job
  waits.
- **Larger-runner sizing (PAR-176).** A runner may declare `--max-cpus` / `--max-memory` (recorded at
  register/declare). A job whose `container.options` ask for more is *not offered* to that runner and
  stays queued with the exact request in its reason ("no matching runner offers 4 cpu / 16 GiB").
  Undeclared (zero) means the daemon's own limits, so a fleet on one big host is unchanged. This is a
  scheduling filter, not a second enforcement point: docker still applies the cgroup limit.
- **Isolation is per attempt (PAR-178).** Every job attempt gets its own container, network and
  workspace volume, named deterministically from `(repository, run, job, attempt)`. Two concurrent
  jobs — even on one capacity-N runner — share no namespace and no volume; a retried plan reuses the
  same names so cleanup is identifiable. The Docker daemon remains the isolation boundary: capacity-N
  is one trust domain (trusted repositories), and a runner that grants privilege or a docker endpoint
  refuses `--allow-fork-runs`.
- **The repository policy is tenancy, not isolation.** An allowlist keeps one team's code off
  another team's repositories; it does not contain the job that runs. Treat the *host* as the
  isolation boundary — see the dedicated-worker shape in §8.5.

### 8.3a Registration scopes and runner groups (PAR-172/PAR-175)

The admin-set repository policy **is** the registration scope: `allow_all` is an instance-scoped
runner, an all-`owner/*` list is organization-scoped, and any `owner/name` is repository-scoped
(mixed lists report the narrowest: repository). A spindle owner is a user or an organization
namespace, so Forgejo's "user" scope is the same shape; there is no separate user-scope credential.
The fleet row and `GET /api/v1/admin/runners` show the derived `scope`/`scope_detail`.

A **runner group** bundles inherited labels with a visibility allowlist:

    POST /api/v1/admin/runner-groups   {"name":"gpu-boxes","labels":["gpu","cuda"],"repos":["acme/*"]}
    POST /api/v1/admin/runners/{id}/group   {"group":"gpu-boxes"}

Group visibility is **intersected** with the runner's own policy — a group can only narrow what a
host may serve, never widen it — and its labels are unioned with the runner's declared labels for
`runs-on` matching. Both are materialized onto the registry record when the group changes, so the
claim filter still reads one record (no extra GET on the hot path). Group create/update/delete,
assignment and every membership change is audited.

### 8.3b One-job workers and the fleet (PAR-173/PAR-181)

`--ephemeral` makes a one-job worker: the process exits after one completed job and the server
deletes its registry record, credential and token record (audited `runner_ephemeral_retire`), so a
disposable worker cannot be replayed. Capacity must be 1. This is the documented scale-out shape
(the managed pool is a non-goal, D-14).

The fleet verbs are available with the runner's **own** credential (no admin session needed) and,
for an administrator, at `/admin/runners` and the JSON admin lane:

| Verb | Runner CLI | Effect |
|---|---|---|
| drain | `spindle runner drain --token-file F` | finish the current job, claim nothing new |
| resume | `spindle runner resume --token-file F` | undo the drain |
| rotate | `spindle runner rotate --token-file F` | new credential with a 15-minute overlap; the old one is a real 401 after it |
| deregister | `spindle runner deregister --token-file F` | delete the record and credential (refused mid-job) |
| revoke | `/admin/runners` (host admin) | kill the credential immediately (a real 401) |

Rotation is shown once (the page renders it in place; the CLI writes it to the token file), never in
a URL or the log. Admin-side verbs are `POST /api/v1/admin/runners/{id}/{drain|resume|rotate-token|deregister}`.

### 8.4 Stop, cancel, cleanup

`SIGTERM` (and `SIGINT`) cancels the in-flight jobs: each job's context dies, its containers are
removed, and the attempt is reported `cancelled` on a fresh context so the conclusion is not lost.
Cleanup runs on **every** path — success, failure, setup error, cancel, step/job timeout, runner
panic — with a fresh 90-second budget: `docker rm -f` on the job container and every service,
`docker network rm`, `docker volume rm -f`. Names are a hash of (repository, run, job, attempt), so a
leftover from a killed runner is identifiable and removable by name.

Every background step (PAR-080) is part of that budget: a cancel or SIGTERM tears its process group
down immediately, and the job-end pass joins or kills every remaining background **before** the
containers are removed. A process group that survives the SIGKILL is reported as a job failure (its
id and the kill output in the log), so a capacity-N runner cannot silently hand a leaked process to
the next job.

### 8.5 Running the runner in a container

The published spindle image **is** the runner image: the server and the runner are the same binary,
and the image ships the `docker` CLI (the CI container contract asserts `docker --version`), so
`runner serve` works inside it as-is. There is no compose service for a runner; the container is
started with `docker run` beside the daemon it drives (§2.2 has the copy-paste form). The shapes are:

- run the binary on a host that has the `docker` CLI and a daemon (the default);
- run the published image as a container: mount the host socket, grant its numeric GID with
  `docker run --group-add` (the image runs as the nonroot uid 1000), mount the token file readable by
  that uid, and keep `--allow-host-docker`;
- for untrusted code, point `DOCKER_HOST` at a `docker:dind` daemon (or a sidecar) instead and leave
  `--allow-host-docker` off.

A custom image is only needed when you want a different docker client or base than the published one:
the shipped image already has what the runner shells out to.

Do not mount the host socket into a runner serving untrusted repositories (§8.7).

#### The dedicated-worker shape (the safe topology for untrusted code)

**The rule: a runner host's compromise must not reach the git host.** The spindle server, its object
store and the runner fleet are separate trust domains; the only thing that crosses is what the
protocol needs.

```
   git host (spindle server + bucket)          runner host (dedicated VM / disposable box)
   ┌──────────────────────────────┐            ┌────────────────────────────────────────────┐
   │ /admin/runners mint →  wrg_  │            │ spindle runner serve  ── DOCKER_HOST ──▶  │
   │ claim/lease over HTTPS  ◀────┼────────────┼── fetch (runner token)                     │
   │ attempt token (read scope)   │            │      daemon (rootless or dind) → job ctrs  │
   └──────────────────────────────┘            └────────────────────────────────────────────┘
        no socket, no bucket creds                 no repository credentials on disk
```

- **One host per trust class, and not the git host.** A host that runs untrusted code is assumed
  compromisable, so it holds no store credentials, no admin session and no repository secrets: the
  job's only authority is the per-attempt token the server issued (read scope, one repository, one
  attempt), and secrets are delivered at fetch only for a trusted, approved run.
- **Egress is the protocol and the registries.** The runner needs the spindle origin, the checkout
  origin (often the same), and the image registry/mirror — nothing else. Deny everything else at the
  network edge; a job that can reach your internal network is a pivot, not a job.
- **Run the jobs on a daemon you control on that host**, not a socket on the git host:
  - a **local daemon** (default `DOCKER_HOST`): the jobs are siblings on that host's daemon. Pass
    `--allow-host-docker` **only** when that daemon is that host's root socket — the flag is the
    acknowledgement that the daemon exposes the host root filesystem.
  - a **remote daemon over TLS**: `DOCKER_HOST=tcp://ci-daemon.internal:2376`,
    `DOCKER_TLS_VERIFY=1` and the client certs (`DOCKER_CERT_PATH`). TLS is mandatory — the Docker
    API itself has no authentication. `--allow-host-docker` only guards the **default** socket (the
    runner refuses to start on `unix:///var/run/docker.sock` without it), so it is not consulted for
    a remote `DOCKER_HOST`: any non-default daemon you name is your declaration, and a rootful remote
    daemon is exactly as powerful as a local root socket. Treat it as root-equivalent either way; a
    daemon on a *shared* host shares that host's failure domain (§8.3), so keep it per-trust-class.
  - a **per-runner `docker:dind` sidecar** (one of the shapes above): the runner dials the dind
    daemon, so a container escape lands in the dind container, not the runner host. This is the
    cheapest step up from the host socket; it still needs a privileged dind container (or a rootless
    one) and one dind per runner.
- **Harden the daemon itself** (defence in depth, not a substitute for a dedicated host):
  - **rootless dockerd** (`dockerd-rootless-setuptool.sh`, socket under
    `$XDG_RUNTIME_DIR/docker.sock`): namespaces the whole daemon under an unprivileged uid, so a
    daemon-level escape lands as that user, not root. The runner must run as the same uid that owns
    the socket, and cgroup-v2 + the kernel requirements apply. `--allow-host-docker` is not needed
    because the socket is not the host root.
  - **sysbox** (`sysbox-runc` as the container runtime): presents a stronger, VM-like container
    (its own userns/pid/cgroup view) and supports running a dind inside it without `--privileged`.
  - **gVisor** (`runsc`): a user-space kernel between the container and the host — the strongest
    cheap mitigation for kernel-attack surface; some syscalls/workloads (and nested docker) do not
    work under it.
  - **Kata Containers**: a microVM per container, the heaviest and most isolated option; slower
    starts, needs hardware virtualisation.
  - Whichever you pick, keep `--capacity 1` per daemon for untrusted code and **never share one
    daemon between trusted and fork runners**.
- **The allowlist is tenancy, not isolation.** The minted repository policy (§8.2) decides which
  repositories reach a host; it says nothing about what a job can do once it is running. A runner
  with a daemon socket is **root-equivalent on its host**, and the host is the thing that must be
  disposable. A compromised runner sees the code of every repository its policy allows and the
  attempt tokens it was handed — size the policy to the blast radius you accept, narrow it with one
  audited edit when the risk grows, and revoke the runner when the host itself is suspect.

### 8.6 Mirroring job images

A job image is code: whatever bytes the tag resolves to run as the workflow, with the repository's
secrets in the environment. That makes an image source a **code-execution trust root, exactly like
Docker Hub**. Pointing CI at a mirror (an OCI registry proxy such as Artifact Keeper) buys three
real things — rate-limit insulation, cold-start cache, one pull identity — but it does not move the
trust boundary: the mirror decides which bytes become the container. Treat it accordingly: pull
credentials read-only, pushes to the image repository only from an operator/trusted pipeline, and a
digest pin wherever you can afford one.

**The instance allowlist names upstream references, not the mirror.** `threads.image_allow_prefixes`
and `threads.image_require_digest` are checked at run creation against the reference the workflow
authored — before `--image-prefix` rewrites anything in the runner. A workflow that writes
`golang:1.27` or `ghcr.io/acme/x` needs that upstream spelling allowed; adding a mirror prefix to the
list does not allow it for you.

**The pull identity is the runner's (PAR-078).** A private image authenticates with the runner's
credentials, never a workflow `credentials:` block (refused by name — it would put the password in
the job payload and docker argv). Three options, in order of preference: the daemon's existing
credential store; `--image-prefix` with a pulling mirror; or `--registry-auth-file`, a 0600 file of
`registry username password` lines. At startup the runner logs in once per registry with
`docker login --username … --password-stdin` (the password on stdin, never argv) into a private
temporary `DOCKER_CONFIG` that every later docker call uses, adds each password to every job's mask
list, and removes the directory when it stops. A credential file readable by group/other is refused
at startup.

#### L0 — a daemon-level registry mirror (no spindle change)

On every runner host, `/etc/docker/daemon.json` (a daemon restart is required):

```json
{ "registry-mirrors": ["https://ak.example"] }
```

On the mirror, create a **Remote/Docker** repository (one repository key = one upstream; for Docker
Hub, key `dockerhub`):

- `upstream_url: https://registry-1.docker.io`
- `oci_trusted_bearer_realms: ["https://auth.docker.io"]` when a Hub credential is configured.
  Without it the mirror's token request is anonymous and a private/rate-limited upstream fails
  (upstream advisory GHSA-78h6-3wp8-2542; verified in Artifact Keeper's `oci-trusted-bearer-realms`
  doc).
- visibility `public`: job images are not secrets, and an anonymous pull path removes an expiry
  failure mode. Do **not** make a repository that holds private base images public.
- `AK_DEFAULT_DOCKER_MIRROR_REPO=dockerhub` (or the equivalent setting for your version): this is
  what makes dockerd's `registry-mirrors` config resolve — a pull of `/v2/library/golang/…` that
  matches no repository key falls back to that proxy repository.

Then a workflow keeps writing `golang:1.27` and never knows: every Docker Hub pull is served by the
mirror, cached, and rate-limit insulated. Prove it once on a clean host: pull with the daemon, then
check the mirror's access log or that `ak_proxy_cache_lookups_total{repository="dockerhub"}` moved.

A few limits are worth stating plainly:

- **L0 covers Docker Hub only.** `dockerd` consults `registry-mirrors` for Docker Hub; a workflow
  naming `ghcr.io/owner/img` still goes direct. That is L1's job below.
- **A mirror's mutable-tag cache has a TTL** (Artifact Keeper's default is 300 s for a tag), and
  `docker create` without `--pull` never refreshes a tag the daemon already has. "The tag moved"
  therefore reaches CI late, or not at all — digest pinning is the only fix.
- **Plain HTTP on a LAN** needs `"insecure-registries": ["ak.example:5000"]` beside
  `registry-mirrors` in the same file; prefer TLS. A plain-HTTP registry that is only in
  `registry-mirrors` fails with `http: server gave HTTP response to HTTPS client`.

#### L1 — the runner's image prefix (one pull identity)

`--image-prefix ak.example/dockerhub` makes the runner rewrite a job's image references under the
mirror: the host-less (Docker Hub) ones always, and with `--image-prefix-all` the ones that name
their own registry too. The rule, applied to `container.image`, `services.<id>.image` and the
runner's own `--image`:

| Authored reference | `--image-prefix ak.example/dockerhub` | with `--image-prefix-all` |
|---|---|---|
| `golang:1.27` | `ak.example/dockerhub/golang:1.27` | same |
| `alpine/git:latest` | `ak.example/dockerhub/alpine/git:latest` | same |
| `docker.io/library/golang:1.27` | unchanged | `ak.example/dockerhub/library/golang:1.27` |
| `ghcr.io/a/b:t` | unchanged | `ak.example/<your-ghcr-mirror>/a/b:t` |
| `localhost:5000/a/b@sha256:…` | unchanged | `<prefix>/a/b@sha256:…` |

- A reference **has a registry** when its first path segment contains `.` or `:`, or is `localhost`
  (docker's own rule). No registry and a prefix set: it is a Docker Hub reference and is rewritten
  under the prefix.
- A reference that names a registry is left alone unless `--image-prefix-all` is set: the flag makes
  the runner **replace the registry host** with the prefix (the original host is not carried in the
  path). One mirror repository per upstream registry is then what the prefix names — run different
  runners with different prefixes, or pick one prefix per upstream pool.
- **Docker Hub's implicit namespace is not rewritten.** `golang:1.27` becomes
  `<prefix>/golang:1.27` — not `<prefix>/library/golang:1.27`. The mirror's Docker Hub proxy
  applies the same `library/` normalisation docker itself does (`dockerhub/golang` → upstream
  `library/golang`), and inserting it here would break a proxy repository configured for the
  single-segment spelling. `docker.io/library/golang` names a registry, so it stays as written
  unless `--image-prefix-all` is set.
- `--image-pull` maps to `docker create --pull=<value>` on the job **and** every service container
  (the runner always passes it explicitly, so docker's default and the operator's choice are one
  observable argv). `missing` is today's behaviour; `always` is the only way a moved tag reaches CI
  and costs a manifest round trip per job; `never` makes a run reproducible without digests and
  turns a mirror outage into an immediate failure for a cold image. All three need a docker CLI whose
  `docker create` lists `--pull` (check with `docker create --help`; modern 23+ CLIs do).
- `--image-require-digest` refuses at plan time an image without `@sha256:<64 hex>` — the job fails
  naming the image and this flag, before any container is created. The instance-wide equivalent is
  `threads.image_require_digest` (§3.3 and the Configuration page); the runner flag is the
  per-runner rule, the config key is the server-side rule for every runner attached to the
  instance. They compose: either one refuses, and neither changes the other's decision — a job the
  server refuses never reaches a runner, and a runner with the flag refuses a job the server allowed
  (it fails at plan time, so an operator sees the runner's flag in the reason).
- The digest rule and the pull policy are **independent**: a digest is immutable, so
  `--image-pull=missing` is already reproducible and `--image-require-digest` never rewrites
  `--image-pull`. `--image-pull=never` plus digest pinning is the offline-audit shape; `always` is
  for tag freshness, where digests are not in play.

Credentials never travel in argv. A mirror that needs authentication uses a pull-only, repo-scoped
token written into a 0600 `docker config.json` and pointed at by `DOCKER_CONFIG` in the runner's
environment (a systemd `Environment=` line or a container env var). The docker CLI reads it; the
job container never receives it (an image pull happens in the daemon, before the container exists),
and `docker login -p` in a job step is wrong twice — it puts the secret in argv and in the wrong
identity.

**Pin the runner's own `--image` by digest** in its unit or container definition — e.g.
`--image ak.example/dockerhub/alpine/git@sha256:…`. That image runs every job that names no
`container:`, it is the operator's declaration rather than workflow input, and no server-side rule
governs it; a mutable tag there silently changes every such job.

#### When a pull fails

`docker create` exits 125 and the job log keeps docker's own text. The runner then adds a named
diagnostic (the same shape as the checkout reachability diagnostic in §4.4) carrying the reference
actually attempted, the mirror prefix that rewrote it, whether a fallback exists (with a prefix
there is **none**: failing closed is the point), the pull policy, and the fix for three classes:

- **unreachable** (name, connection, TLS): the pull happens in the **daemon**, so the daemon must
  resolve and reach the mirror; a plain-HTTP registry belongs in `insecure-registries`.
- **401 / credential refused**: the daemon's `$DOCKER_CONFIG` file needs a pull-only token for the
  mirror host. A missing repository path produces the same docker text, so the diagnostic says so.
- **404 / manifest unknown**: the mirror has no such repository/image — check the prefix's
  repository key (one per upstream when `--image-prefix-all` is on) and the tag/digest.

To verify the rewrite itself without a workflow:
`docker pull ak.example/dockerhub/golang:1.27` on the runner host must resolve (this single-segment
case is the one that breaks every workflow if the mirror normalises differently, so check it first).

### 8.7 Trust boundary, container options and fork runs

#### The security boundary

CI executes code. The boundary, in one place:

- **Only admin-registered runners exist.** A host administrator mints the registration token at
  `/admin/runners`; there is no self-registration, no per-repository runner, and no anonymous
  runner. That mint is the whole authorization to execute instance jobs, and it carries the
  runner's repository policy (§8.2).
- **The repository policy is tenancy, not isolation.** The allowlist decides which repositories a
  runner is offered; it does not contain the job that runs. The runner never sets or widens it
  (register/declare carry no policy field), a legacy record with no policy is offered nothing, and an
  administrator changes it with an audited edit on the fleet row. What a leaked runner token or a
  compromised runner can reach is bounded by that policy — not by the container (§8.5's
  dedicated-worker shape).
- **A runner with the docker socket is root-equivalent infrastructure.** Job containers are
  **siblings** on the node daemon, not children of the runner: the *runner* holds the socket (job
  containers do not mount it), and the daemon can mount the host filesystem. Whoever controls a job
  container's `docker create` argv controls the host. `--allow-host-docker` is the explicit
  acknowledgement that `DOCKER_HOST` is that daemon; against the host socket the runner refuses to
  start without it.
- **A runner host is not the git host.** Run the runner (and the daemon it drives) on a dedicated or
  disposable host whose compromise must not reach the server, the object store or another trust
  class's repositories; the runner holds no store credentials and no admin session (§8.5).
- **Fork runs are off by default.** A fork pull request runs a workflow file from the base branch
  (never the fork's), with a read-only checkout and no secrets, but the *step scripts are the fork's
  code* checked out at the PR head. It is offered only to a runner started with `--allow-fork-runs`,
  and that flag belongs on a disposable host; without such a runner the fork job stays queued.
- **`--capacity 1` per daemon, on a dedicated host for untrusted code.** A shared daemon is a shared
  failure domain (§8.3); never run untrusted code beside the git host.
- **The image allowlist and digest rule are hygiene, not isolation.** `threads.image_allow_prefixes`
  and `threads.image_require_digest` decide which bytes may be pulled; an allowed image still runs as
  the job, with its token and secrets (§8.6).

#### Root-equivalent infrastructure

Job containers are **siblings** on the node daemon, not children of the runner, and the daemon can
mount the host filesystem.

**Residual risk: container escape on a shared host.** A shared daemon is a shared failure domain. A
kernel/docker escape (or a daemon-level bug) on a runner that also executes trusted code exposes that
code and its secrets, not merely the fork's container. Fork and trusted jobs therefore belong on
**separate runners and separate hosts**; the isolation card (daemonless/per-job daemons) is the
durable fix and is deliberately not part of this policy.

**Residual risk: a compromised runner host.** The repository policy bounds *which* repositories'
code a host is offered — it does not stop a runner that is already compromised from reading
everything its policy allows, nor the attempt tokens it holds while a job runs. Treat the host, not
the allowlist, as the security boundary: dedicated hosts, one trust class per host, and revoke +
re-mint when a host is retired (§8.5).

#### Container options and mounts

`container.options`, `container.volumes`, `services.<id>.options` and `services.<id>.volumes` are
workflow-controlled argv, so the server and the runner enforce the same allow-list:

| Allowed | Notes |
|---|---|
| `--cpus`, `--memory`/`-m`, `--memory-swap`, `--shm-size` | resource accounting |
| `--ulimit nofile=…` | open-file limit only; other ulimit keys are node knobs |
| `--init`, `--read-only` | in-container confinement |
| `--user`/`-u`, `--workdir`/`-w` | run as a non-root user, working directory |

A container **TTY** (`--tty`/`-t`) is refused by name: steps run with `docker exec` and no terminal,
so a TTY would be an accepted-but-inert surface (use `script -qc '<cmd>' /dev/null` inside the step).

**Runner-granted (off by default, PAR-179/D-8).** The server accepts these shapes at creation so a
granting runner can be offered the job, and the **runner's own policy** decides whether they run:

| Shape | The operator knob that admits it |
|---|---|
| `--privileged`, `--cap-add` | `--container-privileged` |
| a workflow volume (`/srv/cache:/cache[:ro]`) | `--container-valid-volume /srv/cache/*` (matching absolute host source; root, relative paths, named volumes and mount options outside `ro/rw/z/Z/copy/nocopy` are always refused) |
| the job's docker endpoint (never a workflow field) | `--docker-host` |
| a shared network | `--container-network` |

A runner that grants **any** of these — privilege, a docker endpoint, a shared network or a volume
allowlist — **refuses to start with `--allow-fork-runs`**, so an anonymous PR can never reach a
widened runner. `--container-network` selects a user-defined network for jobs, services and docker
actions (`host`, `none`, `bridge` and namespace modes are refused at startup).

Everything else is refused permanently, at both ends: every namespace flag (`--pid`, `--ipc`,
`--network`, `--userns`, `--cgroupns`, `--uts`), `--device`, `--security-opt`, `--group-add`,
`--add-host`, `--publish`, `--entrypoint`, `--tty`, and any mount other than a workflow volume
matched by the runner's allowlist. The host root (`/:/host`), a relative source and a named volume are
never valid. Without `--container-valid-volume`, **every** workflow-supplied volume is refused: the
runner adds its own per-job workspace volume, and a workflow-chosen bind mount is host filesystem
access.

The job container's own `ports` key is not `--publish` in `options`: it is applied by the runner as
an **ephemeral loopback** mapping (`127.0.0.1::<port>`), a host-side port is refused by name, and
therefore a workflow cannot open a fixed port on the runner host (§4.1). A workflow's
`container.credentials` is likewise refused by name (D-23): the registry pull identity is the
operator's `--registry-auth-file` (§8.6).

The check parses both `--opt=value` and `--opt value` with the same splitter the runner builds argv
with (quoting included), refuses a bare argument, and runs in two places:

- **Server, at run creation and rerun** — the same pass as the image policy. A permanent refusal is a
  failed job with a `Reason` naming the field and the offending option (e.g. `container.options
  carries "--device"`), a red check mirror, and no enqueue; a job a condition already skipped is not
  refused.
- **Runner, at plan time** — the runner's own grants are applied here, before any container, network
  or volume is created. A grantable shape on a runner that did not opt in fails with a named
  `spindle:` diagnostic naming the missing flag (the job fails; nothing is started).

A runner's own workspace volume and network are added by the runner after that check, so they are
not workflow-controllable, and the job token/secret delivery is unchanged.


## 9. Operating it

### 9.1 Cancel and rerun

- **Cancel** (`cancel run` on the run page, `POST /{o}/{r}/api/threads/{run}/cancel`, or
  `spindle threads cancel OWNER/REPO RUN`): an
  unclaimed job becomes `cancelled` immediately; a running job is signalled and acknowledges through
  its own log/complete channel (at most one heartbeat interval, ~25 s), then reports `cancelled`.
  The run folds to `cancelled`.
- **Re-run** (`re-run` on the page, `POST /{o}/{r}/api/threads/{run}/rerun`, or
  `spindle threads rerun OWNER/REPO RUN`): refuses while
  any job is running (`409`, "cancel it first"), then resets every job to `queued` at the next
  attempt and re-enqueues. A rerun is a new attempt: the previous attempt's token is rejected by
  identity, and its log frames stay under their own attempt prefix. A job-level `if:` is evaluated
  again, so a job the condition skips stays skipped.
- **Re-run failed jobs** (`POST /{o}/{r}/api/threads/{run}/rerun-failed`): GitHub's subset — the
  run's attempt advances, and only the **failed jobs plus their transitive dependents** get a new
  attempt. A successful job that is not downstream of a failure keeps its state, its attempt, its
  step results and its green check untouched: nothing rewrites it. A dependent is re-run even if it
  previously succeeded, because its input is about to change. A run with no failed job answers
  `409` naming that fact (use the full re-run). The image, container-option and environment gates
  are re-applied to every re-run job, exactly like a whole-run re-run; a gated deployment is held
  for a fresh approval.
- `spindle threads list|show|run|cancel|rerun` are all served by the API. The verb's answer is the
  run header/state only — poll the run (`spindle threads show`) for per-job state; cancel is
  acknowledged by the runner, not by the call.
- **Why a run is `cancelled`** is on its header: `cancel_reason` is `operator` (with `cancelled_by`
  naming the maintainer), `supersede` (with `superseded_by` naming the newer run — §3.1a) or
  `concurrency` (a group's `cancel-in-progress` took the group). A run that ended on its own
  carries neither, and a second cancel never rewrites the first reason: the path is idempotent.
  The run page shows the same fact on the header and, for a superseded run's queued jobs, as each
  job's reason.

### 9.1a Schedules, pause, priority and notifications (PAR-154/155/157/197)

**Schedules.** The Threads page lists every cron entry of the default branch's files with its next
fire, its last-fire watermark and any coalesced missed ticks. Each row has *disable/enable* (no
commit, audited) and *run now* (a manual trigger, allowed on a disabled entry, audited):
`GET/POST …/api/threads/schedules…` and `spindle threads schedule list|enable|disable|run`. A
disabled entry keeps its watermark; re-enabling fires on the next due tick. The sweeper fires at
most one run per entry per pass, so an outage coalesces to one run and reports the count — a backlog
is never replayed.

**CI pause.** `POST …/api/threads/pause {"paused":true}` (or the page toggle / `spindle threads
pause`) stops the repository claiming jobs; in-flight jobs finish, queued jobs wait with the reason
"CI is paused for this repository", and `spindle threads resume` restores claims immediately. Every
flip is audited as `repo_ci_pause` (maintain+).

**Priority.** A manual dispatch may carry `priority` 0…10 (`spindle threads run --priority 5`).
Claim order is priority descending, then FIFO by enqueue sequence, across repositories and inside
one repository. Priority never bypasses an approval hold, a pause, a concurrency cap or a trust
gate. `GET …/api/threads/queue` (and `spindle threads queue`) shows the live order.

**Notifications.** A signed-in user can *watch* a repository (page toggle; `PUT
…/api/threads/subscription`). A failed run writes exactly one in-app notification per (user, run)
with its link, and the optional `notify_webhook` repository switch receives one POST with the same
facts. The inbox is `GET …/api/threads/notifications` and the Threads page panel; *mark all read*
is a POST.

**Email (D-27).** A third channel sends the same failure notice to a subscriber's verified address.
It is **off by default**: set the `threads.smtp.*` keys (`host`, `from`, and optionally `port`,
`username`/`password`, `tls`, `timeout`) to enable it. Delivery uses the standard library
(`net/smtp` + `crypto/tls`), has the same failure-only event filter as the inbox, and is sent only
when the inbox record is first created — the record key is the dedupe, so one run produces at most
one email per subscriber. The message carries the run link and is masked with the run's job mask
values (the same rule as the log); if the mask list cannot be resolved, email is skipped rather than
sent unmasked. `tls` is `starttls` (default), `implicit`, or `none` for a trusted loopback relay;
credentials are refused over `none`. A delivery failure is logged and never retried into a loop.
The key bounds are in `docs/go/11_config_cli.md`.

### 9.2 Retention

Retention has **two windows** (PAR-158), both operator knobs with validated bounds:

- `threads.retention` (default **720h = 30 days**, valid 24h…8760h) deletes a run once it is
  **terminal** and its completion (or creation, if it never started) is older than the window.
  Jobs, log frames and queue entries of that run go with it; checks do not (§7). A queued, running
  or `pending_approval` run is never touched, whatever its age — and neither is a run with a job
  whose lease is still live, so a stale fold can never delete the log a runner is writing.
- `threads.artifact_retention` (default: the run window, same bounds) is the artifact's own
  lifetime: created + its upload `retention-days` when declared, else created + this window. A
  longer artifact window lets an artifact **outlive its run**: deleting the run keeps the manifest
  and blobs, and the expiry row reclaims them at their own time. A `retention-days` value is
  therefore no longer bounded by the run's window.
- `threads.cache_retention` (default **168h = 7 days**, idle) is the cache entry's own window: an
  entry older than this is reclaimed by the sweeper, together with the repository budget
  (`threads.cache_max_bytes`) and orphaned payloads. A merged PR's scopes are discarded at merge.
  `keep_forever` does not keep caches alive: a cache is derived data, not history.
- `threads.keep_forever = true` reclaims neither runs/logs nor artifacts (dead runner records are
  still pruned: infrastructure, not history). It is the unbounded spelling; there is no "very
  large window" convention any more.
- A 90-day setting is an ordinary value, reported on the admin GC page with the effective windows.

```toml
[threads]
retention = "720h"
artifact_retention = "2160h"   # 90 days: artifacts outlive their runs
cache_max_bytes = "5GiB"       # per-repository cache budget (eviction target)
cache_max_entry_bytes = "1GiB" # one cache entry
cache_retention = "168h"       # 7 days idle
cache_max_entries = 256
keep_forever = false
max_queue_attempts = 3
job_lease = "15m"
approve_first_time_forks = true
default_permissions = "read"
```

Environment overrides are exact paths: `SPINDLE__THREADS__RETENTION`,
`SPINDLE__THREADS__ARTIFACT_RETENTION`, `SPINDLE__THREADS__KEEP_FOREVER`,
`SPINDLE__THREADS__MAX_QUEUE_ATTEMPTS`, `SPINDLE__THREADS__JOB_LEASE`,
`SPINDLE__THREADS__APPROVE_FIRST_TIME_FORKS`, `SPINDLE__THREADS__DEFAULT_PERMISSIONS`,
`SPINDLE__THREADS__CACHE_MAX_BYTES`, `SPINDLE__THREADS__CACHE_MAX_ENTRY_BYTES`,
`SPINDLE__THREADS__CACHE_RETENTION`, `SPINDLE__THREADS__CACHE_MAX_ENTRIES`.

**GC report and dry run (PAR-159).** One retention pass reports bytes reclaimed per family — run,
job, log, artifact, cache, queue entry and runner record — and a **dry run** walks exactly the same
objects and reports the same counts and bytes with nothing deleted. The safety rule is pinned: a
run that is live (queued/running/pending approval) or *attached* (a job lease in the future) is
never reclaimed; a restart repeats a pass rather than skipping one.

### 9.3 Maintainer tasks

| Task kind | Cadence | Body |
|---|---|---|
| `threads-gc` (queue half) | every 5 minutes per repository | requeues claims whose lease lapsed; fails a job that has exhausted 3 attempts; trims retired entries from the dispatch head |
| `threads-gc` (retention half) | at most once an hour per repository | the two windows above; deletes a terminal, unattached run (jobs, logs, queue entries, due artifacts) and reclaims artifacts whose OWN window closed through one expiry-index LIST (an artifact inside its window survives its run); deletes revoked instance runners past the window plus expired unused registrations; reports bytes per family; at most 200 runs and 200 runner records per pass (the runner family is swept at most hourly per process) |
| `threads-queue-reconcile` | every 5 minutes per repository | rebuilds that repository's rows in the global queue hint from its authoritative queue entries (the only place the index LISTs; a lost enqueue write is repaired here) |
| `threads-schedule` | minute-aligned, once per minute per repository | evaluates `schedule:` cron against the watermark (§6.4) |

They run in a process whose roles include `maintain` (`server.roles` empty = all roles). Each pass is a
task with progress packets and a one-line summary notice
(`requeued=… runs=… jobs=… logs=… artifacts=… caches=… queue=… runners=… credentials=… freed=…B (run=… cache=… …)`); read it from
`GET /{o}/{r}/api/tasks` (running + recent), or attach to one with
`GET /{o}/{r}/api/tasks/{id}` and `Accept: text/event-stream`. Passes are idempotent: a second pass
over an unchanged repository deletes nothing, and two instances may pass concurrently.

A claim lease is `max(job timeout + 5m, 15m)` capped at 30m; log frames renew it (coalesced to at
most one write a minute), and a 25-second idle heartbeat keeps a silent step cancellable. After
**3** queue attempts a job fails instead of cycling.

### 9.5 Is the image mirror working?

The runner cannot see a mirror's cache hit or miss and must not guess it. The signal lives on the
mirror's `/metrics` (Artifact Keeper, version-pinned field names — re-check after an upgrade):

```promql
# cache hit ratio per mirror repository (higher is warmer; a cold repo trends up)
sum by (repository) (rate(ak_proxy_cache_lookups_total{result="hit"}[15m]))
  / sum by (repository) (rate(ak_proxy_cache_lookups_total[15m]))

# failed cache publishes (a persistent non-zero rate means the mirror cannot store what it proxies)
sum by (repository) (rate(ak_proxy_cache_publish_failures_total[15m]))
```

For the operator's own latency number, an HTTP load generator measures the wrong thing: the quantity
is `docker create --pull=always` wall time on a cold image. Measure it with
`docker image rm -f <ref>` → timed `docker create --pull=always <ref> /bin/true` → `docker rm -f`,
repeated with the mirror and without it, recording seconds and `docker image inspect --format
'{{.Size}}'` bytes. Any claim beyond "it is cached" belongs in `docs/EVIDENCE.md` with that harness,
not in a doc sentence.

### 9.4 Where the objects are

High level only — exact keys are in the design record (§2):

```
repos/<o>/<r>/threads/runs/<run>.json                        run header
repos/<o>/<r>/threads/runs/<run>/jobs/<job>.json             job record (+ spec)
repos/<o>/<r>/threads/runs/<run>/jobs/<job>/log/<a>-<seq>    log frames (immutable)
repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/manifest.json  artifact manifest (Create-only)
repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/blobs/<a>-<i>  artifact file blobs (immutable)
repos/<o>/<r>/threads/artifact-expiry/<ts>-<run>-<name>.json short-retention expiry index
repos/<o>/<r>/threads/caches/index.json                      cache index (CAS: entries, refcounts, budget)
repos/<o>/<r>/threads/caches/blobs/<sha256>                  content-addressed cache payloads (immutable)
repos/<o>/<r>/threads/queue/<seq>.json                       queue entries + head.json
threads/runners/<id>.json                                    instance runner registry (+ capacity, current job)
threads/runners/tokens/<id>.json                             one-time registration tokens (hashes)
threads/runners/credentials/<id>.json                        runner credentials (hashes)
threads/queue/index.json                                     global dispatch hint (CAS'd, bounded at 10k)
repos/<o>/<r>/threads/secrets/<name>.json                    sealed secret envelopes
repos/<o>/<r>/threads/schedule.json                          cron watermark
repos/<o>/<r>/threads/settings.json                          per-repository switches (PAR-015/118/153)
```

Nothing here is on local disk; deleting every instance loses warmth, not state.

## 10. Troubleshooting

| Symptom | Cause | What to do |
|---|---|---|
| Run stays `queued`, every job says `not started` | read the job's one `queue_reason`: no runner is fetching; labels don't match (labels are exact and matched as a subset; `ubuntu-latest` is mapped to `self-hosted` + `linux`, a `windows-*`/`macos-*` label never maps — §8.2); the run is a fork PR and no active runner has `--allow-fork-runs` (§8.7); all capacity slots busy; the repository `concurrency_cap` hold; a `needs:` dependency is live; a concurrency group is held by another run; an approval hold | the chip names the one fact to act on; `/admin/runners` shows state, labels, capacity and last-seen; the run page shows `runs-on` per job |
| A step fails naming `$GITHUB_ENV` / `$GITHUB_PATH` | the file line is not `NAME=VALUE` / a directory (a heredoc belongs to `$GITHUB_OUTPUT`), a protected name was refused (`PATH`, `LD_PRELOAD`, `GITHUB_*`, …), or a cap was hit (§3.10) | the error names the line and the variable; use `$GITHUB_PATH` for `PATH`, and a run step's own `env:` for values that must be local to it |
| No run at all after a push | no workflow file at that commit; `on:` doesn't match the event/ref (the glob for that ref kind, or a tags-only trigger on a branch push); the file failed to parse; the ref was a delete or a notes ref; the push was made by a job token | the Threads page's **Workflows** section lists files at the default tip and their parse errors; deletes and notes refs create no run and job-token pushes are suppressed |
| A workflow file is rejected | an unknown key, a bad type, a cron outside the subset, a step with both `run` and `uses`, a `needs:` naming a missing job, a `${{ }}` in a structural field (`runs-on`, a container/service image, an `environment`) or in a workflow/job/container/service `env` value, an unsupported `${{ }}` reference or `if:` function, a matrix over 256 cells, a file over 1 MiB | the error names the file and the offending key, anchored `#workflow-<slug>` on the Threads page and in `GET …/api/threads/workflows`; other files keep running — only this one creates no runs |
| `defaults:` is `UNTRANSLATED` after import | the value is not a literal (a `${{ }}` span) or names a key outside `run.shell`/`run.working-directory` — the engine's shape is deliberately literal (§3.1/§3.3) | make the value literal (a step's own key may substitute), or set `shell`/`working-directory` per step |
| A workflow under `.forgejo/workflows/` runs nothing | discovery is `.spindle/workflows/` only; a foreign directory is shown with the importer command | convert it (`spindle threads import --from forgejo`), review, commit on a branch |
| `actions/cache` never hits | `path` is absolute or `~`; the key changes every run; a PR/fork scope cannot read the entry you expect | point the tool at a workspace directory and key on `runner.os` + branch with `restore-keys` (§3.4d) |
| Cross-run artifact download answers `409` | the consumer is a `workflow_run` follow-up whose untrusted upstream is still pending approval | a maintainer approves that run; a fork run or a secrets-bearing job is refused by name (§3.4) |
| A `docker build` step exits 127 | the job container has no docker socket or daemon by default | use kaniko/buildah, or a runner whose operator granted `--docker-host` (§8.7); that runner never takes fork runs |
| Job fails immediately at checkout | the job image has no `git` (or no CA certs); the clone URL is unreachable from the container; the repository is empty at that commit | use an image with `git`, or the default `alpine/git:latest`; check the checkout URL in the log (loopback is rewritten to `host.docker.internal`); make sure the commit exists |
| Push from a job is refused | shallow clone (`fetch-depth` not `0`); the job has `contents: read`; a protected ref refuses a `ci:` principal; non-fast-forward | see the ERR packet / log line: it names the cause. Add `fetch-depth: 0`, declare `permissions: contents: write`, push a fast-forward, or relax the policy |
| Secret is empty in a step | the run is a fork/untrusted run (secrets are never delivered); no `server.secrets_key`/`session_secret` (the API answers `503`); the name doesn't match | the run page shows the `untrusted` chip; Settings → Secrets explains a missing key; names are exact and case-sensitive |
| Run is `pending_approval` | a first-time contributor's fork PR; an environment with reviewers; an untrusted `workflow_run` upstream; a cross-run artifact consumer | a `maintain`+ user approves (or denies) on the run page, which names the hold; or set `threads.approve_first_time_forks = false` |
| A workflow never runs on a schedule | cron outside the subset (that file is a parse error); the schedule is not on the default branch; no process has the `maintain` role; the tick already fired (at most once) | fix the file; merge it to the default branch; run a maintainer; the summary notice counts fired ticks |
| Required check sits at `pending` forever | its job was never claimed (no runner / labels / capacity) | same as the first row: a runner must pick it up, or the merge stays blocked by design |
| `spindle runner serve` exits 1 at startup | revoked or invalid credential (`401`); the daemon is the host socket and `--allow-host-docker` is missing | mint a new registration token and re-register; or point `DOCKER_HOST` at `dind` / pass the flag deliberately |
| Runner is healthy but claims nothing | its labels aren't a superset of `runs-on`, it did not opt in to fork runs and the only queued job is one, or it is revoked/expired | the fleet at `/admin/runners` and compare labels/state; the global queue hint is a hint, so a job still appears once the reconciler (or the next enqueue) writes it |
| Cancel doesn't stop a job instantly | a running job is signalled through its log channel; the runner acks within one heartbeat (~25 s) | wait; the job then reports `cancelled` and its containers are removed |
| Job container can't reach the server | the checkout URL is derived from the runner's request origin; behind a proxy the scheme comes from `X-Forwarded-Proto` | terminate TLS at a proxy that sets `X-Forwarded-Proto`, or point the runner at the external origin |
| `actions/upload-artifact` fails with "no files were found" | the pattern matched nothing in the workspace (a typo, a path outside the checkout, or `if-no-files-found: error`) | the log line names the patterns; check `working-directory` and whether the earlier step actually wrote the files |
| Artifact step fails naming `find` or `tar` | the job image lacks busybox/coreutils (`distroless`, a scratch image) | use an image with `find` and `tar`, or run the archive step as a `run:` script |
| Upload refused with "over the per-file/per-run cap" | the file or the run's artifacts exceed `threads.max_artifact_bytes_per_file` / `_per_run` | shrink the artifact (split it, exclude `node_modules`), or raise the cap in config |
| `downloaded artifact … the manifest says` | a blob's stored size disagrees with its manifest (a truncated upload or a corrupted object) | re-run the job; if it persists the run's artifacts are damaged — `threads-gc` reclaims them with the run |
| A step fails with `did not publish the output` | the referenced step failed, was skipped, ran before the reference, or wrote a different name; an output is only set by a successful step with an `id:` | check `steps.<id>.outcome` and the exact name in the step's output file (`$SPINDLE_OUTPUT`, §3.9); the error names the reference |
| A step fails with `invalid output name` / `never closed` | the output file line is not `name=value` or `name<<DELIM` | fix the line the error names; the delimiter must appear alone on its own line |
| A dependent job's `needs.<job>.outputs.x` is empty | the dependency did not succeed (only a successful job reports outputs), or its `outputs:` template did not include that name | the job record's `outputs` shows what was reported; add the name to the dependency's `outputs:` map |
| A job fails at its end naming `job output …` | its `outputs:` template names an output that was never published | make the producing step write it, or guard the job with `if:` so it does not export on a partial run |
| A job fails with `over the cap of … outputs` | too many outputs, one value over 64 KiB, or more than 1 MiB total | publish less; pass a file as an artifact (§3.4) instead of a huge output |
| Download step can't find the artifact | the name is wrong, the producing job did not run (a failed `needs:` dependency skips its dependents), or `retention-days` closed its window | the run page's **Artifacts** list is the truth; fix the name or the `needs:` edge |
| Log panel says "loading log…" | the log stream needs JavaScript (the `threadlog` island); a 401 in the console means the session expired | reload the page; `GET /{o}/{r}/api/threads/{run}/jobs/{job}/log?accept=json` returns one window without SSE |
| A job failed at creation naming `threads.image_allow_prefixes` | the server refused its `container.image`, a `services.*.image` or a `docker://` image (the reason names which, and the allowlist) | add the registry prefix the image needs to `threads.image_allow_prefixes` (Configuration → `threads`), or change the workflow's image; the check mirror is red by design |
| A job failed naming `threads.image_require_digest` | the image has no `@sha256:` and the instance requires one | pin the digest (`docker buildx imagetools inspect <image>`); this key is independent of the allowlist |
| Job log says "image pull failed/refused/not found" with a `spindle:` diagnostic | the mirror is unreachable, refused the pull credential, or has no such repository; the log states which and the fix | see §8.6: daemon DNS/TLS/`insecure-registries`, `$DOCKER_CONFIG` token, or the mirror repository key |
| Runner exits 1 naming `--image-prefix` / `--image-pull` | the image flag is malformed (scheme, trailing `/`, missing path, unknown pull value) or `--image-prefix-all` has no `--image-prefix` | fix the flag or its `SPINDLE_RUNNER_IMAGE_*` env spelling; the error names both |
| A workflow's image name works on `main` but not in CI | `--image-prefix` rewrote it under the mirror and the mirror has no such repository/image | `docker pull <prefix>/<path>` on the runner host; remember the host-less rewrite does not insert `library/` (§8.6) |

## 11. Not in v1

Plainly, so nobody plans around them:

- **Remote actions are default-deny.** `uses: owner/repo[/path]@ref` resolves (§3.4e), but only
  from this instance (or the one configured source), and a mutable ref outside
  `threads.actions.allow` needs a full 40-hex commit SHA (D-5). There is no implicit marketplace
  discovery, no implicit credential, and no push to a cache or registry from an action's behalf.
  Local actions (`uses: ./<path>`) and `docker://`/Dockerfile actions are supported; a **local**
  composite's metadata still refuses a remote `uses:` by name.
- **No managed runner pool (D-14).** Spindle does not autoscale workers: `spindle runner pool` is a
  named refusal, and a fresh instance runs queued work only after an operator starts a runner.
  Scale out one-job ephemeral or capacity-1 workers under your own supervisor (§8.3b).
- **No hosted Windows/macOS runners, LXC or unisolated host backends, and no artifact
  attestations (D-15).** Each is refused by name: a `windows-*`/`macos-*` `runs-on` queues with a
  reason naming the non-goal; `lxc://`/`host` runner labels are declare-time errors; the
  `attestations` permission has always been a named parse refusal.
- **No marketplace to trust implicitly — but `setup-*` resolves under policy.** `actions/cache` (and
  its `restore`/`save` forms) IS implemented (§3.4d). `setup-node`/`setup-go`/`setup-python` are
  remote actions: when the instance policy admits them, their `cache:` input is rewritten around the
  built-in `actions/cache` pair; when it refuses them, the step fails by name with the exact recipe
  that replaces the cache half (§3.4d.1). An unmapped setup action or cache manager is refused by
  name.
- **Actions.** The built-in set is `actions/checkout`, `actions/upload-artifact`,
  `actions/download-artifact` and `actions/cache` (+ `/restore`, `/save`); local, remote (composite,
  node20/node24, docker) actions are supported (§3.4a/§3.4e). A `setup-node`/`setup-go`/
  `setup-python` step resolves as a remote action and its `cache:` input becomes the documented
  `actions/cache` pair (§3.4d.1); an unmapped setup action/cache manager is refused by name.
- **Cross-run artifacts require an approval** (§3.4, D-3): `download-artifact` may name another run
  of the same repository only when a maintainer approves the consuming run; a fork run and a
  secrets-bearing job are refused by name.
- **Caches are derived data with a trust split** (§3.4d, D-10): PR-scoped writes, no PR-to-PR reads,
  a fork's entry readable by no trusted run, a repository budget with oldest-first eviction.
- **Reusable workflows** (§3.4b) expand the callee's jobs into the run: `jobs.<id>.uses:
  ./.spindle/workflows/x.yml`, or a pinned `owner/repo/.spindle/workflows/x.yml@<40-hex sha>` when the
  callee is publicly readable on this instance. `on.workflow_call` typed inputs, declared secrets and
  output templates are the call's interface and are enforced. Not supported, each a named refusal:
  `secrets: inherit` across a repository boundary (D-6), a mutable remote ref, a matrix caller, and
  environment secrets inherited by a callee.
- **Merge queue (`merge_group`)**: the trigger and run contract (§6.10) and the queue that creates
  the documented `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` refs and lands the merge
  (`docs/features/22_merge_queue.md`) are implemented. `status` (no
  legacy commit-status surface) and `check_suite` (no app check-suite object; individual check runs
  are emitted) are refused by name, as are the surfaces spindle lacks (`registry_package`, `gollum`,
  `page_build`, `branch_protection_rule`, `public`, `discussion`(+comment)). Every event in the
  §3.2 table is emitted; `workflow_call` is a call interface, not an event. `defaults:` and
  `strategy.max-parallel` ARE
  supported by the engine and translated by the importer (§3.1/§3.3); a defaults value with a
  `${{ }}` span or a max-parallel without a matrix is a named refusal.
- **No GitHub expression language** beyond §3.7 (`startsWith`, `endsWith`, `contains`, `format`,
  `join`, `toJSON`, `fromJSON`, `hashFiles`, ordered comparisons, value-returning `&&`/`||`, index
  syntax and the documented `needs.*`/`steps.*` filters are in; `case` is not a function in this
  language and is a parse error). `${{ }}` substitution itself is complete: `github.*` (the closed field set,
  `actor`, `github.action_path` inside a composite and the two server addresses included), `env.*`,
  `secrets.*` (embedded too), `matrix.*`, `inputs.*`, `vars.*`, `runner.*` and `steps`/`needs`
  outputs all resolve in the sites §3.6 lists, in one pass. `runs-on` labels are a substitution
  site (expanded per cell); every other structural field (a container/service image, an
  `environment`) and a workflow/job/container/service `env` value are **not** — only a step `env:`
  is, and a `${{ }}` span there is a parse error. A job-level `if:` may read `secrets.*` and
  `vars.*` (PAR-058; empty secrets on a fork) and does not resolve the two server addresses. Use
  `$GITHUB_*` and `$NAME` in a script as well — they are always present, and they are what the
  shell sees.
- **No per-job resource knobs beyond `container.options`** (`--cpus`, `--memory`, `--shm-size`,
  `--ulimit nofile=…`) and the job container's own `ports` (an ephemeral loopback publish). A
  workflow-declared `entrypoint`/`command` owns the container process, but it cannot widen the
  daemon's own limits or open a fixed host port. Larger-runner sizing is a runner-declared ceiling
  (§8.3, PAR-176), not a hosted size class.
- **Concurrency caps are per repository and per organization** (§6.11): each `concurrency_cap`
  (0…500) holds new runs with one named reason; the organization cap spans every repository it owns.
- **No checks re-run separate from a job re-run**, and no log retention beyond the run's own window.
