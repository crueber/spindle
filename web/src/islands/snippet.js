// snippet.js — insert a commented static-site settings block
// (docs/features/19_static_sites.md §5).
//
// Markup:
//   <button type="button" data-snippet-insert
//           data-snippet-target="#config-text"
//           data-snippet-status="#config-snippet-status">…</button>
//
// One click appends STATIC_SITE_SNIPPET to the target editor — separated from
// existing content by one blank line — and leaves the caret after it. When the
// document already carries a `[static_site]` section the block is NOT added a
// second time: that section is selected and the status line says why. The
// editor receives a real `input` event, so the form's dirty state (and any
// listener) sees the change exactly as if it had been typed.
//
// The pure text transform is exported for `node --test`; the island is DOM-only.

/** STATIC_SITE_SNIPPET is the per-repo half of the static-site configuration. */
export const STATIC_SITE_SNIPPET = [
  "[static_site]",
  "# Publish this repository as a static site (the instance must have [static_sites] enabled).",
  "enabled = true",
  'ref = ""          # empty = the default branch tip',
  'root = ""         # empty = repository root; set e.g. "web" for a built site',
].join("\n") + "\n";

const SECTION_HEADER = /^[ \t]*\[static_site\][ \t]*(?:#.*)?$/m;
const TABLE_HEADER = /^[ \t]*\[/gm;

/**
 * insertStaticSiteSnippet appends the snippet, or reports the existing section.
 * Returns {text, existing, start, end}; start/end are textarea selection
 * offsets: the inserted block's end (the caret), or the existing section span.
 */
export function insertStaticSiteSnippet(text) {
  const src = String(text ?? "");
  const header = SECTION_HEADER.exec(src);
  if (header) {
    let end = src.length;
    TABLE_HEADER.lastIndex = header.index + header[0].length;
    const next = TABLE_HEADER.exec(src);
    if (next) end = next.index;
    return { text: src, existing: true, start: header.index, end };
  }

  let prefix = src;
  if (prefix.length > 0) {
    if (prefix.endsWith("\n\n")) prefix = src;
    else if (prefix.endsWith("\n")) prefix = src + "\n";
    else prefix = src + "\n\n";
  }
  const start = prefix.length + STATIC_SITE_SNIPPET.length;
  return {
    text: prefix + STATIC_SITE_SNIPPET,
    existing: false,
    start,
    end: start,
  };
}

/** announce writes the click's outcome into the button's status region. */
function announce(btn, message) {
  const spec = btn.getAttribute("data-snippet-status") || "";
  const node = spec ? document.querySelector(spec) : null;
  if (node) node.textContent = message;
}

export function initSnippet(root = document) {
  for (const btn of root.querySelectorAll("[data-snippet-insert]")) {
    if (btn.dataset.islandReady) continue;
    btn.dataset.islandReady = "1";
    btn.addEventListener("click", () => {
      const spec = btn.getAttribute("data-snippet-target") || "";
      const ta = spec ? document.querySelector(spec) : null;
      if (!ta || typeof ta.value !== "string") return;
      const res = insertStaticSiteSnippet(ta.value);
      if (!res.existing) ta.value = res.text;
      ta.focus();
      ta.setSelectionRange(res.start, res.end);
      ta.dispatchEvent(new Event("input", { bubbles: true }));
      announce(
        btn,
        res.existing
          ? "a [static_site] section is already present — edit it here"
          : "static-site snippet inserted — publish to apply it",
      );
    });
  }
}
