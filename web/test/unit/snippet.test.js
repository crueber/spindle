// snippet.test.js — the static-site settings snippet (docs/features/19_static_sites.md §5).
//
// The pure transform is what the island calls; these cases pin the editor
// contract (append, do not duplicate, selection), not the DOM.

import test from "node:test";
import assert from "node:assert/strict";

import { STATIC_SITE_SNIPPET, insertStaticSiteSnippet } from "../../src/islands/snippet.js";

function occurrences(haystack, needle) {
  return haystack.split(needle).length - 1;
}

test("snippet carries the per-repo keys and their comments", () => {
  assert.match(STATIC_SITE_SNIPPET, /^\[static_site\]\n/);
  assert.match(STATIC_SITE_SNIPPET, /^enabled = true$/m);
  assert.match(STATIC_SITE_SNIPPET, /^ref = "" + # empty = the default branch tip$/m);
  assert.match(STATIC_SITE_SNIPPET, /^root = "" + # empty = repository root; set e\.g\. "web" for a built site$/m);
  assert.match(STATIC_SITE_SNIPPET, /\[static_sites\] enabled/);
  assert.ok(STATIC_SITE_SNIPPET.endsWith("\n"), "the block ends with a newline");
});

test("empty editor gets exactly the block and the caret after it", () => {
  const res = insertStaticSiteSnippet("");
  assert.equal(res.existing, false);
  assert.equal(res.text, STATIC_SITE_SNIPPET);
  assert.equal(res.start, STATIC_SITE_SNIPPET.length, "caret lands after the inserted block");
  assert.equal(res.end, STATIC_SITE_SNIPPET.length);
});

test("content without a trailing newline is separated by one blank line", () => {
  const src = 'description = "demo"';
  const res = insertStaticSiteSnippet(src);
  assert.equal(res.existing, false);
  assert.ok(res.text.startsWith(src + "\n\n" + STATIC_SITE_SNIPPET), res.text);
  assert.equal(res.text.slice(res.start - STATIC_SITE_SNIPPET.length), STATIC_SITE_SNIPPET);
  assert.equal(res.end, res.text.length);
});

test("content with one trailing newline still gets a blank-line separator", () => {
  const src = "[features]\nissues = false\n";
  const res = insertStaticSiteSnippet(src);
  assert.ok(res.text.startsWith(src + "\n" + STATIC_SITE_SNIPPET), res.text);
  assert.equal(res.existing, false);
});

test("content already ending in a blank line is not padded further", () => {
  const src = "[features]\nissues = false\n\n";
  const res = insertStaticSiteSnippet(src);
  assert.ok(res.text.startsWith(src + STATIC_SITE_SNIPPET), res.text);
});

test("an existing section is never duplicated: it is selected instead", () => {
  const src = 'description = "demo"\n\n[static_site]\nenabled = false\n\n[bundles]\nmain_only = true\n';
  const res = insertStaticSiteSnippet(src);
  assert.equal(res.existing, true, "the caller is told to point at the existing section");
  assert.equal(res.text, src, "the document is untouched");
  assert.equal(occurrences(res.text, "[static_site]"), 1);
  assert.equal(res.text.slice(res.start, res.end), "[static_site]\nenabled = false\n\n");
  assert.ok(res.start > 0 && res.end <= src.length);
});

test("selecting an existing section stops at the next table header", () => {
  const src = "[static_site]\nenabled = true\n[bundles]\nmain_only = true\n";
  const res = insertStaticSiteSnippet(src);
  assert.equal(res.existing, true);
  assert.equal(res.text.slice(res.start, res.end), "[static_site]\nenabled = true\n");
});

test("an existing section at end of file selects through the end", () => {
  const src = "[static_site]\nenabled = true\n";
  const res = insertStaticSiteSnippet(src);
  assert.equal(res.existing, true);
  assert.equal(res.start, 0);
  assert.equal(res.end, src.length);
});

test("a commented header line still counts as the existing section", () => {
  const src = "[static_site] # published site\nenabled = true\n";
  const res = insertStaticSiteSnippet(src);
  assert.equal(res.existing, true);
  assert.equal(occurrences(res.text, "[static_site]"), 1);
});
