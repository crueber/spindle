# OPERATIONS — first run, identity, administration, credentials

> Status: operator procedure (2026-11-24). Install, ports, the reverse proxy and
> configuration syntax are [`DEPLOY.md`](DEPLOY.md); backup is
> [`BACKUP.md`](BACKUP.md); upgrading a running instance is
> [`UPGRADE.md`](UPGRADE.md).

spindle has exactly one way in: an OpenID Connect provider. There are no local
accounts, no passwords, no anonymous-admin mode and no setup token. This file
covers the restricted first boot, connecting the IdP, the Administration area,
the credentials machines use, and day-two recovery.

## 1. The restricted first boot

A fresh instance has no identity provider, so the boot is **restricted**: only the
Administration area (`/admin`, `/admin/config`, their POSTs — including the
`/admin/config/auth-test` discovery probe), the health endpoints
(`/healthz`, `/readyz`) and the setup API (`/api/v1/setup*`) answer; everything
else is 503. There is no open or anonymous-admin mode.

| Boot | When | What answers |
|---|---|---|
| normal | the effective config passes validation (a complete OIDC set) | everything |
| restricted, `file_state: absent` | no config file at all, and the environment does not complete the OIDC set (the zero-config first run) | `/admin` (the OIDC onboarding card), `/admin/config`, the setup API, `/healthz`, `/readyz`; everything else 503 |
| restricted, `file_state: invalid` | a config file that does not parse, or fails validation | the same set; `/admin/config` pre-fills from the file's best-effort values and names every finding |

In the restricted boot the object store is not opened, there is no git lane, no
JSON API beyond the setup endpoints, no maintainer loop, and **the SSH listener
does not start**.

**Readiness is 200, deliberately.**

| Endpoint | Restricted boot | Normal boot |
|---|---|---|
| `/healthz` | 200 `{"status":"ok","version":…}` — liveness | same |
| `/readyz` | 200 `{"status":"restricted","version":…,"surface":"admin"}` | 200 ready (+ `prewarm_pending`, `instance`, `placement`) |

A restricted instance reports **`/readyz` 200** (`status: restricted`) because
that is the surface it serves — a health-gated ingress must route it, never gate
it out. A phase-2 drain answers 503 in both boots.

## 2. Connect an identity provider (OIDC)

Authentication is **OIDC, always**. There is no `auth.mode` key to switch:
"auth is configured" means the set below is valid, and the boot is normal exactly
then. An operator who supplies the whole set through `SPINDLE__SERVER__AUTH__*`
(and no file) boots normally too.

### 2.1 The required set

| Key | Default | Requirement |
|---|---|---|
| `server.auth.issuer` | — | required; an absolute `http(s)` URL (discovery at `<issuer>/.well-known/openid-configuration`) |
| `server.auth.oauth_client_id` / `oauth_client_secret` | — | required, as a pair |
| `server.auth.session_secret` | — | required, **≥ 32 bytes**; signs every session and `wgt_` token |
| `server.auth.allowed_domains` / `server.auth.allowed_emails` | — | at least one non-empty: the admission allowlist (without one, nobody may sign in) |
| `server.auth.groups_claim` | `groups` | non-empty; the ID-token claim carrying group membership |
| `server.auth.admin_groups` | `["spindle-admin"]` | **non-empty**; the only source of administration (an empty list is a guaranteed lockout, so validation refuses it) |
| `server.auth.session_ttl` | `30d` | browser session lifetime |
| `server.auth.access_token_ttl` | `90d` | `wgt_` token lifetime |
| `server.auth.anonymous_read` | `true` | public clone and browse of public repositories — not an admin path |
| `server.auth.write_domains` | empty (= every admitted domain may write) | write scoping; never grants administration |

`server.auth.admin_emails` / `server.auth.admin_domains` and a static-token
`admin` field **no longer exist**: there are no email/domain admin rules and no
static-token admins. A file that still carries one fails validation with a named
error, and the instance boots restricted until it is removed.

### 2.2 At the identity provider

1. Register a client whose **redirect URI** is `<public-url>/_auth/callback`
   (exact match, no trailing slash — the first-run card computes the exact value
   and offers a copy button).
2. Create the group your `admin_groups` names (`spindle-admin` by default) and put
   your administrators in it. Membership is what grants administration here; no
   email list can substitute.
3. Make sure the ID token carries the claim `groups_claim` names — a JSON array of
   strings or a single string.

No identity provider handy? The dev IdP in
[`DEVELOPMENT.md`](DEVELOPMENT.md) (`go run ./internal/devtools/oidpstub`, or the
`dev-idp` compose profile) prints a `[server.auth]` block to paste at `/admin`.

### 2.3 The first-run card

`/admin` in the absent-file state renders the OIDC onboarding card: issuer,
allowed email domain, client id, client secret, session secret and admin group,
plus the redirect URI with a copy button.

- The **session secret** field shows a server-generated suggestion (copyable) and
  the alternative `openssl rand -hex 32`. It is overwrite-only; a stored value is
  masked and an empty field keeps it.
- **Test** is the live validation: `POST /admin/config/auth-test` fetches the
  issuer's discovery document, checks the issuer matches and the endpoints are
  present, then fetches `jwks_uri` and requires at least one usable `RS256`/`ES256`
  signing key.
- **Save** validates the shape and writes the config file this process **loaded** —
  `<data-dir>/spindle.toml` on a default boot, or the explicit
  `--config`/`SPINDLE_CONFIG`/`WALGIT_CONFIG` file when one was named (and
  `<data-dir>/walgit.toml` if that alias is the loaded file). The page names the
  resolved path above the TOML disclosure and the saved banner repeats it, so
  there is never a question of which file changed. It **never contacts the IdP**:
  recovery must not depend on the thing being recovered. Use Test first.

### 2.4 Save, restart, normal boot

A save never restarts the process, and it writes the file this process loaded
(the page names that absolute path). After a successful save the page reports
**Restart required** with the keys that changed (§3.7); the live keys — including
the import SSRF policy — have already taken effect, so you restart only for the
keys it lists:

- compose: **Administration → Maintenance → Restart**, or `docker compose restart`;
- systemd or another supervisor: restart the unit/task;
- restricted boot: the Maintenance page is not mounted there, but the Configuration
  page's **Restart required** card is — the first OIDC save lands on it, and its
  typed-confirmation form. The platform supervisor is the fallback when no page can
  render ([`UPGRADE.md`](UPGRADE.md) §5).

After the restart the boot is normal: git, the API and the pages serve, `Sign in`
appears in the header, and `/_auth/login` → IdP → `/_auth/callback` (PKCE + nonce)
mints a session cookie carrying the verified groups. If the group intersects
`admin_groups`, the Administration link appears and `/admin` answers; otherwise
`/admin` answers the designed denied view (401 signed out, 403 non-admin).

The first save that rewrites an existing config keeps a `<file>.bak-<utc>` copy
beside it (`spindle.toml.bak-<utc>` for the default file; beside the explicit
`--config` file when one was loaded) — a save-time undo buffer, not a backup
(it lives on the same disk). An unchanged save writes nothing at all.

### 2.5 Group claim semantics and staleness

- Groups are read from the **signature-verified ID-token payload only**, never
  from a request header.
- A JSON **array of strings** yields each non-empty trimmed element; a **single
  string** yields one entry; absent/`null`/number/object/empty yields none. There
  is **no nested traversal** — a provider that emits a tree (Keycloak's default
  group shape) yields nothing, so flatten it at the IdP with a claim mapper.
- The intersection with `admin_groups` is **case-insensitive** and trimmed
  (`Spindle-Admin` matches `spindle-admin`).
- Groups never grant **write** (`write_domains` decides that) and never bypass the
  `allowed_*` admission check.
- Administration is recomputed per request from the verified groups the session
  carries and the configured `admin_groups` — it is never frozen into the session,
  so a change to `admin_groups` (after the restart it needs) applies to existing
  sessions.
- **IdP-side group removal is not re-fetched.** There are no refresh tokens and no
  userinfo backchannel: a demoted user keeps admin until their session expires
  (`session_ttl`, default 30 days) or they sign in again. The immediate global
  lever is rotating `server.auth.session_secret`, which revokes every session and
  every `wgt_` token at once.

### 2.6 Verifying the group actually arrives

- `GET /_auth/me` reports `groups`, `admin` and `groups_claim` for the signed-in
  principal — one `curl` with the session cookie is a machine-diagnosable check.
- **Administration → Access** renders the same facts as *Your access*: principal,
  email, the verified groups this session carries, the configured admin groups,
  the session's issued-at and the verdict, plus (when not admin) both lists and
  how to fix it. Both read the same verified payload, so the page cannot disagree
  with the API.

### 2.7 Recovery when nobody can sign in as an admin

Administration belongs to a signed-in user whose verified group claim intersects
`server.auth.admin_groups`. If that is unreachable (wrong redirect URI at the IdP,
a claim that is not emitted, a typo'd group), the instance is *normal* and nobody
can administer it through the browser. The recovery path is the file:

1. Edit `<data-dir>/spindle.toml` on the host (or set `SPINDLE__SERVER__AUTH__*`)
   and restart. There is **no unauthenticated back door** — the denied page and
   the sign-in failure page both say exactly this.
2. `spindle config check` validates a hand-edited file offline before the restart.
3. An *invalid* file — including one whose OIDC set is incomplete — puts the
   instance back in restricted mode, where `/admin` is open again. That is the
   recovery loop by design, and it grants nothing but the config editor.

A *valid* config whose IdP delivers the wrong group is the diagnosable case: fix
the claim name (`groups_claim`), the group value (`admin_groups`), or the
membership at the IdP, then sign out and in again (§2.6 shows both lists).

## 3. The Administration area

`/admin` is reachable only in the restricted boot, or by an admin in a normal
boot; every page gates through the same access rule as `/api/v1/setup*`.

| Page | Path | What it is |
|---|---|---|
| Overview | `/admin` | the state at a glance, health, the first-run checklist |
| Configuration | `/admin/config` | every setting; the one page that writes |
| Access | `/admin/access` | who can sign in, who administers (read view + your live session facts) |
| Directory | `/admin/directory` | every account and organization, with deactivate/reactivate |
| Maintenance | `/admin/maintenance` | is the machinery healthy and what is it doing: health, packages GC, Restart |
| Reports | `/admin/reports` | the cross-repository abuse queue |
| Audit | `/admin/audit` | every administrative action recorded, newest page first |
| Runners | `/admin/runners` | the instance CI runners: mint a registration token, watch the fleet, revoke one |

Only **Overview** and **Configuration** are mounted in the restricted boot (the
nav hides the rest, which would 503).

### 3.1 Configuration

The Configuration page holds **every setting**. The essentials stay open and the
rest live in collapsed **Advanced** groups; every row is addressable as
`/admin/config#row-<key>`, which is how other pages and the restart notices link
to the exact field. It writes the **whole canonical document** atomically
(temp file, fsync, rename, mode 0600) and runs the same fail-closed validation
`serve` enforces.

### 3.2 Access

Read-only: the OIDC sign-in facts (issuer, claim, admin groups, the redirect URI
to register), *Your access* (the live diagnostic of §2.6), the admission rules
with their consequence ("without an admission rule nobody may sign in", and they
never grant administration), and the static-token table (name/principal, scope,
expiry, last use — never a secret). Every edit affordance links to its row on
Configuration.

### 3.3 Directory

Every account and organization on the instance. **Deactivate** refuses the
account with a neutral 403 and revokes credentials minted before the
deactivation; **reactivate** does not restore them — the user signs in again.
Accounts resolve from the store (`users/<name>/user.json`), never from a local
database.

### 3.4 Maintenance

Health (the same readiness facts `/readyz` reports), the **packages GC** control
(a dry run, then a confirmed apply; the durable summary lives in the object store
at `packages/gc_state.json`), and **Restart**. Per-repository operations stay on
the repository pages.

### 3.5 Runners

Instance-scoped CI runners: a host administrator mints a one-time registration
token in the browser here, together with the runner's **repository policy**
(every repository, or an allowlist of `owner/name` / `owner/*` patterns). The
runner is offered only the repositories that policy allows and can never change
it — changing it is revoke + re-mint. The image policy
(`threads.image_allow_prefixes`, `threads.image_require_digest`) lives in the
`threads` card on Configuration.

There are no runner admin commands in the CLI, so the registration token is minted
in the browser; `runner serve --register` is the operator step after that mint. Runner operation, labels, the container runner and the
security boundary are [`threads.md`](threads.md) §8; the feature record is
[`FEATURES.md`](FEATURES.md) §3.3, and the design record is
[`features/15_threads.md`](features/15_threads.md).

### 3.6 Reports and Audit

**Reports** is the host moderation queue: resolve or dismiss what repository
admins have not. **Audit** is the administrative record — configuration saves,
access grants, ownership transfers, packages GC, moderation and restarts — newest
page first, filterable by action, actor, repository and time window.

### 3.7 Restart required

A save **never restarts the process**. After it, the page reports **Restart
required** with the exact keys that changed, each linking to its row (the server
list is authoritative). Almost every key is consumed once at startup; the live
keys take effect immediately and therefore never appear in that list:

`telemetry.log_format`, `telemetry.log_filter`, `maintenance.interval`,
`maintenance.follow_interval`, `wal.freshness_ttl`, and the import/mirror egress
policy — `import.allow_private_networks`, `import.url_allowlist`,
`import.allow_file_urls`.

The import policy is one policy read at each import: saving it on
Configuration applies to the Import page, the import API and the mirror /
push-mirror / fork-sync creation paths **on the next request**. If the Import page
still refuses a source as private after a save, check *which* file the save wrote
(the page names it) and whether a `SPINDLE__IMPORT__*` environment override is
masking it — an env-supplied value wins over the file, at save time as at boot.

**Maintenance → Restart** asks for a typed confirmation (the word `restart`), is
audited (`admin_restart`), and then exits the process so its supervisor starts it
again — a compose service with `restart: unless-stopped` comes back on its own,
while `restart: no` leaves it down until you start it. There is no in-process
reload. Use it after the first OIDC save, and whenever the list names a key whose
effect you need now.

## 4. Credentials

### 4.1 The one predicate

Administration is exactly **signed in ∧ verified group claim ∩ admin groups ≠ ∅**.
Every credential resolves through that one function: no email list, no domain
list and no static-token row flag can grant it.

### 4.2 `wgt_` personal access tokens

A signed-in user mints one at **`/_auth/tokens`** (`POST` returns
`{token, principal, write, expires_at}`). They are HMAC-signed, valid until
expiry (`server.auth.access_token_ttl`, default 90 days), and **cannot be listed
or revoked individually** — rotating `server.auth.session_secret` revokes every
session and every `wgt_` token at once. A token is user-bound: it carries the
minting session's verified groups, so it re-derives the same admin verdict, and
deactivation kills tokens minted before the revocation.

Use it as the password in git's Basic auth, as `Authorization: Bearer` for the
API/CLI, and as the registry password.

### 4.3 Static machine tokens

`[[server.auth.tokens]]` rows are *operator-authored* machine credentials:

```toml
[[server.auth.tokens]]
principal = "ci-bot"
token_env = "CI_BOT_TOKEN"   # or token = "…" — one of the two is required
write     = true             # write per row
name      = "ci-bot-2026"    # stable label for the last-used record
expires_at = "2027-01-01T00:00:00Z"  # RFC 3339; unset = never expires
```

- They authenticate git (HTTP Basic: username = the principal, password = the
  token), the JSON API and the CLI (`spindle auth login`, or `SPINDLE_TOKEN`).
- They are **never admin**: no row field can change that, and a row still carrying
  `admin = true` is a validation error.
- `server.auth.tokens` requires a **restart**; a row whose credential resolves to
  nothing is dropped at startup. No API route mints or revokes a static token.
- A row without `expires_at` is valid **forever** — a leaked credential stays
  usable until the row is removed and the instance restarts (the Access page
  warns with a count). Set an expiry and rotate on a schedule.
- Rotation without downtime is a **rolling deploy**: add the new row for the same
  principal, roll a second instance with it (both tokens work while both rows are
  configured), update clients, remove the old row, roll again. A single-instance
  in-place restart is a brief outage for its in-flight requests.

### 4.4 Which surface accepts what

| Surface | Credential |
|---|---|
| Browser (pages, Administration area, `/_auth/tokens` minting) | an OIDC session cookie (`/_auth/login`); admin pages need the admin group |
| Git over HTTP | HTTP Basic with the token as the password (the username is ignored; the docs use your name) — a `wgt_` token or a static machine token |
| Git over SSH | your public key, registered at `/keys` (or `GET\|POST\|DELETE /api/v1/ssh-keys`); rights resolve from the principal at auth time |
| JSON API and SDK | `Authorization: Bearer` with a `wgt_` token or a static machine token; the browser uses its session cookie |
| CLI (`spindle …`) | `spindle auth login` stores a token in `~/.config/spindle/auth.json` (mode 0600, never printed; dir overridable with `SPINDLE_CONFIG_DIR`), or `SPINDLE_TOKEN` |
| OCI registry (`docker login`) | user + `wgt_` token or static token (or the browser session); anonymous callers get the Docker token challenge, and public repositories allow anonymous pull while `anonymous_read` is on |

API client commands exit **4** on an authentication/authorization failure
(HTTP 401/403, or no credential where one is required); `--server URL` (or
`SPINDLE_SERVER`) selects the instance and `--json` prints the endpoint body
unchanged.

### 4.5 Your first push

Repositories auto-create on push by default (the zero-config shape sets
`server.auto_create_on_push = true`):

```sh
mkdir demo && cd demo && git init -b main
echo "# demo" > README.md && git add -A && git commit -m "hi"
git remote add origin http://localhost:8080/you/demo.git
git push -u origin main          # username: you; password: a wgt_ token from /_auth/tokens
```

Browse it at **http://localhost:8080/you/demo**. Over SSH, register the key on
`/keys` first and clone `ssh://git@localhost:2222/<owner>/<repo>.git`.

### 4.6 Administration happens in the browser

The CLI has no admin commands, so administrative control is performed in the
Administration area by a signed-in admin: the configuration save, access and
directory changes, the packages GC, and minting a runner registration token.
That is a surface choice, not a credential limit: a `wgt_` token minted by an
admin carries that user's verified groups and re-derives the same admin verdict,
so the admin API accepts it too, while `[[server.auth.tokens]]` static rows can
push and call the API but never administer.

## 5. Maintenance and recovery

### 5.1 The recovery loop

An invalid config never bricks an instance: it boots restricted, `/admin` names
the file and the failing keys, `/admin/config` writes a validated file atomically
and keeps `spindle.toml.bak-<utc>`, and a restart applies it. `/readyz` answers
200 `status: restricted` throughout, so a health-gated proxy keeps the recovery
page reachable. The step-by-step version (including the case where nobody can
sign in) is [`UPGRADE.md`](UPGRADE.md) §5 and §2.7 above.

### 5.2 Health, backup and restore

`/healthz` is liveness, `/readyz` is "this process serves the surface it owns"
(normal or restricted; 503 only while warming or in a phase-2 drain), and both
answer in every boot. For backup, restore and the drill, see
[`BACKUP.md`](BACKUP.md): the object store is the repository, the config and the
SSH host key are backed up beside it, and the cache is expendable.

### 5.3 Upgrade

Moving to a new build is a preflight, not a leap: run the **new** binary against
the **deployed** config (`config check --strict`, then `doctor`, which also reads
a bounded, read-only bucket sample), quiesce, start it, and verify the surfaces.
[`UPGRADE.md`](UPGRADE.md) has the procedure, the rollback pair and what a
restricted boot after an upgrade means.

### 5.4 CI

Threads runs CI from `.spindle/workflows/<name>.yml` in the repository; runners
are instance-scoped and registered from `/admin/runners` (§3.5). Workflow keys,
triggers, secrets and permissions, the runner's flags and `SPINDLE_RUNNER_*`
environment, retention, approval and troubleshooting are
[`threads.md`](threads.md); the design and as-built record is
[`features/15_threads.md`](features/15_threads.md).
