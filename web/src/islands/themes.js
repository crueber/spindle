// themes.js — the theme registry (PLAN-THEMES §7.1, W2).
//
// One data source, three consumers: `theme-boot.js` resolves the choice before
// first paint, `theme.js` drives the header menu, and the `[data-theme]` token
// blocks in `app.css` paint the result. Adding a palette is a DATA change —
// a registry entry (id + side + swatch) plus its CSS block — never a code
// change. `web/test/unit/themes.test.js` pins the three consumers together, so
// an id, a side or a swatch can never drift from the token tables.
//
// The swatch is the menu's three-chip preview (canvas / text / accent). It is
// read from here and written onto the row by the island, so no colour literal
// lives in the template; the test derives the same three values from app.css
// and fails on any mismatch.
//
// `side` is the luminance side, not the mood: a dark-side theme keeps the
// `.dark` class on <html> so every `dark:` utility still fires (§8.1).

export const STORAGE_KEY = "spindle-theme";
export const SYSTEM = "system";
export const DEFAULT_THEME = "dark";

export const THEMES = [
  {
    id: "dark",
    side: "dark",
    swatch: { canvas: "#0f1113", text: "#e6eaec", accent: "#ffb224" },
  },
  {
    id: "light",
    side: "light",
    swatch: { canvas: "#ffffff", text: "#17181a", accent: "#9a5b00" },
  },
  // Higher-contrast variants of the two defaults (PLAN-THEMES §4.3/§4.4):
  // the same identities, stronger — text pairs ≥ 7:1, control borders ≥ 3:1.
  {
    id: "hc-dark",
    side: "dark",
    swatch: { canvas: "#0f1113", text: "#f4f8fa", accent: "#ffc23e" },
  },
  {
    id: "hc-light",
    side: "light",
    swatch: { canvas: "#ffffff", text: "#0a0b0c", accent: "#7a4600" },
  },
  // Expressive palettes (§4.5–§4.7). The expressive move is the world; the
  // roles never move — the action accent is terracotta / azure / amber, green
  // stays state, links stay ink.
  {
    id: "paper",
    side: "light",
    swatch: { canvas: "#f9f3e7", text: "#241d10", accent: "#9c4a12" },
  },
  {
    id: "ocean",
    side: "dark",
    swatch: { canvas: "#0c1420", text: "#e0eaf4", accent: "#6cb6ff" },
  },
  {
    id: "terminal",
    side: "dark",
    swatch: { canvas: "#0a0f0c", text: "#d9e8dc", accent: "#ffb224" },
  },
];

const BY_ID = new Map(THEMES.map((t) => [t.id, t]));

export function themeById(id) {
  return BY_ID.get(id) || null;
}

export function isThemeId(id) {
  return BY_ID.has(id);
}

export function sideOf(id) {
  const theme = themeById(id);
  return theme ? theme.side : "dark";
}

// High-contrast mapping (§7.5): system mode + an OS "more contrast" request
// lands on the HC theme of the resolved side. Only applies while the user has
// not picked a theme by hand, and only when that palette exists.
function hcFor(side) {
  const id = `hc-${side}`;
  return isThemeId(id) ? id : null;
}

// resolveTheme — §7.1, in order:
//   1. a stored explicit choice (a valid theme id) wins, always;
//   2. otherwise the OS side: prefers-color-scheme dark ? dark : light,
//      upgraded to hc-<side> when the OS also asks for more contrast;
//   3. otherwise (no matchMedia / no preference) the product default, dark.
// `prefersDark === undefined` is the no-matchMedia case, deliberately distinct
// from `false` (the OS declares light).
export function resolveTheme({ stored, prefersDark, prefersContrast } = {}) {
  if (stored && stored !== SYSTEM && isThemeId(stored)) return stored;
  if (prefersDark === undefined) return DEFAULT_THEME;
  const side = prefersDark ? "dark" : "light";
  if (prefersContrast) {
    const hc = hcFor(side);
    if (hc) return hc;
  }
  return side;
}

// resolveChoice — the storage-denied path (§7.3): a read that throws (private
// mode, blocked storage) resolves exactly like an absent value; it never takes
// the page down and never changes the fallback order.
export function resolveChoice(read, env) {
  let stored = null;
  try {
    stored = read();
  } catch {
    stored = null;
  }
  return resolveTheme({ stored, ...env });
}

// applyTheme — the only place <html> is written. Sets the id attribute and the
// side class together so a theme is never half-applied. Accepts a document so
// the logic is headless-testable; an unknown id degrades to the default.
export function applyTheme(doc, id) {
  const theme = themeById(id) || themeById(DEFAULT_THEME);
  doc.documentElement.setAttribute("data-theme", theme.id);
  doc.documentElement.classList.toggle("dark", theme.side === "dark");
  return theme.id;
}
