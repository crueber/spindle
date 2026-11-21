// markdown-width.test.js — the rendered-markdown container contract.
//
// A README (or issue body, release note, comment) must FILL its card the way
// GitHub's does. The shipped `.markdown-body` rule used `max-width: 72ch`,
// which shrank the body to ~641px inside a 1120px card and left half the
// README empty (screenshot-reported). This pins the class-level fix: the base
// rule carries no width/measure cap, containment lives on the media rules, and
// no template pairs `markdown-body` with a width-constraining utility.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..", "..");
const css = readFileSync(join(root, "web", "src", "app.css"), "utf8");

// The first `.markdown-body { … }` block is the base prose rule; the
// `.markdown-body > …` / `.markdown-body img` rules follow it.
function baseBlock() {
  const start = css.indexOf(".markdown-body {");
  assert.ok(start >= 0, "the .markdown-body base rule must exist");
  const end = css.indexOf("}", start);
  return css.slice(start, end + 1);
}

function rule(selector) {
  const start = css.indexOf(selector + " {");
  assert.ok(start >= 0, `${selector} rule must exist`);
  const end = css.indexOf("}", start);
  return css.slice(start, end + 1);
}

test("markdown body has no width/measure cap (fills its card)", () => {
  const block = baseBlock();
  assert.doesNotMatch(block, /max-width\s*:/, "no max-width — the body fills the card");
  assert.doesNotMatch(block, /(^|[^-])width\s*:/m, "no width — block width is the container's");
  assert.doesNotMatch(block, /(^|[^-])margin\s*:[^;]*auto/m, "no auto margins re-centering a capped column");
});

test("markdown containment still holds (images, tables, code)", () => {
  const img = rule(".markdown-body img");
  assert.match(img, /max-width:\s*100%/, "images never exceed the body");
  assert.match(img, /height:\s*auto/, "images keep their aspect ratio");
  assert.match(rule(".markdown-body pre"), /overflow-x:\s*auto/, "code blocks scroll inside");
  const table = rule(".markdown-body table");
  assert.match(table, /max-w-full/, "tables never exceed the body");
  assert.match(table, /overflow-x-auto/, "wide tables scroll inside");
});

function templateFiles(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...templateFiles(p));
    else if (name.endsWith(".html")) out.push(p);
  }
  return out;
}

test("every markdown surface composes markdown-body with no width cap", () => {
  const dir = join(root, "internal", "webui", "templates");
  const files = templateFiles(dir);
  assert.ok(files.length > 0, "template tree found");
  const surfaces = [];
  for (const file of files) {
    const src = readFileSync(file, "utf8");
    for (const m of src.matchAll(/class="([^"]*\bmarkdown-body\b[^"]*)"/g)) {
      surfaces.push({ file, classes: m[1].split(/\s+/) });
    }
  }
  assert.ok(surfaces.length >= 8, `expected every markdown surface, saw ${surfaces.length}`);
  for (const { file, classes } of surfaces) {
    const capped = classes.filter((c) =>
      /^(max-)?w-|^basis-|^col-span-|^justify-self-/.test(c));
    assert.deepEqual(capped, [],
      `${file}: markdown body must fill its card, found width class(es): ${capped.join(", ")}`);
  }
});
