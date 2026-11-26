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

- trusted: `push`, same-repository `pull_request`, `workflow_dispatch`, `schedule` — repository
  secrets are delivered and `permissions:` decides the job token's scopes;
- untrusted: a **fork** `pull_request` — the workflow file is read from the base branch, no secrets
  are delivered, the token is always `contents: read`, and a first-time contributor's run waits for
  a maintainer's approval.

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
| `/{owner}/{repo}/threads/{run-id}` | one run: jobs, `runs-on`, runner attribution, check context, durations, step results, the **Artifacts** list (name, size, age, ZIP download), and one live log panel per job |
| the commit page | a **Checks** panel with one row per context, linking to the full checks page |
| `GET /{owner}/{repo}/api/threads` | the same run list as JSON (newest first, cursor `after`, `n` default 50 / max 200) |
| `GET /{owner}/{repo}/api/threads/{run}/artifacts` | the run's artifacts (`name`, `files`, `total_size`, `created_at`, `expires_at`) under the same read gate as the run |
| `GET /{owner}/{repo}/threads/{run}/artifacts/{name}` | the artifact as a ZIP download (`Content-Disposition: attachment; filename="<name>.zip"`, `application/zip`), streamed from the blobs; a reader's gate, never a cross-repository read |

The run id is in the page URL and in `spindle threads list acme/widgets`.

### 2.4 Migrating in a Forgejo or GitHub workflow

Threads only ever runs `.spindle/workflows`; it does not read `.github/workflows` or
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
| `on` | event name, list of names, or mapping | — | a file with no trigger parses but never fires; the event vocabulary is closed (`push`, `pull_request`, `workflow_dispatch`, `schedule`) |
| `env` | map of string → string | empty | merged **under** the job's `env` (job wins); a YAML scalar value (number, bool) is coerced to its text |
| `concurrency` | group name, or `{group, cancel-in-progress}` | none | one in-flight run per group per repository; `cancel-in-progress: true` cancels the holder; the group may use `${{ github.* }}`, `${{ matrix.* }}` (job level) and `${{ inputs.* }}` — see §3.6 |
| `permissions` | mapping, or `read-all` / `write-all` | instance default (`threads.default_permissions`, `read`) | `contents: read\|write` scopes the job token; `id-token: write` mints a job OIDC request token (§5.5, trusted runs only) |
| `jobs` | map of key → job | required | at least one job |

### 3.1a Supersede: a newer push cancels the older run's work

A push to a ref and a PR `synchronize` start work for **one** commit. Without a
rule, two pushes in a row race each other (the classic double deploy). So
Threads does what Forgejo does: **the newer run supersedes the older one**.

- The pair an implicit supersede acts on is **(workflow file, ref)**, and the ref
  is the full name: `refs/heads/<branch>` for a branch push, `refs/tags/<tag>`
  for a tag push (§6.1), `refs/pull/<n>/head` for a PR. So a push on `main` never
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
  Weave/Scriobh files convert with **no refusals** today. A file whose feature
  rows are not green yet must instead fail with the documented refusal (an
  `# UNTRANSLATED:` marker the importer emitted, or a parse error the harness
  declares) — so a gap is named, never inert.

`make parity-closed` adds the plan's §9 closure rule (no `missing` row at all)
and is the check to run when a wave closes, not on every PR. `make parity-exec`
is the opt-in execution tier: it boots a server and a runner and executes a
corpus-shaped workflow in containers (`SPINDLE_PARITY_EXEC=1`, docker required).

### 3.2 `on`

| Trigger | Keys | What fires |
|---|---|---|
| `push` | `branches: [glob, …]`, `tags: [glob, …]` | a branch push (`refs/heads/*`) matching `branches` or a tag push (`refs/tags/*`) matching `tags`; `*` wildcards only. The two filters are independent: `tags`-only does not fire on a branch, `branches`-only does not fire on a tag, and naming neither fires on both kinds. Branch and tag deletes never fire. A merged PR creates a `push` run for the base branch. |
| `pull_request` | `branches: [glob, …]` (target branch), `types: [opened\|synchronize\|reopened]` | the listed lifecycle edges; no `types` = all three. `opened`, `synchronize` (only when the head actually moved) and `reopened` are the only emitted actions; a close never fires, and `closed`/`labeled` in `types` is a parse error |
| `workflow_dispatch` | `inputs:` (see below) | a manual dispatch from the UI/API |
| `schedule` | `- cron: "…"` | the maintainer's cron sweep, **default branch only** |

**`on.pull_request.types`** — the filter selects the lifecycle edges that start a run. The
vocabulary is **closed to what Threads can emit**: `opened`, `synchronize`, `reopened`. An absent
(or null) `types` fires on all three; a listed `types` fires only on the listed edges, and it is an
OR (`types: [opened, reopened]`). Two spellings are parse errors for that file, never silently dead
triggers:

- a type Threads never emits — `closed`, `labeled`, `assigned`, … — fails with
  `pull_request.types: unsupported type "closed" (supported: opened, synchronize, reopened)`; a
  closing PR never starts work (§6.2) and no label event exists, so accepting the filter would be a
  lie;
- `types: []` matches no action at all, so it fails with an explanation; omit `types` to fire on
  every edge.

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

**`workflow_dispatch.inputs`** — types `string` (default), `boolean`, `choice` (requires `options`):

| Field | Accepted | Notes |
|---|---|---|
| `description` | string | shown on the Run workflow form |
| `type` | `string` \| `boolean` \| `choice` | anything else is a parse error |
| `required` | bool | a missing required input is `422` naming the input |
| `default` | scalar | carried as text; a boolean default reads `true`/`false` |
| `options` | list of strings | required for `choice`; a value outside the list is `422` |

`${{ inputs.<name> }}` is substituted in step names, `run` scripts, step `env` values, step `if`,
`shell` and `working-directory`, and `inputs.<name>` is readable in `if`. A dispatch with `inputs`
must name exactly one workflow (the one whose schema applies).

**`on.schedule` — the supported cron subset.** Five fields, UTC:

```
minute(0-59) hour(0-23) day-of-month(1-31) month(1-12) day-of-week(0-6, 7=Sunday)
```

- supported: `*`, a number, a range `a-b`, a step `*/N`, `a-b/N`, `n/N` (from `n` to the field
  maximum, every `N`), and comma lists of those;
- **not** supported (each is that file's parse error): month/weekday names (`MON`), `@daily`-style
  macros, `L`/`W`/`#`, and a six-field expression with seconds;
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
| `runs-on` | label, or list of labels | required (unless the job is a reusable-workflow `uses`) | runner must have **every** label, case-insensitive. The hosted Linux labels (`ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, `ubuntu-20.04`) map onto `self-hosted` + `linux` before matching (§8.2); `windows-*`/`macos-*` are deliberately unmapped |
| `container` | image string, or mapping | runner's `--image` (`alpine/git:latest`) | mapping fields: `image`, `env`, `volumes`, `options`; `ports`/`command`/`entrypoint` are accepted but not applied |
| `services` | map of id → `image`/`env`/`ports`/`command`/`entrypoint`/`options`/`volumes` | none | one sidecar per entry, reachable at the id as a hostname |
| `env` | map of string → string | empty | overrides the workflow `env` |
| `needs` | one job key, or a list of job keys | none | `needs: test` and `needs: [test]` are identical; must name existing jobs, no cycles; a matrix job's dependents wait for **every** cell |
| `if` | expression string | implicit `success()`; with `needs`, every dependency must have succeeded | evaluated by the server before the job is claimable; `false` makes the job `skipped` (see §3.7) |
| `timeout-minutes` | integer | unbounded | bounds the whole job; when it expires the job is cancelled and its containers are removed (a step timeout reports `timed out`, a job timeout reads `cancelled`) |
| `continue-on-error` | bool | false | a failing job stays `failure` (marked `tolerated`), but the run conclusion follows the other jobs; fail-fast does not fire from it, `needs.<job>.result` reads `success`, and dependents run. **Divergence (D-4):** Forgejo documents this key as *ignored*; Threads keeps GitHub's semantics because dropping a working behaviour would turn a green run red |
| `environment` | name string, or mapping `{name, url}` | none | the deployment environment this job targets (§5.6). The name must match `[A-Za-z0-9][A-Za-z0-9._-]{0,63}` and the environment must exist in the repository (an undefined one fails the job); `url` must be absolute http(s); a job without one is unchanged |
| `strategy.matrix` | axes, plus `include` / `exclude` lists of objects | none | product of scalar axes; `include` merges into cells whose axis values do not conflict (an object matching no cell becomes a new cell), `exclude` removes a base cell matching all its pairs; axes expand in name order; 256 cells max (§3.3a) |
| `strategy.fail-fast` | bool | `true` | cancels the sibling matrix cells when one fails |
| `steps` | list | required (unless `uses`) | see §3.4 |
| `concurrency` | group or mapping | workflow default | job-level wins |
| `permissions` | mapping or shorthand | workflow default | job-level wins |
| `uses` / `with` | reusable workflow | — | accepted by the parser, then the job fails with "reusable workflows … are not supported in v1" |

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
itself (`name: cell-${{ matrix.v }}` renders `cell-a`; §3.6):

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
| `uses` | `actions/checkout`, `actions/upload-artifact`, `actions/download-artifact`, `./<path>` (a local action, §3.4a), `docker://image[:tag\|@digest]` (§3.4a) | — | a remote marketplace action (`owner/repo@ref`) fails the step with "there is no marketplace"; the built-in set falls back to the same rule |
| `with` | map of string → string | — | per action: checkout reads `fetch-depth`; the artifact actions accept exactly the keys below (an unknown key fails the step — it is never ignored) |
| `shell` | `sh` (default), `bash`, `python`/`python3`, `pwsh`/`powershell`, `cmd` | `sh` | anything else fails the step explicitly |
| `working-directory` | path | the workspace | relative paths join the workspace; absolute paths win |
| `env` | map of string → string | empty | overrides job and container env; `${{ }}` substituted |
| `if` | expression | implicit `success()` | the documented subset in §3.7; an unsupported expression **fails the step** with the parse error |
| `continue-on-error` | bool | false | the step may fail without failing the job; the result still records `failure` |
| `timeout-minutes` | integer | the remaining job time | on expiry the job container is killed (the only portable in-container kill) and the step fails with `timed out after …` |
| `id` | GitHub identifier: letters, digits, `_` and `-`, not starting with a digit | — | unique within the job (a duplicate is a parse error naming the job and the id); `steps.<id>.outcome` / `.conclusion` read it, and only a step with an id can publish outputs (§3.9) |

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
| `name` | both | letters, digits, `.`, `_`, `-`; not starting with `.`/`-`; ≤ 128 bytes | `artifact` | **unique per run**: a second upload of one name fails the step with `artifact "…" already exists in run …` — it never replaces what an earlier job published |
| `path` | upload | one or more patterns (newline- or comma-separated), relative to the workspace | required | see the pattern rules below |
| `if-no-files-found` | upload | `warn`, `error`, `ignore` | `warn` | `warn` logs a line and succeeds with no artifact; `error` fails the step; `ignore` says nothing |
| `retention-days` | upload | `0` (the run's window) or `1`…`90` | `0` | may **shorten** the run's `threads.retention` window, never extend it (expiry is always `min(run expiry, created + retention-days)`) |
| `path` | download | a workspace-relative directory (absolute container paths are taken as-is) | the workspace | the artifact's file paths are preserved under it (`dist/a.txt` → `<path>/dist/a.txt`) |

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
download; the attempt token can only read artifacts of its own run.


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
`actions/download-artifact` (the built-ins above), `./<path>` (a local action) and
`docker://<image>[:tag|@digest]` (an image). A remote marketplace action (`owner/repo@ref`) is
**not** supported and fails the step with "there is no marketplace": fetching and trusting arbitrary
repositories is its own security decision (pinning, mirroring, review, revocation), so it is
deliberately out of this slice rather than silently ignored. A local action's path is any
repository-relative directory; `..`, absolute paths, backslashes and `:` are refused (the path is a
git object path, never a host path).

**Resolution and pinning.** Local actions are resolved **at run creation, from the run commit** —
the same commit the job checks out. The parsed definitions are embedded in the job payload
(`Spec.Actions`), and the runner never reads a repository, so a run cannot be made to execute an
action file from another ref. The run page shows the expanded steps before anything runs. A
definition is *not* re-read when a step runs: if an earlier step edits `action.yml` in the workspace,
this run still executes the pinned definition (the next run, at a new commit, uses the new file).
What is **not** sandboxed is the action's `run:` scripts: they execute in the job container as
repository code, with the same trust as a workflow script step (the job token and repository secrets
that a script step receives). A compromised composite action file is a compromised job; review it
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
and `runs`. `runs.using` is required and is `composite` or `docker`; anything else (`node20`, …) is a
named refusal. Unknown keys are errors, as in workflow files.

**Composite actions.** `runs.steps` expands **in place** in the job's step list (the run page shows
the inner steps, each carrying the calling step's name as its parent). Rules:

- each inner step has exactly one of `run` or `uses`; a `run` step **must** name a `shell:`
  (`sh`, `bash`, `python`/`python3`, `pwsh`/`powershell`, `cmd` — the same resolution as a workflow
  step);
- `uses` is `./<path>` (another local action) or `docker://<image>`; the built-in actions and the
  marketplace are refused inside a composite (checkout belongs in the job, not in an action);
- the caller's `with:` values become `INPUT_<NAME>` environment variables (uppercase, spaces →
  underscores: `dry run` → `INPUT_DRY_RUN`), with the action's declared `default`s applied; a `with:`
  key the action does not declare is a **named refusal** (a typo must not silently do nothing).
  Inside an action, read the inputs from the environment (`$INPUT_VERSION`): there is no
  `${{ inputs.* }}` substitution in an action's fields (the expression subset §3.6/§3.7 applies to
  the workflow file), so a shell step is the supported way to consume an input;
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
- nesting is limited to **10** composite levels and cycles are refused, both at run creation (the job
  fails with a reason naming the chain) and again by the runner (it never trusts the payload, so a
  hostile spec cannot loop it).

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

**Not supported:** action `pre`/`post` hooks, `runs.steps[].uses: actions/...` inside a composite,
building a `Dockerfile` (push the image and use `docker://`), and remote marketplace actions (above).

### 3.5 `env` layering and built-ins



Precedence, lowest first: runner built-ins → workflow `env` → job `env` → container `env` → step
`env`; repository secrets are added last, by name. Every step sees:

| Variable | Value |
|---|---|
| `CI` | `true` |
| `GITHUB_ACTIONS` | `true` |
| `GITHUB_REPOSITORY` | `owner/repo` |
| `GITHUB_SHA` | the run's commit |
| `GITHUB_REF` | the triggering ref: `refs/heads/<branch>` for a push, `refs/tags/<tag>` for a tag push (the run has no branch then), the base branch for a pull request. A PR's *stored* `refs/pull/<n>/head` is the supersede key (§3.1a), never a job's `GITHUB_REF` |
| `GITHUB_REF_NAME` | the short name of that ref: the branch, the tag (`v1.2.3`), or the PR's base branch. A tag run never reads the empty branch — `${{ github.ref_name }}` is the tag too |
| `GITHUB_EVENT_NAME` | `push`, `pull_request`, `workflow_dispatch`, `schedule` |
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
| each repository secret | by its own name, trusted runs only (§5.1) |

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

| Context | `${{ matrix.<axis> }}` | `${{ inputs.<name> }}` | `${{ steps.<id>.outputs.<name> }}` / `${{ needs.<job>.outputs.<name> }}` |
|---|---|---|---|
| step `name`, `run`, `if`, `shell`, `working-directory` | yes | yes | yes |
| step `env` **values** | yes | yes | yes |
| step `with` values (the built-in actions) | yes | yes | yes |
| a job's `outputs` **values** | yes | yes | yes (`steps` of the same job, `needs` dependencies) |
| concurrency `group` (and a job `name`) | yes | yes | no (the group and name are computed before any step runs) |

**Structural fields and the env layers above a step are not substitution sites**: `runs-on` (every
accepted shape), `container.image`, `services.<id>.image` and `environment` (its `name` and `url`)
are read before a matrix cell or dispatch input exists, and a workflow/job/container/service `env`
value is copied verbatim — only a **step** `env:` value is substituted. A `${{ … }}` span in one is
a **parse error naming the field** — the file is reported and creates no runs — never a literal that
queues a job against a label no runner declares, hands the image gate the authored text, or reaches
the process environment unexpanded (`docs/WORKFLOWS.md` §6.2). Substitution there is PAR-053; until
it lands, write the value literally or declare one job per value, and put a reference that must be
substituted in a step `env:`.

Every "yes" cell also accepts `github.<field>` / `forgejo.<field>` / `forge.<field>`
(`event_name`, `sha`, `ref`, `ref_name`, `repository`, `workflow`, `job`, `run_id`, `actor`, and
the two server addresses `server_url`/`api_url` of §3.5), `github.event.inputs.<name>`,
`env.<NAME>` and `secrets.<name>`. The vocabulary is closed and checked at parse time: an unknown
context or field, an undeclared matrix key or dispatch input, or a malformed `secrets.<name>` is
that file's error naming the supported set — never a literal that reaches a shell.

The runner resolves a site's references in **one pass**: a substituted value is data, never a
template, so a secret whose body happens to contain `${{ github.ref }}` stays exactly that text.
The rule for a value that is not there is one of two:

- **an absent supported value expands empty.** An unset `env.NAME`, a secret the run does not have
  (a fork run, a typo), `github.actor` on a run with no actor, and `server_url`/`api_url` when no
  canonical origin is configured all read `''` — never the literal `${{ … }}` text.
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
`github.*` fields, `actor` included. A **job** `if:` still does not resolve `secrets.*` (PAR-058)
or the two addresses; a **step** `if:` reads `github.actor`, `github.server_url`/`api_url` and
`secrets.*` (§3.7).

A concurrency `group` also reads the run's ref contexts: `github.ref`, `github.ref_name`,
`github.repository`, `github.workflow`, `github.event_name`, `github.sha`, `github.run_id`. That is
the idiom `group: ci-${{ github.ref }}`, which gives each branch its own lock — without it, with
`cancel-in-progress: true`, a push on any branch would cancel `main`'s run. `env.*`, `steps.*`,
`needs.*`, `secrets.*`, `github.job`, `github.event.inputs.*` and operators are **not** supported
in a group (write `inputs.<name>` instead; `SPINDLE_*` are environment variables, and the
approved canonical `spindle.*` context — D-1, with `github.*`/`forgejo.*`/`forge.*` kept as
value-identical aliases — is not implemented yet, so `github.*` is the accepted spelling today).

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
leaves the job to fail mid-run. A `${{ … }}` span in a step field must be one reference; an
expression with operators or a function call belongs in `if:` (§3.7).

### 3.7 The expression subset (`if:`)

Supported in a **step** `if:`:

```
success()  failure()  always()  cancelled()   (canceled() is accepted too)
startsWith(text, prefix)  contains(text, search)  format('{0}-{1}', a, b)
github.<field>  forgejo.<field>  forge.<field>  github.event.inputs.<name>
matrix.<key>  inputs.<name>  env.<NAME>  secrets.<NAME>
steps.<id>.outcome  steps.<id>.conclusion  steps.<id>.outputs.<name>
needs.<job>.result  needs.<job>.outputs.<name>   (job if:)
!  ==  !=  &&  ||  (  )  'literal'  "literal"  true  false
```

The `github`/`forgejo`/`forge` fields are the closed set `event_name`, `sha`, `ref`, `ref_name`,
`repository`, `workflow`, `job`, `run_id`, `actor`; the aliases are value-identical. `format` uses
`{N}` argument indexes and `{{`/`}}` for literal braces. `secrets.<name>` reads the run's secret
map (empty on an untrusted run, never the literal text).

- comparisons are string-wise and case-insensitive (`'TRUE' == true`), matching GitHub's loose equality;
- an empty `if` means `success()`, so a plain step list runs in order;
- `success()`/`failure()` describe the **job** status: a step failure tolerated by
  `continue-on-error` does not flip them;
- `steps.<id>.outcome` is the step's raw result (`success`, `failure`, `cancelled`, `skipped`) and
  `steps.<id>.conclusion` is that result after the step's `continue-on-error` (a tolerated failure's
  conclusion is `success`). A step that did not run (skipped, or the remainder after a hard failure)
  is recorded too. Step ids are case-insensitive.
- anything else — `endsWith(...)`, `join(...)`, `toJSON(...)`, a nested object, an unknown
  context or `github` field, an undeclared matrix key/input, a wrong argument count — is a
  **parse error for the file** (the same expression also fails the step or job at evaluation time
  if a stored workflow reaches it), never a silent skip;
- `steps.<id>.outputs.<name>` reads the output a step published through its output file (§3.9). A
  step that ran but did not publish that name reads empty, so `if: steps.x.outputs.y == ''` works;
  an `<id>` that is not a step of the job is an evaluation **error** naming it (a typo must never
  read as "not set").

A **job** `if:` is evaluated by the server, before the job is claimable, with the same subset plus
the job contexts: `needs.<job>.result` (the dependency's outcome, folded over a matrix job's cells),
`needs.<job>.outputs.<name>` (the dependency's reported outputs, §3.9), `github.*`, `matrix.*`,
`inputs.*`, `env.*`, and `success()`/`failure()`/`cancelled()` folded from the dependencies. A job
with no `needs:` is decided when the run is created (and again on approval or re-run), so a false
condition is `skipped` immediately and never reaches a runner; a dependent job is decided once
every dependency is terminal. A false condition is `skipped` with the condition in the job's
`reason`; an expression outside this subset **fails the job** with the error, never a silent
skip. A job-level `if:` cannot read `secrets.*` or `steps.*` (neither is available at the
server's decision point — the parse error names it); a step `if:` can read `secrets.*`. A true condition lets the job run even after a failed dependency (`if: always()`,
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
name, an unterminated heredoc, a heredoc with an empty delimiter — is a **named step failure**
(`step output file /tmp/spindle-output-1: line 3: …`), never a silently dropped output. Names use
the step-id charset (letters, digits, `_`, `-`; at most 128 bytes).

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
across steps. The runner owns the container's lifetime (`--entrypoint /bin/sh` plus a keep-alive
command), so an image `ENTRYPOINT`/`CMD` is overridden; `container.entrypoint` and
`container.command` are accepted and recorded but not applied.

The workspace is a per-job named volume mounted at `/spindle/workspace`. The runner mounts nothing
else; a workflow that wants a host path must say so with `container.volumes` (`- /host/path:/in/job`),
which is a bind mount running under the daemon's user.

### 4.2 Services

Each `services:` entry becomes one sidecar container on the job's own network, with the entry's key
as its network alias — the job reaches it at `postgres:5432` (the *container* port, never the host
port). `env`, `volumes`, `options`, `command` and `entrypoint` are applied.

Readiness before the first step: the runner polls `docker inspect` (an image `HEALTHCHECK` is
honoured) and, for each declared port, TCP-dials it through an **ephemeral loopback** host port that
concurrent jobs cannot collide on. `unhealthy`, `exited` or `dead` fails the job immediately; no
service becomes ready within 60 seconds, and the job fails naming the service and its state. There is
no workflow field for a healthcheck in v1 — use the image's own `HEALTHCHECK` or a declared port.

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
  step of every job in the run;
- a fork run gets none: its payload carries `secrets: {}` and `secrets_denied: true`;
- masking is server-side and **best effort**: exact substring replacement of each value (values
  shorter than 4 bytes are skipped) in every log frame and step result. A runner that encodes,
  splits or derives a value defeats it;
- masking covers LOG LINES only. A step output (`$SPINDLE_OUTPUT`, §3.9) is data: it is stored on the
  job record and substituted into a dependent job verbatim, so never put a secret in an output and
  never echo one into an output file. Pass it as a secret to the dependent job instead;
- secrets are stored sealed (AES-256-GCM, bound to repository + name). Without
  `server.secrets_key` or `server.auth.session_secret` the store fails closed: the secrets API
  answers `503`, Settings → Secrets explains which key to set, and attempt tokens are signed with a
  process-local key (so a restart invalidates in-flight jobs' tokens).

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
- `permissions: contents: write` in the effective map (workflow merged with job, job wins, over
  `threads.default_permissions`) adds push to that one repository. `contents: read`, `none`, or no
  `permissions` at all means no push. A fork run is pinned to `read` regardless of what the file
  declares. An unrecognised level never widens access;
- the push policy still applies on top, because the token is a `ci:<runner>` principal and never the
  triggering user: a protected ref refuses it exactly like any other non-owner;
- it expires with the job — the job timeout plus 10 minutes, two hours when the job declares no
  timeout, capped at six hours — and is revoked at completion (the server re-reads the job record and
  rejects a terminal or superseded attempt);
- a push authenticated by it **never starts a workflow run** (the `GITHUB_TOKEN` rule), so a
  `contents: write` workflow that pushes cannot rebuild itself forever. The push lands, mirrors may
  sync, and a PR head may move — with no `push` run and no `pull_request: synchronize` run.

Read it from the environment; never write it into argv or `.git/config`:

```sh
git -c credential.helper='!f() { echo username=ci; echo password=$SPINDLE_JOB_TOKEN; }; f' \
    push origin HEAD:refs/heads/ci-output
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

**The default when a workflow declares no `permissions:` is `contents: read`** (PAR-114, D-12). This
is a recorded divergence from Forgejo, whose job token is implicitly write-capable and ignores
`permissions`: a Forgejo workflow that relies on the implicit write fails here with a real `403`
from the push gate — `the job token has no push permission (workflow permissions: contents: read)`
— rather than being silently upgraded. The default is the operator's decision
(`threads.default_permissions`, read or write); changing it is a configuration change, not a
workflow one. Unrecognised permission levels never widen access, and a fork run is pinned to
`contents: read` whatever the file says.

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

### 5.5 Job OIDC tokens (cloud identity)

A job can present a short-lived, audience-scoped ID token to a cloud provider — the Threads
equivalent of GitHub Actions' OIDC token. It is **off unless the job asks for it**:

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

- **What the job gets.** `SPINDLE_ID_TOKEN_REQUEST_URL` + `SPINDLE_ID_TOKEN_REQUEST_TOKEN` (and the
  GitHub spellings `ACTIONS_ID_TOKEN_REQUEST_URL` + `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, same values) in
  every repository-code step. A `docker://` action gets neither. The URL is container environment;
  the token is passed by name, never in `docker` argv.
- **Exchange.** `POST` the request URL with `{"audience":"…"}`. The answer is
  `{"value":"<signed JWT>"}`. A missing or empty audience is a `400` naming the field; the exchange
  is limited to **10 requests per minute per job attempt** (`429` with `Retry-After`).
- **The token.** RS256, five-minute lifetime, signed with the instance key. The issuer is
  `server.public_url` (set it: providers fetch
  `<public_url>/.well-known/openid-configuration` and `<public_url>/.well-known/jwks.json`). Claims:
  `iss`, `sub` (`repo:<owner>/<name>:ref:<ref>`), `aud`, `exp`, `iat`, `nbf`, `jti`, `repository`,
  `repository_owner`, `ref`, `sha`, `workflow`, `job`, `run_id`, `run_attempt`, `event_name`. For a
  pull request the `ref`/`sub` name the **base** branch (matching `GITHUB_REF`), not the PR head.
- **Who may ask.** Only a **trusted** run whose job's effective `permissions:` say `id-token: write`.
  A fork run never gets a request token and cannot exchange one; a request token is bound to one
  attempt of one job and expires with it; a finished job's token is refused. Configure the provider's
  trust policy against `iss`, `aud`, and the `sub`/`repository` claims.
- **The request token is a bearer credential.** It is stored hash-only server-side, delivered only to
  its own job, and masked in the runner's logs (best effort) — but a step that echoes or derives it
  defeats that. Never print it.

**Trust statement.** The signing key lives in the bucket (`threads/oidc/signing-key.json`). The bucket
is therefore a trust root for this feature exactly as it already is for every repository and secret:
whoever can write the key object can mint tokens this instance signs. There is one instance-wide key
(rotated automatically every 90 days; old public keys stay in the JWKS for an hour so in-flight
tokens still verify), and no per-repository key. The private material is never served by a page or an
API.

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
| `GET` | `/{o}/{r}/api/threads/environments` | `{environments: [{name, required_reviewers, secrets, created_at, updated_at, version}], enabled}` |
| `GET` | `/{o}/{r}/api/threads/environments/{name}` | `{environment: {…}}` |
| `PUT` | `/{o}/{r}/api/threads/environments/{name}` | `{"required_reviewers": ["ada", "grace"]}` → the environment |
| `DELETE` | `/{o}/{r}/api/threads/environments/{name}` | `204` |
| `GET` | `/{o}/{r}/api/threads/environments/{name}/secrets` | `{secrets: [{name, created_at, updated_at}], enabled}` |
| `PUT` | `/{o}/{r}/api/threads/environments/{name}/secrets/{secret}` | `{"value": "…"}` → `204` |
| `DELETE` | `/{o}/{r}/api/threads/environments/{name}/secrets/{secret}` | `204` |

- **A reviewer needs at least write access**, checked when the environment is saved: a name that
  cannot write is a `400` naming the reviewer, never a stored gate nobody can open. At most 10
  reviewers, deduped and lowercased.
- **A job naming an undefined environment fails** with `environment "x" is not defined in this
  repository (create it in Settings → Environments)`. The rest of the workflow still runs.
- **Required reviewers hold the run.** The run is created `pending_approval` (`approval_kind:
  environment`, `environments: ["production"]`), its checks mirror `pending`, and **nothing is
  enqueued** — no runner can see the deployment. Only a **listed reviewer (holding write or more) or
  a repo admin** may approve (`POST …/api/threads/{run}/approve`) or deny (`…/deny`); approval stamps
  the job's deployment record with who and when and then queues the job. A second decision is `409`.
  With **no available reviewer the run simply stays pending** — there is no timeout and no
  auto-approval.
- **Denied** fails exactly the deployment jobs, with `deployment to environment "production" denied by
  <who>`; the run's other jobs continue so the failure reads beside the build.
- **Secrets** are stored exactly like repository secrets (AES-256-GCM, write-only) but bound to the
  environment: only a job that names it, in a **trusted** run, with an **approved** deployment,
  receives them. They are injected as environment variables of their own name and masked best effort
  in logs. **They override repository secrets of the same name** (GitHub's rule); the rest of the
  repository set is still present.
- **A fork run never receives environment secrets**, cannot be listed as a reviewer (it has no write
  access) and therefore cannot approve its own deployment. A fork run naming an environment **with**
  reviewers stays pending until a reviewer decides — or fails closed if nobody does; naming one
  **without** reviewers runs like any fork job (no secrets).
- **Deletion while a run is pending** fails the deployment job closed (`… the environment no longer
  exists`) instead of releasing it, and a **re-run** is a new decision: the gate is re-applied.
- The run page shows the environment, `pending`/`approved`/`denied`, the decider and the time, and
  the declared URL. It never shows a secret.

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
- a pull request **merge** creates a `push` run for the base branch (the merge commit), because the
  merge moves the base ref server-side.

### 6.2 Pull request

- fires on `opened`, `synchronize` and `reopened`; **`close` never fires** (a closing PR must not
  start work, so no `closed` action exists to filter on);
- `on.pull_request.branches` filters on the **target** branch; `on.pull_request.types` filters the
  lifecycle edge (§3.2): absent = all three, `types: [opened]` = only a new PR. The vocabulary is
  closed — `closed`, `labeled` and any other unemitted action are that file's parse error naming the
  supported set, so a filter can never name an edge that will not happen;
- same repository: the head commit's workflow file runs, trusted, with secrets;
- fork: the **base branch's** workflow file runs at the base tip, untrusted, no secrets, read-only
  token, and possibly behind the approval gate (§5.4). The run header records both commits
  (`commit` = the head being built, `workflow_commit` = where the file was read), so the trust
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

spindle threads run acme/widgets nightly.yml --ref main   # no inputs from the CLI
```

Response `201 {"runs": […]}`. An unknown ref or workflow is `404`, a bad body `400`, and an input
that does not match the declared schema `422` naming the input. `inputs` requires an explicit
`workflow`; omitting `workflow` dispatches **every** file that declares `workflow_dispatch` at the
ref.

### 6.4 Schedule

The parser validates cron; the **maintainer** evaluates it.

- **UTC, five fields**, the subset in §3.2;
- the sweep reads workflow files at the **default branch tip** only, so a schedule on a feature
  branch never fires; the run is `event: schedule`, `actor: schedule`, trusted, on the default
  branch's commit;
- when a process with the `maintain` role is running, the sweep is due once per wall-clock minute
  (cron's own granularity). A process started with `server.roles = ["serve"]` and no maintainer runs
  no schedules;
- ticks are **at most once**: a per-file/per-cron watermark object is CAS-updated **before** the run
  is created, so two instances racing the same minute create one run, and a restart re-reads the
  watermark instead of re-firing. A crash between the watermark write and run creation loses that
  one tick (deliberate: a duplicate scheduled deploy is worse than a missed one); a failed run
  creation consumes the tick and is logged, never retried in a spin;
- on a first run with no watermark, the current minute's boundary is eligible immediately, so a
  daily schedule that starts moments late still gets today's tick.

There is no separate "run now" verb for a schedule — use `workflow_dispatch`.

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
| `--allow-fork-runs` | `SPINDLE_RUNNER_ALLOW_FORK_RUNS` | off | execute fork pull-request code on this runner (only on a disposable host; §8.7) |
| `--fetch-timeout` | — | `20` | long-poll seconds per fetch (clamped 1–30) |

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
not silently accept it. The **image** stays the runner's own declaration (`--image`, or the
workflow's `container.image`): there are no hosted Ubuntu images here (D-15), so the operator who
wants the hosted toolchain installs it in the image (the label→image mapping is PAR-171/177, a later
wave). Deliberately **withheld**: `windows-*`, `macos-*` (the D-15 Windows/macOS non-goal — there is
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
- Concurrency groups cap a *run* (not a runner): one in-flight run per group per repository.
- **The repository policy is tenancy, not isolation.** An allowlist keeps one team's code off
  another team's repositories; it does not contain the job that runs. Treat the *host* as the
  isolation boundary — see the dedicated-worker shape in §8.5.

### 8.4 Stop, cancel, cleanup

`SIGTERM` (and `SIGINT`) cancels the in-flight jobs: each job's context dies, its containers are
removed, and the attempt is reported `cancelled` on a fresh context so the conclusion is not lost.
Cleanup runs on **every** path — success, failure, setup error, cancel, step/job timeout, runner
panic — with a fresh 90-second budget: `docker rm -f` on the job container and every service,
`docker network rm`, `docker volume rm -f`. Names are a hash of (repository, run, job, attempt), so a
leftover from a killed runner is identifiable and removable by name.

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
workflow-controlled argv, so both ends enforce one small allow-list:

| Allowed | Notes |
|---|---|
| `--cpus`, `--memory`/`-m`, `--memory-swap`, `--shm-size` | resource accounting |
| `--ulimit nofile=…` | open-file limit only; other ulimit keys are node knobs |
| `--init`, `--read-only` | in-container confinement |
| `--user`/`-u`, `--workdir`/`-w` | run as a non-root user, working directory |

Everything else is refused: `--privileged`, every namespace flag (`--pid`, `--ipc`, `--network`,
`--userns`, `--cgroupns`, `--uts`), `--cap-add`, `--device`, `--security-opt`, `--group-add`,
`--add-host`, `--publish`, `--entrypoint`, and any mount (`--volume`, `--mount`, `--tmpfs`,
`--volumes-from`). **Workflow-supplied volumes are refused outright** in this version: the runner
adds its own per-job workspace volume, and a workflow-chosen bind mount is host filesystem access.

The check parses both `--opt=value` and `--opt value` with the same splitter the runner builds argv
with (quoting included), refuses a bare argument, and runs in two places:

- **Server, at run creation and rerun** — the same pass as the image policy. A refusal is a failed
  job with a `Reason` naming the field and the offending option (e.g. `container.options carries
  "--privileged"`), a red check mirror, and no enqueue; a job a condition already skipped is not
  refused.
- **Runner, at plan time** — a defensive re-check: a job whose options or mounts are not allow-listed
  fails with a `spindle:` diagnostic before any container, network or volume is created, even if the
  server was older or bypassed.

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
- `spindle threads list|show|run|cancel|rerun` are all served by the API. The verb's answer is the
  run header/state only — poll the run (`spindle threads show`) for per-job state; cancel is
  acknowledged by the runner, not by the call.
- **Why a run is `cancelled`** is on its header: `cancel_reason` is `operator` (with `cancelled_by`
  naming the maintainer), `supersede` (with `superseded_by` naming the newer run — §3.1a) or
  `concurrency` (a group's `cancel-in-progress` took the group). A run that ended on its own
  carries neither, and a second cancel never rewrites the first reason: the path is idempotent.
  The run page shows the same fact on the header and, for a superseded run's queued jobs, as each
  job's reason.

### 9.2 Retention

`threads.retention` (default **720h = 30 days**) deletes a run once it is **terminal** and its
completion (or creation, if it never started) is older than the window. Jobs, log frames and queue
entries of that run go with it; checks do not (§7). A queued, running or `pending_approval` run is
never touched, whatever its age. There is no "keep forever" spelling — set a very large window if
you want one.

```toml
[threads]
retention = "720h"
approve_first_time_forks = true
default_permissions = "read"
```

Environment overrides are exact paths: `SPINDLE__THREADS__RETENTION`,
`SPINDLE__THREADS__APPROVE_FIRST_TIME_FORKS`, `SPINDLE__THREADS__DEFAULT_PERMISSIONS`.

### 9.3 Maintainer tasks

| Task kind | Cadence | Body |
|---|---|---|
| `threads-gc` (queue half) | every 5 minutes per repository | requeues claims whose lease lapsed; fails a job that has exhausted 3 attempts; trims retired entries from the dispatch head |
| `threads-gc` (retention half) | at most once an hour per repository | the retention window above; deletes a run's artifact blobs and manifests with the run, reclaims artifacts whose `retention-days` window closed early (one expiry-index LIST), and deletes revoked instance runners past the window plus expired unused registrations; at most 200 runs and 200 runner records per pass (the runner family is swept at most hourly per process) |
| `threads-queue-reconcile` | every 5 minutes per repository | rebuilds that repository's rows in the global queue hint from its authoritative queue entries (the only place the index LISTs; a lost enqueue write is repaired here) |
| `threads-schedule` | minute-aligned, once per minute per repository | evaluates `schedule:` cron against the watermark (§6.4) |

They run in a process whose roles include `maintain` (`server.roles` empty = all roles). Each pass is a
task with progress packets and a one-line summary notice
(`requeued=… runs=… jobs=… logs=… artifacts=… artifact_bytes=… queue=… runners=… credentials=… freed=…B`); read it from
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
repos/<o>/<r>/threads/queue/<seq>.json                       queue entries + head.json
threads/runners/<id>.json                                    instance runner registry (+ capacity, current job)
threads/runners/tokens/<id>.json                             one-time registration tokens (hashes)
threads/runners/credentials/<id>.json                        runner credentials (hashes)
threads/queue/index.json                                     global dispatch hint (CAS'd, bounded at 10k)
repos/<o>/<r>/threads/secrets/<name>.json                    sealed secret envelopes
repos/<o>/<r>/threads/schedule.json                          cron watermark
```

Nothing here is on local disk; deleting every instance loses warmth, not state.

## 10. Troubleshooting

| Symptom | Cause | What to do |
|---|---|---|
| Run stays `queued`, every job says `not started` | no runner is fetching; labels don't match (labels are exact and matched as a subset; `ubuntu-latest` is mapped to `self-hosted` + `linux`, a `windows-*`/`macos-*` label never maps — §8.2); the run is a fork PR and no active runner has `--allow-fork-runs` (§8.7); all capacity slots busy; a concurrency group is held by another run | `/admin/runners`: a runner in state `active` with a recent last-seen and a label superset of `runs-on`; the run page shows `runs-on` per job; check the other run holding the group |
| A step fails naming `$GITHUB_ENV` / `$GITHUB_PATH` | the file line is not `NAME=VALUE` / a directory (a heredoc belongs to `$GITHUB_OUTPUT`), a protected name was refused (`PATH`, `LD_PRELOAD`, `GITHUB_*`, …), or a cap was hit (§3.10) | the error names the line and the variable; use `$GITHUB_PATH` for `PATH`, and a run step's own `env:` for values that must be local to it |
| No run at all after a push | no workflow file at that commit; `on:` doesn't match the event/ref (the glob for that ref kind, or a tags-only trigger on a branch push); the file failed to parse; the ref was a delete or a notes ref; the push was made by a job token | the Threads page's **Workflows** section lists files at the default tip and their parse errors; deletes and notes refs create no run and job-token pushes are suppressed |
| A workflow file is rejected | an unknown key, a bad type, a cron outside the subset, a step with both `run` and `uses`, a `needs:` naming a missing job, a `${{ }}` in a structural field (`runs-on`, a container/service image, an `environment`) or in a workflow/job/container/service `env` value, an unsupported `${{ }}` reference or `if:` function, a matrix over 256 cells, a file over 1 MiB | the error names the file and the offending key, anchored `#workflow-<slug>` on the Threads page and in `GET …/api/threads/workflows`; other files keep running — only this one creates no runs |
| `defaults:` or `paths:` is rejected | not part of the v1 subset | drop it; see §11 |
| Job fails immediately at checkout | the job image has no `git` (or no CA certs); the clone URL is unreachable from the container; the repository is empty at that commit | use an image with `git`, or the default `alpine/git:latest`; check the checkout URL in the log (loopback is rewritten to `host.docker.internal`); make sure the commit exists |
| Push from a job is refused | shallow clone (`fetch-depth` not `0`); the job has `contents: read`; a protected ref refuses a `ci:` principal; non-fast-forward | see the ERR packet / log line: it names the cause. Add `fetch-depth: 0`, declare `permissions: contents: write`, push a fast-forward, or relax the policy |
| Secret is empty in a step | the run is a fork/untrusted run (secrets are never delivered); no `server.secrets_key`/`session_secret` (the API answers `503`); the name doesn't match | the run page shows the `untrusted` chip; Settings → Secrets explains a missing key; names are exact and case-sensitive |
| Run is `pending_approval` | a first-time contributor's fork PR, gate enabled | a `maintain`+ user approves (or denies) on the run page; or set `threads.approve_first_time_forks = false` |
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

- **No caches.** `actions/cache` is not implemented (artifacts are: §3.4). Every job starts from a
  cold workspace; reuse a registry image or fetch a baked dependency layer instead.
- **Actions.** The built-in set is `actions/checkout`, `actions/upload-artifact` and
  `actions/download-artifact`; a repository-local action (`uses: ./<path>`) and a `docker://image`
  step are supported (§3.4a). A **remote marketplace action** (`owner/repo@ref`) is not, and fails
  the step with "there is no marketplace": fetching and trusting an arbitrary repository is its own
  security decision (pinning, mirroring, review, revocation), deliberately deferred rather than
  half-done.
- **Artifacts are same-run only.** `download-artifact` cannot name another run or workflow; pass
  data across runs through a release, a package or a pushed branch.
- **No reusable workflows.** A job-level `uses:`/`with:` is accepted by the parser and then fails the
  job with "reusable workflows … are not supported in v1".
- **No `defaults:` block**, no `on.push.paths`/`paths-ignore`, no
  `pull_request` `types` beyond `opened`/`synchronize`/`reopened` (`closed`, `labeled` and the rest
  are parse errors — see §3.2), no `pull_request_target`, no `workflow_run`, no
  `strategy.max-parallel` (matrix `include`/`exclude` **are** supported, §3.3a).
- **No GitHub expression language** beyond §3.7 (`startsWith`, `contains`, `format` are in; the
  rest are parse errors). `${{ }}` substitution itself is complete: `github.*` (the closed field set,
  `actor` and the two server addresses included), `env.*`, `secrets.*` (embedded too), `matrix.*`,
  `inputs.*` and `steps`/`needs` outputs all resolve in every site §3.6 lists, in one pass. A
  structural field (`runs-on`, a container/service image, an `environment`) and a
  workflow/job/container/service `env` value are **not** sites — only a step `env:` is: a `${{ }}`
  span there is a parse error until PAR-053, never a literal that reaches the runner or the process
  environment. A job-level `if:` still cannot read a secret (PAR-058) and does not resolve the two
  server addresses. Use `$GITHUB_*` and `$NAME` in a script as well — they are always present, and
  they are what the shell sees.
- **No per-job resource knobs beyond `container.options`** (`--cpus`, `--memory`, `--shm-size`,
  `--ulimit nofile=…`), and no workflow-declared container `entrypoint`/`command`/`ports` (the
  runner owns the container's lifetime; the daemon's own limits still apply).
- **No server-side per-repository concurrency cap**; the only caps are runner capacity, concurrency
  groups, the 256-cell matrix product and the 3-attempt queue limit.
- **No checks re-run separate from a job re-run**, and no log retention beyond the run's own window.
