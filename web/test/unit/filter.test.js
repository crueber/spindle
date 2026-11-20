import { test } from "node:test";
import assert from "node:assert/strict";

import { filterMatches } from "../../src/islands/filter.js";

test("empty query matches every row", () => {
  assert.equal(filterMatches("README.md", ""), true);
  assert.equal(filterMatches("README.md", "   "), true);
});

test("matching is case-insensitive and substring-based", () => {
  assert.equal(filterMatches("README.md", "readme"), true);
  assert.equal(filterMatches("src/main.go", "MAIN"), true);
  assert.equal(filterMatches("go.mod", ".mod"), true);
});

test("non-matching rows are excluded", () => {
  assert.equal(filterMatches("README.md", "telemetry"), false);
  assert.equal(filterMatches("", "x"), false);
});

test("missing names never throw", () => {
  assert.equal(filterMatches(undefined, "a"), false);
  assert.equal(filterMatches(null, ""), true);
});
