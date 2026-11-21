import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";
const DIGEST = `sha256:${"a".repeat(64)}`;

/** Packages surface (docs/features/14 §7): exact endpoints + methods. */
const SURFACE = [
  { name: "packages.list", run: (c) => c.repo("o/r").packages.list(), method: "GET", path: "/o/r/api/packages" },
  { name: "packages.list n", run: (c) => c.repo("o/r").packages.list({ n: 25 }), method: "GET", path: "/o/r/api/packages?n=25" },
  { name: "packages.get", run: (c) => c.repo("o/r").packages.get(DIGEST), method: "GET", path: `/o/r/api/packages/${encodeURIComponent(DIGEST)}` },
  { name: "packages.get encoded", run: (c) => c.repo("o/r").packages.get("sha256:a/b"), method: "GET", path: "/o/r/api/packages/sha256%3Aa%2Fb" },
  { name: "packages.deleteTag", run: (c) => c.repo("o/r").packages.deleteTag("v1.0"), method: "DELETE", path: "/o/r/api/packages/tags/v1.0" },
  { name: "packages.deleteTag encoded", run: (c) => c.repo("o/r").packages.deleteTag("a/b"), method: "DELETE", path: "/o/r/api/packages/tags/a%2Fb" },
  { name: "packages.deleteManifest", run: (c) => c.repo("o/r").packages.deleteManifest(DIGEST), method: "DELETE", path: `/o/r/api/packages/${encodeURIComponent(DIGEST)}` },
];

test("packages surface: every member hits its exact endpoint and method", async () => {
  for (const row of SURFACE) {
    const { fetch, calls } = fakeFetch((ctx) =>
      ctx.init.method === row.method ? jsonResponse({ ok: true }) : new Response("bad method", { status: 405 })
    );
    const client = new ReposClient({ base: BASE, fetch, token: "t" }); // bearer lane → paths unchanged
    await row.run(client);
    assert.equal(calls.length, 1, row.name);
    assert.equal(calls[0].url, `${BASE}${row.path}`, `${row.name} → ${row.method} ${row.path}`);
    assert.equal(calls[0].init.method, row.method, row.name);
  }
});

test("packages.list passes the collection body through", async () => {
  const body = {
    repository: "o/r",
    packages: [{ tag: "v1", digest: DIGEST, size: 12, media_type: "application/vnd.oci.image.manifest.v1+json", updated_at: "2026-11-21T12:00:00Z", platforms: [], pull: "docker pull h/o/r:v1" }],
    manifests: [],
    total_size: 12,
    more: false,
  };
  const { fetch } = fakeFetch(() => jsonResponse(body));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const got = await client.repo("o/r").packages.list();
  assert.deepEqual(got, body);
});

test("packages.deleteManifest maps a 404 to ReposError", async () => {
  const { fetch } = fakeFetch(() => new Response("manifest unknown", { status: 404 }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await assert.rejects(() => client.repo("o/r").packages.deleteManifest(DIGEST), (err) => {
    assert.equal(err.status, 404);
    assert.ok(err.url.endsWith(`/o/r/api/packages/${encodeURIComponent(DIGEST)}`));
    return true;
  });
});
