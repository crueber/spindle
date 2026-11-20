// theme.js — dark/light toggle (the only client-persisted preference).
//
// The document ships with class="dark" on <html> (the default). The toggle
// flips that class and stores the choice; the button's two icons are CSS
// swapped on [aria-pressed] so there is no server round trip.

const KEY = "walhub-theme";

function apply(theme) {
  const dark = theme !== "light";
  document.documentElement.classList.toggle("dark", dark);
  for (const btn of document.querySelectorAll("[data-theme-toggle]")) {
    btn.setAttribute("aria-pressed", String(!dark));
    btn.setAttribute("title", dark ? "Switch to light mode" : "Switch to dark mode");
  }
}

export function initTheme() {
  for (const btn of document.querySelectorAll("[data-theme-toggle]")) {
    if (btn.dataset.islandReady) continue;
    btn.dataset.islandReady = "1";
    btn.addEventListener("click", () => {
      const dark = document.documentElement.classList.contains("dark");
      const next = dark ? "light" : "dark";
      try {
        localStorage.setItem(KEY, next);
      } catch {
        // storage denied: the toggle still works for this page
      }
      apply(next);
    });
  }
  let stored = null;
  try {
    stored = localStorage.getItem(KEY);
  } catch {
    stored = null;
  }
  if (stored) apply(stored);
  else apply(document.documentElement.classList.contains("dark") ? "dark" : "light");
}
