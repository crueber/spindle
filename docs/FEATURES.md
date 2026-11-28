# Features

> What spindle does, capability by capability, with the deep doc for each. Setup, the first-run
> OIDC card, backup/restore and the compose examples live in the [README](../README.md);
> the command-line surface is [`CLI.md`](CLI.md). Everything here has one architectural
> constraint in common: the object store is the only database, so an instance is disposable and
> "wipe it" loses nothing but warmth.

## 1. Git hosting

Repositories speak **smart HTTP v0 and v2** and **SSH**. Over HTTP the advertisement is
`GET /{owner}/{repo}[.git]/info/refs?service=git-upload-pack|git-receive-pack`, with the protocol
version chosen by the client's `Git-Protocol` header; fetches are `POST …/git-upload-pack`, pushes
`POST …/git-receive-pack` (which requires the `.git` suffix). Over SSH the same repositories are
served by `server.ssh.listen` (e.g. `0.0.0.0:2222`) at
`ssh://git@host:2222/<owner>/<repo>.git`, with public keys added on the **/keys** page or with
`spindle user keys add`; the host key lives in the data dir, so it survives restarts and container
recreation. Every git operation is a `git` subprocess with a fixed argv — the wire details are
[`go/04_git.md`](go/04_git.md) and [`go/17_ssh.md`](go/17_ssh.md).

Around that core:

- **Git LFS** — the batch API (`POST …/info/lfs/objects/batch`, `application/vnd.git-lfs+json`),
  per-object `GET|HEAD` and `PUT …/info/lfs/objects/{oid}`, and `…/info/lfs/verify`, capped by
  `lfs.max_object_bytes` ([`go/06_server_http.md`](go/06_server_http.md) §5).
- **Bundle-uri** — `…/bundles/list?filter=blob:none`, `…/bundles/catchup`, and
  `…/bundles/{strategy}/{name}`, so a blobless clone bootstraps from prebuilt bundles instead of a
  full pack; strategies and slots are [`go/08_bundles.md`](go/08_bundles.md).
- **Source archives** — `GET|HEAD /{owner}/{repo}/archive/{ref}.tar.gz` and `.zip`, produced by
  `git archive` in a bounded pool ([`go/04_git.md`](go/04_git.md) §11.3).
- **Push policy** — the per-repository `policy.json` rule language (`protect`, `history`, `size`
  effects, fail-closed parse, ordered rules) evaluated before any forward or sync work; read, save
  and clear it with `spindle repo policy get|set --file F|clear`, and dry-run a proposal through the
  API ([`go/14_extensibility.md`](go/14_extensibility.md), the policy section of
  [`go/06_server_http.md`](go/06_server_http.md) §4.4).
- **Auto-create on push** — `server.auto_create_on_push` is `true` in the zero-config first-run shape (the compiled default is
`false`), so the first push to
  a new `owner/name` registers the repository; `spindle repo create OWNER/REPO` is the explicit
  path.
- **Import, mirrors and forks** — one-shot import from a local git directory or a URL
  (`spindle import --from GITDIR owner/name`, `spindle import --url URL owner/name`, with ref
  filters and provenance recorded in `meta/import.json`), pull-only mirrors with scheduled upstream
  syncs and push mirrors (`repos/<o>/<r>/meta/{mirror,pushmirror}.json`), and in-instance forks with
  a "Sync fork" driver. Surfaces: Settings → Mirror / Push mirror, the fork page,
  [`features/10_git_import.md`](features/10_git_import.md),
  [`features/11_mirror.md`](features/11_mirror.md),
  [`features/13_push_mirror.md`](features/13_push_mirror.md), and `internal/forksync` (PLAN-V1 WP5).

**Bucket-native state.** Refs, packs, the WAL, policy, access bindings and every collaboration
object are objects under `repos/<owner>/<repo>/` (plus the instance-root families `users/`,
`orgs/`, `meta/`, `packages/`, `threads/`, `moderation/`). There is no SQL, no Redis, no queue
server and no search index: disk and memory are caches, the manifest CAS is the only commit point,
and a push is never acknowledged before the bucket acknowledges it. Key layout, protobuf field
numbers and JSON shapes stay byte-compatible with the Rust walgit implementation, pinned by golden
fixtures in the packages' `testdata/` directories ([`go/02_storage_protobuf.md`](go/02_storage_protobuf.md)).

## 2. Collaboration

The collaboration layer is a parallel object family with its own CAS discipline — it never produces
WAL entries and never gates a push except where a policy effect explicitly consults it. Every
feature is a set of objects under a top-level prefix, so it inherits the same durability,
replication and backup story as git itself. The normative primitive list (P1–P9, including the
shared issue/PR numbering space) is
[`features/README.md`](features/README.md#p1--the-collaboration-object-family).

- **Issues** — immutable event log plus a CAS'd header: comments, labels, milestones, assignees,
  close/reopen, templates, pinning and dependency links. Shared numbering with pull requests (an
  issue and a PR never take the same number) ([`features/02_issues.md`](features/02_issues.md)).
- **Pull requests and review** — the PR head is an ordinary ref (`refs/pull/N/head`), mergeability
  is computed on demand, merging is a task (merge/squash/rebase, optional head-branch deletion),
  and reviews carry approve / request-changes / comment verdicts plus line-anchored threads and
  review requests ([`features/03_pull_requests.md`](features/03_pull_requests.md),
  [`features/04_code_review.md`](features/04_code_review.md)). `spindle pr …` covers list, show,
  diff, create, close, merge, review, comment and checks.
- **Checks and statuses** — external CI posts commit statuses and check runs; a repository can
  require checks before merge, and Threads reports its jobs into the same surface
  ([`features/05_checks_statuses.md`](features/05_checks_statuses.md)). `spindle checks list|status`,
  `spindle pr checks`.
- **Notifications** — subscriptions and mentions, delivered in the UI and fanned out to webhooks
  ([`features/06_notifications.md`](features/06_notifications.md)).
- **Releases and stars** — releases with markdown notes, assets (streamed upload up to
  `releases.max_asset_bytes`, 2 GiB by default) and one release per tag; stars and watches on
  repositories ([`features/07_releases_stars.md`](features/07_releases_stars.md)).
  `spindle release list|show|create|delete|upload`.
- **Organizations and teams** — org profiles, members, teams, repository role bindings in
  `access.json`, and invitations
  ([`features/01_identity_permissions.md`](features/01_identity_permissions.md)). `spindle org …`.
- **Moderation** — user blocks, comment minimize/hide and conversation locks, abuse reports with
  per-repository and host queues, rate limits and advisory spam signals, all audited
  ([`features/16_moderation.md`](features/16_moderation.md)).
- **Browser file edit** — create, edit and delete one text file as one commit, directly or on a new
  branch with a PR, through the same push funnel and gates as a CLI push
  ([`features/17_file_edit.md`](features/17_file_edit.md)). `spindle file put|create|delete`.
- **Insights** — a cached pulse projection (commits, contributors, code frequency, issue/PR counts)
  with an honest statement that spindle collects no traffic data
  ([`features/18_repo_insights.md`](features/18_repo_insights.md)).

## 3. Threads (CI)

Threads runs CI from `.spindle/workflows/<name>.yml` in the repository. Writing one — the key
reference, recipes, the Forgejo migration cookbook and a troubleshooting tree — is
[`WORKFLOWS.md`](WORKFLOWS.md); the exhaustive reference is [`threads.md`](threads.md).
Workflow files are triggered by `push` (branch and tag pushes, filtered independently by
`branches`/`tags` and by changed files through `paths`/`paths-ignore`), `pull_request` (the full
emitted lifecycle vocabulary: opened/synchronize/reopened, closed/merged, edited,
ready_for_review/converted_to_draft, labeled/unlabeled, assigned/unassigned,
milestoned/demilestoned; every edge's run is the server-computed merge commit), `pull_request_target`
(per-repository opt-in, D-2), `repository_dispatch`, `workflow_run` (with a per-run approval hold for
an untrusted parent), `merge_group` (a merge-queue entry's run; the queue itself is
[`features/22_merge_queue.md`](features/22_merge_queue.md)), `create`/`delete`, `deployment`/`deployment_status`, `release`, `issues`,
`issue_comment`, `pull_request_review` and `pull_request_review_comment`, the label and milestone
**definition** events, `fork`, `watch`, `check_run` (an external CI report only — Threads' own
mirrored check never re-fires it), `workflow_dispatch` with typed inputs (`string`, `boolean`,
`choice`, `number`,
`environment`), and `schedule` (five-field cron with names, macros, `L`/`NW`/`#` and a per-entry
timezone); jobs run in containers with services,
`needs:` ordering, `strategy.matrix` (including GitHub's `include`/`exclude` merge rules),
reusable workflows — local or SHA-pinned cross-repository, with typed `on.workflow_call` inputs,
secrets and outputs — per-cell `job.status` and `strategy.*` contexts, and `concurrency` groups (one
running plus one pending run),
report into the checks surface that gates merges, and
stream logs over SSE with cancel, rerun and rerun-failed. `actions/cache` is built in with
trust-class scopes (default/branch/PR/fork, D-10), and artifacts take the v4 inputs, support a
maintainer-approved cross-run read (D-3), and have a list/download/delete API. `permissions:` mints a
scoped job token (`$SPINDLE_JOB_TOKEN`, also delivered as `$GITHUB_TOKEN`/`$FORGEJO_TOKEN`) whose
closed vocabulary (`contents`, `issues`, `pull-requests`, `deployments`, `packages`, `checks`,
`statuses`, `id-token`) is also its
platform-API authority — `packages` is exchanged at `/v2/token` for a short-lived registry token
scoped to the job's own repository (`read` pulls, `write` pushes), and `checks`/`statuses` read and
write check runs only for that repository; `read-all`/`write-all` set the three repository baselines
and name none of the feature scopes — and, with `id-token: write` (or Forgejo's
`enable-openid-connect: true`) on a
trusted run,
a short-lived audience-scoped OIDC ID token for cloud providers; secrets are write-only and masked,
at three scopes — organization (with an `all`/`selected` repository policy), repository and
environment — resolved environment > repository > organization and recorded on the job by scope
only, with no user scope by decision (D-22), and never exposed to fork PRs (a fork's token is
`contents: read` unless the repository enabled
the audited `send_write_tokens` switch), and first-time fork contributors need approval. Deployment
environments add required reviewers (users or teams, at most six), a wait timer,
prevent-self-review, a deployment branch/tag allowlist, environment-scoped secrets and variables:
a job names one with `environment:`, and nothing starts — and no environment secret is resolved —
until every listed reviewer (or a repo admin) approves the deployment (Settings → Environments).
Deployments are records with statuses and a serialization slot per environment, readable and
writable through `…/api/threads/deployments` under the `deployments` token scope. A per-base **merge
queue** (Queue tab, PR panel, and `…/api/merge-queue`) orders entries by priority then FIFO, checks
one entry at a time through the documented `merge_group` ref, and lands it through the one merge task
when the required checks on the checked merge commit are green; a failure, conflict or refusal ejects
the entry with a named reason and deletes the temporary ref
([`features/22_merge_queue.md`](features/22_merge_queue.md)). Schedules can be
disabled/enabled and run on demand, CI can be paused, and a manual dispatch may set a queue
priority; failed runs notify watchers in-app (with an optional webhook, and an operator-configured
SMTP email channel that is off by default, D-27). Checks carry annotations,
a per-workflow badge and run-list filters, and one job can be cancelled without its siblings.
Runners declare a capacity, an optional one-job (ephemeral) mode and CPU/memory ceilings; groups
narrow what a runner sees; drain/resume/rotate/deregister operate the fleet; a managed pool,
hosted Windows/macOS, LXC/host backends and artifact attestations are signed non-goals (D-14/D-15).

Runs, jobs and steps, from the outside in: one **run** per workflow file per event (the file pinned
to a path, commit and sha256), one **job** per `jobs:` key or matrix cell (one container, one
workspace volume, one runner claim, reported as the check `threads/<workflow>/<job>`), and one
**step** per `steps:` entry (one `docker exec` in that container). A run's state folds from its jobs,
and a job that never ran because its `if:` was false or its `needs:` dependency failed is `skipped`,
not `cancelled`.

### 3.1 Workflows, triggers and secrets

The workflow is read **from the pushed commit**, so the push that introduces the file triggers the
run itself. Only `.spindle/workflows/*.yml` and `*.yaml` are read (one directory, no nesting, no
`.github/` fallback), each capped at 1 MiB.

A run is **trusted** or **untrusted**, and that is server-side policy a file cannot opt out of:

| Run | Workflow read at | Trusted | Secrets | Job token |
|---|---|---|---|---|
| `push` (client push or PR merge) | the pushed commit / merge commit | yes | yes | per `permissions` |
| `pull_request`, same repository | the head commit | yes | yes | per `permissions` |
| `pull_request`, fork | the **base branch tip** | no | none | `contents: read` |
| `workflow_dispatch` | the resolved ref's commit | yes | yes | per `permissions` |
| `schedule` | the default branch tip | yes | yes | per `permissions` |
| `release` | the **default branch tip** (the run reports the tag commit) | yes | yes | per `permissions` |
| `issues` / `issue_comment` | the **default branch tip** | yes | yes | per `permissions` |
| `pull_request_target` (opt-in, D-2) | the **base branch tip** | yes | yes | per `permissions` (a head checkout needs the second switch) |
| `repository_dispatch` | the **default branch tip** | yes | yes | per `permissions` |
| `workflow_run` | the **default branch tip** | yes unless the upstream was untrusted, which holds the run | yes | per `permissions` |
| `merge_group` | the **base branch tip** (the run reports the queue ref and its head commit) | yes | yes | per `permissions` |
| `create` / `delete`, `deployment` / `deployment_status` | the **default branch tip** | yes | yes | per `permissions` |

`push` fires for `refs/heads/<branch>` and for `refs/tags/<tag>`, filtered independently by
`branches` and `tags` (a tags-only trigger never matches a branch push, and the reverse); branch and
tag deletes create no run, and neither does a notes ref. Secrets are
managed in **Settings → Secrets** on the repository or organization page, or the API, at
**organization** (an `all`/`selected` repository policy), **repository** and **environment** scope;
they are write-only (the API returns names and timestamps, never values), stored sealed with
AES-256-GCM, and injected as environment variables of their own name in every step of every job.
Precedence is **environment > repository > organization**, and the supplying scope is recorded on the
job as `secret_scopes` (never a value); there is deliberately **no user scope** (D-22: a run executes
as the repository, not a person). Masking is server-side and best-effort under one rule shared with
the runner: a value of 4+ bytes is replaced wherever it appears (longest first), a **multi-line**
value is masked whole and line by line, and a value **shorter than 4 bytes** only as a whole token;
the server re-masks every stored frame and step result, so an older runner cannot write a value into
the bucket. A runner that encodes, splits or derives a value still defeats it, and step outputs
(`$SPINDLE_OUTPUT`) are data, not logs, so a secret must never go into one. A fork run's payload
carries `secrets: {}` and
`secrets_denied: true`. The job token is scoped to one attempt of one job in one repository:
always read of its own repository, plus push only when the effective `permissions:` say
`contents: write` — and a push authenticated by it never starts a workflow run, so a
`contents: write` workflow cannot rebuild itself forever. A fork PR from a first-time contributor is
created `pending_approval` (jobs written, checks mirrored as `pending`, nothing enqueued) and waits
for a maintainer's approve or deny unless the author already has a merged PR in the repository.
Details: [`threads.md`](threads.md#51-repository-secrets),
[`threads.md` §5.1b](threads.md#51b-organization-secrets),
[`threads.md`](threads.md#52-the-check-every-run-passes-through),
[`threads.md`](threads.md#53-the-job-token), [`threads.md`](threads.md#54-the-approval-gate).

**Migrating and checking a file.** A repository that still carries only `.forgejo/workflows` or
`.github/workflows` gets a display-only **migration banner** on its Threads page: the foreign files
it found and the exact `spindle threads import --from forgejo --repo .` (or `--from github`)
command. The banner parses and imports nothing and creates no run, and the engine still opens only
`.spindle/workflows`. The one-way importer writes the converted files and a report;
`spindle threads validate [PATH|-] [--repo DIR] [--json]` then checks them offline with the server's
own validator, so the `file:line: key: message` text is the one a push (and the page editor) would
report. It exits 0 when every file parses, 1 when one is invalid or unreadable, and 2 for a usage
error.

Job OIDC tokens are off unless the job asks: `permissions: id-token: write` (never `write-all`), a
trusted run only, then `SPINDLE_ID_TOKEN_REQUEST_URL` + `SPINDLE_ID_TOKEN_REQUEST_TOKEN` (and the
`ACTIONS_*` spellings) let the job `POST {"audience": "…"}` for an RS256 five-minute JWT whose
issuer is `server.public_url`, with `sub` = `repo:<owner>/<name>:ref:<ref>` and the run/job claims
([`threads.md`](threads.md#55-job-oidc-tokens-cloud-identity)). Environments are repository-scoped
deployment targets with required reviewers (users or teams, write or more, checked when saved, at
most six) and their own secrets, which override repository and organization secrets of the same name
and reach only a job that names the environment in a trusted run with an approved deployment; the
run is created
`pending_approval` and nothing is enqueued until every listed reviewer (or a repo admin) decides.
Wait timers, prevent-self-review, and the deployment branch/tag allowlist are part of the same gate
([`threads.md`](threads.md#56-deployment-environments)).

### 3.2 Steps and actions

A step is a `run:` command (or a group: `background`/`wait`/`cancel`/`parallel`, D-7) or a `uses:`
of the built-in `actions/checkout`, `actions/upload-artifact`, `actions/download-artifact` or
`actions/cache` (+ `/restore`, `/save`), a **local composite action**
(`uses: ./.spindle/actions/setup`, resolved at run creation from the run commit; `github.action_path`
is the innermost action's directory), a **`docker://` image** (`uses: docker://alpine:3.20`), or a
**remote action** (`owner/repo[/path]@ref`, a URL, or a `setup-*`) when the instance's
`[threads.actions]` policy admits it: the server pins the ref to a commit, hashes the content,
embeds it in the job and records both on the run (`action_loads`); the runner never fetches. The
policy defaults to allowlist mode with a full commit SHA required outside it — never an implicit
marketplace trust. JavaScript (`node20`/`node24`, with a declared runtime minimum), Dockerfile/
registry and nested remote composite actions are supported; a local composite's metadata still
refuses a remote `uses:` by name. Artifacts pass files between jobs of the same run, and another run
of the same repository only behind the cross-run approval (D-3); caches persist a workspace tree
across runs with trust-class scopes. The reference is
[§3.4 steps](threads.md#34-steps), [§3.4a local actions and docker images](threads.md#34a-local-actions-and-docker-images)
and [§3.4e remote actions](threads.md#34e-remote-actions-par-070071072074).

### 3.3 Runners

A runner executes the jobs. Runners are **instance-scoped**: a host administrator mints a one-time
registration token in the browser at `/admin/runners` together with the runner's **repository
policy** — every repository, or an allowlist of `owner/name` / `owner/*` patterns — and the runner is
offered only the repositories that policy allows (it can never change it; changing it is revoke +
re-mint). There is no runner admin surface in the CLI (static machine credentials never
administer; a user `wgt_` token still re-derives its owner's admin verdict on the API), so there are
no `runner token|list|revoke` commands; `runner serve --register` is the operator step after a
browser mint. Start the runner
binary; it needs a Docker daemon, and the stock host socket must be acknowledged with
`--allow-host-docker`:

```sh
spindle runner serve --server https://spindle.example.com --register wrg_… \
  --token-file /srv/spindle/runner.token \
  --labels self-hosted,linux,docker --capacity 1 \
  --allow-host-docker   # only when that daemon is the host socket

# afterwards the token file is enough
spindle runner serve --server https://spindle.example.com \
  --token-file /srv/spindle/runner.token \
  --labels self-hosted,linux,docker --capacity 1 --allow-host-docker
```

Labels are **exact, comma-separated strings matched as a subset**: a runner claims a job only when
it declares every label the job's `runs-on` names (no wildcards or aliases; both sides are
normalized to lower case, so matching is case-insensitive). The default set is
`self-hosted,linux,docker`, so declare it in the workflow:

```yaml
jobs:
  test:
    runs-on: [self-hosted, linux, docker]
    container: golang:1.27
    steps:
      - uses: actions/checkout
      - run: go test ./...
```

The default job image is `alpine/git:latest` (it carries `git`, which checkout needs inside the job
container); a stock label (`ubuntu-latest`, `node`, `python`, `go`, `rust`, `docker`) with no
`container.image` resolves to a digest-pinned upstream image, and `spindle runner images --label`
resolves one entry and refuses an unknown label by name. `--capacity 1` per runner is the
isolation-preserving default for untrusted code; scale
out with more runners rather than a bigger capacity. A job that pushes must check out with
`fetch-depth: 0`. A job with a label no runner declares simply stays queued — the run page names the
one `queue_reason` (no runner, labels unmatched, all busy, a fork with no fork-opted runner, a cap or
approval hold) and `/admin/runners` shows the declared sets. Every flag and its `SPINDLE_RUNNER_*`
environment variable is tabulated in [`threads.md`](threads.md#81-flags-and-environment); there is
deliberately no repository-policy flag.

Container capability is the operator's, never the workflow's: `--container-privileged`,
`--container-network`, `--container-valid-volume` and `--docker-host` each admit one shape for every
job the runner takes, and any one of them refuses
`--allow-fork-runs`. A private image pull uses the runner's own identity — its daemon config, an
`--image-prefix` mirror, or `--registry-auth-file` (password on stdin) — never workflow
`services.*.credentials`. Daemonless image builds (kaniko, buildah) need no grant; `docker build`
needs `--docker-host` on a disposable host.

A fleet is more than a list of processes: an admin-set **runner group** adds inherited labels and a
visibility allowlist (intersected with the runner's own policy, so a group can only narrow);
`--ephemeral` makes a one-job worker whose record and credential are deleted after its terminal
attempt; `--max-cpus`/`--max-memory` declare a ceiling that becomes an exact queue reason. One
process can serve several `[[runner]]` scopes from a `--config` file (0600 credentials), and
`runner drain|resume|rotate|deregister` are ordinary operations (rotation keeps a 15-minute overlap).
`spindle runner images` prints the stock toolchain catalogue; the images are upstream toolchains, not
published by spindle (PAR-177). The reference is [`threads.md` §8.3a–8.3b](threads.md#83a-registration-scopes-and-runner-groups-par-172par-175).

### 3.4 Runner in a container

The published image is also the runner (same binary) and ships the `docker` CLI — the CI container
contract asserts `docker --version` — so the runner can be a container on the host whose daemon it
drives:

```sh
docker run -d --name spindle-runner --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  -v /srv/spindle/runner.token:/run/secrets/runner.token:ro \
  ghcr.io/crueber/spindle:latest \
  runner serve --server https://spindle.example.com \
    --token-file /run/secrets/runner.token \
    --labels self-hosted,linux,docker --capacity 1 --allow-host-docker
```

`--group-add` is how the image's nonroot user (uid 1000) opens the socket and reads a token file
`chown`ed to it, and `--allow-host-docker` stays required for the host socket (job containers are
siblings on that daemon). If job containers cannot resolve your server's name, set
`server.public_url` to an address they can reach — see [`threads.md`](threads.md#44-checkout).

### 3.5 Image policy

A workflow's `container.image`, every `services.*.image` and every `docker://` image (a step's or a
local docker action's) can be constrained instance-wide on **Administration → Configuration**, in the
`threads` card:

- `threads.image_allow_prefixes` — a comma-separated allowlist of literal `host[:port]/path`
  prefixes, e.g. `docker.io/library, ghcr.io/acme, ak.example/dockerhub`. A job naming any image
  outside it is **refused at creation**: a failed job whose reason names the image, its field and the
  allowlist, with a red check; nothing is enqueued.
- `threads.image_require_digest` (**Advanced**) — require `@sha256:` on every workflow-named image, so
  a moved tag cannot change what CI runs. It refuses tag-named images too, so pin the images first.
  The runner's own `--image` is the operator's declaration and neither key governs it.

Both are **supply-chain hygiene, not a sandbox**: an allowed, digest-pinned image still runs as the
job, with the job's token and secrets. They decide which bytes may be pulled — the allowlist matches
the reference the workflow authored, not a runner's mirror rewrite — and they do not contain the code
that then runs. The mirror these keys pair with is
[`threads.md`](threads.md#86-mirroring-job-images).

### 3.6 Security

- A runner that mounts the host docker socket is **root-equivalent infrastructure**. Job containers
  are **siblings** on that daemon — the runner holds the socket, not the job containers — so a
  container escape or a daemon-level bug is root on the host, exposing every job and secret it ran.
- The minted **repository policy is tenancy, not isolation**: it decides which repositories reach a
  runner's host (and a runner cannot widen it), not what a job can do once it runs. Put the runner
  and the daemon it drives on a **host whose compromise must not touch the git host** — no store
  credentials, no admin session, egress limited to spindle and the registries — and see
  [the dedicated-worker shape](threads.md#the-dedicated-worker-shape-the-safe-topology-for-untrusted-code)
  for the safe topology, rootless/sandboxed runtimes and the `DOCKER_HOST` rules.
- Run untrusted code only on a **dedicated or disposable host**, never the git host, and keep
  `--capacity 1` per daemon.
- **Fork runs are off by default.** A fork PR runs the base branch's workflow file with a read-only
  checkout and no secrets, but its step scripts are the fork's code; only a runner started with
  `--allow-fork-runs` (on a disposable host) accepts one. Without such a runner the fork job stays
  queued. A repository-level `send_write_tokens` switch (maintain+, audited) can widen the fork
  token's scopes — never its secrets and never `id-token`.
- **Capability grants are runner-side and mutually exclusive with fork runs.** A workflow cannot ask
  for `--privileged`, a host volume or a docker endpoint; an operator can grant them to a runner, and
  a runner that grants privilege or a docker endpoint refuses `--allow-fork-runs`. Private image
  credentials are the runner's (`--registry-auth-file`, password on stdin into a private
  `DOCKER_CONFIG`), never a workflow field.
- The image allowlist and digest rule are hygiene, not isolation (above), and only
  admin-registered runners exist at all — the browser mint at `/admin/runners` is the entire
  authorization to execute instance jobs.

The full boundary — the container option/mount allowlist, the fork-run rules and the residual-risk
statement — is [the security boundary](threads.md#the-security-boundary); capacity shapes are
[§8.3](threads.md#83-capacity-and-isolation) and the container runner
[§8.5](threads.md#85-running-the-runner-in-a-container).

Full reference — workflow keys, triggers, containers/services, secrets and permissions, the runner's
flags and `SPINDLE_RUNNER_*` environment, schedules, retention, approval and troubleshooting — is
[`threads.md`](threads.md); the design and as-built record is
[`features/15_threads.md`](features/15_threads.md).

## 4. Packages (OCI registry)

Each repository is also an OCI registry under its own path. The package **is** the repository, so its
permissions are the repository's: `pull` uses the read gate, `push` is the same gate as `git push`.

```sh
docker login spindle.example.com -u <user> -p <token>
docker push spindle.example.com/acme/demo:1.4.2
docker pull spindle.example.com/acme/demo@sha256:<digest>
```

Tags are CAS pointers and manifests are content-addressed; multi-arch indexes are stored like any
other manifest; deleting a manifest digest removes that version and the tags aimed at it. With
`server.auth.anonymous_read` on (the default), public repositories allow anonymous pull. The same
registry is exposed by the **Packages** tab (`/<owner>/<repo>/packages`), the
`/<owner>/<repo>/api/packages…` JSON API, the `repos.js` SDK and `spindle packages list|show|delete`;
the `packages-gc` maintainer sweep reclaims unreferenced blobs. `packages.max_blob_bytes` defaults to
10 GiB. The registry requires the same credential as `git push` (session, `wgt_` token or a static
machine token) — anonymous callers get the Docker token challenge.

Under the hood: blobs and manifests live at `repos/<o>/<r>/packages/registry/{blobs,manifests}/sha256/…`
with tags as CAS JSON pointers and chunked uploads staged under `uploads/<id>/data` (TTL, GC'd);
the `/v2` protocol subset (ping, manifests, blobs, chunked upload with resume, `tags/list` paging,
`/v2/token`) is what Docker, Podman, `buildx` and `crane` need. Auth is Basic or the standard Bearer
challenge → `/v2/token`, whose short-lived (5 min) HMAC-signed token carries the requested scopes and
whose `push` scope is granted only when `CheckPush` — the git receive-pack gate — passes. Per-repository
quota and tag retention are stored counters, not a second index. Protocol, routes, error codes and
the five surfaces: [`features/14_packages.md`](features/14_packages.md).

## 5. Static sites

A repository's **committed bytes** can be served read-only from a **dedicated static host** on its
own listener:

```
https://<static-host>/<owner>/<repo>/            → the ref's index.html
https://<static-host>/<owner>/<repo>/docs/       → docs/index.html
https://<static-host>/<owner>/<repo>/assets/app.css
```

There is no build step, no template engine, no per-site configuration file and no per-site redirect
rules — what is committed is what is served (a CI job may produce the tree; spindle does not learn
Jekyll). Resolution follows GitHub Pages: a bare `/{owner}/{repo}` is a `301` to the slashed form, a
directory resolves `index.html` then `index.htm` (never a listing), a path that is missing but has an
`x.html` sibling is served with `200`, and anything else is the site's own `404.html`
with status `404` (or a one-line plain-text 404 when there is none). Dotfile segments, symlinks and
gitlinks are **never** served; paths are case-sensitive. Responses carry a pinned content-type
table with `X-Content-Type-Options: nosniff`, a strong `ETag` (the blob's git object id) with
`If-None-Match` → `304`, `Cache-Control: public, max-age=600` on 200s and their 304 revalidations (404s and 301s carry no cache
headers), and
`Accept-Ranges: bytes` with single-`Range` `206`/`416` and `HEAD`.

Configuration is a global section plus a per-repository one:

```toml
[static_sites]
enabled        = false              # off by default: the listener is not started at all
address        = "127.0.0.1:8081"   # must differ from server.listen
host           = "static.localhost" # the ONLY Host this listener answers for (port-insensitive)
max_file_bytes = "8MiB"             # larger files are a 404 in v1
index_files    = ["index.html", "index.htm"]

[static_site]                       # per repository, in the repo settings document
enabled = true                      # *bool: absent/false = not published
ref     = ""                        # "" = the default branch tip
root    = ""                        # "" = repository root; narrows to e.g. "public" or "docs"
```

Validation fails closed: `enabled` requires a non-empty `host`, a parseable `address` and
`address != server.listen`; a per-repo `ref` must be a valid ref name and `root` a clean relative
path with no `..`. An unreadable or invalid settings document means **not published**. The global
section lives in `spindle.toml` (Administration → Configuration); the per-repo section lives in the
repository settings document, editable in Settings → Effective config & history.

**Why a separate host, not a path on the app.** Serving user HTML on the application origin is
account takeover: same-origin script can `fetch` the app's API with the visitor's session, read
pages for CSRF tokens and reach non-HttpOnly storage — `SameSite` does not help, because the
request is same-site *and* same-origin. Ports do not isolate cookies (RFC 6265 cookies are
host-scoped), so the isolation comes from the **hostname**: the listener is cookie-less, answers
only when `Host` equals `static_sites.host`, serves **public repositories only** (a private repo is
a plain 404, so existence is not leaked and there is no login redirect loop), and never mounts the
session middleware, CORS, page provider or API routes.

Everything — the resolution table, response semantics, the request/subprocess budget, the residual
cookie-tossing risk and the v1 non-goals — is
[`features/19_static_sites.md`](features/19_static_sites.md).

## 6. Search

Search answers "where is this text?" over what spindle already holds — one
repository's tree at a resolved commit, the repositories a caller can read under
one owner, and the issue/PR threads — with **no index, no database and no new
dependency**: every code answer is one `git grep` subprocess against the serving
copy the browser, blame and compare views already use, and every issue answer is
the existing index, event and moderation reads under a bounded text scan.

```sh
spindle search "func main" -R acme/demo
spindle search TODO --owner acme --path internal/ --limit 20
spindle search 'errgroup\..*' --regex --case -R acme/demo
```

The API, page, CLI and SDK surfaces:

- `GET /{owner}/{repo}/api/search` (and the `/api-browser` twin) — repository
  scope: `q`, optional `ref` (default branch, tag or sha), `regex`, `case`,
  `word`, repeatable `path` and `limit`.
- `GET /api/v1/search?owner=…` — the same envelope across the repositories the
  caller can read under one owner: at most 50 candidates, warm-only, with a
  per-repository `not_warm`/`not_searched`/`failed` status instead of a silent
  omission. Instance-wide code search is not offered in v1: without an owner the
  cost is every repository on the instance and cannot be bounded honestly.
- `/{owner}/{repo}/search` and `/search?owner=…` — native GET forms (no
  JavaScript), file-grouped results, every match linked to
  `/{owner}/{repo}/blob/{sha}/{path}#L{line}` and its matched span wrapped in
  `<mark>` by the server renderer. The repository header carries a compact
  search box; there is deliberately no masthead box, because a global box
  without owner context would promise instance-wide code search.
- `spindle search` and the SDK's `repo.search` / `client.search` — the same
  handlers, rendered for a terminal or returned as the envelope.

Every answer echoes the normalised `options`, the resolved `ref` and `sha`, and
the completeness fields (`complete`, `truncated`, `timed_out`, and a
per-repository `status`): a match cap, a byte cap, a fired deadline, a cold
repository or a candidate the request deadline never started is a **field**,
never a silently short list. Gating is the existing read gate
(`identity.CheckRead`), so denied repositories contribute no name, no count and
no reason; both pages use the Code page's gate (an invisible repository is a
404 page) and both API lanes answer `Cache-Control: no-store` with no ETag,
because the result is principal-, ref- and query-dependent.

`type=issues|all` covers issue and pull-request titles, labels, authors, states
and kinds from the shared index, plus body/comment text under a bounded scan
(newest 50 threads, 500 events per repository) folded through moderation
**before** matching — a hidden body can never be matched, counted or excerpted.

The engine is a deliberate decision, not a stopgap: a full-tree query is linear
in file count and a capped broad query is flat ([`EVIDENCE.md`](EVIDENCE.md)
E21). An index starts a new planning doc when measured p95 exceeds 1.5 s on the
median repository, when a repository that must be searchable is remote-served,
when instance-wide code search becomes a requirement, or when ranking,
symbol/definition search or exact whole-tree counts are wanted. Until then,
grep on the serving copy is exact at the resolved sha, adds no push-path round
trip and no bucket family.

## 7. API, SDK and web UI

Three front doors over the same state — JSON API, SDK, web UI — plus the API browser that documents
the first and the Administration area that operates the instance.

- **JSON API** — `/{owner}/{repo}/api` (and the `/api-browser` lane for browser sessions): repo
  summary, refs, resolve, tree, blob, commits, commit, diff, policy, settings, overview, ops, tasks
  and every collaboration feature's endpoints. Long work answers as **SSE** when the request sends
  `Accept: text/event-stream`: an opener comment, `notice` / `progress` / `task` packets, then
  exactly one terminal `result` or `error`, with a `: keepalive` every 10 s; a client that
  disconnects stops only the writing, and the render lands in the cache. Errors are plain text;
  lists are `[]`, never `null`; every cacheable GET belongs to one of three classes — immutable
  (`private, max-age=31536000, immutable`, sha-addressed), ref-dependent
  (`private, max-age=0, stale-while-revalidate=60` + ETag), or mutable collaboration
  (`private, no-cache` + a version ETag) — so user-mutable state revalidates on every read. Wire
  contract: [`go/07_api.md`](go/07_api.md); the route table is
  [`go/06_server_http.md`](go/06_server_http.md) §5.
- **SDK** — `repos.js`, a dependency-free ES module served at `/repos.js` (module MIME is
  load-bearing) and bundled from `web/sdk/src/`: repo reads plus `repo.issues`, `repo.pulls`,
  `repo.checks`, `repo.releases`, `repo.packages` and the rest, Node-tested module by module. The
  browser shares the visitor's session; scripts use the same API through it
  ([`features/08_ui_sdk.md`](features/08_ui_sdk.md)).
- **Web UI** — server-rendered Go `html/template` pages enhanced with vendored htmx and small
  vanilla-ES islands (`web/src/islands/`) for theme, menus, copy, SSE and submit keys; no UI
  framework, no client data layer, no client-side router. Markdown, unified diffs, commit graphs and
  syntax highlighting are rendered **server-side in Go**, so user text is escaped and sanitized on
  the server; README, issue, PR and release-note markdown follows GitHub (tables, task lists,
  relative links and images, sanitized `<details>`, `<video>` and `align=`). The htmx SSE extension is deliberately not used (the island keeps a fetch stream
  with cookies, backoff and auth handling). The design language is Precision Dark
  ([`style-guideline.md`](style-guideline.md)).
- **API browser** — `/api` renders the route table with live discovery (`GET /api/v1`), so you can
  see what the instance actually serves before writing a call. `spindle api PATH` is the same thing
  from a terminal, with `--json` (`curl`-equivalent: `-X`, `-d`, `-H`).
- **Administration** — `/admin` (Overview), `/admin/config` (every setting, with restart-required
  keys linked to their rows), `/admin/access`, `/admin/maintenance` (health, packages GC, Restart),
  `/admin/directory` (accounts and organizations), `/admin/runners` (instance runners and the image
  policy), reports and the audit log. Administration is OIDC-group gated (browser only) and open
  only in the restricted first boot; the CLI's machine credentials cannot administer.

