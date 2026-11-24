import { test } from "node:test";
import assert from "node:assert/strict";

import { leaveGuardMessage, isDirtyForm, guardedNavigation } from "../../src/islands/leaveguard.js";

test("the prompt has one wording for both guards", () => {
  assert.match(leaveGuardMessage(), /unsaved changes/i);
  assert.match(leaveGuardMessage(), /without saving/i);
});

test("a form is dirty only on its live flag", () => {
  assert.equal(isDirtyForm({ dataset: { dirty: "1" } }), true);
  assert.equal(isDirtyForm({ dataset: { dirty: "0" } }), false);
  assert.equal(isDirtyForm({ dataset: {} }), false);
  assert.equal(isDirtyForm(null), false);
});

test("in-page links are guarded, same-page and new-tab links are not", () => {
  assert.equal(guardedNavigation("/admin/config"), true);
  assert.equal(guardedNavigation("/acme/repo"), true);
  assert.equal(guardedNavigation("#auth"), false);
  assert.equal(guardedNavigation("#row-server.listen"), false);
  assert.equal(guardedNavigation(""), false);
  assert.equal(guardedNavigation("javascript:void(0)"), false);
  assert.equal(guardedNavigation("/admin", "_blank"), false);
  assert.equal(guardedNavigation("/file.zip", undefined, true), false);
});
