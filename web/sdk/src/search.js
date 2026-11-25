/**
 * Code-search client group (docs/features/20_search.md §6; 07_api.md §9.13).
 *
 * `repo.search(query, opts)` hits `GET /{o}/{r}/api/search`; the instance-level
 * `client.search(owner, query, opts)` hits `GET /api/v1/search` (owner scope is
 * required in v1 — there is no instance-wide code search). Both return the
 * frozen SearchEnvelope unchanged; the SDK adds no envelope and no ranking.
 *
 * The server's answer is a completeness contract: `code.complete`,
 * `results[].status` and `results[].truncated`/`timed_out` are fields, so a
 * caller must render them rather than presenting an empty list as "no match".
 */

/**
 * Attach the repository-scope search onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachSearch(repo) {
  const client = repo.client;
  /**
   * @param {string} query the text or POSIX ERE to find
   * @param {{ref?: string, regex?: boolean, case?: boolean, word?: boolean,
   *          path?: string|string[], limit?: number, signal?: AbortSignal}} [opts]
   * @returns {Promise<Object>} the SearchEnvelope (07_api.md §9.13)
   */
  repo.search = (query, opts = {}) =>
    client._call(repo._path(`/search${searchQuery(query, opts)}`), {
      method: "GET",
      cache: "no-store",
      ...opts,
    });
}

/**
 * Attach the owner-scope search onto the client itself (it does not depend on
 * a repository existing yet).
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachSearchTop(client) {
  /**
   * @param {string} owner the repository owner to search across
   * @param {string} query the text or POSIX ERE to find
   * @param {{regex?: boolean, case?: boolean, word?: boolean,
   *          path?: string|string[], limit?: number, signal?: AbortSignal}} [opts]
   * @returns {Promise<Object>} the SearchEnvelope with `scope.kind === "owner"`
   */
  client.search = (owner, query, opts = {}) =>
    client._call(`/api/v1/search${searchQuery(query, { ...opts, owner })}`, {
      method: "GET",
      cache: "no-store",
      ...opts,
    });
}

/** Query string for one search request; skips absent knobs, never sends false as "0". */
function searchQuery(query, opts = {}) {
  const pairs = [];
  const add = (k, v) => pairs.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  if (query !== undefined && query !== null && query !== "") add("q", query);
  if (opts.owner) add("owner", opts.owner);
  if (opts.ref) add("ref", opts.ref);
  if (opts.regex) add("regex", "1");
  if (opts.case) add("case", "1");
  if (opts.word) add("word", "1");
  for (const p of pathsOf(opts.path)) add("path", p);
  if (opts.limit !== undefined && opts.limit !== null && opts.limit !== "" && Number(opts.limit) > 0) {
    add("limit", opts.limit);
  }
  return pairs.length ? `?${pairs.join("&")}` : "";
}

/** Normalize `path` (a string or an array) to an array of strings. */
function pathsOf(path) {
  if (path === undefined || path === null || path === "") return [];
  const list = Array.isArray(path) ? path : [path];
  return list.filter((p) => p !== undefined && p !== null && p !== "");
}
