// selection.js — line selection and #L anchors for blob and diff views.
//
// Markup contract (templates):
//   <table data-line-select data-path="/src/app.go">
//     <tr data-line="12" data-side="new"> … </tr>
//   </table>
// Clicking a line number toggles `is-selected`; Shift+click selects a range.
// The URL carries `#L12` (one line) or `#L12-L18` (a range) for sharing, and
// clicking a line already in the hash clears it.
//
// This is DOM-only by design: the parsing/formatting lives in lib/blob-lines.js
// and lib/diff-lines.js (headless, node --test).

function hashFor(lines) {
  if (!lines.length) return "";
  const sorted = [...lines].sort((a, b) => a - b);
  return sorted.length === 1 ? `#L${sorted[0]}` : `#L${sorted[0]}-L${sorted[sorted.length - 1]}`;
}

function parseHash(hash) {
  const m = /^#L(\d+)(?:-L(\d+))?$/.exec(hash || "");
  if (!m) return [];
  const a = Number(m[1]);
  const b = m[2] ? Number(m[2]) : a;
  const out = [];
  for (let i = Math.min(a, b); i <= Math.max(a, b); i++) out.push(i);
  return out;
}

function paint(table, selected) {
  // `line-hl` is the shipped highlight class (web/src/app.css: .blob-row.line-hl /
  // .diff-row.line-hl); `is-selected` is kept as the semantic hook.
  for (const tr of table.querySelectorAll("[data-line]")) {
    const on = selected.has(Number(tr.dataset.line));
    tr.classList.toggle("line-hl", on);
    tr.classList.toggle("is-selected", on);
  }
}

export function initSelection(root = document) {
  for (const table of root.querySelectorAll("[data-line-select]")) {
    if (table.dataset.lineReady) continue;
    table.dataset.lineReady = "1";

    let selected = new Set();
    const fromHash = parseHash(location.hash);
    if (fromHash.length) {
      selected = new Set(fromHash);
      paint(table, selected);
    }

    table.addEventListener("click", (ev) => {
      const cell = ev.target.closest("[data-line]");
      if (!cell || !table.contains(cell)) return;
      // Only the line-number cell selects; text selection keeps working.
      if (!ev.target.closest("[data-line-no]")) return;
      const n = Number(cell.dataset.line);
      if (ev.shiftKey && selected.size) {
        const anchor = [...selected].sort((a, b) => a - b)[0];
        const lo = Math.min(anchor, n);
        const hi = Math.max(anchor, n);
        selected = new Set();
        for (let i = lo; i <= hi; i++) selected.add(i);
      } else if (selected.size === 1 && selected.has(n)) {
        selected = new Set();
      } else {
        selected = new Set([n]);
      }
      paint(table, selected);
      const hash = hashFor([...selected]);
      history.replaceState(null, "", hash || location.pathname + location.search);
    });
  }
}
