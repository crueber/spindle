# AGENTS.md — operating manual for spindle (humans and agents)

**What this repo is:** the implementation workspace for **spindle** — a Go rewrite of the walgit git host. `docs/MASTER_RUST_SPEC.md` specifies the system's behavior completely (it describes the Rust reference implementation and is normative for behavior); `docs/go/*.md` specify how to build it in Go. When you are asked to implement, review, or extend spindle, this file tells you how to work — and the same rules bind human contributors; humans additionally start at [`README.md`](README.md) for setup and day-to-day workflow. Reading order: `docs/go/README.md` → `docs/go/01_overview.md` → your task's doc → `docs/go/13_concurrency.md`. Work on the collaboration layer (issues, PRs, review, checks, notifications, orgs) additionally starts at `docs/features/README.md` — its primitives are frozen contracts, exactly like `14_extensibility.md`.

> **Pre-1.0, no backwards compatibility within spindle.** Change the shape and delete the old shape in the
> same change — no aliases, shims, deprecated flags, or "still accepted for" branches. The ONE exception is
> bucket compatibility with the Rust implementation (below): formats on the bucket are append-only and
> shared.

## 1. The laws (violating any of these is a rejected change)

1. **Dependency budget is law.** Backend third-party modules: `github.com/go-chi/chi/v5` (the router), `github.com/BurntSushi/toml` (config), `golang.org/x/net` (h2c), `golang.org/x/crypto` (SSH server transport only — amended 2026-09-02 by explicit user request for SSH git transport; see `docs/go/17_ssh.md` decision 17.1), `github.com/dicebear/dicebear-go/v10` + `github.com/dicebear/styles/v10` (deterministic user-avatar generation only — amended 2026-09-12 by explicit user request for auto-generated avatars, Forgejo #376; see `docs/features/01_identity_permissions.md` Decisions; build-required transitives are `github.com/dicebear/schema` + `github.com/santhosh-tekuri/jsonschema/v6`, verified with `go mod graph` — the issue's "zero sub-dependencies" assumption was wrong, the option-validation path needs both; `golang.org/x/text` was already an indirect dependency). Frontend (amended 2026-11-19 by explicit user request — the server-rendered HTMX migration, see `DEVIATIONS.md` D-WEB-14, which supersedes D-WEB-6/D-WEB-7): pages are **Go `html/template`** rendered by `internal/webui`; interactivity is **htmx 2.0.4, vendored** (`web/vendor/`, MIT) plus small vanilla **ES modules** under `web/src/islands/` (no framework, no client data layer, no client-side router). User-authored markdown, unified diffs, commit graphs and syntax highlighting are rendered **server-side in Go** (`internal/webui/{markdown,diff,graph,highlight}.go`: escape everything, allow only http/https/mailto/relative links) — so `marked` and `DOMPurify` are gone and the XSS boundary is the server renderer. The `repos.js` SDK stays dependency-free. Dev-time tooling: `tailwindcss` v4 (CSS-first; the no-config rule and dark-by-default stay) builds `web/dist/app.css` (through `@tailwindcss/vite`, so `vite` stays as the stylesheet build host — amended 2026-11-20, D-WEB-15); `esbuild` bundles the SDK and the island entry. Still NO TypeScript (plain JSX-free JS). Anything else needs a written amendment in the relevant doc's "Decisions & deviations" section BEFORE the code lands. Hand-roll instead: S3 SigV4, protobuf wire codec, JWKS/JWT verification, SSE, Prometheus text exposition, LRU caches, singleflight, CLI dispatch, CORS and all other middleware (chi core only — no chi/cors, no chi/middleware packages).
2. **git is a subprocess, always.** Every git operation shells out to the `git` binary with the exact argv specified in `docs/go/04_git.md`. Never link a Go git library. Never deviate from the specified argv without updating the doc in the same change.
3. **Concurrency: goroutines yes, deadlocks never.** Every concurrent design carries a `### Concurrency` subsection (hazard + avoidance). The lock rules in `docs/go/13_concurrency.md` are binding:
   - Lock order `syncMu → packMu → rw`; never acquire out of order.
   - `rw` writes are **TryLock-only** (pack removal); a blocked writer starves readers, and a clone can
     hold a read guard for an hour. If TryLock fails, defer the removal to the next pass.
   - Never hold a lock across a store/network call unless the doc explicitly says so (it almost never does).
   - Channel rule: the sender owns and closes the channel; receivers never close; every goroutine exits via
     context.
   - Bulk bytes never share an http.Client or a semaphore with control-plane traffic (the 2026-08 incidents
     in `13_concurrency.md §7` are the proof).
4. **The bucket is the repository.** No state outside the object store may survive a restart (disk and memory are caches; "if every instance is wiped, what is lost?" must answer "warmth"). The manifest CAS is the only commit point. Never ACK a push before the bucket ACKs. No LIST on a hot path — probe, don't list. Every read revalidates (conditional GET) — there is no "eventually".
5. **Bucket compatibility with the Rust implementation.** Key layout, protobuf field numbers/wire encoding, JSON file shapes, and git wire behavior are byte-compatible (see `docs/go/02_storage_protobuf.md`). Golden fixtures in `testdata/` pin this; a change that breaks round-tripping against the fixtures is a bug even if both sides are ours. Proto is append-only: never reuse/renumber a field.
6. **Round trips are the cost model.** Happy-path budgets (push ≤ 5 requests, warm refs 1, cold refs 2, checkpoint 4) are asserted in the sim (`docs/go/15_testing.md`). A correct change that adds a sequential store round trip to a hot path is a regression. Parallelize independent PUTs; let conditional writes be the read; verification goes on the failure path.
7. **No silent waiting.** Long work is a task: unique id, `(repo, kind)` single-flight, progress packets, attachable SSE stream. A client must never stare at a silent spinner.
8. **Modularity seams are contracts.** New capabilities (issues, PRs, review, multi-user) attach ONLY through the registries in `docs/go/14_extensibility.md` (route providers, auth providers, policy effects, event sinks, task kinds, CLI subcommands). Core packages (`internal/store`, `internal/wal`, `internal/git`) must not import upward (`internal/server`, `internal/api`, feature packages), and feature state must never live on local disk or in the WAL without a schema decision.
9. **Fail closed — with one deliberate exception.** Config validation still refuses oidc without an allowlist and rejects unparseable policies; an invalid credential gets a real 401 (that is what makes git erase it), never a 200 with in-band error — except the exact four-condition case in `docs/go/06_server_http.md §8.4`. **Superseded by divergence:** auth `none` no longer requires a loopback bind — it is allowed anywhere (zero-config friendliness) with loud warnings in logs and the setup UI; the setup UI is the one-click path to real auth.
10. **Zero-config first run.** `spindle` with no config boots on sane defaults (`0.0.0.0:8080`, filesystem
    store under the data dir, auth `none`, `auto_create_on_push`) and the `/setup` UI configures
    everything. A present config file is validated before boot; an INVALID file puts the server in
    setup-only mode (everything but setup/health answers 503) until the user saves a fixed one. Setup
    save is validated, written atomically to `<data-dir>/spindle.toml`, and lists restart-required keys.
11. **Tests are the definition of done.** Backend: near-100% coverage, enforced — every `internal/...`
    package holds ≥ 95% statement coverage in CI (`make cover`), table-driven httptest for every handler,
    `-race` mandatory. Frontend: JS tested with Node's built-in `node --test` (logic modules
    headless-testable, DOM kept thin). Never merge with skipped tests; never weaken a budget assertion to
    make it pass.
12. **Documents change with the code.** Every doc has a `Decisions & deviations from the Rust design`
    section; decisions are appended there with a one-line rationale, never silently overridden. If code and
    doc disagree, fix one of them in the same change — say which and why in the commit.

## 2. Working rules (agents and humans — the same bar)

- **Read your doc first.** Each `docs/go/NN_*.md` is self-sufficient for its package: interfaces, argv, wire shapes, concurrency rules, tests. Cross-references are by file name. Do not guess behavior that the Rust spec pins down — open `docs/MASTER_RUST_SPEC.md` and search it.
- **Implement in waves** (see `docs/go/README.md`): storage → git/WAL → HTTP/API → subsystems. Independent docs can be implemented in parallel by separate agents; the seams between them are the interfaces named in the docs (store.ObjectStore, wal.RepoHandle, api route providers). When two agents meet at a seam, the interface in the doc wins; propose amendments, don't freelance.
- **Tests are part of the definition of done.** Each package's doc names its tests (contract cases, budget assertions, e2e flows). `-race` is mandatory in CI; concurrency-heavy packages get stress tests (`-count=100`) and the deadlock canary; `make cover` enforces the ≥ 95% per-package gate. Never merge with skipped tests; never weaken a budget assertion to make it pass.
- **Never run unbounded commands.** Wrap test/build invocations in a timeout. `go test ./...` in this repo is tiered (see `docs/go/15_testing.md`) — run `make test` / `make race` / `make sim` for the tier you need, not the world.
- **Commit discipline.** One logical change per commit; the commit message states the doc section it implements and any decision it appends. Format with gofmt (`make fmt`); vet clean (`make vet`); the budgeted modules (law 1) are the only non-stdlib imports a linter should ever flag.
- **Naming.** Binary `spindle`, module `github.com/crueber/spindle`, packages exactly as in `docs/go/01_overview.md` (cmd/spindle; internal/{store,wal,git,bundle,policy,server,api,setup,events,maintain,config}; the collaboration layer in its own packages; web/). Wire/bucket identifiers (header names `X-Walgit-*`, config key names, bucket key paths, protobuf package `walgit.v1`) keep their Rust-era names — they are wire contracts, not branding. Root markdown files are all-uppercase (`AGENTS.md`, `DESIGN.md`, `DEVIATIONS.md`) — except `README.md`, which keeps the universal lowercase spelling (#212).
- **Mobile viewport is always checked.** Every browser-facing change is verified at a narrow/mobile viewport (~390px width) as well as desktop — layout, overflow, and tap targets. UI work without a mobile-viewport check is not done.
- **Tailwind is the only styling language — compose it correctly (Forgejo #533).** No text-field utilities on non-text controls (the `.input` class on a checkbox stretched it full-width and scattered the Settings Features rows); no one-off CSS without shared-pattern justification (a new rule earns its place only as a shared pattern in `web/src/app.css`, per the #405 opaque-popover precedent); controls align in both themes at desktop and ~390px. **Layout changes are verified rendered before merge** — a screenshot or DOM measurement (row-rect alignment, control positions), never code reading alone; a layout ticket closed without rendered proof is not done.
- **The style guideline is binding for all UI work (Forgejo #533).** `docs/style-guideline.md` codifies the app's design language with canonical references; it is normative, not advisory. Every change to `web/` composes the shared classes, components, and page-anatomy patterns it names — a new UI element reuses the canonical idiom (naming its reference) instead of re-deciding styling. Deviating from the guideline, or introducing a new pattern where a canonical one exists, requires either (a) extending `docs/style-guideline.md` in the same change with the new canonical reference and rationale, or (b) an amendment in the relevant doc's "Decisions & deviations" section. Reviewers reject UI changes that re-decide what the guideline already settles. The guideline never overrides AGENTS.md law 1 (dependency budget) or the Tailwind-only rule; where they conflict, the law wins and the guideline is corrected.

Performance claims (scale, latency shape) are **evidence-backed**: measured entries with reproduction harnesses live in [`docs/EVIDENCE.md`](docs/EVIDENCE.md). A transport/storage feature that makes a hot-path claim gets an entry there — measured on the real code path, named backend, both population sizes, harness committed to `internal/devtools/`.

### Field lessons (each one shipped a real failure — do not relearn them)

- **Env overlay keys are exact paths.** `SPINDLE__STORE__S3__ENDPOINT`, never `SPINDLE__STORE__ENDPOINT` — a missing segment is recorded as an unknown key and silently ignored; `spindle config dump` (or `check`) is how you catch it. rustfs/MinIO need `store.s3.force_path_style = true` (virtual-host addressing 403s); healthchecks pin `127.0.0.1` (busybox `localhost` = `::1`, rustfs binds IPv4); pnpm 11 needs `allowBuilds: { esbuild: true }` in `web/pnpm-workspace.yaml` (the package.json `pnpm` field is dead). The compose examples encode all of this.
- **`--data-dir` flag syncing must preserve the env overlay.** It re-points only the flag-derived paths (`DataDir`, `Store.Root`, `Cache.Dir`); re-deriving `FirstRunDefaults` there silently discarded every `SPINDLE__*` override on zero-config boots (shipped and reverted once).
- **Setup values round-trip through the overrides channel.** The setup API accepts exactly the spellings the TOML file accepts: sizes (`64MiB`), `d`/`w` durations, floats, unsigned counters, and `[[name]]` TOML fragments for struct slices (`bundles.strategy`, `server.auth.tokens`) decoded by the field's toml name. `setup.Coerce` must use the shared `config` parsers — a page save failing "not an int"/"unsupported type" means the coerce path regressed.
- **Schema lookups are by key name, never by section group.** Group layout changes (auth was split out of `server`); `setup.EffectiveValue`/`setup.RestartKeys` and any new schema consumer match keys directly, or `requires_restart` and diffs silently pollute with keys that did not change.
- **Server values render in the spec spelling** (`64GiB`, `1h` — `fmtSpecSize`/`fmtSpecDuration`); the setup page's editable values and its per-field metadata (`webui.settingsFieldMeta`, the successor of the old FIELDS table) share one spelling, and every example is tested to validate in the page and accepted by `/api/v1/setup/test` (server).
- **`node --test`: pass files or globs, never a directory.** Node 22 executes a directory positional as a module (`Cannot find module …/web/test`); Node 24 accepts both. All entry points (`make test-web`, the CI workflow, the published GitHub workflow) use `web/test/unit/*.test.js`.
- **Tests that spawn git pin their identity.** Annotated tags and `commit-tree` need an author/committer; runners and containers have no global git config. `TestMain` in `internal/git` and `internal/maintain` sets `GIT_AUTHOR_*`/`GIT_COMMITTER_*` for spawned subprocesses only. Reproduce environment gaps with `env -i PATH=… HOME=/tmp/nohome go test …`.
- **Fresh clones compile.** `web/dist/.keep` is tracked because `go:embed all:dist` fails on a missing directory — never gitignore it away, and build the web BEFORE `go test` in CI (the embed shell is what `/setup` 200s on).
- **Tailwind `@source` paths are relative to the CSS file, and a missing one is silent.** `web/src/app.css` scans `../../internal/webui/templates`; the image's web stage copies only `web/`, so the relative path pointed at nothing, Tailwind emitted a smaller `app.css` containing the `@apply` components (`.brand`, `.btn`) and none of the utilities the templates use, and the deployed site rendered unstyled while every dev/CI build (whole tree present) passed. The web stage now copies the template tree to the resolved path and the build (`Dockerfile` + `make web`) greps for `max-w-6xl`/`.flex{`. Any new `@source` outside `web/` needs the same treatment; file-exists checks do not catch an empty scan.
- **Development builds and publishing are separate steps.** Every pull request and push to `main` runs `.github/workflows/ci.yml` (the full gate) and publishes the image `ghcr.io/crueber/spindle:latest` (plus the commit SHA, and the version on `v*` tags). Keep the package name stable: it is the pull path users have. README "Run with docker compose" documents pulling.
  Publishing is the button, not a mirror: `bump` (patch/minor/major) or `bump: none` + an exact `tag`
  (Weave's process); a publish whose exported tree is unchanged produces a tag only, never an empty
  commit.
- **The publish tree is the tracked set — `.gitignore` must not subtract from it.** The published
  copy is `git archive HEAD` re-committed under a fresh `git init`, so an ignore rule that matches a
  tracked path silently drops it (the stale `covergate/` rule removed a whole package from a
  publish, and GitHub CI died at `make cover`). The script stages with `git add -A -f` and refuses
  when the index is smaller than the export.
- **S3 ETags are content hashes, not counters.** Writing identical bytes keeps the same version, so
  CAS and lease tests must change the body to observe a stale token. The contract suite encodes
  that, and pins the §2.3 quirk (an unparseable Update token skips the precondition).
- **`buildah run` execs its argv directly.** The image `ENTRYPOINT` is not applied, so the container
  contract wraps its checks in `/bin/sh -c '…'` (a bare `git --version` string was exec'd as a
  program name and failed the job).
- **The gate needs a git newer than any released Debian, and must run unprivileged.** The pull
  replay path uses `git replay --ref-action` (newer than trixie's 2.47), and several tests assert
  that an unwritable path fails — which root bypasses. The CI gate is a digest-pinned `debian:sid`
  running the suite as a normal user; `node:22-trixie` covers the web and S3 jobs.
- **The UI language is Precision Dark; use the semantic tokens.** `web/src/app.css`
  retunes the raw zinc/emerald/amber ramps so the utilities already in templates adopt the
  palette, but new component styles reference `--pd-*`. Amber means *action* (one
  `.btn.primary` per page); emerald means *state*; links are ink with an underline, never the
  accent. A hex literal or an emerald text-link in a template is a regression —
  `docs/style-guideline.md` §1 is normative.
- **Never assert on `runtime.NumGoroutine()`.** It is process-global: an
  unrelated goroutine exiting between the baseline and the check shrinks the
  delta, and a healthy SSE keepalive set reads as a leak (`expected >= 8
  goroutines after attach, got 7` — hit in both `internal/notify` and
  `internal/api`). Count the thing you own: each writer bumps an
  `activeKeepalives` atomic on start and decrements on exit; the test polls
  that counter.
- **`html/template` builtins are process-wide — never shadow them.** A FuncMap entry named `eq`
  replaces the builtin for every template; a string-only `eq` broke every error page
  (`eq .Data.Status 401`). Do not define `eq/ne/lt/le/gt/ge/len/index/printf/slice`.
- **Pages vs partials is the template glob.** `templates/pages/<group>/*.html` is parsed as a PAGE
  (its own `content`); shared blocks belong in `templates/partials/` (parsed into every page set).
  A `_partial` under `pages/` is a page, and its defines are invisible to its siblings.
- **`ServeMux` wildcards cannot overlap.** Two patterns of the same shape (`/{a}/{b}/{c}` and
  `/{owner}/{name}/{tab}`) panic at registration. Literal segments win, so keep literal routes
  literal and route shape-generic families (e.g. `/{owner}/teams/{slug}`) through ONE dispatcher
  that writes nothing for non-matches — the page seam then falls through to the next handler.
- **The htmx SSE extension is not our transport — and is not shipped.** It cannot POST, set
  headers, read JSON payloads, or distinguish a 401 (it retries forever), and the htmx-1 build
  warned on every page against htmx 2. `web/src/islands/sse.js` keeps a fetch stream (cookies,
  coalesced invalidation, backoff, `ui:auth-required`); htmx only consumes the effects
  (`hx-trigger="ui:sse-… from:body"`). No template uses `hx-ext`/`sse-connect`/`sse-swap` — if one
  ever needs the extension, vendor the htmx 2 build, not the 1.x one.
- **Server rendering IS the XSS boundary.** `internal/webui/markdown.go` escapes all text and
  allows only http/https/mailto/relative links; `diff.go`/`graph.go`/`highlight.go` escape too.
  Never pass user-derived bytes through `template.HTML`, and never reintroduce a client sanitizer
  (marked/DOMPurify are gone by decision, D-WEB-14).
- **A page gate must mirror the API's gate exactly.** `s.gated` (anonymous_read), `s.repoVisible`
  (identity require_read, Forgejo #345), per-action role floors, setup-only mode. A page that skips
  one leaks what the API protects; the URL has no privilege of its own.
- **Canonical host redirect bites headless UI drives.** `canonicalBrowserHost` (`docs/go/06_server_http.md §2.2 #2`) 302s browser-looking loopback GETs from `127.0.0.1:<port>` to `walgit.localhost:<port>` — curl fails the browser test and never redirects, so "curl says 200" proves nothing about a browser path. Drive the UI against the canonical host (or expect the hop).

## 3. Quick orientation (what runs where)

```
cmd/spindle          one binary; no subcommand = serve; roles by config (serve/maintain/events)
internal/store      ObjectStore interface, protobuf+framing codecs, filesystem/S3/memory backends, leases
internal/wal        the WAL engine: sync levels, publish/CAS, checkpoints, replay, remote reader, tasks
internal/git        the git subprocess layer (ingest, refs, pkt-line, receive/upload-pack, repack)
internal/server     HTTP (chi): middleware, routing, git/LFS/static endpoints, auth, the setup API shell,
                    and the page-provider mount (bind_ui.go)
internal/api        JSON API + SSE envelope + render caches (the /{o}/{r}/api[-browser] surface)
internal/setup      bootstrap + setup: first-run defaults, config schema for the UI, validated save
internal/bundle     bundle-uri scheduler: slots, chains, lists, D17
internal/events     WAL → webhook bridge (cursor, delivery, wake-ups)
internal/maintain   maintainer loop: checkpoints, bundles, compaction, fsck/repair, follow
internal/sshd       SSH git transport (x/crypto/ssh): sessions, key auth, command parsing
internal/config     spindle.toml (optional) + SPINDLE__ env overrides, per-repo settings, validation
internal/policy     push policy rule language (protect/history/size effects)
internal/identity,issues,pulls,review,checks,notify,releases,social,tags,mirror,repoimport,pushmirror
                    the collaboration layer: each package owns its store shapes, routes, tasks and
                    SDK submodule (docs/features/README.md; seams/registries in 14_extensibility.md)
internal/e2e,sim,devtools
                    the harnesses: real-git end-to-end, fault-injection sim, covergate and friends
internal/pages      the page-provider seam: the server mounts it (internal/server/bind_ui.go) and
                    internal/webui implements it; a path it does not own is the plain 404
internal/webui      server-rendered pages: one html/template set per page (templates/base.html +
                    partials/ + pages/<group>/), render/gating/chrome helpers, and the Go
                    markdown/diff/commit-graph/highlight renderers
web/                templates live in internal/webui; web/src carries islands/ (vanilla ES: theme,
                    menus, copy, autogrow, selection, SSE, danger, submit keys), vendor/ (htmx),
                    app.css (Tailwind v4, the design system) and the dependency-free SDK
                    esbuild-bundled to web/dist/repos.js. `make web` builds css + islands + vendor +
                    SDK. The binary embeds dist/: /_ui/app.css, /_ui/ui.js, /_ui/htmx*.js,
                    /_ui/assets/* (immutable; reserved, nothing hashed ships today), /_ui/concepts/*.gif,
                    /repos.js (module MIME is load-bearing)
Dockerfile          three-stage image (web build → static Go binary → alpine+git runtime, nonroot)
compose*.y(ml)      compose.standalone.yml = spindle alone (filesystem store); compose.yaml = spindle +
                    rustfs + create-bucket (the S3 rig, also `make dev-store`); both build from source
.github/workflows  CI: ci.yml = the merge gate + image to ghcr.io/crueber/spindle:latest
scripts/ci-s3-bucket.sh  makes the S3 contract bucket exist (curl SigV4, no aws-cli)
```

## 4. Verification ladder (what to run before you say "done")

1. `make fmt && make vet` — clean.
2. Package tests for everything you touched: `make test` (or `go test ./internal/<pkg>/... -race`).
3. `make cover` — the ≥ 95% per-package gate holds with your change included.
4. Store contract suite: `make contract` (memory + filesystem always; S3 via the rustfs rig when you touched a backend).
5. `make sim` when you touched `internal/wal` or any publish/sync path (budget assertions included).
6. `make e2e` with real git when you touched `internal/git` or `internal/server` git routes.
7. `make test-web` (node --test) when you touched `web/`.
8. **Real browser when you touched anything browser-facing** (`internal/webui/templates`, `web/src/islands`, `web/src/app.css`, static/UI serving, auth flows, setup): load `/`, a repo page, the changed page and `/setup` in actual Chromium at desktop **and ~390px**, check the console, and assert `document.documentElement.scrollWidth === clientWidth` (no page-level horizontal pan — the tab strip/sidebar/table wrappers scroll internally by design). The island bundle is a module script (MIME load-bearing) and htmx is a classic script; "curl says 200" and DOM-fetch smokes have both shipped a broken page (the uiAsset stub incident, 2026-09-01), so a rendered check is required. Drive Chromium over CDP (`Emulation.setDeviceMetricsOverride` for 390px; navigate/evaluate/screenshot) or the hub-managed `chrome-cdp` on :9222; remember the canonical host redirect (§2 field lessons) when pointing it at a loopback port.
9. If you appended a decision: the doc's "Decisions & deviations" section updated in the same commit.
