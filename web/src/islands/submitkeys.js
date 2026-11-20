// submitkeys.js — keyboard submission (island spec §5.3).
//
// Markup: <form data-submit-keys="mod+enter"> (or on a textarea). Only the
// exact combo matches: (metaKey || ctrlKey) && key === "Enter". A plain Enter
// keeps its normal newline behavior inside a textarea.

const COMBOS = {
  "mod+enter": (e) => (e.metaKey || e.ctrlKey) && e.key === "Enter",
};

export function initSubmitKeys(root = document) {
  for (const host of root.querySelectorAll("[data-submit-keys]")) {
    if (host.dataset.keysReady) continue;
    host.dataset.keysReady = "1";
    const combo = COMBOS[host.getAttribute("data-submit-keys")];
    if (!combo) continue;
    host.addEventListener("keydown", (e) => {
      if (!combo(e)) return;
      e.preventDefault();
      const form = host.tagName === "FORM" ? host : host.form;
      if (!form) return;
      if (typeof form.requestSubmit === "function") form.requestSubmit();
      else form.submit();
    });
  }
}
