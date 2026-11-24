import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";
const SHA = "0123456789abcdef0123456789abcdef01234567";

/** Feature F: the browser editor's one-file-per-commit write path. */
const SURFACE = [
  {
    name: "contents.put",
    run: (c) => c.repo("o/r").contents.put("main", "docs/a.md", { message: "m", content: "aGk=", expected_blob_sha: SHA }),
    method: "PUT",
    path: "/o/r/api/contents?ref=main&path=docs%2Fa.md",
  },
  {
    name: "contents.create",
    run: (c) => c.repo("o/r").contents.create("main", "new.md", { message: "m", content: "aGk=" }),
    method: "POST",
    path: "/o/r/api/contents?ref=main&path=new.md",
  },
  {
    name: "contents.remove",
    run: (c) => c.repo("o/r").contents.remove("main", "docs/a.md", { message: "rm", expected_blob_sha: SHA }),
    method: "DELETE",
    path: "/o/r/api/contents?ref=main&path=docs%2Fa.md",
  },
];

test("contents surface: verbs hit their exact endpoint and method", async () => {
  for (const row of SURFACE) {
    const { fetch, calls } = fakeFetch((ctx) =>
      ctx.init.method === row.method ? jsonResponse({ ok: true }) : new Response("bad method", { status: 405 })
    );
    const client = new ReposClient({ base: BASE, fetch, token: "t" });
    await row.run(client);
    assert.equal(calls.length, 1, row.name);
    assert.equal(calls[0].url, `${BASE}${row.path}`, row.name);
    assert.equal(calls[0].init.method, row.method, row.name);
  }
});

test("contents.put sends the JSON envelope verbatim", async () => {
  const { fetch, calls } = fakeFetch(() =>
    jsonResponse({ commit: { sha: "a".repeat(40) }, branch: "main", pr: { num: 4 } })
  );
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const res = await client.repo("o/r").contents.put("main", "a.md", {
    message: "Edit a",
    description: "why",
    content: "aGk=",
    expected_blob_sha: SHA,
    new_branch: "fix/a",
    open_pr: true,
  });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.message, "Edit a");
  assert.equal(sent.description, "why");
  assert.equal(sent.expected_blob_sha, SHA);
  assert.equal(sent.new_branch, "fix/a");
  assert.equal(sent.open_pr, true);
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  assert.equal(res.pr.num, 4);
});

test("contents.create omits no field by accident and remove requires the blob", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ commit: { sha: "b".repeat(40) }, branch: "main" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").contents.create("main", "new.md", { message: "New", content: "" });
  let sent = JSON.parse(calls[0].init.body);
  assert.equal("expected_blob_sha" in sent, false);
  await client.repo("o/r").contents.remove("main", "new.md", { message: "rm" });
  sent = JSON.parse(calls[1].init.body);
  assert.equal(sent.message, "rm");
  assert.equal("expected_blob_sha" in sent, false, "the server rejects a delete without it; the SDK does not invent one");
});
