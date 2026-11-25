import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

// The summary's `website` is a plain wire field (07_api.md §9.1): the SDK
// passes it through untouched, so a consumer reads `repo.get().website`
// without a second request. The RepoInfo typedef is the documented
// projection — both halves are pinned here.
test("repo summary carries website through the SDK", async () => {
  const summary = {
    owner: "demo",
    name: "hello",
    full_name: "demo/hello",
    description: "a repo",
    website: "https://example.com/docs?ref=spindle",
    head: { name: "refs/heads/main", sha: "abc123" },
    branches: 1,
    tags: 0,
  };
  const { fetch, calls } = fakeFetch(() => jsonResponse(summary));
  const c = new ReposClient({ base: BASE, fetch, token: "t" });
  const got = await c.repo("demo/hello").get();
  assert.equal(calls[0].url, `${BASE}/demo/hello/api`);
  assert.equal(got.website, "https://example.com/docs?ref=spindle");
  // Unset stays "" (the field is always present, never null).
  const { fetch: fetch2 } = fakeFetch(() => jsonResponse({ ...summary, website: "" }));
  const c2 = new ReposClient({ base: BASE, fetch: fetch2, token: "t" });
  assert.equal((await c2.repo("demo/hello").get()).website, "");
});

test("RepoInfo typedef documents website beside description", async () => {
  const src = await readFile(new URL("../../sdk/src/types.js", import.meta.url), "utf8");
  const at = src.indexOf("@typedef {{owner: string, name: string, full_name: string, description: string,");
  assert.ok(at >= 0, "RepoInfo typedef not found");
  const block = src.slice(at, src.indexOf("}} RepoInfo", at));
  assert.match(block, /website: string/, "RepoInfo must list website");
  assert.match(block, /description: string/, "RepoInfo must still list description");
});
