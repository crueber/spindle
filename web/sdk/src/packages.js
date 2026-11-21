/**
 * Packages client group (docs/features/14 §7): the OCI registry's JSON twin —
 * list tags/digests, read one manifest, delete a tag, delete a manifest
 * version. Repo-scoped calls ride `/{o}/{r}/api/packages…` (both lanes via the
 * browser-lane rewrite). The registry wire protocol itself (docker push/pull)
 * is not an SDK surface: clients speak it with a container client.
 */

/**
 * Attach the packages surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachPackages(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  repo.packages = {
    /**
     * Tag/digest collection: `GET …/packages[?n=]` →
     * `{repository, packages:[{tag,digest,size,media_type,updated_at,platforms,pull}],
     *   manifests:[{digest,media_type,size,tags,platforms,updated_at}], total_size, more}`.
     */
    list: (query = {}, opts) =>
      client._call(p(`/packages${qs(query)}`), { method: "GET", ...opts }),
    /** One manifest by digest: `GET …/packages/{digest}` (404 when unknown). */
    get: (digest, opts) =>
      client._call(p(`/packages/${encodeURIComponent(digest)}`), { method: "GET", ...opts }),
    /** Delete a tag pointer: `DELETE …/packages/tags/{tag}` → 204 (write; unknown 404). */
    deleteTag: (tag, opts) =>
      client._call(p(`/packages/tags/${encodeURIComponent(tag)}`), { method: "DELETE", ...opts }),
    /**
     * Delete a manifest version (`DELETE …/packages/{digest}` → 204): every tag
     * aimed at the digest goes with it (GHCR semantics); blobs await the
     * maintainer's packages-gc sweep.
     */
    deleteManifest: (digest, opts) =>
      client._call(p(`/packages/${encodeURIComponent(digest)}`), { method: "DELETE", ...opts }),
  };
}

/** Query-string helper: skips null/undefined/empty; encodes key and value. */
function qs(params) {
  const parts = [];
  for (const [k, v] of Object.entries(params ?? {})) {
    if (v === undefined || v === null || v === "") continue;
    parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  }
  return parts.length ? `?${parts.join("&")}` : "";
}
