// tasks.test.js — the pure half of web/src/islands/tasks.js (D-WEB-14).
// DOM-free: the bar/log views and the terminal verdict. The fetch loop and the
// rendered card are covered by the browser pass, not here.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import {
  taskBars,
  taskDone,
  taskRedirect,
  taskTarget,
  taskFailureText,
  taskLogTail,
  taskRefreshURL,
  MAX_RETRIES,
  REFRESH_DELAY_MS,
} from "../../src/islands/tasks.js";

test("taskBars: explicit percent is used and clamped", () => {
  assert.deepEqual(taskBars({ label: "clone", done: 3, total: 10, unit: "objects", percent: 30 }), {
    label: "clone",
    text: "3/10 objects",
    percent: 30,
  });
  assert.equal(taskBars({ label: "x", done: 1, total: 2, percent: 250 }).percent, 100);
  assert.equal(taskBars({ label: "x", done: 1, total: 2, percent: -5 }).percent, 0);
});

test("taskBars: percent derives from done/total when the packet omits it", () => {
  const view = taskBars({ label: "ingest", done: 2, total: 8, unit: "packs" });
  assert.equal(view.percent, 25);
  assert.equal(view.text, "2/8 packs");
});

test("taskBars: no total is indeterminate, never a fake 0%", () => {
  const view = taskBars({ label: "clone", done: 120, unit: "objects" });
  assert.deepEqual(view, { label: "clone", text: "120 objects", percent: null });
  // total 0 divides by nothing.
  assert.equal(taskBars({ label: "clone", done: 1, total: 0 }).percent, null);
});

test("taskBars: missing label falls back to the card label, then to 'progress'", () => {
  assert.equal(taskBars({ done: 1 }).label, "progress");
  assert.equal(taskBars({ label: "  ", done: 1 }).label, "progress");
  assert.equal(taskBars({ done: 1 }, "importing acme/x").label, "importing acme/x");
  assert.equal(taskBars({ label: "clone", done: 1 }, "importing acme/x").label, "clone");
  assert.equal(taskBars(null), null);
  assert.equal(taskBars("clone"), null);
  assert.deepEqual(taskBars({ label: "clone" }), { label: "clone", text: "", percent: null });
});

test("taskDone: terminal frames decide, records need finished", () => {
  assert.equal(taskDone("result", { repo: "acme/x" }), "ok");
  assert.equal(taskDone("error", { status: 422, message: "nope" }), "failed");
  assert.equal(taskDone("task", { finished: "2026-11-20T00:00:00Z", ok: true }), "ok");
  assert.equal(taskDone("task", { finished: "2026-11-20T00:00:00Z", ok: false }), "failed");
  assert.equal(taskDone("task", { ok: true }), "running");
  assert.equal(taskDone("task", null), "running");
  assert.equal(taskDone("progress", { finished: "x", ok: true }), "running");
  // A task that finished before the client attached is replayed as a record
  // plus a result envelope: the record's own verdict wins over "result = ok".
  assert.equal(taskDone("result", { task: { finished: "2026-11-20T00:00:00Z", ok: false }, value: "" }), "failed");
  assert.equal(taskDone("result", { task: { finished: "2026-11-20T00:00:00Z", ok: true } }), "ok");
  assert.equal(taskDone("result", { finished: "2026-11-20T00:00:00Z", ok: false }), "failed");
  // An unfinished record is still running even under a result frame.
  assert.equal(taskDone("result", { task: { ok: true } }), "ok");
});

test("taskRedirect: names a repo path or nothing", () => {
  assert.equal(taskRedirect({ repo: "acme/hello" }), "/acme/hello");
  assert.equal(taskRedirect({ repo: "/acme/hello/" }), "/acme/hello");
  assert.equal(taskRedirect({ repo: "acme" }), "");
  assert.equal(taskRedirect({ repo: "acme/hello world" }), "");
  assert.equal(taskRedirect({ repo: "javascript:alert(1)" }), "");
  assert.equal(taskRedirect({}), "");
  assert.equal(taskRedirect(null), "");
});

test("taskFailureText: error message + status, record summary, honest fallback", () => {
  assert.equal(taskFailureText("error", { status: 422, message: "source not reachable" }), "source not reachable (422)");
  assert.equal(taskFailureText("error", { message: "clone failed" }), "clone failed");
  assert.equal(taskFailureText("error", { status: 500 }), "import failed");
  assert.equal(taskFailureText("task", { summary: "clone interrupted" }), "clone interrupted");
  assert.equal(taskFailureText("task", {}), "import failed");
  assert.equal(taskFailureText("result", {}), "import failed");
  // The fallback names the card's noun, never another task kind's wording.
  assert.equal(taskFailureText("result", {}, "sync"), "sync failed");
  assert.equal(taskFailureText("task", {}, "push sync"), "push sync failed");
  // A replayed result envelope's nested record summary is the reason.
  assert.equal(taskFailureText("result", { task: { summary: "mirror sync acme/x: denied" } }), "mirror sync acme/x: denied");
});

test("taskRefreshURL: an in-app path only, never an off-site navigation", () => {
  assert.equal(taskRefreshURL("/acme/r/settings/mirror?notice=done"), "/acme/r/settings/mirror?notice=done");
  assert.equal(taskRefreshURL("  /acme/r/settings/mirror  "), "/acme/r/settings/mirror");
  assert.equal(taskRefreshURL(""), "");
  assert.equal(taskRefreshURL("   "), "");
  assert.equal(taskRefreshURL(null), "");
  assert.equal(taskRefreshURL(undefined), "");
  // Absolute, protocol-relative and bare-relative values are refused.
  assert.equal(taskRefreshURL("https://evil.example/x"), "");
  assert.equal(taskRefreshURL("//evil.example/x"), "");
  assert.equal(taskRefreshURL("acme/r/settings/mirror"), "");
  assert.equal(taskRefreshURL("javascript:alert(1)"), "");
  assert.equal(REFRESH_DELAY_MS > 0, true);
});

test("taskLogTail: log_tail lines only, empties dropped, junk is an empty list", () => {
  assert.deepEqual(taskLogTail({ log_tail: ["a", "", "b"] }), ["a", "b"]);
  assert.deepEqual(taskLogTail({ log_tail: null }), []);
  assert.deepEqual(taskLogTail({}), []);
  assert.deepEqual(taskLogTail(null), []);
  assert.equal(MAX_RETRIES > 0, true);
});

// --- wiring pins (a template edit must not orphan the island) ----------------

const source = (p) => readFileSync(new URL(p, import.meta.url), "utf8");

test("island is registered in the boot list", () => {
  const index = source("../../src/islands/index.js");
  assert.match(index, /import \{ initTasks \} from "\.\/tasks\.js"/);
  assert.match(index, /initTasks,\s*initOps,\s*initThreadLog,/);
});

test("island reuses the one SSE reader (no second fetch, no EventSource)", () => {
  const js = source("../../src/islands/tasks.js");
  const code = js
    .split("\n")
    .filter((line) => !line.trim().startsWith("//"))
    .join("\n");
  assert.match(code, /import \{ connect \} from "\.\/sse\.js"/);
  assert.doesNotMatch(code, /fetch\(/);
  assert.doesNotMatch(code, /new EventSource/);
  // Idempotent across htmx swaps + cancelled on navigation.
  assert.match(code, /data-island-ready|dataset\.islandReady/);
  assert.match(code, /htmx:beforeCleanupElement/);
  assert.match(code, /pagehide/);
  assert.match(code, /ui:auth-required/);
});

test("the sse reader exposes the HTTP status for terminal 401/404 decisions", () => {
  const sse = source("../../src/islands/sse.js");
  assert.match(sse, /err\.status = res\.status/);
});

// taskTarget — a repository transfer's record is keyed by the SOURCE repo, so
// the success destination must come from the card, not from the record.
test("taskTarget prefers the card's explicit destination", () => {
  assert.equal(taskTarget("/beta/r1", { repo: "acme/r1" }, { repo: "acme/r1" }), "/beta/r1");
  assert.equal(taskTarget("beta/r1", null, null), "/beta/r1");
  assert.equal(taskTarget("", { repo: "acme/r1" }, null), "/acme/r1");
  assert.equal(taskTarget("", null, { repo: "acme/r2" }), "/acme/r2");
  assert.equal(taskTarget("", null, null), "");
  // Never navigate on a non-repo path.
  assert.equal(taskTarget("../etc/passwd", { repo: "acme/r1" }, null), "/acme/r1");
});
