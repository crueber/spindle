// sse-island.test.js — the pure half of web/src/islands/sse.js (D-WEB-14).
// DOM-free: frame parsing, backoff, coalescing. The fetch loop is covered by
// the rendered checks, not here.
import test from "node:test";
import assert from "node:assert/strict";

import { parseFrame, backoffMs, coalesce } from "../../src/islands/sse.js";

test("parseFrame: event + data + id + retry", () => {
  const f = parseFrame("event: issue\nid: 42\nretry: 1500\ndata: {\"num\":7}");
  assert.deepEqual(f, { event: "issue", data: '{"num":7}', id: "42", retry: 1500 });
});

test("parseFrame: multi-line data joins with newlines", () => {
  const f = parseFrame("event: notice\ndata: line one\ndata: line two");
  assert.equal(f.event, "notice");
  assert.equal(f.data, "line one\nline two");
});

test("parseFrame: missing event defaults to message; comments ignored", () => {
  const f = parseFrame(": keep-alive\ndata: hi");
  assert.equal(f.event, "message");
  assert.equal(f.data, "hi");
});

test("parseFrame: empty frame is null (heartbeats do not dispatch)", () => {
  assert.equal(parseFrame(": ping"), null);
  assert.equal(parseFrame(""), null);
});

test("parseFrame: only a leading single space after the colon is stripped", () => {
  const f = parseFrame("data:  two");
  assert.equal(f.data, " two");
});

test("parseFrame: non-numeric retry is ignored", () => {
  const f = parseFrame("retry: soon\ndata: x");
  assert.equal(f.retry, null);
});

test("backoffMs: grows exponentially and caps", () => {
  const a = backoffMs(0, 1000, 30000);
  assert.ok(a >= 800 && a <= 1000, `attempt 0: ${a}`);
  const b = backoffMs(3, 1000, 30000);
  assert.ok(b >= 6400 && b <= 8000, `attempt 3: ${b}`);
  const c = backoffMs(20, 1000, 30000);
  assert.ok(c >= 24000 && c <= 30000, `cap: ${c}`);
});

test("coalesce: last call wins within the window, cancel drops it", async () => {
  let calls = [];
  const fn = coalesce((v) => calls.push(v), 20);
  fn(1);
  fn(2);
  await new Promise((r) => setTimeout(r, 40));
  assert.deepEqual(calls, [2]);
  fn(3);
  fn.cancel();
  await new Promise((r) => setTimeout(r, 40));
  assert.deepEqual(calls, [2]);
});
