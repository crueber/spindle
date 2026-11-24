# PLAN-OIDC-ONLY — one auth: OIDC, administered by group

> Status: **plan, not implementation.** Written 2026-11-21 against `main`
> (`e21f98e`, worktree `plan/oidconly`). No code in this change; every file,
> key, route, test and role string below is named so a reviewer can check the
> plan against the tree.
>
> Ground truth read for this plan: `internal/config/config.go` (the `Auth`
> struct, `Defaults()`, `FirstRunDefaults`), `internal/config/validate.go`
> (`checkAuthNoneLoopback`, `checkOIDC`), `internal/config/firstrun.go`
> (`LoadSetupBase`, `SaveSetup`), `internal/config/setuponly.go`
> (`LoadSetupOnly`), `internal/server/auth.go` (`Authenticate`,
> `principalFromEmailGroups`, `PrincipalForName`, `MintSession/VerifyToken`),
> `internal/server/auth_oidc.go` (`/_auth` flow, `loginPageHTML`,
> `authTokenSignIn`), `internal/server/auth/principal.go` (`None()`,
> `Anonymous()`), `internal/server/setup.go` (`setupAccess`),
> `internal/server/router.go` (`mount`, `mountSetupOnly`, `authFlow`),
> `internal/server/middleware.go` (`authFailure`, `requireAuth`),
> `internal/server/health.go` (`readyz` warnings, clone-help `token_url`),
> `internal/setup/setup.go` (`schemaEnums`, `ProbeAuth`, `BuildSchema`),
> `internal/webui/{render,server,settings,forms,collab,public}.go`,
> `cmd/spindle/{serve,webui,config}.go`, `internal/api/discovery.go`
> (`authModeOf`), `internal/threads/tokens.go` (runner credential shapes),
> `internal/e2e/{e2e,collab,threads_triggers,threads_runner}_test.go`,
> `internal/server/static_test.go` (the in-process JWKS stub),
> `internal/mirror/http.go` + `internal/repoimport/service.go`
> (`dangerousAllowed`), `compose.yaml`, `compose.standalone.yml`, `README.md`,
> `DEVIATIONS.md` (D-UX-1/2/3), `docs/go/06_server_http.md` §3.4/§8,
> `docs/features/01_identity_permissions.md`, and **`docs/PLAN-ADMIN.md`**
> (this plan lands inside that area; see §1.6).

---

## 0. The decision this plan records

**Open mode is removed. OIDC is the only human authentication. Administration
belongs exclusively to a signed-in user whose verified group claim intersects
the configured admin group.**

This is a **deliberate reversal** of two standing decisions:

- **AGENTS law 10 / DEVIATIONS D-UX-1** — “zero-config first run boots on
  auth `none` with loud warnings, allowed on any bind”. Reversed: a missing or
  invalid config now boots **restricted** (the setup-only route set); the
  warnings-based compromise is deleted, not softened.
- **AGENTS law 9’s divergence note / DEVIATIONS D-UX-3** — “auth `none` no
  longer requires a loopback bind; loud warnings instead of refusal”.
  Reversed with it: there is no `none` to warn about.

**Rationale (the owner’s words, adopted as the recorded one):** *an instance
that is administrable by anyone who reaches it is not shippable.* The warning
regime asked an operator to notice a log line before an attacker noticed the
port; it made “temporary” the default state of every container pull. The
zero-config law survives in weakened, honest form: **zero-config still boots
first-run** — it just refuses to *serve* until a real identity provider is
configured, and the only thing it answers is the page that configures one.

Also recorded as superseded inside this change: the `admin_emails` /
`admin_domains` grant (WP13’s “migration path”) — see §1.4 — and the
`StaticToken.Admin` row flag — see §1.3. WP13’s group mechanism itself
(`admin_groups` × `groups_claim`, session-carried, re-derived per request)
is the **kept core** this plan hardens into the single admin predicate.

**Effort:** ≈ 7.5 dev-days across five milestones, each independently green
(§9). Sequencing relative to PLAN-ADMIN is in §1.6.

---

## 1. The auth model

### 1.1 `mode` is deleted; OIDC is what auth *is*

`server.auth.mode` **ceases to exist as a config key** (pre-1.0: no enums with
one value, no “still accepted for” spellings). “Is auth configured?” is
answered by the OIDC set itself:

```
auth configured ⇔ issuer ∧ oauth_client_id ∧ oauth_client_secret ∧
                  session_secret (≥32 bytes) ∧ (allowed_domains ∨ allowed_emails) ∧
                  groups_claim (non-empty) ∧ admin_groups (non-empty)
```

- `internal/config/config.go`: `Auth.Mode` field deleted; `Defaults()` stops
  setting it; `FirstRunDefaults` stops setting it.
- `internal/config/validate.go`: `checkAuthNoneLoopback` deleted;
  `checkOIDC` becomes `checkAuthConfigured` and becomes part of plain
  `Validate` — no mode guard, it simply always runs (a bare first-run shape
  fails it, which is exactly what puts a fresh instance in restricted mode).
  New rules, each failing closed with its named key (§4.2).
- The effective config (`file ⊕ env overlay`, or `defaults ⊕ env overlay` when
  no file exists) decides the boot: **normal iff validation passes,
  restricted otherwise.** An operator who configures OIDC purely through
  `SPINDLE__SERVER__AUTH__*` env vars with no file boots normal — the
  zero-config law's honest survivor.
- `internal/server/auth.go`: the `Authenticate` switch collapses to the
  today-`oidc` tree (static tokens → `ExtraCredential` hook → `wgt_` → ID
  token via JWKS → session cookie → `Anonymous()`). The `auth.None()`
  principal and `internal/server/auth/principal.go`’s `None()` are **deleted**
  (law: no dead shape); `Anonymous()` stays — it is the `anonymous_read`
  viewer, not an admin.
- `internal/server/auth/principal.go` comment contract, `docs/go/06_server_http.md`
  §8.1's “`none` ⇒ everyone is anon with write+admin” row, and every
  `authMode()` branch disappear (§5 sweeps the callers).

### 1.2 The single admin predicate

**Admin ⇔ signed-in ∧ (groups_claim ∩ admin_groups ≠ ∅).** Nothing else
grants it — no email list, no domain list, no static-token flag, no
forwarded header.

It is computed in **exactly one place**: `AuthService.principalFromEmailGroups`
(`internal/server/auth.go`), which already builds `Principal.Admin` from the
signature-verified groups carried by the credential (ID token, session wire,
or minted `wgt_` wire — all three already route through it, WP13). Every
consumer reads the resolved flag:

- API lanes: `requireAdmin(p)` / `p.Admin` (unchanged call sites).
- Pages: `pages.Auth.Authenticate` → `Chrome.Admin` (the seam already carries
  the same principal; PLAN-ADMIN's `ShowAdmin` reads it).
- `identity` resolution, `dangerousAllowed` predicates, packages push gate,
  `CheckCreateOwner`/`CheckPush` host-admin bypass: all read `p.Admin`.

After the change the email/domain arms in `principalFromEmailGroups` are
deleted, leaving the group arm as the only one; because validation requires
`admin_groups ≠ ∅` (§4.2), an empty-list guard debate disappears. The
predicate the page layer and API see cannot diverge because neither of them
*has* a second copy — the flag is derived at the credential boundary and
everything downstream is the same field. A regression test pins it:
`TestAdminPredicateSingleSource` (new, `internal/server/auth_admin_test.go`):
for each credential shape (ID token, session, `wgt_`, static token, `wct_` CI
token, `wjt_` job token, `wrt_` runner), assert `Admin` equals the group
intersection verdict and nothing else — table-driven over the shapes.

### 1.3 What happens to `token` mode and static tokens

**`token` mode is deleted as a mode. Static tokens survive — demoted to
machine credentials with no admin rights.**

- `server.auth.tokens` rows keep `principal`, `token`/`token_env`, `write`.
  The `admin` field is **removed from the struct** (`config.StaticToken`);
  a file carrying `admin = true` on a row fails validation with the named
  error `server.auth.tokens[].admin: static tokens can no longer be admins —
  administration is granted only by the OIDC admin group` (BurntSushi
  reports the undecoded key; `checkAuthConfigured` turns it into that
  message).
- Why keep them at all: they are the *operator-authored* machine credential
  (the file is operator territory the way a password file is; the operator who
  can write it can already change the admin group). They serve git-over-HTTP
  Basic, the CLI (`spindle auth login` stores any bearer the server
  accepts), headless automation, and the e2e lanes' non-admin writers. Their
  browser identity career is over: `POST /_auth/token` (paste-an-admin-token
  sign-in), `TokenSignInEnabled`, the `loginPageHTML` token branch, the
  restricted-page paste form and the `authTokenSignIn`/`authToken` session
  path are **all deleted** in the same change.
- Why not remove tokens entirely (rejected, Appendix B): machine write access
  would then depend on `wgt_` tokens minted by a *human* session —
  90-day expiry, no individual revocation, minting requires a browser. That
  is worse for CI and backups, and it buys no admin-safety (tokens were never
  the leak; the *mode* was).
- A static-token principal is **never** `Anonymous`, has `Write` per its row,
  and `Admin: false` always. `PrincipalForName` (`internal/server/auth.go`)
  loses its `none` and `token` branches; SSH key auth resolves usernames
  only through the wired `EmailLookup` → the OIDC admission path (fail closed
  on nil/miss, as today's oidc branch).

### 1.4 What happens to `admin_emails` / `admin_domains`

**Deleted.** No break-glass retention. The owner's direction is explicit —
admin only via the group — and the two honest readings of “break-glass” both
fail:

1. An email/domain admin list is *ambient* authority: anyone who can register
   `ops@$domain` forever holds host admin, and domain wildcarding
   (`admin_domains = ["*"]`-shaped mistakes) reopens the hole wider than the
   group check ever was.
2. A lockout is already recoverable through the file/env edit path (§2.4) —
   an operator who can reach the data dir does not need a second,
   network-reachable back door; one who *cannot* reach the data dir cannot
   be saved by an email list either.

`Auth.AdminEmails` / `Auth.AdminDomains` fields, their setup-schema rows, the
`settingsFieldMeta` notes, the `x_gaps7_test.go` / `roundtrip_test.go`
fixtures, the `authFailure` hint text (“see: spindle config (server.auth:
admin_groups, admin_emails)”) and the `06_server_http.md` §3.4 access-rule
sentence are all updated in the same sweep. **Migration note for operators
(README + DEVIATIONS):** create the IdP group, put your admins in it, set
`admin_groups` — *before* upgrading, or the upgrade lands you in restricted
mode with the instructions on screen (§7).

### 1.5 What stays (and the one default that moves)

| Key | Verdict |
|---|---|
| `server.auth.anonymous_read` | **Stays** (default `true`) — it is the public-browsing lever (`identity` §4.1), orthogonal to who may sign in. Not an admin path. |
| `server.auth.groups_claim` | **Stays**, default `"groups"` (§3.1 semantics unchanged) |
| `server.auth.admin_groups` | **Stays**; **default renamed `["spindle-admins"]` → `["spindle-admin"]`** — the owner's stated spelling. One-line change in `config.Defaults()` + docs. Pre-1.0: no alias; the renamed default rides the same sweep. **Flagged for owner confirm (§10).** |
| `allowed_domains` / `allowed_emails` / `write_domains` / `audiences` / `trusted_forwarders` | **Stays** unchanged (admission, write scoping, token audience, broker forwarding) |
| `session_ttl` / `access_token_ttl` / `session_secret` | **Stays**; the browser-login trio requirement becomes the boot requirement |
| `[[server.auth.tokens]]` | **Stays** as machine credentials; `admin` field deleted (§1.3) |

`identityForward` (§8.6 broker rule) keeps its shape: a forwarded *email*
re-derives admission and flags through `principalFromEmailGroups` — which now
means group-derived admin; a forwarded bare username keeps the caller's flags
(fail closed — forwarding never grants admin it cannot prove).

### 1.6 Compatibility with PLAN-ADMIN (this plan does not re-litigate it)

The Administration area (`/admin`, `/admin/config`, `/admin/access`,
`/admin/maintenance`; the area shell; the “one editor” rule; first-run living
on the Overview) is PLAN-ADMIN's structure and stands. This plan **amends four
spots** in it, to be applied when the later-landing plan is merged (or by the
PLAN-ADMIN implementer as a rider — both plans pin the same end state, so
order does not matter, only the second-landing side updates the other's
text):

1. **§1.2 nav rule** `ShowAdmin: openMode || p.Admin` becomes
   `ShowAdmin: restrictedBoot || p.Admin` — `openMode` (`mode == "" ||
   mode == "none"`) no longer exists; `restrictedBoot` is the boot state in
   which `/admin` is the only surface anyway. Signed-in admins keep the
   account-menu link; non-admins never see it (unchanged).
2. **§2.1 first-run checklist** — step 1 (“Choose how people sign in:
   `server.auth.mode != "none"`”) is *satisfied at boot* by construction and
   becomes step 0 done-state on the restricted Overview; step 3 (“Name the
   first administrators”) drops the `admin_emails`/`admin_domains`/token-admin
   disjunct: done iff `admin_groups` is set (which validation forces anyway —
   the card states what it is for, not that it is pending).
3. **§2.3 “Establishing the first administrator”** — the token-mode paragraph
   is deleted; the OIDC-group path is the whole section.
4. **§3.4 Access page** — the sign-in-method card's mode enum disappears;
   the card reports issuer, `groups_claim`, admin group, and (new) the
   session diagnostic of §3.2.

Sequencing: **this plan's M1 lands after PLAN-ADMIN's M1** (the `/setup` →
`/admin*` move) so URL strings are edited once. If the order flips, the
PLAN-ADMIN M1 sweep already re-points both plans' names; the interlock is
recorded here so neither implementer freelances.

---

## 2. First run: container start → first admin, five steps

### 2.1 The state machine (one restricted state)

Boot decision (`cmd/spindle/serve.go`):

| File state | Today | After |
|---|---|---|
| absent, env does not complete OIDC | `boot.Mode = "defaults"`: serves git/pages on auth-`none` + banner | **restricted**: `boot.Mode = "setup_only"`, `FileState = "absent"`, empty error list |
| absent, `SPINDLE__SERVER__AUTH__*` env completes a valid OIDC set | defaults-mode serving on auth-`none` (env ignored for mode) | **normal** — the effective config passes validation; the env-only operator never sees restricted mode |
| present, valid (OIDC complete) | normal | normal (unchanged) |
| present, invalid | restricted (`setup_only`) | restricted — now *also* the destination for every old `none`/`token` config, which fails the new rules with named keys |

Restricted mode is today's `mountSetupOnly` mechanics, unchanged in shape:
only `GET/POST /admin*`, `POST /admin/config/auth-test`, `/api/v1/setup*`,
`/_ui/*`, `/healthz`, `/readyz`, `/services/public/install.sh` answer;
everything else 503 with `walgit: configuration is invalid — open /admin to
fix it` (the absent-file variant says `…not configured — open /admin to
finish setup`). **No store opens, no git lane, no JSON API, no maintain
loops; the SSH listener is additionally gated OFF in restricted mode** (today
it starts but its registry is nil — every key lookup fails closed; refusing
to listen is the honest version of the same fail-closed).

`readyz` answers **200 `{"status":"restricted","version":…,"surface":"admin"}`**
in restricted mode, not 503: the instance is ready for the surface it serves,
and a health-gated ingress can route `/admin` — the alternative was the
2026-11-22 outage, where a restricted boot answered 503 and the proxy routed
nothing, so the recovery UI was unreachable. `/healthz` stays the liveness
answer (200, version only); `/` stays 503. Amendments are in
`docs/go/06_server_http.md` §10.2. The `"auth_none"` warning entry in *normal*
mode (`internal/server/health.go`) is deleted with the mode. The zero-config log
banner (`logSetupBanner`, `cmd/spindle/serve.go:559`) becomes
`first run: OIDC is not configured — serving only /admin until a config is
saved and the server restarted`.

### 2.2 The page: first-run OIDC onboarding

On `GET /admin` in restricted-`absent` state, the Overview renders PLAN-ADMIN's
first-run card, now a **five-field OIDC onboarding** (composed from the
existing `.setup-row`/`.setup-callout` idioms — no new design language):

1. `server.auth.issuer` (e.g. `https://id.example.com`)
2. `server.auth.oauth_client_id`
3. `server.auth.oauth_client_secret` (masked, overwrite-only)
4. `server.auth.session_secret` (masked, overwrite-only; the card shows a
   one-time server-generated 64-hex suggestion in a copyable code block plus
   the `openssl rand -hex 32` alternative — a suggestion to paste, not a
   write path)
5. `server.auth.admin_groups` (prefilled with the default `spindle-admin`)

Plus the live **Test** button — `POST /admin/config/auth-test` →
`setup.ProbeAuth` (already exists: discovery fetch, issuer-match,
endpoint-presence) **extended to fetch the discovered `jwks_uri` and require
at least one `RS256`/`ES256` signing key** (`ProbeAuth` gains a
`SigningKeys int` result field; ~25 lines, same client/timeout discipline).
The card marks “verified” after a passing probe. Save is the ordinary
whole-document `POST /admin/config` (byte-identical contract untouched); the
five rows are the auth-card rows it already knows how to edit. **Save itself
does not dial the IdP** — validation is shape-only: a recovery save must
never be hostage to IdP reachability. The Test button is the live validation
“before save”, and the card's copy says so.

`/admin/config` (the full 149-key editor) stays reachable in restricted mode
exactly as today's `/setup` is: it is the recovery surface for *other*
invalid keys too (a bad `store.backend`, a malformed `server.listen`), and it
writes only a config that passes validation, masks secrets, and is
same-origin-checked. The first-run card can therefore be its own small form:
`setup.ParseValues` applies **only the posted keys on top of the
file-visible base** (verified, `internal/setup/setup.go:425–479` — absent
keys keep the base; secrets are overwrite-only), so a five-key POST to the
ordinary `/admin/config` handler cannot wipe anything. One new handler test
pins that property for this path (`TestFirstRunCardPartialPost` — post the
five keys on an absent-file base, assert the untouched keys survive the
save). **Flagged for owner confirm (§10):** the stricter reading —
OIDC-card-only until the first save, full editor unlocked after — is one
boolean in the router; this plan recommends keeping the editor open
(recovery beats surface minimization here, and the instance serves nothing
else while it is open).

### 2.3 The five steps (operator's path, no token, no open window)

1. **Start the container.** No config ⇒ restricted mode; every URL except
   `/admin`, `/healthz`, `/readyz` answers 503 with the pointer.
2. **Open `/admin`.** The first-run card asks for the five OIDC fields.
   Meanwhile the operator creates the OIDC client at their IdP with redirect
   URI `{public_url}/_auth/callback` (the card computes and offers the
   exact redirect URI with its copy button — existing pattern) and puts
   their user in a group (e.g. `spindle-admin`).
3. **Test, then Save.** Discovery + JWKS probe passes; Save validates
   (`checkAuthConfigured`) and writes `<data-dir>/spindle.toml`; the
   saved-restart card lists `server.auth.*` keys and says restart.
4. **Restart the container.** The boot is now normal: git, API, pages serve;
   `/admin` requires an admin session; `Sign in` appears in the header
   (`ShowLogin` is always true now — `BrowserLoginEnabled` is guaranteed by
   validation).
5. **Sign in.** `/_auth/login` → IdP → `/_auth/callback` (PKCE + nonce,
   unchanged) → session cookie minted with the verified groups → the header
   shows the identity; if their group intersects `admin_groups`, the
   Administration link appears (`ShowAdmin`) and `/admin` answers.

### 2.4 Lockout recovery is the file edit — stated in the UI, never a hidden open mode

If Save succeeded but the IdP misconfigures login (wrong redirect URI, group
claim not emitted, secret typo at the IdP), the server is *normal* and no one
can administer it through the browser. The recovery path:

- **The denied page says it.** PLAN-ADMIN's shared denied view (§2.4 there)
  carries, for the OIDC case: *“No administrator can sign in? Recovery is
  the config file: edit `<data-dir>/spindle.toml` on the host (or set
  `SPINDLE__SERVER__AUTH__*` env vars) and restart. There is no
  unauthenticated back door.”* The sign-in failure page
  (`loginPageHTML`, kept single-branch) carries the same sentence.
- **`spindle config check`** prints the OIDC rules with named keys
  (§4.4), so an operator can validate a hand-edited file before restart.
- An *invalid* file (including one whose OIDC set is incomplete) returns the
  instance to restricted mode where `/admin` is open again — by design, that
  is the recovery loop, and it grants nothing but the config editor.

This is the deliberate trade: the network-reachable surface during recovery
is one validated-save form, never the repository store.

---

## 3. Group claim semantics

### 3.1 Shapes, matching, and the claim name (pinned, mostly unchanged)

Semantics are WP13's, restated as normative and unchanged:

- The claim named by `server.auth.groups_claim` (default `"groups"`) is read
  from the **signature-verified ID-token payload only** — never a request
  header (`groupsFromClaim`, `internal/server/auth.go`).
- A JSON **array of strings** yields each non-empty trimmed element
  (non-string elements skipped); a **single string** yields one entry;
  absent / `null` / number / object / empty yields none. **No nested
  traversal**: an array of objects (e.g. Keycloak's nested group trees)
  yields no groups — flatten at the IdP (a mapper/claim transform) or the
  intersection is empty. Documented, with the diagnostic below as the
  operator's tool.
- Intersection with `admin_groups` is **case-insensitive** (`EqualFold`)
  with values trimmed — `Spindle-Admin` matches `spindle-admin`.
- Groups never grant `Write` (`write_domains` stays the write authority,
  default allow) and never bypass the `allowed_*` admission check.

### 3.2 How the operator verifies the group actually arrives

- **`GET /_auth/me` is extended** (additive fields, §8.6 wire): the response
  gains `"groups": [...]`, `"admin": bool`, `"groups_claim": "<name>"` for a
  signed-in principal. Machine-diagnosable in one curl with the session
  cookie.
- **The Access page (PLAN-ADMIN §3.4) renders the same facts** as a “Your
  session” card: resolved groups (mono), configured admin group(s), the
  verdict line (“admin” / “not admin — your token's `groups` claim does not
  list `spindle-admin`”), and the session's issued-at so staleness is
  visible. Read-only; edits link to `/admin/config#auth`.
- These two read one source (the verified session/token payload), so the
  page cannot lie about what the API sees.

### 3.3 Refresh behaviour and staleness (the honest statement)

- The groups ride the session/`wgt_` wire at mint and the **admin policy is
  re-applied per request** — editing `admin_groups` takes effect immediately
  (grant *and* revoke) for live sessions.
- **IdP-side group removal is NOT re-fetched**: there are no refresh tokens
  and no userinfo backchannel (deliberately — both would be new wire and new
  round trips). A demoted user keeps admin until their session expires
  (`session_ttl`, default 30d) or they re-authenticate.
- Immediate demotion exists and is documented on the Access card: rotate
  `session_secret` (revokes every session and `wgt_` token at once — the
  §8.5 property) or shorten `session_ttl`. `spindle config check` mentions
  both in the auth notes.

### 3.4 What a signed-in non-admin can do

Everything their OIDC admission and repo roles allow — exactly the §5
role ladder of `docs/features/01_identity_permissions.md`: browse public
repos (`anonymous_read` on) and everything else per `access.json` visibility
+ bindings, create under their own namespace/member orgs, push where roles
allow, mint `wgt_` tokens, manage keys/invitations. **Nothing administrative**:
no `/admin*`, no `PUT /api/v1/setup*`, no `dangerous:true` import/mirror, no
host-admin API verbs (`requireAdmin` floors unchanged — the flag simply
never arrives without the group). The nav never shows-then-403s (PLAN-ADMIN
§6 rule).

---

## 4. Config surface and deaths

### 4.1 Removed (deleted, no shims — a present occurrence is a named error)

| Key | Fate on a file that still carries it |
|---|---|
| `server.auth.mode` | unknown key error at load: `server.auth.mode was removed — OIDC (issuer + client + session secret) is the only authentication; delete the key` |
| `server.auth.admin_emails` | unknown-key error: `server.auth.admin_emails was removed — administration is the OIDC admin group (server.auth.admin_groups)` |
| `server.auth.admin_domains` | same shape, names `admin_domains` |
| `[[server.auth.tokens]].admin` | named error (§1.3): row field removed |
| `SPINDLE__SERVER__AUTH__MODE` (env) | unknown-key error in the overlay report (`spindle config check` names it; boot logs it in restricted mode) |

Env overlay names for kept keys are unchanged (`SPINDLE__SERVER__AUTH__ISSUER`,
`…__OAUTH_CLIENT_ID`, `…__OAUTH_CLIENT_SECRET`, `…__SESSION_SECRET`,
`…__ADMIN_GROUPS`, `…__GROUPS_CLAIM`, `…__ALLOWED_DOMAINS`, …) — the
overlay channel itself is untouched.

### 4.2 Validation rules (fail closed, every error names its key)

`checkAuthConfigured` (was `checkOIDC`), a plain `Validate` rule — always on,
for boot, `config check`, and save alike:

1. `issuer` required, an absolute `http(s)` URL (new — today it is only
   probed, never validated).
2. `oauth_client_id` + `oauth_client_secret` both set (existing pair rule).
3. `session_secret` set and ≥ 32 bytes (existing).
4. `allowed_domains` or `allowed_emails` non-empty (existing admission rule).
5. `groups_claim` non-empty (existing; default covers it).
6. **`admin_groups` non-empty** — new. Under WP13 an empty list was legal
   (“group admins disabled”) because emails/domains were the migration path;
   with those gone, an empty list is a guaranteed lockout, and lockouts are
   configuration errors, not silent states. (Operators who truly want zero
   admins may not have that; the file can still be *written* by hand, but
   boot calls it invalid — deliberately.)

Everything else in `config.Validate` is untouched. No warning paths for auth
remain (`mustWarnings` keeps only non-auth warnings).

### 4.3 Setup schema rows

`internal/setup/setup.go`: `schemaEnums["server.auth.mode"]` deleted; the
`Field.Enum` mechanism survives for the other enums. The auth card loses
`mode`, `admin_emails`, `admin_domains` rows and the token-row `admin`
column (`elementFields`); `settingsFieldMeta` (webui) drops the matching
entries/notes and gains the renamed-default example (`spindle-admin`).
`AnchorOf`/`RestartKeys` need no change (key-name-driven). The `auth-test`
fragment and `ProbeAuth` extension are §2.2.

### 4.4 `spindle config check`

Reports the §4.1 removals as errors (exit 1, as all validation errors) and
the §4.2 rules with named keys; the old auth-none warning line is deleted.
`spindle config dump` reflects the shrunken section. The examples in
`docs/go/11_config_cli.md` §7 and `docs/go/16_packaging.md` §193 are
rewritten to the OIDC shape.

### 4.5 Route table after the change (`/_auth` and the auth surface)

| Route | Verdict |
|---|---|
| `GET /_auth/login` | keep — the OIDC redirector; disabled-flow page collapses to one honest branch (“browser login is not wired on this build”) |
| `GET /_auth/callback`, `GET /_auth/claimed` | keep (PKCE + nonce + loopback cookie hop, unchanged) |
| `GET /_auth/logout` | keep |
| `GET /_auth/me` | keep + `groups`/`admin`/`groups_claim` fields (§3.2) |
| `GET /_auth/check` | keep (edge `auth_request`; same principal) |
| `GET/POST /_auth/tokens` | keep — `wgt_` minting (groups ride the wire, admin re-derived) |
| `POST /_auth/token` | **deleted** (paste-an-admin-token sign-in dies with token mode) |
| `/_auth/login` 307 from `authFailure` | keep (browser GET, no Authorization → login) |
| `WWW-Authenticate: Basic` advertisement | keep whenever static tokens are configured (machine credentials over Basic) |

`install_sh.go` `token_url`, `health.go` clone-help `token_url`/`login_url`,
and the `smart.go` pkt-ERR help line (`/_auth/tokens`) are unchanged — they
describe `wgt_` minting and the login entry, both of which survive.

---

## 5. Code seams (file by file)

| File | Change |
|---|---|
| `internal/config/config.go` | `Auth.Mode`, `AdminEmails`, `AdminDomains` fields deleted; `StaticToken.Admin` deleted; `Defaults()`/`FirstRunDefaults` updated; admin-group default renamed (`spindle-admin`) |
| `internal/config/validate.go` | `checkAuthNoneLoopback` deleted; `checkOIDC` → `checkAuthConfigured` (§4.2, an always-on `Validate` rule — no mode guard; issuer URL shape; token-row `admin` named error) |
| `internal/config/firstrun.go`, `load.go`, `env.go`, `settings.go` | first-run shape stops setting mode; overlay/env unknown-key report gains the removal hints; per-repo settings untouched |
| `internal/server/auth.go` | `Authenticate` unconditional OIDC tree; `authToken`/`sessionPrincipal` (token-mode session) deleted; `principalFromEmailGroups` loses email/domain arms; `TokenSignInEnabled` deleted; `PrincipalForName` oidc-only; `authChallenge` Basic rule unchanged |
| `internal/server/auth_oidc.go` | `authTokenSignIn`, `tokenSignInFormHTML`, `writeTokenSignInError`, `loginPageHTML` token/none branches deleted; single honest disabled page |
| `internal/server/auth/principal.go` | `None()` deleted; doc contract updated |
| `internal/server/setup.go` | `setupAccess`: `open := boot.InSetupOnly()` only (the `Mode == "defaults"` arm dies with the boot mode; auth-none arm dies with the mode) |
| `internal/server/router.go` | `mountSetupOnly` copy gains the absent-file variant (§2.1); `authFlow` loses `POST /_auth/token`; `apiServe` anonymous-write assert unconditional (drop the mode test) |
| `internal/server/middleware.go` | `authFailure` Forbidden hint names `admin_groups` only; login-page call sites single-branch |
| `internal/server/health.go` | `auth_none` warning deleted; `token_url` no longer mode-gated |
| `internal/server/smart.go`, `install_sh.go` | copy touches only (help strings) |
| `internal/setup/setup.go` | enums/rows/`ProbeAuth` JWKS extension (§2.2, §4.3) |
| `internal/webui/render.go` | `ShowSetup` → PLAN-ADMIN's `ShowAdmin` with `restrictedBoot || p.Admin`; `ShowLogin` unconditional (validation guarantees the trio) |
| `internal/webui/server.go` | `authMode()` deleted; `signedIn = !p.Anonymous`; `identityAccessFor` `identityDisabled` branch deleted; `authModeOrDefault` → constant `"oidc"` for discovery |
| `internal/webui/settings.go` | `setupAuthInfo` token-sign-in arm deleted; `settingsFieldMeta` auth rows (§4.3); gate fallback `s.authMode() != "none" && !p.Admin` → `!p.Admin` |
| `internal/webui/forms.go` | `formsDangerousAllowed` → `!p.Anonymous && p.Admin`; none-branch deleted |
| `internal/webui/collab.go`, `public.go` | `SignedIn`/mode branches collapse |
| `internal/mirror/http.go`, `internal/repoimport/service.go` | `dangerousAllowed` → `!p.Anonymous && p.Admin` (the `AuthMode` seam on mirror's Handler is deleted with its only consumer) |
| `internal/api/discovery.go` | `authModeOf` reports `"oidc"` always (wire field kept, one value; restricted mode does not serve `/api/v1` at all) |
| `cmd/spindle/serve.go` | boot table (§2.1); `logSetupBanner` rewritten; SSH gated off in restricted mode |
| `cmd/spindle/webui.go` | `SetupAccess` binding follows `setup.go`; first-run card data |
| `cmd/spindle/config.go` | `config check` output (§4.4) |
| `internal/threads/*`, `internal/threads/runner` | **no change** — runner protocol credentials (`wrg_`/`wrt_`/`wjt_`) resolve through `ExtraCredential` + local HMAC/registry checks, never the human mode; `runnerProtocolPath` exemption keeps `register` reachable; the runner binary talks only to those endpoints |
| `internal/packages` | `AuthRequired` unconditional (`mode != "none"` test deleted); README's “`auth none` makes the registry anonymously writable” line dies |
| `internal/webui/templates/**` | header partial (`ShowAdmin`), settings/auth rows, the first-run card, denied-view copy (§2.4), Access-page session card (§3.2) — all composing existing classes; style guideline needs no extension (the denied/first-run copy are existing `.setup-callout`/card idioms) |

Concurrency: nothing new. The admin predicate stays a pure function over the
verified wire payload + static config (no locks, no I/O); `ProbeAuth`'s JWKS
fetch keeps the existing 10 s client; the restricted-mode gate is
boot-constant.

---

## 6. Test and dev story (no IdP required, and no shipped bypass)

**Product vs test-only, stated hard:** there is **no configuration flag,
env var, or build mode that authenticates without a verified credential in
the shipped binary**. The only credential shapes the serve binary knows are
static tokens (operator-authored, non-admin), `wgt_` HMAC tokens, the
runner/CI/job prefixes (`ExtraCredential` — per-composition, unchanged), and
OIDC-verified ID tokens/sessions. Nothing in §6 adds a seam to `internal/config`.

1. **In-process Go tests** already mint RS256 ID tokens against a stub JWKS
   (`internal/server/static_test.go`'s `TestJWKSVerify` issuer pattern) and
   construct `auth.Principal` values directly for page gates
   (`cmd/spindle/*_test.go` does this everywhere). Unchanged method, extended
   coverage (§8).
2. **E2E exercises the real verification path**: the harness gains
   `internal/e2e/oidp.go` — a ~120-line in-test IdP (stdlib only: an RSA key,
   discovery doc, `/jwks`, and a mint helper for RS256 ID tokens carrying
   configurable `groups`), same shape as `static_test.go`'s stub but on a
   real listener. Pushes and API calls authenticate with
   `http.extraHeader=Authorization: Bearer <minted ID token>` — the oidc
   tree's direct bearer-ID-token branch. **No injected principal, no
   shortcut: the signature is actually verified.** A full browser login is
   driven once through `/_auth/login` → stub authorize → callback (the stub
   serves an auto-approving authorize endpoint) in the M4 milestone's
   rendered drive.
3. **Dev IdP for humans**: `internal/devtools/oidpstub` — a new devtools
   program (`go run ./internal/devtools/oidpstub`), stdlib-only, serving
   discovery, JWKS, an authorize page (username + comma-separated groups
   form) and a token endpoint that mints the signed ID token. This is
   **test/dev tooling, not product**: `internal/devtools` is not in the
   release binary's import graph (the binary builds from `cmd/spindle`;
   devtools already hosts covergate), the Dockerfile does not ship it, and
   its `-help` says so. Optional compose profile `compose.dev-idp.yml`
   wires it beside spindle for local development. No new Go module, no new
   image dependency — configuration only (law 1 untouched).
4. **A loud-documentation backstop** instead of an impossible-to-enable seam:
   `docs/go/15_testing.md` documents exactly this list, and a grep gate in
   M5 asserts `internal/config` contains no `testauth`-shaped key (the
   strongest claim available: there is nothing to leak, and the gate keeps it
   that way).

---

## 7. Migration (pre-1.0: change in place, no compat path)

- **Operators on `none`**: the upgraded binary boots restricted; `/admin`
  explains exactly what to set (the five fields), and every other route 503s
  with the pointer. Existing data is untouched (the store never opened under
  `none` any differently). The old warnings are gone; the README quick start
  stops saying “just run it”.
- **Operators on `token`**: tokens keep authenticating git/API/CLI as
  *write* machine credentials, but no token is an admin any more and the
  paste-form sign-in is gone. Administration requires the OIDC group. If
  they configured no OIDC, they land restricted with the same onboarding.
- **Operators on `oidc`**: unaffected except (a) if their admins came from
  `admin_emails`/`admin_domains`, those grants end — the migration note
  (§1.4) applies; (b) the default admin group name changes only if they
  never set `admin_groups` and relied on the default `spindle-admins` —
  **they must add the IdP group `spindle-admin` or set
  `admin_groups = ["spindle-admins"]`**; the restricted page says this
  verbatim in the boot errors.
- **Sessions/cookies**: none-mode had none. Token-mode browser sessions
  minted under `PrincipalForName` fail verification on the new binary (their
  resolver branch is gone) → those users are simply signed out. OIDC sessions
  survive byte-identically (HMAC + email/groups payload unchanged, law 5) —
  but admin is re-derived per request, so a session whose email used to match
  `admin_emails` loses admin at the next request, and a group-admin session
  keeps it. Rotating `session_secret` remains the global revoke.

---

## 8. Tests that must change (the boots-with-`none` inventory)

- `internal/config/validate_test.go` — none/token loop (line ~158), the
  empty-`admin_groups` allowance (line ~99) flips to an error, admin-group
  default rename pin, issuer-URL rule, token-row-`admin` rejection.
- `internal/server/setup_group_access_test.go` — the access matrix: `none`
  arm deleted; email/domain arm (line ~131) deleted; new
  `admin_groups = []` ⇒ 403-for-everyone arm.
- `internal/server/setup_merge_test.go` — “setup is open at ANY time while
  mode is none” (line ~425) lane deleted; restricted-mode lanes keep their
  shape; `oauth_client_secret` clearing (line ~145) now fails the new rule.
- `internal/server/static_test.go`, `auth_oidc_test.go`,
  `auth_hardening_test.go`, `x_gaps*_test.go`, `x_auth_test.go` — ID-token
  lanes unchanged; add `TestAdminPredicateSingleSource` (§1.2).
- `internal/server/auth_login344_test.go`, `auth_token_signin_test.go` —
  paste-form and token-sign-in suites deleted with the surface (their
  login-page assertions move to the single-branch page test).
- `internal/server/router_test.go` (`auth_none` readyz warning, ~line 266),
  `x_health_test.go` — warning assertions deleted; restricted-mode matrix
  gains the absent-file boot.
- `internal/server/sshkeys_registry_test.go` (line ~324 `Mode: "none"`),
  `x_gaps7_test.go` (AdminEmails), `x_misc_test.go` (`/_auth/*` inventory and
  the login-page assertions for the deleted branches).
- `internal/webui/identity_gate_test.go` (none-mode case), `settings_test.go`
  (`AuthMode: "none"` fixture, enum `{none,token,oidc}` pin), `public_test.go`
  (`authModeOrDefault`), `collab.go`/`forms.go` gate tests — none branches
  collapse.
- `internal/e2e/e2e_test.go` — `bootstrap_no_config` becomes
  boots-restricted (git push 503; `/admin` 200); `TestE2E_SetupEditsExistingFile`
  (the `decoded.Server.Auth.Mode != "none"` assertion, line ~458) and
  `TestE2E_SetupSaveRestart` save **complete OIDC configs** through the same
  open setup surface; `TestE2E_InvalidConfigSetupOnly` unchanged in shape.
- `internal/e2e/collab_test.go`, `threads_triggers_test.go`,
  `threads_runner_test.go` — the `chainServer` token configs become
  `oidp.go` configs: alice = minted ID token with `groups:
  ["spindle-admin"]`, bob = plain email at the allowed domain;
  `runAuth`/`mustAPI` take the minted tokens; the runner lanes' job/CI
  tokens are untouched.
- `cmd/spindle/*` — `push_budget_test.go` (Boot defaults, line ~213) boots
  restricted; `settings_wiring_test.go`'s pinned `SetupAuthTest` binding
  stays; `setuponly_test.go`/`setup_blocker_test.go` fixture configs gain the
  OIDC set.
- `make sim` — no change (the sim is store-level, no HTTP auth);
  `make contract` — no change.

New coverage: `TestAdminPredicateSingleSource`, the restricted-boot matrix
(absent/invalid × the mountSetupOnly route set), `ProbeAuth`-with-JWKS unit
tables, the first-run card's five-field save + suggestion copy, the
`/_auth/me` extension, and the e2e browser login drive (M4).

---

## 9. Milestones, effort, risks

| # | Milestone | Contents | Est. |
|---|---|---|---|
| M1 | **The flip** | Config deaths + `checkAuthConfigured`; auth tree unconditional-OIDC; `auth.None`/paste-form/token-session deletions; boot table (absent ⇒ restricted, SSH off in restricted); `setupAccess`, webui gates/branches; discovery/health/copy; e2e bootstrap + collab/threads lanes move to `oidp.go`; compose/README/AGENTS/DEVIATIONS entries. Wide by necessity: the mode key touches every switch, and the tree must compile in one commit. | 2.5 d |
| M2 | **First-run onboarding** | The five-field card, session-secret suggestion, redirect-URI copy, `ProbeAuth` JWKS extension, saved/restart copy, restricted-`absent` banner copy. | 1 d |
| M3 | **Access diagnostics** | `/_auth/me` extension + the Access-page session card + denied-view recovery copy (§2.4) + the PLAN-ADMIN amendment set (§1.6). | 1 d |
| M4 | **E2E + rendered drive** | Full OIDC login through the stub (browser drive, desktop + 390 px + keyboard pass), restricted-mode drive, the changed-lane greens. | 1.5 d |
| M5 | **Dev IdP + doc sweep** | `internal/devtools/oidpstub`, `compose.dev-idp.yml`, the remaining doc sweep (11/16/01/12/15/06/feature-01), the no-test-seam grep gate. | 1.5 d |

Every milestone: `make fmt && make vet && make test && make race && make cover`
(the ≥95 % gate holds — deleted code deletes coverage debt; new branches are
table-covered), `make e2e` for M1/M4, the rendered drive for M2–M4.

**Risks:**

1. **Lockout** (the headline risk). Three independent backstops: validation
   refuses a config with an empty `admin_groups` or incomplete OIDC set
   (§4.2) so an *invalid* config never boots normal; the denied/first-run
   pages state the file/env recovery verbatim (§2.4); `config check`
   validates offline. Residual: a *valid* config whose IdP never emits the
   group — mitigated by the Access/`_auth/me` diagnostics (§3.2) and the
   session-secret rotation lever; accepted residual is “edit the file”,
   which is also the answer for every other fatal misconfiguration.
2. **IdP outage**: new logins block (discovery/exchange fail, existing 503
   mapping); existing sessions and `wgt_` tokens keep working (local HMAC
   verification, no IdP call); JWKS serves stale-while-refresh (existing);
   static tokens keep git/CI moving. The Access page says this.
3. **Session/group staleness** (§3.3): a demoted admin until session
   expiry. Documented on the Access card + rotation lever; no refresh-token
   machinery (would be new wire and new round trips — rejected, Appendix B).
4. **Test seam leaking into production**: there is none to leak (§6.4) —
   the assertion is a grep gate plus the design fact that no bypass key
   exists in `internal/config`.
5. **First-run editor exposure** (§2.2): a restricted instance's config
   form is network-reachable; it can only write validated configs and shows
   no secrets. Operators who dislike the window bind private or set
   `server.listen` before first exposure — the card says so.
6. **M1 breadth** (compile-coupled deletions) — mitigated by doing M1 as one
   reviewed commit with the e2e lane rewrite inside it, never a “temporarily
   broken main”.
7. **PLAN-ADMIN interlock** (§1.6): both plans edit four shared spots; the
   second-landing side updates the other's text. Recorded in both.

---

## 10. Owner decisions requested

1. **Admin-group default spelling**: plan adopts the owner's `spindle-admin`
   (renaming today's default `spindle-admins`). Confirm, or keep
   `spindle-admins` and skip the rename churn (one line either way).
2. **Static tokens retained** as machine write credentials with admin
   stripped (§1.3) vs removed entirely. Plan recommends retain; removal is a
   1-day smaller M1 but costs CI/automation a stable credential.
3. **Full editor reachable in first-run restricted mode** (§2.2, recommended
   — it is the recovery path) vs OIDC-card-only until first save.
4. Confirm `anonymous_read` stays (default `true`, public browsing).
5. Confirm this plan lands **after** PLAN-ADMIN M1 (URL move) per §1.6.

---

## Appendix A — where the removed shapes live on `main` (verified, for the sweep's reviewer)

The M1 sweep is “done” when grep finds none of these. Locations verified on
`main` (`e21f98e`):

| Shape | Sites on `main` |
|---|---|
| `Auth.Mode` field / `Mode: "none"` defaults | `internal/config/config.go:36,403` (field + `Defaults()`), `internal/config/firstrun.go:24` (`FirstRunDefaults`) |
| `case "none"` / `case "token"` branches | `internal/server/auth.go:159–169` (`Authenticate`), `internal/server/auth.go:1170–1214` (`PrincipalForName`), `internal/server/auth_oidc.go:182` (`authTokenSignIn` mode gate), `internal/server/auth_oidc.go:264–321` (`loginPageHTML` token/default branches) |
| `auth.None()` principal | `internal/server/auth/principal.go:38`, callers `auth.go:161,1172` |
| `checkAuthNoneLoopback` warning | `internal/config/validate.go:143–153` + `serve.go:83` (`mustWarnings`) |
| `AdminEmails`/`AdminDomains` arms | `internal/server/auth.go:335–344` (`principalFromEmailGroups`), `internal/config/config.go:39–40`, `middleware.go:537` (hint text), `x_gaps7_test.go:126`, `setup/roundtrip_test.go:49` |
| `StaticToken.Admin` | `internal/config/config.go:32` (field), `auth.go:86` (resolution), setup row schema via `elementFields`; e2e token configs `collab_test.go:148–165`, `threads_triggers_test.go:29–33`, `threads_runner_test.go:42–46` |
| `mode == "" || mode == "none"` page gates | `internal/webui/render.go:79` (`ShowSetup`), `internal/webui/server.go:142,169` (`signedIn`, `identityAccessFor`), `internal/webui/forms.go:267,1321` (`formsDangerousAllowed`), `internal/webui/settings.go:1236,1307` (gate fallbacks), `internal/webui/public.go:380` (`authModeOrDefault`), `internal/mirror/http.go:606–616`, `internal/repoimport/service.go:587–596` |
| `"auth_none"` / setup banner | `internal/server/health.go:81–83` (readyz), `cmd/spindle/serve.go:557–561` (`logSetupBanner`), `router_test.go:266` |
| `TokenSignInEnabled` / paste form | `internal/server/auth.go:426–428`, `auth_oidc.go:162–245`, `webui/settings.go:1283–1291` (`setupAuthInfo`), route `router.go:254` |
| `schemaEnums["server.auth.mode"]` | `internal/setup/setup.go:79`; page pin `webui/settings_test.go:1993`; docs `06 §3.4`, `11 §2/§5`, `12 §2.10`, `15:267/270` (bootstrap tiers), `16:170/193/325/504/560`, `01:455`, README 17/20/22/33/65/121, DEVIATIONS D-UX-1/2/3, AGENTS law 9 note + law 10 |
| `apiServe` mode assert | `internal/server/router.go:50` (`Mode == "oidc" &&` — becomes unconditional) |
| `packages` gate | `cmd/spindle/packages.go:62` (`AuthRequired: Mode != "none"`) |

---

## Appendix B — rejected alternatives (with reasons)

- **Keep `mode` with a single value `"oidc"`.** A one-value enum is a vestigial
  key: it forces every switch site to defend a state that cannot occur, and
  it invites the next mode back. Deleting the key makes “auth configured”
  observable from the values that actually matter.
- **Retain `admin_emails`/`admin_domains` as break-glass.** §1.4: ambient
  authority, and the file edit is the only break-glass an operator actually
  needs. Keeping it re-opens wildcard-admin misconfigurations.
- **Keep `admin = true` static tokens.** Directly contradicts the owner's
  single-predicate rule; a paste form that mints admin sessions from a file
  row is the exact “administrable by anyone who reaches it” shape in
  miniature.
- **A network dial inside Save** (probe-then-save enforced). An unreachable
  IdP during recovery would block writing the fixed config — the recovery
  path must never depend on the thing being recovered from. The Test button
  is the live validation; save stays shape-validation.
- **Refresh tokens / userinfo backchannel for group freshness.** New wire
  (law 5 surface), new per-request round trips (law 6), and IdP-cooperation
  requirements — for staleness that session rotation already answers.
  Documented staleness beats a half-standard refresh.
- **Env-based test bypass in the serve binary.** Anything the binary reads
  from config is one leaked env var away from an open instance. The e2e
  stub-IdP route exercises the real verification path instead, which is
  better *and* safer.
- **Block restricted mode on `server.listen` being non-private.** Rejected:
  it would make a fresh container unfixable from its own published port. The
  restricted surface is one validated-save form; that is the accepted trade
  (risk 5).

---

## As built (2026-11-21, branch `feat/oidc-only`)

Status: **implemented**, one PR with sequenced commits. What landed, with the
places where the tree forced a decision the plan did not pin:

1. **The mode and the ambient authority are deleted.** `server.auth.mode`,
   `admin_emails`, `admin_domains` and `Server.auth.tokens[].admin` are gone;
   a file/env still carrying one fails validation with the named removal hint
   (`removedAuthKeyHint`, §4.1). `Auth.None()`, `POST /_auth/token`, the
   paste-form path, `TokenSignInEnabled`, `PrincipalForName`'s none/token arms
   and every `authMode()` branch were deleted in the same sweep.
2. **The single predicate** is `AuthService.principalFromEmailGroups`
   (`internal/server/auth.go`): it builds `Admin` from the verified groups ∩
   `server.auth.admin_groups` and nothing else. `TestAdminPredicateSingleSource`
   (`internal/server/auth_admin_test.go`) pins every credential shape (ID
   token, session, `wgt_`, static token, `ExtraCredential` CI/runner/job
   shapes, anonymous).
3. **Boot:** `resolveConfig` grew `stateUnconfigured` (no file + invalid
   effective config); `serve` turns it into `BootState{Mode:"setup_only",
   FileState:"absent"}` with no error list, and an invalid file keeps
   `FileState:"invalid"` plus its errors. `BootState.Mode` is now exactly
   `normal|setup_only` — the old `defaults` mode is deleted. SSH does not
   listen in the restricted boot.
4. **First run as built:** the Overview card posts **six** keys, not five —
   `server.auth.allowed_domains` is required by `checkAuthConfigured`, so the
   plan's five-field list could never save. The sixth field is the email-domain
   allowlist (or `allowed_emails` on Configuration). The card's Save posts only
   those keys through the ordinary `POST /admin/config`;
   `TestFirstRunCardPartialPost` asserts the untouched first-run keys survive.
   Test = `setup.ProbeAuth`, now fetching the discovered `jwks_uri` and
   requiring ≥1 usable RS256/ES256 key (`SigningKeys` on the result). Save never
   dials the IdP.
5. **Diagnostics:** `/_auth/me` gains `groups`/`admin`/`groups_claim`; the
   Access page renders the live groups beside the configured group, the
   verdict, and the session issued-at (via the new `pages.Auth.SessionInfo`
   seam). The denied view names the file/env recovery verbatim.
6. **Test seam:** `internal/e2e/oidp_test.go` is an in-test IdP (discovery,
   JWKS, authorize, token endpoint) minting real RS256 ID tokens; every e2e
   boot that relied on `auth.mode=none/token` now points at it, and the full
   browser login (nonce + PKCE) is driven in
   `TestE2E_OIDCFirstRunAndAdminByGroup`. The file is `_test.go`, so it cannot
   compile into any release binary; `internal/devtools/oidpstub` is the human
   dev IdP and is not in `cmd/spindle`'s import graph. The no-seam gate is
   `TestNoTestAuthSeamInConfig`.
7. **Machine credentials over git:** the 401 challenge advertises Basic
   whenever a machine credential can exist (static rows **or** an
   `ExtraCredential` hook), because git only asks its credential helper when
   Basic is offered — without this, job/CI tokens could not push even though
   the chain accepts them. This is an addition to §4.5's "whenever static
   tokens are configured" wording (same rationale, wider predicate).
8. **Fork-PR approval:** a group-only host admin is not resolvable from the
   bare actor name an event carries, so the fork e2e approves its held run
   explicitly (the §1.3 trade-off, as documented). No production code changed
   for it.
9. **Restricted-mode nav:** only Overview and Configuration are mounted, so the
   area nav hides Access/Maintenance in the restricted boot (they would 503).
10. **Docs:** README quick start, both compose files (IdP prerequisites +
    healthz-based healthcheck during the restricted first run), `DEVIATIONS.md`
    (D-OIDC-1, reversing D-UX-3 and the auth-`none` half of D-UX-1), AGENTS laws
    9/10, `docs/features/01_identity_permissions.md`, `docs/go/06_server_http.md`
    §3.4/§8 (+ the appended D-OIDC-1 decision), PLAN-ADMIN's nav/first-run/
    first-admin/denied statements, `docs/go/11_config_cli.md`,
    `docs/go/16_packaging.md`, `docs/go/15_testing.md`, and the AGENTS
    Administration field lesson.
11. **Readiness as amended (2026-11-22, incident fix):** the restricted boot
    answers `/readyz` **200** `{"status":"restricted","version":…,"surface":"admin"}`
    instead of the 503 `setup_required` this plan originally pinned (§2.1).
    The plan's 503 made a health-gated ingress route nothing, so the operator
    could not reach `/admin` to fix the file that caused the restricted boot
    (the live incident). Readiness means "serve this process's allowed
    surface"; `/healthz` stays liveness; a phase-2 drain still answers 503 in
    both boots, and the normal boot's readiness rules are untouched
    (`docs/go/06_server_http.md` §10.2 carries the ingress contract).
