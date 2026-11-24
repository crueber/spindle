// theme-boot.js — no flash of the wrong theme (PLAN-THEMES §7.3, W2).
//
// A CLASSIC, parser-blocking script from 'self' (the CSP forbids inline
// scripts), loaded in <head> immediately after the stylesheet: it resolves the
// stored choice and writes `data-theme` + the `.dark` side class on <html>
// before the first paint. The server ships `class="dark" data-theme="dark"`,
// so a no-JS client keeps the product default and a storage-denied client
// still resolves through prefers-color-scheme; a write failure (private mode)
// only loses persistence, never the current page's theme.
//
// Built by the existing esbuild step as a second entry point (IIFE, no
// imports at runtime): `web/src/theme-boot.js` → `web/dist/theme-boot.js`.
import { STORAGE_KEY, resolveChoice, applyTheme } from "./islands/themes.js";

(function boot() {
  const mq = typeof matchMedia === "function" ? matchMedia.bind(window) : null;
  const dark = mq ? mq("(prefers-color-scheme: dark)") : null;
  const contrast = mq ? mq("(prefers-contrast: more)") : null;

  const id = resolveChoice(() => localStorage.getItem(STORAGE_KEY), {
    prefersDark: dark ? dark.matches : undefined,
    prefersContrast: contrast ? contrast.matches : false,
  });

  applyTheme(document, id);
  // First-paint marker for the rendered smoke: present in the very first paint
  // probe, it proves the theme was resolved before any content painted.
  document.documentElement.setAttribute("data-theme-boot", id);
})();
