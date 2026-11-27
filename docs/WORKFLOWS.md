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
| `concurrency` | string or `{group, cancel-in-progress}` | none | one running and one pending run per group per repository (§5.4) |
| `permissions` | mapping, or `read-all` / `write-all` | instance `threads.default_permissions` (`read`) | the job token's authority: `contents`, `issues`, `pull-requests`, `id-token`; any other scope is a parse error (§7.2) |
| `enable-openid-connect` | bool | false | Forgejo's spelling for the OIDC grant; the same grant as `permissions: id-token: write` (§7.2) |
| `jobs` | map[string]job | **required** | at least one job |

### 2.2 `on`

| Trigger | Accepted keys | Fires when |
|---|---|---|
| `push` | `branches`, `branches-ignore`, `tags`, `tags-ignore`, `paths`, `paths-ignore` | a branch push matching `branches`, a tag push matching `tags`, and (when declared) a changed-file set matching `paths`; a trigger that names only `tags` does **not** fire on a branch push (§3.1) |
| `pull_request` | `branches`, `branches-ignore`, `paths`, `paths-ignore`, `types` | the listed lifecycle actions on a PR targeting a matching branch whose changed files match `paths`; the run is the simulated merge commit (§3.2) |
| `pull_request_target` | same as `pull_request` | the same PR edges, with the **base branch's** file and base secrets; off per repository by default (§3.2) |
| `repository_dispatch` | `types` | `POST …/api/threads/dispatches` with a write role (§3.7) |
| `workflow_run` | `workflows`, `types`, `branches`, `branches-ignore` | another workflow run's lifecycle; omitted `types` means `completed` (§3.7) |
| `create` / `delete` | none | a branch or tag created/deleted by a push (§3.7) |
| `deployment` / `deployment_status` | none | a Threads deployment record created or decided (§3.7) |
| `workflow_dispatch` | `inputs` | a manual dispatch from the UI, API or CLI |
| `workflow_call` | `inputs`, `secrets`, `outputs` | a reusable workflow's interface; the key never starts a run itself (§4.5) |
| `release` | `types` | a release lifecycle action emitted by the releases service (§3.6) |
| `issues` | `types` | an issue lifecycle action emitted by the issues service (§3.6) |
| `issue_comment` | `types` | a new issue comment (the only emitted action) (§3.6) |
| `schedule` | a list of `- cron: "…"`, optional `timezone:` | the maintainer's cron sweep, default branch only |

`branches`/`tags`/`paths` and their `*-ignore` twins are exclusion/include pairs: a trigger may not
name both forms of the same dimension (use the include form with `!` to mix). All four pattern lists
use the dialect in §3.1.

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
| `type` | `string` \| `boolean` \| `choice` \| `number` \| `environment` | default `string`; anything else is a parse error |
| `required` | bool | a missing required value is `422` naming the input |
| `default` | scalar | kept as text; a boolean default reads `"true"` / `"false"`, a number default its decimal text |
| `options` | list of strings | **required** for `choice`; a value outside the list is `422`. An `environment` input's options are the repository's environment names, resolved at dispatch time and rendered as a picker |

`number` accepts a decimal number (the value is carried as its authored text, which is also what
`github.event.inputs.<name>` reads). `environment` must name an existing repository environment;
`${{ inputs.environment }}` is the name. `inputs.<name>` and `github.event.inputs.<name>` are the
same value for every type.

### 2.3 A job

| Key | Type | Default | What it does |
|---|---|---|---|
| `name` | string | job key | display name and the `<job>` segment of the check context; `${{ matrix.* }}` / `${{ inputs.* }}` are filled in |
| `runs-on` | label or list | **required** unless `uses` | the runner must declare **every** label, case-insensitive and exact (no wildcards). The hosted Linux labels (`ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, `ubuntu-20.04`) map onto `self-hosted` + `linux` (§8.2); `windows-*`/`macos-*` are unmapped |
| `enable-openid-connect` | bool | false | job-level spelling of the OIDC grant (§7.2) |
| `container` | image string or mapping | runner `--image` (`alpine/git:latest`) | one container for the job; fields below |
| `services` | map[id]service | none | sidecars on the job network, reachable at their id |
| `env` | map[string]string | empty | overrides workflow `env` |
| `needs` | one job key or a list | none | `needs: test` and `needs: [test]` are the same; must exist and be acyclic; a matrix job's dependents wait for **every** cell |
| `if` | expression | `success()` over `needs` | evaluated by the server before the job is claimable (§5.3) |
| `timeout-minutes` | int | unbounded | cancels the whole job and removes its containers |
| `continue-on-error` | bool | false | the job stays `failure` but the **run** conclusion follows the other jobs; fail-fast does not fire from it, `needs.<job>.result` reads `success` and dependents run. Forgejo documents this key as *ignored* — a recorded divergence (D-4), GitHub's semantics are kept |
| `environment` | name or `{name, url}` | none | deployment target with reviewers and secrets (§7.4) |
| `outputs` | map[name]template | none | values a dependent job reads as `needs.<job>.outputs.<name>` (§6.5) |
| `strategy.matrix` | axes, plus `include` / `exclude` lists | none | product of scalar axes with GitHub's `include`/`exclude` merge rules (§5.2) |
| `strategy.fail-fast` | bool | `true` | a failed cell cancels its siblings |
| `steps` | list | **required** unless `uses` | §4 |
| `concurrency` | group or mapping | workflow default | job-level wins |
| `permissions` | mapping or shorthand | workflow default | job-level wins |
| `uses` / `with` / `secrets` | string / map / map | — | call a reusable workflow (`./.spindle/workflows/<name>.yml`, or a pinned `owner/repo/.spindle/workflows/<name>.yml@<40-hex sha>` on this instance); the callee must declare `on.workflow_call`, whose typed inputs, secrets and outputs are its interface (§4.5). |

**Structural fields and the env layers above a step take literal values only.** The runner reads a
workflow/job/container/service `env` value verbatim — only a **step** `env:` is substituted — and
`runs-on`, `container.image`, `services.<id>.image` and `environment` (its `name` and `url`) are
chosen before a matrix cell or dispatch input exists. A `${{ … }}` span in any of them is a
**parse error naming the field** (§6.2), never a literal that queues the job against a label no
runner declares or reaches the process environment unexpanded.

Container mapping keys: `image` (required in practice), `env`, `ports`, `command`, `entrypoint`,
`options`, `volumes`. Only `image`, `env` and `options` are applied; `ports`, `command` and
`entrypoint` are accepted and not applied — the runner owns the container's lifetime and mounts only
its workspace volume. `volumes` is refused by name unless the runner's operator allow-listed the
host path (`--container-valid-volume`, §4.4a). `options` is checked against a small allowlist
(§4.4a), and may carry docker's `--health-*` readiness flags for a service (§4.2 of the reference).

A service entry takes `image`, `env`, `ports`, `command` (Forgejo spelling `cmd`),
`entrypoint`, `options`, `volumes`. `credentials` is **refused by name**: a registry password in a
workflow would travel in the job payload and in docker argv. The pull identity belongs to the runner
— its daemon's config, an `--image-prefix` mirror, or the operator's `--registry-auth-file` — and the
runner logs in with the password on stdin into a private `DOCKER_CONFIG`, never argv and never a log
frame.

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
| `shell` | `sh` (default), `bash`, `python`, `python3`, `pwsh`/`powershell`, `cmd`, or a custom `<command> {0}` template | `sh -e` | the interpreter; `sh` runs `sh -e -c`, `bash` runs `bash --noprofile --norc -eo pipefail -c` (falling back to `sh -e` with a log line when the image has no bash), a custom template is split into argv words with `{0}` replaced by a script file (§4.1) |
| `working-directory` | path | the workspace | relative joins the workspace; absolute wins |
| `continue-on-error` | bool | false | step may fail without failing the job (result still `failure`) |
| `timeout-minutes` | int | remaining job time | on expiry the step's container (a foreground step) or its own process group (a background step) is killed and the step reports `timed out` |
| `background` | bool | false | start asynchronously in its own session; needs an `id` (§4.6) |
| `wait` / `wait-all` | background step id or `all` / bool | — | wait for one or every outstanding background step; the wait fails if it failed |
| `cancel` | background step id or `all` | — | terminate a background step's process group (SIGTERM then SIGKILL) |
| `parallel` | list of steps | — | run the members concurrently as one unit (§4.6) |

A failed step without `continue-on-error` skips the rest of the job (each recorded `skipped`).

### 2.5 What is deliberately not accepted

**Refused** — a parse error naming the key, so the file creates no runs:

- `defaults:` (workflow, job or step);
- an `on.pull_request.types`/`on.release.types`/`on.issues.types`/`on.issue_comment.types` value the
  owning service never emits, or an empty `types: []` list;
- naming both an include filter and its `*-ignore` twin (`branches` + `branches-ignore`,
  `paths` + `paths-ignore`, `tags` + `tags-ignore`);
- `strategy.max-parallel`; an `include`/`exclude` entry whose value is a list or a nested object;
- a `${{ … }}` reference outside the documented vocabulary — an unknown context, an unknown
  `github.*` field, an undeclared matrix key or dispatch input, a malformed `secrets.<name>`, an
  operator in a non-`if` site — is a parse error naming the supported set (§6.2);
- a `${{ … }}` span in a **structural** field — `runs-on` (any accepted shape), `container.image`,
  `services.<id>.image`, `environment` (string form, `name` and `url`) — or in a
  workflow/job/container/service `env` value, is a parse error naming the field; these take a
  literal (a step `env:` is the one env layer that substitutes), or one job per value (§6.2);
- an `if:` function outside `success`, `failure`, `always`, `cancelled`, `startsWith`,
  `contains`, `format` (§5.3);
- a step with both `run` and `uses`, or neither;
- a duplicate step id, or one starting with a digit;
- a `needs:` naming a missing job, or a cycle;
- a job without `runs-on` (unless it is a reusable `uses` job) or without `steps`;
- a `permissions` scope this server cannot enforce (`packages`, `checks`, `actions`,
  `deployments`, `statuses`, …) — a named parse error listing the supported set (§7.2); a scope is
  never accepted and then ignored;
- an `on:` trigger this engine cannot emit (the named-refusal list in §3.7);
- a remote `uses:` (`owner/repo@ref` or a URL), a cross-repository reusable call that is not pinned
  to a full 40-hex commit, or a `secrets: inherit` on a cross-repository call (D-6);
- an unknown trigger name (`on: [tag]`), `on: schedule` without `- cron:` entries, and any cron
  outside the subset (§8).

**Accepted but inert** — the file parses and runs, but the key changes nothing:

- `container.ports` / `command` / `entrypoint` (the job container's lifetime is the runner's). A
  **service's** `env`, `ports`, `command`, `entrypoint`, `options` and `volumes` are applied (§4.2 of
  the reference: a service is a sidecar, not the job container). `container.volumes` are applied only
  when the runner's operator allow-lists the host path (§4.4a); otherwise they are refused by name.

No trigger is inert: every accepted trigger key now changes what fires (§3).

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

- `branches` filters a branch push (`refs/heads/*`) and `tags` filters a tag push
  (`refs/tags/*`), independently:
  - `branches: [main]` fires on `main` and ignores tags;
  - `tags: ["v*"]` fires on `v1.2.3` and — unlike earlier releases — **not** on any branch
    push;
  - naming both filters each kind separately; naming neither fires on every branch and tag.
- Branch and tag deletes create no run.
- The filter dialect is the documented Actions cheat sheet:

  | Pattern | Matches |
  |---|---|
  | `*` | zero or more characters, **not** `/` |
  | `**` | zero or more characters, including `/` |
  | `?` | zero or one of the preceding character (`v?` matches `` and `v`) |
  | `+` | one or more of the preceding character |
  | `!pattern` | negation; patterns are evaluated in order and the **last** match decides |

  `branches-ignore`/`tags-ignore`/`paths-ignore` are exclusion lists (a name fires when no entry
  matches); naming both the include list and its ignore twin is a parse error. An absent or empty
  filter for the dimension being delivered matches every ref of that kind.

- `paths`/`paths-ignore` select a push (or PR) by its **changed files**. A docs-only commit never
  fires a `paths: ['src/**']` workflow; a mixed commit does. A `paths`-only trigger uses the pushed
  commit's diff (a new ref has no pre-image, so its own change set is used); a PR uses
  `base...head` (the merge base). A path-filtered trigger does not fire when no diff can be computed
  (fail closed). One name-only `git diff` is spent per event, and only when a discovered workflow
  declares a path filter — there is no store round trip for the filter.
- **The push hook delivers both kinds.** A landed `refs/heads/…` create matches `branches`, a
  landed `refs/tags/…` create matches `tags`; a delete of either starts nothing. A tag run has no
  branch: `github.ref` is the full `refs/tags/v1.2.3`, `github.ref_name` is `v1.2.3`, and
  `github.event_name` stays `push`. A tag named like a branch is still a tag: `tags: ["v*"]`
  fires for the tag push and not for a branch push of the same name.
  If a team prefers one release path, dispatching a version input and creating the tag in that job
  still works (§9.5) — both shapes are supported.
- The workflow is read at the pushed commit.
- A merged pull request creates a `push` run for the base branch (the merge commit).
- A push authenticated by a job token creates nothing (the `GITHUB_TOKEN` rule), so a
  `contents: write` workflow cannot rebuild itself forever, for branch and tag pushes alike.

A tag push is a separate ref kind, not a branch name: `tags: ["v*"]` never matches a branch
push, and `branches: [main]` never matches a tag. A bare `on: push` fires on both.

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
- `types` selects lifecycle actions. Absent = every emitted action. The vocabulary is closed to what
  the pulls service emits: `opened`, `synchronize` (only when the head actually moved), `reopened`,
  `closed` (including a merge), `edited` (title/body), `ready_for_review`, `converted_to_draft`,
  `labeled`, `unlabeled`, `assigned`, `unassigned`, `milestoned`, `demilestoned`. An unemittable
  type (`deleted`, `locked`, …) is a parse error naming the supported set.
- `paths`/`paths-ignore` filter the PR's changed files (the merge-base diff), exactly like a push.
- Same repository: the head commit's workflow runs, **trusted**, with secrets.
- Fork: the **base branch's** workflow file runs at the base tip, **untrusted**, no secrets,
  read-only job token; a first-time contributor's run may wait for approval (§7.3).
  A repository may opt in to `send_write_tokens` (maintain+, audited), which gives the next fork
  run the scopes it declares — never secrets and never an id-token (§7.2).
- The run pins the **server-computed merge commit** of base and head on every `pull_request` edge —
  the code edges (`opened`, `synchronize`, `reopened`, `closed`, `edited`, `ready_for_review`,
  `converted_to_draft`) and the metadata-only edges (`labeled`/`unlabeled`, `assigned`/`unassigned`,
  `milestoned`/`demilestoned`): its ref
  and `${{ github.ref }}` are `refs/pull/<n>/merge`, and the run commit (so also `GITHUB_SHA` and the
  check's commit) is that merge commit, so a failure that only exists after the merge goes red.
  The job's `GITHUB_REF` **variable** stays the PR's base branch, the documented job identity (§6.1);
  the run header and expression context carry the merge ref. If the merge conflicts, or the git
  reader cannot build one, the run falls back to the head commit and `refs/pull/<n>/head` and says so
  in the log, and the payload sets `pull_request.merge_commit_sha` only when a real merge exists. The
  workflow file is still the head's (same repo) or the base's (fork) — never the contributor's file
  for a fork.
- `synchronize` fires only when the head moved, so a replay cannot duplicate a run.

`pull_request_target` runs the base-branch file with base secrets (D-2). It is **off by default**
per repository; a maintainer enables it on the Threads page and the change is audited. An
`actions/checkout` naming a `ref` or another `repository` is refused at plan time unless the
repository also enabled `pr_target_allow_head_checkout` (a second, separately audited switch). A
`run:` step can still fetch the head itself; that residual is why the default is off.

### 3.3 `workflow_dispatch`

`environment: ${{ inputs.environment }}` is a **parse error**: the structural fields
(`runs-on`, `container.image`, `services.<id>.image`, `environment`) and the
workflow/job/container/service `env` layers do not take `${{ }}` (only a step `env:` does), so the
file is reported and creates no runs. Write the environment literally and use the input for the
command:

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

Five fields: `minute hour day-of-month month day-of-week`, **UTC** unless the entry names a
`timezone:`. Supported:

- `*`, a number, a range `a-b`, a step `*/N`, `a-b/N`, `n/N` (from `n` to the field maximum every
  `N`), and comma lists;
- three-letter month and weekday names (`JAN-DEC`, `SUN-SAT`) and `7` as Sunday;
- the `@yearly`/`@annually`/`@monthly`/`@weekly`/`@daily`/`@midnight`/`@hourly` macros;
- `L` (last day of month), `L-N`, `NW` (nearest weekday), `DL` (last weekday of month) and `D#n`
  (nth weekday);
- `timezone:` per entry (an IANA name such as `Europe/Berlin`; absent means UTC).

Not supported (parse error): a six-field expression with seconds, an unknown macro, an unknown
name, and an invalid timezone.

DST follows the local wall clock: a local time skipped by a spring-forward has **no tick that day**;
a local time repeated by a fall-back is **one slot and fires once** (the watermark coalesces it).
Behaviour: default branch tip only (a schedule on any other branch never fires); a process with the
`maintain` role must be running; at most once per tick (a CAS'd watermark), so a restart does not
re-fire and a crash between the watermark and run creation loses that tick rather than duplicating a
deploy. There is no "run now" — dispatch the workflow manually. See
[`threads.md` §6.4](threads.md#64-schedule).

### 3.5 What starts a run: the summary

| Event | Created by | Trusted | Secrets | Workflow read at |
|---|---|---|---|---|
| `push` | client push, PR merge | yes | yes | the pushed/merge commit |
| `pull_request` (same repo) | PR opened/synchronize/reopened | yes | yes | the head commit |
| `pull_request` (fork) | same | **no** | **none** | the **base branch tip** |
| `workflow_dispatch` | UI / API / CLI | yes | yes | the resolved ref |
| `schedule` | maintainer sweep | yes | yes | the default branch tip |
| `release` | the releases service | yes | yes | the **default branch tip** (the run reports the tag commit) |
| `issues` / `issue_comment` | the issues service | yes | yes | the **default branch tip** |
| `pull_request_target` | PR opened/synchronize/reopened | yes | **yes** | the **base branch tip** (a head checkout needs the second switch) |
| `repository_dispatch` | API dispatch (write role) | yes | yes | the **default branch tip** |
| `workflow_run` | an upstream run's edge | yes (unless the upstream was untrusted, which holds it) | yes | the **default branch tip** |
| `create` / `delete` | the push hook | yes | yes | the **default branch tip** |
| `deployment` / `deployment_status` | a deployment record | yes | yes | the **default branch tip** |

### 3.6 `release`, `issues` and `issue_comment`

```yaml
name: Announce
on:
  release:
    types: [published]
  issues:
    types: [opened, closed]
  issue_comment:
    types: [created]
jobs:
  announce:
    runs-on: linux
    steps:
      - run: |
          echo "action=${{ github.event.action }}"
          jq -r '.release.tag_name // .issue.number' "$GITHUB_EVENT_PATH"
```

- **Emitters, not a wish list.** A `types` entry the owning service never emits is a parse error:
  - `release`: `created` (a draft), `published`, `edited`, `deleted`, `prereleased` (a prerelease
    published), `released` (a published prerelease's flag cleared), `unpublished` (published →
    draft);
  - `issues`: `opened`, `edited`, `closed`, `reopened`, `labeled`, `unlabeled`, `assigned`,
    `unassigned`, `milestoned`, `demilestoned`;
  - `issue_comment`: `created` only (comment editing/deletion is not implemented).
- The workflow file is read from the **default branch tip**, never from the release tag or the
  commenter's checkout. A release run's `commit`/`GITHUB_SHA` is the tag's commit; an issue run's is
  the default branch tip.
- The event payload is a bounded projection: a free-text comment body is capped at 4 KiB, an asset
  or label list at 100 entries, and the encoded JSON at 64 KiB (an over-cap payload is a creation
  error, never a silent truncation). Read the full record with `jq` from
  `$GITHUB_EVENT_PATH`; `github.event.*` is the documented subset (§6.2).

### 3.7 The GitHub-only triggers

```yaml
name: Follow up
on:
  workflow_run:
    workflows: [Build]
    types: [completed]
    branches: [main]
  repository_dispatch:
    types: [deploy]
  create:
  delete:
jobs:
  follow:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.workflow_run.head_sha }}"
```

- **`repository_dispatch`**: `POST /{o}/{r}/api/threads/dispatches` with
  `{"event_type":"deploy","client_payload":{…}}`. The caller needs the write role (anonymous is
  401, a read role 403, a job token 403). `event_type` matches `[A-Za-z0-9._-]{1,100}` and selects
  `types`. `client_payload` is an object with at most 10 top-level keys, nesting at most 4 levels,
  scalar leaves, 64 KiB encoded; it is exposed at
  `${{ github.event.client_payload.<key> }}`. The workflow runs the **default branch** only
  (a `ref` must resolve to its tip). The answer is `202 {"runs":[…]}`.
- **`workflow_run`**: `workflows` matches upstream workflow **names** exactly (case-insensitive,
  no globs), `types` is `requested` or `completed` (**omitted = `completed`**), and
  `branches`/`branches-ignore` filter the upstream head branch. The follow-up file is read from the
  default branch. An upstream fork run is untrusted, so the follow-up is held for maintainer
  approval before any job starts (D-3); a chain stops at depth 3.
- **`create` / `delete`**: a branch or tag created or deleted, with `ref` and `ref_type` in the
  payload. A job-token push starts nothing.
- **`deployment` / `deployment_status`**: a job's deployment record created, or its status changed
  (`pending`, `in_progress`, `failure`).
- **Named refusals**: `merge_group`, `fork`, `watch`, `status`, `check_run`, `check_suite`,
  `registry_package`, `gollum`, `page_build`, `branch_protection_rule` and `public` are **parse
  errors naming why** — this engine does not emit them, and a trigger that can never fire is not
  accepted. `merge_group` returns when the separate merge-queue item lands, with the documented
  `refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>` ref shape.

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

- `sh` is `sh -e -c`; `bash` is `bash --noprofile --norc -eo pipefail -c`; `python`/`python3`
  map to `python -c` / `python3 -c`; `pwsh` and `cmd` are accepted but need the interpreter in the
  image. If the image has no **bash**, a `shell: bash` step falls back to `sh -e` and prints one
  log line saying so (GitHub's Linux fallback) instead of failing with exit 127.
- An arbitrary command template is allowed if it contains exactly one `{0}`: `shell: dash -e {0}`,
  `shell: perl {0}`. The template is split into argv words — it is never handed to a shell — and the
  runner writes the script to a per-step file in the container and substitutes its path. A template
  with no `{0}`, two of them, or one that starts with `{0}`, is a named step failure.
- Quoting is the shell's. `$NAME` is a container environment variable (workflow/job/step `env`,
  a secret, or a built-in); every `${{ … }}` reference is substituted **before** the shell sees the
  script — `matrix.*`, `inputs.*`, `github.*`, `env.*`, `secrets.*` and step/needs outputs alike
  (§6.2). Writing `$GITHUB_SHA` / `$NAME` instead is still preferable when you can: the shell
  expands it, so it never lands in the container's argv.

### 4.2 `uses:` — the exact spellings

| Spelling | What happens |
|---|---|
| `actions/checkout` | built-in checkout (§4.3). `actions/checkout@v4` also resolves: everything after `@` is ignored. |
| `actions/upload-artifact` | upload files from the job container (§6.4). `@v4` works too. |
| `actions/download-artifact` | download an artifact of the same run — or, with the D-3 approval, another run's in this repository (§6.4). `@v4` works too. |
| `actions/cache` / `actions/cache/restore` / `actions/cache/save` | restore and save a workspace tree across runs (§6.4a). `@v4` works too. |
| `./.spindle/actions/<name>` (any `./` path) | repository-local action from **this run's commit** (§4.4) |
| `docker://image[:tag\|@sha256:…]` | run the image as its own container on the job network (§4.4) |
| `actions/setup-node`, `actions/setup-go`, `actions/setup-python` | refused by name, with the exact `actions/cache` recipe that replaces the cache half (§6.4a) |
| anything else (`owner/repo@ref`, `actions/setup-java`, …) | the step fails: *"there is no marketplace for remote actions"* |

There is **no marketplace and no setup actions** (a toolchain is a base image, a `run:` step, or a
local action), and no `pre`/`post` hooks. The **cache service is built in** (§6.4a), so a dependency
cache does not need one.

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

`with:` keys (anything else fails the step by name, with what to do instead):

| Input | Default | Effect |
|---|---|---|
| `fetch-depth` | `1` | `git fetch --depth=N`; `0` is a full clone |
| `fetch-tags` | `false` | `true` adds `--tags` (the default passes `--no-tags` explicitly) |
| `ref` | the run commit | fetch and check out this branch, tag or SHA instead |
| `path` | the workspace root | check out into `<workspace>/<path>` (relative only) |

```yaml
name: tags and a subdirectory
on:
  workflow_dispatch:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout
        with:
          fetch-tags: true
          ref: refs/heads/main
          path: source
      - run: git describe --tags
        working-directory: source
```

Refused by name today: `submodules` (`git submodule update --init --recursive` in a step),
`repository` (plain `git fetch` with your own credential), `token`, `ssh-key`,
`persist-credentials` (the checkout uses the job token and writes nothing to `.git/config`),
`clean` (fresh volume per job) and `sparse-checkout` (`git sparse-checkout` in a step).

- The workspace is `/spindle/workspace` (`$GITHUB_WORKSPACE`); `path` is relative to it, and later
  steps still start at the workspace root unless they set `working-directory`.
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
- Composite steps expand in place. A composite `run:` step **must** name `shell:` (any form §4.1
  accepts, including a custom `{0}` template); `uses:` may be another local action or
  `docker://`, never a built-in or a marketplace action.
- `with:` values become `INPUT_<NAME>` (`dry run` → `INPUT_DRY_RUN`); an undeclared input is a
  named refusal; read inputs from the environment, not `${{ inputs.* }}`.
- Per-step `env:` layers: the job/container layer, then the caller's `env:`, then the inner step's
  own; an inner `if:` sees the layer below it. The action's `outputs:` mapping is evaluated over its
  own steps and published under the calling step's id (output threading).
- Nesting is capped at 10 and cycles are refused (both at run creation and again at plan time).
- `${{ github.action_path }}` is the composite action's **own** directory (PAR-074): inside a
  composite's steps it resolves to `<workspace>/<action dir>` for the innermost action whose step is
  running, so a nested local action gets its own path, not its caller's. The parser accepts the field
  only in a composite action's steps; a workflow step, a job `if:` or an output template that reads
  it is a named parse error, never an empty string. The runner supplies the value per composite
  level (docs/threads.md §3.4a states the exact contract).
- A `docker://` step (or docker action) runs as its own container with the workspace volume and
  the step environment plus `INPUT_*` — **never** the job token and never a repository secret.
  Its `$SPINDLE_OUTPUT` file is copied out and published under the calling step's id.
- Every `docker://` image goes through the same server image policy as a job container
  (`threads.image_allow_prefixes`, `threads.image_require_digest`).

### 4.4a Container options

`container.options` / `services.<id>.options` accept only:

`--cpus`, `--memory`/`-m`, `--memory-swap`, `--shm-size`, `--ulimit nofile=…`, `--init`,
`--read-only`, `--user`/`-u`, `--workdir`/`-w`.

By default everything else — `--privileged`, `--cap-add`, `--device`, `--security-opt`,
`--group-add`, `--pid`, `--network`, any mount — is refused, and a workflow-supplied `volume` is
refused outright. The server refuses at run creation (failed job, red check) and the runner
re-checks at plan time. The only exceptions are the operator grants below.

**Runner grants (operator decision, PAR-179).** A runner operator may widen exactly four things,
and the server then defers those shapes to the runner that claims the job:

- `--container-privileged` admits `--privileged` and `--cap-add` in workflow options;
- `--container-valid-volume <glob>` admits a workflow volume whose absolute host source matches the
  glob (`/srv/cache/*`); a root mount, a relative path, a named volume and an unknown mount option
  stay refused always;
- `--container-network` selects a user-defined docker network jobs, services and docker actions
  join (`host`, `none`, `bridge` and namespace modes are refused at startup);
- `--docker-host` gives the **job container only** a docker endpoint (`unix:///…` or `tcp://…`).

A runner that grants privilege or a docker endpoint refuses `--allow-fork-runs`, so an untrusted PR
can never reach it. A workflow asking for a grant on a runner that did not opt in fails at plan time
with a message naming the missing flag — nothing is started.

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

### 4.5 Reusable workflows

A job can call another workflow instead of declaring steps. The callee is a normal workflow file
that declares `on.workflow_call` — its interface:

```yaml
# .spindle/workflows/ci.yml
name: CI
on: [push]
jobs:
  test:
    runs-on: linux
    steps:
      - run: go test ./...
  deploy:
    needs: [test]
    uses: ./.spindle/workflows/deploy.yml
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

```yaml
# .spindle/workflows/deploy.yml
name: Deploy
on:
  workflow_call:
    inputs:
      target:
        type: string          # string | boolean | number
        required: true
      dry-run:
        type: boolean
        default: false
    secrets:
      REGISTRY_TOKEN:
        required: true
    outputs:
      url:
        description: the deployed URL
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

The callee's jobs join the same run as `deploy / release`, with their own checks and logs; there is
no hidden caller record. `needs:` inside the callee is rewritten to the expanded keys; a callee job
without `needs:` inherits the caller's; the caller's `if:` is conjoined with each callee job's
condition.

Rules (all enforced, a violation is a named failure — never an ignored key):

- **The callee must declare `on.workflow_call`.** Its typed inputs are the only `with:` names; an
  unknown input, a missing `required` input without a default, and a value that does not parse as the
  declared type (`boolean` is `true`/`false`, `number` is decimal) are named refusals on the caller
  job. Defaults apply for omitted optional inputs, so `inputs.<name>` is always present.
- **A `with:` value is expanded once, at run creation**, from `github.*` and the caller's own
  `inputs.*` only. A `secrets.*`, `needs.*`, `matrix.*`, `env.*` or `runner.*` reference there is a
  parse error (GitHub's available set is wider; this is the enforced subset).
- **`secrets:`** is an explicit `callee-name: ${{ secrets.SOURCE }}` map (a literal value is a parse
  error — it would put the secret in the file) or `inherit`. The callee's declared secrets are the
  only names it may read (`secrets.<name>` in a callee that does not declare it is a parse error);
  every `required` declaration must be passed. A same-repository callee with no `secrets:` block
  receives the repository's secrets (the documented implicit inherit); a cross-repository callee with
  no block receives **none**.
- **`secrets: inherit` never crosses a repository boundary** (decision D-6): it is refused by name,
  and a cross-repository callee gets exactly the listed secrets.
- **A callee's declared `outputs:` are the caller job's outputs**: `needs.<caller job>.outputs.<name>`
  works for every job of the run, and an output name the callee does not declare is a named refusal
  on the job that reads it. An output value may reference only a direct, non-caller job of the callee
  file (`outputs.<name>: ${{ jobs.<id>.outputs.<x> }}`), and that job must declare the output.
- **`permissions:` is downgrade-only**: every callee scope is capped at the caller's effective scope.
- A caller job is a call, not a runner job: `steps`, `runs-on`, `container`, `services`, `env`,
  `strategy`, `timeout-minutes`, `concurrency`, `environment` and `outputs` are named refusals.
- **Cross-repository calls** are `owner/repo/.spindle/workflows/<name>.yml@<40-hex commit>`: the
  callee must be readable without credentials on this instance, the reference must be a full commit
  SHA (a branch or tag is refused — it changes under the run), and every such call is written to the
  host audit family (`reusable_cross_repo`, naming the caller, the callee and the pinned commit;
  never a secret value). A URL, a `docker://` image and a foreign workflow directory are refused.
- nesting is capped at 4 levels and a `(repository, path)` cycle is refused by name. A missing or
  unparseable callee fails that job with the reason and the rest of the run continues.

Not supported today: a matrix caller (`strategy:` on a caller job), environment secret
inheritance into a callee, and output references to a nested call's job (read the outermost call's
declared outputs). Each is a named refusal, never inert syntax.

### 4.6 Background, `wait`/`cancel` and parallel steps

A `background: true` step keeps running while later steps execute; `wait: <id>` (or `wait-all: true`)
joins it and fails when it failed, `cancel: <id>` terminates it, and the job end kills every remaining
one. Each background runs in its own session/process group inside the job container (`setsid` must be
in the image), so a kill reaches the whole tree; the runner verifies the group is gone and fails the
job rather than leaking it. A non-zero exit is surfaced at the wait or, if never waited for, at the
job end.

```yaml-fragment
      - id: server
        run: ./bin/dev-server
        background: true
      - run: ./bin/smoke
      - cancel: server
      - name: linters
        parallel:
          - run: ./bin/lint-go
          - run: ./bin/lint-web
```

Parallel members run concurrently and are reported in declaration order (their logs are buffered and
flushed in that order). They share the workspace, so two members writing one file race — the same
caveat the GitHub documentation carries. A member failure fails the group unless that member has
`continue-on-error`.

```yaml
name: sidecar
on: push
jobs:
  test:
    runs-on: linux
    steps:
      - id: server
        run: ./bin/dev-server
        background: true
      - wait: server
      - name: linters
        parallel:
          - run: ./bin/lint-go
          - run: ./bin/lint-web
```

**Accepted model and refusals.** The keys are exactly the runner's `StepSpec` fields:
`background` (bool, needs an `id`, only on a `run`, `docker://`, or local `uses: ./…` step),
`wait` (a background step id or `all`), `wait-all` (bool), `cancel` (id or `all`), and `parallel`
(a non-empty list of ordinary steps). A file is a **named parse error** for:

- `wait`, `wait-all` and `cancel` combined on one step (they are mutually exclusive);
- a control step that also has `run`/`uses`, or that owns an `id`;
- a `wait`/`cancel` naming an id that is not a **background step started earlier**, or naming a
  step twice is the duplicate-id error (a self-reference is the control-step refusal);
- `background` without an `id`, or on a built-in action (`actions/checkout`, artifacts, cache) that
  is not a run/docker step;
- a `parallel` group with `run`/`uses`, with no members (`parallel: []`), that is itself a
  background/control step, or a member that is background, control, nested `parallel`, has neither
  `run` nor `uses`, or repeats an id;
- any of the five keys inside a composite action's `runs.steps`.

A local `uses: ./…` action may be a docker action (background-capable) or a composite (not); the
action file is read at run creation, and the runner refuses a background composite there by name —
the parser accepts the authored form because it cannot know the action kind yet.

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

- The matrix is the product of scalar axes, plus GitHub's `include` and `exclude` lists:

  | Key | Shape | Effect |
  |---|---|---|
  | *(axis)* | scalar or list of scalars | one value per cell; axes expand in name order, values in declaration order |
  | `include` | list of objects (`- key: value`) | merges into existing cells under GitHub's rules below; an object that fits no cell becomes a **new** cell |
  | `exclude` | list of objects | removes every base cell that matches **all** of the object's key/value pairs |

  `include` merge rules, exactly as implemented:

  1. Base cells are the product, minus the cells an `exclude` object removed.
  2. An include object merges into every surviving base cell whose **axis** values do not
     conflict with it (a key that is an axis must already hold the same value). Keys that are
     not axes are added, and may overwrite a value an earlier include entry added.
  3. An object that merges into no base cell becomes a new cell of its own, in include-list
     order; it is never merged into a cell created by an earlier include entry.
  4. An excluded cell is not resurrected by an include entry.

  A matrix with only `include` entries and no axes yields one cell per entry (the Scriobh
  release shape). A list-valued or nested value inside `include`/`exclude` is a parse error.
- The expansion is capped at **256 cells** — the base product plus the number of include
  entries — and a declaration over the cap is a parse error naming the bound.
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
startsWith(text, prefix)  contains(text, search)  format('{0}-{1}', a, b)
github.<field>  forgejo.<field>  forge.<field>  github.event.inputs.<name>
matrix.<axis>  inputs.<name>  env.<NAME>  secrets.<NAME>
steps.<id>.outcome  steps.<id>.conclusion  steps.<id>.outputs.<name>
!  ==  !=  &&  ||  (  )  'literal'  "literal"  true  false
```

`github.<field>` is the closed set `event_name`, `sha`, `ref`, `ref_name`, `repository`,
`workflow`, `job`, `run_id`, `actor`; `forgejo.*` and `forge.*` are value-identical aliases.
`format` uses `{N}` argument indexes and `{{`/`}}` for literal braces. `secrets.<name>` is the
secret's value on a trusted run and empty on an untrusted one — never the literal text.

A **job** `if:` adds `needs.<job>.result` and `needs.<job>.outputs.<name>`; `steps.*` and
`secrets.*` are not available there (a parse error naming the limitation — use a step `if:` or a
step `env` value). Comparisons are string-wise and case-insensitive. An empty `if` means
`success()`.

Functions: `startsWith(a, b)`, `contains(a, b)` and `format('{0}-{1}', a, b)` are supported
alongside the four status functions; `format` uses `{N}` indexes and `{{`/`}}` for literal
braces. Anything else — `endsWith`, `join`, `toJSON`, `hashFiles`, a nested object, an unknown
context — is a **parse error for the file** (and an evaluation error if a stale file reaches a
job), never a silent skip. `if: startsWith(github.ref, 'refs/tags/v')` and
`if: github.ref == 'refs/heads/main'` are both valid.

Do not wrap an expression-with-operators in `${{ }}`: write `if: inputs.dry_run != 'true'`. A
single whole-span condition (`if: ${{ inputs.flag }}`) is accepted as a substituted value, but an
operator expression belongs in the native form.

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

- One in-flight **run** per group per repository, and **one pending run** behind it (GitHub's
  rule). A run's sibling jobs do not serialize each other (the group belongs to the run).
- A newer run that wants a held group becomes the pending run and **cancels the run that was
  already pending** — only one run waits, so three rapid pushes leave one running and one
  pending, never two waiting.
- `cancel-in-progress: true` cancels the run holding the group (and any pending run) as soon as
  the new run is created, then takes the group.
- `cancel-in-progress: false` (the default) sequences in trigger order: the holder finishes, the
  pending run starts, and a later run replaces the pending one.
- The group is expanded at run creation and is a bucket key. A reference that cannot exist is a
  **parse error**; a supported reference with no value for this run (an input without a
  default) fails run creation with `422`. It never falls back to literal text.
- Workflow-level groups may read `github.*` (the fields listed above), `inputs.*` and — at job
  level — the job's `matrix.*`. `env.*`, `steps.*`, `needs.*` and operators are not supported.
- `group: ci-${{ github.ref }}` is the usual idiom: without the ref, `cancel-in-progress: true`
  on a push to any branch cancels `main`.
- Enforcement is at **claim/admission**: a job whose group is held waits in the queue; no runner
  sees it until the group is free. The run header names the group it waits for
  (`pending_group`), so the run page and the run list say why a run has not started. A grouped
  run is never implicitly superseded: the group itself is the explicit serialisation choice (§5.4).

#### If you do not declare a group: the newer push wins

You do not need a `concurrency:` block to stop a double deploy. A push to the same branch, and a
PR `synchronize`, **supersede** the run of the commit they replace: the older run turns
`cancelled` and its header says `cancel_reason: supersede`, `superseded_by: <newer run id>`.
That is Forgejo's rule and it is on by default. Two things to know as an author:

- **A run you declared a group for is never cancelled this way.** `concurrency:` — and
  `cancel-in-progress: false` in particular — is your explicit serialisation choice, so Threads
  leaves the older run alone and your group decides whether the new one queues or runs. One or
  the other: the default is supersede, the block is the override.
- **Only the same workflow file and the same ref pair up.** A push to `main` never cancels a
  feature branch's run, a PR's run is keyed on `refs/pull/<n>/head`, and a dispatch or a cron
  tick never cancels anything. A run that already finished is untouched.
- What you see: the old run page reads **cancelled** with the reason and a link to its successor;
  its queued jobs say "superseded by run `<id>`"; the new run is the one live run of that branch.
  If you need every push to be built (a compatibility matrix, an audit trail of builds), use a
  distinct `concurrency` group per run — e.g. `group: build-${{ github.sha }}` — and the
  supersede rule no longer applies to it.

## 6. Environment, expressions, outputs, artifacts

### 6.1 `env` layering and built-ins

Lowest to highest: runner built-ins → workflow `env` → job `env` → container `env` → step `env`;
repository secrets are added last, by name. Every step sees:

| Variable | Value |
|---|---|
| `CI`, `GITHUB_ACTIONS`, `SPINDLE` | `true` |
| `GITHUB_REPOSITORY` / `SPINDLE_REPO` | `owner/repo` |
| `GITHUB_SHA` / `SPINDLE_SHA` | the run's commit |
| `GITHUB_REF` / `SPINDLE_REF` | the triggering ref: `refs/heads/<branch>` (a PR: the base branch — the job's identity; the run header and `${{ github.ref }}` carry `refs/pull/<n>/merge`, §3.2), or `refs/tags/<tag>` for a tag push, where `GITHUB_REF_NAME` is the tag |
| `GITHUB_REF_NAME` / `GITHUB_EVENT_NAME` | the ref's short name (branch, tag, or PR base branch) / `push`, `pull_request`, `pull_request_target`, `repository_dispatch`, `workflow_run`, `create`, `delete`, `deployment`, `deployment_status`, `workflow_dispatch`, `schedule`, `release`, `issues`, `issue_comment` |
| `GITHUB_EVENT_PATH` / `FORGEJO_EVENT_PATH` | `/tmp/spindle-event.json`: the bounded event payload, written before the first step |
| `RUNNER_OS`, `RUNNER_ARCH`, `RUNNER_NAME`, `RUNNER_TEMP`, `RUNNER_TOOL_CACHE`, `RUNNER_ENVIRONMENT`, `RUNNER_DEBUG` | `Linux`, `X64`/`ARM64`/…, the registered runner name, writable `/tmp/spindle-temp` and `/tmp/spindle-tool-cache`, `self-hosted`, `0` |
| `SPINDLE_EVENT_PATH`, `SPINDLE_RUNNER_*` | the same values in spindle spelling |
| `GITHUB_WORKFLOW`, `GITHUB_JOB`, `GITHUB_RUN_ID` | workflow, job, run |
| `GITHUB_ACTOR` | the principal that triggered the run (absent when the run has none); the same value `${{ github.actor }}` reads |
| `GITHUB_WORKSPACE` / `SPINDLE_WORKSPACE` | `/spindle/workspace` |
| `SPINDLE_RUN_ID`, `SPINDLE_JOB_ID`, `SPINDLE_CHECK`, `SPINDLE_ATTEMPT` | ids and the check context |
| `SPINDLE_TRUSTED` | `true` for a trusted run |
| `SPINDLE_SERVER` | the spindle origin |
| `GITHUB_SERVER_URL`, `FORGEJO_SERVER_URL` | the canonical origin (`server.public_url`, else the runner's origin), no trailing slash |
| `GITHUB_API_URL` | `<GITHUB_SERVER_URL>/api/v1` |
| `MATRIX_<AXIS>` | the cell's value, axis uppercased |
| `SPINDLE_JOB_TOKEN` | the job token (§7.2) |
| `GITHUB_TOKEN`, `FORGEJO_TOKEN` | the **same** job token (§7.2); an `env:` value of that name wins |
| `GITHUB_ENV` / `FORGEJO_ENV`, `GITHUB_PATH` / `FORGEJO_PATH` | this step's command files (§6.6) |
| `SPINDLE_OUTPUT` / `GITHUB_OUTPUT` | the step output file (§6.5) |
| `SPINDLE_ID_TOKEN_REQUEST_URL` / `…_TOKEN` (and `ACTIONS_*`) | OIDC exchange, `id-token: write` only |
| each repository/environment secret | by its own name, trusted runs only |

Not provided: `GITHUB_STATE` (there are no `pre`/`post` hooks) and the `github.token` /
`forgejo.token` context field (a parse error; use `$GITHUB_TOKEN` in a script). A workflow migrating
from elsewhere usually has to replace the language's own conveniences (§9 and §10).

### 6.2 Where `${{ … }}` is substituted

| Site | `matrix.*` | `inputs.*` | `steps/needs.*.outputs.*` |
|---|---|---|---|
| step `name`, `run`, `if`, `shell`, `working-directory` | yes | yes | yes |
| step `env` values, `with` values | yes | yes | yes |
| job `outputs` values | yes | yes | yes |
| job `name`, `concurrency.group` | yes (job-level group) | yes | no |

Inside those "yes" cells a site may also read the `github`/`forgejo`/`forge` fields, `env.*`,
`secrets.<name>` and `github.event.inputs.<name>`; every reference is validated at parse time, so
an unknown context, field or name is the file's error, never a literal.

**Structural fields and the env layers above a step are not substitution sites at all.**
`runs-on` (every accepted shape), `container.image`, `services.<id>.image` and `environment` (its
`name` and `url`) are chosen before a matrix cell or dispatch input exists, and a
workflow/job/container/service `env` value is handed to the runner verbatim — only a **step** `env:`
value is substituted. A `${{ … }}` span in any of them is a **parse error naming the field** — the
file is reported and creates no runs — never a value the run carries as literal text:
`runs-on: ${{ matrix.os }}` would queue forever against a label no runner declares, the image gate
would compare the string `debian:${{ matrix.variant }}`, and an env value would reach the process
environment unexpanded. Write the value literally; for per-value runners or images, declare one job
per value with literal fields (substitution is PAR-053), and put a reference that must be
substituted in a step `env:`.

Each reference resolves at run time:

| Reference | Value |
|---|---|
| `github.<field>` / `forgejo.<field>` / `forge.<field>` | the run's `event_name`, `sha`, `ref`, `ref_name`, `repository`, `workflow`, `job`, `run_id`, `actor`, plus the two server addresses `server_url`/`api_url` (§6.1); the aliases are value-identical |
| `github.event.inputs.<name>` | the dispatch input's value (same as `inputs.<name>`); the name must be declared |
| `github.event.<field>` | a documented payload field: `action`, `ref`, `before`/`after`, `repository.*`, `sender.login`, `pull_request.*` (`number`, `title`, `state`, `draft`, `merged`, `user.login`, `head.*`, `base.*`), `release.tag_name`/`name`/`draft`/`prerelease`/`target_commitish`/`author.login`/`assets`, `issue.number`/`title`/`state`/`user.login`/`labels`, `comment.body`/`user.login`. The set is closed; the full JSON is at `$GITHUB_EVENT_PATH` |
| `runner.<field>` | `os`, `arch`, `name`, `temp`, `tool_cache`, `environment`, `debug` (step sites and step `if:` only) |
| `vars.<name>` | a repository variable (overriding an organization variable of the same name); values are masked in the log |
| `matrix.<key>` | the cell's value; the key must be an axis or an `include` key |
| `inputs.<name>` | the dispatch input's value; the name must be declared |
| `env.<NAME>` | the authored environment at that layer (runner built-ins → workflow → job → container → step, the step layer winning); a variable a step wrote to `$GITHUB_ENV` is runtime state for later steps, not part of the context |
| `secrets.<name>` | the repository secret's value on a trusted run, empty on an untrusted one or an unknown name — **never the literal text**; case-insensitive, and the reserved aliases `GITHUB_TOKEN`/`FORGEJO_TOKEN` are the job token |
| `steps.<id>.outputs.<name>` / `needs.<job>.outputs.<name>` | the published output (strict: an unknown id or output is an error) |

Every site's references are expanded in **one pass**, so a substituted value is data, never a
template: a secret or output whose body contains `${{ github.ref }}` stays exactly that text. The
rule for a value that is not there is one of two:

- **an absent supported value expands empty** — an unset `env.NAME`, a secret the run does not have
  (a fork run, a typo), `github.actor` on a run with none, `server_url`/`api_url` with no canonical
  origin configured;
- **an undeclared `steps`/`needs` output fails the step** with an error naming the reference: an id
  that is not one of the job's steps, or an output that step never published, or a `needs:` job
  that is not a dependency. A value must never cross a step or job boundary as an empty string by
  accident.

`${{ secrets.<name> }}` as the **whole** value of a step `env:` entry keeps its stronger delivery:
the variable arrives **by name** and the value never reaches argv. Embedded in a larger string
(`https://user:${{ secrets.T }}@host`) it is substituted like on GitHub — the value is necessarily
part of the value it is spliced into — and it joins the log mask, so the stream prints `***`.

A **job-level** `if:` still cannot read `secrets.*` (PAR-058) and does not resolve
`server_url`/`api_url`; key a **step** `if:` or the environment variable for those. A step `if:`
reads every field above, `github.actor` included.

Concurrency groups additionally read `github.ref`, `github.ref_name`, `github.repository`,
`github.workflow`, `github.event_name`, `github.sha`, `github.run_id` (not `github.job` or
`github.event.inputs`; write `inputs.<name>` there).

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
      - name: release only
        if: startsWith(github.ref, 'refs/tags/v') && contains(github.ref, 'rc-') == false
        run: echo "release ${{ github.ref_name }}"
```

`steps.<id>.outcome` is the raw result (`success`, `failure`, `cancelled`, `skipped`);
`.conclusion` is that result after `continue-on-error`. A step that did not run is recorded too.
Step ids are case-insensitive. `success()` / `failure()` describe the **job** status, so a
tolerated failure does not flip them.

### 6.4 Artifacts

Artifacts pass files between jobs of the **same run**, and — with an approval (D-3) — from another
run of the same repository. They travel over the authenticated job channel; the job container never
talks to the object store.

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
|---|---|---|---|---|
| `name` | both | letters, digits, `.`, `_`, `-`; not starting with `.`/`-`; ≤ 255 bytes; **unique per run** | `artifact` (upload); every artifact (download) |
| `path` | upload | one or more patterns, newline/comma separated, workspace-relative | required |
| `if-no-files-found` | upload | `warn`, `error`, `ignore` | `warn` |
| `retention-days` | upload | `0` (run window) or `1`–`90`; may shorten, never extend | `0` |
| `overwrite` | upload | `true` / `false` (replaces a same-name artifact in the run; the runner prepares the upload first) | `false` |
| `include-hidden-files` | upload | `true` / `false` (dot-prefixed paths) | `false` |
| `compression-level` | upload | `0` (store) … `9` (deflate; shapes the ZIP download only) | `6` |
| `path` | download | workspace-relative directory | the workspace |
| `pattern` | download | a glob over artifact **names** (`dist-*`) | — |
| `artifact-ids` | download | comma-separated artifact ids (the list API shows each `id`) | — |
| `merge-multiple` | download | `true` / `false` (one merged tree instead of `<path>/<name>/`) | `false` |
| `run-id` | download | another run of THIS repository; requires the D-3 approval | this run |

`name`, `pattern` and `artifact-ids` are mutually exclusive. `github-token` and `repository` are
refused by name (the job token is used, and an artifact never crosses a repository). Without a
`name`, every artifact of the run is downloaded.

**Cross-run consumption (D-3).** A `run-id` step holds the consuming run in `pending_approval`: a
maintainer must approve it (a fork run, or a job that references a repository/environment secret, is
refused by name), and every cross-run read is audited. Its source must be a run of the same
repository.

Patterns: exact file, directory (recursive), `*`/`?` within a segment, `**` across segments,
`[ab]`, and `.` for the whole workspace. Only regular files are uploaded; symlinks are not
followed. The image needs `find` and `tar`. Caps: 256 MiB per file, 2 GiB per run, 1000 files
per artifact (configurable). Artifact **contents are data, not masked** — never write a secret
into one. Use an artifact name with no `/` or spaces; a matrix cell's name must be unique, e.g.
`dist-${{ matrix.target }}` with `target: [linux-amd64, linux-arm64]`.

### 6.4a Caches

`actions/cache` restores a workspace tree at the step and saves it at the end of a green job:

```yaml
name: cache
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - name: point npm at a workspace cache
        run: echo "npm_config_cache=.npm" >> "$GITHUB_ENV"
      - uses: actions/cache
        id: cache
        with:
          path: .npm
          key: npm-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}
          restore-keys: |
            npm-${{ runner.os }}-${{ github.ref_name }}-
      - if: steps.cache.outputs.cache-hit != 'true'
        run: npm ci
```

| Key | Accepted | Default |
|---|---|---|
| `path` | workspace-relative paths, newline/comma separated (**absolute paths and `~` are refused**) | required |
| `key` | ≤ 512 bytes | required |
| `restore-keys` | up to 10 prefixes, one per line | — |
| `fail-on-cache-miss` | `true` / `false` (a miss is silent success by default) | `false` |
| `lookup-only` | `true` / `false` (no restore, no save) | `false` |

The step publishes `cache-hit` (`"true"` only on an exact key match). `actions/cache/restore`
restores only; `actions/cache/save` saves immediately. The archive is a tar stored
content-addressed and integrity-checked, so the same bytes are stored once.

**Scopes** (D-10): a default-branch run reads/writes `default`; another branch reads/writes its own
branch and reads `default`; a pull request reads/writes `pr:<n>` and reads its base; a **fork's**
entry lives in `pr:<n>:untrusted`, which no trusted run reads; a tag run is read-only. A PR never
reads another PR's cache, and a PR can never write a base-readable entry. A merged PR's caches are
discarded. Limits: 1 GiB per entry, a 5 GiB repository budget (oldest-first eviction), 256 entries,
and a 7-day idle window — all configurable under `[threads] cache_*` (docs/threads.md §9).

**`setup-*` actions.** They are marketplace JavaScript actions and are refused by name; the refusal
quotes the recipe:

| Action | Recipe |
|---|---|
| `setup-node` (`cache: npm`) | `npm_config_cache: .npm` (job env) + cache `.npm`, key `npm-${{ runner.os }}-${{ github.ref_name }}-${{ github.sha }}`, `restore-keys: npm-${{ runner.os }}-${{ github.ref_name }}-` |
| `setup-go` | `GOCACHE: .cache/go-build`, `GOMODCACHE: .cache/gomod` + cache both, key `go-…` |
| `setup-python` (`cache: pip`) | `PIP_CACHE_DIR: .cache/pip` + cache that path, key `pip-…` |

`setup-java`/`setup-dotnet`/`setup-ruby` are refused with "not one of the mapped ecosystems
(npm/pip/go)".

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
A malformed line is a named step failure, and the error names the line. The heredoc delimiter is
matched exactly on its own line and is capped at 128 bytes (use a short random value); an
unterminated heredoc or an empty delimiter is a named failure too. Caps: 50 per step, 100 per job,
64 KiB per value, 1 MiB per job.

The deprecated `::set-output name=NAME::VALUE` alias (GitHub disabled it) is still honoured: the
value joins the same namespace and a name written both ways keeps the `$GITHUB_OUTPUT` value. A
step without an `id:` discards it with a warning.

Reading is **fail-closed**: a reference to an unknown step id, or to an output the step did not
publish, fails the step naming the reference. Inside a native `if:`, an unpublished output of a
**known** step reads empty, so `== ''` works.

Only a **successful** job reports outputs. A failed/skipped/cancelled dependency exposes none.
Outputs are **not masked**; never put a secret in one.

### 6.6 Command files: `$GITHUB_ENV` and `$GITHUB_PATH`

A step can change the environment and `PATH` of the steps **after** it:

```yaml
name: env files
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - name: derive a version
        run: |
          echo "TAG=$(cat VERSION)" >> "$GITHUB_ENV"
          echo "$PWD/.tools/bin" >> "$GITHUB_PATH"
      - name: use it
        run: |
          echo "tag is $TAG"
          tool --version
```

- `$GITHUB_ENV` (alias `$FORGEJO_ENV`): one `NAME=VALUE` per line. `$GITHUB_PATH` (alias
  `$FORGEJO_PATH`): one directory per line.
- **Later steps only**: the writing step does not see its own value; every later step of the job
  does. A step that writes and then fails still applies its writes.
- PATH entries are **prepended** to the next step's PATH, in write order, ahead of the image's own
  PATH — never replacing it. Set `PATH` through `$GITHUB_PATH`, not `$GITHUB_ENV`.
- No heredoc (that form is `$GITHUB_OUTPUT`'s, §6.5), no `$VAR` expansion, no globs. A malformed
  line is a named step failure; a repeated name keeps the last value.
- Refused by name: `GITHUB_*`, `SPINDLE_*`, `FORGEJO_*`, `ACTIONS_*`, `RUNNER_*`, `CI`, `PATH` and
  the startup hooks `LD_PRELOAD`, `LD_AUDIT`, `BASH_ENV`, `ENV`, `NODE_OPTIONS`, `IFS`, `PS4`,
  `SHELLOPTS`.
- A later step's own `env:` block wins over a file value; otherwise the file overrides the
  workflow/job/container `env` layer.
- Caps: 100 variables per job, 8 KiB per value, 64 KiB of added environment, 64 PATH entries.
- `docker://` actions receive none of it (third-party image, same rule as secrets).
- Values are not masked when collected: their echoed log lines are masked (§7.1), but a secret
  written to these files is handed to every later step — move secrets between jobs as secrets
  instead.

### 6.6a Step summaries

Every repository-code step gets `$GITHUB_STEP_SUMMARY` (alias `$SPINDLE_STEP_SUMMARY`), a file it
may append markdown to. The runner reads it at step end (success or failure) and the run page
renders the joined result in a **step summary** panel through the same sanitizing markdown renderer
as a README; a ```` ```mermaid ```` block is shown as a code block, never executed.

```yaml-fragment
      - name: report
        run: echo "### Tests passed :white_check_mark:" >> "$GITHUB_STEP_SUMMARY"
```

Limits: 1 MiB per step, 20 summaries per job, 4 MiB joined. An oversized summary fails a step that
otherwise succeeded (on a failed step it is only logged). `docker://` actions do not receive the
file.

`$GITHUB_STATE` is **not** provided: it exists for an action's pre→post hand-off and this engine
runs no pre/post hooks yet. `::save-state::` is answered with a named warning, so nothing is
accepted-and-inert.

### 6.7 Workflow commands

Lines of the form `::name key=value::message` in a step's output are workflow commands:

```yaml-fragment
      - run: |
          echo "::notice title=Build::artifacts are ready"
          echo "::group::tests"
          go test ./...
          echo "::endgroup::"
          echo "::error file=main.go,line=3,col=2::the handler is wrong"
          echo "::warning::deprecated call"
```

| Command | Effect |
|---|---|
| `::error::` / `::warning::` / `::notice::` | an annotation on the job (properties `file`, `line`, `endLine`, `col`, `endColumn`, `title`), shown on the run page and counted on the commit's check description. An `error` annotation does **not** fail the step |
| `::group::` / `::endgroup::` | log group markers; an unclosed group is closed at step end |
| `::add-mask::VALUE` | adds VALUE to the job's log mask permanently — there is no unmask. Under 4 bytes it is recorded but not applied. At most 128 masks |
| `::stop-commands::TOKEN` / `::TOKEN::` | stop/resume command parsing around untrusted output |
| `::set-output name=NAME::VALUE` | deprecated output alias (§6.5) |
| `::debug::` | a debug log line |
| anything else | a visible warning naming it; the line stays in the log |

Escapes: `%25`→`%`, `%0A`/`%0D`→newline/CR; properties also `%3A`→`:` and `%2C`→`,`. One output
line is buffered to at most 64 KiB (longer is plain text, never parsed mid-line); annotation
messages/titles/paths are capped; 10 annotations per step and 50 per job are kept; every payload is
masked before storage and rendering. Annotation text is data, escaped by the page.

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

- Read `$NAME`: every secret is exposed to a trusted step as an environment variable of its own
  name. `${{ secrets.NAME }}` is substituted too (empty on an untrusted run, masked in the log —
  §6.2); a whole-value `env:` entry is delivered by name and keeps the value out of argv, so
  `env: { TOKEN: "${{ secrets.TOKEN }}" }` plus `$TOKEN` is the form that never reaches the
  container's argv.
- Expect nothing on a **fork** run (`secrets: {}`). Guard with `${VAR:?message}` so a missing
  secret is a clear failure.
- Masking is best effort: exact substring replacement in log lines, values under 4 bytes skipped.
  A value that is encoded, split or derived will print. Never echo a secret.
- Never put a secret in an **output** or an artifact: both are data and are not rewritten.

### 7.2 The job token and `permissions`

Every step receives `$SPINDLE_JOB_TOKEN`, scoped to one attempt of one job in one repository:

- always read of its own repository;
- the **effective** `permissions` map (workflow merged with job, job wins, over
  `threads.default_permissions`) decides everything else. It is a closed vocabulary: a scope this
  server cannot enforce is a **named parse error**, never an inert key;
- a fork run is pinned to `contents: read` regardless of the file, unless the repository enabled
  **`send_write_tokens`** (maintain+, audited): then the fork run's token carries the scopes its
  workflow declares, still never secrets and never `id-token`;
- a protected ref still refuses it, because the token is a `ci:<runner>` principal, not the
  user;
- it expires with the job and is revoked at completion; a push authenticated by it starts no
  workflow.

**The scope matrix** (PAR-112, D-11; the authority is the same for git and for the platform API):

| Scope | Levels | The token may |
|---|---|---|
| `contents` | `read` / `write` / `none` | clone/fetch; `write` also pushes to the one repository |
| `issues` | `read` / `write` / `none` | read issues; `write` = triage (label, assign, close others' issues, manage labels/milestones) |
| `pull-requests` (`pull_requests` accepted) | `read` / `write` / `none` | read PRs; `write` = write role (open/update PRs, create releases) |
| `id-token` | `write` / `none` | exchange for an OIDC ID token (trusted runs only; §7.2a) |
| `read-all` / `write-all` | shorthand | baseline for the three repo scopes only — **never** `id-token` |

Anything else — `actions`, `attestations`, `checks`, `deployments`, `discussions`, `metadata`,
`models`, `packages`, `pages`, `repository-projects`, `security-events`, `statuses`, `wiki`, or a
typo — is refused at parse time with the supported set named. A job token never becomes a user, has
no session, and its implied repository role never reaches admin, so it cannot mint another
credential; a write to any other repository is a `403`.

Within the repository the scopes imply an ordered role (read → triage → write): a `contents: write`
token also has triage actions, a `pull-requests: write` token does not reach releases (those need
`contents: write`), the commit-status API always refuses a job token (`checks`/`statuses` are refused
scopes, so a job cannot forge a required check), the registry push gate ignores the job identity
(`packages` is refused), and the Threads operator surface (dispatch, cancel/rerun, approvals,
environments/secrets) never accepts a job token.

> Divergence recorded: spindle lets any reader comment on an issue, so a `contents: read` job token
> can post a comment. Triage/write actions still require their scope.

The same credential is also `$GITHUB_TOKEN` and `$FORGEJO_TOKEN` in every step, and
`${{ secrets.GITHUB_TOKEN }}` resolves to it everywhere `secrets.*` does (§6.2) — one token, no
second mint; as a whole-value `env:` entry it is still delivered by name (never argv). An `env:`
value you set for `GITHUB_TOKEN` wins (a workflow publishing to GitHub with its own PAT keeps the
PAT), the secret name `GITHUB_TOKEN` is reserved, and the value is masked in the log.

**Reusable workflows are downgrade-only.** A callee job's declared scopes are capped at the caller
job's effective scope (§4.5): a callee can narrow what the caller grants, never widen it, and a
scope the caller does not grant is `none` for every callee job. A caller with no `permissions:`
block therefore grants `contents: read` and nothing else, whatever the callee asks for.

**No `permissions:` = `contents: read`.** That is a recorded divergence from Forgejo, whose token is
implicitly write-capable and ignores `permissions` (D-12): a workflow relying on the implicit write
fails here with a `403` naming the missing scope (`the job token has no push permission (workflow
permissions: contents: read)`) instead of being silently upgraded. The default is the operator's
`threads.default_permissions` setting, not a workflow concern.

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

The token works against the platform API too (comment on an issue/PR, label, release):

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

**OIDC.** `permissions: id-token: write` or the Forgejo `enable-openid-connect: true` lets a
**trusted** job exchange a short-lived, audience-scoped ID token. The audience is required (body
`{"audience":"…"}` or `?audience=…`), the subject binds the repository plus its ref — or its
environment when the job names one, or `:pull_request` for a PR run — and
`ref_type`/`ref_protected`/`job_workflow_ref`/`runner_environment` are part of the closed claim set.
A fork run never gets one, `write-all` never implies it, and `id-token: none` denies it. The full
contract (claim table, subject templates, TTL, rate limit) is
[`threads.md` §5.5](threads.md#55-job-oidc-tokens-cloud-identity).

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

The two documented forms are the bare name (`environment: production`) and the mapping above;
`name` is required and `url`, when present, must be an absolute `http(s)` URL. Both are read
**before** any matrix cell or dispatch input exists, so a `${{ … }}` in either is this file's
parse error — an expression there never reaches a bucket key (PAR-130).

Create it in **Settings → Environments** (maintain+) with the protection rules GitHub documents:

| Rule | Behaviour |
|---|---|
| required reviewers | up to **six** principals **and** teams together, each principal holding write or more (checked when saved); every one of them must approve |
| team reviewers | `org/team`; the team must resolve (checked when saved), and its members at decision time must approve |
| wait timer | 0–360 minutes, counted from deployment creation; the job cannot start before it elapses, even with every approval in |
| prevent self-review | the principal who triggered the run cannot approve its deployment |
| deployment branches and tags | exact names and `*` patterns; empty allows every ref; a deployment from any other ref **fails** with a named reason (it is not held for approval) |

A job naming a **defined** environment with any of the first three rules makes the run
`pending_approval` (`approval_kind: environment`) and nothing is enqueued until every required
reviewer (or a repo admin, who bypasses the reviewer set) approves. A partial approval is
recorded on the deployment and the run stays pending, naming who is still awaited. A denied
deployment fails only the deployment jobs. A job naming an **undefined** environment fails with a
message naming Settings → Environments.

An approval is not enough if the wait timer has not elapsed: the claim path refuses to offer the
job until the deadline, and the run page and the API name that deadline (`wait_until`). This is
the one hold with no reviewer in it, and it is why a deploy cannot "start immediately" after an
approval when the environment declares a timer.

Environment secrets override repository secrets of the same name, are delivered only to a job
that names the environment in a trusted run with an approved deployment, and a fork run never
receives them.

## 8. Schedules, retention, runner

### 8.1 Retention

Retention has **two windows**, both operator knobs (`threads.retention`, default **720h =
30 days**, and `threads.artifact_retention`, default: the run window; each valid 24h…8760h):

- the **run window** deletes a **terminal** run once its completion (or creation, if it never
  started) is older than the window. Jobs, log frames and queue entries go with it; **checks do
  not**, so an old required check still gates a merge. A queued, running or `pending_approval`
  run is never swept, whatever its age, and neither is a run with a job whose lease is still
  live (a stale fold must not delete the log a runner is writing);
- the **artifact window** is the artifact's own lifetime: created plus its `retention-days`
  when the upload declared one, otherwise created plus `threads.artifact_retention`. An
  artifact whose window is longer than the run's **outlives its run** — deleting the run leaves
  its manifest and blobs, and the expiry row reclaims them at their own time.

`threads.keep_forever = true` reclaims neither run/logs nor artifacts (dead runner records are
still pruned — infrastructure, not history). A 90-day setting is a normal value, not a ceiling.

The sweeper reports bytes reclaimed **per family** (run, job, log, artifact, queue, runner) and
`POST …/api/v1/admin/gc` (or the admin GC page) can run a **dry run**: identical counts and
bytes, nothing deleted.

### 8.2 The runner, in brief

| Need | Answer |
|---|---|
| minting | `/admin/runners` (browser, host admin) → `wrg_` registration token, 15 min, with a repository policy (`all` or up to 50 `owner/name` / `owner/*` patterns) |
| start | `spindle --server URL runner serve --register wrg_… --token-file F --labels … --name … --capacity N` |
| labels | exact, comma-separated, matched as a **subset**, case-insensitive; default `self-hosted,linux,docker` |
| label → image | Forgejo's `name:docker://image` spelling (e.g. `ubuntu-latest:docker://node:lts`): a job whose `runs-on` names that label and names no `container.image` runs in that image. `lxc://` and `host` are refused by name (D-15 non-goals) |
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

Kaniko is the portable choice: it needs no privileges, no user namespaces and no socket.

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
      image: gcr.io/kaniko-project/executor:debug
    steps:
      - uses: actions/checkout
      - name: build and push
        run: |
          /kaniko/executor \
            --context="dir://$GITHUB_WORKSPACE" \
            --dockerfile="$GITHUB_WORKSPACE/Dockerfile" \
            --destination="registry.example.com/acme/app:$GITHUB_SHA"
```

Buildah works where the runner host lets it create user namespaces (kernel and daemon configuration,
not this file):

```yaml-fragment
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

`docker build` works only on a runner whose operator granted `--docker-host` (PAR-081/D-8); the job
is then root-equivalent on that runner and such a runner never accepts fork runs. On a runner without
the grant, a step that invokes `docker` fails with exit 127 **and the daemonless remedy in the
message**, and a workflow that tries to mount the socket itself is refused by the container policy.
Registry credentials for a push are the job's (`${{ secrets.* }}` substituted into the command, with
the masking caveat in §7.1), not a service `credentials:` block.

### 9.5 Release from a tag push, or from a manual dispatch

A tag push starts the runs whose `on.push.tags` globs match it (§3.1), so a release can also be a
tag-triggered workflow (below). The dispatch shape is for a team that prefers one manual button, or
needs to choose the version: dispatch it, and the job creates the tag.

```yaml
name: Release
on:
  push:
    tags: ["v*"]
jobs:
  release:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
      - name: validate the tag
        run: |
          case "$GITHUB_REF_NAME" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "not a vX.Y.Z tag: $GITHUB_REF_NAME" >&2; exit 1 ;; esac
      - run: make release VERSION="$GITHUB_REF_NAME"
```

The manual alternative keeps the tag-creation step in the job:

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

A tag pushed by a job token inside that job starts no workflow run (the `GITHUB_TOKEN` rule, §3.1),
so publish the assets from the same job or dispatch a second workflow.

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

Threads never reads `.forgejo/workflows/` or `.github/workflows/` — not as a fallback, not behind a
flag. The migration is an explicit, **one-way conversion** performed by the `spindle` binary:

```sh
spindle threads import --from forgejo --repo .                  # dry run: print the converted files
spindle threads import --from forgejo --repo . --out /tmp/x    # write them somewhere to review
spindle threads import --from forgejo --repo . --apply         # commit them on a new branch
```

`--from github` runs the same translator with the other profile (`.github/workflows/` is the source
directory; a hosted Linux `runs-on` label such as `ubuntu-latest` is carried verbatim and mapped
onto `self-hosted` + `linux` at enqueue, while `windows-*`/`macos-*` stay unmapped). The command is
read-only by default, never executes the source, never edits the source directory, never pushes, and
never commits to your current branch.

**It refuses to guess.** Everything the target language can express is translated; everything else is
listed in the conversion report **and** marked `# UNTRANSLATED:` at its own site in the converted
file, and the command exits non-zero even though the file is still written. A lossy conversion can
never look like a successful one, and `--apply` refuses to commit one unless you pass
`--allow-partial`. The report is `--json` for machines: source, output, translated count, every
untranslated item, every translation note, and `parses` (the importer validates every file with the
real parser; a file that does not parse is an importer error, never a warning).

The examples below are derived from working repositories (Weave and Scriobh), not invented.

### 10.1 The translation table

| Forgejo | Threads | Notes |
|---|---|---|
| `on: push/pull_request/schedule` | same | same shapes and the documented filter dialect (§3.1) |
| `on: push: tags: ["v*"]` | same | a tag push matching the glob starts the run (§3.1); the importer converts the filter as written, and a tags-only trigger never matches a branch push |
| `on: push: paths:` / `paths-ignore:` | same | the changed-file filter (§3.1); an `on.pull_request.paths` filter is the merge-base diff |
| `on: release` / `issues` / `issue_comment` | same | each `types:` is the emitter vocabulary in §3.6; the run reads the default branch's file |
| `runs-on: docker` | `runs-on: [self-hosted, linux, docker]` | labels are exact; match a runner's declared set |
| `runs-on: ubuntu-latest` | same spelling | mapped to `self-hosted` + `linux` and scheduled on the stock runner; no image change (`windows-*`/`macos-*` stay queued, D-15) |
| `container: {image: …}` | same | digest pins are honoured and may be required by policy |
| `uses: actions/checkout@v4` | `uses: actions/checkout` | built-in; the `@ref` is accepted and ignored by the runner, and the importer drops it |
| `with: fetch-depth: 0` | same | `fetch-tags`, `ref` and `path` are supported too |
| `actions/setup-go`, `setup-node` | base image (`golang:1.27`, `node:22`), or a `run:` step plus `actions/cache` | no marketplace actions; the cache recipe is named in the refusal (§6.4a) |
| `actions/cache` | same spelling | built in (§6.4a): `path`/`key`/`restore-keys`; a PR's writes stay PR-scoped |
| `secrets.NAME` | `$NAME` (already an env var) | `${{ secrets.NAME }}` is substituted everywhere (§6.2), and masked in the log; a whole-value step `env:` reference is delivered by **name**, which keeps the value out of argv |
| `secrets.GITHUB_TOKEN` | `$GITHUB_TOKEN` / `$SPINDLE_JOB_TOKEN` | the same automatic, scoped job token under both spellings (§7.2) |
| `github.event_name`, `github.ref`, `github.sha`, `github.repository`, `github.ref_name`, `github.actor` | `${{ github.* }}` in a step field or step `if:`; `$GITHUB_*` in scripts | the field set is closed — an unknown field is a parse error. `$GITHUB_ACTOR` and `${{ github.actor }}` are the same value |
| `forgejo.*` / `forge.*` | `github.*` | value-identical aliases |
| `$GITHUB_SERVER_URL` | same spelling | also `$GITHUB_API_URL` (`/api/v1`), `$FORGEJO_SERVER_URL` and `${{ github.server_url }}`; `$SPINDLE_SERVER` is the same origin (§6.1) |
| `github.event.inputs.x` | `github.event.inputs.x` or `inputs.x` | dispatch inputs only; the input must be declared |
| `github.event.*` | same | the documented payload projection (§6.2); `jq` at `$GITHUB_EVENT_PATH` reads the whole record |
| `vars.X` | same | repository/org variables; repository wins, values are masked |
| `runner.os`, `runner.arch`, `runner.temp`, … | same | the runner context (§6.2); Linux containers only |
| `$GITHUB_OUTPUT` | same (`$SPINDLE_OUTPUT`) | `name=value` or heredoc |
| `$GITHUB_ENV` | same spelling (alias `$FORGEJO_ENV`) | later steps of the same job only (§6.6) |
| `$GITHUB_PATH` | same spelling (alias `$FORGEJO_PATH`) | prepended to the next step's PATH (§6.6) |
| `if: startsWith(github.ref, 'refs/tags/')` | same | `startsWith`, `contains` and `format` are supported |
| `strategy.matrix.include` / `exclude` | same | GitHub's merge rules (§5.2) |
| `strategy.max-parallel` | **nothing** | no equivalent; concurrency groups cap runs, not cells |
| job `outputs` + `needs.*.outputs` | same | only a successful job reports |
| `environment:` + reviewers | same | created in Settings → Environments |
| `permissions:` | same for `contents` / `id-token` | other keys parse but do nothing. Forgejo's implicit-write token is a recorded divergence: no `permissions:` here means `contents: read` (D-12) |

### 10.2 Weave: tests, then a daemonless image

**Original** (`.forgejo/workflows/ci.yml`, excerpt):

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

Translations: `needs: test` is accepted as written (`needs: [test]` means the same);
`github.event_name == 'push'` is supported at job
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
    runs-on: ubuntu-latest          # mapped to self-hosted + linux
    container: alpine/git:latest
    steps:
      - uses: actions/checkout
        with:
          fetch-depth: 0
          fetch-tags: true
      - name: publish
        env:
          GITHUB_TOKEN: ${{ secrets.PUBLISH_GITHUB_TOKEN }}   # delivered by name
          BUMP: ${{ inputs.bump }}
          INPUT_TAG: ${{ inputs.tag }}
          TARGET: crueber/weave
        run: |
          test -n "${GITHUB_TOKEN:?PUBLISH_GITHUB_TOKEN is not set}"
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

Translations: `ubuntu-latest` and `fetch-tags: true` are supported as written (§4.3, §8.2 of
[`threads.md`](threads.md)); `github.event.inputs.bump` → `inputs.bump`; the step `env:` that maps
`GITHUB_TOKEN` to the PAT secret is delivered by **name** (the value never reaches argv), and the
authored value wins over the automatic job-token alias. Keep the secret name and read `$GITHUB_TOKEN`
in the script.

### 10.3 Scriobh: multi-job tests and a matrix release

**Original** (`.forgejo/workflows/ci.yml`, the editor job):

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
unchanged; checkout and the token handling disappear. `npm ci` runs cold unless the repository adds
an `actions/cache` step for `.npm` (§6.4a) — the base image stays the same.

**Original** (`.forgejo/workflows/release.yml`, release job):

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

- `on.push.tags: ["v*"]` is accepted with GitHub's semantics and now fires: a landed tag push
  starts the workflow, with `github.ref` = `refs/tags/<tag>` and `github.ref_name` = the tag. A
  `tags`-only trigger still never matches a branch push (§3.1). A team that prefers one manual
  release path can also create the tag from a dispatched job (§9.5).
- `$GITHUB_ENV` is supported, but only for steps of the **same** job (§6.6). `TAG` crosses into
  other jobs, so it travels as the job output `tag` and every dependent step sets
  `env: TAG: ${{ needs.prepare.outputs.tag }}` (a file write cannot cross a job boundary).
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

**Threads equivalent (the `include` list runs unmodified):**

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
permissions:
  contents: write
jobs:
  prepare:
    runs-on: [self-hosted, linux, docker]
    container: ubuntu:24.04
    outputs:
      tag: ${{ steps.tag.outputs.tag }}
    steps:
      - id: tag
        env:
          BUMP: ${{ inputs.bump }}
        run: |
          # The bump arithmetic from the Forgejo step is unchanged shell.
          # TAG crosses into other jobs, so it leaves as this job's output (a GITHUB_ENV
          # write would only reach later steps of THIS job — §6.6).
          TAG="v0.1.0"
          case "${BUMP:-none}" in
            major) TAG="v1.0.0" ;;
            minor) TAG="v0.2.0" ;;
            patch) TAG="v0.1.1" ;;
          esac
          echo "tag=$TAG" >> "$SPINDLE_OUTPUT"
  cli:
    needs: [prepare]
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    strategy:
      fail-fast: false
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
          - { goos: darwin, goarch: arm64 }
          - { goos: windows, goarch: amd64 }
    steps:
      - uses: actions/checkout
      - name: build
        env:
          TAG: ${{ needs.prepare.outputs.tag }}
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
        run: |
          set -eu
          name="scriobh-$TAG-$GOOS-$GOARCH"
          mkdir -p "dist/$name"
          suffix=""
          [ "$GOOS" = windows ] && suffix=".exe"
          CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -trimpath \
            -ldflags "-s -w -X main.version=$TAG" \
            -o "dist/$name/scriobh$suffix" ./cmd/scriobh
          tar -czf "dist/$name.tar.gz" -C "dist/$name" .
      - uses: actions/upload-artifact
        with:
          name: dist-${{ matrix.goos }}-${{ matrix.goarch }}
          path: dist/scriobh-*.tar.gz
          if-no-files-found: error
```

The `include:` list is carried as-is: a matrix with no axes yields one cell per include entry
(§5.2), so the four cells are exactly the four original combinations, and every check
(`cli (amd64, linux)` … — include-only keys render in name order) and artifact name stays distinct.

Not translated, deliberately: the release-upload step is a `run:` script that talks to the Forgejo
API. The importer carries scripts verbatim, so the job keeps the call until you move it; Threads
releases are spindle's own API, and the safe migration order moves the credential last (§10.5).

### 10.4 Exactly what the importer refuses to translate — and what it carries as-is

Every unmarked row is a **named refusal**: the conversion report lists the construct and its path,
the output carries `# UNTRANSLATED: <path>: <reason>` at that site, and the command exits `1`. A file
with a refusal is still written, so the parts that did translate can be reviewed and kept — but the
refusal is never silent, and `--apply` will not commit it without `--allow-partial`.

A row marked **carried** is not a refusal: the importer writes it unchanged, the file parses and the
command exits `0` — but the construct does not yet do what the source system did. The row records
what runs today, and the report mentions it as a note.

A row marked **supported** needs no action: the construct behaves the same in Threads and the
importer converts it as written. It stays in this table only so a reader who expected a gap finds
the answer where the other gaps are listed.

| Source construct | Why it is refused, or what it does today | What to do in the Spindle workflow |
|---|---|---|
| `on:` a trigger the **engine** cannot emit (`merge_group`, `fork`, `watch`, `status`, `check_run`, `check_suite`, `registry_package`, `gollum`, `page_build`, `branch_protection_rule`, `public`) | a named engine refusal (`on: <name>: <why>` at parse), never an accepted-and-inert key; `merge_group` waits on the merge-queue card | dispatch the work (`workflow_dispatch`) or start it from a `push`; the trigger is removed |
| `on:` a trigger the engine emits but the importer has not learned yet (`repository_dispatch`, `workflow_run`, `pull_request_target`, `create`/`delete`, `deployment`/`deployment_status`) | the engine parses and fires it (gated as §3.2/§3.7 documents); the importer reports it `UNTRANSLATED` by name today | convert, then add the trigger by hand; the importer names it in the report so nothing is silent |
| `on.push.paths` / `paths-ignore`, `on.pull_request.paths` / `paths-ignore` | **supported**: the changed-file filter (§3.1) for a push (pre-image diff) and a PR (merge-base diff) | nothing to change |
| `on.push.branches-ignore` / `tags-ignore` | **supported**: exclusion lists, and the include form with `!` mixes both (§3.1) | nothing to change |
| `pull_request.types` values the services never emit (`deleted`, `locked`, …) | the services emit exactly the vocabulary in §3.2 | the unsupported values are dropped (the supported ones are kept); restructure around the emitted actions |
| `strategy.max-parallel` | no per-job cell cap | none; a concurrency group caps *runs*, not cells |
| object-valued matrix **axes** (`matrix.os: [{name: …}]`) | the target's axes are scalars | a scalar axis (`os: [linux, darwin]`) or one `include:` object per combination; the axis is kept as one placeholder cell so references parse, and the job gets a step that fails |
| `defaults:` (workflow or job) | not in the parser | set `shell` / `working-directory` on each step |
| `run-name:` | not supported yet | the workflow `name` |
| `on.workflow_call` with typed inputs, declared secrets and output templates | **supported** (§4.5): carried as written; the callee must declare it, and its declarations are its interface | nothing to change (a mismatched declaration is a named error on the call) |
| a caller's `with:` / `secrets:` | **supported** (§4.5): `with:` is carried when it reads `github.*`/`inputs.*`; a `secrets: inherit` map is carried same-repository | nothing to change; a `${{ secrets.* }}`-in-`with:` or a cross-repo `inherit` is refused by name |
| a callee's declared `outputs:` | **supported** (§4.5): `value: ${{ jobs.<id>.outputs.<x> }}` is carried and readable as `needs.<caller>.outputs.<name>` | nothing to change |
| a cross-repository `uses:` (`owner/repo/…@<40-hex sha>`) | **supported** (§4.5) when the callee is publicly readable: explicit secrets only (D-6), `inherit` refused by name, audited | pin the full commit SHA; list every secret explicitly |
| remote / marketplace `uses:` (`owner/repo@ref`, a URL) — including the `setup-*` actions | there is no marketplace (each action is code with the job's privileges) | a base image or a `run:` step; install a toolchain in the step; for the setup actions' cache half, the refusal quotes the exact `actions/cache` recipe (§6.4a); or a repository-local `uses: ./action` / `uses: docker://image` |
| `actions/cache` | **supported** (§6.4a; nothing to translate beyond the workspace-relative `path` rule) | keep `path` inside the workspace: point the tool at `.npm`/`.cache/pip` in `env:` |
| `container.credentials`, `services.<id>.credentials` | the pull identity belongs to the runner, not the file | the runner's docker config / mirror |
| `environment` whose `name` is an expression or is not a valid name, or whose `url` is not an absolute http(s) URL | the name is a bucket key and the URL is rendered as a link | a literal name; the URL literal or omitted |
| `concurrency.cancel-in-progress` as an expression | only a literal `true`/`false` is supported | a literal, or split workflows |
| a concurrency `group` the target cannot expand (`${{ env.* }}`, `${{ secrets.* }}`, an undeclared axis or dispatch input) | the group is a lock; it fails closed rather than degrading to literal text | a group built from `github.*`, a declared matrix axis, or a declared input |
| `timeout-minutes`, `continue-on-error`, `strategy.fail-fast` as expressions | the target takes a literal number/boolean | a literal, or one job per case |
| an unknown key anywhere (the parser is strict) | it would be a typo the engine ignores | fix or remove it |
| a dispatch input type other than `string`/`boolean`/`choice`/`number`/`environment` | the target has no such type | carried as a **string** input (the name still resolves) with the original type named |
| a cron entry the scheduler cannot evaluate (an unknown macro/name/zone, seconds) | the target scheduler rejects the file | rewrite the schedule; the entry is dropped by name |
| **supported** — `on.push.tags` / a tags-only trigger | a landed tag push matching an `on.push.tags` glob starts a run (§3.1); the importer converts the filter as written, and a tags-only trigger never matches a **branch** push | nothing to change; dispatch with a `version` input if one manual release path is preferred (§9.5) |
| **carried** — `$GITHUB_ENV` | implemented for later steps of the same job only (§6.6) | nothing to change; a step cannot read its own write — use a local variable |
| **carried** — `$GITHUB_PATH` | implemented, prepended to the next step's PATH (§6.6) | nothing to change; it never replaces the image's PATH |
| **carried** — `${{ secrets.* }}` rewritten to `${X}` inside a `run:` script | the runner substitutes every documented context in every step site (§6.2), so both spellings work; the importer prefers the environment variable, which never reaches argv | nothing to change; `$NAME` / `$GITHUB_*` and `${{ … }}` are equivalent here |
| **carried** — `secrets.GITHUB_TOKEN` as a PAT for the host API | the job token is not the user's token | declare `permissions: contents: write` for git; use a real secret for API writes the job token's scope does not cover |
| **carried** — `docker build` in a job | job containers have no docker socket | buildah (or another daemonless builder) as the job image |
| **carried** — `--privileged`, `--cap-add`, `--security-opt` for a builder | the runner allowlist refuses them at plan time | the runner host must permit it without per-job flags |
| **carried** — Forgejo release/asset API calls | the step is a `run:` script, carried verbatim; spindle's API is different | publish from spindle's releases API |
| **carried** — `permissions` scopes other than `contents` and `id-token` | they parse but drive no capability yet | reported as a note; do not rely on them |

Three things are **resolved** rather than refused, because the target has an exact spelling:

- **YAML anchors/aliases** are materialized: an `*alias` becomes a copy of its anchor target, so the
  converted file carries no reference that could dangle.
- **`strategy.matrix.include` / `exclude`** are carried as written (the object list, its keys and
  values): the target expands them with GitHub's merge rules (§5.2), so an include-only matrix is one
  cell per entry and an include key becomes a declared `matrix.<key>`.
- **Mechanical rewrites** are applied and recorded as report *notes*: `needs: test` → `needs: [test]`,
  a scalar `branches`/`tags` filter → a one-element list, `runs-on: docker` → the default runner
  labels, a built-in action's `@ref` dropped, `github.event.inputs.x` → `inputs.x`,
  `$GITHUB_SERVER_URL` → `$SPINDLE_SERVER`, and `${{ secrets.X }}` inside a `run:` script →
  `${X}` (the secret is already an environment variable).

Constructs that parse but behave differently are *notes*, not refusals: a `with:` on a `docker://`
step (not applied yet; pass arguments in the image or use a local action) and an unmapped
`windows-*`/`macos-*` label (a written non-goal — the job stays queued with a named reason).

Run-time policy refusals stay named at run time: an image outside `threads.image_allow_prefixes` (or
without a digest when the instance requires one), a `container.options` flag outside the runner's
allowlist, and a workflow-supplied `volume`. Those are policy, not translation — the converted file
carries them and the job fails with the option named.

### 10.5 The migration workflow: convert → review → commit on a branch → run

1. **Convert, read-only.**
   ```sh
   spindle threads import --from forgejo --repo . --json > /tmp/import.json
   ```
   stdout is the converted YAML (one `# ==> path` header per file) and the report is on stderr; with
   `--json` stdout is the report alone. Nothing in the repository changes.
2. **Review the report and the diff.** Read every `UNTRANSLATED` line in `/tmp/import.json` and every
   `note`. Decide, per row, whether to edit the source workflow (best: the converted file should need
   no edits at all), edit the converted file, or accept the named gap. An object-valued matrix axis
   is the common case: rewrite it as a scalar axis (or one `include:` object per combination), then
   re-run step 1 until the report is clean.
3. **Commit on a new branch.**
   ```sh
   spindle threads import --from forgejo --repo . --apply
   # created spindle/import-forgejo-20261126T101500Z; nothing pushed
   ```
   `--apply` refuses a working tree with uncommitted changes, refuses an existing branch, refuses
   the current branch, refuses a branch name git rejects, and refuses a conversion with any
   untranslated construct unless `--allow-partial` is passed. It creates the branch, writes
   `.spindle/workflows/*.yml`, commits, and stops — no push, no force, no rewrite of another branch.
   Use `--branch NAME` to pick the name.
4. **Push the branch and open a pull request.** Do not start with the release job: the first PR is the
   read-only test job (labels, image, checkout reachability, duration). Add the matrix and services
   next, then artifact-producing jobs, and only then the job that holds a credential.
5. **Run it.** Merge, watch the first run on the run page, and compare the produced bytes with the
   old CI's — the digest, not the log. Add the secrets the workflow reads under Settings → Secrets
   (they are injected by name; `${{ secrets.X }}` in a script becomes `${X}`).
6. **Retire the old file last**, after a release has shipped from spindle. The rollback is
   "re-enable the old workflow", one revert.

Never start by moving the release job: it holds the credentials and is the least reversible failure.
A workflow that keeps a step on the old system (a Forgejo API call, a marketplace action with no
substitute) is fine during migration — the importer names it, and the pull request records the gap.

## 11. Troubleshooting

Start at the first row that describes what you see; each branch names the evidence to read.

| Symptom | Cause | Fix |
|---|---|---|
| **No run at all after a push** | no workflow file at that commit; `on:` does not match the ref's kind/glob (a tags-only trigger on a branch push, or the reverse); the file failed to parse; the ref was a delete or a notes ref; the push used a job token | Threads → **Workflows** lists files at the default tip with their parse errors; deletes and notes refs never fire; a job-token push is suppressed |
| **A file is rejected** | unknown key, bad type, unsupported cron, a `types` value the service never emits, an unsupported `${{ }}` reference or `if:` function, a matrix over 256 cells, both `run` and `uses`, a bad `needs:`/step id | the error names the file/key; fix that file only — the others keep running |
| **Run stays `queued`, jobs `not started`** | the job's `queue_reason` names one cause: no active runner; labels are not a superset of `runs-on`; a fork run with no `--allow-fork-runs`; all matching runners busy; a repository `concurrency_cap` hold; a live `needs:` dependency; a concurrency group is held; an approval hold | read the chip; `/admin/runners` shows labels, capacity and last-seen; the run page shows `runs-on` and the group it waits for |
| **Job failed before any step** | image refused (`threads.image_allow_prefixes` / `image_require_digest`), a bad container option or a runner grant the runner did not declare, an undefined `environment`, an unresolvable local action, a malformed reusable call | the job's `reason` names the field and value; a missing grant names the operator flag |
| **Cache never hits** | `path` is absolute/`~`; the key is derived from `github.sha`; a PR/fork scope cannot see the entry you expect | point the tool at a workspace directory; key on `runner.os` + branch and add `restore-keys` |
| **Cross-run artifact download is 409** | the consumer is a `workflow_run` follow-up whose upstream was untrusted and its approval hold is still pending | a maintainer approves that run; a fork run or a secrets-bearing job is refused by name |
| **`docker build` exits 127** | the job has no docker socket or daemon | use kaniko/buildah, or a runner whose operator granted `--docker-host` (it never takes fork runs) |
| **Job fails at checkout** | image has no `git`; the clone URL is unreachable from the container; the commit does not exist | use an image with `git` (default `alpine/git`); read the checkout URL and the reachability diagnostic; set `server.public_url` |
| **Push from a job refused** | shallow clone; `permissions: contents: read`; protected ref; non-fast-forward | add `fetch-depth: 0`, declare `contents: write`, push a fast-forward, or relax the policy |
| **A secret is empty** | the run is a fork/untrusted run; no `server.secrets_key`/`session_secret` (API `503`); wrong name | follow the `untrusted` chip; set the key; `${{ secrets.X }}` and `$X` resolve the same name (`secrets.*` is case-insensitive, the variable name is not) |
| **`***` appears in the log** | a substituted secret matched the mask list (best effort: exact substrings, values under 4 bytes skipped) | expected; never echo a secret, and prefer a whole-value `env:` entry with `$X` in the script so the value never enters argv |
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

1. **No marketplace.** `uses:` resolves exactly the built-ins (`checkout`, `upload-artifact`,
   `download-artifact`, `cache` + `/restore`/`/save`), `./path`, and `docker://image`. A toolchain is
   a base image or a `run:` step; the `setup-*` refusal names the cache recipe that replaces its cache
   half.
2. **Caches are scoped to the run's trust class.** `actions/cache` exists (§6.4a), but a PR's writes
   stay in that PR's scope, a fork's entry is never readable from the base branch, and a cache is
   derived data (7-day idle window, repository budget) — never a place to publish artifacts.
3. **Secrets are environment variables first.** Every secret is `$NAME` in a trusted step.
   `${{ secrets.X }}` resolves in every substituted site (empty on an untrusted run, masked in
   the log); a whole-value step `env:` entry is still delivered by name, which is the form that
   keeps the value out of argv.
4. **`${{ }}` is not generalized.** It substitutes the documented contexts in the sites listed
   in §6.2 — a script may also read `$GITHUB_*` and `$NAME`, which never reach argv. A structural
   field (`runs-on`, a container/service image, an environment) and a workflow/job/container/service
   `env` value never take one — only a step `env:` does: a span there is a parse error naming the
   field, not a literal that reaches the runner or the process environment.
5. **The expression language is small.** `success/failure/always/cancelled`, `startsWith`,
   `contains`, `format`, comparisons and boolean logic — no object filters. An unsupported
   expression is a parse error rather than a skipped job.
6. **Tag pushes start a run only when a workflow opts in.** `on.push.tags` has GitHub's
   semantics and a landed tag push fires the matching files; a workflow with no `tags` filter (or
   `branches` only) still ignores tags, so a repository that wants a release workflow declares one.
7. **Jobs do not get a Docker socket.** Building an image means a daemonless builder (kaniko,
   buildah) or a prebuilt image. A runner whose operator granted `--docker-host` exposes an endpoint
   to the job container only; that runner never accepts fork runs.
8. **Matrices follow GitHub's rules.** Axes plus `include`/`exclude`; no `max-parallel`; 256
   cells max.
9. **Labels are exact and subset-matched.** Hosted Linux labels (`ubuntu-latest` and friends) map
   onto `self-hosted` + `linux`; Windows/macOS labels do not map at all.
10. **Fork PRs are read-only by policy.** No secrets, the base branch's workflow file, and possibly
    an approval gate — the file cannot opt out. The token is `contents: read` unless the repository
    enabled `send_write_tokens` (maintain+, audited); `id-token` is never granted to a fork.
11. **`permissions` is a closed vocabulary.** `contents`, `issues`, `pull-requests` and
    `id-token` are the only scopes, and each drives a real capability (the job token is also a
    scoped API credential); any other scope is a named parse error, never accepted-and-inert.
    No `permissions:` means `contents: read` (Forgejo's implicit write is a recorded divergence).
12. **Everything is in the bucket.** There is no queue server and no local state; a restart
    loses warmth, not runs.

## 13. How the compatibility claims in this file are checked

Every claim here about "accepted" or "documented" is backed by a test, and the
compatibility surface has its own gate:

- **`make parity`** runs the conformance gate and the corpus. The gate reads
  [`features/parity/parity.json`](features/parity/parity.json) (one row per
  capability) and fails when a row marked **have** does not name a test that
  exists in the tree, when a gap is unplanned (no wave, no decision), when the
  master plan and the matrix disagree on which rows exist, or when the generated
  [`matrix.md`](features/parity/matrix.md) is stale. A capability cannot be
  claimed green here and untested in the code.
- **The corpus** is four real Forgejo workflows from Weave and Scriobh
  (`internal/e2e/testdata/parity/`, with a `SOURCE.json` stamp naming repository,
  path, commit and fetch date). They are converted to `.spindle/workflows/` and
  run through the parser and the engine's run-creation path: the jobs and steps
  must appear exactly as documented, and a feature that is not implemented yet
  must be a **named refusal**, not a silent acceptance. All four convert, parse
  and plan with **zero refusals** today; when a capability turns green, the
  corpus entry stops accepting the refusal in the same change.
- **The execution tier** (`make parity-exec`, docker required, opt-in) runs a
  corpus-shaped workflow through a real runner to completion. The PR gate runs
  the parse/plan tier only, so a change never needs a docker daemon to prove the
  compatibility surface.

What this means for you as an author: if this file documents a key and the
workflow parses, the behaviour is tested or the row is honestly marked partial
with a named refusal. If you hit a parse error that *is* documented as accepted
here, that is a bug in the gate, not in your workflow — report it.

## 14. Where the depth lives

| Topic | Reference |
|---|---|
| every trigger edge, filter and cron rule | [`threads.md` §3.2](threads.md#32-on), [§6](threads.md#6-triggers-in-detail) |
| containers, services, network, checkout reachability | [`threads.md` §4](threads.md#4-containers-services-and-checkout) |
| secrets, job token, fork gate, OIDC, environments | [`threads.md` §5](threads.md#5-secrets-and-permissions) |
| checks and merge gating | [`threads.md` §7](threads.md#7-checks-and-merge-gating) |
| runner flags, tenancy, capacity, isolation, mirrors | [`threads.md` §8](threads.md#8-the-runner) |
| cancel/rerun, retention, maintainer tasks | [`threads.md` §9](threads.md#9-operating-it) |
| design, protocol and frozen wire shapes | [`features/15_threads.md`](features/15_threads.md) |
