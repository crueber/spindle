// ops.test.js — pure helpers of the maintenance-op run island (WP9 G12).
import { test } from "node:test";
import assert from "node:assert/strict";

import { opQuery, opProgressText, opTaskLines, opResultView } from "../../src/islands/ops.js";

test("opQuery drops blanks and encodes params", () => {
  assert.equal(opQuery([]), "");
  assert.equal(
    opQuery([
      ["connectivity", "1"],
      ["slot", "  "],
      ["strategy", " daily "],
    ]),
    "connectivity=1&strategy=daily",
  );
  assert.equal(opQuery([["ref", "refs/heads/main"]]), "ref=refs%2Fheads%2Fmain");
});

test("opProgressText renders counted and percent progress", () => {
  assert.equal(opProgressText({ label: "packs", done: 2, total: 5 }), "packs — 2/5");
  assert.equal(opProgressText({ label: "packs", done: 2, unit: "packs" }), "packs — 2 packs");
  assert.equal(opProgressText({ percent: 42 }), "progress — 42%");
  assert.equal(opProgressText(null), "");
});

test("opTaskLines reports a running task and its tail", () => {
  const running = opTaskLines({ kind: "fsck", log_tail: ["a", ""] });
  assert.equal(running.title, "fsck — running");
  assert.deepEqual(running.lines, ["a"]);

  const done = opTaskLines({ kind: "compact", finished: "t", ok: true, summary: "merged 3 packs", hostname: "n1" });
  assert.equal(done.state, "ok");
  assert.deepEqual(done.lines, ["merged 3 packs", "host: n1"]);
});

test("opResultView reads the terminal verdict", () => {
  assert.deepEqual(opResultView("result", { task: { ok: true, summary: "done" } }), { state: "ok", text: "done" });
  assert.deepEqual(opResultView("result", { task: { ok: false, summary: "boom" } }), { state: "failed", text: "boom" });
  assert.deepEqual(opResultView("error", { status: 409, message: "busy" }), { state: "failed", text: "busy (409)" });
  assert.deepEqual(opResultView("result", null), { state: "ok", text: "op finished" });
});
