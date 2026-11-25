# 11 — Configuration & CLI (`internal/config`, `cmd/spindle`)

> Source: MASTER_RUST_SPEC.md §15 (configuration reference, per-repo settings D24), §16 (CLI reference), §19 (porting notes: clap/exit codes) · Status: normative for the spindle Go implementation.

The binary is `spindle` (module `github.com/crueber/spindle`). It reads the same config files, honors the same environment variables, emits the same exit codes, and exposes the same subcommand surface as the Rust `walgit`, so operators' `walgit.toml`, deployment scripts, and shell habits carry over unchanged. Config parsing uses `github.com/BurntSushi/toml` — the only TOML dependency allowed, and one of the seven direct backend modules in the budget (AGENTS.md law 1: chi, toml, x/net, x/crypto, dicebear-go + dicebear/styles, yaml.v3). One deliberate divergence: **a config file is now OPTIONAL** — a first run with no file boots on built-in defaults (§2.3), and the default file location lives under the data dir (§3.1). All other CLI mechanics (dispatch, flag parsing, duration/size parsing) are hand-rolled or stdlib.

---

## 1. Layering and packages

```
cmd/spindle/            main.go: build-info version, global --config and --data-dir, dispatcher, exit-code mapping
cmd/spindle/cli_*.go    the WP11 API client (cli_api.go client/state/table, cli_auth, cli_user, cli_org,
                        cli_issue, cli_pr, cli_release (releases+checks), cli_misc (packages/threads/api/completion))
internal/config/       file+env loading, decoding, validation, per-repo settings, effective-config dump
internal/config/       (files) config.go, env.go, validate.go, size.go, repo_settings.go, check.go
internal/doctor/       the read-only upgrade preflight (§6.7): config findings + a bounded bucket sample
```

`internal/config` owns everything the rest of the system consumes:

```go
// internal/config/config.go
type Config struct {  // field layout mirrors the §2 key table 1:1; TOML tags keep the Rust key names
    Server    ServerConfig
    Store     StoreConfig
    Cache     CacheConfig
    Wal       WalConfig
    Maintenance MaintenanceConfig
    Placement PlacementConfig
    Compaction CompactionConfig
    Bundles   BundlesConfig
    LFS       LFSConfig  `toml:"lfs"`
    Upstream  UpstreamConfig
    Git       GitConfig
    Telemetry TelemetryConfig
    Events    EventsConfig
}
// Load(filePaths []string, env func(string) string) (*Config, []Override, error) — file, then env
// overlay, then Validate (fail-closed). Returns applied-but-ignored overrides for `config check`.
```

Every consumer receives `*config.Config` (read-only after load; the server treats it as immutable). Duration and byte-size keys are decoded from TOML strings (`"1h"`, `"64GiB"`, `"0B"`) into `time.Duration` / `int64` by the same parser (`size.go`): unit suffixes `ms|s|m|h|d|w` for durations (`d`/`w` only in config strings, never in Go literals) and `B|KiB|MiB|GiB|TiB` (case-insensitive, binary) for sizes; a bare integer for a size means bytes, a bare integer for a duration means seconds. `0`/`"0B"`/`"0s"` mean "disabled" where the table says so. This parser is shared with the per-repo settings merge so `[bundles] min_bytes` accepts the same syntax in both places.

## 2. The complete key table

Copied from Rust spec §15.1; key names, defaults, and meanings are **normative and unchanged**. The Go `Config` struct paths use the same section/field names.

| Key | Default | Meaning |
|---|---|---|
| `server.listen` | `"127.0.0.1:8080"` | bind address; loopback twin for `*.localhost`; `none` auth on a non-loopback bind warns (divergence, §5) † |
| `server.http2` | `true` | h2c / ALPN (Go: `golang.org/x/net/http2` h2c handler) |
| `server.max_concurrent_requests` | `512` | global in-flight cap (advisory) |
| `server.max_concurrent_per_repo` | `64` | per-repo git semaphore |
| `server.request_timeout` | `"1h"` | documented cap |
| `server.drain_timeout` | `"20s"` | phase-2 drain: in-flight requests finish; new work refused |
| `server.serve_sync_timeout` | `"45s"` | how long a serve-level Sync waits for pack materialization (packMu + materialize join); exceeded → 503 + degraded marker while the body keeps warming the cache (issue #320; 05 §5.2) |
| `server.serve_materialize_timeout` | `"10m"` | cap on one detached materialize body; must comfortably exceed `serve_sync_timeout` (issue #320; 05 §5.2) |
| `server.max_push_bytes` | `"64GiB"` | largest accepted push |
| `server.max_tree_log` | `200` | per-entry tree-date walk cap: commits walked per tree listing (07_api.md §9.4, issue #301) |
| `server.roles` | `[]` | `serve` / `maintain` (implies compact+bundle) / `events`; empty = all |
| `server.auto_create_on_push` | `false` | create a repo on first push † |
| `server.accel_redirect` | `false` | answer byte requests with X-Accel-Redirect (edge-fronted hosts only) |
| `server.public_url` | — | pins absolute URIs (bundle-uri, LFS, recipes, OAuth callback) behind a proxy |
| `server.cors_origins` | `[]` | exact or one leading `*.`; empty = no cross-origin lane |
| `server.ssh.listen` | `""` | SSH git transport bind address; empty = disabled (17_ssh.md) |
| `server.ssh.external_port` | `0` | port advertised in `ssh://` clone URLs; 0 = the listen port — the external/internal split (issue #215) |
| `server.ssh.host_key` / `host_key_env` | — | OpenSSH/PEM private key: path / env var NAME; auto-generated ed25519 under `<data-dir>/ssh/` when unset |
| SSH public keys | — | user-managed in the object store via `GET\|POST\|DELETE /api/v1/ssh-keys` and the `/keys` page — not config (17_ssh.md §3) |
| authentication | — | **OIDC only**: issuer, client id/secret, ≥32-byte session secret, an admission allowlist, `groups_claim` and a non-empty `admin_groups` are all required; without them the boot is restricted (`docs/PLAN-OIDC-ONLY.md`) |
| `server.auth.anonymous_read` | `true` | true = public browsing, false = hard-lock |
| `server.auth.tokens[]` | — | `{principal, name?, token \| token_env, write, expires_at?}` — machine credentials over Bearer/Basic, never admin; `expires_at` is RFC 3339, `name` is the stable rotation/usage label |
| `server.auth.admin_groups` | `["spindle-admin"]` | OIDC groups granted global admin (the Administration area); must be non-empty |
| `server.auth.issuer` | — | discovery at `<issuer>/.well-known/openid-configuration` |
| `server.auth.allowed_domains / allowed_emails` | `[]` | admission (email_verified required) |
| `server.auth.write_domains` | — | omit = every admitted identity writes |
| `server.auth.oauth_client_id / oauth_client_secret` | — | browser sign-in; redirect `<public_url>/_auth/callback` |
| `server.auth.session_secret` | — | HMAC key ≥ 32 bytes for cookie + issued tokens (shared by every host; rotation revokes all) |
| `server.auth.session_ttl` | `"30d"` | sliding (re-issued at ttl/4) |
| `server.auth.access_token_ttl` | `"90d"` | `wgt_` token lifetime |
| `server.auth.audiences` | `[]` | accepted `aud` on bearer ID tokens (∪ oauth_client_id) |
| `server.auth.trusted_forwarders` | `[]` | principals allowed to set `X-Walgit-Principal` (push broker hop) |
| `store.backend` | `"filesystem"` | `s3` \| `memory` \| `filesystem` (D4, 03_store_backends.md) †; a bucket backend is selected explicitly, never inherited by omission (OBS-1) |
| `store.bucket` / `store.prefix` | `"walgit"` / `""` | bucket + key prefix |
| `store.root` | — | filesystem backend root (D4); default `<data-dir>/store` (§2.3) † |
| `store.max_retries` | `8` | retryable store errors, jittered backoff |
| `store.multipart_threshold` / `multipart_part_size` | `"64MiB"` / `"32MiB"` | multipart thresholds |
| `store.s3.endpoint` | — | **required when `store.backend = "s3"`** (validation fails loudly; no implicit AWS host, OBS-1) |
| `store.s3.region/access_key_env/secret_key_env/force_path_style` | `"us-east-1"` / `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `false` | incl. rustfs/MinIO (path style true); the `*_env` values are env-var names |
| `cache.dir` | `"/tmp/walgit"` | local cache root (`<dir>/<owner>/<name>.git`) † |
| `cache.mode` | `"auto"` | `budget` \| `disk` \| `auto` (= disk when `maintenance.disk = "ssd"`) |
| `cache.max_bytes` | `"20GiB"` | budget-mode cap for everything on disk; lowered at boot to the cache filesystem's eviction watermark when it is smaller and the value is still the compiled default (§8, BUG-6) |
| `cache.disk_high_watermark` | `0.9` | disk-mode eviction trigger (low mark = −0.10) |
| `cache.evict_idle_after` | `"6h"` | idle repos evicted (budget cap or watermark) |
| `cache.prewarm[]` / `prewarm_parallelism` / `prewarm_ready_timeout` | `[]` / `2` / `"0s"` | warm repos at startup; `/readyz` gating |
| `cache.ref_advert_entries` / `object_info_entries` / `bundle_list_entries` | `256` / `4096` / `128` | render cache sizes |
| `cache.remote_block_bytes` / `remote_object_bytes` | `"1GiB"` / `"256MiB"` | remote-reader block/decoded-object LRU |
| `cache.shared_render_cache` | `true` | mirror immutable API JSON into the bucket for all instances |
| `cache.store_mount` | — | read-only bucket mount; Serve syncs link tier-2 base packs from it |
| `wal.batch_window` / `max_batch` | `"5ms"` / `64` | group commit |
| `wal.push_broker_url` / `push_broker_token` / `push_broker_buffer_bytes` | — / — / `"64MiB"` | forward receive-pack to the single-writer broker; replayable fallback buffer |
| `wal.snapshot_every_entries` / `checkpoint_interval` / `checkpoint_tail_bytes` | `256` / `"1h"` / `"8MiB"` | checkpoint triggers |
| `wal.cas_max_retries` | `16` | manifest CAS retry cap |
| `wal.fsck_objects` / `check_connectivity` | `true` / `true` | pushed-object verification |
| `wal.freshness_ttl` | `"0s"` | 0 = always revalidate the manifest |
| `wal.prefetch_packs` / `prefetch_max_bytes` | `true` / `"1GiB"` | background Serve sync after refs-only; bound |
| `wal.remote_objects` | `true` | remote reader for too-large repos |
| `maintenance.interval` | `"60s"` | pass cadence |
| `maintenance.checkpoints` | `true` | unit 1 enabled |
| `maintenance.max_pack_bytes` | `"0B"` | declared capacity (0 = cache budget) |
| `maintenance.disk` | `"tmpfs"` | `ssd` = may rebuild bases |
| `maintenance.host` | instance id | heartbeat name |
| `maintenance.fsck_interval` | `"7d"` | 0 = off |
| `maintenance.follow_interval` | `"30s"` | upstream-follow loop cadence (0 = off) |
| `placement.serve` / `serve_exclude` / `maintain` / `maintain_exclude` | `["*"]` / `[]` / `["*"]` / `[]` | globs `owner/name` \| `owner/*` \| `*` |
| `compaction.enabled/factor/trigger_packs/trigger_bytes/lease_ttl/retention_superseded/engine` | `true`/`2`/`16`/`1GiB`/`10m`/`7d`/`"git"` | geometric fold; engine is git |
| `bundles.strategy[]` | weekly full + daily + hourly (see 08_bundles.md) | `{name, kind, base?, schedule, keep?, backfill_max, chain?, filter?, refs?, min_commits?}` |
| `bundles.min_commits` / `min_bytes` | `25` / `"0B"` | incremental gates |
| `bundles.main_only` / `extra_refs` | `true` / `[]` | bundle ref set |
| `bundles.serve_via` / `signed_url_ttl` / `signed_url_for` | `"proxy"` / `"1h"` / `[]` | byte path |
| `bundles.advertise` / `advertise_filtered` | `true` / `false` | v2 advertisement; filtered families only with the patched git |
| `bundles.require` | `[]` | repos whose unbounded zero-have clones must use bundle-uri (D17) |
| `lfs.enabled` / `serve_via` / `signed_url_ttl` / `max_object_bytes` | `true` / `"proxy"` / `"1h"` / `"16GiB"` | LFS |
| `attachments.max_image_bytes` | `"8MiB"` | per-file cap for pasted/dropped issue images (02 §12; over cap → 413) |
| `packages.max_blob_bytes` | `"10GiB"` | one OCI blob object (GHCR per-layer parity); over cap → 413 `SIZE_INVALID` (14 §5) |
| `packages.max_manifest_bytes` | `"4MiB"` | one manifest body; over cap → 413 `SIZE_INVALID` (14 §5) |
| `packages.upload_ttl` | `"24h"` | chunked-upload staging lifetime; expired ids are `BLOB_UPLOAD_UNKNOWN` (14 §5) |
| `packages.quota_bytes` | `"0B"` | instance default per-repository registry quota (stored blob + manifest bytes); `0` = unlimited; a repository's `[packages] quota_bytes` setting overrides it; over quota → named `413 DENIED` at the commit point (14 §5) |
| `threads.retention` | `"720h"` (30d) | terminal Threads runs (with their job records, log frames and queue entries) older than this are deleted by the maintainer's `threads-gc` pass; non-positive falls back to the default — there is no "keep forever" spelling (15 §8) |
| `threads.approve_first_time_forks` | `true` | hold a fork pull request from an author with no merged PR in the repository in `pending_approval` until a maintainer approves it (15 §5.2); `false` disables the gate |
| `threads.max_artifact_bytes_per_run` | `2GiB` | per-run total for published artifacts (sum of every artifact of the run; enforced on upload, finalize and download; 15 §2.5). A named `413` states the cap and this key |
| `threads.max_artifact_bytes_per_file` | `256MiB` | one artifact file; clamped down to the per-run value when larger. A named `413` states the cap and this key |
| `threads.default_permissions` | `"read"` | the job token's `contents` level when a workflow declares no `permissions:` (`read` \| `write`); a fork run is always `read` (15 §5) |
| `upstream.git` / `upstream.lfs` / `upstream.token_env` / `upstream.follow` | — | repair source; LFS read-through; token env var (never the token); refs kept equal |
| `git.binary` | `"git"` | path to git binary (spindle always execs it; see 04_git.md) |
| `git.upload_pack_engine` | `"auto"` | `auto` = stock git wherever packs are local/mounted; remote-served bases per 04_git.md |
| `git.allow_filter` / `allow_any_sha1_in_want` | `true` / `false` | uploadpack.* |
| `git.object_format` | `"sha1"` | default for new repos |
| `git.commit_graph` / `commit_graph_changed_paths` | `true` / `false` | split commit-graph chain per repo; Bloom filters |
| `git.history_pack` | `true` | base rebuild also publishes the commits+trees pack |
| `git.max_wants` | `0` | refuse a fetch wanting more objects (0 = off) |
| `telemetry.log_format` / `log_filter` | `"pretty"` / `"info,walgit=debug"` | JSON or pretty logs; `RUST_LOG` env wins if set (accept `SPINDLE_LOG` too, see §3.4) |
| `telemetry.metrics` | `true` | Prometheus on /metrics |
| `telemetry.lock_wait_warn` | `"1s"` | lock-wait WARN + histogram threshold |
| `events.webhook_url` / `webhook_secret` / `sweep_interval` | — / — / `"5m"` | events bridge (09_events.md) |
| `rate_limit.enabled` | `false` | request rate limiting (PLAN-V2 §M.7). Off by default: a zero-config boot changes nothing (L10). In-process, built at boot — every key below is **restart-required**, and `/admin/config`'s restart list names each changed one |
| `rate_limit.collab_write` | `"30/min"` | comments, review submits/dismissals, reactions, review-thread opens/comments, review requests |
| `rate_limit.issue_open` | `"10/h"` | opening an issue or a pull request |
| `rate_limit.report` | `"5/h"` | filing an abuse report |
| `rate_limit.auth_fail` | `"10/min"` | failed credential attempts per address |
| `rate_limit.trust_proxy` | `false` | key anonymous clients on the first `X-Forwarded-For` hop; only with a proxy that overwrites the header |
| `static_sites.enabled` | `false` | serve published static sites from a **second HTTP listener** on its own hostname (docs/features/19, 06_server_http.md §2.6); `false` binds nothing and the rest of the section is not validated |
| `static_sites.address` | `"127.0.0.1:8081"` | the static listener's own `host:port`; must differ from `server.listen` |
| `static_sites.host` | `"static.localhost"` | the ONLY `Host` header the static listener answers for (port-insensitive, case-insensitive); every other Host is a bodiless 404 |
| `static_sites.max_file_bytes` | `"8MiB"` | per-file cap on the buffered read path; a larger blob is a 404 (v1 has no streaming path) |
| `static_sites.index_files` | `["index.html","index.htm"]` | directory-index probe order inside a tree; a directory matching none is a 404, never a listing |
| `search.enabled` | `true` | code search over each repository's serving copy (`docs/features/20_search.md`). A `*bool` so "absent" stays distinct from an explicit `false`; absent/nil means enabled, and `false` answers the search routes as not found. Operator-only, restart-required (the service and its request pool are built at boot). No index, no bucket key |
| `search.max_concurrent` | `2` | concurrent searches per instance (`0` = the default 2; ceiling 16). One search can fan out to four grep workers, so this is the CPU bound a public repository cannot turn a stranger into; every other search cap is a package constant in `internal/search` (20_search.md §5.2) |

The four rates are spelled `N/s`, `N/min` or `N/h` with `N > 0` (a token bucket of capacity `N`
refilled continuously over the window). Anything else — `30/m`, `30 per minute`, `0/min`, a day unit —
is a validation error naming the exact key, so the Configuration page can mark the row (§7). Git
pushes and reads are never rate limited (a CI push storm must not throttle); the classes, keying,
`Retry-After` and the in-memory bound are §2.4 of `06_server_http.md`, and the normative product
contract is `docs/features/16_moderation.md` §12.


Notes:
- `git.upload_pack_engine`: the Rust `gix` engine has no Go equivalent and none is wanted (dependency law). In spindle the accepted values are still `auto | git | gix` for file compatibility, but `gix` is treated exactly as `git` at load time **with a one-time startup WARN naming the key** (ruling C-4: an inert config value must be visible to the operator); `auto` resolves to `git`. See 04_git.md for the upload-pack implementation.
- `telemetry.log_filter` keeps the Rust EnvFilter syntax as a string; the Go logger interprets the `info,walgit=debug` shape by mapping the `walgit` target to `spindle` (see §7.3 of 06_server_http.md — that doc owns logging).
- Bundle strategy array entries (`[[bundles.strategy]]`) decode in declaration order; `bundles.strategy` order is meaningful for §11 default families.
- Rows marked † have a different effective value on a zero-config first run; §2.3 is normative there. The table otherwise lists the compiled-in defaults, which equal the Rust-spec defaults.

### 2.1 Example: minimal config

```toml
# walgit.toml — one-box, loopback, token auth off, explicit S3 backend
[server]
listen = "127.0.0.1:8080"

[store]
backend = "s3"
bucket = "walgit"

[store.s3]
endpoint = "http://127.0.0.1:9000"   # rustfs/MinIO on the same box
region = "us-east-1"
force_path_style = true
access_key_env = "AWS_ACCESS_KEY_ID"
secret_key_env = "AWS_SECRET_ACCESS_KEY"

[cache]
dir = "/tmp/walgit"
```

### 2.2 Example: standalone-ish (public, token auth)

```toml
[server]
listen = "0.0.0.0:8080"
public_url = "https://git.example.com"   # the reverse proxy's origin (it terminates TLS; 16_packaging.md §3.4)

[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
oauth_client_id = "spindle"
oauth_client_secret = "…"
session_secret = "…"                  # openssl rand -hex 32
admin_groups = ["spindle-admin"]
anonymous_read = false
[[server.auth.tokens]]                # machine credentials (git/API/CLI only)
principal = "ci-bot"                  # the identity it authenticates as
token_env = "WALGIT_TOKEN_CI"         # or `token = "…"` (masked in the editor)
write = true
name = "ci-bot-2026h2"                # optional stable label (rotation + last-used key)
expires_at = "2026-12-31T23:59:59Z"   # optional RFC 3339; omit = never expires (warned)

[store]
backend = "s3"
bucket = "acme-walgit"

[cache]
dir = "/var/cache/spindle"
mode = "disk"                   # real SSD host: full materialization + watermark eviction

[git]
object_format = "sha1"
```

### 2.3 First-run defaults (zero-config boot; divergence D5)

When no config file is found (§3.1), the server boots anyway on these **first-run defaults**. They replace the compiled-in defaults of the table above for the marked keys; every unmarked key keeps its table value. User friendliness is a first-class law: `spindle` with zero configuration must serve git over HTTP on a fresh machine.

| Key | First-run value | Why it differs |
|---|---|---|
| `server.listen` | `"0.0.0.0:8080"` | a first-run server should be reachable, not only loopback |
| `store.root` | `<data-dir>/store` | the filesystem backend's root, inside the data dir (`store.backend` is already `filesystem` in the compiled table) |
| `cache.dir` | `<data-dir>/cache` | the data dir owns all writable state — no `/tmp` surprise |
| authentication | **unconfigured** | a fresh machine has no IdP; the boot is restricted (only the Administration area + health answer) until OIDC is saved |
| `server.auto_create_on_push` | `true` | `git push` to a fresh server should just work |

All other keys take their table (Rust-spec) defaults. The effective first-run config is exactly what `config dump` prints when no file is present, and what `GET /api/v1/setup` reports as effective values with `file_state = "absent"`.

A first-run boot logs `first run: OIDC is not configured — serving only /admin until a config is saved and the server restarted` and serves the restricted surface. Its Overview carries the OIDC onboarding card; there is no warning-only compromise and no open mode (`DEVIATIONS.md` D-OIDC-1).

## 3. Loading order, env overrides, PORT lockstep

### 3.1 The ladder

1. **Defaults.** The `Config` struct's Go zero values are NOT the defaults; `Config` values are first set from the compiled-in default table (§2). This is a literal table (`defaultConfig()` returning a fully populated `Config`), because the zero value of `time.Duration(0)` or `int64(0)` legitimately means "off" for some keys — the defaults must not be confusable with "off". When no config file exists at all, the first-run defaults of §2.3 are applied on top of the compiled-in table before the remaining legs run.
2. **File (optional).** The config file is looked up in this order, first hit wins:
   - **Explicit `--config PATH`** (or `SPINDLE_CONFIG` / legacy `WALGIT_CONFIG` env when `--config` is absent). BurntSushi/toml `DecodeFile` into a fresh struct seeded from defaults; unknown keys are an error listing key + line (fail-closed, same as serde's `deny_unknown_fields` behavior in the Rust binary). **A missing explicitly-named file is fatal (exit 2, `config file not found: <path>`)** — if the operator pointed at a file, silence would be a footgun. Explicit `--config /dev/null` = defaults+env only (file leg is skipped when the path is `/dev/null`).
   - **`<data-dir>/spindle.toml`**, if it exists (data dir: §3.1.1). `<data-dir>/walgit.toml` is accepted as an alias (checked second). If the file exists but is INVALID — parse error or unknown key — boot enters **restricted mode** (06_server_http.md): only `/admin` + `/admin/config`, `/healthz`, and `/readyz` answer; everything else returns 503 with a pointer to the errors, and the Administration area displays them. Saving a fixed config (§3.7) then requires a restart.
   - **No file** → zero-config first run on the §2.3 first-run defaults, with the setup banner (§2.3).
3. **Env overlay.** `SPINDLE__SECTION__KEY=value` (legacy `WALGIT__SECTION__KEY=value` accepted as a fallback alias; `SPINDLE_` wins when both define the same key) on top of the file result (§3.2) — or on top of the defaults when no file was loaded. The overlay applies identically in all three file states; **env always wins over the file** (file ⊕ env, documented).
4. **PORT lockstep.** If `PORT` env is set, it overrides the port portion of `server.listen` AND, when `server.public_url` is a loopback URL (`http://127.0.0.1:PORT`, `http://localhost:PORT`, `http://[::1]:PORT`), rewrites its port to the same value (§3.3).
5. **Validation.** Fail-closed; any violation is exit 2 with the offending key(s) and reason on stderr (§5) — with the single warn-only exception of rule 1 (auth-none loopback, divergence).

#### 3.1.1 The data dir (divergence D5)

All writable state lives under one **data dir**, selected by:

1. the global `--data-dir PATH` flag (accepted in the same argv positions as `--config`);
2. else the `SPINDLE_DATA_DIR` env var;
3. else the default: `~/.local/share/spindle` (XDG), or `/var/lib/spindle` when the process detects a container context (`/.dockerenv` present or `KUBERNETES_SERVICE_HOST` set).

The data dir holds `<data-dir>/store/` (filesystem store backend root, `store.root` when `backend = "filesystem"`, D4), `<data-dir>/cache/` (first-run `cache.dir`, §2.3), and the saved config `<data-dir>/spindle.toml` (written by the Administration area, §3.7; `walgit.toml` alias accepted).

The directory is created (mode `0700`) on first boot if missing. `--data-dir` never sets a config value directly — it only relocates the file leg (§3.1 step 2) and provides the default expansion for `store.root` / `cache.dir` in the first-run table; an explicit `store.root` or `cache.dir` in the file or env always wins. Like `--config`, `--data-dir` is peeled before subcommand dispatch (§6.3) and is honored by every subcommand that loads config. Concretely, after the ladder, `resolveConfig` re-points the flag-derived paths (`DataDir`, plus `Store.Root`/`Cache.Dir` when they still equal the env-default-derived paths) at the flag dir — the shared #611 sync; `serve` repeats the same comparison as an idempotent no-op.

### 3.2 Env override mechanics

Decision: **primary prefix `SPINDLE__`, legacy `WALGIT__` accepted as an alias** — new deployments and docs use `SPINDLE__`, while deployment scripts, systemd units, and secret manifests written for walgit keep working unchanged, and both binaries can be mixed in one fleet. When both spellings are set for the same key, the `SPINDLE__` value wins and the `WALGIT__` one is ignored (reported as an ignored override, never fatal). Same rule for the config-file pointer: `SPINDLE_CONFIG` primary, `WALGIT_CONFIG` fallback.

- Pattern: `WALGIT__` + section path joined by `__`, value = TOML value syntax. `WALGIT__STORE__BUCKET=acme` sets `store.bucket = "acme"`. Nesting beyond one level: `WALGIT__SERVER__AUTH__ISSUER=https://id.example.com` sets `server.auth.issuer`; a removed auth key (`…__MODE`, `…__ADMIN_EMAILS`) is a named fatal error.
- Array-of-table (`[[bundles.strategy]]`) overrides: `WALGIT__BUNDLES__STRATEGY__0__BACKFILL_MAX=1` (index after the section, zero-based, whole-array replacement semantics like §3.4 placement). An override naming index N when the file provides fewer entries is an error (exit 1): the operator asked to mutate a slot that does not exist.
- The value is parsed as a TOML value (TOML "value" productions only — strings, integers, floats, booleans, arrays, inline tables). `WALGIT__SERVER__ROLES='["serve"]'` is an array; a bare `serve` is a parse error naming the key. Dates/datetimes are not needed by any key and are rejected.
- Scalars under `[bundles.strategy]` like `WALGIT__BUNDLES__MIN_COMMITS=10` go through the same typed decode as the file leg: the string is decoded as TOML, then assigned; a type mismatch with the target field is an error (exit 1) naming the key, the raw value, and the expected type.
- Unknown section/key in an env override: the key is recorded as an **ignored override** and validation reports it; see `config check` (§6) for how this is surfaced and the `--strict` exit-3 path. The process still boots otherwise (a stale `WALGIT__STORE__BKUET` typo must not take down a host — but it must be visible).
- `token_env`-style indirection is unaffected: `server.auth.tokens[].token_env` names another env var read at use time, as in Rust.

### 3.3 PORT override and loopback public_url lockstep

```go
// internal/config/env.go
func applyPort(c *Config, getenv func(string) string) error {
    p := getenv("PORT"); if p == "" { return nil }
    port, err := strconv.Atoi(p)
    if err != nil || port < 1 || port > 65535 { return fmt.Errorf("PORT: %q is not a TCP port", p) }
    host, _, err := net.SplitHostPort(c.Server.Listen)   // error = exit 2
    c.Server.Listen = net.JoinHostPort(host, strconv.Itoa(port))
    if u, err := url.Parse(c.Server.PublicURL); err == nil && u.Host != "" && isLoopbackHost(u.Host) {
        u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
        c.Server.PublicURL = u.String()
    }
    return nil
}
// isLoopbackHost: 127.0.0.0/8, ::1, "localhost". A non-loopback public_url (the normal proxied case)
// is NOT touched: PORT is a local-dev/reverse-proxy-port knob, not a general URL rewriter.
```

### 3.4 Placement all-or-nothing

If ANY `WALGIT__PLACEMENT__*` override is present, the entire `[placement]` section is taken from the environment: the file's `[placement]` table is discarded first, then the four keys are defaulted (`serve=["*"]`, `serve_exclude=[]`, `maintain=["*"]`, `maintain_exclude=[]`) and only the env-provided ones assigned. Rationale: placement lists are the one config where a partial override silently changes which host does what; half-file-half-env would be un-auditable. This mirrors the Rust behavior. (Other sections merge key-by-key; placement is the sole exception.)

### 3.5 Validation is fail-closed

Every rule in §5 runs after the ladder completes. A violation aborts startup (exit 2) or `config check` (exit 1). The one softer path is the ignored-override treatment for unknown-key env overrides (§3.2), because those cannot change behavior. Everything else remains fail-closed (§5).

### 3.6 Concurrency

Config is loaded once, before any goroutine that reads it is started, and is never mutated afterwards. `cmd/spindle` passes `*Config` by value (the struct is read-only by convention) to subsystem constructors. No locks, no atomics, no copy-on-write: the hazard (a subsystem mutating shared config mid-flight) is avoided by construction — nothing in spindle holds a `*Config` it may write to. If a future feature needs hot-reload, it must own its own snapshot (see 13_concurrency.md "immutable snapshot" pattern); it MUST NOT mutate the shared `*Config`.

### 3.7 Setup save semantics (divergence D6)

The Administration area (`/admin/config`) and its API (`GET /api/v1/setup`, `POST /api/v1/setup/test`, `PUT /api/v1/setup`; endpoint shapes and access rules are 06_server_http.md's job — this doc fixes the file semantics) write the config file:

- **Atomic write.** The setup API serializes the submitted config to TOML (same key names as §2), writes it to `<data-dir>/spindle.toml.tmp`, fsyncs, then `rename`s it over `<data-dir>/spindle.toml`. Readers see either the old or the new file, never a partial one. Permissions `0600` (the file may contain `token_env`-adjacent material; secrets themselves never live in the file — `token_env` names an env var).
- **Validate before write.** `PUT /api/v1/setup` runs the full §5 validation on the submitted values FIRST; invalid input writes nothing and returns the per-key errors (this is also what `POST /api/v1/setup/test` does without saving). Only an all-green submission is written.
- **Restart-required keys.** The save response reports, per key, whether it takes effect without a restart. Restart-required (the default — most keys are consumed once at subsystem construction: store backend, WAL, cache, auth, listen address, …) vs. read-live (picked up on the next tick/emit): `telemetry.log_format`, `telemetry.log_filter`, `maintenance.interval`, `maintenance.follow_interval`, `wal.freshness_ttl`. spindle does NOT hot-reload the shared `*Config` (§3.6); read-live keys are re-read from the effective config at their point of use, which the setup save updates by writing the file and refreshing the in-memory first-run overlay — a restart is still the supported way to be sure.
- **Edits apply to the current file (§3.4).** Submitted overrides merge onto the file-visible config — compiled-in defaults ⊕ the first existing candidate file, or the zero-config first-run shape when no file exists — so a save changes only the keys the operator touched; keys the form does not carry keep their file values. Env overlays are never baked into the file: the file keeps its own values and env still wins at load (see the env-interaction bullet below).
- **Interaction with env (file ⊕ env, env wins).** The saved file participates in the ordinary ladder (§3.1): env overrides and PORT lockstep still apply on top of it, and an env override of a key the operator just set in the UI will silently win — the Administration area therefore shows the effective value (post-env) for every key and warns when a submitted value is masked by an active `SPINDLE__`/`WALGIT__`/`PORT` override. Setup never writes env vars.
- **Invalid-config rescue.** When boot landed in restricted mode (§3.1 step 2), a successful setup save writes a valid file but does NOT swap the running config; the response says `restart_required: true` for every key and the UI tells the operator to restart. (Boot flow: config present → validate → boot if it all works; config missing → defaults + setup banner; config INVALID → restricted mode.)
- **Every advertised key round-trips (G1 fix, 2026-11-21).** The page renders each schema key's current effective value in the spec spelling and posts it back through `setup.Coerce` (the shared `config` parsers). Struct slices (`server.auth.tokens`, `bundles.strategy`) are repeating rows over the element's toml fields — the element schema travels in the row so no column is invented — and are rebuilt in posted order, with secrets kept by row identity (`__base`), so deleting row 0 can never hand row 1 the deleted credential. Unsigned counters (`wal.snapshot_every_entries`, `wal.cas_max_retries`) coerce like ints. A blank value for a list field is the empty list; a nil slice stays nil (it encodes to nothing), so an untouched form changes **no value** — the acceptance gate is `TestFormValuesWholeSchemaRoundTrip` (whole schema, `internal/setup`: render → coerce → decode → re-encode identical) plus `TestSetupPageZeroEditFormSavesByteIdentical` (GET /admin/config → scrape every control → POST it back unmodified → 303 with the effective values unchanged, a second save of the now-canonical file byte-identical, and a rejected change leaving the file untouched while naming its key; `cmd/spindle`). Validation errors carry the schema key their message names (`setup.Validate` → `Error.Key`), so a failure marks the offending row instead of printing one keyless line on every row.
- **Coercion errors are keyed (round-3 audit fix, 2026-11-21).** `setup.Coerce` returns a `setup.Error{Key, Message}` (the same type `Validate` uses; it implements `error` and is lifted back with `setup.KeyOf`), so a value its parser rejects — duration, size, int, uint, float, bool, list, struct-slice cell, even a nested unknown key — marks its row exactly like a `config.Validate` rejection and opens a collapsed Advanced group. A struct-slice cell is keyed `key[i].field` and the page attaches it to the owning row editor by the bracket prefix. Only a genuinely keyless failure stays in the banner. The acceptance gate is `TestCoerceFailuresCarryTheirKey` (every field type, `internal/setup`), `TestUnknownSetupKeyCannotPassSilently` (negative control), and `TestSetupPageCoercionErrorMarksItsRow` (rendered row + Advanced group + KEY cell, `cmd/spindle`); the JSON 422 body carries the same key.
- **Keyed errors name the row's anchor (2026-11-21, PLAN-ADMIN M3).** `setup.AnchorOf(key)` is the
  total key→section map the Configuration page renders (`/admin/config#<section>`, with the auth
  card's `#server.auth` alias), so every validator/coercion error that names a key also names where
  to fix it: the page's `role="alert"` summary links each error to its row (`#row-<key>`; a
  struct-slice cell to its row editor; an unmounted key to its section), marks the row with a
  visible `invalid` chip plus `aria-invalid`/`aria-describedby`, opens the Advanced group, and
  focuses the offending control. The same coordinate system serves the boot-error table, the
  saved-restart list and the docs. `setup.AnchorOf` is pinned total over `BuildSchema`
  (`internal/setup/anchor_test.go`).
- **Numeric coercion never saturates (closing-audit Observation 3, 2026-11-21).** `setup.Coerce` rejects a number outside its field's range — or one its parser cannot represent — as a keyed `setup.Error` that names the key and the accepted range (`setup.parseSettingInt`/`parseSettingUint`), so the page marks the row and `PUT /api/v1/setup` answers 422 with that key instead of persisting a clipped value. Widths are bounded by the Go field (`int8..int64`, `uint8..uint64`, named types: `-2147483648..2147483647`, `0..4294967295`, `0..18446744073709551615`); sizes by `0..9223372036854775807` bytes (the `ByteSize` int64 range, with config.ParseByteSize's 2^63 boundary fixed); durations by `time.Duration`'s maximum (`2562047h47m16.854775807s`); floats must be finite and parse at the field's width. Scientific notation is still accepted for ints (`1.073741824e+09`) but only where float64 represents the integer exactly (|value| < 2^53), so a value like `99999999999999999999` is an error, never `MinInt64` (which is what the pre-fix `int64(f)` conversion produced on amd64 while reporting a successful save). A legitimate boundary value (MinInt64/MaxInt64, MaxUint32, MaxInt64 bytes) still saves. Gates: `TestNumericCoercionNeverSaturates` (every width and spelling), `TestStructSliceCellOverflowIsKeyedAndChangesNothing` (row cell; rejected save leaves the file byte-identical), `TestSetupAPIRejectsOutOfRangeNumbers` (PUT → 422 with the key, file untouched), and `TestNumericBoundaryMessagesNameTheRange` (the range is spelled in the message).
- **The canonical rewrite keeps the operator's file, once (round-3 audit fix, 2026-11-21).** Save writes the FULL canonical document — BurntSushi/toml decodes comments but cannot re-emit them, and hand-rolling a trivia-preserving TOML editor was rejected in favor of a backup (law 1: no new dependency). Before the first rewrite of an existing file, `config.SaveSetup` copies the previous bytes to `<data-dir>/spindle.toml.bak-<utc>` (`0600`, once — a later save sees it and keeps it), and a save whose bytes already match the file writes nothing at all. The page says both next to Save and names the backup after the save; the API response reports it as `backup` (empty when none was made). A read error on an existing file fails closed rather than overwriting text that could not be preserved. Gates: `TestSaveSetupKeepsOneBackupOfExistingFile` (`internal/config`) and `TestSetupPageCanonicalSaveKeepsOneBackup` (`cmd/spindle`, rendered).

## 4. Per-repo settings (D24)

### 4.1 Shape

Per-repo settings are published INTO the WAL (inline on the manifest + SETTINGS log entries with history) and merged over the host config by `with_settings`. The Go surface:

```go
// internal/config/repo_settings.go
type RepoSettings struct {  // the four allowed sections; zero = "not set, inherit host config"
    Bundles      *BundlesConfig      `toml:"bundles"`
    Maintenance  *MaintenanceConfig  `toml:"maintenance"`
    Compaction   *CompactionConfig   `toml:"compaction"`
    Upstream     *UpstreamConfig     `toml:"upstream"`
    Integrations map[string]toml.Primitive `toml:"integrations"` // accepted, forward-compat, never interpreted
    // Passengers (validated, persisted, revisioned, admin-written — never merged):
    Description  string              // issue #235 display metadata
    Features     *RepoFeatures       // Forgejo #522 [features] six flags, absent = all-on
    Review       *RepoReview         // Forgejo #586 [review] allow_self_approval, absent = allowed
    StaticSite   *RepoStaticSite     // docs/features/19 [static_site] enabled/ref/root, absent = not published
}
func (r *RepoSettings) Merge(base *Config) (*Config, error)  // "with_settings": pointer-set fields override base
```

### 4.2 Rules (normative, unchanged from Rust spec §15.2)

- Allowed sections: `[bundles]`, `[maintenance]`, `[compaction]`, `[upstream]`, `[features]`
  (Forgejo #522), `[review]` (Forgejo #586), `[static_site]` (docs/features/19); `[integrations]`
  accepted (stored verbatim, forward-compat). Anything else → 400 at publish.
- Top-level `description` key (issue #235): the per-repo short display string shown in the repo header. Single line (no CR/LF/NUL), at most 512 characters (`MaxRepoDescriptionRunes`); violations → 400 like any other invalid key. Display metadata only — never merged into the host config (`Merge` ignores it), never in `settings/effective`. Rides the same WAL-published document, so revision/author/admin-writes come free.
- `[review] allow_self_approval` (Forgejo #586): the per-repo self-approval knob — `true`
  (or absent key/section: default ON, fresh repos behave like GitHub) lets the PR author
  `APPROVE`/`CHANGES_REQUESTED` their own PR; `false` keeps the historical `422`.
  Behavior metadata only — never merged into the host config (`Merge` ignores it), never
  in `settings/effective` (the `[features]` passenger precedent). Wrong type or unknown
  `[review]` key → 400 like any other invalid key.
- `[static_site]` (docs/features/19 §5): `enabled` (`*bool` — absent and explicit `false` both mean
  **not published**, but they stay distinguishable so the settings summary can say "unset"),
  `ref` (`""` = the repository's default branch tip; otherwise a git ref name, short branch or full
  `refs/…`), `root` (`""` = the repository root; otherwise a clean relative path with no `..`). Same
  passenger discipline: validated + persisted + revisioned + admin-written with the document, never
  merged, never in `settings/effective` — the listener is the host section `[static_sites]`, so there
  is no per-repo default to inherit. A ref that is not a valid git ref name, an absolute root, or a
  root that is not a clean relative path is a 400 at publish; an empty, unparseable or invalid
  settings document resolves to "not published" (fail closed — an unreadable document must never
  expose a repository's bytes on another origin).
- Size: the serialized settings payload MUST be ≤ 16 KiB; larger → 400.
- `[integrations]` contents are stored verbatim and never interpreted; the 16 KiB budget includes them.
- NOT settable via settings: auth, store, server, wal, cache, `upstream.token_env` (host-only). A `[server]`, `[store]`, `[wal]`, `[cache]`, or `upstream.token_env` key inside repo settings → 400.
- Validation at publish runs the host build's `with_settings` merge + validation (§5) against the would-be effective config; invalid → 400, nothing published. This means a settings payload must pass the same fail-closed validation as a host config, restricted to the merged sections.
- Write = admin (API gating is 06_server_http.md's job; `internal/config` exposes the validator and the merge).
- `GET …/settings/effective` returns only those sections (07_api.md owns the endpoint shape).
- Every instance sees a new settings revision on its next refs-level sync, with zero extra round trips — 05_wal_engine.md implements the manifest-inline + SETTINGS log mechanics; this doc fixes only the payload semantics.

### 4.3 Example

```console
$ cat /tmp/settings.toml
[bundles]
main_only = false

$ spindle repo settings set acme/monorepo --file /tmp/settings.toml -m "bundle all heads"
```

## 5. Validation rules (fail-closed — one warn-only divergence; exit 2 at boot, exit 1 in `config check`)

Each rule is a named function in `internal/config/validate.go`; the list is the contract:

1. **OIDC is configured (always on).** `checkAuthConfigured` requires: `issuer` an absolute http(s) URL; at least one of `allowed_domains` / `allowed_emails`; a non-empty `groups_claim`; a non-empty `admin_groups` (an empty list is a guaranteed lockout); and all three of `session_secret` / `oauth_client_id` / `oauth_client_secret`, with `session_secret` ≥ 32 bytes. Every finding names its key. There is no `server.auth.mode`, no warning-only path, and removed keys (`mode`, `admin_emails`, `admin_domains`, a token row's `admin`) are named errors (§4.1 of PLAN-OIDC-ONLY). `anonymous_read` is orthogonal (Forgejo #371).
2. **static-token rows** (`checkStaticTokens`): `[[server.auth.tokens]].expires_at`, when set, parses as RFC 3339 (the key is named in the error); a PAST instant is deliberately accepted and simply makes the credential immediately expired (a config saved before its deadline keeps validating after it) — it is not a warning, because the requested lifetime has simply run out. A non-empty `name` is unique across rows: it is the stable label the rotation procedure and the last-used record key on, so a duplicate would make both ambiguous. An empty `name` is valid (the Access page then identifies the row by a SHA-256 hash prefix, never the secret).
3. **bundle strategy validation.** For each `[[bundles.strategy]]`:
   - `kind = "incremental"` requires `base` naming an earlier-declared strategy's `name` (unknown or later-declared base → error);
   - a whole chain shares one `filter`: a strategy and its transitive base chain MUST declare identical `filter` values (absent = none);
   - `keep` on an incremental strategy fails (keep is a full-strategy concept);
   - `schedule` is a valid 6-field cron expression or one of `@hourly|@daily|@weekly|@monthly|@yearly` (the parser lives in 08_bundles.md; validation here only checks it parses);
   - `filter`, when present, is only `"blob:none"` (Rust accepts nothing else);
   - `backfill_max` ≥ 0; `min_commits` ≥ 0; a strategy's `refs` globs compile as `owner`-free git ref globs.
4. **chain-shared filter (restated).** A filtered chain is all-or-nothing: mixing `filter = "blob:none"` with a filterless strategy in one lineage is the classic footgun and is rejected by name in the error.
5. **store**: `store.backend ∈ {s3,memory,filesystem}` (the compiled default is `filesystem`; a bucket backend is always an explicit choice). For s3, `store.s3.endpoint` and non-empty `store.bucket`/`store.s3.access_key_env`/`store.s3.secret_key_env` are required — selecting `s3` without them is a validation error naming each missing key, never a silent AWS default. The `*_env` values are env-var **names**; whether they resolve to values is checked at use time, not at load (the Rust behavior). `multipart_part_size` ≤ `multipart_threshold` when both are non-zero; for filesystem, `store.root` (default `<data-dir>/store`, §2.3) must be an absolute path or empty (empty = the data-dir default).
6. **sizes/timeouts**: every duration/size key parses (§1); `cache.disk_high_watermark ∈ (0,1)` or 0; negative values nowhere.
7. **placement globs** compile: each entry is `*`, `owner/*`, or `owner/name` (one `/` at most).
8. **roles**: each role ∈ {serve, maintain, events}; duplicates allowed (idempotent), unknown role → error.
9. **paths**: `cache.dir` is absolute unless backend is `memory`.
10. **ssh** (17_ssh.md §3): config validation checks `listen` shape and `external_port`
    range — when `[server.ssh]` is present and `listen` is set, it must be host:port;
    `external_port` must be a TCP port 1-65535 when set (0 = unset = the listen port). The host key is a boot-time check in
    the listener builder, not config validation: `host_key_env` set-but-empty is fatal there
    (a silent substitution would bypass the client's pinned host key; auto-generation covers the
    unset case). SSH public keys are user-managed data in the object store (`/api/v1/ssh-keys`),
    not config — nothing else to validate here.

11. **rate limits** (PLAN-V2 §M.7): each of `rate_limit.collab_write` / `issue_open` / `report` /
    `auth_fail` is `N/s`, `N/min` or `N/h` with `N > 0` (`config.ParseRate`); anything else is an
    error naming the key — `rate_limit.report: "5/day" is not a rate (accepted N/s, N/min or N/h
    with N > 0, e.g. 30/min)`. Validation runs even when `enabled = false`, so a file that is
    switched on later cannot carry a value the loader would have refused. The parser and the
    server's bucket builder are the same function; there is no second spelling.

12. **static sites** (`checkStaticSites`, docs/features/19 §5): with `static_sites.enabled = false`
    the rest of the section is not validated (an operator may stage a config before turning the
    listener on). With `enabled = true`: `host` is required and must not carry surrounding
    whitespace (the handler compares it exactly); `address` is required, must parse as `host:port`,
    and must not equal `server.listen`; `max_file_bytes` must be positive; every `index_files` entry
    is a plain file name (no separator, no `..`, not empty, not `.`). The `address` vs
    `server.listen` comparison is an **exact string comparison of the two configured values** —
    nothing is normalised, so `:8080` vs `0.0.0.0:8080` (or `localhost` vs `127.0.0.1`) passes
    validation and fails at `net.Listen` with a clear bind error; a home-grown normaliser would have
    to guess which spellings are the same bind and would reject valid configs. Every message names
    its key so the Configuration page marks the row.

13. **search** (`checkSearch`, docs/features/20_search.md §5.2/§11): `search.max_concurrent` is
    non-negative and at most 16 (`0` = the default 2) — a larger value would let searches compete
    with clone/upload-pack for CPU. `search.enabled` is a `*bool` (nil = enabled), so there is
    nothing to validate there. Every other search cap (pattern bytes, pathspec count, match/byte
    caps, deadlines, candidate and worker counts) is a package constant in `internal/search`, not a
    config key.

## 6. CLI: `spindle`

### 6.1 Global shape, exit codes, version

- Binary: `spindle`. `spindle [--config PATH] [--data-dir PATH] <command>`; `--config`/`--data-dir` may also appear after the subcommand (`spindle serve --config x.toml`) — both positions are accepted, last wins.
- Config default: the implied default path is **`<data-dir>/spindle.toml`** (data dir: §3.1.1), with `<data-dir>/walgit.toml` accepted as an alias name (checked second; §8). `SPINDLE_CONFIG` (legacy `WALGIT_CONFIG`) overrides the default path when `--config` is absent; explicit `--config` beats the env var. A missing file is NOT fatal at the default location — it means zero-config first run (§2.3). A missing explicitly-named file (`--config` or the env pointer) IS fatal: exit 2 with `config file not found: <path>`. An explicit path of `/dev/null` forces defaults+env (§3.1).
- **No subcommand = `serve`** (with the parsed config; identical to `spindle serve`).
- Exit codes (normative; copy of Rust §16):
  - `0` — success;
  - `1` — command/config error (bad flag value, validation failure surfaced by a command, strategy not found, …);
  - `2` — a missing **explicitly-named** config file (`--config`/`SPINDLE_CONFIG`/`WALGIT_CONFIG` — never the absent default file, which is the zero-config first run), and argv-level errors (unknown command, malformed `--config`/`--data-dir` use);
  - `3` — `config check --strict` observed ignored overrides (supervisor pre-start check). For the
    API client commands (§6.6) `3` also means "not found" (HTTP 404).
  - `4` — an API client command failed authentication/authorization (HTTP 401/403, or no credential
    where one is required); see §6.6.
  - Mapping: Go's `flag` package calls `os.Exit(2)` on flag errors by default — the dispatcher overrides `flag.Usage`/error handling per subcommand so every argv error funnels to exit 2 uniformly (see §6.3).
- Version: from build info, in order: linker-injected `SPINDLE_BUILD_SHA` (set via `-ldflags "-X main.buildSHA=$(git rev-parse --short=12 HEAD)"` at build time, from `runtime/debug.ReadBuildInfo()` VCS info when present (Go embeds `vcs.revision` with `-buildvcs=true`, default when building from a git checkout; take first 12 hex chars), else `dev`. `spindle version` prints it; `serve` logs it at startup.
- Legacy compat: `walgit-server` is not shipped; `spindle serve` is the only server entrypoint. (If operators' units reference `walgit-server`, a `cmd/walgit-server` symlink-compatible binary MAY be added later; it is out of scope here.)

### 6.2 Subcommand table

Dispatch is a hand-rolled table (`cmd/spindle/main.go`), not a CLI framework:

```go
type command struct {
    name  string                        // e.g. "bundle", or "" for the serve default
    help  string
    run   func(ctx context.Context, args []string) error   // ctx cancelled on SIGTERM/SIGINT
}
var subcommands = []command{ /* flat table; "bundle run" is a two-token lookup */ }
```

Implementation notes per group (behavioral truth from Rust spec §16; each command's heavy lifting lives in the doc named in the last column):

| Command | Go implementation notes | See |
|---|---|---|
| `serve` | open store per §2 table, construct AppState, spawn maintainer + follow loops honoring `server.roles`, install SIGTERM/SIGINT handler that triggers the two-phase drain (`server.drain_timeout`), run the HTTP server. Legacy shape: a roles list of exactly `["maintain"]` or exactly `["compact"]`-equivalent runs its 60 s loop and no HTTP listener? NO — Rust runs the HTTP server in all cases; spindle MUST also always run HTTP (it serves /readyz for the supervisor). | 06_server_http.md |
| `compact [REPO\|--all] [--once] [--base]` | `--base` requires `--once`; drives 10_maintenance.md's compaction unit (or the base-rebuild path: full repack -adb + bitmap + commit-graph + history pack, checkpoint at that seq, then `bundle compose`). Exit 0 even when nothing to do. | 10_maintenance.md |
| `bundle run [--repo ID] [--strategy N]` / `bundle plan <repo>` / `bundle compose <repo> [--strategy N]` / `bundle rm <repo> <IDS…>` | thin wrappers over internal/bundle's plan/compose/delete; `run` without `--repo` iterates all repos the placement rules assign to this host. Output: one line per slot decision. | 08_bundles.md |
| `repo create <REPO> [--object-format sha1\|sha256]` / `repo list` / `repo info <REPO>` | create is CAS-based (create-if-absent on the manifest key); `repo info` prints manifest stats, pack inventory, checkpoint, segments as a table. | 05_wal_engine.md |
| `repo policy get\|set --file F\|clear <REPO>` | read/save/clear `policy.json` (14.1 envelope; the file is stored as bytes, validated by internal/policy on set). | 14.1 of MASTER_RUST_SPEC.md / internal/policy |
| `repo settings show <REPO> [--effective]` / `set --file F [-m MSG]` / `clear` / `history` | D24 surface. `show` prints the stored payload; `--effective` prints the `with_settings`-merged host+repo config restricted to the allowed sections. Author = `$USER`. `set` validates via §4.2 before publish. | §4 this doc |
| `wal ls <REPO> [--from N] [--to N]` | log table: seq, kind, pack (12 hex), supersedes count, ref count. Streams entries from the store; no full-log read into memory. | 05_wal_engine.md |
| `wal show <REPO> <SEQ>` | one entry, full: writer, created_at, pack, supersedes, ref updates, push options, checkpoint, meta. | 05_wal_engine.md |
| `wal materialize <REPO> --at-seq N --out DIR` | build a standalone repo at a point in time (checkpoint + replay + pack set fetched from the store or copied from the local cache copy; refs applied last). When history is folded, the error names the oldest rewindable state. | 05_wal_engine.md |
| `wal add-pack <REPO> <pack> [--history-of CHECKSUM] [--tier N]` | publish an existing pack as a COMPACT entry (recovery path: `--tier 0`). | 05_wal_engine.md |
| `wal annotate-pack <REPO> <CHECKSUM> [--rev F] [--bitmap F] [--commit-graph F]` | retrofit side files via manifest-only CAS (no log entry). | 05_wal_engine.md |
| `wal rev-index <IDX> [--out P]` | write a `.rev` from an `.idx` alone, byte-identical to git's output (04_git.md owns the algorithm). | 04_git.md |
| `synth --out DIR --size s\|m\|l [--commits N] [--files N] [--seed N]` | deterministic synthetic repo via `git fast-import`; sizes S=50c/200f, M=2000c/5000f+binary, L=50000c/50000f; output fsck-verified before exit 0. | 15_testing.md |
| `import --from GITDIR owner/name [--reuse-packs] [--refs GLOB]…` | classic import: publish refs + packs (reuse source packs or `pack-objects --all`), then immediately a full bitmap'd repack published as the tier-2 base. | 05_wal_engine.md, 04_git.md |
| `import --url URL owner/name [--ref REF]… [--default-branch-only] [--include-pull-heads] [--include-notes] [--token-env VAR] [--format sha1\|sha256] [--dangerous]` | URL import (docs/features/10): same Service as the server (same publish/CAS path) — clone `--mirror` into task scratch, refmap filter, tier-0 packs + tier-2 base, manifest `PutCreate` commit point, importer-admin access, `import.json` provenance. Token from the named env var (never argv). Re-run on a landed source = adopt/no-op. Exit 0 ok, 1 error, 2 argv. | docs/features/10_git_import.md, 04_git.md |
| `import --direct --from GITDIR owner/name [--packs DIR] [--refs GLOB]… [--bundle=true] [--bundle-strategy S] [--replace] [--force] [--commit-graph=true] [--history-pack=true] [--parallelism 8] [--verify-closure=true]` | bucket-direct import of ready-made packs: verify closure → side files → history pack → striped uploads (HEAD-skip existing, marker-file resumability, `--force` after a moved target) → ref snapshot + checkpoint → manifest CAS (`min_seq = seq+1`; `first_state_at = as_of = now`) → bundle list (supersedes same-strategy + dependents). Re-run on a completed import = no-op. Striped upload parallelism = `--parallelism` goroutines (see 03_store_backends.md §striped). | 05_wal_engine.md, 03_store_backends.md |
| `mirror --from URL --to URL --dir PATH [--ref NAME]… [--interval 30s] [--once] [--force] [--repack-every 1h] [--identity token\|gcloud\|gce]` | external bridge via a local bare buffer repo: fetch from source, ff-only push to destination; bearer token from `$WALGIT_TOKEN` / gcloud / GCE metadata (cached 50 min); `--once` exits non-zero on push failure; geometric repack of the buffer on `--repack-every`. The interval loop and any child git processes take the command's ctx. | 10_maintenance.md |
| `config check [--env-file PATH]… [--strict]` / `config dump` | validate file ⊕ env, print ignored overrides to stderr; `--strict` exits 3 when any override was ignored (supervisor pre-start check). With NO config file present, `config check` validates the effective defaults (§2.3) ⊕ env — it passes (exit 0) unless an env override breaks a rule; this makes it usable as a pre-flight check on a not-yet-configured host. `config dump` prints the effective config as TOML (defaults ⊕ first-run overrides ⊕ file ⊕ env, post-PORT-lockstep, post-placement-all-or-nothing), reporting `file_state` (present / absent / invalid) so scripts can tell the three boot states apart. `--env-file` loads KEY=VALUE pairs into the override set (does not touch the real environment). | §6.3 |
| `doctor [--limit N] [--repos N] [--budget N] [--json]` | **read-only upgrade readiness check** (§6.7): the config preflight through the shipped validators plus a bounded, sampling bucket read (one LIST per family) for corrupt/undecodable objects, missing/unknown schema versions, missing required fields, size outliers and the four highest-risk orphan pointers. Exit 0 clean, 1 findings, 2 usage/IO. Never writes, never repairs; `LIST` is allowed here because an explicit diagnostic is not a request path. Run against the **deployed** config with the **new** binary before recreating a task (`docs/UPGRADE.md`). | §6.7 |

There are NO `fsck`/`repair` subcommands — those are maintainer units (10_maintenance.md); manual recovery is `spindle wal add-pack --tier 0` + ref-delete pushes.

The collaboration surface (`auth`, `user`, `org`, `issue`, `pr`, `release`, `checks`, `packages`,
`threads`, `api`, `completion`) is an **HTTP client of the JSON API** and is specified in §6.6.

### 6.3 Flag parsing and dispatcher mechanics

- Per-subcommand `flag.NewFlagSet` (stdlib), one instance per invocation. The dispatcher:
  1. peel `--config PATH`, `--data-dir PATH`, `--server URL` and `-R OWNER/REPO` from
     anywhere in argv before dispatch (last wins);
  2. a `-h`/`--help` **before** the first command token prints the global command table and
     exits 0. **After** a command token it belongs to that command: the subcommand's FlagSet
     prints its own usage line and every flag it defines (§6.6). `spindle issue list --help`
     is therefore the per-command help; `spindle help` / `spindle -h` remain the table.
  3. look up the subcommand (first token; `bundle`/`wal`/`repo`/`import`/`config` consume a second token from a nested table);
  4. run the subcommand's `run(ctx, remainingArgs)`.
- Errors: a FlagSet parse error or an unknown subcommand prints usage to stderr and exits 2; a runtime error from `run` prints `spindle: <err>` and exits 1. A requested `-h`/`--help` (`flag.ErrHelp`) prints usage and exits **0**, never 2. `ctx` is cancelled on SIGTERM/SIGINT — every subcommand's `run` MUST honor it (interval loops, `git` subprocesses, store retries).
- No hidden flags, no config-in-flags tricks: flags never set config values except `--config` and `--data-dir` themselves (`--data-dir` only relocates the file leg and the first-run root defaults, §3.1.1; the env/PORT layering in §3 is the only other writer). This keeps `config dump` honest.
- Output formats: table commands (`wal ls`, `repo list`, `repo info`, `bundle plan`) print human tables to stdout; every command accepts `--json` (added for spindle; not in the Rust CLI) emitting one JSON object per line (JSONL), keys stable and snake_case, for scripting. When a Rust command had a documented exact output shape (e.g. `wal ls` columns), the human table keeps those columns in that order.

### 6.4 Concurrency

- `serve`: see 13_concurrency.md. Config provides only the numbers.
- `mirror`: the `--interval` loop is one goroutine; each fetch/push is a child process; the goroutine exits when ctx is cancelled. The buffer repo directory is owned by this process alone (the Rust spec gives it the same single-owner rule).
- `import --direct`: striped uploads run `--parallelism` goroutines (default 8) over a bounded channel of upload items; the importing goroutine closes the channel; workers exit on channel close or ctx cancel. Marker files make re-runs idempotent, so a ctx cancel mid-import is safe to re-run (05_wal_engine.md owns the marker scheme).
- Everything else is single-goroutine + ctx.

### 6.5 Examples

```console
# one-box: serve with defaults + env overrides
$ WALGIT__STORE__BUCKET=acme-spindle PORT=9000 spindle --config /etc/spindle/walgit.toml serve

# pre-start supervisor check (fail the boot when the file+env combo is wrong)
$ spindle config check --config /etc/spindle/walgit.toml --env-file /etc/spindle/overrides.env --strict || exit 3

# import a big repo, direct mode
$ spindle import --direct --from /srv/git/monorepo.git acme/monorepo \
    --bundle=true --bundle-strategy weekly --parallelism 12

# bundle ops
$ spindle bundle plan acme/monorepo
$ spindle bundle compose acme/monorepo --strategy weekly
$ spindle bundle run --strategy daily

# recovery
$ spindle wal add-pack acme/monorepo /tmp/pack-ab12.pack --tier 0

# synth a test repo
$ spindle synth --out /tmp/big --size l --seed 7
```
```console
# zero-config first run: no file anywhere, boots RESTRICTED on §2.3 defaults
$ spindle serve
# → server.listen = "0.0.0.0:8080", store.backend = "filesystem" (<data-dir>/store),
#   authentication unconfigured (§5 rule 1) ⇒ only /admin*, their POSTs and health answer,
#   server.auto_create_on_push = true. Configure OIDC on /admin, save, restart.
```

### 6.6 API client commands (WP11)

The collaboration surface is a client of the shipped JSON API (`docs/go/07_api.md` and the
feature-package route tables) — **no direct store access and no second source of truth**. The
operator commands in §6.2 (serve/compact/bundle/WAL/import/mirror) stay store-side; everything
below is HTTP.

```text
auth      login|logout|status|token list|create|revoke
user      whoami|list|show|keys list|keys add|keys remove
org       list|show|create · member list|add|remove · team list|create|add-repo
          · webhook list|create|delete|ping|deliveries|redeliver
issue     list|show|create|comment|close|reopen
pr        list|show|diff|create|close|merge|review|comment|checks
release   list|show|create|delete|upload
checks    list|status REPO [REF]
packages  list|show|delete
threads   list|show|run|cancel|rerun
runner    serve [--capacity N] [--labels a,b] [--register TOKEN] · token OWNER/REPO [--labels a,b] [--name N] · list OWNER/REPO · revoke OWNER/REPO ID
api       [-X METHOD] [-d DATA] [-H 'Key: Value'] PATH
completion bash|zsh|fish
```

Every command accepts `--json`; list commands accept `--limit`, `--after` where the API paginates;
mutating commands accept `--quiet`. Flags may appear before or after positionals (gh-style
permutation) — the CLI reorders argv before `flag.Parse`. Every subcommand answers `-h`/`--help`
with **its own** usage line and flag list (a leading help token still prints the global table,
§6.3); the command table, help and shell completion are generated from one table
(`cmd/spindle/cli_api.go`), so a flag cannot exist in one and not the others.

**Repo selector.** Commands that take an `OWNER/REPO` positional accept `-R OWNER/REPO` (peeled as
a global) or `SPINDLE_REPO` as the default, exactly like `gh -R`/`GH_REPO`: `spindle -R acme/mono
issue list`, `SPINDLE_REPO=acme/mono spindle pr show 42`. The bare `--repo` spelling is not the
CLI's (it is already `bundle run --repo ID`), so the long form is `-R`.

**Flag table (command-specific flags; each command's `--help` prints the exact set it defines —
the help and shell completion read the same table).**

Every API command also accepts `--json`; list commands add `--limit` and, where the API paginates,
`--after`; mutating commands add `--quiet`.

| command | command-specific flags |
|---|---|
| `auth login` | `--token-file F`, `--stdin`, `--no-verify` |
| `auth logout` | `--all` |
| `auth token …` | none (documented gap, see below) |
| `issue list` | `--state --label… --assignee --milestone` |
| `issue show` | `--comments N` |
| `issue create` | `--title --body --body-file --label… --assignee… --milestone` |
| `issue comment` / `pr comment` | `--body --body-file` |
| `issue close` | `--reason completed\|not_planned` |
| `pr list` | `--state open\|closed\|all --base --head` |
| `pr show` | `--comments N` |
| `pr diff` | none (text/plain unified diff on stdout) |
| `pr create` | `--title --base --head --body --body-file --draft` |
| `pr merge` | `--method merge\|squash\|rebase --delete-head --title --message --wait` |
| `pr review` | `--approve\|--request-changes\|--comment --body --body-file --commit` |
| `release create` | `--name --notes --notes-file --target --draft --prerelease` |
| `release upload` | `--name` |
| `packages delete` | `--tag T` (or a `DIGEST` positional) |
| `threads run` | `--ref REF` (required) |
| `threads cancel` / `threads rerun` | none (the verb is the URL; the run is a positional) |
| `runner serve` | `--capacity N --labels a,b --name N --register TOKEN --token-file F --docker BIN --image IMG --allow-host-docker --fetch-timeout S` |

**Server and token (`--server`, `SPINDLE_SERVER`, `SPINDLE_TOKEN`).**

1. Base URL: `--server URL` → `SPINDLE_SERVER` → the server written by the last `spindle auth login`
   → `http://127.0.0.1:8080`. A bare host gains `http://`; only http/https is accepted; trailing
   slashes are trimmed (a reverse-proxy path prefix is preserved).
2. Token: `SPINDLE_TOKEN` → legacy `WALGIT_TOKEN` → the state file entry for the resolved server.
   There is no `--token` flag: a token on argv is visible in `ps` and shell history.
3. State file: `${SPINDLE_CONFIG_DIR:-${XDG_CONFIG_HOME:-~/.config}/spindle}/auth.json`, mode
   **0600**, written atomically (temp + rename). Shape:
   `{version:1, current:"<server>", servers:{"<server>":{token,principal,write,admin,updated_at}}}`.
   `spindle auth login` verifies the token against `GET /api/v1/me` before storing it
   (`--no-verify` skips the call); `--token-file F` or `--stdin` supplies the token. Tokens are
   **never printed** — not by `auth status`, not in error bodies, and not in a `spindle api` response
   that echoes them (the client substitutes `[REDACTED]` for the credential in every message and
   body it writes).

**Output contract.**

- `--json` prints the endpoint's JSON body, pretty-printed (2-space indent) and otherwise
  **unchanged**: no invented envelope, no field renaming. Where the API returns an array, the CLI
  prints a JSON array. This keeps the CLI schema stable by construction — the API doc is the schema.
- Human output is a tab-aligned table (list commands) or a key/value block (show commands). The
  human text is explicitly NOT a stable contract; scripts use `--json`.
- `--quiet` suppresses confirmations on mutations; it never suppresses data.
- Commands that write print the created object's number and a browser URL on the human path.

**Exit codes** (superset of §6.1; the same numeric values, no renumbering):

| code | meaning |
|---|---|
| 0 | success |
| 1 | user/API error (400/409/422/5xx, validation, transport failure) |
| 2 | usage error (unknown subcommand, missing/unknown flag, missing required argument) |
| 3 | not found (HTTP 404) — shared with `config check --strict`; the commands are distinct |
| 4 | authentication/authorization failed (HTTP 401/403, or no credential when one is required) |

The 401/403 path prints one hint naming `spindle auth login` / `SPINDLE_TOKEN`; the message never
includes the token.

**Deliberate gaps (documented, not invented).**

- `auth token list|create|revoke` is **not available**: the JSON API exposes no token surface.
  Tokens are minted and revoked in a signed-in browser at `<server>/_auth/tokens`
  (`06_server_http.md` §8.6). The commands exist and exit 1 with that message.
- `packages` (WP1, `14_packages.md`) is **served** (`docs/features/14` §7): `list`/`show`/`delete`
  hit `GET|DELETE /{o}/{r}/api/packages…`, a 404 is the endpoint's own not-found (exit 3), and
  `packages delete` takes exactly one target — `--tag T` or a `DIGEST` positional (both, or
  neither, is exit 2). The human table carries PLATFORMS and UPDATED columns.
- `threads` (WP10, `15_threads.md`) and `runner` (its pass-B1 protocol surface, `15_threads.md`
  §6.2) are SERVED: `threads list|show` read the run records, `threads run` dispatches
  (`POST …/api/threads` with `{ref, workflow}`), `threads cancel` posts
  `…/api/threads/{run}/cancel` and `threads rerun` posts `…/api/threads/{run}/rerun` (both repo
  write, the same service calls the run page's buttons make, both answering `{"run": Run}` — 200
  for cancel, 201 for rerun). `runner serve` is the only runner verb: administration is
  browser-only at `/admin/runners` because the CLI's static machine credentials cannot administer
  (AGENTS law 9), so there are no `runner token|list|revoke` commands. A 404 from the threads paths
  is a real not-found (exit 3).
  `threads cancel` reports what the response can prove: a returned run state of `cancelled` means
  every job took effect now; any other state means a running job still waits for its runner to
  acknowledge `cancel_requested` (the body carries the run header, not per-job flags).
  `threads rerun` prints the new attempt number; the run id is unchanged (rerun advances the same
  run's attempt, it does not allocate a new run).

**`spindle runner serve` is not an API command** (pass B2, `15_threads.md` §4.1): it is the Threads
runner process — a long-lived protocol client that talks to the runner endpoints with its own
credential, never the user's API token. It does not read `spindle auth` state and it has no
`spindle.toml` section (a runner owns no data dir and no store), so its settings are flags ⊕
environment and are documented in `15_threads.md` §4.1; the token comes from
`SPINDLE_RUNNER_TOKEN`/`--token-file` or a one-time `--register wrg_…`. `--server` and
`SPINDLE_SERVER` resolve exactly as for the API commands (`--server`, then the env). It exits 0 on
SIGTERM and 1 on a rejected credential (re-register); a docker error fails the job, not the runner.
- `org team add-repo` targets the documented team→repository binding
  (`PUT /api/v1/orgs/{org}/teams/{slug}/repos/{owner}/{repo}`); same 404 treatment until it lands.
- `user list` calls `GET /api/v1/owners` (no users-only listing endpoint exists), so it lists
  owners — users and organizations alike.
- `pr merge` starts the documented `pull-merge` task and prints the task id; `--wait` polls
  `GET …/pulls/{num}/merge/task` until the task leaves `running` (no invented synchronous merge).
- `pr diff` streams `GET …/pulls/{num}/diff` (text/plain) verbatim to stdout; `pr close` is the
  documented pull PATCH with `{state: "closed"}`; `pr list --state all` omits the filter (the API's
  default) instead of sending a value the server rejects.
- `release create` omits `name` when `--name` is unset: the server defaults the title to the tag
  on create and leaves an existing release's title untouched on update (gh's behaviour without
  `--name`). `--target REF` seeds a tag that does not exist yet and therefore requires `--draft`
  — the tag is created on publish (`07_releases_stars.md`, WP3.3), never before.
- `issue create` sends labels, assignees and milestone **in the create body** (one write): the
  server validates every name before it allocates a number, so a bad `--label` fails with exit 1
  and leaves **no issue behind** (no create-then-PATCH).

**`spindle api`** sends one request, with the resolved bearer token when one exists (anonymous
otherwise): `-X METHOD` (default GET, POST when `-d` is present), `-d DATA` (literal, `@FILE`, or
`-` for stdin), `-H 'Key: Value'` (repeatable), `--json` to pretty-print the response. `PATH` may be
a full URL. Non-2xx maps to the exit codes above and writes the (redacted) error body to stderr.

**Completion** (`spindle completion bash|zsh|fish`) is generated from the same tables dispatch
reads — top-level commands, two-token operator subcommands, group subcommands, and each command's
flags. There is no hand-maintained second list.

Examples:

```console
$ export SPINDLE_SERVER=https://git.example.com
$ spindle auth login --token-file ~/.config/spindle/token
Logged in to https://git.example.com as alice (state: ~/.config/spindle/spindle/auth.json)

$ spindle issue list acme/mono --state open --label bug --json | jq '.issues[].num'
$ spindle issue create acme/mono --title "Crash on clone" --body-file /tmp/report.md --label bug
$ spindle pr diff acme/mono 42 | git apply
$ spindle pr review acme/mono 42 --approve --body "LGTM"
$ spindle pr merge acme/mono 42 --method squash --wait
$ spindle release create acme/mono v1.3.0-rc1 --draft --target refs/heads/main
$ spindle release upload acme/mono v1.2.0 dist/app-linux-amd64
$ spindle threads run acme/mono ci.yml --ref refs/heads/main
$ spindle threads cancel acme/mono 1795176002000000000-abcdef
$ spindle threads rerun acme/mono 1795176002000000000-abcdef
$ spindle -R acme/mono issue show 1
$ spindle api /api/v1/me
$ spindle api -X POST /api/v1/orgs -d '{"org":"acme","display_name":"Acme"}'
$ spindle completion bash > /etc/bash_completion.d/spindle
```

### 6.7 `spindle doctor` — the read-only upgrade preflight

`doctor` answers one question: **would this build serve this bucket?** It was added after the
2026-11-22 restricted boot, where a `server.auth.mode` key the old binary tolerated was refused
by the new one and nothing checked the deployed config before the task was recreated. Its
operating procedure is [`docs/UPGRADE.md`](../UPGRADE.md).

```console
$ spindle doctor [--config PATH] [--data-dir PATH] \
    [--limit N] [--repos N] [--budget N] [--json]
```

- **Config check.** The command runs the ordinary §3.1 ladder through `config.Load` (so every
  §5 rule and the strict loader's unknown/removed-key errors apply) and reports each
  `config.Validate` error/warning with its key path. When the strict load cannot decode a file at
  all, it reuses `config.LoadSetupOnly` — the restricted-boot loader — for the best-effort config
  and its already-keyed errors, then keeps checking the bucket. Ignored env overrides that name an
  unknown key (the `--strict` detection) are reported too: a typo'd `SPINDLE__STORE__BKUET` means
  the setting the operator believes they deployed never reached the instance. It never
  re-implements a rule and never writes the file back.
- **Bucket sample.** Eight families are sampled with one bounded `LIST` each (or one exact `GET`
  for singleton documents): `manifest` (`manifest.pb`), `refs` (`checkpoints/*/refs.pb`),
  `access` (`access.json`), `identity` (`users/`, `owners/`, `orgs/`), `collab`
  (`issues/`, `pulls/`), `packages` (registry manifests and tags), `threads` (runs, jobs,
  queue, artifacts under `threads/`) and `moderation` (indices, records, audit). Each sampled
  object is decoded with the family's codec — `internal/store/proto` for protobuf, a
  field-tolerant JSON decode for the JSON families (additive fields are the compat rule, so
  unknown fields are not findings) — and reported as `bucket.decode`, `bucket.version`
  (missing/unknown schema version), `bucket.required` (missing/invalid required field) or
  `bucket.size` (a generous per-family byte bound).
- **Bounded, and honest about it.** `--limit N` (default 200) caps decoded objects per family and
  orphan rows per check; `--repos N` (default 200) caps sampled repositories; `--budget N`
  (default 2000) caps total decoded objects. Each family's report line says
  `family <name>: sampled M of N objects`, and a count-cap hit is printed as `N+ (cap reached)`.
  The command never claims an exhaustive scan.
- **Orphans — the four highest-risk pointers only** (not a general referential graph):
  Threads artifacts whose run record (`threads/runs/<run>.json`) is gone; global queue index rows
  (`threads/queue/index.json`) naming a missing per-repo job; moderation report index pointers
  (global and per-repo) with no immutable record; fork child/parent pointers that disagree in
  either direction. Plus one repository check: a `repos/<o>/<r>/` prefix with WAL history
  (`checkpoints/` or `log/`) but no `manifest.pb` — a repository that cannot open. A stale
  prefix with only sidecars, or a fork-provisioned unborn prefix, is expected state and is NOT
  reported.
- **Read-only.** The only store operations are `Get`, `Head`, `List` and `ListPrefixes`; the
  command holds no locks across calls and is safe against a live instance. It does not create the
  store root or cache dir: a missing filesystem root is exit 2. `LIST` is permitted here by
  decision because an explicit operator diagnostic is not a request path; nothing else may call
  `LIST` on a hot path.
- **Output.** One line per finding: `check`, the full key path, a short reason. Then the per-family
  sampling lines, the repository sampling line and a summary count. `--json` emits one stable
  document: `{version, findings:[{check,key,reason}], summary:{findings,clean,limit,budget,
  decoded,budget_exhausted,repos_sampled,repos_total,repos_truncated}, families:[{family,sampled,
  total,truncated,findings}]}`.
- **Exit codes:** `0` clean, `1` at least one finding, `2` usage/IO failure (bad flag, a missing
  explicitly-named config file, an unreadable store). `2` means the check did not complete and must
  not be read as clean. A finding (including a config warning) is `1` because this is a readiness
  gate.

## 7. Worked end-to-end example

```console
# 0. zero-config first run (no --config, no <data-dir>/spindle.toml): §2.3 defaults + env
$ spindle config dump
# → listen = "0.0.0.0:8080", store = filesystem @ ~/.local/share/spindle/store,
#   cache.dir = <data-dir>/cache, authentication unconfigured (restricted boot), auto_create_on_push = true
# → file_state = "absent"

# 1. defaults + file + env
[server]
listen = "127.0.0.1:8080"
public_url = "http://127.0.0.1:8080"
[store.s3]
endpoint = "http://127.0.0.1:9000"
force_path_style = true

$ WALGIT__STORE__BUCKET=spindle-dev PORT=9999 spindle config dump --config /etc/spindle/walgit.toml
# → server.listen = "127.0.0.1:9999"       (PORT lockstep)
# → server.public_url = "http://127.0.0.1:9999"  (loopback URL, same lockstep)
# → store.bucket = "spindle-dev"            (env override)
# → everything else = defaults

# 2. same config against a real public URL: PORT does NOT touch it
$ WALGIT__SERVER__PUBLIC_URL=https://git.example.com PORT=9999 spindle config dump --config /etc/spindle/walgit.toml
# → server.listen = "127.0.0.1:9999"
# → server.public_url = "https://git.example.com"   (non-loopback: untouched)

# 3. placement all-or-nothing
$ WALGIT__PLACEMENT__SERVE='["acme/*"]' spindle config dump --config /etc/spindle/walgit.toml
# → placement.serve = ["acme/*"], placement.maintain = ["*"] (file's placement table discarded, defaults re-applied)

# 4. typo'd env key: boots anyway, but visible
$ WALGIT__STORE__BKUET=x spindle config check --config /etc/spindle/walgit.toml
# warning: ignored override WALGIT__STORE__BKUET (unknown key store.bkuet)
$ WALGIT__STORE__BKUET=x spindle config check --config /etc/spindle/walgit.toml --strict
# exit 3
```

## 8. Decisions & deviations from the Rust design

- **NEW (2026-11-24) — static sites: one host section, one per-repo section (docs/features/19).**
  `[static_sites]` (`enabled`/`address`/`host`/`max_file_bytes`/`index_files`) configures the
  dedicated listener and is host-only; `[static_site]` (`enabled`/`ref`/`root`) is the fifth
  settings passenger and says only what one repository publishes. The split is deliberate:
  publication is per-repository data that rides the WAL document (revisioned, admin-written,
  visible in history), while the listener is a host-wide socket no repository may configure. For
  the same reason `[static_site]` never merges — there is no host default to inherit — and an
  unreadable settings document resolves to "not published" rather than failing open. `enabled`
  defaults `false` host-side, so an upgrade never opens a new origin nobody asked for, and the
  rest of the section is validated only when it is on. `index_files` defaults to
  `["index.html","index.htm"]` and `max_file_bytes` to `8MiB` (`docs/features/19` §5).
- **Env prefix: `SPINDLE__` is primary, `WALGIT__` accepted as a legacy alias** (supersedes the earlier "stays `WALGIT__`" decision): new deployments and docs use `SPINDLE__`; scripts, systemd units, and secret manifests written for walgit keep working via the alias, and `SPINDLE_` wins when both define the same key (§3.2). Compat preserved, naming future-proofed.
- **Config file name/location (superseded in part):** the earlier decision kept `walgit.toml` as the implied default file name. The divergence moves the default location under the data dir — `<data-dir>/spindle.toml` is the default path, `<data-dir>/walgit.toml` is accepted as an alias (checked second, §3.1, §6.1) — so the README's "`walgit.toml` remains a valid config file name" promise still holds for the alias, while first-run and setup-UI files are written as `spindle.toml`.
- **`--json` flag added to table-output commands** (JSONL, stable snake_case keys): Rust's human tables are unscriptable; JSONL costs ~20 lines and no dependency.
- **`gix` as `git.upload_pack_engine` value accepted but treated as `git`**: the Rust gix engine has no Go equivalent and none is wanted (dependency law); file compatibility is preserved.
- **`walgit-server` binary alias not shipped**: `spindle serve` is the entrypoint; units referencing `walgit-server` are a packaging concern (16_packaging.md MAY add an alias later).
- **Version env var is `SPINDLE_BUILD_SHA`** (Rust used `WALGIT_BUILD_SHA`): it is build-time, not deployment-facing, so no compat value; VCS build info from the Go toolchain is the primary source anyway.
- **NEW (2026-11-20, WP11) — the collaboration CLI is an HTTP client, not a second writer.** `auth/user/org/issue/pr/release/checks` (and `api`) call the shipped JSON API; no command in that surface opens the store, and no route shape is duplicated in Go. One `apiClient` helper owns path building, the bearer header, error mapping, and token redaction. Rationale: the API is the single contract, and a CLI that wrote the bucket directly would fork every gate (roles, visibility, policy) it cannot see.
- **NEW (WP11) — `--json` prints the API body unchanged (re-indented).** No CLI-specific envelope is invented: the API doc *is* the JSON schema, so the CLI cannot drift from it. Human tables are deliberately not a stable contract.
- **NEW (WP11) — exit 3 is shared by `config check --strict` (ignored overrides) and API "not found".** Renumbering the established exit code would break supervisor scripts; the two commands are disjoint, and the collision is documented in the help text and §6.1.
- **NEW (WP11) — the login state lives at `${SPINDLE_CONFIG_DIR:-${XDG_CONFIG_HOME:-~/.config}/spindle}/auth.json` (0600, atomic write).** `SPINDLE_CONFIG_DIR` is the CLI-only override (tests and containers need it); it never feeds the server's config ladder (§3.1.1's `SPINDLE_DATA_DIR` is the server's knob). Tokens are never accepted on argv and never printed; `apiClient` redacts the token from every error it renders.
- **NEW (WP11) — documented-but-unserved surfaces report "feature not enabled" (exit 1).** `org team add-repo` is bound to their documented paths now; a 404 from it means "the route is not registered", not "your object is missing". This keeps the 404→exit-3 contract honest for routes that *are* served — `threads` and `packages` left this list when their routes landed (see the 2026-11-21 entry).
- **NEW (2026-11-21) — CLI audit fixes: per-command help, served threads, one-write issue create.**
  (a) `-h`/`--help` after a command token is the command's own usage + flags (`flag.ErrHelp` →
  exit 0); only a help token **before** the first command prints the global table (§6.3), because
  the old blanket peel made every subcommand's flags undiscoverable. (b) `threads` routes are
  served, so `threads run` dispatches `POST …/api/threads {ref, workflow}` and `threads show`'s
  404 is a real exit 3. (The gap this entry recorded — `threads cancel` printing a T2-runner
  message instead of calling a route — is closed by the 2026-11-21 cancel/rerun entry below.)
  (c) `issue create` carries labels/assignees/milestone in the create
  body — the server validates them before allocating a number — so a bad label can no longer
  leave an unlabeled issue behind (the audit's non-atomic create). (d) `-R/--repo` is spelled
  `-R` (the `--repo` long form is already `bundle run --repo ID`) with `SPINDLE_REPO` as the env
  default, matching `gh -R`/`GH_REPO`.
- **NEW (2026-11-21) — `threads cancel` and `threads rerun` are served verbs.** Both POST the
  route the run page's buttons use (`POST …/api/threads/{run}/cancel|rerun`, repo write,
  `docs/features/15_threads.md` §3), and each prints only what the returned body proves: cancel
  names the returned run state (a `cancelled` state means every job took effect now; any other
  state means a live job waits for its runner's ack — the body carries the run header, not the
  per-job `cancel_requested` flags), rerun prints the new attempt on the **same** run id and
  refuses with the API's 409 message while a job is running. The previous "T2 runner protocol;
  not yet available" message outlived the route by a pass; §6.6's exit table is unchanged
  (404 → 3, 401/403 → 4, 409/5xx → 1, usage → 2).
- **NEW (WP11) — `auth token list|create|revoke` is a documented gap.** Minting is a browser-session surface (`/_auth/tokens`, CSRF-guarded), not the public JSON API; the commands exit 1 with a pointer instead of the CLI inventing a token endpoint.
- **NEW (WP11) — shell completion is generated from the dispatch tables.** `completionWords()` walks `subcommands` plus the `apiGroups` table; `spindle completion bash|zsh|fish` differs only in the shell template. No hand-maintained command list can go stale.
- **`RUST_LOG` honored (kept) as an override of `telemetry.log_filter`**, and `SPINDLE_LOG` accepted as the new-style spelling: zero-cost compat for existing log-tuning scripts.
- **Unknown-key env overrides are soft (ignored + reported) rather than fatal**: matches Rust behavior and keeps a stale variable from taking down a fleet; `--strict` makes them fatal for supervisors.
- **NEW (2026-09-05) — `server.tls.*` removed; TLS terminates at the reverse proxy** (Forgejo #165): the `server.tls.mode/cert/key/hostnames` keys are gone (validation rules renumbered: roles 8, paths 9, ssh 10). A file that still sets them is rejected at load and a `SPINDLE__SERVER__TLS__*` env override is fatal — both with a reverse-proxy pointer. The env fatality is a deliberate exception to the "unknown-key env overrides are soft" rule above: a silently ignored TLS setting would leave the operator believing the server terminates TLS. Absolute-URL building honors `X-Forwarded-Proto` (§9.1 of 06_server_http.md); set `server.public_url` to the canonical external origin behind a proxy.
- **NEW (2026-09-08) — CLI `import` repack tail records bare-hex checksums (#205):** the tier-2
  base checksum derives via `git.PackChecksumFromIdx` (not `TrimSuffix`-only) and the serving
  path is built as `pack-<bare>.pack`, so the base lands under `wal/<bare>.pack` like the
  tier-0 trailer-derived packs. Same-file `AddPack` installs are a no-op (05_wal_engine.md).
- **NEW (2026-09-09) — top-level `description` key in per-repo settings (#235):** a single-line ≤512-char display string riding the WAL-published settings TOML (persistence/revision/authorship/admin-writes free); never merged into the host config, never in `settings/effective`. Rationale: 14 §14.12 — additive key, existing keys never change meaning; the 16 KiB payload budget is unchanged and still covers it.
- **NEW (2026-09-15) — `[review]` section in per-repo settings (Forgejo #586):**
  `allow_self_approval` (default ON — nil section/key resolves allowed, zero migration)
  gates author `APPROVE`/`CHANGES_REQUESTED` on their own PR (`COMMENTED` always allowed;
  OFF restores the historical `422`). Same passenger discipline as `description`/`[features]`:
  validated + persisted + revisioned + admin-written with the doc, never merged, never in
  `settings/effective`. (This amendment also records the previously undocumented `[features]`
  section, Forgejo #522, in §4.1/§4.2 — same passenger discipline; the code already accepted
  it.) Rationale: 14 §14.12 — new sections are additive; existing keys never change meaning;
  the 16 KiB payload budget is unchanged and still covers both sections.

### Divergence (2026-08-31)

- **D5 — Config is optional; zero-config first run.** The Rust spec boots fail-closed with no config file ("run `walgit init` first"); spindle boots on the first-run defaults of §2.3 instead: `listen = "0.0.0.0:8080"`, `store.backend = "filesystem"` rooted at `<data-dir>/store`, authentication unconfigured (the boot is restricted — Administration area + health only — until OIDC is configured), `auto_create_on_push = true`, everything else per the Rust defaults. User friendliness is a first-class law. A missing file at the DEFAULT location is a first run, not an error; a missing explicitly-named file (`--config`/config-env pointer) remains fatal (exit 2), and `/dev/null` still forces defaults+env (§3.1). Data dir (`--data-dir` / `SPINDLE_DATA_DIR`, default `~/.local/share/spindle`, `/var/lib/spindle` in containers) owns `store/`, `cache/`, and the saved `spindle.toml` (§3.1.1).
- **NEW (2026-11-22) — `[rate_limit]` is a reflective, restart-required section, `enabled` defaults
  off (PLAN-V2 §M.4).** Six keys (`enabled`, four `N/s|N/min|N/h` rates, `trust_proxy`) with no
  hand-written schema row: `BuildSchema` walks the struct, `Coerce` accepts the spellings as
  strings, `Validate` parses them and names the key, the env overlay is
  `SPINDLE__RATE_LIMIT__*`, and `/admin/config` renders the card with per-key notes. **Restart is
  the honest answer** rather than a hot reload: the buckets are built at boot from the effective
  config (the process has no config-reload path at all), so every changed key appears in
  `RestartKeys` — a limiter that silently applied a saved-but-unsaved-to-memory rate would be a
  config lie. `enabled = false` is the zero-config default (L10) and the plan's open question (c)
  answered: an instance must not start refusing writes because it was upgraded.
- **NEW (2026-11-21) — setup save backs up the first canonical rewrite (round-3 audit).**
  `SaveSetup` still writes the full canonical document (Toml keeps comments only on decode), but
  before the first rewrite of an existing file it copies the previous bytes to
  `spindle.toml.bak-<utc>` beside the config — once, never per save — and a byte-identical save
  writes nothing. Preserving comments would need a trivia-preserving TOML writer (a new
  dependency or a hand-rolled parser), rejected under law 1; a backup keeps the operator's text
  without inventing a format. Rationale: a zero-edit save silently deleting a hand-written comment
  is data loss, and the alternative (save only changed keys) does not restore comments either.
- **D6 — Administration area + API save semantics (§3.7).** The Administration area writes `<data-dir>/spindle.toml` atomically (tmp + fsync + rename, mode `0600`) after full §5 validation; the save response reports restart-required vs read-live keys; the saved file joins the ordinary ladder so env ⊕ file still holds with env winning. Boot states: config present → validate → boot; absent → defaults + setup banner; INVALID → restricted mode (only `/admin` + `/admin/config`, `/healthz`, `/readyz` answer; rest 503) until a fixed config is saved and the process restarted. Endpoint shapes and the setup access rule live in 06_server_http.md.
- **Auth-none loopback rule superseded.** Rust validation rule 1 (auth `none` requires a loopback listen, fail-closed) is replaced by a loud warning on any bind (§5 rule 1); the first-run default `0.0.0.0:8080` + `auth = "none"` depends on it. All other validation stays fail-closed.
- **D4 — `filesystem` added to `store.backend`** (`s3|memory|filesystem`, §2 note † and §5 rule 5): keys map to paths under `store.root`, first-run default `<data-dir>/store`. 03_store_backends.md owns the backend semantics; this doc owns the key, its validation, and the default.
- **Feature 10 amendment (2026-09-04, docs/features/10):** additive `[import]` section (`clone_timeout`, `git_timeout`, `max_bytes`, `max_refs`, `max_concurrent`, `allow_private_networks`, `url_allowlist`, `allow_file_urls`) + the `import --url` CLI row in the §6.2 table (§6.2, same change, law 12). Rationale: 14 §14.12 — new sections/keys are additive; existing keys never change meaning.
- **D1 — dependency budget** (restated where it touches this doc): exactly `github.com/go-chi/chi/v5`, `github.com/BurntSushi/toml`, `golang.org/x/net`. TOML stays the config file format and BurntSushi/toml stays the parser — no format migration was entertained.
- **NEW (2026-09-11) — `server.serve_sync_timeout` / `server.serve_materialize_timeout` (#320):**
  the serve pack phase finally has deadlines (wait 45s / body cap 10m; non-positive falls back to
  the defaults — there is no unbounded serve wait to configure by accident). Rationale: without
  them one stalled materialize wedged every object-level request past the proxy (504) while refs
  stayed instant; the wait must sit under typical proxy 60s timeouts and the body cap comfortably
  above the wait (per-request patience vs per-body-of-work patience are different bounds).
- **NEW (2026-09-11) — oidc validation required the browser-login trio (#344):** §5 rule 2
  refused `auth.mode = "oidc"` when any of `server.auth.session_secret` / `oauth_client_id` /
  `oauth_client_secret` was empty, naming the missing keys. **Superseded by PLAN-OIDC-ONLY
  (2026-11-21):** the mode is gone and §5 rule 1 is the always-on `checkAuthConfigured` set
  (issuer/allowlist/groups claim/admin groups/trio); an incomplete set is a restricted boot, not a
  dead serving boot.
- **NEW (2026-09-15) — `--data-dir` sync lives in the shared CLI path (Forgejo #611):**  `resolveConfig` (`cmd/spindle/config.go`, via `syncDataDirFlag`) re-points the flag-derived
  paths (`DataDir`, `Store.Root`, `Cache.Dir`) to the effective `--data-dir` whenever they
  currently equal the `ResolveDataDir(env)`-derived paths — the exact comparison `serve` always
  performed. Previously only `serve` did this fixup, so every other store-touching subcommand
  (`repo policy set/get`, `config dump`, `wal`, `compact`, `bundle`, `import`, `repo create`,
  …) operated on the default store when `--data-dir` differed from the default. Explicit file
  values and `SPINDLE__*` env overlay never equal the env-derived paths and are preserved by
  construction (no re-derivation of `FirstRunDefaults` — that would discard the overlay, per
  the §3.1.1 field lesson). Serve's own fixup is kept as an idempotent no-op second pass.
  `openEngine` needs no change: `openStore` already prefers `cfg.Store.Root` and falls back to
  `<dataDir>/store` with the same flag dir, and the WAL registry/cache consumers read the
  already-synced `cfg.Cache.Dir`. Pinned by `TestDataDirFlagSyncMatrix` (flag vs env vs file
  vs defaults) + `TestServeFixupIdempotent` in `cmd/spindle/datadir_sync_test.go`.
- **NEW (2026-09-12) — oidc allows `anonymous_read = true` (Forgejo #371):** §5 rule 2 no
  longer refuses `anonymous_read` in oidc mode — the old blanket prohibition predates #345
  (visibility as the read authority for repo surfaces) and made public repos unbrowsable on
  OIDC instances. `true` is now the default recommendation (visitors browse public repos
  without logging in; the navbar offers Login); `false` stays as the everything-requires-login
  hard-lock for non-repo surfaces. The `/admin/config` copy and its field metadata
  (`internal/webui/settings.go`) describe the trade-off instead of "must be false". Interaction
  with #344: the browser-login trio requirement is unchanged — anonymous browsing never
  removes the need for a working login flow.
- **NEW (2026-11-21) — the setup form is a LOSSLESS editor of the schema (G1/G3/G4, WP9).**
  `internal/setup` joins every value the schema advertises to a spelling `Coerce` accepts
  (`setup.DisplayValue`), and `setup.FormValues` renders the whole schema into the map
  `POST /admin/config` hands to `ParseValues`. Struct-slice rows live in `internal/setup/rows.go`
  (form namespace `K#<i>.<tomlname>`, `__base`, `__remove`; `RenderRows` /
  `ParseRowEdits` / `FormValues`), so the page, the API and the acceptance test share one
  renderer and one parser. Rationale: the audit measured 21 keys that could not round-trip
  (two struct slices rendering empty, two unsigned counters blank, 17 empty lists coercing
  to `[""]`), i.e. the advertised one-click path silently corrupted the file; the fix is
  structural (one renderer, one parser, one whole-schema gate) rather than 21 special
  cases. Consequence: `fmtAny` masks secret fields inside a struct slice, so
  `GET /api/v1/setup` no longer publishes a static token's value either (the row value is
  a toml-name map with `SecretMask` in place of the secret). A blank numeric/size/duration
  row cell keeps the row's stored value (type `0` to clear it); blank string cells clear.
- **NEW (2026-11-21, WP6.2) — `spindle org webhook …`.** Six HTTP-client commands over the
  owner-gated org webhook API: `list` (table: id/url/events/active/secret-set), `create`
  (`--url`, `--events`, `--secret`, `--inactive`, `--insecure-tls`), `delete`, `ping`
  (`{delivery}`; a failed POST exits 1 and points at `deliveries`), `deliveries` (seq, event,
  status, duration, at, error) and `redeliver ORG ID SEQ` (`{delivery, entry}`; a 410
  payload-not-retained is exit 1 with the server's reason, a 404 is exit 3 — the standard
  mapping). Every command accepts `--json`; the JSON is the API body unchanged.
- **NEW (2026-11-21) — `store.backend` defaults to `filesystem`, and `s3` must be configured
  explicitly (OBS-1).** The compiled-in table used to default `store.backend = "s3"` with an
  implicit AWS endpoint, so a config file that simply omitted the key pointed a deployment at
  AWS (the audit found one); the first-run defaults hid this by overriding the backend only in
  the no-file case. Both the file leg and `--config /dev/null` now default to `filesystem`, and
  `store.s3.endpoint` is empty by default: validation rejects `store.backend = "s3"` without an
  endpoint, bucket, or credential env-var names, naming each missing key (fail closed instead of
  half-working against an unintended host). The env overlay still wins over the file for the
  backend and every S3 key. `store.root`'s data-dir default is unchanged and now applies to the
  compiled default too.
- **NEW (2026-11-21) — the cache budget is reconciled with the cache filesystem (BUG-6).**
  `cache.max_bytes`'s default (20 GiB) assumed a volume that large; on a tmpfs/small data dir the
  budget can never fit, and a push died mid-ingest with ENOSPC while the client saw only
  `unpacker error`. At boot (`serve` and the CLI engine paths, after the loader has recorded which
  keys the file/env actually set) the filesystem holding `cache.dir` is statfs'd: if it is smaller
  than the budget and `cache.max_bytes` is still the **compiled default**, the budget is lowered to
  the eviction watermark (`cache.disk_high_watermark`, default 0.9) of that filesystem, with a WARN
  naming the path, filesystem type and both values. A **user-configured** `cache.max_bytes` is never
  changed — it is reported as a warning naming `cache.dir`/`cache.max_bytes` (fail loud, honor the
  operator). Independently of the budget, ENOSPC/EDQUOT during ingest now produce a diagnosis that
  names the failing path, the filesystem and its capacity (04_git.md §3.4), and
  `store.root`'s filesystem-full error names the root (03_store_backends.md §4).
- **NEW (2026-11-22) — `spindle doctor` is a first-class subcommand, and LIST inside it is by
  decision.** A deployed instance can be unreadable to a new binary for reasons no unit test sees
  (the 2026-11-22 restricted boot: a removed `server.auth.mode` key the old binary tolerated).
  The preflight has to exercise the **new** binary's validator against the **deployed** file and
  decode real bucket objects, so it needs `LIST` — which stays forbidden on every request path.
  The decision is scoped: `internal/doctor` is the only caller, every family walk is bounded
  (`--limit`, `--repos`, `--budget`, and a per-family count cap) and the output states the sample
  rather than implying an exhaustive scan. The command writes nothing, repairs nothing and holds
  no locks across store calls, so it is safe against a live instance; config errors are reported
  through the existing `config.Validate`/`config.LoadSetupOnly` surfaces instead of a second copy
  of the rules. Exit 2 (usage/IO) is distinct from exit 1 (findings) so a preflight script can
  tell "the check ran and found problems" from "the check did not run".
- **NEW (2026-11-23) — static machine tokens have a lifecycle.** `[[server.auth.tokens]]`
  gains `name` (optional, unique, the stable rotation/usage label) and `expires_at`
  (optional RFC 3339). Validation (`checkStaticTokens`) refuses an unparseable expiry and a
  duplicate name, naming the row key; a past instant is accepted and means "expired now", because
  a config written before a deadline must keep validating after it. The row editor needs no new
  code: both are ordinary element fields, so they render as cells and coerce through the shared
  `setup.Coerce` path, and a zero-edit save stays byte-identical. On authentication an expired row
  is an ordinary `ErrInvalid` (401 + the normal Basic/Bearer challenge), never a 403 and never an
  in-band message, so git erases it. Rationale: a permanent, unobservable credential is a permanent
  leak; the fields add a bounded lifetime and an audit trail at the config layer where the
  credential already lives, without adding a token-minting API or local state.
