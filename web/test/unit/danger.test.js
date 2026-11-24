// danger.test.js — the typed-confirmation island's markup contract.
//
// The transfer form carries an "owner" text input BEFORE its confirmation
// input, so the island's old "first text input" fallback armed against the
// owner name and left the submit disabled forever. The armed input is marked
// explicitly; these pins keep the selector order and the template marker in
// sync (the repo's source-pin convention: no DOM in node --test).
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../../", import.meta.url));

// readTemplate finds a template by BASENAME anywhere under the templates tree.
// A hand-written path pin fails with ENOENT the day the markup moves or is
// split into a partial; the markup still has exactly one home, so assert that
// and name the candidates instead.
function readTemplate(name) {
  const walk = (dir, out = []) => {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) walk(full, out);
      else if (entry.endsWith(".html")) out.push(full);
    }
    return out;
  };
  const hits = walk(join(root, "internal", "webui", "templates")).filter(
    (p) => p.slice(p.lastIndexOf("/") + 1) === name,
  );
  assert.equal(
    hits.length,
    1,
    `expected exactly one ${name} under internal/webui/templates, found ${hits.length}: ` +
      `${hits.join(", ") || "(none)"} — the template that owns this markup moved or was split; ` +
      "point this test at the file that owns it now",
  );
  return readFileSync(hits[0], "utf8");
}

const island = readFileSync(new URL("../../src/islands/danger.js", import.meta.url), "utf8");
const template = readTemplate("settings_repo_danger.html");

test("danger island prefers the explicitly armed input", () => {
  const q = island.slice(island.indexOf("const input ="));
  const arm = q.indexOf("[data-danger-arm]");
  const legacy = q.indexOf("[data-danger-input]");
  const fallback = q.indexOf('input[type=text]');
  assert.ok(arm > 0, "data-danger-arm must be consulted");
  assert.ok(arm < legacy && legacy < fallback, "selector order: arm → input → first text");
});

test("the transfer card's confirmation input is the armed one", () => {
  const form = template.slice(template.indexOf('name="danger_action" value="transfer"'));
  const body = form.slice(0, form.indexOf("</form>"));
  const owner = body.indexOf('name="owner"');
  const confirm = body.indexOf("data-danger-arm");
  assert.ok(owner > 0 && confirm > 0, "both fields present");
  assert.ok(owner < confirm, "the owner field comes first (why the marker matters)");
  assert.ok(
    body.slice(body.lastIndexOf("<input", confirm)).includes('name="confirm"'),
    "the marked input is the confirmation field",
  );
});

test("the transfer card renders the live task hooks", () => {
  assert.ok(template.includes("data-task-stream"), "task attach stream");
  assert.ok(template.includes("data-task-next"), "task success target");
  assert.ok(template.includes('data-task-noun="transfer"'), "card copy");
});

// The swap hook that binds swapped-in islands (the transfer card is hx-swap=outerHTML).
test("boot is wired to htmx:load as well as afterSwap/afterSettle", () => {
  const src = readFileSync(new URL("../../src/islands/index.js", import.meta.url), "utf8");
  assert.ok(src.includes("htmx:afterSwap"), "afterSwap pass");
  assert.ok(src.includes("htmx:load"), "htmx:load pass (the inserted content)");
});
