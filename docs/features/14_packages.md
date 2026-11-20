# 14 — Packages: an OCI registry

> **Status:** proposed for v1 (2026-11-20). Specified here, not implemented. Supersedes the P9
> "Packages — out of scope" line in `docs/features/README.md` for the OCI case only.
> Companion: `docs/PLAN-V1.md` WP1.

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
| DELETE | `/v2/{o}/{r}/manifests/{digest}` | removes the manifest; blobs stay until GC; `202` |
| GET/HEAD | `/v2/{o}/{r}/blobs/{digest}` | `200`/`307` (store redirect where the backend supports it), Range supported, `Docker-Content-Digest` |
| POST | `/v2/{o}/{r}/blobs/uploads/` | `202` + `Location: /v2/<o>/<r>/blobs/uploads/<id>` |
| PATCH | `/v2/{o}/{r}/blobs/uploads/{id}` | append a chunk; `202` + `Range` |
| PUT | `/v2/{o}/{r}/blobs/uploads/{id}?digest=` | finalize; digest mismatch → `400`, never stored; `201` |
| DELETE | `/v2/{o}/{r}/blobs/uploads/{id}` | abort; `204` |
| GET | `/v2/{o}/{r}/tags/list?n=&last=` | paged tag names + `Link` header (the one user-triggered LIST) |
| GET | `/v2/token?service=&scope=` | Docker token endpoint (§4); `/v2/auth` is an alias |

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

Authorisation maps scope to the repository role ladder: `pull` requires `read`, `push` requires
`write`. Anonymous `pull` is allowed only when the repository is publicly readable under the same
`anonymous_read` rule the web and API use — the registry has no privilege of its own (the page-gate
parity lesson). Auth mode `none` permits everything, with the same loud first-run warning.

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
- The happy path never re-reads a blob to verify it; the digest is checked while streaming into the
  staging object, and the finalize rename is the commit point.
- Chunk staging lives in `uploads/<id>/data`; resuming a `PATCH` continues at the last byte.
- Limits (config, validated and surfaced in `/setup`): `packages.max_blob_bytes` (default 2 GiB),
  `packages.max_manifest_bytes` (default 4 MiB), `packages.upload_ttl` (default 24h).

## 6. Deletion and garbage collection

- `DELETE` of a tag removes only the pointer; `DELETE` of a manifest digest removes the manifest.
- Blobs are removed by a **maintainer sweep**, never inline (no refcount, no LIST on the request
  path): the pass marks every blob referenced by any stored manifest and deletes unreferenced ones,
  plus expired uploads. It runs as a task kind `packages-gc` with progress and single-flight.
- A tag that is the target of a release is unaffected: releases point at git tags, not registry tags.

## 7. Surfaces

- **Page**: `/{o}/{r}/packages` — one package per repository, listing tags (name, digest, size,
  updated, platforms for an index), a copyable `docker pull` command, and delete actions behind the
  danger confirmation. Linked from the repository tabs when `features.packages` is on.
- **Feature toggle**: `features.packages` in repository settings; off ⇒ the tab disappears and every
  `/v2/<o>/<r>/*` route answers `404` (fail closed, like the other feature flags).
- **JSON API**: `GET /{o}/{r}/api/packages` (tags + digests), `GET .../api/packages/{digest}`,
  `DELETE .../api/packages/tags/{tag}`, `DELETE .../api/packages/{digest}` — the same shapes the page
  renders, with the same gates.
- **SDK**: `web/sdk/src/packages.js` (`list`, `get`, `deleteTag`, `deleteManifest`), Node-tested like
  its siblings.

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

## 9. Risks

- **GC correctness** is the sharp edge: a wrong sweep deletes bytes a live manifest references. The
  mark phase must read every manifest under the prefix; the delete phase must be idempotent.
- **Chunked-upload state** is the only mutable scratch in the bucket; TTL + maintainer cleanup keeps
  it from accumulating when clients vanish.
- **Auth caching**: bearer tokens are verified locally (HMAC) and short-lived, so removing a user's
  access takes effect within the token TTL, not instantly. Document that window; keep it ≤ 5 min.

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
