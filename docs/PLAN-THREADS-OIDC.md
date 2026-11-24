# PLAN — Threads job OIDC tokens (audience-scoped ID tokens)

Status: implemented (2026-11-23). Scope: the short-lived ID token a Threads job can present to a
cloud provider (AWS/GCP/Azure workload identity, Vault, etc.), gated by the workflow's `permissions:`
and issued by the spindle instance. It is the Threads equivalent of GitHub Actions' OIDC token and
has the same shape (a request token in the job environment, an exchange endpoint, an RS256 JWT, a
discoverable JWKS).

Read with: [`features/15_threads.md`](features/15_threads.md) §2.6/§8, [`threads.md`](threads.md) §5.5
(the user-facing contract), `docs/go/06_server_http.md` (routes) and `docs/go/02_storage_protobuf.md`
(the key family).

## 1. Issuer and discovery

The **issuer is `server.public_url`**, trimmed of its trailing slash. That key already exists and is
already the operator's declaration of the externally reachable origin (the checkout URL uses it,
§4.4), so no new configuration key is introduced. When `server.public_url` is empty the issuer is the
request's origin (`scheme://host`, honouring `X-Forwarded-Proto`); that fallback is for development
and for instances that never leave localhost. A deployment behind a proxy **must** set
`server.public_url`: otherwise `iss` follows the `Host` header, a provider configured from one host
will reject tokens minted through another, and nothing in spindle can make that consistent.

Two public routes make the issuer discoverable, hand-rolled JSON, no third-party library:

| Route | Answer |
|---|---|
| `GET /.well-known/openid-configuration` | `{issuer, jwks_uri, subject_types_supported:["public"], id_token_signing_alg_values_supported:["RS256"], response_types_supported:["id_token"], scopes_supported:["openid"], claims_supported:[…]}` |
| `GET /.well-known/jwks.json` | `{keys:[{kty:"RSA", use:"sig", alg:"RS256", kid, n, e}]}` |

`jwks_uri` is `{issuer}/.well-known/jwks.json`. Both documents are `public, max-age=60`: keys rotate
at most every 90 days, and a verifier that sees an unknown `kid` refetches. They are **open** (a
provider has no spindle credential) and are registered on the core router only when the API seam is
wired; without the Threads surface they are the plain 404. The JWKS is the **public** half only — the
private PEM never leaves the key object and never appears in a page, an API answer, a job payload or a
log line (tests assert the served JSON has no `d`/`private_key_pem` and no `PRIVATE` text).

## 2. The signing key

```
threads/oidc/signing-key.json
{
  "version": 1,
  "active_kid": "<kid>",
  "keys": [
    { "kid": "<kid>", "alg": "RS256",
      "private_key_pem": "-----BEGIN RSA PRIVATE KEY-----…",
      "created_at": "<RFC3339>", "retired_at": "<RFC3339>" }
  ]
}
```

- **Algorithm**: RS256 with the standard library (`crypto/rsa`, `crypto/x509`, `crypto/sha256`). The
  modulus is 2048 bits: the RS256 floor every provider accepts. No new dependency (AGENTS law 1).
- **Generated once, CAS-created.** On first use the instance generates a key and
  `PutCreate`s the object. A 412 means another instance won the race: the loser re-reads the winner's
  object and signs with **that** key. It never overwrites, because two trust roots would mean two
  answers to "which key signs this instance's tokens".
- **`kid`** is `base64url(sha256(n || e))[:16]` — a public-key thumbprint, stable across copies and
  never derived from private material.
- **Rotation is automatic and CAS'd.** An active key older than **90 days** is retired
  (`retired_at` stamped) and a new key becomes active. The old public key stays in the JWKS for
  **one hour** after retirement — five token lifetimes plus clock skew — so a token signed just
  before a rotation verifies until it expires; keys retired longer than that are pruned from the
  object on the next rotation. A manual rotation is the same operation: an operator edits the key
  object (or another instance rotates first) — there is no rotation API and therefore no new admin
  surface to gate.
- **In-process cache (one entry).** The active private key and the rendered public JWKS are held in
  memory. The shape is the account-gate LRU precedent minus the LRU: a plain mutex around a pointer,
  never a store client held across a call (law 3). **Cold = one exact-key GET; a warm JWKS read and a
  warm exchange do zero store reads.** A 60 s TTL bounds how long this instance can miss a **peer**
  instance's rotation; a **local** rotation is stored immediately (the loader is exclusive, so no
  other caller can load the old set after the CAS commits), and the retired public keys stay in the
  cached JWKS for the 1 h overlap. A cold burst is single-flighted, so the open JWKS endpoint cannot
  amplify into one bucket GET per request. Errors are **never** cached: a store fault fails that
  request and the next request re-reads. The cache is memory-only — a restart re-reads the bucket.
- **The bucket is a trust root, and this is acknowledged.** Whoever can write
  `threads/oidc/signing-key.json` can mint tokens this instance will present as its own. That is the
  same trust the store already holds for every repository, policy and secret envelope; the key is
  not additionally sealed because a lost sealing key would be a worse failure (a permanently
  unverifiable instance) and the bucket already contains the ciphertexts' key material. Backup and
  access control for the bucket therefore cover this family too.

### Concurrency

Hazard: two instances generating the first key (or rotating at the same instant). Avoidance: the
first write is `PutCreate` and rotation is a bounded CAS; the loser re-reads and adopts the winner's
active key. No in-process lock is taken and no lock is held across a store call (13_concurrency.md
§2 rule 4). Hazard: two exchanges for one job racing the rate-limit counter. Avoidance: the counter is
a CAS on the request-token record (the loser re-reads and re-applies); the point is to stop a loop,
not to meter precisely, so a racing pair may overshoot by the number of concurrent writers. Hazard:
an instance reading the key mid-rotation. Avoidance: a rotation appends the new key and retires the
old one in one CAS; a reader sees either the pre- or post-rotation object, and both name a usable
active key.

## 3. The request token

Each **job attempt** gets one opaque request token:

```
wot_<64 hex chars>          (32 random bytes, hex)
```

- **Hash-only storage.** The record holds `sha256(token)`:
  `repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/id-token/<attempt:03d>.json` with
  `{version, repo, run, job, attempt, token_hash, created_at, expires_at, window_from, window_count}`.
  The plaintext exists only in the fetch payload that handed it to the runner and in the job's
  environment. A record cannot be replayed from the bucket.
- **Bound to (repo, run, job, attempt).** The exchange derives the record key from the URL's opaque
  job reference and attempt and requires the record's identity to match and the hash to compare in
  constant time. The job record's **current attempt** is the authority: a requeue or rerun advances
  the attempt, so the old token fails at the record key (no record) or at the identity check. One
  job's token can never fetch another job's identity.
- **Lifetime** is the attempt token's own: the job's timeout plus ten minutes, two hours when the job
  declares no timeout, capped at six hours. A finished job revokes its token (the exchange refuses a
  terminal job state), and expiry is enforced server-side.
- **Only for a trusted job with the permission.** The token is minted at claim time only when the
  run is trusted **and** the job's effective permission map resolves `id-token` to `write`. A fork run
  is untrusted (its effective map is `contents: read` by policy) and never receives one.

## 4. Claims

```
iss               {issuer}
sub               repo:<owner>/<name>:ref:<ref>   (a pull request's ref is its base branch)
aud               <the requested audience>
exp, iat, nbf     iat = nbf = now; exp = now + 300 s (≤ 10 min)
jti               random 16-byte hex (replay identifier)
repository        <owner>/<name>
repository_owner  <owner>
ref               refs/heads/<branch> (the run's ref)
sha               the run's commit
workflow          the workflow's display name
job               the YAML job key
run_id            the run id
run_attempt       the run header's attempt
event_name        push | pull_request | workflow_dispatch | schedule
```

The claim set is a Go struct, not a map, so the wire shape is pinned and cannot grow an accidentally
exported field; the test asserts the exact 16 keys. `exp` is 5 minutes (GitHub's default) and never
longer than 10 minutes. `sub` mirrors GitHub's repository/ref form; there is no
`environment:`/`pull_request` subject variant in v1 (the run's ref is always known).

## 5. The exchange

```
POST {issuer}/api/v1/threads/jobs/{ref}/attempt/{n}/id-token
Authorization: Bearer <request token>
{"audience":"sts.amazonaws.com"}
→ 200 {"value":"<compact JWT>"}
```

- **Audience is required.** A missing, empty, whitespace-only, over-512-byte or control-character
  audience is a `400` naming the field. Unknown top-level keys are a `400` (the strict body rule).
- **The endpoint is machine-only and rides the runner protocol's path family**
  (`/api/v1/threads/jobs/…`). The request path is handed to the API provider **without
  session/ID-token authentication**: the `wot_` bearer is not a session credential, and the ordinary
  authenticator would reject it as an unknown token before the handler could verify it. The handler
  is the only claimant of that path shape and authorises the request token against the bucket record,
  so the hand-off grants nothing; every other path keeps the normal gate (a `wot_` presented
  elsewhere is still a real 401). A job token (`wjt_`) presented here is refused with an explicit
  "takes the request token, not the job token" message.
- **Errors**: missing/unknown/expired request token or a mismatched hash → `401` (generic, no job
  existence probe); a token scoped to another attempt, a finished job, a fork run, or a missing
  `id-token: write` → `403` naming the rule; over-budget → `429` with `Retry-After: 60`; unwired
  surface → `503`.
- **Rate limit**: 10 exchanges per job attempt per 60-second window. The window and counter live on
  the request-token record and are updated by the same CAS that verifies the token, so it is
  multi-instance-correct and needs no process state or LIST. A window that has lapsed resets on the
  next exchange.

## 6. Permission and fork rules

- The gate is the **effective** permission map (`workflow` merged with the `job`, job wins, over
  `threads.default_permissions`) with `id-token == write`. Explicit is the only way to grant it:
  `permissions: write-all` / `permissions: all: write` does **not** imply `id-token: write` (the
  existing `all` handling only widens `contents`). That is deliberately fail-closed: an OIDC token
  can become a cloud identity, so it is never a side effect of a broad shorthand.
- An unknown permission level never widens access (unchanged).
- **Fork runs never get a token**, at the mint path and again at the exchange: the run/job trust pair
  must be true, and a fork run's effective map cannot carry `id-token: write`. A request-token record
  that somehow exists for a fork job is refused with `403`.
- A missing permission, a finished job, a superseded attempt and an expired token are all refusals
  **after** the token is verified, so an attacker without a valid token learns nothing about the job.

## 7. Environment contract

Only a job with the permission receives these; a fork run and a job without it receive none of them.

| Variable | Value |
|---|---|
| `SPINDLE_ID_TOKEN_REQUEST_URL` | the exchange URL (issuer + the job's path) |
| `SPINDLE_ID_TOKEN_REQUEST_TOKEN` | the opaque request token |
| `ACTIONS_ID_TOKEN_REQUEST_URL` | the same URL under GitHub's spelling (compatibility: workflows already written for GitHub read it) |
| `ACTIONS_ID_TOKEN_REQUEST_TOKEN` | the same token under GitHub's spelling |

Both spellings are set for one reason: this product executes GitHub-shaped workflows, and a step
that already implements GitHub's OIDC dance should not need a spindle branch. They are the same value.
The **URL** is container-level environment; the **token** is delivered by name (`--env NAME`) with its
value in the docker client's environment, exactly like `SPINDLE_JOB_TOKEN`, so it never appears in a
`docker create`/`docker exec` argv, in `docker inspect`, or in `.git/config`. A `docker://` action
receives **neither** (the image is third-party code, the same rule as the job token and secrets). The
runner adds the token to its local log mask (best effort).

## 8. Deviations and limits

- **No `id-token` shorthand widening.** `write-all` does not grant it (see §6).
- **No environment/pull-request subject variants.** `sub` is always `repo:o/r:ref:<ref>`; a provider
  that keys on GitHub's `environment:` or `pull_request` subject needs a workflow-level environment
  feature Threads does not have.
- **No server-side replay store.** The `jti` is unique per token but the endpoint has no nonce
  registry: a provider enforces `aud`/`exp`, and re-presenting the same request token mints a new
  token (bounded by the rate limit) rather than replaying one. Revocation is "expire or finish the
  job"; there is no per-token deny list.
- **Instance key, not per-repository.** One signing key trust root covers the instance. A per-repo key
  would multiply key objects and rotation races for no security gain (the same bucket holds all of
  them); the claims carry the repository identity instead.
- **Not exercised end to end against a real cloud provider or a registered runner.** The server
  half, the runner plan and the exchange are tested with fakes and the in-memory store; the runner is
  unregistered in this environment, so no real job container performed a live exchange and no cloud
  STS endpoint verified a live token. Signature verification is covered by verifying the JWT against
  the served JWKS with the standard library.
