// filter.js — client-side filtering for a rendered list.
//
// Markup: <input data-filter="#file-rows"> where the selector points at a
// container whose children carry data-name="…". Matching rows stay, the rest
// get [hidden]; an optional [data-filter-empty] sibling shows when nothing
// matches. The list is already on the page — this never fetches, so it keeps
// working when the network does not, and the server remains the source of
// truth (a reload restores the full list).
//
// The Configuration page extends the same contract (docs/PLAN-ADMIN.md §4.3):
// [data-section-card] containers hide when none of their [data-name] rows
// match, every <details class="setup-advanced"> opens while a query is typed
// (a matching row inside a collapsed group is otherwise invisible) and closes
// again when the query clears, and an optional [data-filter-count] in the
// input's [data-filter-toolbar] reports "Showing N of TOTAL keys". The server's
// ?q= filter uses the SAME predicate (trimmed, case-insensitive substring); the
// Go test mirrors these cases, so the two can never disagree.

/** filterMatches reports whether a row name matches the typed query. */
export function filterMatches(name, query) {
  const q = String(query ?? "").trim().toLowerCase();
  if (!q) return true;
  return String(name ?? "").toLowerCase().includes(q);
}

/** filterCountText is the shared row-count wording (server renders it too). */
export function filterCountText(shown, total) {
  return `Showing ${shown} of ${total} keys`;
}

/** filterGroupVisible reports whether a group keeps any visible row. */
export function filterGroupVisible(rows) {
  return rows.some((row) => !row.hidden);
}

export function initFilter(root = document) {
  for (const input of root.querySelectorAll("[data-filter]")) {
    if (input.dataset.islandReady) continue;
    input.dataset.islandReady = "1";
    const target = document.querySelector(input.getAttribute("data-filter") || "");
    if (!target) continue;
    const rows = Array.from(target.querySelectorAll("[data-name]"));
    const groups = Array.from(target.querySelectorAll("[data-section-card]"));
    const details = Array.from(target.querySelectorAll("details.setup-advanced"));
    const toolbar = input.closest("[data-filter-toolbar]") || null;
    const count = toolbar?.querySelector("[data-filter-count]") || null;
    const total = Number(count?.dataset.total || 0);
    const empty = target.parentElement?.querySelector("[data-filter-empty]") || null;
    const apply = () => {
      const q = input.value;
      let shown = 0;
      for (const row of rows) {
        const hit = filterMatches(row.dataset.name, q);
        row.hidden = !hit;
        if (hit) shown += 1;
      }
      for (const group of groups) {
        const groupRows = Array.from(group.querySelectorAll("[data-name]"));
        group.hidden = groupRows.length > 0 && !filterGroupVisible(groupRows);
      }
      // A filtered row inside a collapsed Advanced group must be visible.
      for (const detail of details) {
        if (String(q).trim()) {
          if (!detail.dataset.filterOpened) {
            detail.dataset.filterOpened = "1";
            detail.dataset.filterWasOpen = detail.open ? "1" : "0";
            detail.open = true;
          }
        } else if (detail.dataset.filterOpened) {
          if (detail.dataset.filterWasOpen !== "1") detail.open = false;
          delete detail.dataset.filterOpened;
          delete detail.dataset.filterWasOpen;
        }
      }
      if (count && total > 0) count.textContent = filterCountText(shown, total);
      if (empty) empty.classList.toggle("hidden", shown > 0 || rows.length === 0);
    };
    input.addEventListener("input", apply);
    const clear = toolbar?.querySelector("[data-filter-clear]") || null;
    if (clear) {
      clear.addEventListener("click", (e) => {
        // The link is the no-JS path (/admin/config). With JS, clear in place
        // only when the full row set is mounted; a server-narrowed page (?q=
        // submitted, or changed=1) must navigate to restore the hidden rows.
        const changed = toolbar?.querySelector('input[name="changed"]');
        if (changed?.checked || rows.length !== total) return;
        e.preventDefault();
        input.value = "";
        apply();
        input.focus();
      });
    }
    for (const toggle of toolbar?.querySelectorAll("[data-autosubmit]") || []) {
      toggle.addEventListener("change", () => {
        if (input.form?.requestSubmit) input.form.requestSubmit();
        else input.form?.submit?.();
      });
    }
    apply();
  }
}
