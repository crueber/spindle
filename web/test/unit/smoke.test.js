// web/test/unit/smoke.test.js — OPT-IN fetch smoke against a RUNNING spindle
// (server-rendered pages since D-WEB-14): the base layout shell, the /_ui/
// page assets and their serving contracts, and the /repos.js SDK bundle.
// This tier never assumes an ambient server: only SPINDLE_TEST_WEB_BASE_URL
// names one. Unset → every case skips (the CI web job runs node --test with no
// server, so `make test`/CI stay green on a cold machine). Set but unreachable
// or setup-only → skip with the reason. Reachable → asserted strictly.
//
// The asserted markup is what internal/webui actually emits, not a guess:
//   layout     internal/webui/templates/base.html ("base")
//   landing    internal/webui/templates/pages/public/landing.html
//   setup      internal/webui/templates/pages/settings/setup.html
//   assets     internal/server/health.go (serveUIAssets, uiAsset, sdkReposJS)
//   page MIME  internal/webui/render.go (text/html; charset=utf-8, no-cache)

import test from "node:test";
import assert from "node:assert/strict";

const RAW = (process.env.SPINDLE_TEST_WEB_BASE_URL ?? "").trim();
const BASE = RAW ? RAW.replace(/\/+$/, "") : "";

// probe: decide whether the whole file's cases can run, and why not.
async function probe() {
  if (!BASE) {
    return "SPINDLE_TEST_WEB_BASE_URL is not set — the fetch smoke is opt-in; start a server and name its base URL";
  }
  let res;
  try {
    res = await fetch(`${BASE}/healthz`, { signal: AbortSignal.timeout(2000) });
  } catch (err) {
    return `SPINDLE_TEST_WEB_BASE_URL=${BASE} is unreachable: ${err.message}`;
  }
  if (!res.ok) return `SPINDLE_TEST_WEB_BASE_URL=${BASE} /healthz answered ${res.status}`;
  let home;
  try {
    home = await fetch(`${BASE}/`, { signal: AbortSignal.timeout(5000) });
  } catch (err) {
    return `SPINDLE_TEST_WEB_BASE_URL=${BASE} is unreachable at /: ${err.message}`;
  }
  // Setup-only mode (§3.4) answers 503 everywhere but /setup and health.
  if (home.status === 503) {
    return `server at ${BASE} is in setup-only mode (/ answered 503 — fix the config, restart, or point the smoke at another instance)`;
  }
  return "";
}

// node:test treats ANY string under `skip` as a skip (even ""), so collapse
// the empty reason to false — only a real reason skips.
const skip = (await probe()) || false;

// assertShell: the exact layout base.html emits around every page.
function assertShell(html, label) {
  assert.match(html, /^<!doctype html>/, `${label}: base.html opens with the doctype`);
  assert.match(html, /<html lang="en" class="dark">/, `${label}: dark is the default theme (base.html)`);
  assert.match(html, /<link rel="stylesheet" href="\/_ui\/app\.css">/, `${label}: stylesheet`);
  assert.match(html, /<script src="\/_ui\/htmx\.min\.js" defer><\/script>/, `${label}: vendored htmx is a classic script`);
  assert.match(html, /<script type="module" src="\/_ui\/ui\.js"><\/script>/, `${label}: island entry is a module script`);
  assert.match(html, /<main id="app"/, `${label}: the layout mounts pages in <main id="app">`);
  assert.match(html, /<a href="\/" class="brand/, `${label}: site header renders server-side`);
  assert.match(html, /the bucket is the repository/, `${label}: footer renders server-side`);
  assert.doesNotMatch(html, /id="root"/, `${label}: the Solid SPA root is gone (D-WEB-14)`);
}

test("GET / is a server-rendered page with the layout shell and landing content", { skip }, async () => {
  const res = await fetch(`${BASE}/`);
  assert.equal(res.status, 200, "/ status (a 401 means the smoke target requires auth — point it at an auth-none instance)");
  assert.equal(res.headers.get("content-type"), "text/html; charset=utf-8", "render.go sets the page MIME");
  assert.match(res.headers.get("cache-control") ?? "", /no-cache/, "pages are never shared-cached (render.go)");
  const html = await res.text();
  assertShell(html, "/");
  assert.match(
    html,
    /spindle — a git host whose only database is an object store\./,
    "/ renders landing.html's headline (server-side template, not an empty shell)",
  );
});

test("GET /setup is a server-rendered page with the layout shell and the setup form", { skip }, async () => {
  const res = await fetch(`${BASE}/setup`);
  assert.equal(res.status, 200, "/setup status (the denied card is still a 200 page)");
  assert.equal(res.headers.get("content-type"), "text/html; charset=utf-8");
  const html = await res.text();
  assertShell(html, "/setup");
  // setup.html renders this wrapper in both the form and the access-restricted card.
  assert.match(html, /id="setup-page"/, "/setup renders settings/setup.html");
});

test("/_ui/ page assets are served as their real types, no-cache, with the built content", { skip }, async () => {
  const cases = [
    { path: "/_ui/app.css", type: "text/css; charset=utf-8" },
    { path: "/_ui/ui.js", type: "text/javascript; charset=utf-8" },
    { path: "/_ui/htmx.min.js", type: "text/javascript; charset=utf-8" },
  ];
  for (const c of cases) {
    const res = await fetch(`${BASE}${c.path}`);
    assert.equal(res.status, 200, `${c.path} status (is the web built?)`);
    assert.equal(res.headers.get("content-type"), c.type, `${c.path} content-type (serveUIAssets)`);
    assert.equal(res.headers.get("cache-control"), "no-cache", `${c.path}: stable names are no-cache, never immutable`);
  }
  // The stylesheet must be Tailwind output over the templates (the same two
  // markers the Makefile/Dockerfile build check uses; an empty scan ships CSS
  // with components but no utilities).
  const css = await (await fetch(`${BASE}/_ui/app.css`)).text();
  assert.match(css, /max-w-6xl/, "app.css carries utilities the templates use");
  assert.match(css, /\.flex\{/, "app.css is not an empty Tailwind scan");
  // Vendored htmx is the real library, not a stub.
  const htmx = await (await fetch(`${BASE}/_ui/htmx.min.js`)).text();
  assert.match(htmx, /var htmx=/, "htmx.min.js is the vendored classic build");
  // The SPA shell must stay gone: index.html is not in the /_ui/ asset lane.
  const gone = await fetch(`${BASE}/_ui/index.html`);
  assert.equal(gone.status, 404, "/_ui/index.html is not an asset any more (uiAsset)");
});

test("/repos.js is the dependency-free SDK ES module with a strong ETag", { skip }, async () => {
  const res = await fetch(`${BASE}/repos.js`);
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("content-type"), "text/javascript; charset=utf-8", "module MIME is load-bearing");
  assert.equal(res.headers.get("cache-control"), "no-cache", "sdkReposJS: no-cache + ETag");
  const etag = res.headers.get("etag");
  assert.match(etag ?? "", /^"sdk-[0-9a-f]+"$/, "strong sdk ETag");
  const body = await res.text();
  assert.match(body, /ReposClient/, "bundle contains the SDK client");
  assert.match(body, /export/, "bundle is an ES module");
  // A conditional revalidation must answer 304 with the same token.
  const again = await fetch(`${BASE}/repos.js`, { headers: { "If-None-Match": etag } });
  assert.equal(again.status, 304, "conditional GET answers 304 (sdkReposJS)");
});
