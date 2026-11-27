// threadlog.test.js — the pure half of web/src/islands/threadlog.js
// (docs/features/15_threads.md §6; PAR-190 log fidelity): sequence dedupe, the
// named gap and attempt reset, marker labels, timestamps and the no-stream
// fallback. The DOM attachment is covered by the rendered run-page check.
import test from "node:test";
import assert from "node:assert/strict";

import {
  frameTime,
  markerLabel,
  durationText,
  isTerminal,
  sequenceStep,
  storedText,
} from "../../src/islands/threadlog.js";

test("frameTime: local clock, empty for a missing or bad value", () => {
  const d = new Date(2026, 0, 2, 3, 4, 5);
  assert.equal(frameTime(d.toISOString()), "03:04:05");
  assert.equal(frameTime(""), "");
  assert.equal(frameTime("not a date"), "");
});

test("sequenceStep: append, duplicate, gap and attempt reset", () => {
  assert.deepEqual(sequenceStep({ seq: 1, attempt: 1 }, 0, 1, 1), { seq: 1, attempt: 1, action: "append" });
  assert.equal(sequenceStep({ seq: 2, attempt: 1 }, 2, 1, 1).action, "duplicate");
  const gap = sequenceStep({ seq: 5, attempt: 1 }, 2, 1, 1);
  assert.equal(gap.action, "gap");
  assert.equal(gap.from, 3);
  assert.equal(gap.to, 4);
  // A re-run restarts at seq 1: the new attempt must not read as a duplicate.
  const reset = sequenceStep({ seq: 1, attempt: 2 }, 40, 2, 1);
  assert.equal(reset.action, "attempt");
  assert.equal(reset.seq, 1);
});

test("markerLabel: every marker is named, never blank", () => {
  assert.match(markerLabel({ kind: "step_start", step: 2, name: "Build" }), /step 2: Build/);
  assert.match(
    markerLabel({ kind: "step_end", step: 2, conclusion: "failure", exit_code: 1, duration_ms: 1500 }),
    /failure.*1\.5s.*exit 1/,
  );
  assert.match(markerLabel({ kind: "group_start", name: "compile" }), /group: compile/);
  assert.equal(markerLabel({ kind: "group_end" }), "◂ group end");
  assert.match(markerLabel({ kind: "truncated", text: "output truncated at 4 bytes" }), /truncated/);
});

test("durationText: millisecond steps read compactly", () => {
  assert.equal(durationText(0), "");
  assert.equal(durationText(250), "250ms");
  assert.equal(durationText(1500), "1.5s");
  assert.equal(durationText(65000), "1m 05s");
});

test("isTerminal: only concluded states", () => {
  for (const s of ["success", "failure", "cancelled"]) assert.equal(isTerminal(s), true);
  for (const s of ["queued", "running", ""]) assert.equal(isTerminal(s), false);
});

test("storedText: frames render in order, bytes verbatim, duplicates dropped", () => {
  const page = {
    attempt: 1,
    frames: [
      { seq: 1, attempt: 1, text: "a" },
      { seq: 2, attempt: 1, text: "b\n" },
      { seq: 2, attempt: 1, text: "b\n" },
    ],
  };
  // A frame is a byte chunk, not a line: "a" + "b\n" is "ab\n", never a
  // fabricated line break at the chunk boundary.
  assert.equal(storedText(page), "ab\n");
  assert.equal(storedText(null), "");
  assert.equal(storedText({ frames: [] }), "");
});

test("storedText: markers and a named gap survive the no-stream fallback", () => {
  const page = {
    attempt: 1,
    frames: [
      { seq: 1, attempt: 1, kind: "step_start", step: 1, name: "Build" },
      { seq: 2, attempt: 1, text: "ok\n" },
      { seq: 5, attempt: 1, kind: "group_start", name: "compile" },
    ],
  };
  const out = storedText(page);
  assert.match(out, /step 1: Build/);
  assert.match(out, /ok/);
  assert.match(out, /log frames 3\.\.4 missing/);
});
