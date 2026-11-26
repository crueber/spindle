# Writing workflows for Threads

> **This is the authoring guide.** It is for the person writing and debugging a workflow file:
> every key the parser accepts, what it does, the recipes that cover most real CI, and the
> migration path from another CI system. The exhaustive reference — runner flags, image
> mirroring, capacity and isolation, the wire protocol, operating the fleet — is
> [`threads.md`](threads.md). When this guide and the reference disagree, `threads.md` and the
> parser (`internal/threads/workflow/`) win; the examples here are pinned to the parser by a test.
>
> **Fence convention.** Every ` ```yaml ` block in this file is a complete workflow and parses
> under the real parser (`TestWorkflowsGuideExamplesParse`). Action metadata uses
> ` ```yaml-action `. A block marked ` ```yaml-forgejo ` is another CI system's original, kept
> beside its translation; it is deliberately not parsed here.

Threads is spindle's CI. A workflow file lives in the repository at
`.spindle/workflows/<name>.yml`; a matching event creates a **run**; the run's **jobs** are
claimed by registered **runners** and each job runs in its own container; every job reports one
check (`threads/<workflow>/<job>`) into the same surface that gates merges.

**Which file to open:**

| You want to… | Read |
|---|---|
| write, review or migrate a workflow | this file |
| understand a trigger, a key or a failure in full | [`threads.md` §3](threads.md#3-workflow-reference), [§5](threads.md#5-secrets-and-permissions), [§6](threads.md#6-triggers-in-detail), [§10](threads.md#10-troubleshooting) |
| install or size a runner, mirror images, reason about isolation | [`threads.md` §8](threads.md#8-the-runner) |
| change the engine itself | [`features/15_threads.md`](features/15_threads.md) |

## 1. The five-minute path

### 1.1 The file

```
.spindle/workflows/ci.yml
.spindle/workflows/release.yaml
```

- Only `.spindle/workflows/*.yml` and `*.yaml` are read: **one directory, no nesting**, no
  `.github/` fallback and no subdirectories.
- Each file is capped at **1 MiB**. A file that does not parse creates no runs of its own; its
  error appears in the repository's **Threads → Workflows** section (and
  `GET …/api/threads/workflows`) while every other file keeps running.
- The file is read **from the commit being built**, so the push that adds it triggers it.
- Workflow names become part of the check context: `name: Build` + job `test` reports
  `threads/build/test`.

### 1.2 The smallest useful workflow

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
permissions:
  contents: read
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    steps:
      - uses: actions/checkout
      - run: go vet ./...
      - run: go test ./... -count=1
```

### 1.3 What has to exist before it runs

A run does nothing until a runner claims its jobs. A host administrator mints a registration
token in the browser at **`/admin/runners`** (with the runner's repository policy), then on the
runner host:

```sh
spindle --server https://git.example.com runner serve \
  --register wrg_… \
  --token-file /etc/spindle/runner.token \
  --labels self-hosted,linux,docker --name build-1 --capacity 1
```

Afterwards the token file is enough. Docker is required, and the daemon is named by `DOCKER_HOST`.
The full flag table, the containerised runner, `--allow-host-docker`, `--allow-fork-runs` and the
image flags are [`threads.md` §8](threads.md#8-the-runner); the minimum an author needs is that
the runner's labels are a **superset** of every `runs-on` you write.

### 1.4 Watch it

| Where | What |
|---|---|
| `/{owner}/{repo}/threads` | run list, parse errors, **Run workflow** |
| `/{owner}/{repo}/threads/{run-id}` | jobs, `runs-on`, runner, check context, step results, artifacts, live logs |
| the commit page | the **Checks** panel |
| `spindle threads list\|show\|run\|cancel\|rerun` | the same over the API/CLI |

## 2. Key reference

The parser is **strict**: an unknown key is a **parse error for that file** — never silently
ignored. That includes top-level keys, trigger mappings, job/step keys, container keys, action
metadata and matrix axes. A file with an unknown key runs nothing and is reported by name.

### 2.1 Top level

| Key | Type | Default | What it does |
|---|---|---|---|
| `name` | string | file stem | display name and the `<workflow>` segment of every check context |
| `on` | event name, list, or mapping | — | triggers; a file with no `on` parses but never fires |
| `env` | map[string]string | empty | variables merged **under** job/step `env`; scalars are coerced to text (`COUNT: 3` → `"3"`), a nested value is a parse error |
| `concurrency` | string or `{group, cancel-in-progress}` | none | one in-flight run per group per repository (§5.4) |
| `permissions` | mapping, or `read-all` / `write-all` | instance `threads.default_permissions` (`read`) | scopes the job token; only `contents` and `id-token` have an effect (§7) |
| `jobs` | map[string]job | **required** | at least one job |

### 2.2 `on`

| Trigger | Accepted keys | Fires when |
|---|---|---|
| `push` | `branches`, `tags` | a branch push matching `branches`; `tags` is inert and unsafe — a `tags`-only trigger matches **every branch push** (§3.1) |
| `pull_request` | `branches`, `types` | the listed lifecycle actions on a PR targeting a matching branch |
| `workflow_dispatch` | `inputs` | a manual dispatch from the UI, API or CLI |
| `schedule` | a list of `- cron: "…"` | the maintainer's cron sweep, default branch only |

`on` accepts all three spellings. A bare key is a real trigger:

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

`workflow_dispatch.inputs`:

| Field | Type | Notes |
|---|---|---|
| `description` | string | shown on the Run workflow form |
| `type` | `string` \| `boolean` \| `choice` | default `string`; anything else is a parse error |
| `required` | bool | a missing required value is `422` naming the input |
| `default` | scalar | kept as text; a boolean default reads `"true"` / `"false"` |
| `options` | list of strings | **required** for `choice`; a value outside the list is `422` |

### 2.3 A job

| Key | Type | Default | What it does |
|---|---|---|---|
| `name` | string | job key | display name and the `<job>` segment of the check context; `${{ matrix.* }}` / `${{ inputs.* }}` are filled in |
| `runs-on` | label or list | **required** unless `uses` | the runner must declare **every** label, case-insensitive and exact (no wildcards) |
| `container` | image string or mapping | runner `--image` (`alpine/git:latest`) | one container for the job; fields below |
| `services` | map[id]service | none | sidecars on the job network, reachable at their id |
| `env` | map[string]string | empty | overrides workflow `env` |
| `needs` | list of job keys | none | must exist and be acyclic; a matrix job's dependents wait for **every** cell |
| `if` | expression | `success()` over `needs` | evaluated by the server before the job is claimable (§5.3) |
| `timeout-minutes` | int | unbounded | cancels the whole job and removes its containers |
| `continue-on-error` | bool | false | the job stays `failure` but the **run** conclusion follows the other jobs; fail-fast does not fire from it |
| `environment` | name or `{name, url}` | none | deployment target with reviewers and secrets (§7.4) |
| `outputs` | map[name]template | none | values a dependent job reads as `needs.<job>.outputs.<name>` (§6.5) |
| `strategy.matrix` | map[axis][]scalar | none | simple product; `include`/`exclude` are parse errors (§5.2) |
| `strategy.fail-fast` | bool | `true` | a failed cell cancels its siblings |
| `steps` | list | **required** unless `uses` | §4 |
| `concurrency` | group or mapping | workflow default | job-level wins |
| `permissions` | mapping or shorthand | workflow default | job-level wins |
| `uses` / `with` | string / map | — | reusable workflow: parses, then the job fails with "not supported in v1". Do not plan around it. |

Container mapping keys: `image` (required in practice), `env`, `ports`, `command`, `entrypoint`,
`options`, `volumes`. Only `image`, `env` and `options` are applied; `ports`, `command`,
`entrypoint` and `volumes` are accepted and not applied — the runner owns the container's
lifetime and mounts only its workspace volume. `options` is checked against a small allowlist
(§4.5).

A service entry takes `image`, `env`, `ports`, `command`, `entrypoint`, `options`, `volumes`.

### 2.4 A step

| Key | Type | Default | What it does |
|---|---|---|---|
| `id` | identifier | — | letters, digits, `_`, `-`; not starting with a digit; unique in the job. Needed to publish outputs and to read `steps.<id>.*` |
| `name` | string | `Step <n>` | display name; substituted |
| `run` | string | — | exactly one of `run` / `uses` |
| `uses` | action | — | built-in, local, or `docker://` (§4) |
| `with` | map[string]string | — | action inputs; an unknown key fails the step |
| `env` | map[string]string | empty | overrides job/container env |
| `if` | expression | `success()` | the subset in §6.3 |
| `shell` | `sh` (default), `bash`, `python`/`python3`, `pwsh`/`powershell`, `cmd` | `sh` | the interpreter; `sh` runs `sh -e -c`, `bash` runs `bash -eo pipefail -c` |
| `working-directory` | path | the workspace | relative joins the workspace; absolute wins |
| `continue-on-error` | bool | false | step may fail without failing the job (result still `failure`) |
| `timeout-minutes` | int | remaining job time | on expiry the job container is killed and the step reports `timed out` |

A failed step without `continue-on-error` skips the rest of the job (each recorded `skipped`).

### 2.5 What is deliberately not accepted

**Refused** — a parse error naming the key, so the file creates no runs:

- `defaults:` (workflow, job or step);
- `on.push.paths` / `paths-ignore`, and `pull_request.paths`;
- `on.pull_request.types` values other than `opened`, `synchronize`, `reopened` (and an empty
  list);
- `strategy.matrix.include` / `exclude`, `strategy.max-parallel`;
- a step with both `run` and `uses`, or neither;
- a duplicate step id, or one starting with a digit;
- a `needs:` naming a missing job, or a cycle;
- a job without `runs-on` (unless it is a reusable `uses` job) or without `steps`;
- an unknown trigger name (`on: [tag]`), `on: schedule` without `- cron:` entries, and any cron
  outside the subset (§8).

**Accepted but inert** — the file parses and runs, but the key changes nothing:

- `permissions` keys other than `contents` and `id-token` (`actions: write`,
  `packages: write`, … parse and do nothing);
- `container.ports` / `command` / `entrypoint` / `volumes` and the `services.*` equivalents;
- `on.push.tags` — a tag push **never** creates a run, and because an empty `branches`
  filter matches everything, a `tags`-only trigger fires on **every branch push**. Do not
  use it (§3.1).

## 3. Triggers and filters

### 3.1 `push`

```yaml
name: main and release branches
on:
  push:
    branches: [main, "release/*"]
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: make
```

- Fires for `refs/heads/<branch>` only. **Tag pushes and branch deletes create no run** — a
  release process cannot start from a tag push; use `workflow_dispatch` (§8.4).
- `branches` matches exact names and `*` wildcards only (`release/*`). There is no `!` negation
  and no `**`. An absent or empty filter matches every branch.
- The workflow is read at the pushed commit.
- A merged pull request creates a `push` run for the base branch (the merge commit).
- A push authenticated by a job token creates nothing (the `GITHUB_TOKEN` rule), so a
  `contents: write` workflow cannot rebuild itself forever.

`on.push.tags` parses but never fires, and it does not narrow anything: a trigger whose only
filter is `tags` has an empty `branches` list, which matches **every branch push**. Never write
it, and never mix it with `branches` expecting tag filtering. Dispatch the version as an input
and create the tag in the job (§9.5); the tag push then intentionally starts nothing.

### 3.2 `pull_request`

```yaml
name: PR checks
on:
  pull_request:
    branches: [main]
    types: [opened, synchronize, reopened]
jobs:
  test:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: go test ./...
```

- `branches` filters the **target** branch.
- `types` selects lifecycle actions. Absent = all three. The vocabulary is closed:
  `opened`, `synchronize` (only when the head actually moved), `reopened`. `closed`, `labeled`
  and the rest are parse errors; a closing PR never starts work.
- Same repository: the head commit's workflow runs, **trusted**, with secrets.
- Fork: the **base branch's** workflow file runs at the base tip, **untrusted**, no secrets,
  read-only job token; a first-time contributor's run may wait for approval (§7.3).
- `synchronize` fires only when the head moved, so a replay cannot duplicate a run.

### 3.3 `workflow_dispatch`

`environment: ${{ inputs.environment }}` is **not** substituted: the structural fields
(`runs-on`, `container.image`, `environment`) and workflow/job/container `env` do not take
`${{ }}`. Write the environment literally and use the input for the command:

```yaml
name: Deploy
on:
  workflow_dispatch:
    inputs:
      environment:
        description: target environment
        type: choice
        options: [staging, production]
        required: true
        default: staging
jobs:
  deploy:
    runs-on: linux
    environment: production
    steps:
      - uses: actions/checkout
      - name: deploy
        if: inputs.environment == 'production'
        run: ./deploy.sh --target "${{ inputs.environment }}"
```

How to dispatch:

- UI: **Run workflow** (write role only), typed fields rendered from the declared schema.
- API: `POST /{owner}/{repo}/api/threads` with
  `{"ref":"main","workflow":"deploy.yml","inputs":{"environment":"staging"}}` → `201`.
- CLI: `spindle threads run OWNER/REPO deploy.yml --ref main` (no inputs from the CLI).

`inputs` requires an explicit `workflow`; omitting it dispatches every file that declares
`workflow_dispatch` at the ref. An unknown ref/workflow is `404`, a bad body `400`, a value that
does not match the schema `422` naming the input.

### 3.4 `schedule`

```yaml
name: Nightly
on:
  schedule:
    - cron: "0 3 * * *"
jobs:
  sweep:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: ./scripts/nightly.sh
```

Five fields, **UTC**: `minute hour day-of-month month day-of-week`. Supported: `*`, a number, a
range `a-b`, a step `*/N`, `a-b/N`, `n/N` (from `n` to the field maximum every `N`), and comma
lists. Not supported (parse error): weekday/month names, `@daily` macros, `L`/`W`/`#`, and a
six-field expression with seconds.

Behaviour: default branch tip only; a process with the `maintain` role must be running; at most
once per tick (a CAS'd watermark), so a restart does not re-fire and a crash between the
watermark and run creation loses that tick rather than duplicating a deploy. There is no
"run now" — dispatch the workflow manually. See [`threads.md` §6.4](threads.md#64-schedule).

### 3.5 What starts a run: the summary

| Event | Created by | Trusted | Secrets | Workflow read at |
|---|---|---|---|---|
| `push` | client push, PR merge | yes | yes | the pushed/merge commit |
| `pull_request` (same repo) | PR opened/synchronize/reopened | yes | yes | the head commit |
| `pull_request` (fork) | same | **no** | **none** | the **base branch tip** |
| `workflow_dispatch` | UI / API / CLI | yes | yes | the resolved ref |
| `schedule` | maintainer sweep | yes | yes | the default branch tip |

## 4. Steps and actions

### 4.1 `run` steps

`run` is a shell script. Multi-line scripts use YAML's block scalar; the shell is `sh` unless
`shell:` says otherwise:

```yaml
name: scripts
on: push
jobs:
  shell:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: multi-line sh
        run: |
          set -eu
          for f in *.go; do
            echo "checking $f"
          done
      - name: bash needs pipefail explicitly
        shell: bash
        run: |
          set -euo pipefail
          grep -R "TODO" . | head -5
      - name: run in a subdirectory
        working-directory: web
        run: npm test
```

- `sh` is `sh -e -c`; `bash` is `bash -eo pipefail -c`; `python` is `python3 -c`. `pwsh` and
  `cmd` are accepted but need the interpreter in the image.
- Quoting is the shell's. `$NAME` is a container environment variable (workflow/job/step `env`,
  a secret, or a built-in); `${{ matrix.x }}` / `${{ inputs.x }}` are substituted **before** the
  shell sees the script.
- `${{ github.* }}`, `${{ env.* }}` and `${{ secrets.* }}` are **not** substituted in a script.
  Use `$GITHUB_SHA`, `$GITHUB_REF`, the secret's own name, and so on.

### 4.2 `uses:` — the exact spellings

| Spelling | What happens |
|---|---|
| `actions/checkout` | built-in checkout (§4.3). `actions/checkout@v4` also resolves: everything after `@` is ignored. |
| `actions/upload-artifact` | upload files from the job container (§6.4). `@v4` works too. |
| `actions/download-artifact` | download an artifact of the same run. `@v4` works too. |
| `./.spindle/actions/<name>` (any `./` path) | repository-local action from **this run's commit** (§4.4) |
| `docker://image[:tag\|@sha256:…]` | run the image as its own container on the job network (§4.4) |
| anything else (`owner/repo@ref`, `actions/setup-go`, `actions/cache`, …) | the step fails: *"there is no marketplace for remote actions"* |

There is **no marketplace, no action cache, no `actions/cache`**, no setup actions, and no
`pre`/`post` hooks. A toolchain is a base image, a `run:` step, or a local action — never a
remote `uses:`.

### 4.3 Checkout

`actions/checkout` runs **inside** the job container. It clones the run's exact commit with the
job token, shallow by default:

```yaml
name: full history
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

- The only supported `with:` key is `fetch-depth`. `0` (or `"0"`) is a full clone; anything
  else is shallow.
- The workspace is `/spindle/workspace` (`$GITHUB_WORKSPACE`).
- **Any job that runs `git push` must use `fetch-depth: 0`**: a shallow clone cannot push, and
  the server refuses it with an ERR packet naming the cause.
- The clone URL comes from `server.public_url`, else the runner's origin. It must be reachable
  from **inside** the job container; a loopback origin is rewritten to `host.docker.internal`.
  Failure adds a named reachability diagnostic to the log. Operator detail:
  [`threads.md` §4.4](threads.md#44-checkout).
- The checkout's authority is the job token's: a `contents: read` job can clone but not push.

### 4.4 Local actions and `docker://`

A local action is a directory in the repository with `action.yml` or `action.yaml`. It is
resolved **at run creation from the run commit** and embedded in the job, so editing it in an
earlier step does not change the run.

```yaml
name: local action
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: setup
        uses: ./.spindle/actions/setup
        with:
          version: "1.4"
      - run: make test
        env:
          TOOL: ${{ steps.setup.outputs.bin }}
      - uses: docker://alpine:3.20
        with:
          who: world
```

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

- `runs.using` is `composite` or `docker`; `node20` and friends are refused.
- Composite steps expand in place. A composite `run:` step **must** name `shell:`; `uses:` may
  be another local action or `docker://`, never a built-in or a marketplace action.
- `with:` values become `INPUT_<NAME>` (`dry run` → `INPUT_DRY_RUN`); an undeclared input is a
  named refusal; read inputs from the environment, not `${{ inputs.* }}`.
- Nesting is capped at 10 and cycles are refused.
- A `docker://` step (or docker action) runs as its own container with the workspace volume and
  the step environment plus `INPUT_*` — **never** the job token and never a repository secret.
  Its `$SPINDLE_OUTPUT` file is copied out and published under the calling step's id.
- Every `docker://` image goes through the same server image policy as a job container
  (`threads.image_allow_prefixes`, `threads.image_require_digest`).

### 4.5 Container options

`container.options` / `services.<id>.options` accept only:

`--cpus`, `--memory`/`-m`, `--memory-swap`, `--shm-size`, `--ulimit nofile=…`, `--init`,
`--read-only`, `--user`/`-u`, `--workdir`/`-w`.

Everything else — `--privileged`, `--cap-add`, `--device`, `--security-opt`, `--group-add`,
`--pid`, `--network`, any mount — is refused. Workflow-supplied `volumes` are refused outright.
The server refuses at run creation (failed job, red check) and the runner re-checks at plan time.

```yaml
name: sized
on: push
jobs:
  test:
    runs-on: linux
    container:
      image: golang:1.27
      options: "--cpus 2 --memory 2g --shm-size 512m"
    steps:
      - uses: actions/checkout
      - run: go test ./...
```

## 5. Jobs, dependencies, matrix, concurrency

### 5.1 `needs` and failure propagation

A job with no `if:` runs only when **every** dependency succeeded. A dependency that fails,
is cancelled, or is skipped blocks its dependents, and they are recorded `skipped` with a reason.

```yaml
name: pipeline
on: push
jobs:
  test:
    runs-on: linux
    steps:
      - run: make test
  package:
    needs: [test]
    runs-on: linux
    steps:
      - run: make dist
  notify:
    needs: [test, package]
    if: failure()
    runs-on: linux
    steps:
      - run: echo "something above failed"
```

A true `if:` runs the job even after a failure (`always()`, `failure()`,
`needs.test.result == 'failure'`). A false `if:` makes the job `skipped`, decided by the server
before a runner sees it. `if: false` is the simplest way to disable a job without deleting it.

A matrix job's dependents wait for **every** cell; `needs.<job>.result` is the worst cell's
state. Job outputs are the union of the cells, the last cell in job-id order winning a name
collision — and only a wholly successful dependency exposes any.

### 5.2 `matrix`

```yaml
name: matrix
on: push
jobs:
  test:
    runs-on: linux
    strategy:
      fail-fast: false
      matrix:
        go: ["1.26", "1.27"]
        os: [linux, darwin]
    container: golang:1.27
    steps:
      - uses: actions/checkout
      - name: test
        env:
          GO_VERSION: ${{ matrix.go }}
          TARGET_OS: ${{ matrix.os }}
        run: |
          echo "go $GO_VERSION on $TARGET_OS"
          go test ./...
```

- The matrix is a **simple product** of scalar axes. `include` and `exclude` are parse errors.
- Axes expand in **alphabetical order**; values in declaration order.
- The product is capped at **256 cells**.
- Every cell gets its own job id, check context and `MATRIX_<AXIS>` environment variable
  (`MATRIX_GO`).
- Names: `test (1.26, linux)` — unless the job `name` itself names a matrix axis
  (`name: cell-${{ matrix.go }}` renders `cell-1.26`).
- `fail-fast: true` (the default) cancels the other cells when one fails; a job-level
  `continue-on-error: true` suppresses that.
- A cell value is a scalar; a nested object is a parse error.

### 5.3 `if:` expressions

Supported in a **step** `if:`:

```
success()  failure()  always()  cancelled()   (canceled() is accepted)
github.ref  github.ref_name  github.sha  github.event_name  github.workflow
github.job  github.repository  github.run_id
matrix.<axis>  inputs.<name>  env.<NAME>
steps.<id>.outcome  steps.<id>.conclusion  steps.<id>.outputs.<name>
!  ==  !=  &&  ||  (  )  'literal'  "literal"  true  false
```

A **job** `if:` adds `needs.<job>.result` and `needs.<job>.outputs.<name>`; `steps.*` reads
empty there. Comparisons are string-wise and case-insensitive. An empty `if` means `success()`.

Anything else — `contains(...)`, `startsWith(...)`, `endsWith(...)`, `format(...)`, a nested
object, an unknown context — is an **evaluation error** and fails the step or job with the
message. It is never a silent skip. Write `if: github.ref == 'refs/heads/main'`, not
`if: startsWith(github.ref, 'refs/heads/')`.

Do not wrap the expression in `${{ }}`: write `if: inputs.dry_run != 'true'`. A bare
`${{ inputs.flag }}` span is substituted (handy for a boolean value), but an expression with
operators belongs in the native form.

### 5.4 `concurrency`

```yaml
name: deploy
on:
  push:
    branches: [main]
concurrency:
  group: deploy-${{ github.ref }}
  cancel-in-progress: true
jobs:
  ship:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: ./deploy.sh
```

- One in-flight **run** per group per repository. A run's sibling jobs do not serialize each
  other (the group belongs to the run).
- `cancel-in-progress: true` cancels the run holding the group and takes it.
- The group is expanded at run creation and is a bucket key. A reference that cannot exist is a
  **parse error**; a supported reference with no value for this run (an input without a
  default) fails run creation with `422`. It never falls back to literal text.
- Workflow-level groups may read `github.*` (the fields listed above), `inputs.*` and — at job
  level — the job's `matrix.*`. `env.*`, `steps.*`, `needs.*` and operators are not supported.
- `group: ci-${{ github.ref }}` is the usual idiom: without the ref, `cancel-in-progress: true`
  on a push to any branch cancels `main`.
- Enforcement is at **claim/admission**: a job whose group is held waits in the queue; no runner
  sees it until the group is free.

## 6. Environment, expressions, outputs, artifacts

### 6.1 `env` layering and built-ins

Lowest to highest: runner built-ins → workflow `env` → job `env` → container `env` → step `env`;
repository secrets are added last, by name. Every step sees:

| Variable | Value |
|---|---|
| `CI`, `GITHUB_ACTIONS`, `SPINDLE` | `true` |
| `GITHUB_REPOSITORY` / `SPINDLE_REPO` | `owner/repo` |
| `GITHUB_SHA` / `SPINDLE_SHA` | the run's commit |
| `GITHUB_REF` / `SPINDLE_REF` | `refs/heads/<branch>` (a PR: the base branch) |
| `GITHUB_REF_NAME` / `GITHUB_EVENT_NAME` | branch name / `push`, `pull_request`, `workflow_dispatch`, `schedule` |
| `GITHUB_WORKFLOW`, `GITHUB_JOB`, `GITHUB_RUN_ID` | workflow, job, run |
| `GITHUB_WORKSPACE` / `SPINDLE_WORKSPACE` | `/spindle/workspace` |
| `SPINDLE_RUN_ID`, `SPINDLE_JOB_ID`, `SPINDLE_CHECK`, `SPINDLE_ATTEMPT` | ids and the check context |
| `SPINDLE_TRUSTED` | `true` for a trusted run |
| `SPINDLE_SERVER` | the spindle origin |
| `MATRIX_<AXIS>` | the cell's value, axis uppercased |
| `SPINDLE_JOB_TOKEN` | the job token (§7.2) |
| `SPINDLE_OUTPUT` / `GITHUB_OUTPUT` | the step output file (§6.5) |
| `SPINDLE_ID_TOKEN_REQUEST_URL` / `…_TOKEN` (and `ACTIONS_*`) | OIDC exchange, `id-token: write` only |
| each repository/environment secret | by its own name, trusted runs only |

Not provided: `GITHUB_ENV`, `GITHUB_PATH`, `GITHUB_ACTOR`, `GITHUB_SERVER_URL`,
`GITHUB_EVENT_PATH`. A workflow migrating from elsewhere usually has to replace those (§9).

### 6.2 Where `${{ … }}` is substituted

| Site | `matrix.*` | `inputs.*` | `steps/needs.*.outputs.*` |
|---|---|---|---|
| step `name`, `run`, `if`, `shell`, `working-directory` | yes | yes | yes |
| step `env` values, `with` values | yes | yes | yes |
| job `outputs` values | yes | yes | yes |
| job `name`, `concurrency.group` | yes (job-level group) | yes | no |
| workflow/job/container/service `env`, `runs-on`, `container.image`, `environment` | no | no | no |

Concurrency groups additionally read `github.ref`, `github.ref_name`, `github.repository`,
`github.workflow`, `github.event_name`, `github.sha`, `github.run_id`.

### 6.3 Expressions in `if:` — the shape

```yaml
name: conditional steps
on: [push, pull_request]
jobs:
  lint:
    runs-on: linux
    if: github.event_name == 'push' || github.event_name == 'pull_request'
    steps:
      - id: vet
        run: go vet ./...
      - name: report
        if: steps.vet.outcome == 'failure'
        run: echo "vet failed; see above"
      - name: always clean up
        if: always()
        run: rm -rf /tmp/cache
```

`steps.<id>.outcome` is the raw result (`success`, `failure`, `cancelled`, `skipped`);
`.conclusion` is that result after `continue-on-error`. A step that did not run is recorded too.
Step ids are case-insensitive. `success()` / `failure()` describe the **job** status, so a
tolerated failure does not flip them.

### 6.4 Artifacts

Artifacts pass files between jobs of the **same run**. They travel over the authenticated job
channel; the job container never talks to the object store.

```yaml
name: artifacts
on: push
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
      - run: ls -R in
```

| Key | Action | Accepted | Default |
|---|---|---|---|
| `name` | both | letters, digits, `.`, `_`, `-`; not starting with `.`/`-`; ≤ 128 bytes; **unique per run** | `artifact` |
| `path` | upload | one or more patterns, newline/comma separated, workspace-relative | required |
| `if-no-files-found` | upload | `warn`, `error`, `ignore` | `warn` |
| `retention-days` | upload | `0` (run window) or `1`–`90`; may shorten, never extend | `0` |
| `path` | download | workspace-relative directory | the workspace |

Patterns: exact file, directory (recursive), `*`/`?` within a segment, `**` across segments,
`[ab]`, and `.` for the whole workspace. Only regular files are uploaded; symlinks are not
followed. The image needs `find` and `tar`. Caps: 256 MiB per file, 2 GiB per run, 1000 files
per artifact (configurable). Artifact **contents are data, not masked** — never write a secret
into one. Use an artifact name with no `/` or spaces; a matrix cell's name must be unique, e.g.
`dist-${{ matrix.target }}` with `target: [linux-amd64, linux-arm64]`.

### 6.5 Step and job outputs

Every step gets a writable output file at `$SPINDLE_OUTPUT` / `$GITHUB_OUTPUT`:

```yaml
name: outputs
on: push
jobs:
  build:
    runs-on: linux
    outputs:
      digest: ${{ steps.build.outputs.digest }}
    steps:
      - id: build
        run: |
          echo "digest=$(sha256sum go.mod | cut -d' ' -f1)" >> "$SPINDLE_OUTPUT"
      - run: echo "digest is ${{ steps.build.outputs.digest }}"
  publish:
    needs: [build]
    runs-on: linux
    if: needs.build.outputs.digest != ''
    steps:
      - run: echo "publishing ${{ needs.build.outputs.digest }}"
```

Format: `name=value`, or a heredoc `name<<EOF` … `EOF`. A repeated name keeps the last value.
A malformed line is a named step failure. Caps: 50 per step, 100 per job, 64 KiB per value,
1 MiB per job.

Reading is **fail-closed**: a reference to an unknown step id, or to an output the step did not
publish, fails the step naming the reference. Inside a native `if:`, an unpublished output of a
**known** step reads empty, so `== ''` works.

Only a **successful** job reports outputs. A failed/skipped/cancelled dependency exposes none.
Outputs are **not masked**; never put a secret in one.

## 7. Secrets, permissions, environments

### 7.1 Repository secrets

Managed in **Settings → Secrets** (admin) or `/{o}/{r}/api/threads/secrets` — write-only, never
readable back. Names match `[A-Za-z_][A-Za-z0-9_]{0,63}`; values 1 B – 64 KiB.

A secret is injected as an **environment variable of its own name** in every step of every job
of a **trusted** run:

```yaml
name: uses a secret
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: push to the registry
        run: |
          test -n "${REGISTRY_TOKEN:?REGISTRY_TOKEN is not set on this repository}"
          echo "$REGISTRY_TOKEN" | docker login -u "$REGISTRY_USER" --password-stdin registry.example.com
```

What an author must do:

- Read `$NAME`. **`${{ secrets.NAME }}` is not substituted** — write `env: TOKEN: ${{ secrets.TOKEN }}`
  and the value is the literal text.
- Expect nothing on a **fork** run (`secrets: {}`). Guard with `${VAR:?message}` so a missing
  secret is a clear failure.
- Masking is best effort: exact substring replacement in log lines, values under 4 bytes skipped.
  A value that is encoded, split or derived will print. Never echo a secret.
- Never put a secret in an **output** or an artifact: both are data and are not rewritten.

### 7.2 The job token and `permissions`

Every step receives `$SPINDLE_JOB_TOKEN`, scoped to one attempt of one job in one repository:

- always read of its own repository;
- push to that repository only when the **effective** `permissions` say `contents: write`
  (workflow merged with job, job wins, over `threads.default_permissions`); `contents: read`,
  `none`, or no `permissions` at all means no push;
- a fork run is pinned to `read` regardless of the file;
- a protected ref still refuses it, because the token is a `ci:<runner>` principal, not the
  user;
- it expires with the job and is revoked at completion; a push authenticated by it starts no
  workflow.

```yaml
name: push a build branch
on:
  push:
    branches: [main]
permissions:
  contents: write
jobs:
  publish:
    runs-on: linux
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - name: commit and push
        run: |
          git config user.name ci
          git config user.email ci@example.com
          date > build.txt
          git add build.txt
          git commit -q -m "ci: build"
          git push origin HEAD:refs/heads/ci-output
```

`fetch-depth: 0` is mandatory for the push. The token is already in the environment; do not
write it into `.git/config` or argv. If a push needs a different credential (one that *should*
trigger CI), use a secret instead — a job-token push deliberately starts nothing.

OIDC (`permissions: id-token: write`, trusted runs only) is available for cloud identity; its
env vars and claim set are [`threads.md` §5.5](threads.md#55-job-oidc-tokens-cloud-identity).

### 7.3 Approval gates

A fork pull request from a first-time contributor is created `pending_approval`: jobs written,
checks `pending`, nothing enqueued. A `maintain`+ user approves or denies on the run page (or
`POST …/api/threads/{run}/approve|deny`). It applies when all of these hold: the event is a fork
`pull_request`, the actor is not `maintain`/`admin`, the actor has no previously merged PR in
the repository, and `threads.approve_first_time_forks` is `true`. A same-repository PR is never
gated.

An author cannot approve their own fork run (they have no write access). If nobody approves, it
stays pending — there is no timeout and no auto-approval.

### 7.4 Deployment environments

An environment is a repository-scoped deployment target with required reviewers and its own
secrets:

```yaml
name: ship
on:
  workflow_dispatch:
jobs:
  ship:
    runs-on: linux
    environment:
      name: production
      url: https://example.com/
    steps:
      - uses: actions/checkout
      - run: ./deploy.sh
```

Create it in **Settings → Environments** (maintain+) with up to 10 reviewers, each holding
write or more (checked when saved). A job naming a **defined** environment with reviewers makes
the run `pending_approval` (`approval_kind: environment`) and nothing is enqueued until a listed
reviewer or a repo admin approves. A denied deployment fails only the deployment jobs. A job
naming an **undefined** environment fails with a message naming Settings → Environments.

Environment secrets override repository secrets of the same name, are delivered only to a job
that names the environment in a trusted run with an approved deployment, and a fork run never
receives them.

## 8. Schedules, retention, runner

### 8.1 Retention

`threads.retention` (default **720h = 30 days**) deletes a **terminal** run once its completion
(or creation, if it never started) is older than the window. Jobs, log frames and queue entries
go with it; **checks do not**, so an old required check still gates a merge. A queued, running
or `pending_approval` run is never swept, whatever its age. There is no "keep forever" spelling.

Artifacts expire at `min(run expiry, created + retention-days)`. The sweeper removes their
blobs and manifests with the run.

### 8.2 The runner, in brief

| Need | Answer |
|---|---|
| minting | `/admin/runners` (browser, host admin) → `wrg_` registration token, 15 min, with a repository policy (`all` or up to 50 `owner/name` / `owner/*` patterns) |
| start | `spindle --server URL runner serve --register wrg_… --token-file F --labels … --name … --capacity N` |
| labels | exact, comma-separated, matched as a **subset**, case-insensitive; default `self-hosted,linux,docker` |
| capacity | default `1`; N jobs share the daemon and are **not** isolated from each other |
| Docker | required; `DOCKER_HOST` names the daemon; the host socket needs `--allow-host-docker` |
| fork runs | off by default; a runner needs `--allow-fork-runs` **and** the repository allowed |
| images | `--image` (default `alpine/git:latest`, must carry `git`), `--image-prefix`, `--image-pull`, `--image-require-digest` |

**Protocol shape.** On start the runner declares its version, labels, capacity and fork opt-in,
then keeps `--capacity` long-poll `fetch` loops. A claim is a compare-and-swap on that
repository's queue entry, so the server keeps no worker counter and any number of runners can
serve the instance; fetching *is* the heartbeat (errors back off 1 s → 30 s), and a revoked
credential is a real `401` that stops the process. Log frames and the per-attempt token travel on
the same authenticated channel.

Operator depth — the dedicated-worker shape, mirroring, rootless/dind, revoke — starts at
[`threads.md` §8](threads.md#8-the-runner).

## 9. Recipes

### 9.1 Test-only CI for a Go repository

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
permissions:
  contents: read
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    timeout-minutes: 20
    steps:
      - uses: actions/checkout
      - run: go mod download
      - name: formatting
        run: test -z "$(gofmt -l .)"
      - run: go vet ./...
      - run: go test ./... -count=1
```

### 9.2 Matrix over Go versions, with a database service

```yaml
name: Build
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    strategy:
      fail-fast: false
      matrix:
        go: ["1.26", "1.27"]
    services:
      postgres:
        image: postgres:17
        env:
          POSTGRES_PASSWORD: ci
          POSTGRES_DB: test
        ports:
          - "5432"
    steps:
      - uses: actions/checkout
      - name: test
        env:
          GO_VERSION: ${{ matrix.go }}
          PGHOST: postgres
          PGUSER: postgres
          PGPASSWORD: ci
        run: |
          go version
          go test ./... -count=1
```

The service is reachable at its key as a hostname (`postgres:5432`, the container port). The
runner waits for a declared port or the image's `HEALTHCHECK`; 60 s without readiness fails the
job naming the service.

### 9.3 Pass a build between jobs

```yaml
name: build and verify
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - run: make release
      - uses: actions/upload-artifact
        with:
          name: release-out
          path: dist
          if-no-files-found: error
  verify:
    needs: [build]
    runs-on: linux
    steps:
      - uses: actions/download-artifact
        with:
          name: release-out
          path: dist
      - run: ./scripts/verify-release.sh dist
```

### 9.4 Build and push a container image

A job container has **no docker socket**, so `docker build` cannot reach a daemon. Use a
daemonless builder image (this is the same constraint a Forgejo runner has):

```yaml
name: image
on:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  image:
    runs-on: [self-hosted, linux, docker]
    container:
      image: quay.io/buildah/stable:latest
      options: "--ulimit nofile=65536:65536"
    steps:
      - uses: actions/checkout
      - name: build
        run: |
          mkdir -p /etc/containers /var/lib/containers /var/tmp/containers
          printf '[storage]\ndriver="vfs"\nrunroot="/var/tmp/containers"\ngraphroot="/var/lib/containers"\n' > /etc/containers/storage.conf
          buildah bud --storage-driver vfs --isolation=chroot -t registry.example.com/acme/app:"$GITHUB_SHA" .
      - name: push
        run: |
          test -n "${REGISTRY_TOKEN:?REGISTRY_TOKEN is not set}"
          buildah --storage-driver vfs login -u "$REGISTRY_USER" -p "$REGISTRY_TOKEN" registry.example.com
          buildah --storage-driver vfs push registry.example.com/acme/app:"$GITHUB_SHA"
```

Buildah's ability to create namespaces is a property of the **runner host** (kernel and daemon
configuration), not of this file. The runner forbids `--privileged`, `--cap-add` and
`--security-opt`, so a host that needs those cannot run this job; keep the build on the runner
host that can, or in the old CI until it is proven (§10.4).

### 9.5 Tag a release from a manual dispatch

Tag pushes never start a run, so a release is dispatched, and the job creates the tag:

```yaml
name: Release
on:
  workflow_dispatch:
    inputs:
      version:
        description: "Tag to create, e.g. v1.4.0"
        required: true
        type: string
permissions:
  contents: write
jobs:
  tag:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - name: validate and tag
        env:
          VERSION: ${{ inputs.version }}
        run: |
          case "$VERSION" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "not a vX.Y.Z tag: $VERSION" >&2; exit 1 ;; esac
          git tag -a "$VERSION" -m "release $VERSION"
          git push origin "refs/tags/$VERSION"
      - name: build release assets
        run: make release VERSION="${{ inputs.version }}"
```

The tag push lands; it creates no workflow run, which is expected. Publish the assets from this
same job, or dispatch a second workflow.

### 9.6 Scheduled maintenance

```yaml
name: Nightly maintenance
on:
  schedule:
    - cron: "17 2 * * *"
  workflow_dispatch:
permissions:
  contents: write
jobs:
  tidy:
    runs-on: linux
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - run: ./scripts/tidy.sh
      - name: commit if anything changed
        run: |
          git config user.name ci
          git config user.email ci@example.com
          if ! git diff --quiet; then
            git add -A
            git commit -q -m "chore: nightly tidy"
            git push origin HEAD:"$GITHUB_REF_NAME"
          fi
```

The `workflow_dispatch:` key with no value is also a trigger, so the same file can be run by
hand. A schedule fires only from the default branch; keep the file there.

### 9.7 A local composite action

```yaml
name: use the action
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: tools
        uses: ./.spindle/actions/tools
        with:
          version: "2.0"
      - run: |
          "${{ steps.tools.outputs.bin }}" --version
```

`.spindle/actions/tools/action.yml`:

```yaml-action
name: Tools
inputs:
  version:
    description: tool version
    required: false
    default: "1.0"
outputs:
  bin:
    description: the installed binary
    value: ${{ steps.install.outputs.bin }}
runs:
  using: composite
  steps:
    - id: install
      shell: bash
      run: |
        mkdir -p "$GITHUB_WORKSPACE/.tools"
        echo "bin=/spindle/workspace/.tools/tool-$INPUT_VERSION" >> "$SPINDLE_OUTPUT"
```

The action's `outputs:` may only read `steps.<id>.outputs.<name>` from its own steps; the
runner copies them under the calling step's id.

## 10. Migrating from Forgejo

The mapping is mechanical for tests, and there are four real gaps to plan around. Examples below
are derived from working repositories (Weave and Scriobh), not invented.

### 10.1 The translation table

| Forgejo | Threads | Notes |
|---|---|---|
| `on: push/pull_request/schedule` | same | same shapes; `branches` globs are `*` only |
| `on: push: tags: ["v*"]` | **nothing** | tag pushes never fire (§3.1). Use `workflow_dispatch` (§9.5). |
| `on: push: paths:` | **parse error** | filter inside a `run:` step, or split workflows by directory |
| `runs-on: docker` | `runs-on: [self-hosted, linux, docker]` | labels are exact; match a runner's declared set |
| `runs-on: ubuntu-latest` | a label your runner declares | `ubuntu-latest` is not special |
| `container: {image: …}` | same | digest pins are honoured and may be required by policy |
| `uses: actions/checkout@v4` | `uses: actions/checkout` | built-in; `@v4` is accepted and ignored |
| `with: fetch-depth: 0` | same | `fetch-tags` is **not** supported: `git fetch --tags` in a step |
| `actions/setup-go`, `setup-node` | base image (`golang:1.27`, `node:22`), or a `run:` step | no marketplace actions |
| `actions/cache` | **nothing** | no cache; bake dependencies into an image |
| `secrets.NAME` | `$NAME` (already an env var) | `${{ secrets.NAME }}` is not substituted |
| `secrets.GITHUB_TOKEN` | `$SPINDLE_JOB_TOKEN` | automatic, scoped to the job |
| `github.event_name`, `github.ref`, `github.sha`, `github.repository`, `github.ref_name` | same names in `if:`; `$GITHUB_*` in scripts | no `${{ github.* }}` in `run:` |
| `github.actor` | **nothing** | no actor context; use a secret or a fixed robot name |
| `$GITHUB_SERVER_URL` | `$SPINDLE_SERVER` | the repository is `$GITHUB_REPOSITORY` on both |
| `github.event.inputs.x` | `inputs.x` | dispatch inputs only |
| `$GITHUB_OUTPUT` | same (`$SPINDLE_OUTPUT`) | `name=value` or heredoc |
| `$GITHUB_ENV` | **nothing** | use job `outputs` + `needs.<job>.outputs.<name>`, or set `env:` on each step |
| `$GITHUB_PATH` | **nothing** | use absolute paths or `export PATH=…` in the same step |
| `if: startsWith(github.ref, 'refs/tags/')` | `if: github.ref == 'refs/tags/…'` | no `startsWith`; tags do not fire anyway |
| `strategy.matrix.include` | a simple product, or one job per case | `include`/`exclude` are parse errors |
| `strategy.max-parallel` | **nothing** | no equivalent; concurrency groups cap runs, not cells |
| job `outputs` + `needs.*.outputs` | same | only a successful job reports |
| `environment:` + reviewers | same | created in Settings → Environments |
| `permissions:` | same for `contents` / `id-token` | other keys parse but do nothing |

### 10.2 Weave: tests, then a daemonless image

**Original** (`.github/workflows/ci.yml`, excerpt):

```yaml-forgejo
jobs:
  test:
    runs-on: docker
    container:
      image: golang:1.27@sha256:01aa404fa56b604b6f48e90c1d6e2c79fe5cd78f9592f78d80303fbe171cdcc0
    steps:
      - name: Checkout
        env:
          FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: eval "$CHECKOUT"
      - run: go vet ./...
      - run: go test ./... -count=1
```

The `CHECKOUT` env block (a plain-git clone with `http.extraHeader`) exists because the job image
has no Node for a JavaScript checkout action. Threads' checkout is built in and runs in the job
container, so it disappears.

**Threads equivalent:**

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container:
      image: golang:1.27@sha256:01aa404fa56b604b6f48e90c1d6e2c79fe5cd78f9592f78d80303fbe171cdcc0
    steps:
      - uses: actions/checkout
      - run: go vet ./...
      - run: go test ./... -count=1
```

Translations: `runs-on: docker` → the three default labels; the digest-pinned image is
unchanged; the custom checkout disappears; `secrets.GITHUB_TOKEN` is not needed (the job token
is automatic).

**Original** (the image job):

```yaml-forgejo
jobs:
  package:
    runs-on: docker
    needs: test
    if: github.event_name == 'push'
    container:
      image: quay.io/buildah/stable:latest@sha256:e5a33092657823b0830186ea525054cf4cdeef29b6a44833444c59d86f561f29
    steps:
      - name: Checkout
        env:
          FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          if command -v apk >/dev/null 2>&1; then apk add --no-cache git >/dev/null; fi
          eval "$CHECKOUT"
      - name: Build
        run: |
          buildah bud --storage-driver vfs --isolation=chroot --build-arg "VERSION=$GITHUB_SHA" -t git.example.com/acme/weave:latest .
```

**Threads equivalent:**

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    steps:
      - uses: actions/checkout
      - run: go test ./... -count=1
  package:
    runs-on: [self-hosted, linux, docker]
    needs: [test]
    if: github.event_name == 'push'
    container:
      image: quay.io/buildah/stable@sha256:e5a33092657823b0830186ea525054cf4cdeef29b6a44833444c59d86f561f29
      options: "--ulimit nofile=65536:65536"
    steps:
      - uses: actions/checkout
      - name: build
        run: |
          mkdir -p /etc/containers /var/lib/containers /var/tmp/containers
          printf '[storage]\ndriver="vfs"\nrunroot="/var/tmp/containers"\ngraphroot="/var/lib/containers"\n' > /etc/containers/storage.conf
          buildah bud --storage-driver vfs --isolation=chroot \
            --build-arg "VERSION=$GITHUB_SHA" -t registry.example.com/acme/weave:latest .
      - name: push
        run: |
          test -n "${REGISTRY_TOKEN:?REGISTRY_TOKEN is not set}"
          buildah --storage-driver vfs login -u "$REGISTRY_USER" -p "$REGISTRY_TOKEN" registry.example.com
          buildah --storage-driver vfs push registry.example.com/acme/weave:latest
```

Translations: `needs: test` → `needs: [test]`; `github.event_name == 'push'` is supported at job
level; the `apk add git` block disappears with the built-in checkout; `secrets.REGISTRY_*` become
the secrets `REGISTRY_USER`/`REGISTRY_TOKEN` read as `$REGISTRY_*`. `--ulimit` is the one sizing
option buildah needs that the allowlist permits. The **no-equivalent** here is the Forgejo runner's
capability grant (SYS_ADMIN / apparmor / nofile at the runner level): in Threads the job container
cannot ask for it, so the host must already permit daemonless builds, otherwise the image job
stays in Forgejo.

**Original** (`publish-github.yml`, the dispatch and checkout part):

```yaml-forgejo
on:
  workflow_dispatch:
    inputs:
      bump:
        type: choice
        options: [patch, minor, major, none]
        default: patch
      tag:
        required: false
        default: ''
        type: string
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
          fetch-tags: true
      - name: publish
        env:
          GITHUB_TOKEN: ${{ secrets.PUBLISH_GITHUB_TOKEN }}
          BUMP: ${{ github.event.inputs.bump }}
        run: ./scripts/publish-to-github.sh --bump "$BUMP"
```

**Threads equivalent:**

```yaml
name: publish to github
on:
  workflow_dispatch:
    inputs:
      bump:
        description: version bump
        type: choice
        options: [patch, minor, major, none]
        default: patch
      tag:
        description: exact tag used when bump is none
        type: string
        required: false
        default: ""
jobs:
  publish:
    runs-on: [self-hosted, linux, docker]
    container: alpine/git:latest
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - name: fetch tags
        run: git fetch --tags --force origin
      - name: publish
        env:
          BUMP: ${{ inputs.bump }}
          INPUT_TAG: ${{ inputs.tag }}
          TARGET: crueber/weave
        run: |
          test -n "${PUBLISH_GITHUB_TOKEN:?PUBLISH_GITHUB_TOKEN is not set}"
          chmod +x scripts/publish-to-github.sh
          if [ "${BUMP:-none}" = "none" ]; then
            VERSION="${INPUT_TAG:?set the tag input when bump is none}"
          else
            latest=$(git tag --list 'v[0-9]*' | sort -V | tail -1)
            [ -n "$latest" ] || latest=v0.0.0
            version=${latest#v}
            MA=${version%%.*}; rest=${version#*.}; MI=${rest%%.*}; PA=${rest##*.}
            case "$BUMP" in
              major) MA=$((MA + 1)); MI=0; PA=0 ;;
              minor) MI=$((MI + 1)); PA=0 ;;
              patch) PA=$((PA + 1)) ;;
              *) echo "unknown bump: $BUMP" >&2; exit 1 ;;
            esac
            VERSION="v$MA.$MI.$PA"
          fi
          ./scripts/publish-to-github.sh --repo "$TARGET" --version "$VERSION" --apply
```

Translations: `ubuntu-latest` → runner labels (and an image that actually has the tools);
`fetch-tags: true` → a `git fetch --tags` step; `github.event.inputs.bump` → `inputs.bump`;
`secrets.PUBLISH_GITHUB_TOKEN` → `$PUBLISH_GITHUB_TOKEN`. The script's `GITHUB_TOKEN` variable
name is its own (a GitHub PAT secret), not the platform's token — keep the secret name and read
it from the environment.

### 10.3 Scriobh: multi-job tests and a matrix release

**Original** (`.github/workflows/ci.yml`, the editor job):

```yaml-forgejo
jobs:
  editor:
    runs-on: docker
    container:
      image: node:22-bookworm
    steps:
      - name: Checkout
        env:
          FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: eval "$CHECKOUT"
      - name: Build and test the editor bundle
        run: |
          cd web/editor
          npm ci --no-audit --no-fund
          npm test
```

**Threads equivalent:**

```yaml
name: CI
on:
  push:
    branches: [main, "phase-*"]
  pull_request:
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    steps:
      - uses: actions/checkout
      - run: go vet ./...
      - run: go test ./... -count=1
  editor:
    runs-on: [self-hosted, linux, docker]
    container: node:22-bookworm
    steps:
      - uses: actions/checkout
      - name: Build and test the editor bundle
        run: |
          cd web/editor
          npm ci --no-audit --no-fund
          npm test
```

Translations: `branches: [main, 'phase-*']` is unchanged (a `*` glob); the Node image is
unchanged; checkout and the token handling disappear. `npm ci` needs no cache action — a cold
run is slower, which is the accepted trade (no `actions/cache`).

**Original** (`.github/workflows/release.yml`, release job):

```yaml-forgejo
on:
  push:
    tags: ["v*"]
  workflow_dispatch:
    inputs:
      bump:
        type: choice
        options: [none, patch, minor, major]
        default: none
      tag:
        required: false
jobs:
  prepare:
    runs-on: docker
    container:
      image: ubuntu:24.04
    outputs:
      tag: ${{ steps.tag.outputs.tag }}
    steps:
      - name: Resolve tag
        id: tag
        env:
          BUMP: ${{ github.event.inputs.bump }}
          INPUT_TAG: ${{ github.event.inputs.tag }}
        run: |
          echo "tag=$TAG" >> "$GITHUB_OUTPUT"
          echo "TAG=$TAG" >> "$GITHUB_ENV"
```

**Threads equivalent:**

```yaml
name: Release
on:
  workflow_dispatch:
    inputs:
      bump:
        description: version bump
        type: choice
        options: [none, patch, minor, major]
        default: none
      tag:
        description: explicit tag used when bump is none
        type: string
        required: false
        default: ""
permissions:
  contents: write
jobs:
  prepare:
    runs-on: [self-hosted, linux, docker]
    container: ubuntu:24.04
    outputs:
      tag: ${{ steps.tag.outputs.tag }}
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - name: tools
        run: apt-get update -qq && apt-get install -y -qq curl git jq >/dev/null
      - name: fetch tags
        run: git fetch --tags --force origin
      - name: resolve tag
        id: tag
        env:
          BUMP: ${{ inputs.bump }}
          INPUT_TAG: ${{ inputs.tag }}
        run: |
          set -eu
          if [ "${BUMP:-none}" != "none" ]; then
            latest=$(git tag --list 'v[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)
            [ -n "$latest" ] || latest=v0.0.0
            version=${latest#v}; major=${version%%.*}; rest=${version#*.}; minor=${rest%%.*}; patch=${rest##*.}
            case "$BUMP" in
              major) major=$((major + 1)); minor=0; patch=0 ;;
              minor) minor=$((minor + 1)); patch=0 ;;
              patch) patch=$((patch + 1)) ;;
              *) echo "unknown bump: $BUMP" >&2; exit 1 ;;
            esac
            TAG="v$major.$minor.$patch"
          elif [ -n "${INPUT_TAG:-}" ]; then
            TAG="$INPUT_TAG"
          else
            echo "choose a bump or an explicit tag" >&2; exit 1
          fi
          case "$TAG" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "release tags are vX.Y.Z: $TAG" >&2; exit 1 ;; esac
          echo "tag=$TAG" >> "$SPINDLE_OUTPUT"
```

Translations and the two hard parts:

- `on.push.tags: ["v*"]` has **no equivalent** — a tag push never fires. The tag is created by
  a dispatched job (§9.5) instead, and that job's tag push starts nothing.
- `$GITHUB_ENV` has **no equivalent**. `TAG` travels as the job output `tag`, and every later
  step sets `env: TAG: ${{ needs.prepare.outputs.tag }}`.
- `$GITHUB_OUTPUT` is supported unchanged.

**Original** (the matrix build and upload):

```yaml-forgejo
jobs:
  cli:
    needs: prepare
    runs-on: docker
    container:
      image: golang:1.27.4
    strategy:
      fail-fast: false
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
          - { goos: darwin, goarch: arm64 }
          - { goos: windows, goarch: amd64 }
    steps:
      - name: Checkout
        run: eval "$CHECKOUT"
      - name: Build
        env:
          TAG: ${{ needs.prepare.outputs.tag }}
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
        run: |
          CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -o "dist/scriobh-$GOOS-$GOARCH" ./cmd/scriobh
```

**Threads equivalent (simple product, no `include`):**

```yaml
name: Release
on:
  workflow_dispatch:
    inputs:
      bump:
        description: version bump
        type: choice
        options: [none, patch, minor, major]
        default: none
jobs:
  prepare:
    runs-on: [self-hosted, linux, docker]
    container: ubuntu:24.04
    outputs:
      tag: ${{ steps.tag.outputs.tag }}
    steps:
      - id: tag
        env:
          INPUT_TAG: ${{ inputs.tag }}
        run: |
          # The bump/tag arithmetic from the Forgejo step is unchanged shell.
          # GITHUB_ENV has no equivalent: TAG leaves as this job output instead.
          TAG="${INPUT_TAG:-v0.1.0}"
          echo "tag=$TAG" >> "$SPINDLE_OUTPUT"
  cli:
    needs: [prepare]
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    strategy:
      fail-fast: false
      matrix:
        target: [linux-amd64, linux-arm64, darwin-arm64, windows-amd64]
    steps:
      - uses: actions/checkout
      - name: build
        env:
          TAG: ${{ needs.prepare.outputs.tag }}
          TARGET: ${{ matrix.target }}
        run: |
          set -eu
          GOOS=${TARGET%-*}
          GOARCH=${TARGET##*-}
          mkdir -p "dist/$TARGET"
          suffix=""
          [ "$GOOS" = windows ] && suffix=".exe"
          CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -trimpath \
            -ldflags "-s -w -X main.version=$TAG" \
            -o "dist/$TARGET/scriobh$suffix" ./cmd/scriobh
          tar -czf "dist/scriobh-$TAG-$TARGET.tar.gz" -C "dist/$TARGET" .
      - uses: actions/upload-artifact
        with:
          name: dist-${{ matrix.target }}
          path: dist/scriobh-*.tar.gz
          if-no-files-found: error
```

The `include:` matrix becomes a **single `target` axis** whose values encode OS and arch, and
the step splits them. That is the only supported rewrite: `include`/`exclude` are parse errors.
The four cells are exactly the four original combinations, and each artifact name is unique per
run because `target` is part of it.

Not translated, deliberately: the Forgejo release-upload script talks to the Forgejo API. Threads
releases are spindle's own API, and the safe migration order keeps publishing where it is until
the spindle build job is proven (§10.4).

### 10.4 What has no equivalent, and what to do

| Construct in the real workflows | Why it cannot translate | Workaround |
|---|---|---|
| `on.push.tags` / a tag-triggered release | tag pushes create no run | dispatch with a `version` input; create the tag in the job (§9.5) |
| `on.push.paths` | not in the parser | filter in a step with `git diff --name-only`, or split by directory |
| `strategy.matrix.include` | parser refuses it | single axis of composite values, split in the step; or one job per case |
| `strategy.max-parallel` | not in the parser | no equivalent; concurrency groups cap runs, not cells |
| `$GITHUB_ENV` | not implemented | job `outputs` + `needs.<job>.outputs.<name>` |
| `$GITHUB_PATH` | not implemented | absolute paths, or `export PATH=…` in the same step |
| `github.actor` | no actor context | a fixed robot user secret, or the repository name |
| `${{ secrets.* }}` inside `run:` | not substituted | the secret is already `$NAME` |
| `secrets.GITHUB_TOKEN` as a PAT for the host API | the job token is not the user's token | declare `permissions: contents: write` for git; use a real secret for API writes the job token's scope does not cover |
| `actions/cache` | no cache engine | bake layers into a base image, or accept a cold workspace |
| `actions/setup-go`, `setup-node`, `setup-buildx` | no marketplace | choose the base image; install in a `run:` step |
| `docker build` in a job | job containers have no docker socket | buildah (or another daemonless builder) as the job image; or keep the build on Forgejo |
| `--privileged`, `--cap-add`, `--security-opt` for a builder | runner allowlist refuses them | the runner host must permit it without per-job flags; otherwise keep the job on Forgejo |
| Forgejo release/asset API calls | spindle's API is different | publish from the spindle releases API, or keep publishing in Forgejo during migration |

### 10.5 Safe migration order

1. **Tests first.** Copy the test job, replace the custom checkout with `actions/checkout`, and
   run it beside the old CI. It is read-only, needs no secrets and proves labels, image, checkout
   reachability and duration.
2. **Add the matrix and services.** Prove the cells and the database service, still read-only.
3. **Move artifact-producing jobs.** Build, upload, download and verify inside Threads; no
   credentials yet.
4. **Keep publishing where it is.** Image pushes and release uploads stay in Forgejo until the
   spindle job has produced the same bytes twice. Then move the credential, run both once in
   parallel, and compare the digests — not the logs.
5. **Retire the old workflow** only after a release has shipped from spindle and the rollback is
   "re-enable the old file", which is one revert.

Never start by moving the release job: it is the one with the credentials, the tag semantics
this platform does not have, and the least reversible failure.

## 11. Troubleshooting

Start at the first row that describes what you see; each branch names the evidence to read.

| Symptom | Cause | Fix |
|---|---|---|
| **No run at all after a push** | no workflow file at that commit; `on:` does not match; the file failed to parse; the ref was a tag or a delete; the push used a job token | Threads → **Workflows** lists files at the default tip with their parse errors; tags/deletes never fire; a job-token push is suppressed |
| **A file is rejected** | unknown key, bad type, unsupported cron, `paths`, `matrix.include`, both `run` and `uses`, a bad `needs:`/step id | the error names the file/key; fix that file only — the others keep running |
| **Run stays `queued`, jobs `not started`** | no active runner; labels are not a superset of `runs-on`; a fork run with no `--allow-fork-runs`; capacity full; a concurrency group is held | `/admin/runners`: an `active` runner with a recent last-seen and the labels; the run page shows `runs-on`; check the other run holding the group |
| **Job failed before any step** | image refused (`threads.image_allow_prefixes` / `image_require_digest`), a bad container option, an undefined `environment`, an unresolvable local action | the job's `reason` names the field and value; fix the file or the policy |
| **Job fails at checkout** | image has no `git`; the clone URL is unreachable from the container; the commit does not exist | use an image with `git` (default `alpine/git`); read the checkout URL and the reachability diagnostic; set `server.public_url` |
| **Push from a job refused** | shallow clone; `permissions: contents: read`; protected ref; non-fast-forward | add `fetch-depth: 0`, declare `contents: write`, push a fast-forward, or relax the policy |
| **A secret is empty** | the run is a fork/untrusted run; no `server.secrets_key`/`session_secret` (API `503`); wrong name; used `${{ secrets.X }}` | follow the `untrusted` chip; set the key; names are exact and case-sensitive; read `$X` |
| **`${{ secrets.X }}` prints literally** | secrets are env vars, not template contexts | use `$X`; guard with `${X:?message}` |
| **Step says "did not publish the output"** | the step failed/was skipped/ran later, or wrote a different name | check `steps.<id>.outcome` and the exact name in `$SPINDLE_OUTPUT` |
| **`needs.<job>.outputs.x` is empty** | the dependency did not succeed, or its `outputs:` template omits the name | the job record's `outputs` shows what was reported |
| **Artifact step: "no files were found"** | the pattern matched nothing (typo, outside the checkout, wrong `working-directory`) | the log names the patterns; `if-no-files-found: warn` makes it non-fatal |
| **Artifact step names `find`/`tar`** | the image has no busybox/coreutils | use an image with both, or archive in a `run:` step |
| **Upload over the per-file/per-run cap** | 256 MiB/file, 2 GiB/run | split or exclude; raise `threads.max_artifact_bytes_*` |
| **`image pull failed` with a `spindle:` diagnostic** | the mirror is unreachable, refused the credential, or lacks the repository | read §8.6 of [`threads.md`](threads.md#86-mirroring-job-images): daemon DNS/TLS, `$DOCKER_CONFIG`, mirror repo key |
| **Run is `pending_approval`** | fork gate, or an environment with required reviewers | a maintainer/reviewer approves on the run page; or `threads.approve_first_time_forks = false` |
| **Approval is stuck** | nobody decides; there is no timeout and no auto-approval | a listed reviewer or repo admin must decide; a deleted environment fails the deployment closed |
| **Schedule never fires** | bad cron (file rejected); not on the default branch; no `maintain` role; the tick already fired | fix and merge to the default branch; run a maintainer; use dispatch for "now" |
| **Required check stays `pending`** | its job was never claimed | a runner must claim it, or the merge stays blocked by design |
| **Log panel says "loading log…"** | JavaScript is required for the live log; an expired session shows 401 | reload; `GET …/api/threads/{run}/jobs/{job}/log?accept=json` returns one window |
| **Cancel is not instant** | a running job acknowledges through its log channel | wait one heartbeat (~25 s); then it reports `cancelled` and containers are removed |

For messages this table does not cover, the authoritative list is
[`threads.md` §10](threads.md#10-troubleshooting).

## 12. Differences from what you may know

If you come from a hosted CI system, these are the surprises, in the order they usually bite:

1. **No marketplace.** `uses:` resolves exactly four things: the three built-ins, `./path`, and
   `docker://image`. A toolchain is a base image or a `run:` step.
2. **No cache.** Every job starts from a cold workspace. There is no `actions/cache`.
3. **Secrets are environment variables, not template values.** `${{ secrets.X }}` is literal.
4. **`${{ }}` is not generalized.** It substitutes matrix/inputs/outputs in the documented sites
   only; scripts read `$GITHUB_*` and `$NAME`.
5. **The expression language is small.** No `contains`, `startsWith`, `format`, no object
   filters. An unsupported expression fails the step or job rather than skipping it.
6. **Tag pushes do not trigger.** Releases are dispatched.
7. **Jobs do not get a Docker socket.** Building an image means a daemonless builder or a
   prebuilt image.
8. **Matrices are simple products.** No `include`/`exclude`, no `max-parallel`; 256 cells max.
9. **Labels are exact and subset-matched.** There is no `ubuntu-latest` unless a runner declares
   it.
10. **Fork PRs are read-only by policy.** No secrets, a read-only token, the base branch's
    workflow file, and possibly an approval gate — the file cannot opt out.
11. **`permissions` is narrow.** Only `contents` and `id-token` change anything; the rest parse
    and do nothing.
12. **Everything is in the bucket.** There is no queue server and no local state; a restart
    loses warmth, not runs.

## 13. Where the depth lives

| Topic | Reference |
|---|---|
| every trigger edge, filter and cron rule | [`threads.md` §3.2](threads.md#32-on), [§6](threads.md#6-triggers-in-detail) |
| containers, services, network, checkout reachability | [`threads.md` §4](threads.md#4-containers-services-and-checkout) |
| secrets, job token, fork gate, OIDC, environments | [`threads.md` §5](threads.md#5-secrets-and-permissions) |
| checks and merge gating | [`threads.md` §7](threads.md#7-checks-and-merge-gating) |
| runner flags, tenancy, capacity, isolation, mirrors | [`threads.md` §8](threads.md#8-the-runner) |
| cancel/rerun, retention, maintainer tasks | [`threads.md` §9](threads.md#9-operating-it) |
| design, protocol and frozen wire shapes | [`features/15_threads.md`](features/15_threads.md) |
