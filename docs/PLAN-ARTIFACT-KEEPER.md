# PLAN-ARTIFACT-KEEPER — Artifact Keeper as the Threads runner's image source

> Status: **plan, not implementation.** Written 2026-11-21 against `main` (`9d9ba8e`,
> worktree `plan/artifact-keeper`). No code, no config edits, no dependencies.
> Every spindle claim below is read from the tree at that commit; every Artifact
> Keeper claim is read from its published source/docs at the URLs cited and marked
> **[verified]** (read in the upstream repo today) or **[assume]** (needs the
> operator's confirmation — see §10).
>
> Ground truth read for this plan: `docs/threads.md` §4/§8, `docs/features/15_threads.md`
> (§2 payload, §6 runner, Decisions), `internal/threads/runner/{plan.go,exec.go}`,
> `cmd/spindle/cli_runner.go`, `internal/threads/{model.go,service.go,runners.go,spec.go}`,
> `internal/config/{config.go,validate.go}`, `internal/setup/{setup.go,rows.go}`,
> `internal/webui/admin.go` (`adminFieldMeta`, `BuildSchema` consumers),
> `internal/webui/templates/pages/collab/thread_detail.html`,
> `docs/features/14_packages.md` (spindle's own registry).
>
> **Artifact Keeper** = `artifact-keeper/artifact-keeper`, an open-source universal
> artifact registry (Rust backend + Postgres + S3/filesystem), ~1.3k stars, active
> (docs reference 1.12.0 features) — [github.com/artifact-keeper/artifact-keeper](https://github.com/artifact-keeper/artifact-keeper).
> Confidence: **high** that this is the service meant. **Unverified**: which instance,
> version and repository keys the operator actually runs (§10).

## 0. Decision at a glance

| # | Shape | Spindle code | Covers | Verified win | Recommendation |
|---|---|---|---|---|---|
| **L0** | Docker **registry mirror** on the runner host (`daemon.json` → Keeper's default-mirror repo) | **none** | Docker Hub only | rate limits, cold-start, central scan | **do first** (M0) |
| **L1** | Runner **image-prefix rewrite** (`--image-prefix`) | runner flags only | every registry | full coverage, one pull identity | **do second** (M1) |
| **L2** | Server **image policy** at run creation (`threads.image_allow_prefixes`, `threads.image_require_digest`) | config + `internal/threads` | all jobs, all events | closes the same-repo-PR image hole | **do third** (M2) |
| L3 | Server passes the allowlist **in the job spec** | wire + runner | — | none over L2 | **reject** (§3.5) |
| L4 | Spindle's own registry as the source, Keeper as its mirror | large | — | none today | **reject** (§3.6) |
| L5 | Keeper as a proxy for non-image traffic (git, LFS, artifacts) | large | — | none | **reject** (§10) |

Sequence: L0 (one day, no repo change) → L1 (runner flags) → L2 (the actual security
control). L0 alone is a real win and is the honest minimum; L2 alone is the security
win and works without any Keeper at all (an allowlist can name `docker.io`).

## 1. What the problem is, exactly

A Threads job image comes from exactly two places (`internal/threads/runner/plan.go`):

1. the workflow's `container.image` (`plan.go:149`), passed verbatim to
   `docker create … --entrypoint /bin/sh <image>` (`plan.go:324-326`);
2. otherwise the runner's `--image` / `SPINDLE_RUNNER_IMAGE`, default
   `alpine/git:latest` (`plan.go:28`, `cmd/spindle/cli_runner.go:52`).

`services.<id>.image` is a third, easily forgotten path (`plan.go:372`). The runner
never pulls explicitly: `docker create` pulls implicitly when the image is absent and
**does not refresh a tag that is already present** (no `--pull` flag is passed), and it
authenticates from the docker CLI's own config, inherited through
`DockerCLIProcess` (`exec.go:47`, `cmd.Env = os.Environ() + env`). So today:

- every runner host pulls from Docker Hub (and whatever else workflows name) directly,
  on its own IP, with no cache, no rate-limit budget and no scanning;
- a `docker create` that hits a rate limit or an auth wall is a **job setup error**
  with docker's text and nothing else (`exec.go:279-280`);
- the resolved image is never recorded — the run page shows the *requested* ref
  (`thread_detail.html:94`), so "which bytes ran" is unanswerable after the fact.

### 1.1 Which wins are real, and which are speculative

| Claim | Verdict | Why |
|---|---|---|
| **Rate-limit insulation** | **real** | anonymous Docker Hub pulls are IP-limited; a CI host that pulls `golang`, `node`, `postgres` for many jobs on one IP hits it. A shared cache pulls once. |
| **Cold-start latency** | **real but narrower than it looks** | the local daemon layer cache already makes warm starts free. The win is *cold* hosts (ephemeral runners, `docker:dind` per job — the isolation shape `docs/threads.md` §8.3 recommends), new image tags, and fan-out across N hosts. |
| **Pinned digest allowlist** | **real, and the biggest one** | see §1.2 — a same-repository PR runs a workflow it authored, with repository secrets, and can name any image. Today nothing stops it. This is a hole in the current gate, not a Keeper feature request. |
| **Per-repo image namespaces** | **speculative** | images are not secrets; a private namespace per spindle repo means N credentials for zero containment gain. One pull-only identity for the fleet is enough (§4.2). |
| **Central vulnerability scanning** | **real but secondary** | Keeper scans proxied images (Trivy adapter, `fail_closed` available). Useful as a policy input; it is not why a CI platform mirrors. |
| **Air-gap / multi-site** | **speculative for this product** | a self-hosted git host may be air-gapped; if it is, Keeper's peer replication is the answer and this plan's L0/L1 are unchanged. |

### 1.2 The trust asymmetry (why L2 matters more than the cache)

`docs/threads.md` §1: a **fork** `pull_request` reads its workflow from the **base
branch**, gets no secrets, and gets `contents: read`. So untrusted fork code cannot
choose its own image — already correct.

A **same-repository** `pull_request` is a *trusted* event: secrets are delivered and the
workflow file comes from the PR head. Anyone who can push a branch (a collaborator, a
compromised account, a bot with write access) can therefore make CI run
`container: {image: attacker/collector:1}` with the repository's secrets in the job
environment. A digest pin does not fix this; an **allowlist of image prefixes** does,
and it is enforced by the server, not the runner.

## 2. What Artifact Keeper actually provides

Verified against the published tree today:

| Capability | Detail | Source |
|---|---|---|
| **Remote (proxy) repos with pull-through caching, Docker/OCI included** | one repository key = one upstream; `<ak-host>/<repo_key>/<image path>` | README format table; `backend/src/api/handlers/oci_v2.rs` `requested_repo_key`/`resolve_repo_inner` **[verified]** |
| **dockerd `registry-mirrors` mode** | `AK_DEFAULT_DOCKER_MIRROR_REPO=<key>`: a pull of `/v2/library/postgres/manifests/…` that matches no repo key falls back to that proxy repo — "makes dockerd's `registry-mirrors` config work end-to-end" | `oci_v2.rs:3634-3643`, `resolve_repo_inner_with` step 2 **[verified]** |
| **Docker Hub path normalization** | `dockerhub/golang` → upstream `library/golang`; `dockerhub/alpine/git` → `alpine/git` | `normalize_docker_image`, `candidate_upstream_images` **[verified]** |
| **Cache lifetimes** | mutable paths (an **OCI tag**) 300 s then conditional revalidation; immutable (an **OCI digest blob**) ~10 years; `stale-if-error` grace default 3600 s with `Warning: 110` + `X-AK-Stale-Since` | `docs/proxy-cache-freshness.md` **[verified]** |
| **Repository visibility** | `public` (anonymous read, enforced on `/v2/*`), `internal`, `private` | `docs/repository-visibility.md` **[verified]** |
| **Pull-only credentials** | repo-scoped API tokens (`POST /api/v1/repositories/{key}/tokens`, scopes + expiry), and `allowed_repo_ids` confinement with an existence-hiding 404 | `backend/src/api/handlers/repo_tokens.rs`, `repository-visibility.md` **[verified]** |
| **Upstream auth to Docker Hub** | credentialed Remote needs `oci_trusted_bearer_realms: ["https://auth.docker.io"]`, else the token request is anonymous (GHSA-78h6-3wp8-2542) | `docs/oci-trusted-bearer-realms.md` **[verified]** |
| **Cache hit/miss metrics** | `ak_proxy_cache_lookups_total{repository,result}`, `ak_proxy_cache_publish_failures_total` | `docs/operations/proxy-cache-metrics.md` **[verified]** |
| **Scan-on-proxy gate** | `scan_on_proxy`, `proxy_scan_action: fail_open\|fail_closed\|record_only`, `block_on_policy_violation`, `severity_threshold`, quarantine | `docs/security/scan-on-proxy.md` **[verified]** |
| **CI OIDC token exchange** | `POST /api/v1/auth/ci/token` → short-lived token for a service account; providers `gitlab`, `github`, `kubernetes`, `generic` | `docs/ci-oidc.md` **[verified]** — **not usable here**: spindle mints no OIDC ID token, and Keeper has no spindle provider (§9). |

Consequences worth stating plainly:

- **L0 works with zero spindle and zero runner changes.** Configure the Docker remote in
  Keeper, set `AK_DEFAULT_DOCKER_MIRROR_REPO`, put `registry-mirrors` in the runner
  hosts' `/etc/docker/daemon.json`. Docker Hub traffic is then cached, rate-limit
  insulated and scannable, and every workflow keeps its image names verbatim.
- **L0 covers Docker Hub only.** `dockerd` consults a mirror only for Docker Hub; a
  workflow naming `ghcr.io/owner/img` still goes direct. That is L1's job.
- **Keeper's mutable-tag TTL is 300 s**, so "the tag moved" reaches CI within five
  minutes — not instantly, and not at all if the daemon already holds the tag (§6.3).

## 3. Integration shapes

### 3.1 L0 — dockerd mirror (zero code) — recommended first

Operator-side, once per runner host:

```json
// /etc/docker/daemon.json  (runner host; daemon restart required)
{ "registry-mirrors": ["https://ak.example"] }
```

Plus, on the Keeper: a Remote/Docker repository (e.g. key `dockerhub`) with
`upstream_url: https://registry-1.docker.io`, `oci_trusted_bearer_realms:
["https://auth.docker.io"]` if a Hub credential is configured, visibility `public` (CI
pulls are not secret), and `AK_DEFAULT_DOCKER_MIRROR_REPO=dockerhub`.

Spindle's only deliverable is documentation: a "Mirroring job images" subsection in
`docs/threads.md` §8.5 and a decision line in `docs/features/15_threads.md`.

### 3.2 L1 — runner image-prefix rewrite — recommended second

New runner flags (below, §5.1). Rule, applied to `container.image`,
`services.*.image` and the runner's own default image:

- split the ref into `[registry/]path[:tag|@digest]`;
- a ref **has a registry** when its first path segment contains `.` or `:`, or equals
  `localhost` (docker's own rule);
- if it has none, and `--image-prefix` is set, rewrite to `<prefix>/<path>[tag|@digest]`
  (`golang:1.27` → `ak.example/dockerhub/golang:1.27`);
- if it has one, leave it alone — unless `--image-prefix-all` is set, in which case the
  registry host is replaced too (`ghcr.io/a/b` → `ak.example/<ghcr-mirror>/a/b`, which
  needs a matching Keeper repo per upstream registry; **off by default**).

Why a prefix and not "make authors write the Keeper": existing workflows keep working,
and the operator can swap the mirror for a different one without editing every repository.

### 3.3 L2 — server-side image policy — recommended third (the security control)

Two keys under `[threads]`, enforced where the workflow becomes a job
(`internal/threads/service.go` `createOne`, next to the existing `needs`/`if`
pre-failure path):

- `threads.image_allow_prefixes` — a list of literal ref prefixes. Empty list = no
  restriction (today's behaviour, and the only compatible default). A non-empty list
  refuses any job whose `container.image` or `services.<id>.image` does not start with
  one of the entries at a `:`/`@`/end boundary.
- `threads.image_require_digest` — when true, those images must carry `@sha256:`.

Refusal is a **job-level failure with a reason**, not a silently disabled workflow: the
job record is written `failure` with `Reason: "image \"x\" is not allowed by
threads.image_allow_prefixes (…) — allowed: …"` and the check mirror is red. The
`Job.Reason` field already exists for exactly this class of decision
(`internal/threads/model.go:332-336`, rendered at `thread_detail.html:92`), so **no
bucket schema change and no runner change**. A fork run takes the same check; it simply
never gets the secrets that made the hole interesting.

Scope note, stated in the docs: the policy governs *workflow-named* images. The runner's
own `--image` is the operator's declaration — pin it by digest in the runner's unit or
container definition (`--image ak.example/dockerhub/alpine/git@sha256:…`).

### 3.4 Credentials reach the runner without touching argv

There is no new credential plumbing. `docker create` reads the docker CLI's config from
`$DOCKER_CONFIG` (or `~/.docker/config.json`), and `DockerCLIProcess` already passes the
whole process environment through (`exec.go:47`). So:

- Keeper repo **`public`** → no credential at all for pulls (recommended: job images are
  not secret, and an anonymous pull path removes an expiry failure mode);
- Keeper repo **`private`/`internal`** → a repo-scoped, pull-only token
  (`POST /api/v1/repositories/<key>/tokens`, scopes `read`, expiry ≤ 90 days) written into
  a 0600 `config.json` and pointed at by `DOCKER_CONFIG` in the runner's environment (a
  systemd `Environment=` line, a container env var — never a flag, never argv, never
  `docker login -p` in a job step);
- the runner token (`wrt_`) and the Keeper token stay separate identities; do not reuse.

### 3.5 Rejected: the server passes the allowlist in the job spec

Enforcing at `createOne` *is* the server-side decision, made earlier and with the run's
event context. Shipping the list inside `FetchResult.Spec` adds wire surface and per-job
bytes so the runner can re-apply a rule the server already applied to a payload the
runner trusts unconditionally. The trust root is the spindle server; a runner that
distrusts it cannot be fixed by an allowlist field. **Reject.**

### 3.6 Rejected: spindle's own registry as the image source

`internal/packages` is a real OCI registry, but `docs/features/14_packages.md` §1 lists
**pull-through caching** as an explicit v1 non-goal, the package name *is* the repository
name (`<host>/<owner>/<repo>`), and there is no upstream, no cache, no scanning and no
rate limiting. Using it as the Hub mirror would mean building pull-through into spindle —
a second registry, not an integration. Keeping Keeper *behind* spindle's registry is the
same build with an extra hop. **Reject for now**; revisit only for the narrow case of
"a trusted workflow builds an image, a later job consumes it by digest", which is a
Threads feature (job-produced images), not a Keeper one.

## 4. Failure modes

| Failure | What the operator sees | Design response |
|---|---|---|
| **Keeper down / unreachable** | `docker create … exit 125: …` — docker's error only | M1 adds a named runner diagnostic on the create failure: the mirror prefix, the fallback state, and the fix (same shape as the checkout reachability diagnostic in `docs/threads.md` §4.4). **Fail closed** is the default: a silent bypass would defeat L2. |
| **Keeper up, Docker Hub down** | pulls still succeed for cached immutable blobs; mutable tag lookups serve stale for the grace window (`Warning: 110`) | document `stale_if_error_grace_seconds`; no spindle change |
| **Auth expired / token revoked** | `/v2` 401 → `docker create: unauthorized: …` | a *named* diagnostic on 401 specifically ("the runner's DOCKER_CONFIG credential for <host> was refused"), because this is the one failure an operator must fix, not retry |
| **Upstream tag moved** | nothing, until 300 s (Keeper) and never (daemon holds the tag: `docker create` without `--pull` does not re-resolve) | pin the default image by digest; `--image-pull=always` for operators who want mutable-tag freshness (costs a manifest round trip per job) |
| **Image deleted/renamed in Keeper** | `404 NAME_UNKNOWN` in the job log | dictionary-style hint in the diagnostic: "the Keeper repository key prefix no longer exists" |
| **Plain-HTTP Keeper on a LAN** | `http: server gave HTTP response to HTTPS client` | documented daemon requirement: `insecure-registries` alongside `registry-mirrors`; prefer TLS |

## 5. Config surface

### 5.1 Runner flags (`spindle runner serve`) — L1

Follows the existing flags ⊕ env pattern in `cmd/spindle/cli_runner.go`; names must be
added to `runnerGroup.commands[serve].flags` (`cli_runner.go:170`) or `--help` drifts.

| Flag | Env | Default | Validation |
|---|---|---|---|
| `--image-prefix` | `SPINDLE_RUNNER_IMAGE_PREFIX` | `""` (off) | no scheme, no trailing `/`, must look like `host[:port]/path`; rejected at startup with the flag name |
| `--image-prefix-all` | `SPINDLE_RUNNER_IMAGE_PREFIX_ALL` | off | requires `--image-prefix`; refusing the combination beats a confusing no-op |
| `--image-pull` | `SPINDLE_RUNNER_IMAGE_PULL` | `missing` | enum `missing\|always\|never` → `docker create --pull=<v>` (docker ≥ 23; verify the operator's CLI in M0) |
| `--image-require-digest` | `SPINDLE_RUNNER_IMAGE_REQUIRE_DIGEST` | off | digit-less image ⇒ the job fails at plan time with a named reason |

All four are pure plan/argv changes (`plan.go`), so they are unit-testable without a
daemon — the existing `plan_test.go` argv assertions are the template.

### 5.2 Server keys (`[threads]`) — L2

`internal/config/config.go:345` (`Threads` struct) + `internal/config/validate.go:91`:

```toml
[threads]
# Literal image-ref prefixes a job may run. Empty = unrestricted (compatible default).
image_allow_prefixes = []
# Require @sha256: on every workflow-named image.
image_require_digest = false
```

Validation: entries must be non-empty, contain no scheme and no whitespace, and match
`^[a-z0-9._:/-]+$`; a duplicate is dropped. Errors name the key
(`threads.image_allow_prefixes`, the field lesson: a coercion/validation failure without
its key cannot mark its row).

**Administration page:** no template work. `setup.BuildSchema` is reflection over the
`Config` struct (`internal/setup/setup.go:144-180`), so a new `[threads]` field appears in
its own `threads` group automatically, and the page's save/coerce/restart-keys path
(`setup.Coerce`, `setup.RestartKeys`) treats it like any other key. Two additions in
`internal/webui/admin.go` `adminFieldMeta` (`admin.go:887`):

```go
"threads.image_allow_prefixes": {Type: "list", Example: "docker.io/library, ak.example/dockerhub",
    Note: "a job's container.image and every services.*.image must start with one of these; empty allows any image (today's behaviour)"},
"threads.image_require_digest": {Type: "bool", Example: "true", Advanced: true,
    Note: "require @sha256: on workflow-named images — a moved tag cannot change what CI runs"},
```

Both are restart-required by construction (any changed schema key appears in
`RestartKeys`), which is correct: the policy is read where the run is created.

### 5.3 CLI

None. `spindle runner` is the client; the server policy is config, not a verb. If an
operator wants to preview the decision, `spindle threads show` already renders the job's
`container` and `reason`.

## 6. Security

### 6.1 Digest pinning vs mutable tags

- `:tag` is a mutable pointer: Keeper revalidates it every 300 s, and the daemon **never**
  re-resolves it once present. Two runs of the same commit can be different bytes.
- `@sha256:` is immutable in Keeper (~10 y TTL) and is what makes a CI result reproducible.
- Therefore: the runner's default image **must** be digest-pinned in the operator's
  config (L1 doc requirement), `--image-require-digest` is the per-runner enforcement, and
  `threads.image_require_digest` is the instance-wide enforcement. Roll the instance key
  out as **off by default**: turning it on breaks every existing workflow that names a
  tag, so it is an operator decision with a documented migration (`docker buildx imagetools
  inspect` → pin).
- An allowlist of prefixes does **not** imply pinning: `ak.example/dockerhub/golang:*` is
  prefix-allowed and still mutable. Keep the two keys independent and say so on the page.

### 6.2 `imagePullPolicy` semantics

There is no Kubernetes here: `--image-pull` maps to `docker create --pull`. The three
values mean exactly what docker means — `missing` (default, today's behaviour), `always`
(manifest round trip per job; the only way a moved tag reaches CI), `never` (offline
audits; a missing image fails the job). **Never** is the one that makes a run reproducible
without digests, and it is also the footgun that turns a Keeper outage into an
instant failure — document both.

### 6.3 Credential scoping

- One **pull-only** identity for the whole runner fleet, scoped to the images repository
  (`read` scope, `allowed_repo_ids` = that repo), never an admin token, never a token with
  `write`. It lives in a `DOCKER_CONFIG` file at 0600, outside the job container.
- The job container never receives it: the runner's steps get `$SPINDLE_JOB_TOKEN` and
  secrets by name (`exec.go:459-483`), and an image pull happens in the daemon, before the
  container exists.
- Keeper's `public` visibility for an image repo is a deliberate trade — anonymous pull
  — and must not extend to any repository that holds private base images.

### 6.4 What a compromised Keeper can do

It can serve different bytes under a tag that CI runs, i.e. **arbitrary code execution as
the runner's docker daemon** — which on the default host-socket setup is root on that
host (`docs/threads.md` §8.5). That is the strongest argument for one rule:

> **A Keeper is a code-execution trust root, exactly like Docker Hub. It must never be
> reachable-with-write by an untrusted workflow, and a job image must be digest-pinned
> wherever the operator can afford it.**

Concretely: no `contents: write` job pushes to the images repository (push to it only
from an operator/trusted-pipeline identity), `scan_on_proxy: fail_closed` if scanning is
wanted (accepting the latency), and a compromised Keeper is contained by digest
pinning + `--image-pull=never` on audit runs, not by the allowlist (which the Keeper
host itself could satisfy).

## 7. Observability

**Runner log** (job start line, one line, beside the existing "checkout from …" line):

```
image ak.example/dockerhub/alpine/git@sha256:1f2a… (mirror ak.example/dockerhub, pull=missing)
image a1b2c3d4e5f6 (ak.example/dockerhub/golang:1.27 — no repo digest on a local image)
```

Source of truth: one `docker image inspect --format '{{index .RepoDigests 0}}' <id>` after
`docker create` (local, ~10 ms). `.RepoDigests` is empty for an image loaded from a tar or
built locally, so the line falls back to the image ID (`.Image`) — say which it printed.

**Keeper cache hit/miss is not visible to the runner** and must not be guessed: it is
`ak_proxy_cache_lookups_total{repository,result}` on the Keeper's `/metrics`
(§2). Document the two PromQL queries (hit ratio per repo; publish failures) in
`docs/threads.md` §9 as the operator's cold/warm signal.

**Run page**: `job.Container` (the requested ref) already renders
(`thread_detail.html:94`). M3 adds, in this order of value: the resolved digest as a
second muted token next to it, and a "policy" chip when the job was refused at creation
(the `Reason` it already shows). Do **not** invent a "cache hit" badge — the runner does
not know.

## 8. Milestones

| M | Deliverable | Effort | Green when |
|---|---|---|---|
| **M0** | Operator guide only: `docs/threads.md` §8.5 "Mirroring job images" (L0 `daemon.json` + Keeper repo recipe incl. `oci_trusted_bearer_realms`, TLS/`insecure-registries`, digest pinning of `--image`), a decision line in `docs/features/15_threads.md` | 1 day, **no code** | an operator follows the page on a clean host and a workflow's `golang:1.27` pull is served by the Keeper (proved by a Keeper access log / `ak_proxy_cache_lookups_total` increment), and `docker pull ak.example/dockerhub/golang:1.27` resolves (§10 Q3) |
| **M1** | Runner flags: `--image-prefix`, `--image-prefix-all`, `--image-pull`, `--image-require-digest`; ref-rewrite helper in `plan.go`; `--pull=` in `ContainerArgv`; named diagnostics on create failure (mirror unreachable / 401 / 404); `docs/threads.md` §8.1 table + §8.6 mirror section | 3–4 days | table-driven `plan_test.go` cases for every rewrite shape (`alpine/git:latest`, `golang:1.27`, `localhost:5000/a/b@sha256:…`, `ghcr.io/a/b:t`), argv assertions for `--pull`, and the three diagnostics tested through the `fakedocker` double; `make test`, `make race`, `make cover` green |
| **M2** | `threads.image_allow_prefixes` + `threads.image_require_digest`: struct, defaults, `validate.go` rules, the policy pass in `createOne`, `adminFieldMeta` rows, docs (`docs/threads.md` §3.3 workflow reference + `docs/features/15_threads.md` decision) | 4–6 days | tests: empty list = unchanged behaviour (the compatibility case), allowed/denied matrix over `container.image` **and** `services.*.image`, digest-required on/off, a fork run refused with `Reason` and no secrets, the check mirror red, and the admin page round-trip (`TestSettingsWiringBindsEveryRenderedSeam`-style: the two keys render, save, coerce, and appear in `RestartKeys`) |
| **M3** | Resolved digest in the job-start log + `job.Container` line on the run page; optional Keeper cache note in the docs | 2–3 days | log line asserted from the fake docker double; page render at desktop and ~390 px with one digest-pinned and one digest-less job |

**Measurement harness (M0's evidence, and `docs/EVIDENCE.md` if it makes a latency
claim).** k6 is the wrong instrument — it is an HTTP load generator, and the thing being
measured is `docker create` wall time. The harness is a small shell/Go driver:
`internal/devtools/imagemirror` (same place as the other reproduction harnesses) doing,
for each of {direct Hub, Keeper cold, Keeper warm} × 5 repetitions:
`docker image rm -f <ref>` → wall-clock `docker create --pull=always <ref> /bin/true` →
`docker rm -f` → record seconds, bytes (`docker image inspect --format '{{.Size}}'`), and
any 429 in docker's output. Claims it can support: cold start with the mirror vs without,
and steady-state bytes saved. It **cannot** measure Keeper's own hit ratio — that is
Prometheus, cited separately.

**Risks.** (1) Availability: L0/L1 make CI depend on one more service; mitigate with
digests + `--image-pull=missing` (default) so a warm daemon survives an outage.
(2) Upstream churn: Keeper is at 1.12.x and moving; the fields this plan names
(`oci_trusted_bearer_realms`, `AK_DEFAULT_DOCKER_MIRROR_REPO`, the scrape metrics) are
version-pinned in the doc so an upgrade that renames one is visible.
(3) Silent divergence: a 300 s mutable-tag window plus a daemon that never re-resolves
means "dev pulled it, CI did not" is possible; digest pinning is the only real fix.
(4) L2 rollout: an allowlist that omits a legitimately used registry breaks CI on the
first push after the change — ship it empty, and document the "add a prefix" runbook next
to the key.
(5) Storage: Keeper grows with every distinct image; its lifecycle policies are the
operator's problem, but the doc should say CI pulls are the growth driver.

## 9. What this must NOT become

- **Not a second spindle registry.** Spindle's `internal/packages` keeps its scope; no
  pull-through, no upstream proxying, no mirroring code lands there (§3.6).
- **Not a runner-side credential broker.** The runner reads a docker config file and
  nothing else: no token exchange, no per-job Keeper credentials, no `docker login` in a
  step, no Keeper API calls from the runner beyond what docker itself makes (§3.4).
- **Not a proxy for non-image traffic.** Git, LFS, Threads artifacts, releases and the
  API stay on spindle; routing them through Keeper would put a second auth boundary in
  front of the product's own data.
- **Not an authorization engine.** The allowlist answers "may this workflow run this
  image", nothing about who may push images; image publishing stays a repository
  permission (`internal/packages`) or an operator-owned Keeper repository.
- **Not a spindle↔Keeper SSO integration.** Keeper's CI OIDC exchange has no spindle
  provider and spindle mints no OIDC ID token; a static pull-only token is the whole
  integration until spindle grows OIDC issuance for another reason.
- **Not a soft dependency in the test suite.** No test may require a running Keeper; the
  unit tests are plan/argv and policy tables, and the measurement harness is
  operator-run.

## 10. Open questions for the owner (the §0 assumptions to confirm)

1. **The instance**: URL, version, and whether a Docker/OCI Remote already exists — and
   its key. This plan assumes one Hub proxy repo, key `dockerhub`, visibility `public`.
2. **Does your daemon config allow `registry-mirrors`?** L0 needs a daemon restart on
   every runner host; if runners are ephemeral containers, the mirror has to be in the
   image or the `dind` entrypoint instead, which changes M0's shape but not the plan.
3. **Is `ak.example/dockerhub/golang:1.27` (single-segment official image) served
   correctly?** Source says yes (`normalize_docker_image` → `library/golang`), but this is
   the one thing that breaks every workflow if it is wrong, so M0 must prove it with a
   real pull before anything else is planned on top.
4. **Docker CLI version on the runners** — `--pull` needs ≥ 23.
5. **Which upstreams matter**: Hub only (L0 suffices), or also `ghcr.io`/`quay` (L1 with
   `--image-prefix-all` and one Keeper repo per upstream).
6. **Scanning**: is a `fail_closed` proxy scan wanted for job images? It changes CI
   latency and adds a failure mode; it is a policy answer, not a Keeper question.

## 11. Decisions & deviations (implementation log)

M0 + M1 + M2 landed 2026-11-22 (`feat/artifact-keeper-l1l2`). Everything else in this plan is
unchanged; M3 (the resolved-digest job-start line, the run-page digest token, the optional Keeper
cache note) remains open. Where reality forced a deviation, it is recorded here:

- **The mirror section is `docs/threads.md` §8.6, not §8.5.** §8.5 is already "Running the runner in
  a container"; M0 and M1 named different numbers for the same new section, and the next free
  subsection is the only coherent reading. §8.1's flag table and §3.3's workflow reference point at
  it.
- **The allowlist boundary set includes `/`.** §3.3 says "a `:`/`@`/end boundary", but the plan's own
  example (`image_allow_prefixes = ["docker.io/library", …]`) must allow
  `docker.io/library/golang:1.27` — its next byte is `/`. The implemented boundary is `/`, `:`, `@`
  or end; without `/` every repository-path entry in the plan's own example would match nothing.
- **A host-less image is matched in its canonical Docker Hub spelling too.** "Literal ref prefixes"
  alone would make `docker.io` (the plan's §0 example: "an allowlist can name `docker.io`") useless
  for the dominant spelling `golang:1.27`. The policy matches the authored reference and its
  docker-canonical form (`docker.io/library/golang:1.27`), so `docker.io/library` allows both
  spellings. It is the same reference under docker's own grammar, not an alias across registries;
  the canonical form only ever names `docker.io`. A literal entry naming the image directly
  (`golang`) still works.
- **`--pull=` is always passed explicitly, including the default.** §5.1 writes `--pull=` in
  `ContainerArgv` with `missing` as the default; the runner therefore always emits `--pull=missing`
  rather than omitting the flag, so the effective policy is observable and testable. This needs a
  docker CLI whose `docker create` lists `--pull` (verify with `docker create --help`; the docs
  assume 23+). Job and
  service containers both carry it.
- **`--image-prefix-all` drops the original registry host.** §3.2's example
  (`ghcr.io/a/b` → `ak.example/<ghcr-mirror>/a/b`) implies it; the implementation makes it explicit
  and tests it: one mirror repository per upstream, named by the prefix itself, and the original
  host never appears in the rewritten path.
- **Docker Hub's implicit `library/` is not inserted by the rewrite.** §3.2's example
  (`golang:1.27` → `ak.example/dockerhub/golang:1.27`) is what the runner does; inserting
  `library/golang` would target a path a Hub proxy configured for the single-segment spelling does
  not serve. The mirror (or docker itself) applies its normalisation — §10 Q3 still has to be
  proved with a real pull.
- **Duplicate allowlist entries are tolerated, not dropped.** §5.2 says "a duplicate is dropped".
  The validator never mutates its input, and matching is set-like, so a duplicate has no effect;
  reporting it as an error would break an idempotent page save for no gain.
- **A job a job-level `if:` already skipped is not refused by the image policy.** No image is pulled
  for a skipped job, so refusing it would turn a legitimate skip into a failure. A dependent job
  whose condition only resolves in the `needs:` cascade is refused at creation like any other
  (stricter, never looser).
- **An entry that cannot be evaluated refuses the job (fail closed).** The service re-validates
  every allowlist entry instead of trusting the config validator; a policy error is a job failure
  whose reason says it could not be evaluated. An invalid `--image-prefix` is a runner startup
  error naming its flag, never a per-job surprise.
- **The gate is re-applied on rerun.** §3.3 places it where a workflow becomes a job (`createOne`),
  but a rerun creates a new attempt of an existing job without re-creating it — so a policy
  tightened after the run was created could be bypassed with the re-run button. `RerunRun`
  re-evaluates the same per-job check before requeueing; a refused job becomes `failure` with the
  named reason and a run with nothing left to run folds to failure. `JobUpdate` now applies an
  explicit `Reason` after `ClearTimings` resets the previous attempt's, which is what lets one
  update clear stale step results and state the refusal together.
- **Bare-host entries are rejected; `host[:port]/path` is the shape.** Both the server allowlist and
  the runner prefix require at least one path segment (§5.2's "normalised `host[:port]/path`
  literals"). An operator allowlisting a whole registry writes `docker.io/library` (or another
  path); a prefix that is just a host cannot name a mirror repository.
- **`cmd/spindle/cli_runner.go` and `cmd/spindle/threads.go` were touched** (the four serve flags
  and the config→service wiring). The file partition named only `internal/**` and the docs, but the
  flags live in the CLI and the service fields are set where the service is constructed; there is
  no second construction site.
- **The named image diagnostics cover services and the unknown class.** §4 asks for three classes on
  the job container; the same helper runs for every sidecar create, and docker text that matches no
  class gets no image diagnosis (docker's own error is still shown). The success-path job-start
  line with the resolved digest is M3 and is not in this change.
