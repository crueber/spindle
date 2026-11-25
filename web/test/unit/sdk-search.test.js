import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { ReposError } from "../../sdk/src/errors.js";
import { fakeFetch, jsonResponse, textResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

const ENVELOPE = {
  query: "func main",
  type: "code",
  options: { regex: false, case_sensitive: false, word: false, paths: [], limit: 100 },
  scope: { kind: "repo", owner: "o", repo: "r" },
  code: { complete: true, repos_considered: 1, repos_searched: 1, results: [] },
};

/** Bearer lane: paths stay as written (the browser rewrite has its own test). */
function bearerClient(handler) {
  const { fetch, calls } = fakeFetch(handler);
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  return { client, calls };
}

test("repo.search builds the frozen query and returns the envelope", async () => {
  const { client, calls } = bearerClient(() => jsonResponse(ENVELOPE));
  const ans = await client.repo("o/r").search("func main", {
    ref: "dev",
    regex: true,
    case: true,
    word: true,
    path: ["cmd/", "web/"],
    limit: 50,
  });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].init.method, "GET");
  assert.equal(
    calls[0].url,
    `${BASE}/o/r/api/search?q=func%20main&ref=dev&regex=1&case=1&word=1&path=cmd%2F&path=web%2F&limit=50`
  );
  assert.equal(calls[0].init.cache, "no-store");
  // The SDK adds no envelope: the body is the server document, verbatim.
  assert.deepEqual(ans, ENVELOPE);
});

test("repo.search defaults omit false switches and absent knobs", async () => {
  const { client, calls } = bearerClient(() => jsonResponse(ENVELOPE));
  await client.repo("o/r").search("x");
  assert.equal(calls[0].url, `${BASE}/o/r/api/search?q=x`);
  assert.ok(!calls[0].url.includes("regex=0"));
  assert.ok(!calls[0].url.includes("limit="));
});

test("repo.search accepts a single path string and a leading-dash query as data", async () => {
  const { client, calls } = bearerClient(() => jsonResponse(ENVELOPE));
  await client.repo("o/r").search("-flag", { path: "src/" });
  assert.equal(calls[0].url, `${BASE}/o/r/api/search?q=-flag&path=src%2F`);
});

test("client.search builds the owner-scope request", async () => {
  const { client, calls } = bearerClient(() =>
    jsonResponse({ ...ENVELOPE, scope: { kind: "owner", owner: "acme" } })
  );
  const ans = await client.search("acme", "func", { regex: true, limit: 10 });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].init.method, "GET");
  assert.equal(
    calls[0].url,
    `${BASE}/api/v1/search?q=func&owner=acme&regex=1&limit=10`
  );
  assert.equal(calls[0].init.cache, "no-store");
  assert.equal(ans.scope.kind, "owner");
});

test("browser lane rewrites the repo route but not the owner route", async () => {
  // No token and no page origin → browser lane (lane selection §1.2).
  const browser = (handler) => {
    const { fetch, calls } = fakeFetch(handler);
    return { client: new ReposClient({ base: BASE, fetch }), calls };
  };
  const repoRun = browser(() => jsonResponse(ENVELOPE));
  await repoRun.client.repo("o/r").search("x");
  assert.equal(repoRun.calls[0].url, `${BASE}/o/r/api-browser/search?q=x`);

  const ownerRun = browser(() => jsonResponse(ENVELOPE));
  await ownerRun.client.search("o", "x");
  assert.equal(ownerRun.calls[0].url, `${BASE}/api/v1/search?q=x&owner=o`);
});

test("search errors propagate as ReposError with the server body", async () => {
  const { client } = bearerClient(() => textResponse("unknown revision \"nope\"", 404));
  await assert.rejects(
    () => client.repo("o/r").search("x", { ref: "nope" }),
    (err) => {
      assert.ok(err instanceof ReposError);
      assert.equal(err.status, 404);
      assert.equal(err.message, 'unknown revision "nope"');
      return true;
    }
  );

  const cold = bearerClient(() =>
    textResponse("search is warming this repository; retry in a few seconds", 503)
  ).client;
  await assert.rejects(
    () => cold.repo("o/r").search("x"),
    (err) => err instanceof ReposError && err.status === 503
  );
});

test("search forwards a caller signal", async () => {
  const { client, calls } = bearerClient(() => jsonResponse(ENVELOPE));
  const ctrl = new AbortController();
  await client.repo("o/r").search("x", { signal: ctrl.signal });
  assert.ok(calls[0].init.signal, "the caller's signal reaches fetch");
});
