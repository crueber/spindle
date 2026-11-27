# Development

spindle is one Go binary over one object store, built from a tree where the Make rules are the only task
runner and the tests are the definition of done. This file is the working manual: prerequisites, build,
the package map, the rules that shape the code, the verification ladder, CI and publishing, and where the
reasoning behind each subsystem is written down.

The showcase and day-to-day operator view is [`../README.md`](../README.md); [`../AGENTS.md`](../AGENTS.md)
is the law (laws, working rules, field lessons) and this file never overrides it — where the two differ,
AGENTS.md wins and this file is corrected.

## Prerequisites

| Tool | Version | Why |
|---|---|---|
| Go | 1.27 | `go.mod` states `go 1.27.0`; CI reads the toolchain from that file (`go-version-file`). |
| Node | 22 or newer (24 works) | `node --test` runs the JS logic tests, and `vite`/`esbuild` build the assets. Node 22 executes a bare directory positional as a module, so every entry point passes files or a glob (`web/test/unit/*.test.js`). |
| pnpm | 11 | CI activates `pnpm@11.25.0` with corepack. `web/pnpm-workspace.yaml` carries `allowBuilds: { esbuild: true }` — pnpm 11 needs it, and the `pnpm` field in `package.json` is dead. |
| git | ≥ 2.47 at runtime, newer for the gate | The server refuses to start below 2.47 (`pack.writeReverseIndex`, `--rev-index`, bundle-uri are load-bearing), and every git operation is a subprocess of that binary. The gate needs more than any released Debian ships — the pull path uses `git replay --ref-action` — which is why CI's gate container is a digest-pinned `debian:sid`. |
| Docker | any recent | `make dev-store` (rustfs for the S3 contract), `make image`, and the Threads runner's job containers. |
| Chromium | optional | `make a11y` drives it over CDP. Without a browser the suite skips locally; CI sets `SPINDLE_A11Y=1`, which makes a missing browser a **failure**. |
| GNU coreutils `timeout` | any | The long-running recipes are watchdog-wrapped via `T5`/`T15`/`T30` in the `Makefile`; when `timeout` is absent the recipe runs unwrapped, which is worse than a watchdog but better than an un-runnable tier. |

Everything not listed here is the Go standard library or hand-rolled — see the dependency budget below.

## Build

```sh
make build   # page assets, then the binary ./spindle (web/dist is embedded)
make web     # only the assets: app.css, ui.js, theme-boot.js, htmx, repos.js
```

`make build` depends on `web` and then runs
`go build -trimpath -buildvcs=auto -ldflags "-s -w -X main.buildSHA=<12-hex>" -o spindle ./cmd/spindle`
(a bare `go build ./cmd/spindle` works too, once the assets exist).
The tree is pure Go (no cgo), so the result is a static binary; `-buildvcs=auto` records the VCS
revision as the version fallback when the ldflag is absent.

`make web` runs `pnpm --dir web run build`, which is five steps:

- **`build:css`** — `vite build --config vite.app.config.mjs` → `web/dist/app.css`. Tailwind v4,
  CSS-first: `web/src/app.css` holds `@import "tailwindcss"` and two `@source` lines
  (`../../internal/webui/templates`, `./islands`). `@source` paths are relative to the CSS file and a
  missing one is **silent**, so the recipe greps the output for `max-w-6xl` and `.flex{` and fails when
  Tailwind scanned nothing.
- **`build:islands`** — esbuild `src/islands/index.js` → `dist/ui.js` (an ES module).
- **`build:boot`** — esbuild `src/theme-boot.js` → `dist/theme-boot.js` (IIFE; applies the saved theme
  before first paint).
- **`build:vendor`** — copies `web/vendor/htmx.min.js` (htmx 2.0.4) into `dist/` and deletes a stale
  `dist/htmx-ext-sse.js`; `make web` fails if that file reappears, because the htmx SSE extension is not
  our transport and must never ship.
- **`build:sdk`** — esbuild `sdk/src/index.js` → `dist/repos.js`, the dependency-free SDK.

**The embed rule.** `web/embed.go` is `//go:embed all:dist`, and the directive fails on a *missing*
directory, so `web/dist/.keep` is tracked and must never be gitignored. Build the web before the Go build
or any Go test run: `make vet` and `make test-go` depend on the `web` target for exactly that reason, and a
fresh clone that skips it does not compile. `make clean` removes the binary, `.cover/` and `web/dist/`,
then restores `.keep`.

**Local loop.** A zero-config `./spindle` boots *restricted* by design (only `/admin`, its POSTs and the
health endpoints answer), so a real sign-in needs an identity provider. `go run ./internal/devtools/oidpstub`
is a stdlib dev IdP that prints the `[server.auth]` block it accepts, and the compose profile wraps it:

```sh
docker compose -f compose.standalone.yml -f compose.dev-idp.yml --profile dev-idp up
```

The stub is dev tooling only: `cmd/spindle` never imports `internal/devtools`, so it is in neither the
binary nor the image. For the S3 lane, `make dev-store` starts rustfs on `127.0.0.1:9000` with the dev
keys the contract target expects, and `make dev-store-stop` tears the compose stack down.

## Repository map

The Go packages, and what each one owns:

| Package | Owns |
|---|---|
| `cmd/spindle` | The only binary: no subcommand serves; the rest are operator-local commands, the JSON-API client groups, and `runner serve` (the Threads runner process). |
| `internal/store` | `ObjectStore`, the protobuf + framing codecs, the filesystem/S3/memory backends, leases. |
| `internal/wal` | The WAL engine: sync levels, publish/CAS, checkpoints, replay, the remote reader, tasks. |
| `internal/git` | The git subprocess layer: ingest, refs, pkt-line, receive-/upload-pack, repack. |
| `internal/server` | HTTP on chi: middleware, routing, git/LFS/static endpoints, auth, the setup API shell, the page-provider mount (`bind_ui.go`), the `/v2` seam (`bind_top.go`). |
| `internal/staticpages` | The second HTTP listener: committed site bytes at `/{owner}/{repo}/…` on its own hostname, public repos only, off by default. |
| `internal/api` | The JSON API, the SSE envelope, and the render caches (`/{o}/{r}/api[-browser]`). |
| `internal/setup` | Bootstrap and first run: defaults, the config schema the UI renders, validated save. |
| `internal/bundle` | The bundle-uri scheduler: slots, chains, lists. |
| `internal/events` | The WAL → webhook bridge: cursor, delivery, wake-ups. |
| `internal/maintain` | The maintainer loop: checkpoints, bundles, compaction, fsck/repair, follow, sweepers. |
| `internal/sshd` | The SSH git transport (`x/crypto/ssh`): sessions, key auth, command parsing. |
| `internal/config` | `spindle.toml`, the `SPINDLE__SECTION__KEY` env overlay, per-repo settings, validation. |
| `internal/policy` | The push-policy rule language (`protect`/`history`/`size` effects). |
| `internal/archive` | Source archives (tar.gz/zip) produced by `git archive`. |
| `internal/branches` | Creating a branch from a ref and deleting one — the write half of the branches page. |
| `internal/httpsec` | The response-header policy and the redirect-target rule shared by the HTTP lanes and the pages. |
| `internal/cachepolicy` | The single source of HTTP `Cache-Control` classes. |
| `internal/egress` | Shared webhook/outbound egress hardening (refuse-all redirects, IP screen). |
| `internal/sizecatalog` | Repository catalog rows and `meta/stats.json` (sizes, activity, listing). |
| `internal/forksync` | The "Sync fork" driver. |
| `internal/pages` | The page-provider seam `internal/server` mounts; `internal/webui` implements it. |
| `internal/webui` | The server-rendered pages, plus the Go markdown, diff, commit-graph and highlight renderers. |
| `internal/packages` | The OCI registry: `/v2` protocol, Bearer token endpoint, storage, gc, page/API projections. |
| `internal/threads`, `internal/threads/runner`, `internal/threads/workflow` | The CI server half (runs, queue, logs, secrets, permissions, schedules, retention, approval); the runner engine (declare/fetch, docker plan/execute, log streaming); the workflow YAML parser — the only importer of `yaml.v3`. |
| `internal/identity`, `issues`, `pulls`, `review`, `checks`, `notify`, `releases`, `social`, `tags`, `mirror`, `repoimport`, `pushmirror`, `moderation`, `insights`, `webedit` | The collaboration layer: each package owns its store shapes, routes, tasks and SDK submodule. |
| `internal/doctor` | `spindle doctor`: read-only, bounded readiness checks over the bucket. |
| `internal/e2e`, `internal/sim`, `internal/devtools` | The harnesses: real-git end-to-end, the fault-injection sim, and `covergate`/bench/landing-GIF/dev-IdP tooling. |

The non-Go trees:

| Path | Contents |
|---|---|
| `web/` | `src/` (Tailwind entry, islands, theme boot), `sdk/src/` (SDK modules), `vendor/htmx.min.js`, `test/unit/`, `public/`, `dist/` (built, embedded). |
| `internal/webui/templates/` | `base.html`, `partials/`, and `pages/<group>/*.html` — one template set per page. |
| `Dockerfile` | Three stages: web build → static Go binary → alpine + git runtime, nonroot. |
| `compose.yaml`, `compose.standalone.yml`, `compose.dev-idp.yml` | The S3 rig (spindle + rustfs + bucket provisioning), the standalone filesystem stack, and the dev IdP profile. |
| `.forgejo/workflows/` | `ci.yml`, the merge gate and image publish described below. |
| `Makefile` | The only task runner; every tier and CI entry point is a target. |
| `scripts/` | `ci-s3-bucket.sh` (provisions the contract bucket; the store never creates buckets) and the publish tooling. |

## Rules that shape the code

Read [`../AGENTS.md`](../AGENTS.md) §1 before your first change; this is the summary, not the source.

- **Dependency budget (law 1).** Seven direct third-party modules, and nothing else: `go-chi/chi/v5` (the
  router — core only, all middleware hand-rolled), `BurntSushi/toml` (config), `golang.org/x/net` (h2c and
  the hand-rolled CDP client), `golang.org/x/crypto` (SSH server transport only — amended 2026-09-02,
  [`go/17_ssh.md`](go/17_ssh.md)), `dicebear-go/v10` + `dicebear/styles/v10` (deterministic
  avatars only), and `gopkg.in/yaml.v3` (Threads workflow files, imported by `internal/threads/workflow`
  and nothing else). Anything else needs a written amendment in the relevant doc's Decisions section
  *before* the code lands. Hand-roll instead: S3 SigV4, the protobuf wire codec, JWKS/JWT verification,
  SSE, Prometheus text exposition, LRU caches, singleflight, CLI dispatch, CORS and all other middleware.
- **git is a subprocess, always (law 2).** Every git operation shells out to the `git` binary with the
  exact argv pinned in [`go/04_git.md`](go/04_git.md). Never link a Go git library; never deviate from the
  argv without updating the doc in the same change.
- **The bucket is the repository (law 4).** No state outside the object store may survive a restart; disk
  and memory are caches, and wiping every instance must cost only warmth. The manifest CAS is the only
  commit point, and a push is never ACKed before the bucket AKs it.
- **Probe, don't list (law 4 and 6).** No `LIST` on a hot path; every read revalidates with a conditional
  GET. Round trips are the cost model: push ≤ 5 requests, warm refs 1, cold refs 2, checkpoint 4, asserted
  by the sim. The publish path's `meta/stats.json` sidecar adds one *parallel* op and zero sequential
  depth, which is why the asserted op bound is 6 cold and 5 once synced — the depth is what law 6 defends.
- **Lock order and channel ownership (law 3).** `syncMu → packMu → rw`; `rw` writes are `TryLock`-only;
  never hold a lock across a store or network call; the sender owns and closes a channel, receivers never
  close, and every goroutine exits via a context. [`go/13_concurrency.md`](go/13_concurrency.md) is the
  playbook, and every concurrent design carries a `### Concurrency` subsection naming hazard and avoidance.
- **No silent waiting (law 7).** Long work is a task: unique id, `(repo, kind)` single-flight, progress
  packets, an attachable SSE stream. A client never stares at a silent spinner.
- **Seams are contracts (law 8).** New capability attaches through the registries in
  [`go/14_extensibility.md`](go/14_extensibility.md) — route providers, auth providers, policy effects,
  event sinks, task kinds, CLI subcommands. `internal/store`, `internal/wal` and `internal/git` never
  import upward, and feature state never lands on local disk without a schema decision.
- **Fail closed (laws 9 and 10).** Authentication is OIDC and nothing else; admin is "signed in ∧ verified
  `groups_claim` ∩ `admin_groups` ≠ ∅". Zero config boots *restricted*, never open, and an invalid file
  lands in the same recoverable state — that is the recovery loop, not a bug.
- **Tests are the definition of done (law 11), and docs change with the code (law 12).** Near-100%
  coverage with a CI floor of ≥ 95% per `internal/...` package, table-driven `httptest` for every handler,
  `-race` mandatory, no skipped tests, and no budget assertion weakened to make a run pass. If code and doc
  disagree, one of them is fixed in the same change and the commit says which.

## Frontend stack

- **Pages are Go `html/template`**, rendered by `internal/webui` from `templates/base.html` +
  `templates/partials/` + `templates/pages/<group>/*.html`, parsed once per process. There is no client
  router, no data layer and no framework.
- **htmx 2.0.4 is vendored** (`web/vendor/htmx.min.js` → `dist/htmx.min.js`, a classic script). The htmx
  SSE extension is not shipped: `web/src/islands/sse.js` keeps a `fetch` stream (cookies, coalesced
  invalidation, backoff, `ui:auth-required`), and htmx only consumes the effects via
  `hx-trigger="ui:sse-… from:body"`.
- **Islands are vanilla ES modules** under `web/src/islands/` (theme, menus, copy, autogrow, selection,
  SSE, danger, submit keys, filters, task tray, thread log), bundled by esbuild to `dist/ui.js`.
- **The SDK** is authored as plain ESM submodules in `web/sdk/src/` and bundled, dependency-free, to
  `dist/repos.js`; it stays the programmatic surface alongside the JSON API.
- **Tailwind v4, CSS-first** (`web/src/app.css`: `@import "tailwindcss"`, no config file, no CDN, dark by
  default). Tailwind is the only styling language; a new rule earns its place only as a shared pattern in
  `app.css`, and component styles reference the semantic `--pd-*` tokens rather than hex literals.
- **No TypeScript, no runtime npm dependencies.** Build dev dependencies are exactly `vite`,
  `@tailwindcss/vite`, `tailwindcss` and `esbuild`; the JS tests use Node's built-in runner against the
  source modules, so testing needs no build.
- **Server rendering is the XSS boundary.** Markdown, unified diffs, commit graphs and syntax highlighting
  are rendered in Go (`internal/webui/{markdown,diff,graph,highlight}.go`, sanitized through
  `mdsanitize.go`), so user bytes never reach `template.HTML` and no client sanitizer exists to bypass.
- **The design language is Precision Dark**, specified normatively in
  [`style-guideline.md`](style-guideline.md); §4b covers the accessibility half. Compose the shared
  classes and page anatomy it names rather than re-deciding styling.

## Marketing site (`site/`)

`site/` is the static marketing page published at <https://crueber.github.io/spindle/>. It is hand-written
HTML, CSS and a small classic script — no build step, no framework, no external requests, no webfonts. The
palette is copied from `web/src/app.css` (Precision Dark) and every asset URL is relative, so the GitHub
project-site base path works unchanged. The screenshots are real captures of a running instance; the three
concept clips are the shipped `web/public/concepts/` GIFs, shown as their still frames until the reader
presses Play (and a switch to reduced motion puts the still back). Claims come from `README.md`,
`docs/FEATURES.md`, `docs/DEPLOY.md` and the numbers from `docs/EVIDENCE.md` — nothing on the page is
invented.

- `make site` stages `site/` plus `docs/demo.webm` into `dist/site`, exactly as GitHub Pages serves it;
  `make site-preview` stages and serves it at <http://localhost:8099>.
- `make site-check` is the gate (the Pages workflow runs it first): no private-estate hostname or old
  CI prose in the exported files, every referenced asset exists (including `srcset`), one `h1`,
  non-empty `alt` on every image, a 1200×630 PNG Open Graph card under 300 KiB, and no off-origin
  script or stylesheet.
- `.github/workflows/pages.yml` — installed from `scripts/publish/pages.yml` by the publish script — runs
  the check, uploads `dist/site` with `actions/upload-pages-artifact` and deploys it with
  `actions/deploy-pages` on the `main` pushes that touch the site or the demo, on `release: published`,
  and by manual dispatch. One-time setup: repository Settings → Pages → Source: **GitHub Actions**;
  otherwise `configure-pages` fails and nothing deploys.
- The canonical, Open Graph/Twitter and sitemap URLs are hardcoded to
  `https://crueber.github.io/spindle/`: this tree has one publish target. If that changes, extend the
  publish transform rather than editing each file by hand.
- GitHub Pages does not auto-discover `robots.txt` or `sitemap.xml` for a project site. After the first
  deploy, submit <https://crueber.github.io/spindle/sitemap.xml> to the search consoles manually.
- **Never reference the private estate here.** The publish gate rejects the estate's hostname
  anywhere in the exported tree, so every link points at `github.com/crueber/spindle`; `make
  site-check` performs the same scan over the site sources and the staging files.

## Verification ladder

Every entry point is a target in the `Makefile` (there is no other task runner). Run them in this order;
`make ci` is the merge gate.

| Target | Runs | Bar |
|---|---|---|
| `make fmt` | `gofmt -l . \| xargs -r gofmt -w` | Nothing left unformatted (`make vet` fails otherwise). |
| `make vet` | `make web`, then `gofmt -l` check, `go vet ./...`, `go build ./...` | Zero findings — the Go equivalent of `-D warnings`. |
| `make test` | `test-go` (`go test -short -count=1 ./...`, watchdog `timeout 300`) + `test-web` | Hermetic, under a minute. A Go-only green is **not** green. |
| `make test-web` | `node --test web/test/unit/*.test.js` — island/SDK logic modules imported from source, plus an opt-in fetch smoke when `SPINDLE_TEST_WEB_BASE_URL` names a server | Zero npm dependencies; part of `make test`. |
| `make race-pr` | `-race -short -count=1 -timeout 10m` over the packages that own shared state (`wal`, `store`, `notify`, `api`, `events`, `threads`, `packages`, `maintain`, `identity`, `server`, `git`, `sshd`, `bundle`, `sim`, `staticpages`) | What every pull request runs. |
| `make race` | The same `-race` flags over `./...` | Every push to `main` and on demand; the full set, `webui` included. |
| `make cover` | One `go test -coverprofile` per package discovered by `go list ./internal/...` (devtools and the e2e harness excluded), judged by `internal/devtools/covergate -min 95` | ≥ 95% statements, per package, never merged profiles. |
| `make contract` | `TestContract` against the memory and filesystem backends | Both backends on every run — the conditional-write machinery is contract-proven. |
| `make contract-fs` | The filesystem backend alone | Debugging aid for the above. |
| `make dev-store`, then `make contract-s3` | rustfs on `127.0.0.1:9000` (dev keys, bucket `spindle-test`), `TestContractS3` | S3 is first class: when the env is set the suite must pass. The store never creates buckets. |
| `make sim` | `./internal/sim/...` under `timeout 900` (`-timeout 15m`), seeded by `SPINDLE_SIM_SEED` / `SPINDLE_SIM_SEEDS` | The fault-injection consistency proof **and** the round-trip budgets; skips in `-short`. |
| `make e2e` | `./internal/e2e/...` with `-skip TestA11y` | Real git binary and real server subprocesses: smart HTTP, bundles, LFS, bootstrap, the collaboration chain. |
| `make a11y` | `-run TestA11y -v ./internal/e2e/` | The rendered floor over a fixed page set (landing, repo code, issue, PR, releases, `/admin`, `/admin/config`) at 390 px and desktop. Skips without a browser locally; `SPINDLE_A11Y=1` makes a missing browser a failure. |
| `make image` | `docker build -t spindle .` | Needs a Docker daemon. The CI image job builds the same Dockerfile with buildah, then runs the container contract. |
| `make backup-drill` | `TestE2E_BackupRestoreDrill` | Seed → snapshot → restore into an empty instance → compare every surface ([`BACKUP.md`](BACKUP.md)). |
| `make landing-gifs` | `internal/devtools/landinggif` → `web/public/concepts/` | Manual, and only when the landing scenes change (a golden test pins their freshness). |
| `make ci` | `vet test race cover contract sim e2e a11y` | The whole merge gate. |

`make help` lists these with their one-line descriptions.

**Never run unbounded.** The long-running recipes are wrapped in a watchdog (`T5` = 300 s, `T15` =
900 s, `T30` = 1800 s), coverage runs each package under an explicit 10-minute ceiling rather than a raised
one, and `go test ./...` is tiered — use the target for the tier you need, not the world.

**Browser-facing changes** add one step that no target can substitute for a contributor's judgement: run
`make web` **first** (a stale gitignored `web/dist` is embedded and makes a page lie), then drive the
changed page plus `/`, a repo page and `/admin` in a real browser at desktop **and ~390 px**. Check the
console, and assert `document.documentElement.scrollWidth === clientWidth` (no page-level horizontal pan;
tab strips, sidebars and table wrappers scroll internally by design). Then run the keyboard pass: skip
link first, exactly one `h1`, the `header/nav/main/footer` landmarks, every control named and reachable in
document order, the `:focus-visible` outline, `Esc` on menus — in both themes. Loopback caveat: the
canonical-host redirect 302s browser-looking `127.0.0.1:<port>` GETs to `walgit.localhost:<port>`, so a
`curl` 200 proves nothing about the browser path. `make a11y` is the machine floor over its fixed page
set; it does not replace looking at the page you changed.

## CI and publishing

### The CI toolchain is a prepped image — no apt in a job

The merge gate runs in `github.com/crueber/spindle-ci@sha256:…`, built from
[`docker/ci.Dockerfile`](../docker/ci.Dockerfile): Debian sid plus git (2.5x), node, npm, gcc,
`openssh-client`, chromium and the DejaVu fonts, installed once at image build. The gate does not
install packages. `apt-get` in a job step is a regression: it hits the Debian mirrors on every push,
pays the download on every run, and makes a green gate depend on a mirror being reachable.

When a pin moves, rebuild and re-pin the digest:

```sh
docker build -f docker/ci.Dockerfile -t github.com/crueber/spindle-ci:sid-$(date +%Y%m%d) .
docker push ghcr.io/crueber/spindle-ci:sid-$(date +%Y%m%d)
docker inspect --format='{{index .RepoDigests 0}}' github.com/crueber/spindle-ci:sid-$(date +%Y%m%d)
# copy the @sha256:… into .forgejo/workflows/ci.yml (container.image)
```


CI runs on Forgejo Actions (`.forgejo/workflows/ci.yml`) on every pull request, every push to `main` and
every `v*` tag. Each job builds the web itself — the runner rejects the artifact actions, and the Go build
embeds `web/dist` anyway.

| Job | Runs | Notes |
|---|---|---|
| `web` | Embed guard (`web/dist/.keep`), pnpm 11, `make web`, `node --test web/test/unit/*.test.js` | `node:22-trixie`, digest-pinned. |
| `gate` | `make vet`, `make race-pr` on a PR / `make race` on `main` and dispatch, `make cover`, `make e2e`, `make contract`, `make parity`, then `make a11y` with `SPINDLE_A11Y=1` | Digest-pinned `spindle-ci` (`docker/ci.Dockerfile`): git newer than trixie's, node, gcc and chromium are installed **at image build**, never by the job. Runs as an unprivileged user, because several tests assert that an unwritable path fails and root bypasses permission bits. `SPINDLE_CHROMIUM_NO_SANDBOX=1` is set here only, because a job container has no user namespaces. |
| `s3-contract` | rustfs service container, `scripts/ci-s3-bucket.sh`, `make contract-s3` | The service is a sibling on the job network, so the endpoint is `http://rustfs:9000`. |
| `image` | buildah (daemonless), the container contract, then push | Digest-pinned `spindle-ci-buildah` (`docker/ci-buildah.Dockerfile`): buildah **and git** are in the image, so the job does not run `apk`/`dnf` on every push. Push events only. Jobs never receive `/var/run/docker.sock`: the runner owns that socket and creates jobs as siblings, so a `docker` CLI inside a job would have no daemon — and mounting it would hand every workflow root. The contract asserts git ≥ 2.47, a working `docker` CLI (the runner needs it), that zero config is **refused** with the named OIDC key, and that a shape-valid OIDC config passes `--strict`. |

Every push to `main` publishes `ghcr.io/crueber/spindle:latest` and the commit SHA; a `v*` tag adds
`vX.Y.Z`. Pull requests run the gate and publish nothing. The package name is stable because it is the
pull path users have. The workflow file — not this table — is authoritative for the job list and the env
each job sets.


## Where the thinking lives

| Document | Holds |
|---|---|
| [`../AGENTS.md`](../AGENTS.md) | The laws, the working rules, the field lessons (each one shipped a real failure), and the project-tracking rules. |
| [`go/`](go/README.md) | The per-package implementation contracts. Start at [`go/01_overview.md`](go/01_overview.md), then your task's doc, then [`go/13_concurrency.md`](go/13_concurrency.md). |
| [`MASTER_RUST_SPEC.md`](MASTER_RUST_SPEC.md) | The Rust reference implementation's complete behaviour — normative for behavior, not for how Go implements it. |
| [`features/`](features/README.md) | The collaboration layer's contracts (identity, issues, PRs, review, checks, notifications, releases, import, mirrors, packages, Threads, moderation, file edit, insights, static sites). [`features/15_threads.md`](features/15_threads.md) is the CI design and as-built record. |
| [`threads.md`](threads.md) | The Threads reference: workflow keys, triggers, containers, secrets, runner flags, retention, troubleshooting. |
| [`PLAN-V1.md`](PLAN-V1.md), [`PLAN-V2.md`](PLAN-V2.md) | What v1 and v2 mean, work package by work package. |
| [`AUDIT-V1.md`](AUDIT-V1.md) | The independent audits, their verdicts and the rendered evidence behind them. |
| [`EVIDENCE.md`](EVIDENCE.md) | Measured performance claims — every entry has a committed harness, a named backend and both population sizes. A hot-path claim without an entry here is not evidence. |
| [`style-guideline.md`](style-guideline.md) | The normative design language (Precision Dark), page anatomy, and the accessibility rules. |
| [`BACKUP.md`](BACKUP.md) | Backup/restore procedure, mid-push caveat, RPO/RTO assumptions, post-restore checks. |
| [`go/15_testing.md`](go/15_testing.md) | The test tiers, the store contract, the FaultStore sim, the coverage gate and every Make target — in depth. |
| [`go/16_packaging.md`](go/16_packaging.md) | Build, container, nginx edge, the local rig, CI. |
| [`../DEVIATIONS.md`](../DEVIATIONS.md) | The decision ledger where Go-specific divergences are recorded. |

## Contributing

- Read [`../AGENTS.md`](../AGENTS.md) and [`../CODE_OF_CONDUCT.md`](../CODE_OF_CONDUCT.md) first. The laws
  are not advisory.
- **One logical change per commit.** The message names the doc section it implements and any decision it
  appends; formatting is `make fmt`, and `make vet` must be clean. A change that alters behaviour and not
  the doc (or the reverse) is rejected on that ground alone (law 12).
- **Tests land with the code.** New code arrives with table-driven tests, and a diff that drops a package
  below the 95% floor fails CI before review. Never weaken a budget assertion, never merge with a skipped
  test; name a flake and fix its cause.
- **Work on a branch or worktree**, and keep the project board honest instead of leaving work in a
  comment — AGENTS.md §2 "Project tracking" has the cycle (start, working, result, spill, never abandon).
- **Never commit scratch.** `.run/`, `shots/`, `live/`, `web/node_modules` and build output are agent and
  local workspace artifacts; stage explicitly and read `git status` before every commit.
- **Leave the machine as you found it.** Keep big artifacts and `TMPDIR` under `/var/tmp` (never the
  repo), remove a worktree once its branch is merged, kill the servers, watchers and browsers your pass
  started, and check `df -h /tmp /var/tmp` before a heavy wave. An unbounded test run is a bug in your
  invocation, not in the suite.
