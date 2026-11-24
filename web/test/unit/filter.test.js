import { test } from "node:test";
import assert from "node:assert/strict";

import { filterMatches, filterCountText, filterGroupVisible } from "../../src/islands/filter.js";

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

test("the row count wording is shared with the server", () => {
  assert.equal(filterCountText(1, 149), "Showing 1 of 149 keys");
  assert.equal(filterCountText(149, 149), "Showing 149 of 149 keys");
});

test("a section group hides only when none of its rows match", () => {
  assert.equal(filterGroupVisible([{ hidden: false }, { hidden: true }]), true);
  assert.equal(filterGroupVisible([{ hidden: true }]), false);
  assert.equal(filterGroupVisible([]), false);
});
