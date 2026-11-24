/**
 * Contents client group (Feature F, docs/features/17_file_edit.md): the
 * browser file editor's write path — create/edit/delete ONE text file as ONE
 * commit, published through the same funnel a push uses. Repo-scoped calls
 * ride `/{o}/{r}/api/contents` (both lanes via the browser-lane rewrite) with
 * `?ref=&path=`.
 *
 * The server applies the push gate, policy, and the guardrails (binary/LFS/
 * size), and returns 409 naming both shas when the branch or blob moved. The
 * SDK never retries a conflicting write blindly: the caller re-reads and
 * re-submits with the fresh `expected_*` values.
 */

/**
 * Attach the contents surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachContents(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });
  const query = (ref, path) => {
    const q = new URLSearchParams();
    if (ref) q.set("ref", ref);
    if (path) q.set("path", path);
    const s = q.toString();
    return s ? `?${s}` : "";
  };

  repo.contents = {
    /**
     * Create or edit a file: `PUT …/contents` with
     * `{message, description?, content: base64, expected_blob_sha?,
     * expected_branch_sha?, new_branch?, open_pr?, pr_title?, pr_body?}`.
     * `expected_blob_sha` is required when the path exists; an absent one on
     * an existing path is a 409, never a blind overwrite.
     * 201 `{commit:{sha}, branch, pr?:{num}}`.
     */
    put: (ref, path, fields = {}, opts) =>
      client._call(p("/contents") + query(ref, path), {
        method: "PUT",
        ...json(fields),
        ...opts,
      }),
    /**
     * Create a new file only: `POST …/contents` (same body, no
     * `expected_blob_sha`). 409 when the path already exists.
     */
    create: (ref, path, fields = {}, opts) =>
      client._call(p("/contents") + query(ref, path), {
        method: "POST",
        ...json(fields),
        ...opts,
      }),
    /**
     * Delete a file: `DELETE …/contents` with
     * `{message, expected_blob_sha (required), expected_branch_sha?}`.
     * 200 `{commit:{sha}, branch}`.
     */
    remove: (ref, path, fields = {}, opts) =>
      client._call(p("/contents") + query(ref, path), {
        method: "DELETE",
        ...json(fields),
        ...opts,
      }),
  };
}
