// focus.js — error navigation for the Configuration editor
// (docs/PLAN-ADMIN.md §3.3/§4, M3).
//
// A rejected save re-renders the page with [data-error-focus] on the first
// offending row. htmx swaps the page without a document load, so the browser's
// autofocus never fires; this island moves focus to the offending control (the
// first focusable input/select/textarea in the row) so a keyboard/AT user lands
// on the cause instead of the top of the form. It runs once per marked row and
// never steals focus from a control the user already has focus in.

export function initErrorFocus(root = document) {
  const target =
    (root?.querySelector ? root.querySelector("[data-error-focus]") : null) ||
    document.querySelector("[data-error-focus]");
  if (!target || target.dataset.errorFocusDone === "1") return;
  const focusable = target.matches?.("input, select, textarea, button, a[href]")
    ? target
    : target.querySelector('input:not([type="hidden"]), select, textarea') ||
      target.querySelector("button, a[href]");
  const node = focusable || target;
  if (typeof node.focus !== "function") return;
  const active = document.activeElement;
  if (active && active !== document.body && target.contains(active)) {
    target.dataset.errorFocusDone = "1";
    return;
  }
  node.focus();
  target.dataset.errorFocusDone = "1";
}
