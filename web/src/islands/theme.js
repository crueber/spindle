// theme.js — the header theme menu island (PLAN-THEMES §7.4/§8.2, W2).
//
// The boot script has already put the resolved theme on <html>; this island
// exists to (a) paint the menu's swatches from the registry, (b) mark the
// current choice on the role="menuitemradio" rows, (c) apply + persist a
// selection without a reload, and (d) re-resolve live only while the choice is
// "match system" (an explicit pick is never overridden by an OS change).
//
// It never re-applies a theme to a subtree: `<html>` is the only node written,
// so htmx swaps inherit it (F1). Popover open/close, outside-click, arrows,
// Home/End and Escape come from menus.js; this module only closes the panel
// after a selection and returns focus to the trigger.
//
// Storage-denied (§7.3): a read failure resolves like an absent value, and a
// write failure only loses persistence — the chosen theme still applies to the
// page, because the click's own id is applied directly rather than re-read.

import {
  STORAGE_KEY,
  SYSTEM,
  resolveTheme,
  themeById,
  applyTheme,
  isThemeId,
} from "./themes.js";

function media(query) {
  return typeof matchMedia === "function" ? matchMedia(query) : null;
}

function readChoice() {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

// The choice is "system" when the key is absent, empty, or explicitly system;
// any other value is only honoured if the registry knows it (a stale id from a
// removed palette resolves like "system" rather than breaking the page).
function isSystemChoice(choice) {
  return !choice || choice === SYSTEM || !isThemeId(choice);
}

function resolveNow() {
  const dark = media("(prefers-color-scheme: dark)");
  const contrast = media("(prefers-contrast: more)");
  return resolveTheme({
    stored: readChoice(),
    prefersDark: dark ? dark.matches : undefined,
    prefersContrast: contrast ? contrast.matches : false,
  });
}

// The last choice this page knows about. Storage is the seed; a click updates
// it even when the write throws, so the page keeps the theme it shows.
let choice;
let choiceLoaded = false;

function currentChoice() {
  if (!choiceLoaded) {
    choice = readChoice();
    choiceLoaded = true;
  }
  return choice;
}

function paintSwatch(item) {
  const theme = themeById(item.dataset.themeOption);
  if (!theme) return;
  for (const chip of item.querySelectorAll("[data-swatch]")) {
    const value = theme.swatch[chip.dataset.swatch];
    if (value) chip.style.backgroundColor = value;
  }
}

function mark(root, checked) {
  for (const item of root.querySelectorAll("[data-theme-option]")) {
    item.setAttribute("aria-checked", item.dataset.themeOption === checked ? "true" : "false");
    paintSwatch(item);
  }
}

// Apply the current choice: system resolves against the OS, an explicit pick is
// applied as-is (never re-read, so a storage-denied click still takes effect).
function paint(root) {
  const c = currentChoice();
  const system = isSystemChoice(c);
  const id = applyTheme(document, system ? resolveNow() : c);
  mark(root, system ? SYSTEM : c);
  return id;
}

function closeMenu(root, trigger, panel) {
  panel.hidden = true;
  trigger.setAttribute("aria-expanded", "false");
  delete root.dataset.menuOpen;
}

let watching = false;

// Live re-resolution: the OS changed its side or its contrast request. Only
// meaningful in system mode; an explicit stored theme always wins (§7.5).
function watchSystem(root) {
  if (watching || typeof matchMedia !== "function") return;
  watching = true;
  for (const query of ["(prefers-color-scheme: dark)", "(prefers-contrast: more)"]) {
    const mq = media(query);
    if (!mq) continue;
    const onChange = () => {
      if (isSystemChoice(currentChoice())) paint(root);
    };
    if (typeof mq.addEventListener === "function") mq.addEventListener("change", onChange);
    else if (typeof mq.addListener === "function") mq.addListener(onChange);
  }
}

export function initTheme(root = document) {
  // Paint/mark on every call: after an htmx swap the panel may be new nodes.
  if (document.querySelector("[data-theme-option]")) paint(root);

  for (const item of root.querySelectorAll("[data-theme-option]")) {
    if (item.dataset.themeReady) continue;
    item.dataset.themeReady = "1";
    item.addEventListener("click", () => {
      choice = item.dataset.themeOption === SYSTEM ? SYSTEM : item.dataset.themeOption;
      choiceLoaded = true;
      try {
        localStorage.setItem(STORAGE_KEY, choice);
      } catch {
        // storage denied: the theme still applies for this page (§7.3)
      }
      paint(document);
      const menu = item.closest("[data-menu]");
      const trigger = menu?.querySelector("[data-menu-trigger]");
      const panel = menu?.querySelector("[data-menu-panel]");
      if (menu && trigger && panel) closeMenu(menu, trigger, panel);
      trigger?.focus();
    });
  }

  watchSystem(root);
}
