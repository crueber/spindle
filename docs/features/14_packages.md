# 14 — Packages: an OCI registry

> **Status:** WP1 skeleton + second pass landed (2026-11-20 / 2026-11-21) — the registry protocol,
> bucket storage, Basic + Bearer auth, the git-identical push gate, the feature toggle, limits, the
> Packages page/tab, the JSON API, the SDK group, the CLI and the `packages-gc` maintainer sweep are
> all in (`internal/packages`, `internal/webui/packages.go`, `web/sdk/src/packages.js`). Per-repository
> quota, a stored usage counter with reconcile, and tag retention landed 2026-11-23 (§5b).
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
pull-through caching. Quota and retention are no longer non-goals — see §5 (they shipped as stored
counters, not as a second index).

## 2. Object layout (bucket-native, append-only)

```
repos/<o>/<r>/packages/registry/
  blobs/sha256/<hex>            raw bytes, content-addressed, shared by manifests
  manifests/sha256/<hex>        manifest bytes (content type in the manifest's mediaType)
  tags/<tag>                    CAS JSON: {"digest":"sha256:...","media_type":"...","updated_at":"..."}
  uploads/<id>/data             chunked-upload staging (TTL, GC'd by the maintainer)
  usage.json                    CAS counter: bytes/blobs/manifests (quota + page, §5)

packages/usage.json             instance index (maintainer-written): per-repo bytes + total
packages/gc_state.json          instance GC last-run summary (§6)
```

Rules:

- **Blobs are content-addressed and never rewritten.** The digest is verified on finalize; identical
  bytes mean the same object (the S3 ETag-is-a-hash lesson applies — never assume a version counter).
- **Manifests are blobs with a media type.** An OCI image index (multi-arch) is stored exactly like a
  single manifest; nothing in the registry needs to interpret it beyond its size.
- **A tag is a CAS pointer.** Concurrent pushes to the same tag race on the pointer's version, not on
  the bytes; the loser retries once against the new version or returns `409`.
- **No tag index and no reference counting.** Tags list by prefix paging; GC runs as a sweep
  (§6), so no hot path ever enumerates the bucket (law 4). The ONE stored aggregate is the usage
  counter (one small CAS object per repository), maintained on put/delete and reconciled by the
  sweep — it is a number, never a second index of digests.

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

**Threads job tokens (`permissions: packages`, PAR-112).** A job's `wjt_` token is accepted at
`/v2/token` (as the Basic password, the Docker login shape) and is **exchanged** there for a normal
5-minute registry token whose scopes are capped at the job's own repository: `packages: read` grants
`pull`, `packages: write` grants `pull,push`, and a request for any other repository or for a level
the job does not declare is a real `403`. The minted token carries `job_repo` in its payload so the
registry authorizes from the embedded grant rather than resolving a repository role for the
`ci:<runner>` principal (which owns none by design); it cannot mint another token. The job token is
**never itself accepted** on `/v2/...` or the JSON package routes — the exchange is the only path, so
a `contents: write` job cannot push an image by presenting its job token directly. This is the
narrower of the two designs (a derived, repository-scoped, 5-minute credential rather than a
longer-lived job credential on every route). Each exchange is logged with the repository and the
granted scopes; the push/pull themselves revalidate the registry token locally.

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
- **As built (measured, quota work included):** `POST` costs zero store writes (the upload id embeds
  its creation time and an HMAC, so no metadata object is needed); a one-chunk `PATCH` is one staging
  write; `PUT` finalize spools the staging bytes once (hash while spooling), then writes the
  content-addressed blob and the usage counter — so a push of N layers performs **2N blob writes + 1
  manifest write + N+1 usage writes** (3N+3 `Put` calls for a tag push), **2N+3 reads** (one
  tag-pointer probe, one staging read and one counter read per blob, one counter read and one
  manifest HEAD for the manifest) and **zero LISTs**. The counter is what makes the quota enforceable
  at the commit point without a LIST; it is one CAS pair per committed object, the deliberate cost
  of the feature (docs/features/14 §10 records the budget change). The digest is only known at
  finalize, so the verify pass is the single staging read; no blob is ever written before the digest
  matches. `TestBudgetNoList` asserts these counts.
- The happy path never re-reads a blob to verify it; the digest is checked while streaming into the
  staging object, and the finalize rename is the commit point.
- Chunk staging lives in `uploads/<id>/data`; resuming a `PATCH` continues at the last byte.
- Limits (config, validated and surfaced in `/admin/config`): `packages.max_blob_bytes` (default 10 GiB —
  GHCR's per-layer ceiling), `packages.max_manifest_bytes` (default 4 MiB), `packages.upload_ttl`
  (default 24h), `packages.quota_bytes` (default `0B` = unlimited — see §5b).

## 5b. Quota, usage and retention

The registry used to grow without a bound. It now carries a per-repository byte quota, a stored usage
counter both surfaces read, and a retention policy the maintainer applies.

**Schema (the decision).** The quota is BOTH an instance default and a per-repository override, with
the host key only supplying the default:

- `[packages] quota_bytes` in `spindle.toml` (instance default; `0B` = unlimited — the compatible
  default, because an upgrade must not start refusing pushes nobody asked it to bound).
- `[packages] quota_bytes` in the repository's settings document (`RepoPackages.QuotaBytes`, a
  **pointer**: absent = inherit the instance default, explicit `0` = unlimited for this repository —
  the Forgejo #522 "unset ≠ explicit zero" discipline). Retention lives here only; there is no
  instance-wide retention default, because an instance-wide delete policy is an operator surprise.
- `[[packages.retention]]` rules, first match wins: `pattern` (a `path.Match` glob over the tag name;
  `""` or `"*"` matches every tag), `keep_last` (newest N matching tags survive) and `max_age`
  (tags whose last update is older are pruned). At least one of the two must be set; at most 16 rules;
  validated at publish (`config.ValidateRepoPackages`) so a policy that can never keep anything is a
  rejected save, never a silent no-op.

**Enforcement point.** The push COMMIT POINT is `Service.PutManifest`. It reads the counter once,
projects `usage + len(manifest)` (layer bytes are already counted, see below), and refuses an
over-quota push with `413` and a named error: code `DENIED` (the OCI spec has no QUOTA code, so the
name is in the message), text `packages quota exceeded: repo holds 12.4 MiB of 10.0 MiB quota; this
push needs 1.2 MiB more (3.6 MiB over)`, and `detail {usage, quota, requested}`. Nothing is written:
no manifest object, no tag pointer, no counter change. `FinalizeUpload` applies the same bound to a
layer, so a client cannot stream an unbounded set of layers and only fail at the end; a refused layer
writes no blob (the staging object waits for the TTL sweep). A failed counter read is a `500` — a
bound that silently disappears on a store hiccup is not a bound.

**Usage without a LIST.** `repos/<o>/<r>/packages/usage.json` is a CAS JSON
`{version, bytes, blob_bytes, manifest_bytes, blobs, manifests, updated_at}`. It is adjusted on blob
finalize (+size), on a new manifest object (+body; a re-push of identical bytes is a HEAD hit and
counts zero — S3 ETags are content hashes, never counters) and on a version delete (−the deleted
manifest bodies). A lost CAS is not data loss: the counter is not the source of truth, the sweep's
walk is. The Packages page, the JSON API (`usage` member on `GET …/api/packages`) and the quota check
read that one object (`Service.Usage` — one GET plus the policy projection), and the Maintenance
storage card reads the instance index (one GET). Neither lists. `TestUsageReadsNoList` asserts the
store method set.

The instance index `packages/usage.json` is written ONLY by an applying maintainer pass: a second
instance-level CAS on every layer upload would be a hot-path round trip nobody asked for, so the card
says "as of the last maintenance pass" instead of pretending to be live. It holds
`{version, updated_at, total_bytes, repos: {"owner/repo": bytes}}` (a zero-byte registry's row is
dropped, so the map is bounded by registries that hold objects).

**Reconciliation (repairs drift).** The sweep already lists every manifest and blob, so it computes
the true footprint (surviving manifest bytes + surviving blob bytes) and CAS-writes it as the
absolute counter, reporting `UsageDrift = computed − stored`. That is the reconciliation task; a
crash between a blob write and its counter CAS, a manual object delete, or a repo whose counter was
written by an older build all repair within one due interval (15 min). A corrupt counter reads as
zero, is logged, and is repaired by the same pass. A dry run computes the drift and writes nothing.

**Retention (the maintainer pass).** `packages-gc` (the existing kind — no new task) runs retention
before its mark phase, so the bytes a pruned version owned are reclaimed by the same pass:

1. Read every tag pointer (one LIST, one GET per tag). A pointer without a parsable `updated_at` is
   skipped: retention never prunes what it cannot date.
2. Bucket tags by their first matching rule; newest-first within a bucket; prune `index >= keep_last`
   and `updated_at <= now − max_age`.
3. Delete the chosen pointers (CAS on their version; a pointer that moved or vanished under us is
   left alone), then RE-READ the pointer set (a concurrent push may have re-tagged a digest).
4. A pruned tag's digest is a deletion candidate only when no surviving tag names it. A candidate
   whose manifest another stored manifest names as an index child is kept. Deleting a candidate
   deletes its direct platform manifests too, unless a surviving manifest or tag names them — a
   surviving tag that points straight at a platform manifest protects it.
5. **An untagged manifest is never a candidate.** Only a version whose LAST tag this pass removed can
   go; a manifest pushed by digest and never tagged stays until it is deleted explicitly (the §9
   gap, unchanged).
6. The mark phase then keeps every blob any surviving manifest names — a layer shared with a kept
   version survives because the kept manifest still names it. Nothing here can remove a referenced
   blob.

The pass reports what it pruned: the task narration names the tag count, the manifest count and the
bytes; `GCResult.Retention` carries `{tags, pruned_tags, pruned_names (≤20), manifests, bytes}`; the
instance GC summary (`packages/gc_state.json`) gains `pruned_tags`/`pruned_manifests`/`usage_drift`
(an older stored row reads zero, never a fabricated number). A dry run runs the same decision with
`apply=false` and reports what it would prune without touching a tag.

## 6. Deletion and garbage collection

- `DELETE` of a tag removes only the pointer; `DELETE` of a manifest digest removes the **version**:
  the manifest, the direct platform manifests an index owns (a child another stored manifest still
  references is kept), and every tag pointer aimed at any of them. An unknown tag or digest is a
  real `404 MANIFEST_UNKNOWN` — never a silent `202` (a delete that hides a typo is worse than one
  that says not-found).
- Blobs are removed by a **maintainer sweep**, never inline (no refcount, no LIST on the request
  path): the pass applies the repository's tag retention policy first (§5b), then marks every blob
  referenced by any remaining stored manifest — image `config` + `layers`, artifact `blobs`,
  `subject` — and deletes the rest, plus expired upload staging and abort tombstones. It runs as
  task kind `packages-gc` with progress packets and `(repo, kind)` single-flight. The same pass
  reconciles the usage counter to its own reading (§5b) and records the drift it repaired.
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
- **The instance trigger and its dry run (PLAN-ADMIN §8.6, as built 2026-11-22).** The Admin area's
  Maintenance page owns the one manual control: `POST /admin/maintenance/gc` with `op=dry-run|apply`
  starts (or joins) the SAME `packages-gc` task at **instance scope** — the reserved task-table repo
  key `instance` (`packages.InstanceScope`; a repo key is always `<owner>/<repo>`, so it cannot
  collide). The pass is `Service.SweepAll`, which enumerates registries with delimiter listings over
  `repos/` (the maintainer's LIST, never a request path) and runs the per-repository mark/sweep body
  for each, so one unreadable registry is counted and named rather than hiding the rest. `dry_run=1`
  runs the same body with `apply=false` (`Service.PlanGC`/`sweepRepo`): it counts unreferenced blobs,
  bytes and expired staging without deleting anything and without moving the per-repo due-gate clock.
  The count is a reading of the store at that instant — a push racing the pass changes it — and the
  page's copy says so. `apply` deletes and then records.
  Last-run facts live in `packages/gc_state.json` at the bucket root (overwrite JSON version 1,
  `{version:1,last:{…},last_apply:{…}}`, each row `{at, dry_run, actor, task_id, repos,
  unreferenced, bytes, uploads, ok, error}`), because the task table is instance memory and the page
  must stay honest across a restart; a lost object reads as "no pass recorded", never a fabricated
  number. Every trigger — dry run, apply, or a join of a running pass — writes one `packages_gc`
  record in the host audit family at target `instance:packages`. No new config key: the scheduled
  cadence is the existing per-repo due gate (`DefaultGCInterval`) stated on the card.
  Tests: `internal/packages/gcadmin_test.go` (plan counts without deleting, apply + record,
  idempotence, corrupt/lost summary, enumeration, start/join/audit, refusal) and
  `internal/webui/admin_gc_test.go`.

## 7. Surfaces

- **Page**: `/{o}/{r}/packages` — one package per repository, listing tags (name, digest, size,
  updated, platforms for an index), a copyable `docker pull` command (per tag and for the package),
  a per-version manifest table, and delete actions behind the typed confirmation. Linked from the
  repository tab strip as **Packages** when `features.packages` is on.
  **Usage and quota (docs/features/14 §5b, as built).** The page renders the stored counter in words
  first: `12.4 MiB of 50.0 MiB quota` plus a chip that says **within quota**, **over quota** or **no
  quota** (state is never colour-only), the remaining bytes and the blob/manifest counts, with the
  meter as `role="progressbar"` carrying `aria-valuetext`. An unlimited repository says storage is
  unbounded. A quota/retention policy is set in the repository's settings document (`[packages]` on
  the Settings → Effective config editor) — the page names the keys and links the maintainer pass
  that reclaims what retention prunes.
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
    manifests:[{digest,media_type,size,updated_at,tags,platforms}], total_size, more,
    usage:{bytes,blob_bytes,manifest_bytes,blobs,manifests,quota_bytes,updated_at}}` (the stored
    counter and the resolved quota; `quota_bytes: 0` = unlimited)
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
- **Maintenance storage card (docs/PLAN-ADMIN.md §3.5):** the Storage & cache card gained an **OCI
  registry** section: the instance total and up to five largest registries, read from the stored
  index (`packages/usage.json`) in ONE GET, with the copy stating that the numbers are as of the last
  maintainer pass and that per-repository quota/retention are set in each repository's settings
  `[packages]`. Unwired, restricted and read-error states are the honest ones (never a fake zero);
  the admin config page gains the `packages.quota_bytes` row (`50GiB` example, "0 = unlimited").
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
- **Quota, usage and retention (§5b)**: push refused at the quota (named 413, nothing written), a
  push under the quota succeeding; the counter accurate across blob put, manifest put, version delete
  and the sweep; retention pruning by count and by age, a shared manifest keeping its version, a
  referenced blob never removed, an untagged manifest never pruned by a tag rule; the reconcile
  repairing an under-reported counter and reporting the drift; no LIST on any usage read (the store
  method set); the config key's default/validation and the `[packages]` settings round-trip.
- **Rendered**: the Packages page at desktop and ~390 px, no page-level horizontal scroll; delete
  flow behind the typed confirmation; the usage/quota block in all three states (within, over,
  unlimited) at 1440×900 and ~390×844 in dark and light.

**As built (WP1 + second pass):** `internal/packages` passes the protocol subset (ping/challenge,
Blob/manifest push+pull by tag and digest, multi-arch index round-trip, digest mismatch never stored,
resumed chunked upload with `416` gap detection, tag CAS race, `tags/list` paging, tag/manifest
delete, error codes, Range reads), the auth matrix (anonymous public pull, anonymous push denied,
read-pull-only, write-push, admin-delete, feature-off 404, pull-token-cannot-push), the upload TTL,
and the budget assertions in `TestBudgetNoList`; `TestDockerPushPull` pushes and pulls a real local
image through a live Docker daemon (skips when docker or a local image is absent), asserting the
registry contract — the pulled reference's `RepoDigests` carries the manifest digest the registry
served. The image **ID** is daemon bookkeeping whose meaning depends on the image store (the classic
graphdriver reports the config digest, the containerd image store an internal ID), so it is logged,
never asserted: v0.8.1's tag CI (GitHub `ubuntu-latest`, containerd store) failed that comparison while
the same commit passed on the classic store.

The quota/retention pass's tests live in `internal/packages/quota_test.go` and
`usage_edge_test.go` (`TestQuotaRefusesManifestAtCommitPoint`, `TestQuotaRefusesOversizedLayer`,
`TestUsageCounterPutDeleteGC`, `TestInstanceUsageIndex`, `TestReconcileRepairsDrift`,
`TestRetentionKeepLastSharedManifest`, `TestRetentionKeepLastDistinctVersions`, `TestRetentionMaxAge`,
`TestRetentionBlobSharedByKeptVersion`, `TestRetentionIndexChildren`,
`TestRetentionIndexChildFollowsPrunedIndex`, `TestRetentionKeepsDirectlyTaggedChild`,
`TestUsageReadsNoList`, plus the failure/CAS/no-limit edges in `usage_edge_test.go`), the config
round-trip in `internal/config/packages_cfg_test.go`, the page's three states in
`internal/webui/packages_test.go` (`TestPackagesUsageStates`), the storage card in
`internal/webui/admin_maintenance_test.go` (`TestAdminMaintenancePackagesUsage`) and the new schema
row in `internal/webui/settings_test.go` (`TestAdminConfigPackagesQuotaRow`).

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
`cmd/spindle/cli_commands_test.go`. Package coverage ≥ 95% (`internal/packages` 95%, `internal/config`
96%). The rendered check (1440 + 390 px, no page-level scroll) is recorded in `docs/AUDIT-V1.md`.

**Quota/retention rendered proof (2026-11-23).** Verified in a real browser at desktop and ~390 px,
dark and light, against a live instance (a 5 MiB image pushed to `acme/demo`, instance quota 10 MiB):
the Packages page renders `5.0 MiB of 10.0 MiB quota` + the **within quota** chip + the
`role="progressbar"` reading (`aria-valuetext` = "5.0 MiB of 10.0 MiB quota used (50%); 5.0 MiB
free"), the remaining bytes and the blob/manifest counts; the Maintenance storage card renders
`Registry storage: 5.0 MiB · last reconciled …` with the `acme/demo` row. Measurements at 1440×900
and 390×844: `document.documentElement.scrollWidth === clientWidth` in every case (1440/1440,
390/390), one `h1`, no positive `tabindex`, the skip link first, and Escape closes the theme menu.
The unbounded and over-quota states are pinned by `TestPackagesUsageStates` (the live instance was
within quota); screenshots are working evidence under `/var/tmp/ui-spindle/`, not committed.

## 9. Risks

- **GC correctness** is the sharp edge: a wrong sweep deletes bytes a live manifest references. The
  mark phase must read every manifest under the prefix; the delete phase must be idempotent. The two
  known gaps are documented rather than hidden: a blob whose manifest PUT is slow can only be
  protected by the in-memory grace window, and a manifest pushed by digest and never referenced by a
  tag is never reclaimed automatically (delete it explicitly).
- **Chunked-upload state** is the only mutable scratch in the bucket; TTL + maintainer cleanup keeps
  it from accumulating when clients vanish.
- **The usage counter is a number, not a ledger.** A crash between an object write and its counter
  CAS, a re-push of identical bytes, or two pushes racing a quota check can make the counter drift or
  let one push overshoot before the next sweep (15 min) reconciles it. That is accepted and visible:
  the sweep reports the drift, the Packages page shows the counter, and the quota is a growth bound,
  not an admission controller for bytes. A repository can also exceed its quota by lowering the
  quota below current usage — the next push is refused and the page says "over quota"; nothing is
  deleted to enforce it (retention is the operator's explicit lever).
- **Auth caching**: bearer tokens are verified locally (HMAC) and short-lived, so removing a user's
  access takes effect within the token TTL, not instantly. Document that window; keep it ≤ 5 min.
- **The registry requires a credential to push**, exactly like `git push` (session, `wgt_` token or a
  static machine token). Anonymous callers get the Docker token challenge — `AuthRequired` is
  unconditional now that OIDC is the only authentication; there is no anonymously-writable mode.

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
- **NEW (2026-11-28, PAR-112) — a Threads job token is exchanged, never accepted directly.** A job
  declaring `packages: read|write` presents its `wjt_` token at `/v2/token` and receives the ordinary
  5-minute registry token capped at its own repository (`read` ⇒ pull, `write` ⇒ pull+push); the job
  token is refused on `/v2/*` and the JSON package routes, and the minted token carries `job_repo` so
  the registry authorizes from the embedded grant rather than a repository role for the
  `ci:<runner>` principal. A cross-repository or missing-scope request is a `403`. Chosen over
  accepting the job token as a bearer credential because the derived credential is narrower in both
  scope and lifetime, and it follows the Docker client's existing exchange.

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

The skeleton pass's deferred list is now closed: the Packages page + tab (§7), the JSON API and SDK,
and the `packages-gc` maintainer task all shipped in this pass (the audit re-ran them against a live
Docker 29 client; `docs/AUDIT-V1.md`).

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
- **The open-mode exception is deleted with `auth.mode`** (PLAN-OIDC-ONLY): the Configuration page's
  `packages` card and §9 state that pushes need the same credential as `git push`; there is no
  anonymously-writable registry.

- **NEW (2026-11-23) — quota and retention: a config default plus a settings override, and a
  maintainer-applied policy.** `packages.quota_bytes` is the instance default (`0B` = unlimited, so
  no upgrade starts refusing pushes); the repository's `[packages]` section overrides it, with a
  pointer so "unset (inherit)" and "explicit 0 (unlimited)" stay distinct. Retention is
  per-repository only (`[[packages.retention]]`, first glob match wins) and applied by the existing
  `packages-gc` pass, not a new task — the bytes a pruned version owned are reclaimed by the same
  pass that decided to drop it, and the pass already owns the only LIST. Rationale: a second task
  kind would double the task-table traffic for one cadence, and an instance-wide retention default
  would be an operator surprise that deletes data.
- **NEW — the push budget grows by the counter, deliberately.** The quota needs a number at the
  commit point without a LIST, so a blob finalize now does one counter read + one CAS write and a
  manifest put does one counter read + one manifest HEAD + one CAS write: a push is 3N+3 puts and
  2N+3 reads instead of 2N+2/1+N (still zero LISTs). The alternative — deriving usage from the
  manifests on every push — needs N HEADs and cannot dedupe shared blobs, so the counter wins.
  `TestBudgetNoList` pins the new numbers; §5 states them.
- **NEW — usage is a counter plus a maintainer reconcile, never a second index.** `usage.json` holds
  bytes and counts only (no digest ledger); the sweep's existing walk is the reconciliation, and the
  drift it repairs is reported in the task narration, `GCResult`, and the instance GC summary. A
  corrupt counter reads as zero (logged) instead of failing a pull.
- **NEW — the 413 is `DENIED`, not a new code.** The OCI Distribution spec has no QUOTA code, and a
  registry that invents one breaks clients; the message names the condition and `detail` carries
  `{usage, quota, requested}`.

- **NEW — the quota/retention objects are inside the packages prefix.** `repos/<o>/<r>/packages/usage.json`
  and `packages/{usage,gc_state}.json` are spindle-only keys under the prefix the first decision
  already records; no Rust-era key, field number or file shape changes. A repository move carries the
  per-repo counter with its registry (the prefix travels), and the instance index row is rewritten by
  the next pass under the new name (the old row drops at its next zero reading).
