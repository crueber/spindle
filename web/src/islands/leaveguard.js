// leaveguard.js — the unsaved-changes guard for the Configuration editor
// (docs/PLAN-ADMIN.md §5, M3).
//
// Markup: <form data-leave-guard> with an optional
// [data-dirty-indicator] inside the save button and a [data-dirty-status]
// live region. Any input/change arms the form; a submit or an htmx request
// disarms it (the server is then the one deciding). While a form is dirty:
//   - beforeunload gets the browser's own once-per-navigation prompt;
//   - an in-page link click is confirmed once (links/pills/clear that leave
//     the editor), and a refused confirmation cancels the navigation.
// There is deliberately no modal, no state machine and no second save path:
// Save still posts the whole document exactly as before.

/** leaveGuardMessage is the one wording for both prompts. */
export function leaveGuardMessage() {
  return "You have unsaved changes. Leave this page without saving?";
}

/** isDirtyForm reports whether a [data-leave-guard] form is armed. */
export function isDirtyForm(form) {
  return form?.dataset?.dirty === "1";
}

/** guardedNavigation reports whether a link click would leave the form. */
export function guardedNavigation(href, target, download) {
  const h = String(href ?? "");
  if (!h || h.startsWith("#") || h.startsWith("javascript:")) return false;
  if (target === "_blank" || download) return false;
  return true;
}

let bound = false;

function dirtyForm() {
  return document.querySelector('[data-leave-guard][data-dirty="1"]');
}

export function initLeaveGuard(root = document) {
  for (const form of root.querySelectorAll("[data-leave-guard]")) {
    if (form.dataset.islandReady) continue;
    form.dataset.islandReady = "1";
    const marker = form.querySelector("[data-dirty-indicator]");
    const status = form.querySelector("[data-dirty-status]");
    const setDirty = (dirty) => {
      form.dataset.dirty = dirty ? "1" : "0";
      if (marker) marker.hidden = !dirty;
      if (status) status.textContent = dirty ? "Unsaved changes." : "";
    };
    setDirty(isDirtyForm(form));
    form.addEventListener("input", () => setDirty(true));
    form.addEventListener("change", () => setDirty(true));
    form.addEventListener("submit", () => setDirty(false));
    form.addEventListener("htmx:beforeRequest", () => setDirty(false));
  }
  if (bound) return;
  bound = true;
  window.addEventListener("beforeunload", (e) => {
    if (!dirtyForm()) return;
    e.preventDefault();
    e.returnValue = leaveGuardMessage();
    return e.returnValue;
  });
  document.addEventListener(
    "click",
    (e) => {
      const link = e.target?.closest?.("a[href]");
      if (!link || !dirtyForm()) return;
      if (!guardedNavigation(link.getAttribute("href"), link.getAttribute("target"), link.hasAttribute("download"))) return;
      if (!window.confirm(leaveGuardMessage())) {
        e.preventDefault();
        e.stopPropagation();
      }
    },
    true,
  );
}
