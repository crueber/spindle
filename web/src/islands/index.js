// web/src/islands/index.js — the vanilla-ES island entry (D-WEB-14).
//
// Server-rendered pages carry data-* hooks; these modules attach behavior.
// No framework, no client-side data layer: an island may only touch the DOM it
// is given, and every module here is small enough to read in one sitting.
//
// boot() is idempotent: it runs on DOMContentLoaded and after every htmx swap,
// and each island marks the nodes it took over (data-island-ready) so a swap
// never double-binds.

import { initTheme } from "./theme.js";
import { initMenus } from "./menus.js";
import { initCopy } from "./copy.js";
import { initFilter } from "./filter.js";
import { initAutogrow } from "./autogrow.js";
import { initSelection } from "./selection.js";
import { init as initSSE } from "./sse.js";
import { initDanger } from "./danger.js";
import { initSubmitKeys } from "./submitkeys.js";

const islands = [initTheme, initMenus, initCopy, initFilter, initAutogrow, initSelection, initDanger, initSubmitKeys, initSSE];

export function boot(root = document) {
  for (const init of islands) {
    try {
      init(root);
    } catch (err) {
      // An island must never take the page down with it.
      console.error(`island ${init.name || "anonymous"} failed`, err);
    }
  }
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", () => boot());
} else {
  boot();
}

// htmx swaps: re-run islands over the new subtree (and the body, so OOB swaps
// that land outside the target are initialized too).
document.body.addEventListener("htmx:afterSwap", (e) => boot(e.detail?.target || document));
document.body.addEventListener("htmx:afterSettle", (e) => boot(e.detail?.target || document));
