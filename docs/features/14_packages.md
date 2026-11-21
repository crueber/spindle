# 14 — Packages: an OCI registry

> **Status:** WP1 skeleton + second pass landed (2026-11-20 / 2026-11-21) — the registry protocol,
> bucket storage, Basic + Bearer auth, the git-identical push gate, the feature toggle, limits, the
> Packages page/tab, the JSON API, the SDK group, the CLI and the `packages-gc` maintainer sweep are
> all in (`internal/packages`, `internal/webui/packages.go`, `web/sdk/src/packages.js`).
> Supersedes the P9 "Packages — out of scope" line in `docs/features/README.md` for the OCI case only.
> Companion: `docs/PLAN-V1.md` WP1; audit + fixes: `docs/AUDIT-V1.md`.

## 1. Scope

Spindle hosts **container images** at `https://<host>/<owner>/<repo>` using the **OCI Distribution
Spec v1.1** subset that Docker, Podman, `buildx` and `crane` require:

```
docker login <host> -u <principal> -p <token>
docker push <host>/acme/demo:1.4.2
docker pull <host>/acme/demo@sha256:<digest>
```

The package is **the repository**: `<host>/<owner>/<repo>` addresses `repos/<owner>/<repo>` and its
permissions are that repository's roles. That is a deliberate difference from GHCR (which allows
free-form package names per owner) and it is what makes the object-store model work — no second
namespace to own, no package↔repo permission mapping to invent.

**Non-goals for v1:** npm, Maven, NuGet, RubyGems; signed-image policy (cosign/notation);
vulnerability scanning; cross-repository blob mounting (`?mount=`/`?from=`); the `referrers` API;
pull-through caching; quotas. Each is a later plan, not a hidden requirement.

## 2. Object layout (bucket-native, append-only)

```
repos/<o>/<r>/packages/registry/
  blobs/sha256/<hex>            raw bytes, content-addressed, shared by manifests
  manifests/sha256/<hex>        manifest bytes (content type in the manifest's mediaType)
  tags/<tag>                    CAS JSON: {"digest":"sha256:...","media_type":"...","updated_at":"..."}
  uploads/<id>/data             chunked-upload staging (TTL, GC'd by the maintainer)
```

Rules:

- **Blobs are content-addressed and never rewritten.** The digest is verified on finalize; identical
  bytes mean the same object (the S3 ETag-is-a-hash lesson applies — never assume a version counter).
- **Manifests are blobs with a media type.** An OCI image index (multi-arch) is stored exactly like a
  single manifest; nothing in the registry needs to interpret it beyond its size.
- **A tag is a CAS pointer.** Concurrent pushes to the same tag race on the pointer's version, not on
  the bytes; the loser retries once against the new version or returns `409`.
- **No index object and no reference counting.** Tags list by prefix paging; GC runs as a sweep
  (§6), so no hot path ever enumerates the bucket (law 4).

## 3. Routes

All under the registry namespace; mounted as a `RouteProvider` (`internal/server/bind_api.go`), never
by importing upward into `internal/server` (law 8).

| Method | Path | Behaviour |
|---|---|---|
| GET | `/v2/` | `200 {}`, `Docker-Distribution-API-Version: registry/2.0`; `401` with the challenge when auth requires it |
| GET/HEAD | `/v2/{o}/{r}/manifests/{ref}` | by tag or digest; returns the manifest bytes, `Content-Type` = media type, `Docker-Content-Digest` |
| PUT | `/v2/{o}/{r}/manifests/{ref}` | `ref` tag or digest; digest verified; on a tag, CAS the pointer; `201` + `Location` + digest |
| DELETE | `/v2/{o}/{r}/manifests/{ref}` | tag → pointer only; digest → the manifest; blobs stay until GC; `202` |
| GET/HEAD | `/v2/{o}/{r}/blobs/{digest}` | `200` (proxied; signed/307 offload deferred), single Range → `206`, `Docker-Content-Digest` |
| POST | `/v2/{o}/{r}/blobs/uploads/` | `202` + `Location: /v2/<o>/<r>/blobs/uploads/<id>` |
| PATCH | `/v2/{o}/{r}/blobs/uploads/{id}` | append a chunk; `202` + `Range` |
| PUT | `/v2/{o}/{r}/blobs/uploads/{id}?digest=` | finalize; digest mismatch → `400`, never stored; `201` |
| DELETE | `/v2/{o}/{r}/blobs/uploads/{id}` | abort; `204` |
| GET | `/v2/{o}/{r}/tags/list?n=&last=` | paged tag names + `Link` header (the one user-triggered LIST) |
| GET | `/v2/token?service=&scope=` | Docker token endpoint (§4); `/v2/auth` is an alias |
| GET | `/v2/{o}/{r}/blobs/uploads/{id}` | upload status (`204`, `Range`/`Docker-Upload-UUID`) — the resumable client's probe |

**As built:** `/v2/*` is not an owner/repo path, so it cannot ride the api or repo-subpath seams;
`internal/server/bind_top.go` adds the narrow top-level `ExtraRoutes` seam (`ChainTop`) and the
router registers `/v2` + `/v2/*` only when a surface is chained (un-wired builds keep the plain
404). `internal/packages` implements `ExtraRoutes`; composition (`cmd/spindle/packages.go`) chains
it.

Error bodies follow the spec's `{"errors":[{"code":"...","message":"...","detail":{}}]}` shape with
`UNAUTHORIZED`, `DENIED`, `NAME_UNKNOWN`, `MANIFEST_UNKNOWN`, `BLOB_UNKNOWN`, `DIGEST_INVALID`,
`BLOB_UPLOAD_UNKNOWN`, `SIZE_INVALID`.

## 4. Authentication and authorisation

Two credential paths, both over the existing identity layer (`docs/features/01_identity_permissions.md`):

1. **Basic** — `docker login` sends the principal + token on every request; the server authenticates
   them exactly as the git HTTP path does (static `server.auth.tokens`, OIDC-derived sessions, `wgt_`).
2. **Bearer** — the standard Docker flow: a `401` challenge names `realm=/v2/token`, `service=<host>`
   and the requested `scope=repository:<o>/<r>:pull,push`; the client fetches a short-lived
   (5 min) HMAC-signed token from `/v2/token`; the registry verifies it locally (no store read).

**As built:** both paths are implemented. Basic is resolved by the server's `AuthenticateForwarded`
(the git HTTP path, password = token), so static `server.auth.tokens`, `wgt_` tokens and OIDC-minted
tokens all work. `/v2/token` authenticates the Basic credential and grants only the requested actions
the principal holds (anonymous callers get `pull` for publicly readable repos and nothing else); the
token is `wgp1.<payload>.<hmac>` (HMAC-SHA256, 5 min, scopes embedded) and is verified in the handler
without a store read. The HMAC key is `server.auth.session_secret` when set; otherwise a per-process
random key is generated, so a restart invalidates outstanding registry tokens (Docker re-mints on
every operation — the practical effect is a re-login, never a broken push). Removing a principal's
access takes effect within the 5-minute token TTL, never instantly (§9).

Authorisation maps scope to the repository gates with ONE rule source: `pull` is
`identity.CheckRead`, `push` is **`identity.CheckPush`** — the git receive-pack
gate verbatim. The registry does not resolve roles on its own, so it cannot drift
from git: the repository owner (self-namespace), the owning org's owners, an
explicit binding reaching `write`, and host admins push; every other case is
denied (a host-wide write flag grants nothing, the #347 rule). `/v2/token` grants
`push` in a token's scope only when `CheckPush` passes, so a re-minted token
follows the same rule, and a pull-scoped token still cannot push. Anonymous `pull`
is allowed only when the repository is publicly readable under the same
`anonymous_read` rule the web and API use — the registry has no privilege of its
own (the page-gate parity lesson). Auth mode `none` permits everything, with the
same loud first-run warning; see §9 for why that is sharper here than for git.

## 5. Uploads and budgets (the cost model)

A `docker push` of a three-layer image must behave like a git push (law 6):

| Step | Requests |
|---|---|
| start upload | 1 POST per blob |
| send blob | 1 PATCH (single chunk when ≤ chunk size) + 1 PUT to finalize |
| manifest | 1 PUT |
| tag | part of the manifest PUT (one CAS) |

- Blob uploads are **parallel across layers** — the client already parallelises; the server must not
  serialise them behind a lock. No lock is held across a store call (law 3).
- **As built (measured):** `POST` costs zero store writes (the upload id embeds its creation time and
  an HMAC, so no metadata object is needed); a one-chunk `PATCH` is one staging write; `PUT` finalize
  spools the staging bytes once (hash while spooling), then writes the content-addressed blob — so a
  push of N layers performs **2N blob writes + 1 manifest write**, plus the tag pointer CAS (2N+2
  `Put` calls for a tag push), **N+1 reads** (one tag-pointer probe + one staging read per blob) and
  **zero LISTs**. The digest is only known at finalize, so the verify pass is the single staging read;
  no blob is ever written before the digest matches. `TestBudgetNoList` asserts these counts.
- The happy path never re-reads a blob to verify it; the digest is checked while streaming into the
  staging object, and the finalize rename is the commit point.
- Chunk staging lives in `uploads/<id>/data`; resuming a `PATCH` continues at the last byte.
- Limits (config, validated and surfaced in `/setup`): `packages.max_blob_bytes` (default 10 GiB —
  GHCR's per-layer ceiling), `packages.max_manifest_bytes` (default 4 MiB), `packages.upload_ttl`
  (default 24h).

## 6. Deletion and garbage collection

- `DELETE` of a tag removes only the pointer; `DELETE` of a manifest digest removes the **version**:
  the manifest, the direct platform manifests an index owns (a child another stored manifest still
  references is kept), and every tag pointer aimed at any of them. An unknown tag or digest is a
  real `404 MANIFEST_UNKNOWN` — never a silent `202` (a delete that hides a typo is worse than one
  that says not-found).
- Blobs are removed by a **maintainer sweep**, never inline (no refcount, no LIST on the request
  path): the pass marks every blob referenced by any stored manifest — image `config` + `layers`,
  artifact `blobs`, `subject` — and deletes the rest, plus expired upload staging and abort
  tombstones. It runs as task kind `packages-gc` with progress packets and `(repo, kind)`
  single-flight.
- **As built:** the sweep is a `maintain.Sweeper` plugged into the maintain loop's task path
  (`internal/maintain/units.go`), so it runs after the §4 priority ladder through the same
  `Tasks().Run(Kind: "packages-gc")` — a manual start (`POST …/api/ops/packages-gc`, SSE-attachable
  and listed in the ops table) joins the running pass instead of racing it, and the unit cap bounds
  it (a cancelled pass is safe: every pass is idempotent). A per-repo due gate (`DefaultGCInterval`
  15 min, in memory, asked before the task is opened) keeps an idle registry off the bucket and out
  of the task table; a restart only repeats a pass. A
  just-finalized blob is kept for `DefaultBlobGrace` (10 min) because its manifest PUT is still in
  flight — the one race the marker cannot see. A delete-time tag scan is the only other LIST, and it
  is bounded by the repo's tag count.
- A tag that is the target of a release is unaffected: releases point at git tags, not registry tags.

## 7. Surfaces

- **Page**: `/{o}/{r}/packages` — one package per repository, listing tags (name, digest, size,
  updated, platforms for an index), a copyable `docker pull` command (per tag and for the package),
  a per-version manifest table, and delete actions behind the typed confirmation. Linked from the
  repository tab strip as **Packages** when `features.packages` is on.
  **As built:** `internal/webui/packages.go` + `templates/pages/packages/packages.html`, rendered
  through `s.pageRepo` (the layout renders the chrome from `PageData.Repo`); the read gate is the
  repo visibility gate, the write gate is `packages.Service.CanPush` (the git rule, the SAME call the
  API makes), and the feature toggle off ⇒ the page and both deletes answer 404. The tab sits after
  Tags; `repoTabSection` maps `packages` to it. The table cards are `relative overflow-x-auto`:
  a static scroll container inside the page's grid let Chrome count the table's `min-width` in the
  DOCUMENT scroll area, which broke the 390 px check even though the table scrolled in place.
- **Feature toggle**: `features.packages` in repository settings; off ⇒ the tab disappears and every
  `/v2/<o>/<r>/*` route answers `404` (fail closed, like the other feature flags), as do the page
  and the JSON API.
- **JSON API** (both lanes, plain-text errors, `[]` never null):
  - `GET /{o}/{r}/api/packages[?n=]` → `{repository, packages:[{tag,digest,size,media_type,updated_at,platforms,pull}],
    manifests:[{digest,media_type,size,updated_at,tags,platforms}], total_size, more}`
  - `GET …/api/packages/{digest}` → one manifest view with its tag set and parsed `manifest` body
  - `DELETE …/api/packages/tags/{tag}` → `204`; unknown tag `404`
  - `DELETE …/api/packages/{digest}` → `204` (the version tree, as in §6); unknown `404`
  Gates are the registry's: read = `CheckRead`, delete = `CheckPush`; feature off / unknown repo ⇒
  `404`. Discovery registers the three templates (Forgejo #272).
- **SDK**: `web/sdk/src/packages.js` (`list`, `get`, `deleteTag`, `deleteManifest`), attached as
  `repo.packages` by `core.js`, Node-tested in `web/test/unit/sdk-packages.test.js`.
- **CLI**: `spindle packages list|show|delete` now call the served endpoints; `--tag` deletes a tag
  without a digest positional. A 404 is a real not-found (exit 3), not the old "feature not enabled"
  rewrite.
- **Pushed `docker pull` host**: derived from `server.public_url` when set, else the request host.

## 8. Tests (definition of done)

- **Protocol conformance subset** (httptest, no Docker required): ping + challenge; push and pull by
  tag and by digest; a multi-arch index round-trip; digest mismatch rejected and not stored; resumed
  chunked upload; tag CAS race; `tags/list` paging with `n`/`last`; delete tag/manifest; error codes.
- **Auth matrix**: anonymous pull of a public repo, anonymous push denied, `read` can pull but not
  push, `write` can push, admin can delete, feature-off ⇒ 404.
- **GC**: a manifest shared by two tags keeps its blobs when one tag is deleted; the sweep removes
  only unreferenced blobs and expired uploads; a second run is a no-op.
- **Budgets**: push of an N-layer image performs 2N + 1 writes and no LIST; a pull performs 1 tag
  read + 1 manifest read + N blob reads.
- **End to end**: when a Docker/Podman binary is present, the e2e suite pushes and pulls a real image
  (skip, not fail, when absent — the same rule the git e2e uses for a missing `git replay`).
- **Rendered**: the Packages page at desktop and ~390 px, no page-level horizontal scroll; delete
  flow behind the typed confirmation.

**As built (WP1 + second pass):** `internal/packages` passes the protocol subset (ping/challenge,
Blob/manifest push+pull by tag and digest, multi-arch index round-trip, digest mismatch never stored,
resumed chunked upload with `416` gap detection, tag CAS race, `tags/list` paging, tag/manifest
delete, error codes, Range reads), the auth matrix (anonymous public pull, anonymous push denied,
read-pull-only, write-push, admin-delete, feature-off 404, pull-token-cannot-push), the upload TTL,
and the budget assertions in `TestBudgetNoList`; `TestDockerPushPull` pushes and pulls a real local
image through a live Docker daemon (skips when docker or a local image is absent).

The second-pass tests cover the audit findings end to end: `TestCanPushReusesGitOwnerRule` and
`TestTokenEndpointGrantsOwnerPush` run against the REAL `identity.Service` (owner with no
`access.json` pushes; read binding, host-write-only and anonymous do not), `TestDeleteByDigestDropsTags`
and `TestDeleteIndexVersionOwnsItsChildren` pin the delete semantics (version tree, shared child
kept, dangling tags dropped), `TestGCReclaimsDeletedImage` / `TestGCSharedBlobSurvives` /
`TestGCUploadsAndTombstones` / `TestGCBlobGraceKeepsFreshBlobs` / `TestGCSweepDueAndErrors` pin the
sweep (mark/sweep, shared blob, staging + tombstones, grace, idempotent second pass, due gate), the
API matrix lives in `TestPackagesAPISurface` (+ `TestExposedCoversAPIRoutes` for discovery), and the
maintainer seam in `internal/maintain/sweepers_test.go`. The page, tab, feature-off 404 and delete
gates are in `internal/webui/packages_test.go`; the SDK group in
`web/test/unit/sdk-packages.test.js`; the CLI against a fake endpoint in
`cmd/spindle/cli_commands_test.go`. Package coverage ≥ 95% (`internal/packages` 96%). The rendered
check (1440 + 390 px, no page-level scroll) is recorded in `docs/AUDIT-V1.md`.

## 9. Risks

- **GC correctness** is the sharp edge: a wrong sweep deletes bytes a live manifest references. The
  mark phase must read every manifest under the prefix; the delete phase must be idempotent. The two
  known gaps are documented rather than hidden: a blob whose manifest PUT is slow can only be
  protected by the in-memory grace window, and a manifest pushed by digest and never referenced by a
  tag is never reclaimed automatically (delete it explicitly).
- **Chunked-upload state** is the only mutable scratch in the bucket; TTL + maintainer cleanup keeps
  it from accumulating when clients vanish.
- **Auth caching**: bearer tokens are verified locally (HMAC) and short-lived, so removing a user's
  access takes effect within the token TTL, not instantly. Document that window; keep it ≤ 5 min.
- **`auth.mode = none` makes the registry anonymously writable**, exactly like git. It is the
  zero-config mode's deliberate trade-off, but an image layer is opaque to review, so an exposed
  instance should run `token` (or `oidc`) — the setup page's `packages` card says so.

## 10. Decisions & deviations from the Rust design

- **NEW (2026-11-20) — packages is a spindle-only extension.** The Rust implementation (walgit) has no
  registry, so this is a deliberate divergence from the "byte-compatible bucket" law: it adds a new
  prefix (`repos/<o>/<r>/packages/`) and never changes an existing key, field number or file shape.
  Recorded here and in `DEVIATIONS.md`; the compatibility law is untouched for every pre-existing key.
- **NEW — the package name is the repository name.** GHCR allows arbitrary package names per owner
  with repo links; spindle binds one package to one repository so permissions need no second mapping.
- **NEW — blobs are content-addressed store objects, manifests are blobs.** No sidecar metadata files
  for manifests; the media type is in the manifest itself, and the digest is the path.
- **NEW — tags are CAS pointers, and there is no tag index.** `tags/list` pages the prefix; GC sweeps
  marks instead of refcounting.

### As-built amendments (WP1 skeleton, 2026-11-20)

- **The top-level route seam is new.** `/v2/*` is neither an api lane nor an `{owner}/{repo}` sub-path,
  so `internal/server/bind_top.go` adds `ChainTop` (an `ExtraRoutes` slice consulted from `/v2` and
  `/v2/*` before the catch-all). The registry package still never imports upward (law 8); the seam is
  registered in the router only when a surface is chained.
- **Upload ids carry their own TTL proof.** `uploads/<id>/data` has no metadata sidecar; the id is
  `<unix-hex>-<nonce>-<hmac>` and the handler rejects a signature or TTL violation as
  `BLOB_UPLOAD_UNKNOWN`. `POST …/uploads/` is therefore **zero writes** (the §5 2N budget), and an
  abort writes a zero-byte tombstone (`uploads/<id>/aborted`) so a deleted upload can never be
  resumed or finalized. The tombstone is staging, not state: nothing outside the bucket survives a
  restart.
- **Chunk appends use `Compose`.** The first `PATCH` writes the staging object; later chunks write a
  temporary part and `Compose` it onto `data` (all three backends support it). Resumed `PATCH`
  continues at the last byte; a `Content-Range` whose start does not equal the staged size is `416`.
- **Digest verification is one staging read at finalize** (the digest is only known then): the bytes
  are streamed to the local spool while hashing, and only a match is written to the content-addressed
  blob — a mismatch is `400 DIGEST_INVALID` with no blob object ever created. The spool lives under
  `cache.dir` (`package-spool`) with the OS temp dir as fallback.
- **Bearer tokens are implemented**, not deferred: `/v2/token` (alias `/v2/auth`) issues
  `wgp1.` HMAC tokens with embedded scopes; the registry verifies them locally and a token can never
  exceed its granted scope (a pull token cannot push even for a writer). Basic remains the primary
  credential and is resolved by the server chain exactly like git HTTP.
- **Manifest existence checks are omitted on PUT.** A manifest referencing blobs that were never
  uploaded is accepted (the client's own push order guarantees them); an unpopulated blob then 404s
  on pull. This keeps the push path free of N extra HEADs. Deferred with the GC work (see below).
- **Range reads are proxied**, one range per request; `307`/signed-URL offload (`store.AccelTarget`)
  is deferred. `GET`/`HEAD` carry `Docker-Content-Digest`, and `HEAD` carries `Content-Length`.
- **Deferred to the next pass, explicitly:** the Packages page + tab (§7), the JSON API and SDK, the
  `packages-gc` maintainer task (blob mark/sweep + expired-upload and tombstone cleanup — the TTL is
  already enforced at request time), cross-repository blob mounts, the referrers API, and
  quota/scanning/signing policy (all §1 non-goals).
- **The feature toggle defaults on.** `[features] packages` joins the six Forgejo-#522 flags: absent
  means enabled, explicit `false` makes every `/v2/<o>/<r>/*` route 404. `ResolvedFeatures.ETagBits`
  grew from six to seven chars (the summary cache must bust on a registry toggle — the #235/#505
  suffix discipline).

### As-built amendments (second pass, 2026-11-21)

- **Push authorisation is the git gate, not a copy of it.** `packages.RoleService` gained
  `CheckPush` and `CanPush` calls it verbatim; the previous local role ladder (and its read
  shortcut for the host write flag) is gone. This closes `docs/AUDIT-V1.md` finding 1: an owner with
  no `access.json` now `docker push`es exactly where they `git push`. `CanPull` likewise dropped the
  host-write shortcut, matching `CheckRead`.
- **A digest delete deletes the version tree.** The manifest, its direct platform manifests (an index
  names them), and every tag pointer aimed at any of them; a child another stored manifest still
  references is kept. Rationale: without it, deleting a multi-arch version left untagged child
  manifests whose blobs the conservative mark phase could never reclaim. Deeper nesting is not
  produced by any client and is out of scope.
- **Unknown references on DELETE are 404s.** Both the registry (`MANIFEST_UNKNOWN`) and the JSON API
  answer not-found; idempotent-silent success was hiding typos (audit secondary finding).
- **The sweep is a maintainer plug, not a priority row.** `maintain.Options.Sweepers` runs after the
  §4 ladder through the same task path; adding an eighth priority row would have re-cut the frozen
  plan. Two caches keep it cheap: a per-repo due gate (15 min) and a finalize grace (10 min) that
  protects a blob whose manifest PUT is in flight. Both are memory-only — a restart repeats work,
  never loses a repo.
- **`packages.max_blob_bytes` default is 10 GiB.** A registry that rejects a layer GHCR accepts is a
  parity bug; the cap remains configurable.
- **Registry webhooks/events stay deferred.** The events bridge is WAL-driven and registry writes are
  bucket objects, so a `registry_package`-shaped event would need a new sink and a new read path
  purely for fan-out; the audit's product gaps (page, API, GC) were the ones users hit. A caller who
  needs a reaction can poll `GET …/api/packages` (ETag-able) or run the push through CI.
- **`auth.mode = none` is documented loudly** in the setup page's `packages` card and in §9: the
  registry is anonymously writable in that mode, which is consistent with the mode but sharper than
  git (an image layer is opaque to review).
