# DEPLOY — install, configure, run spindle

> Status: operator procedure (2026-11-24). This file is install, ports, proxy,
> configuration and troubleshooting. Identity, the Administration area and
> credentials are [`OPERATIONS.md`](OPERATIONS.md); backup is
> [`BACKUP.md`](BACKUP.md); moving a running instance to a new build is
> [`UPGRADE.md`](UPGRADE.md).

A spindle instance is one binary in front of one object store. There is no
database to install: the bucket is the repository, and instances are disposable.

## 1. Build from source

```sh
make build          # stylesheet + islands + SDK, then the binary — web/dist is embedded
./spindle           # first run: RESTRICTED — only the Administration area answers until OIDC is configured
```

- **Go 1.27** (`go.mod`), plus **pnpm 11** for the browser assets. The image
  builds them with `node:22-alpine` + `pnpm@11.25.0`; `make web` builds only the
  UI assets (`app.css`, `ui.js`, the vendored htmx and the `repos.js` SDK) and
  `make build` embeds `web/dist` into the binary (`go:embed all:dist`), so no
  checkout is needed at run time.
- The binary is `spindle` (module `github.com/crueber/spindle`). **No
  subcommand means `serve`**; `server.roles` selects `serve`, `maintain` and
  `events` (empty = all). The HTTP listener runs in every role because it serves
  `/readyz` for the supervisor.
- The first boot has no config, so it is **restricted** — see
  [`OPERATIONS.md` §1](OPERATIONS.md#1-the-restricted-first-boot).
- Prerequisites in detail, the package map and the test tiers:
  [`DEVELOPMENT.md`](DEVELOPMENT.md).

## 2. The published image

GitHub Actions publishes the image to
[`ghcr.io/crueber/spindle`](https://github.com/crueber/spindle/pkgs/container/spindle)
on every push to `main` and on `v*` tags — pull it instead of building.

```sh
docker pull ghcr.io/crueber/spindle:latest
```

| Tag | What it tracks |
|---|---|
| `ghcr.io/crueber/spindle:latest` | the newest push to `main` |
| `ghcr.io/crueber/spindle:main` | the `main` branch |
| `ghcr.io/crueber/spindle:sha-<sha>` | one commit |
| `ghcr.io/crueber/spindle:vX.Y.Z` | a release tag |


The image is `spindle serve` by default (`ENTRYPOINT ["/usr/local/bin/spindle"]`,
`CMD ["serve"]`), runs as the non-root user `spindle` (uid 1000), sets
`SPINDLE_DATA_DIR=/var/lib/spindle`, declares that path as a `VOLUME`, exposes
8080, ships `git`, `ca-certificates` and the `docker` CLI, and healthchecks
`/readyz`. See [`../Dockerfile`](../Dockerfile).

`make image` builds the same image locally: three stages (web assets → static Go
binary → Alpine + git runtime), web assets embedded, non-root. Both compose
examples are working starting points — pin a tag (`sha-<sha>` or `vX.Y.Z`) and set
`server.public_url` before exposing one.

## 3. Run it with docker compose

### 3.1 Standalone: filesystem store on a named volume

```yaml
# docker-compose.yml — spindle alone: filesystem store on a named volume
services:
  spindle:
    image: ghcr.io/crueber/spindle:latest   # :main tracks main; vX.Y.Z releases; sha-<sha> per commit
    ports:
      - "8080:8080"
      - "2222:2222"                        # git over SSH (17_ssh.md)
    environment:
      SPINDLE__SERVER__SSH__LISTEN: 0.0.0.0:2222
    volumes:
      - spindle-data:/var/lib/spindle
    restart: unless-stopped

volumes:
  spindle-data:
```

```sh
docker compose up -d        # update later with: docker compose pull && docker compose up -d
```

Then open **http://localhost:8080/admin** — the same restricted first boot as the
binary: save a config from the Configuration page, then restart (Maintenance →
Restart, or `docker compose restart`). Repositories auto-create on push under
`http://localhost:8080/<owner>/<repo>.git`.

`restart: unless-stopped` is what makes **Maintenance → Restart** useful: the
control exits the process and the supervisor starts it again. With `restart: no`
the instance stays down until you start it.

### 3.2 Git over SSH

Git also works over **SSH**: the stack publishes port 2222 and enables the SSH
transport (`SPINDLE__SERVER__SSH__LISTEN`). Add your public key on the **/keys**
page, then:

```sh
git clone ssh://git@localhost:2222/<owner>/<repo>.git
```

The host key auto-generates into the data volume, so it is stable across
container restarts.

### 3.3 S3-backed: spindle + rustfs

[`../compose.yaml`](../compose.yaml) is the bucket-backed stack (project
`spindle-rustfs`): spindle + rustfs (an S3-compatible object store) + a one-shot,
idempotent `create-bucket` service that makes `spindle-test`. The fixed keys
`walgit-dev` / `walgit-dev-secret` are **dev keys — never point this stack at
anything you care about**. rustfs publishes 9000 (S3 API) and 9001 (web console).

```sh
docker compose up -d --build     # the whole S3 rig
make dev-store                   # just the rustfs fixture (`docker compose up -d rustfs`)
make contract-s3                 # store contract suite against it (after make dev-store)
```

The spindle service selects the bucket through the env overlay
([`go/11_config_cli.md`](go/11_config_cli.md) §3.2):

```yaml
SPINDLE__STORE__BACKEND: s3
SPINDLE__STORE__S3__ENDPOINT: http://rustfs:9000
SPINDLE__STORE__S3__REGION: us-east-1
SPINDLE__STORE__BUCKET: spindle-test
SPINDLE__STORE__S3__FORCE_PATH_STYLE: "true"
AWS_ACCESS_KEY_ID: walgit-dev              # named by store.s3.access_key_env (the default)
AWS_SECRET_ACCESS_KEY: walgit-dev-secret   # named by store.s3.secret_key_env
```

`force_path_style` is required: rustfs and MinIO need path-style addressing, and
virtual-host addressing answers 403.

**The shipped stacks build from source.** Both [`../compose.yaml`](../compose.yaml)
and [`../compose.standalone.yml`](../compose.standalone.yml) use `build: .`; to
run the published image instead, replace the `spindle` service's `build: .` with
`image: ghcr.io/crueber/spindle:latest` (the rustfs service and the
`SPINDLE__STORE__*` env stay as they are). `compose.standalone.yml` is the same
standalone shape as §3.1, built from source instead of pulled.

## 4. Ports, URLs and the data directory

### 4.1 Listeners

| Key | Default | What it is |
|---|---|---|
| `server.listen` | `127.0.0.1:8080` (zero-config first run: `0.0.0.0:8080`) | the plain-HTTP listener (h2c + HTTP/1.1) |
| `server.public_url` | empty (derived from the request) | the canonical external origin: clone URLs, the OIDC redirect URI, links |
| `server.ssh.listen` | empty = **disabled** | the SSH git listener, e.g. `0.0.0.0:2222` |
| `server.ssh.external_port` | `0` (= the listen port) | the port advertised in `ssh://` clone URLs (issue #215: listen 2222 internally, advertise 12222) |
| `server.auto_create_on_push` | `true` in the zero-config first-run shape | a push creates the repository |

- The HTTP surface is 8080 by convention (the image exposes it, both compose
  files publish it); git over HTTP is `<public-url>/<owner>/<repo>.git`.
- The SSH listener **does not start in the restricted boot** (the same
  fail-closed rule as every other unconfigured surface) and is disabled unless
  `server.ssh.listen` is set.
- `server.public_url` matters behind a proxy and for CI: job containers must be
  able to resolve the name clone URLs use ([`threads.md`](threads.md) §4.4).

**Optional second listener: static sites.** `[static_sites]` starts its own
listener, off by default:

| Key | Default | What it is |
|---|---|---|
| `static_sites.enabled` | `false` | master switch; no socket is bound while false |
| `static_sites.address` | `127.0.0.1:8081` | the listener's bind address (must differ from `server.listen`) |
| `static_sites.host` | `static.localhost` | the **only** `Host` it answers for (case- and port-insensitive) |
| `static_sites.max_file_bytes` | `8MiB` | per-file cap; larger files 404 in v1 |
| `static_sites.index_files` | `["index.html","index.htm"]` | directory index probe order |

It answers exactly one hostname, so point the proxy's static vhost at that
address and keep it off the app's hostname — the isolation is the host split,
not the port (cookies are host-scoped; a second port on the app host shares the
cookie jar). The listener is cookie-less, so it serves **public repositories
only**: publishing is the per-repo `[static_site] enabled = true` section, and a
private or unpublished repo is a plain 404. URL shape, resolution rules and the
security rationale are [`features/19_static_sites.md`](features/19_static_sites.md)
and [`FEATURES.md`](FEATURES.md) §5.

### 4.2 The data directory

The data dir is `SPINDLE_DATA_DIR` if set, else `/var/lib/spindle` when running
in a container (`/.dockerenv` present or `KUBERNETES_SERVICE_HOST` set), else
`~/.local/share/spindle`. `--data-dir` is authoritative over the env/default and
re-points only the flag-derived paths (`store.root`, `cache.dir`); values from a
file or the `SPINDLE__*` overlay are preserved.

| Path | What it holds | Back it up? |
|---|---|---|
| `<data-dir>/store` | the filesystem object store (`store.root`) — every repository, issue, package, run | **yes** |
| `<data-dir>/spindle.toml` | the config: issuer, client secret, session secret, store coordinates, admin group | **yes** |
| `<data-dir>/spindle.toml.bak-<utc>` | the save-time undo copy of the previous file | no (same disk, not a backup) |
| `<data-dir>/ssh/ed25519_host_key` | the auto-generated SSH host key (0600, dir 0700) | yes (losing it only forces clients to re-pin, TOFU) |
| `<data-dir>/cache` | serving copies, materialized packs, render caches (`cache.dir`) | **no — expendable**, never restore it |

## 5. Reverse proxy and TLS

TLS terminates at the reverse proxy (issue #165). spindle listens **plain HTTP
with h2c** (HTTP/2 prior knowledge) and HTTP/1.1 on `server.listen` and never
wraps the listener in TLS: there is no `server.tls.*` config surface. A file that
still sets one is rejected at load, and a `SPINDLE__SERVER__TLS__*` override is
fatal, both pointing at the proxy — a silently ignored TLS setting is worse than
a refused boot.

- Forward plain HTTP to `server.listen` and set `X-Forwarded-Proto: https`, so
  absolute URLs (clone URLs, the redirect URI) are built with `https`; set
  `server.public_url` to the canonical external origin so displayed URLs never
  depend on a client's `Host`.
- The optional **static-site** listener is a second vhost, not a second path:
  proxy `https://<static_sites.host>/` to `http://<static_sites.address>/` and
  never expose it under the app's hostname. It needs no forwarded headers, sets
  no cookies, and the app's canonical-host redirect does not apply to it.
- X-Forwarded-For is ignored unless `rate_limit.trust_proxy` is true, and then
  only the first hop is used — the header is client-controlled without a trusted
  proxy.
- Minimal Caddy/nginx snippets: [`go/16_packaging.md`](go/16_packaging.md) §3.4;
  the nginx edge contract: [`go/16_packaging.md`](go/16_packaging.md) §4; TLS
  section: [`go/06_server_http.md`](go/06_server_http.md) §11.

**Canonical host.** A browser-looking `GET`/`HEAD` (Accept contains
`text/html`, or `Sec-Fetch-Dest: document`, or a `Mozilla` user agent) whose host
is loopback (`127.0.0.0/8`, `::1`, `localhost`) is answered with a 302 to
`walgit.localhost[:port]`, same path and query, scheme per `X-Forwarded-Proto`.
It is skipped for `/_auth/*`, `/healthz`, `/readyz` and `/services/public*`, and
git/curl clients fail the browser test so they are never redirected. The rule is
[`go/06_server_http.md`](go/06_server_http.md) §2.2 #2 — drive the UI against the
canonical host (or expect the hop) when verifying a page.

## 6. Configuration

### 6.1 The file is optional

- The default path is **`<data-dir>/spindle.toml`** (`<data-dir>/walgit.toml` is
  accepted as an alias, checked second). `--config PATH` (or `SPINDLE_CONFIG`,
  legacy `WALGIT_CONFIG`) overrides it; `--config /dev/null` means "defaults +
  environment, no file".
- A **missing file at the default path is the zero-config first run**, not an
  error. A missing **explicitly named** file is fatal: `config file not found:
  <path>`, exit 2.
- Unknown keys are rejected at load (fail closed), which lands the instance in
  the restricted boot with the findings on `/admin/config`.
- The Administration → Configuration page is the supported editor: it writes the
  whole canonical document atomically (temp file, fsync, rename, mode 0600) and
  keeps `spindle.toml.bak-<utc>` before the first rewrite of an existing file.

### 6.2 The environment overlay

`SPINDLE__SECTION__KEY` with **one segment per TOML path**. The segment is
literal: a missing one does not error, it simply never applies, and the key is
recorded as an *ignored override*.

| Right | Wrong (silently never applied) |
|---|---|
| `SPINDLE__STORE__S3__ENDPOINT` | `SPINDLE__STORE__ENDPOINT` |
| `SPINDLE__SERVER__AUTH__ADMIN_GROUPS` | `SPINDLE__SERVER__ADMIN_GROUPS` |
| `SPINDLE__SERVER__AUTH__SESSION_SECRET` | `SPINDLE__SERVER__SESSION_SECRET` |

- Values are TOML values: `SPINDLE__SERVER__AUTH__ALLOWED_DOMAINS=["example.com"]`,
  `...__FORCE_PATH_STYLE=true`, sizes as `64MiB`, durations as `1h`/`30d`.
  Anything that is not a TOML value falls back to a raw string.
- An unknown key is **reported, never fatal** (a stale
  `SPINDLE__STORE__BKUET` typo must not take a host down) — which is exactly why
  `spindle config check` exists (§6.3).
- `WALGIT__*` is the legacy alias: when both spellings are set, `SPINDLE__*`
  wins and the other is reported as an ignored override.
- `PORT` rewrites the port of `server.listen` (and of a loopback
  `server.public_url`).
- `*_env` keys name **environment variables read at startup**
  (`store.s3.access_key_env` defaults to `AWS_ACCESS_KEY_ID`,
  `store.s3.secret_key_env` to `AWS_SECRET_ACCESS_KEY`;
  `server.ssh.host_key_env` takes the private key itself). A set-but-unreadable
  host-key env is fatal rather than silently re-generating the key.

### 6.3 `config dump` and `config check`

```sh
spindle config dump                  # effective config as TOML + file_state: present|absent|invalid
spindle config check                 # validate the effective config; ignored overrides go to stderr
spindle config check --strict        # exit 3 when any override was ignored (a supervisor pre-start check)
```

| Exit | Meaning |
|---|---|
| `0` | success |
| `1` | a command/config/API error (a validation failure `config check` surfaces, a bad flag value) |
| `2` | usage error, or a missing explicitly-named config file — the command or check did not complete |
| `3` | `config check --strict` saw an ignored override; for an API client command, HTTP 404 (not found) |
| `4` | an API client command failed authentication/authorization (HTTP 401/403, or no credential where one is required) |

With no file present, `config check` validates the effective first-run defaults ⊕
environment, so it is a usable pre-flight on a not-yet-configured host. The full
CLI contract is [`go/11_config_cli.md`](go/11_config_cli.md) §6.

## 7. Backup, restore and upgrade

The **object store is the repository**: back up `store.root`
(`<data-dir>/store` by default) or the S3 bucket, plus `<data-dir>/spindle.toml`
and the SSH host key. The local cache is expendable and is not backed up.
Snapshot a quiesced store with
`sh scripts/backup-store.sh --from <store> --to <archive>` and restore into a
fresh data dir with
`sh scripts/restore-store.sh --from <archive> --to <store>`; S3 needs a
point-in-time copy (quiesce + `aws s3 sync`, or a provider snapshot). The full
procedure, the mid-push caveat, RPO/RTO assumptions and the post-restore checks
are in [`BACKUP.md`](BACKUP.md); `make backup-drill` proves the filesystem path
end to end (seed, snapshot, restore into an empty instance, compare
repo/issue/package/Threads surfaces).

For a new build, run the **new** binary against the **deployed** config before
recreating the task (`spindle config check --config <path> --strict`, then
`spindle doctor`), then start it and verify the surfaces. The whole procedure —
checkpoint and quiesce, preflight, rollback pair, what a restricted boot means —
is [`UPGRADE.md`](UPGRADE.md).

## 8. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Every URL except `/admin`, `/admin/config`, `/healthz`, `/readyz` answers 503 | restricted boot: no config, or an invalid one | fix the keys on `/admin/config` (the page names them), Save, restart — [`OPERATIONS.md` §1](OPERATIONS.md#1-the-restricted-first-boot) |
| The instance boots restricted after a hand edit | the file no longer validates (an unknown or removed key such as `server.auth.mode`, a bad value) | `/admin/config` names each key and links its row; fix and Save, or fix on disk and restart — [`UPGRADE.md`](UPGRADE.md) §5 |
| `/readyz` answers 200 with `"status":"restricted"` | that is a healthy restricted boot, not a failing process | route it. A health-gated ingress that gates it out makes `/admin` unreachable, which is how a restricted instance becomes an outage |
| The Configuration page says **Restart required** | the changed keys are consumed once at startup | restart the process (Maintenance → Restart, `docker compose restart`, `systemctl restart`). A save never restarts it |
| A save succeeded but behaviour did not change | no in-process reload (or the file is invalid and the boot fell back to restricted) | check `/readyz` for `restricted`, then `spindle config check`; restart |
| A `SPINDLE__*` override "does nothing" | a missing segment makes it an unknown key — reported, never applied | spell the full path (§6.2) and run `spindle config check --strict` |
| S3 answers 403 on bucket/object calls | virtual-host addressing against rustfs/MinIO | `store.s3.force_path_style = true` (`SPINDLE__STORE__S3__FORCE_PATH_STYLE=true`) |
| A compose healthcheck never turns healthy | busybox `wget localhost` resolves `::1` while the listener is IPv4 | pin `127.0.0.1` in the check (both shipped stacks do) |
| `pnpm install` fails on a fresh checkout or image build | pnpm 11 blocks dependency build scripts by default | `allowBuilds: { esbuild: true }` in `web/pnpm-workspace.yaml` (already committed) |
| `curl` says 200 but the browser lands on `walgit.localhost:<port>` | the canonical-host redirect (§5) | drive the canonical host, or use a non-browser client |
| `git clone` over SSH is refused | `server.ssh.listen` is empty (default: disabled), or the boot is restricted (the listener does not start) | set the listen address, fix the config, restart; check the key on `/keys` |
| A CI job container cannot reach the server | `server.public_url` is unset or names an address containers cannot resolve | set it to a reachable name ([`threads.md`](threads.md) §4.4) |
| The CLI exits 3 on `config check` but the boot is fine | ignored overrides (a typo'd key, or `WALGIT__*` losing to `SPINDLE__*`) | fix the spelling; the check prints the key |
