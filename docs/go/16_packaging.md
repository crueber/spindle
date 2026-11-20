# 16 — Packaging, deployment, and the local rig

> Source: MASTER_RUST_SPEC.md §18 (packaging/deployment) · §15.1 (ops-referenced config keys) · §16 (CLI/exit codes) · the `deploy/nginx.conf.example` edge contract · Status: normative for the spindle Go implementation.

An operator MUST be able to build, containerize, deploy (one box or a fleet), and onboard a developer from this document alone. Behavioral truth comes from the Rust spec; everything Go-specific here is a marked decision (see the final section).

## 1. Build

### 1.1 The binary

- Module `github.com/crueber/spindle`; main package `cmd/spindle`. One binary, `spindle`, doing everything (`serve`, `bundle`, `repo`, `wal`, `import`, …; **no subcommand = `serve`**). There is no separate `spindle-server` binary.
- `CGO_ENABLED=0` always: pure Go, static, no libc coupling. This is what makes the runtime image choice (§2) and cross-compilation trivial:
  ```sh
  make build                       # native
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 make build   # cross, no toolchain changes
  ```
- Release build flags: `CGO_ENABLED=0 go build -trimpath -buildvcs=auto -ldflags "-s -w -X main.buildSHA=$(SHA)"`. Go has no LTO and does not need one; `-s -w` + `-trimpath` is the equivalent "thin" profile (smaller, reproducible).
- Exit codes stay 0/1/2/3 (§16): 0 ok, 1 command/config error, 2 bad argv (a missing config file no longer exits — D5 boots on built-in defaults), 3 `config check --strict` with ignored overrides.
- git is ALWAYS the subprocess `git` binary with exact argv (never go-git or any VCS library). On startup `serve` MUST run `git version`, parse it, and exit 1 with a clear message if it is < 2.47 (server-side needs: `pack.writeReverseIndex`, `--rev-index`, bundle-uri). The `git.binary` config key (unchanged name) selects the executable.

### 1.2 Embedded web assets (`go:embed`; the built page assets + the SDK bundle)

- File `web/embed.go` embeds the **built** tree only:
  ```go
  package web

  import "embed"

  //go:embed all:dist
  var Files embed.FS
  ```
- `internal/server` serves those bytes through `uiAsset`/`serveUIAssets` (`/_ui/*`, 06_server_http.md
  §3.5) and `sdkReposJS` (`/repos.js`). The page assets are the stylesheet `app.css`
  (Tailwind v4, CSS-first, built by `@tailwindcss/vite` through the one remaining vite build),
  the island bundle `ui.js` (esbuild), the vendored htmx files (`web/vendor/` → `dist/`, copied),
  `concepts/*.gif` (the checked-in landing GIFs, copied from `web/public/` by that same stylesheet
  build), and the immutable `assets/` lane. There is **no SPA bundle and no `index.html`** — pages are
  rendered by `internal/webui` from `html/template` at request time (D-WEB-14/D-WEB-15).
- The SDK is **authored as submodules** (`web/sdk/src/*.js`, 12_web_ui.md §1.0) and bundled by
  esbuild into `web/dist/repos.js`, served at its contract path (see 06_server_http.md / 07_api.md) —
  the `repos.mjs` twin is gone (D2); `make build` depends on `make web`.
- `pnpm run build` = `build:css` (vite + `@tailwindcss/vite` → `dist/app.css`, `emptyOutDir: false`)
  → `build:islands` (esbuild `src/islands/index.js` → `dist/ui.js`) → `build:vendor` (copy
  `vendor/htmx*.js`) → `build:sdk` (esbuild → `dist/repos.js`). Dev dependencies are exactly
  `vite`, `@tailwindcss/vite`, `tailwindcss`, `esbuild`; there are **no runtime npm dependencies**
  (D-WEB-15). `node --test web/test/unit/*.test.js` runs the headless JS tests (§7.2/§8) but never
  feeds the build. Sources (`internal/webui/templates/`, `web/src/`, `web/sdk/src/`, `web/test/`) and
  dotfiles stay out of the binary while remaining on disk for dev/tests.

### 1.3 Version identity

- Resolution order for the reported build sha:
  1. `-ldflags -X main.buildSHA=<12-hex>` (set by Makefile/CI/Dockerfile from `git rev-parse --short=12 HEAD`),
  2. `runtime/debug.ReadBuildInfo` `vcs.revision` (populated by `-buildvcs=auto`),
  3. `"dev"`.
- `spindle version` prints the sha plus the full `debug.BuildInfo` (module versions, Go version, settings). `serve` logs the same identity once at startup. The exact command table lives in 11_config_cli.md; the mechanism is specified here because it is a build artifact.

### 1.4 Runtime external dependencies

Exactly ONE external binary at runtime: `git` (≥ 2.47 server-side, ≥ 2.46 for clients using credential `authtype`). git-lfs is a **client** concern and MUST NOT be shipped in the server image. CA certificates are data, and are required (OIDC discovery, upstream follow, S3/GCS over TLS).

## 2. Container image

`Dockerfile` at the repo root (three stages): node runs the ONE web build (`pnpm run build` — stylesheet + islands + vendored htmx + SDK, D-WEB-14/D-WEB-15) → Go builds the static binary (`golang:1.27-alpine` per go.mod) → alpine runtime with git. (Rust used debian-slim + tini; see deviations.)

```dockerfile
# Dockerfile — one binary in front of a store. Zero config by default: filesystem store in the data dir.
#   docker build -t spindle .
#   docker run --rm -p 8080:8080 spindle     # → open http://host:8080/setup
#
# Compose examples: compose.standalone.yml (filesystem store) and
# compose.yaml (spindle + rustfs, the S3-backed stack).

# ---- 1. Web: the ONE build step — vite builds the stylesheet (dist/app.css),
#         esbuild bundles the islands (dist/ui.js) and web/sdk/src →
#         dist/repos.js; the vendored htmx files are copied in (D-WEB-14) ----
FROM docker.io/library/node:22-alpine AS web
RUN corepack enable && corepack prepare pnpm@11.25.0 --activate
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
# --ignore-scripts: esbuild's postinstall only validates its platform
# binary (shipped as an optional dependency); pnpm 11's build-script gate
# would otherwise fail the fresh install.
RUN pnpm install --frozen-lockfile --ignore-scripts
# Tailwind v4 only scans the paths named by `@source` in web/src/app.css. The
# Go templates live OUTSIDE web/, so they must exist at the path that directive
# resolves to from this stage — /src/web/src/../../internal/webui/templates.
# Without them Tailwind silently scans nothing and emits a stylesheet with no
# utility classes (.brand/.btn survive because they are @apply components), so
# every page renders unstyled. Copy the template tree only: this stage builds
# CSS/JS and gets nothing else from the repo.
COPY internal/webui/templates /src/internal/webui/templates
COPY web/ ./
# Guard: a stylesheet without the utilities the templates use means the scan
# found nothing (a broken @source path), which must fail the image build
# instead of shipping.
RUN pnpm run build \
    && test -f dist/app.css \
    && test -f dist/ui.js \
    && test -f dist/repos.js \
    && if ! grep -q 'max-w-6xl' dist/app.css || ! grep -q '\.flex{' dist/app.css; then \
         echo "ERROR: dist/app.css is missing the utility classes used by the Go templates" >&2; \
         echo "       Tailwind scanned no templates — check /src/internal/webui/templates and the @source paths in web/src/app.css." >&2; \
         exit 1; \
       fi

# ---- 2. Go build (static, no cgo; embeds the built dist/) ----
FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The web stage's dist/ (stylesheet, islands, SDK bundle) replaces whatever
# the context carried — the Go build embeds it (web/embed.go all:dist).
COPY --from=web /src/web/dist /src/web/dist
ARG SPINDLE_BUILD_SHA=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=auto \
      -ldflags "-s -w -X main.buildSHA=${SPINDLE_BUILD_SHA}" \
      -o /out/bin/spindle ./cmd/spindle

# ---- 3. runtime: git >= 2.47, CA certs, nonroot ----
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates \
    && git --version \
    && addgroup -g 1000 spindle && adduser -D -u 1000 -G spindle spindle \
    && mkdir -p /var/lib/spindle && chown -R spindle:spindle /var/lib/spindle
COPY --from=build /out/bin/spindle /usr/local/bin/spindle
ENV SPINDLE_DATA_DIR=/var/lib/spindle
USER spindle
WORKDIR /var/lib/spindle
EXPOSE 8080
VOLUME ["/var/lib/spindle"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s \
    CMD wget -qO- http://127.0.0.1:8080/readyz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/spindle"]
CMD ["serve"]
```

Rules:

- **Runtime base is alpine, not distroless/static**: the one hard runtime dependency is `git`, and distroless ships none. Alpine 3.22 ships git ≥ 2.47; a CI job (§8) fails the image if `git --version` inside it is < 2.47. tini is not needed (alpine `docker run --init`, k8s, and podman reap zombies; `git` children are waited on by `os/exec` anyway — see 04_git.md).
- `/readyz` reflects real readiness: store open + prewarm done (gated by `cache.prewarm_ready_timeout`) — NOT liveness. `/healthz` is the dumb liveness probe.
- **No config file is baked**: the image boots zero-config (D5) with `SPINDLE_DATA_DIR=/var/lib/spindle`; that data dir holds `store/`, `cache/`, and the `spindle.toml` that `/setup` writes. Operators who prefer a file mount one and point `SPINDLE_CONFIG` at it, or set `SPINDLE__*` env overrides — the table in §3.1 is unchanged.
- `SPINDLE_BUILD_SHA` build-arg comes from CI; a locally built image reports `dev`.
- **The web stage must receive the Tailwind scan inputs, not just `web/`.** `web/src/app.css` declares
  `@source "../../internal/webui/templates"`, so the image copies that directory to
  `/src/internal/webui/templates` (the path the directive resolves to from `/src/web`) before the
  build. A missing scan path is silent: Tailwind still writes a valid, smaller `app.css` (the `@apply`
  components survive, every utility used directly in templates disappears) and the image ships pages
  that render unstyled. The post-build `grep` guard (`max-w-6xl` and `.flex{`) fails the build in that
  state; `make web` asserts the same thing so a local/CI build cannot pass silently either. Any new
  `@source` outside `web/` needs its own `COPY` in this stage (and ideally its own guarded probe).

## 3. Configuration for operators

### 3.1 File discovery, env overrides, and compatibility

Behavior is §15 of the Rust spec, unchanged, plus these naming rules:

| Mechanism | Primary (spindle) | Legacy accepted (compat) |
|---|---|---|
| `--config PATH` flag | wins | — |
| config file env pointer | `SPINDLE_CONFIG` | `WALGIT_CONFIG` |
| default file name (CWD) | `spindle.toml` | `walgit.toml` |
| section/key env overrides | `SPINDLE__SECTION__KEY=value` | `WALGIT__SECTION__KEY=value` |
| log filter env | `SPINDLE_LOG` | `RUST_LOG` |

- TOML **key names** are unchanged from §15.1 — that is the compatibility surface. Both prefixes are stripped by the same loop (accept either; the longest matching prefix wins per key), so ops scripts and compose files written for the Rust deployment keep working verbatim.
- `PORT` env overrides the listen port and rewrites a loopback `public_url`'s port in lockstep (unchanged).
- `--config /dev/null` = explicit defaults + env only. **A missing config file is NOT fatal (D5):** boot proceeds on built-in defaults — `server.listen = "0.0.0.0:8080"`, `store.backend = "filesystem"` rooted at `<data-dir>/store`, `server.auth.mode = "none"` (loud WARNINGs in logs + a setup banner), `server.auto_create_on_push = true` — and the UI advertises `/setup` (D6).
- A config file that IS present but fails validation boots into **setup-only mode** (D6): only `/setup`, `/healthz`, `/readyz` answer; everything else is `503` with a pointer to the exact errors, which the setup UI also shows. Saving a fixed config then requires a restart.
- `spindle config check [--env-file …] [--strict]` stays fail-closed for the file it is handed (§15) and remains the supervisor pre-start gate (exit 1 invalid, exit 3 on ignored overrides under `--strict`):
  ```sh
  docker run --rm -v ./spindle.toml:/etc/spindle/spindle.toml:ro spindle config check --strict
  ```

### 3.2 `spindle.standalone.toml` — OPTIONAL one-box bucket shape

Copy-pasteable; every key is a §15.1 key. **This file is OPTIONAL** — the documented default path is NO config at all (D5/D6: `./spindle serve`, first-run `/setup`). Use this shape when you want one binary, one bucket, and one origin behind a TLS-terminating proxy (§3.4). The file the setup UI saves lands at `<data-dir>/spindle.toml` (atomic tmp+rename), not at this path. Memory-friendly: small cache budget, small remote-reader LRUs.

```toml
# spindle.standalone.toml — one binary, one bucket, one origin.
#   spindle serve --config spindle.standalone.toml
#   → http://spindle.localhost:8080/ behind the §3.4 proxy as https://spindle.localhost/

[server]
listen = "127.0.0.1:8080"                      # PORT env wins; also binds the ::1 twin
public_url = "https://spindle.localhost"        # the proxy's origin (it terminates TLS)
auto_create_on_push = true
roles = []                                     # empty = every role: serve, maintain, events

[server.auth]
mode = "none"                 # loopback play: everyone is `anon` with write
# Shared machine — static tokens:
# mode = "token"
# anonymous_read = false
# tokens = [
#   { principal = "alice", token_env = "SPINDLE_TOKEN_ALICE", write = true },
# ]

[store]
backend = "s3"
bucket = "walgit-test"                         # created by `make dev-store` (compose)
prefix = ""
[store.s3]
endpoint = "http://127.0.0.1:9000"
region = "us-east-1"
access_key_env = "AWS_ACCESS_KEY_ID"
secret_key_env = "AWS_SECRET_ACCESS_KEY"
force_path_style = true

# Chained bundles: weekly full, then each daily on the previous daily (first on the
# weekly), each hourly on the previous hourly (restarting at every new daily).
[[bundles.strategy]]
name = "weekly"
kind = "full"
schedule = "0 0 23 * * Sun"
keep = 1
backfill_max = 1
[[bundles.strategy]]
name = "daily"
kind = "incremental"
base = "weekly"
schedule = "0 0 23 * * *"
chain = true
[[bundles.strategy]]
name = "hourly"
kind = "incremental"
base = "daily"
schedule = "0 0 * * * *"
chain = true

[cache]
dir = "/tmp/spindle"                            # materialized repos; wiped = warmth lost
max_bytes = "8GiB"
remote_block_bytes = "512MiB"                  # memory-friendly LRU caps for a laptop/one-box
remote_object_bytes = "128MiB"

[telemetry]
log_format = "pretty"
```

### 3.3 Minimal S3 config (production) — OPTIONAL

OPTIONAL, like §3.2 — the zero-config filesystem default (D4/D5) needs none of this. Shown as the graduation path for when a bucket becomes the job.

```toml
[server]
listen = "0.0.0.0:8080"
public_url = "https://git.example.com"   # the §3.4 proxy's origin
[server.auth]
mode = "oidc"
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
oauth_client_id = "spindle"
# secrets via env: SPINDLE__SERVER__AUTH__OAUTH_CLIENT_SECRET / __SESSION_SECRET
[store]
backend = "s3"
bucket = "spindle-dev-bucket"                      # any S3: AWS, R2, MinIO, Ceph…
[store.s3]
region = "us-east-1"
# endpoint / force_path_style omitted = AWS defaults; set them for non-AWS S3.
```

GCS is the same shape with `backend = "gcs"` + `store.gcs.signing_service_account` for signed URLs (see 03_store_backends.md for credentials and the JSON-API transport).

### 3.4 Terminating TLS at the reverse proxy (Caddy / nginx minimal)

spindle serves plain HTTP on `server.listen` (h2c retained); TLS terminates at the proxy in front (Forgejo #165). The proxy forwards plain HTTP to spindle and MUST set `X-Forwarded-Proto: https` so the server advertises `https://` clone URLs (06_server_http.md §9.1); `server.public_url` pins the canonical external origin. Minimal shapes — adapt domains/ports, keep the header:

```caddy
# Caddy: automatic ACME, one line per site.
git.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

```nginx
# nginx: cert paths from certbot/ACME; the X-Forwarded-Proto line is load-bearing.
server {
    listen 443 ssl http2;
    server_name git.example.com;
    ssl_certificate /etc/letsencrypt/live/git.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/git.example.com/privkey.pem;
    client_max_body_size 0;          # pushes/LFS uploads stream, unbuffered
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

Caddy sets `X-Forwarded-Proto` itself; nginx needs the explicit line. Without it the server still works — it just advertises `http://` URLs in recipes (the 12_web_ui.md clone menu shows whatever the server advertises, verbatim).

## 4. The nginx edge contract

The Rust `deploy/nginx.conf.example` is the contract of record; spindle keeps it byte-for-byte on the wire. Header names stay `X-Walgit-*` (decision below) — an nginx config written for walgit MUST work in front of spindle unchanged.

### 4.1 The per-request contract (never assumed from config)

| Direction | Header | Meaning |
|---|---|---|
| nginx → spindle | `X-Walgit-Capabilities: accel-redirect, client-authorization` | what this edge takes over; spindle trusts it only because *this hop* set it (clients are stripped of it) |
| nginx → spindle | `X-Walgit-Authorization: <client's Authorization>` | the client's credential, preserved even if an intermediate rewrote `Authorization` |
| nginx → spindle | `X-Walgit-Principal: ""` | cleared; only spindle's own push-broker hop may set it (`server.auth.trusted_forwarders`) |
| spindle → nginx | `200` + `X-Accel-Redirect: /_store/` | the bytes are nginx's problem now (only when `server.accel_redirect = true`) |
| spindle → nginx | `X-Walgit-Store-Url` | bucket URL (S3: presigned; GCS: plain) |
| spindle → nginx | `X-Walgit-Store-Authorization` | `Bearer …` (GCS only; S3 URLs are self-credentialed) |
| spindle → nginx | `X-Walgit-Store-Key` | bucket key, percent-encoded — the cache key |
| spindle → nginx | `X-Walgit-Etag` | spindle's strong validator, re-emitted by nginx |

Auth: everything except open routes (`^/(healthz|readyz|repos\.js|repos\.mjs)$`, `/services/public/`, `/_auth/`) passes `auth_request /_auth/check`; spindle answers `204` (or 401/403/503) with no body. (`repos\.mjs` stays in the regex so edge configs written for walgit keep validating; spindle no longer ships that file — D2 — so the route simply never matches a real asset.) Verdicts are cached **5 min per credential** (`proxy_cache_key "$http_authorization|$cookie_walgit_session"`), 401/403 only **5 s**; `proxy_cache_lock on`. A denied browser navigation (Accept: text/html) is 302'd to `/_auth/login?next=…`; everything else gets a real 401 with `WWW-Authenticate: Bearer realm="walgit"` so git erases the dead token.

Byte offload at `/_store/` (internal only): `slice 64m` (a resumed 30 GB download hits cache; `Range` is not a signed header for S3), `proxy_cache_key "$store_key$slice_range"`, cached `30d`, `proxy_cache_lock` + `use_stale updating`, conditionals stripped (`If-Range`/`If-(None-)Match`/`If-Modified-Since` were already decided by spindle against ITS validator), bucket headers hidden, exactly one validator re-emitted: spindle's `X-Walgit-Etag`. Pushes/LFS uploads stream: `client_max_body_size 0` + `proxy_request_buffering off`.

Repo-prefix routing for a fleet: `location ~ ^/<owner>/<repo>([./?]|$)` blocks per upstream group — the repo prefix is the only routing key spindle needs. When the edge hands a repo to the wrong upstream, that instance answers read-only/refs-level with `503` + `Retry-After: 15` (placed-repo fallback; the object work is refused).

### 4.2 When NOT to use nginx

`spindle serve` listens plain HTTP (h2c), streams every byte, and authenticates every request — TLS termination belongs on the edge (§3.4). nginx buys: ACME/public certs + HTTP/2, the disk cache for bundle/LFS bytes (`X-Accel-Redirect` offload), one cached auth verdict per credential, and prefix routing across hosts.

## 5. Deployment shapes

| Shape | Config essence | Notes |
|---|---|---|
| **Personal / self-hosted (default)** | no config at all: `./spindle serve` — filesystem store under `<data-dir>/store`, cache under `<data-dir>/cache`, `auth.mode = "none"` (loud warnings + setup banner), all roles | one binary, no bucket, no edge; tune later via `/setup` → `<data-dir>/spindle.toml`; add the nginx edge (§4) when it leaves the LAN |
| **One box** | §3.2 standalone: all roles, one S3/GCS bucket, nothing in front except the §3.4 TLS proxy | `roles = []`; the bucket is the only durable state |
| **Fleet** | many `spindle serve` hosts behind the nginx edge; the monorepo's host pins `cache.mode = "disk"` + `maintenance.disk = "ssd"`, everyone else budget-mode (`cache.max_bytes`) | placement globs decide who does what (below); the edge does TLS/routing/offload; `server.auth.session_secret` MUST be identical on every host (rotation revokes all sessions+tokens) |
| **Serverless** | `server.roles = ["serve"]` only, config purely via env (`--config /dev/null` + `SPINDLE__*`), `cache.dir` on the ephemeral FS, bounded prefetch (`wal.prefetch_max_bytes`, set it low; `wal.prefetch_packs = false` under tight CPU) | CPU throttles between requests; a maintain fleet elsewhere holds the maintain role; keep-alives off, cold starts re-warm on demand |

One store per instance: `store.backend` is a single choice — the filesystem backend (D4) and the bucket backends cannot be mixed or tiered (03_store_backends.md). The personal shape uses filesystem; move to a bucket when durability/replication outgrows a disk, not alongside it.

Placement globs (`owner/name` | `owner/*` | `*`), on every host:

```toml
[placement]                       # SSD host (the monorepo's home)
serve = ["acme/monorepo", "team/*"]
maintain = ["acme/monorepo"]
maintenance.disk = "ssd"
cache.mode = "disk"
```

```toml
[placement]                       # budget hosts (everything else)
serve = ["*"]
serve_exclude = ["acme/monorepo"]
maintain_exclude = ["acme/monorepo"]
```

`[placement]` env overrides replace the whole section (all-or-nothing) — fleets inject placement per instance via env. Nix-flake packaging is a Rust-side nicety and is NOT ported (deviation below).

## 6. Process lifecycle, readiness, drain

`spindle serve` starts: store → prewarm (if configured) → maintainer + follow loops (per roles) → HTTP server. `SIGTERM`/`SIGINT` → graceful drain: new work refused, in-flight requests finish within `server.drain_timeout` (default `20s`), then exit 0.

### Concurrency

Hazards at the packaging boundary (playbook: 13_concurrency.md):

- **Prewarm vs readiness**: prewarm fetches MUST run on at most `cache.prewarm_parallelism` goroutines sharing one `context.Context` owned by `cmd/spindle`; `/readyz` reports a `sync/atomic` readiness flag, never polls locks — a healthcheck that blocks on a mutex makes the orchestrator's probe the deadlock detector of last resort. `cache.prewarm_ready_timeout` bounds how long readiness may stay false; after it, serve anyway and log a WARNING.
- **Drain**: the root context is canceled on the second signal (hard stop); the first signal stops listeners via `http.Server.Shutdown` and lets bulk worker pools finish their current unit within `drain_timeout`. Every goroutine started by `serve` (maintainer, events, bundle builders) takes that context; `cmd/spindle` owns the context and waits for the worker pools' `WaitGroup` before exiting — nobody else closes the context.
- **Panics**: net/http recovers handler panics per request (log + 500 + request id). Every background loop (maintainer pass, events sweep, bundle build) wraps each pass in its own `recover` so one bad repo degrades to a WARN line, not a dead instance — the Go equivalent of the Rust "unwind panics, one request's panic must not kill the instance" release profile.

## 7. Local rig

### 7.1 Compose examples (repo root) — two stacks, both verified end to end

Two example compose files ship at the repo root:

- **`compose.standalone.yml`** — spindle ALONE: filesystem store on a named
  volume, port 8080, healthcheck. First boot is zero-config (auth `none` +
  loud warning); `/setup` writes `spindle.toml` into the volume for later boots.
- **`compose.yaml`** — the **bucket-backed stack**: rustfs (S3-compatible, the
  same fixture `make dev-store` and CI's s3-contract use) + a one-shot
  `create-bucket` job + spindle configured for the S3 store entirely through
  the `SPINDLE__STORE__*` env overlay (no config file). `make dev-store`
  starts only the rustfs service from this file.

```yaml
# compose.yaml (abridged — the shipped file is the source of truth)
name: spindle-rustfs
services:
  rustfs:
    image: rustfs/rustfs:latest
    environment:
      RUSTFS_ACCESS_KEY: walgit-dev        # DEV KEYS — never production
      RUSTFS_SECRET_KEY: walgit-dev-secret
      RUSTFS_ADDRESS: "0.0.0.0:9000"
      RUSTFS_CONSOLE_ADDRESS: "0.0.0.0:9001"
      RUSTFS_VOLUMES: /data
    healthcheck: { test: ["CMD", "wget", "-qO-", "http://127.0.0.1:9000/minio/health/live"], interval: 5s, retries: 10 }
  create-bucket:
    image: amazon/aws-cli:latest           # idempotent s3 mb s3://spindle-test
    depends_on: { rustfs: { condition: service_healthy } }
  spindle:
    build: .
    depends_on:
      rustfs: { condition: service_healthy }
      create-bucket: { condition: service_completed_successfully }
    environment:
      SPINDLE__STORE__BACKEND: s3
      SPINDLE__STORE__S3__ENDPOINT: http://rustfs:9000   # NESTED keys: store.s3.endpoint
      SPINDLE__STORE__S3__REGION: us-east-1
      SPINDLE__STORE__S3__FORCE_PATH_STYLE: "true"        # required for rustfs/MinIO
      SPINDLE__STORE__BUCKET: spindle-test
      AWS_ACCESS_KEY_ID: walgit-dev
      AWS_SECRET_ACCESS_KEY: walgit-dev-secret
    ports: ["8080:8080"]
```

**Env-overlay gotchas encoded here** (each one broke a real boot):
- Nested config (`store.s3.endpoint`) needs EVERY segment in the env name —
  `SPINDLE__STORE__ENDPOINT` is silently recorded as an unknown key and ignored.
- rustfs/MinIO require `force_path_style: true`; bucket-virtual-host addressing
  403s (`InvalidAccessKeyId`-class errors against the wrong host).
- Healthchecks pin `127.0.0.1` — `localhost` resolves to `::1` in busybox and
  rustfs binds IPv4 only.
- pnpm 11 refuses dependency build scripts unless `pnpm-workspace.yaml`
  declares `allowBuilds: { esbuild: true }` (the web stage's `pnpm install`
  fails otherwise).

## 7.2 `Makefile` (repo root) — the ONLY task runner (D3)

Every dev/CI entry point is a target; `make help` is the authoritative list (the file owns the
commands, this table owns the intent). The watchdog (`T5`/`T15`) wraps the long tiers in GNU
`timeout` and degrades to unwrapped with a warning when it is missing.

| target | what it runs |
|---|---|
| `web` | `pnpm --dir web run build` — vite/Tailwind (`app.css`) + esbuild (islands → `ui.js`, SDK → `repos.js`) + vendored htmx copy |
| `build` | `web`, then the static binary (`-trimpath -buildvcs=auto`, the SHA ldflag) |
| `fmt` / `vet` | `gofmt -w` / the gofmt-clean + `go vet` + `go build` gate |
| `test` | `test-go` (`-short`, watchdog) + `test-web` (`node --test web/test/unit/*.test.js`) |
| `race` | `go test -race -short -count=1 ./...` |
| `cover` | the ≥ 95 % per-`internal/...`-package gate (D7) via `internal/devtools/covergate` |
| `contract` | store contract: memory + filesystem, always |
| `contract-fs` / `contract-s3` / `contract-gcs` | one backend; S3/GCS env-gated (`SPINDLE_TEST_S3_*`, `SPINDLE_TEST_GCS_BUCKET`) |
| `sim` | `internal/sim` fault-injection consistency proof + round-trip budgets |
| `e2e` | smart-HTTP end-to-end against the real git binary |
| `image` | `docker build -t spindle .` |
| `dev-store` / `dev-store-stop` | the rustfs rig that gives the S3 contract a bucket |
| `landing-gifs` | regenerate the landing-page concept GIFs (manual; the golden test pins freshness) |
| `clean` | remove the binary, `.cover/`, and `web/dist` (recreates the tracked `.keep`) |
| `ci` | `vet test race cover contract sim e2e` — what CI runs; the image build happens there |

CI calls these same targets, so a target that is green locally is the target the pipeline runs (the
containers add the toolchain, not the commands). `dev-store` exists only so the S3 contract has a
bucket; the zero-config dev loop never touches it.

(Make is canonical and exclusive — there is no justfile. `dev-store` exists only so `contract-s3` and CI's `s3-contract` job have a bucket; the zero-config dev loop never touches it.)

## 8. CI pipeline

Development happens on Forgejo; GitHub is a distribution channel. The pipeline is expressed as
GitHub Actions (`.github/workflows/ci.yml`) and is the merge gate: web (stylesheet + islands + SDK) →
`go vet` → `-race` → the headless JS tests (`node --test`) → cover gate (D7) → e2e (real git ≥ 2.47) → store
contract (filesystem + memory ALWAYS per D4; S3 against a rustfs service) → image build + container
contract. `make web` builds both web artifacts; JS tests import source and need no build.


The S3 contract job needs a bucket and the store never creates buckets: `scripts/ci-s3-bucket.sh`
provisions it with curl's `--aws-sigv4` (no aws-cli in the job), then `make contract-s3` runs with
`SPINDLE_TEST_S3_ENDPOINT` pointing at the service container (the Makefile's `?=` defaults keep
`http://127.0.0.1:9000` for `make dev-store` locally).


Container images keep one package name — `ghcr.io/crueber/spindle` is the pull path users have — with
tags `latest` + branch from `main`, semver from `v*` tags, and `sha-<sha>` per commit (linux/amd64;
layer cache).


## 9. Onboarding a developer (30-second quickstart)

```sh
# Prerequisites: docker (or Go 1.25 to build the binary); git >= 2.47 wherever the
# server runs, >= 2.46 on client machines.
git clone https://github.com/crueber/spindle && cd spindle

# Zero-config: no TOML, no bucket, no TLS. Filesystem store inside the data dir.
docker run --rm -p 8080:8080 spindle       # or: make build && ./spindle serve
# The boot log warns that auth.mode = "none" and prints the setup banner.

# Open http://host:8080/setup — every config section with its current effective
# value. Save writes <data-dir>/spindle.toml atomically and lists the keys that
# need a restart. Or change nothing and keep the defaults: also a valid choice.

# First push creates the repo (auto_create_on_push defaults to true):
mkdir demo && cd demo && git init -b main && echo hello > README.md
git add . && git commit -m init
git remote add origin http://host:8080/$USER/demo.git
git push -u origin main

# Clone from another terminal/dir — a fresh clone rides bundle-uri:
git clone http://host:8080/$USER/demo.git
```

TLS, OIDC, and bucket-backed shapes are deliberate upgrades, not defaults — §3.2/§3.3 (OPTIONAL config files) and §4 (nginx edge).

Token minting (team/production, `auth.mode = "oidc"`):

1. Open `https://git.example.com` → sign in through the issuer (browser flow, `/_auth/callback`).
2. Visit `/_auth/tokens` → mint a token: `wgt_…`, valid `server.auth.access_token_ttl` (default `90d`).
3. Feed it to git once per machine:
   ```sh
   export SPINDLE_TOKEN=wgt_…   # from the tokens page
   git config --global credential.https://git.example.com.helper \
     '!f() { printf "username=x\npassword=%s\n" "$SPINDLE_TOKEN"; }; f'
   ```
   Static-token deployments (`auth.mode = "token"`) skip the browser: the principal's `token_env` value IS the password (HTTP Basic or bearer).

Install one-liner (from any spindle host):

```sh
curl -fsSL https://github.com/crueber/spindle/raw/main/scripts/install.sh | sh
# installs ./spindle into ~/.local/bin (release tarball by arch, or `go install` fallback).
```

## Decisions & deviations from the Rust design

- **One binary (`spindle`), no `spindle-server` twin** — Rust shipped two; Go compiles one `main` with the same "no subcommand = serve" semantics, so the twin adds nothing.
- **`spindle.toml` is the default config name; `walgit.toml` still accepted, `SPINDLE__` env prefix primary with `WALGIT__` also stripped** — TOML key names stay identical (the compat surface), while process/env names follow the new binary; honoring legacy prefixes keeps Rust-era ops scripts and compose files working verbatim.
- **Runtime image is alpine (git ≥ 2.47), not debian-slim + tini** — distroless/static lacks `git`, the one hard runtime dependency; alpine also ships busybox `wget` so HEALTHCHECK needs no curl; zombie reaping is handled by `os/exec` wait + orchestrator init.
- **git-lfs omitted from the image** — the Rust spec itself notes it is a client-side tool; the server never invokes it.
- **~~Node 20 (not 24) + pnpm@10 for the web stage~~ — SUPERSEDED three times: divergence D2 (2026-08-31) removed the build, D-WEB-6 (2026-09-02) restored a node stage for the SolidJS SPA, and D-WEB-14/D-WEB-15 (2026-11-19/20) deleted the SPA. The node stage now exists ONLY to build the page assets (`app.css`, `ui.js`, vendored htmx) and the SDK bundle (`repos.js`). Historical:** the frontend was zero-build vanilla ES modules embedded raw from `web/`. **Second historical shape:** the SolidJS + Tailwind SPA built by vite plus the esbuild SDK bundle (`pnpm run build`), per D-WEB-6.
- **Go release flags `-trimpath -buildvcs=auto -ldflags "-s -w"` replace thin LTO + unwind-panic profile** — Go has no LTO; per-request/per-pass `recover` reproduces the "one panic must not kill the instance" guarantee (§6).
- **Startup git version check is fatal (exit 1) below 2.47** — the Rust spec declares ≥ 2.47 required for server features; failing fast with a message beats mysterious wire breakage.
- **Header names stay `X-Walgit-*` (including the `walgit_session` cookie in the auth cache key) and the token prefix stays `wgt_`** — the nginx contract and users' stored credentials are the edge-compat surface; renaming would silently invalidate deployed edge configs and credential helpers.
- **Dev-rig rustfs keys/bucket keep the Rust values (`walgit-dev` / `walgit-dev-secret`, bucket `walgit-test`)** — muscle-memory continuity for contributors coming from the Rust rig; local-only credentials.
- **Nix flake packaging not ported** — the flake was a Rust toolchain convenience; Go's static cross-build plus the Dockerfile covers the same ground without a second packaging system (revisit only on demand).
- **CI is a Makefile + GitHub Actions pipeline, not GitHub Actions** — the repo lives on Forgejo; the Actions service block covers the rustfs contract job, and GitHub only receives the published squash commit (where its own Actions run the same gate and build the image).
- **NEW (2026-09-05) — server-side TLS removed; the proxy terminates it** (Forgejo #165, §3.4): spindle listens plain HTTP (h2c) and never loads a cert — the `server.tls.*` keys, the §3.2/§3.3 `[server.tls]` examples, and the install.sh CA-download step are all gone. The one-box and production shapes put Caddy/nginx in front (minimal snippets in §3.4) with `X-Forwarded-Proto: https` so recipes advertise `https://`; `server.public_url` pins the external origin. Rationale: every inbound flow works behind a terminating proxy and outbound HTTPS (OIDC, webhooks, S3/GCS) validates against the system roots — owning an in-process TLS stack bought nothing.
- **`install.sh` is served from the Forgejo raw URL** — the Rust edge example leaves the installer out of the open-route set; keeping it repo-hosted avoids inventing a new public server route in the Go rewrite.
- **Divergence (2026-08-31), applied in this revision:**
- **D2 — SUPERSEDED 2026-09-02 by explicit user request (DEVIATIONS.md D-WEB-6), then again 2026-11-19/20 (D-WEB-14/D-WEB-15).** **Second historical shape:** the SolidJS + Tailwind v4 SPA built by vite into `web/dist/` (plain JSX, no TypeScript; runtime deps exactly solid-js + @solidjs/router; dark by default; no CDN), with the node stage running `pnpm run build` (vite SPA + esbuild SDK bundle). **Third historical shape:** the frontend was vanilla ESM (hand-written ES modules with native import map, `<template>`, ~40 lines of reactive helpers, embedded raw — no TypeScript, no framework; SolidJS/vite gone), superseding the "Node 20 + pnpm@10" decision above; the node stage was esbuild-only. **Shipped:** server-rendered pages (`internal/webui` `html/template` + vendored htmx + vanilla-ES islands) with `@tailwindcss/vite` building only `app.css`; the node stage builds stylesheet + islands + vendored copy + the SDK bundle. The SDK is **authored as submodules** (`web/sdk/src/*.js`) and esbuild-bundled to `web/dist/repos.js` (the `repos.mjs` twin is deleted; `dist/` is gitignored build output); JS tests run on Node's built-in `node --test` and import source, never the build.
- **D3 — Make is the only task runner.** All dev/CI entry points are Make targets (`build vet fmt test race test-web e2e sim contract contract-s3 cover image dev-store clean ci`; `make help` is authoritative); the "`just` is fine too" note is retracted — there is no justfile.
- **D4 — filesystem store backend is first-class.** `store.backend = "filesystem"` roots keys under `store.root` with sidecar-`flock` conditional writes, `renameat2(RENAME_NOREPLACE)` create-if-absent (portable fallback), `"<size>:<mtime_ns>"` version tokens/ETags, and no accel/signed URLs. It joins the contract suite as an ALWAYS-run backend beside memory (Make `contract`); the container's default path uses it; rustfs remains only as the bucket-contract fixture.
- **D5 — zero-config first run (user friendliness is a first-class law).** A missing config boots on built-in defaults — `listen 0.0.0.0:8080`, filesystem store rooted at `<data-dir>/store`, `auth.mode = "none"` on ANY bind with loud warnings (a deliberate reversal of the Rust fail-closed loopback rule), `auto_create_on_push = true`; exit code 2 is now bad-argv only. Data dir = `--data-dir` / `SPINDLE_DATA_DIR` (default `~/.local/share/spindle`, containers `/var/lib/spindle`), holding `store/`, `cache/`, and the saved `spindle.toml`.
- **D6 — setup UI + API are first-class.** `/setup` groups every config key by section with effective values; `GET /api/v1/setup`, `POST /api/v1/setup/test` (validate only), `PUT /api/v1/setup` (validate + atomic write of `<data-dir>/spindle.toml`, reporting restart-needed keys). Invalid config → setup-only mode: only `/setup`, `/healthz`, `/readyz` answer, everything else 503. Access: open while (no config file OR config invalid OR auth.mode = none), else admin principal; optional `SPINDLE_SETUP_TOKEN` gate for exposed hosts.
- **D7 — coverage gate.** CI fails below 95% statement coverage per `internal/...` package (Make `cover`; `cmd/` main glue excluded); the pipeline gains the `cover` and web-test steps; table-driven httptest for every handler.
