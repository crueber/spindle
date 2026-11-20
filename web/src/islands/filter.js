// filter.js — client-side filtering for a rendered list.
//
// Markup: <input data-filter="#file-rows"> where the selector points at a
// container whose children carry data-name="…". Matching rows stay, the rest
// get [hidden]; an optional [data-filter-empty] sibling shows when nothing
// matches. The list is already on the page — this never fetches, so it keeps
// working when the network does not, and the server remains the source of
// truth (a reload restores the full list).

/** filterMatches reports whether a row name matches the typed query. */
export function filterMatches(name, query) {
  const q = String(query ?? "").trim().toLowerCase();
  if (!q) return true;
  return String(name ?? "").toLowerCase().includes(q);
}

export function initFilter(root = document) {
  for (const input of root.querySelectorAll("[data-filter]")) {
    if (input.dataset.islandReady) continue;
    input.dataset.islandReady = "1";
    const target = document.querySelector(input.getAttribute("data-filter") || "");
    if (!target) continue;
    const rows = Array.from(target.querySelectorAll("[data-name]"));
    const empty = target.parentElement?.querySelector("[data-filter-empty]") || null;
    const apply = () => {
      const q = input.value;
      let shown = 0;
      for (const row of rows) {
        const hit = filterMatches(row.dataset.name, q);
        row.hidden = !hit;
        if (hit) shown += 1;
      }
      if (empty) empty.classList.toggle("hidden", shown > 0);
    };
    input.addEventListener("input", apply);
    apply();
  }
}
