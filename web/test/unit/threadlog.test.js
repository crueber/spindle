// threadlog.test.js — the pure half of web/src/islands/threadlog.js (15 §6):
// sequence dedupe, newline normalization and the terminal states. The DOM
// attachment is covered by the rendered run-page check.
import test from "node:test";
import assert from "node:assert/strict";

import { formatFrame, nextSeq, isTerminal, storedText } from "../../src/islands/threadlog.js";

test("formatFrame: appends a newline once", () => {
  assert.equal(formatFrame({ seq: 1, text: "hello" }, 0), "hello\n");
  assert.equal(formatFrame({ seq: 1, text: "hello\n" }, 0), "hello\n");
});

test("formatFrame: a duplicate or older sequence is dropped", () => {
  assert.equal(formatFrame({ seq: 2, text: "again" }, 2), "");
  assert.equal(formatFrame({ seq: 1, text: "older" }, 2), "");
});

test("formatFrame: a missing frame and a missing text are harmless", () => {
  assert.equal(formatFrame(null, 0), "");
  assert.equal(formatFrame({ seq: 3 }, 0), "\n");
});

test("nextSeq: monotonic (a retry never rewinds the cursor)", () => {
  assert.equal(nextSeq({ seq: 4 }, 0), 4);
  assert.equal(nextSeq({ seq: 2 }, 4), 4);
  assert.equal(nextSeq(null, 7), 7);
  assert.equal(nextSeq({ seq: "not-a-number" }, 7), 7);
});

test("isTerminal: only concluded states", () => {
  for (const s of ["success", "failure", "cancelled"]) assert.equal(isTerminal(s), true);
  for (const s of ["queued", "running", ""]) assert.equal(isTerminal(s), false);
});

test("storedText: renders a JSON window in sequence order and dedupes", () => {
  const page = { frames: [{ seq: 1, text: "a" }, { seq: 2, text: "b\n" }, { seq: 2, text: "b\n" }] };
  assert.equal(storedText(page), "a\nb\n");
  assert.equal(storedText(null), "");
  assert.equal(storedText({ frames: [] }), "");
});
