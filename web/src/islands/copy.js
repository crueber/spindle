// copy.js — copy-to-clipboard buttons.
//
// Markup: <button data-copy="#clone-https">copy</button> where the value is
// the text of the selector when it starts with "#", otherwise the literal
// attribute value. The button's label flips to "copied" for a moment; failure
// is silent (the text stays selectable).

export function initCopy(root = document) {
  for (const btn of root.querySelectorAll("[data-copy]")) {
    if (btn.dataset.islandReady) continue;
    btn.dataset.islandReady = "1";
    btn.addEventListener("click", async () => {
      const spec = btn.getAttribute("data-copy") || "";
      const source = spec.startsWith("#") ? document.querySelector(spec) : null;
      const text = source ? source.textContent.trim() : spec;
      if (!text) return;
      const label = btn.textContent;
      try {
        await navigator.clipboard.writeText(text);
        btn.textContent = "copied";
      } catch {
        // Clipboard denied (permissions/insecure context): leave the label.
        return;
      }
      setTimeout(() => {
        btn.textContent = label;
      }, 1200);
    });
  }
}
