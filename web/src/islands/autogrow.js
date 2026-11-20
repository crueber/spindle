// autogrow.js — textareas that grow with their content.
//
// Markup: <textarea data-autogrow rows="3">. Height is reset before measuring
// so the box also shrinks when text is deleted. Re-run after htmx swaps.

export function initAutogrow(root = document) {
  for (const ta of root.querySelectorAll("textarea[data-autogrow]")) {
    if (ta.dataset.islandReady) continue;
    ta.dataset.islandReady = "1";
    const grow = () => {
      ta.style.height = "auto";
      ta.style.height = `${ta.scrollHeight}px`;
    };
    ta.addEventListener("input", grow);
    grow();
  }
}
