// themes.test.js — the theme contract gate (PLAN-THEMES §9.1).
//
// This is the merge gate for the colour system: it parses the REAL token
// source (`web/src/app.css`) rather than a duplicated palette, so a token edit
// anywhere is measured, not reviewed. Every gated pair below is WCAG 2.x
// relative luminance:
//
//   L = 0.2126·R′ + 0.7152·G′ + 0.0722·B′
//   C′ = (C/255 ≤ 0.03928) ? C/12.92 : ((C/255 + 0.055)/1.055)^2.4
//   contrast = (Llighter + 0.05) / (Ldarker + 0.05)
//
// Gates: 4.5:1 for text (< 18px — chips are 10px, code 11.5px, so all of it
// is "small text"), 3:1 for non-text UI (focus ring, state markers, selection
// edge, graph lanes). Two defaults carry deliberate, documented deviations
// asserted at their floor instead: `line-strong` (hairline control border,
// PLAN-THEMES §2-8) and `dim` (decorative gutter numbers, §2 note + §10.5-2).
// The five palettes land in W3; the checks are already generic over every
// `[data-theme]` block that exists.
//
// Structural assertions (pairs 24–26): theme-block completeness, the
// dead-token check (every defined token consumed outside the theme blocks),
// declaration-only theme blocks, no `[data-theme]` outside the theme blocks,
// ramps monotonic per side, and the raw-ramp grep over templates AND Go —
// `checkStateDot` was Go-emitted colour, invisible to a template-only grep.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync, existsSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..", "..");
const CSS = readFileSync(join(root, "web", "src", "app.css"), "utf8");

// --- tiny CSS reader --------------------------------------------------------
// Comments out, then top-level rules (nested braces counted, so an @media /
// @layer body stays one rule). Enough to isolate the theme blocks from the
// component rules — this is a gate, not a CSS parser.
function stripComments(s) {
  return s.replace(/\/\*[\s\S]*?\*\//g, "");
}

function topLevelRules(s) {
  const out = [];
  let i = 0;
  let sel = "";
  while (i < s.length) {
    const ch = s[i];
    if (ch === "{") {
      let depth = 1;
      let j = i + 1;
      while (j < s.length && depth > 0) {
        if (s[j] === "{") depth++;
        else if (s[j] === "}") depth--;
        if (depth === 0) break;
        j++;
      }
      // At-rules that end in `;` (@import, @source, @custom-variant) are
      // statements, not selectors: only the text after the last `;` names
      // this rule.
      const name = sel.slice(sel.lastIndexOf(";") + 1).trim();
      out.push({ selector: name, body: s.slice(i + 1, j) });
      i = j + 1;
      sel = "";
      continue;
    }
    if (ch === "}") {
      // A stray close (should not happen at depth 0) — start fresh.
      sel = "";
      i++;
      continue;
    }
    sel += ch;
    i++;
  }
  return out;
}

function declarations(body) {
  const out = {};
  for (const m of body.matchAll(/(--[a-zA-Z0-9-]+)\s*:\s*([^;]+);/g)) {
    out[m[1]] = m[2].trim();
  }
  return out;
}

const RULES = topLevelRules(stripComments(CSS));
const isThemeSelector = (sel) => sel === ":root" || sel === ".dark" || sel.startsWith("[data-theme");

function mergeDecls(selector) {
  const out = {};
  for (const r of RULES) if (r.selector === selector) Object.assign(out, declarations(r.body));
  return out;
}

const ROOT = mergeDecls(":root");
const DARK = mergeDecls(".dark");

function tokensOf(decls) {
  const out = {};
  for (const [k, v] of Object.entries(decls)) if (k.startsWith("--pd-")) out[k] = v;
  return out;
}
const LIGHT_TOKENS = tokensOf(ROOT);
const DARK_TOKENS = tokensOf(DARK);

// The one token contract: the union of what the shipped themes define.
const TOKEN_NAMES = Object.keys(LIGHT_TOKENS).sort();

// Ramp overrides live beside the token blocks (per side): the @theme zinc ramp
// is shared, so `.dark` restores the light-side text-step override.
function rampOf(overrides) {
  const theme = RULES.find((r) => r.selector === "@theme");
  assert.ok(theme, "@theme block must exist");
  const out = {};
  for (const [k, v] of Object.entries(declarations(theme.body))) if (k.startsWith("--color-zinc-")) out[k] = v;
  for (const [k, v] of Object.entries(overrides)) if (k.startsWith("--color-zinc-")) out[k] = v;
  return out;
}
// The composed light ramp (the print block must mirror every step of it).
const LIGHT_RAMP = rampOf(ROOT);

function graphOf(overrides) {
  const out = {};
  for (const [k, v] of Object.entries(overrides)) if (k.startsWith("--graph-")) out[k] = v;
  return out;
}
const LIGHT_GRAPH = graphOf(ROOT);
const DARK_GRAPH = graphOf(DARK);

// --- colour maths ------------------------------------------------------------
function parseColor(value) {
  const v = value.trim();
  const hex = v.match(/^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$/);
  if (hex) {
    let h = hex[1];
    if (h.length === 3) h = h.split("").map((c) => c + c).join("");
    const n = parseInt(h, 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1];
  }
  const rgb = v.match(/^rgba?\(\s*([\d.]+)[\s,]+([\d.]+)[\s,]+([\d.]+)\s*(?:\/\s*([\d.]+)\s*)?\)$/);
  if (rgb) return [Number(rgb[1]), Number(rgb[2]), Number(rgb[3]), rgb[4] === undefined ? 1 : Number(rgb[4])];
  throw new Error(`unparseable colour: ${value}`);
}

// Composite a (possibly alpha) colour over an opaque base.
function over(fg, baseHex) {
  const [r, g, b, a] = parseColor(fg);
  const [br, bg, bb] = parseColor(baseHex);
  return [r * a + br * (1 - a), g * a + bg * (1 - a), b * a + bb * (1 - a), 1];
}

function lum([r, g, b]) {
  const lin = (c) => {
    c /= 255;
    return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function contrast(a, b) {
  const la = lum(a), lb = lum(b);
  const hi = Math.max(la, lb), lo = Math.min(la, lb);
  return (hi + 0.05) / (lo + 0.05);
}

function ratio(a, b) {
  return contrast(parseColor(a), parseColor(b));
}

// Hue angle (degrees) of an opaque colour — used for the two hue-family rules
// that must survive every palette: green is the STATE family, and the action
// accent never lands in it (§1.3-3/4).
function hue(value) {
  const [r, g, b] = parseColor(value).map((c) => c / 255);
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const d = max - min;
  if (d === 0) return 0;
  let h;
  if (max === r) h = ((g - b) / d) % 6;
  else if (max === g) h = (b - r) / d + 2;
  else h = (r - g) / d + 4;
  h *= 60;
  return h < 0 ? h + 360 : h;
}

// --- every painted theme, composed from the real cascade ---------------------
// `:root`/`.dark` are the two defaults; each palette is a `[data-theme]` block.
// A theme's composed ramp/graph is @theme → its side's base overrides → its own
// block, because the boot script keeps `.dark` on the dark side only and every
// `[data-theme]` block is declared after `.dark` (equal specificity, later
// wins). The side is derived from the measured canvas, never trusted.
function zincOf(decls) {
  const out = {};
  for (const [k, v] of Object.entries(decls)) if (k.startsWith("--color-zinc-")) out[k] = v;
  return out;
}
function composedRamp(side, decls) {
  return { ...rampOf(side === "dark" ? DARK : ROOT), ...zincOf(decls) };
}
function composedGraph(side, decls) {
  return { ...(side === "dark" ? DARK_GRAPH : LIGHT_GRAPH), ...graphOf(decls) };
}

const THEME_BLOCKS = (() => {
  const out = {};
  for (const r of RULES.filter((x) => x.selector.trim().startsWith("[data-theme"))) {
    const m = r.selector.match(/\[data-theme="([^"]+)"\]/);
    assert.ok(m, `unparseable theme selector ${r.selector}`);
    out[m[1]] = declarations(r.body);
  }
  return out;
})();

// light → dark → each palette in source order; every one is measured.
const PAINTED = (() => {
  const entries = [
    ["light", LIGHT_TOKENS, ROOT],
    ["dark", DARK_TOKENS, DARK],
    ...Object.entries(THEME_BLOCKS).map(([name, decls]) => [name, tokensOf(decls), decls]),
  ];
  return entries.map(([name, tokens, decls]) => {
    const side = lum(parseColor(tokens["--pd-canvas"])) > 0.5 ? "light" : "dark";
    return {
      name,
      tokens,
      decls,
      side,
      hc: name.startsWith("hc-"),
      ramp: composedRamp(side, decls),
      graph: composedGraph(side, decls),
    };
  });
})();
// --- the pair matrix ---------------------------------------------------------
// Tailwind's default red is the one sanctioned raw-ramp text (plan §5.2), and
// the shipped form-error classes use it: light `text-red-700`, dark
// `text-red-400`. Values are Tailwind v4 defaults, pinned here on purpose.
const RED = { light: "#b91c1c", dark: "#f87171" };

function pairsFor(tokens, ramp, graph, side, hc = false) {
  const t = tokens;
  const p = [];
  const add = (id, a, b, min) => p.push({ id, a, b, min });
  // §9.1: the two HC palettes raise body/secondary/button-label text to 7:1
  // and the two documented deviations to their real floors (border 3:1,
  // gutter `dim` 4.5:1). Everything else keeps the AA floor.
  const textMin = hc ? 7 : 4.5;
  const borderMin = hc ? 3 : 1.5;
  const dimMin = hc ? 4.5 : 2.4;

  // 1-5: text roles.
  add("text/canvas", t["--pd-text"], t["--pd-canvas"], textMin);
  add("text/surface", t["--pd-text"], t["--pd-surface"], textMin);
  add("text-2/canvas", t["--pd-text-2"], t["--pd-canvas"], textMin);
  add("text-2/surface", t["--pd-text-2"], t["--pd-surface"], textMin);
  add("text-2/raised-2", t["--pd-text-2"], t["--pd-raised-2"], 4.5);
  add("muted/canvas", t["--pd-muted"], t["--pd-canvas"], 4.5);
  add("muted/surface", t["--pd-muted"], t["--pd-surface"], 4.5);
  add("muted/raised", t["--pd-muted"], t["--pd-raised"], 4.5);
  add("link/canvas", t["--pd-link"], t["--pd-canvas"], 4.5);

  // 6-8: the action accent (button label, hover label, focus ring / active edge).
  add("on-accent/accent", t["--pd-on-accent"], t["--pd-accent"], textMin);
  add("on-accent/accent-hover", t["--pd-on-accent"], t["--pd-accent-hover"], 4.5);
  add("accent/canvas", t["--pd-accent"], t["--pd-canvas"], 3);
  add("accent/surface", t["--pd-accent"], t["--pd-surface"], 3);
  add("accent/raised", t["--pd-accent"], t["--pd-raised"], 3);

  // 9-10: state markers / state text.
  for (const s of ["ok", "merged", "prerelease"]) add(`${s}/canvas`, t[`--pd-${s}`], t["--pd-canvas"], 3);
  for (const s of ["warn", "danger"]) add(`${s}/canvas`, t[`--pd-${s}`], t["--pd-canvas"], 4.5);

  // 11: chip text on its own ground.
  for (const s of ["ok", "warn", "danger", "merged", "prerelease"]) {
    add(`${s}/${s}-bg`, t[`--pd-${s}`], t[`--pd-${s}-bg`], 4.5);
  }
  // 12: the neutral chip.
  add("text-2/raised", t["--pd-text-2"], t["--pd-raised"], 4.5);

  // 13: diff rows. The dark side tints with alpha, so measure the effective
  // ground over BOTH surfaces that can carry a diff row.
  for (const base of ["canvas", "surface"]) {
    add(`add-fg/${base}`, t["--pd-add-fg"], over(t["--pd-add-bg"], t[`--pd-${base}`]), 4.5);
    add(`del-fg/${base}`, t["--pd-del-fg"], over(t["--pd-del-bg"], t[`--pd-${base}`]), 4.5);
  }

  // 14-16: the documented deviations and the selection edge.
  add("line-strong/canvas", t["--pd-line-strong"], t["--pd-canvas"], borderMin);
  add("sel-edge/canvas", t["--pd-sel-edge"], t["--pd-canvas"], 3);
  add("dim/surface", t["--pd-dim"], t["--pd-surface"], dimMin);

  // 17: syntax colours on the code ground.
  add("kw/code-bg", t["--pd-kw"], t["--pd-code-bg"], 4.5);
  add("tok-str/code-bg", t["--pd-ok"], t["--pd-code-bg"], 4.5);
  add("tok-com/code-bg", t["--pd-muted"], t["--pd-code-bg"], 4.5);
  add("tok-num/code-bg", t["--pd-accent"], t["--pd-code-bg"], 4.5);

  // 18: the information callout.
  add("text/info-bg", t["--pd-text"], t["--pd-info-bg"], 4.5);

  // 19: the ramp text steps that carry text (11px setup notes).
  const steps = side === "dark" ? ["400", "500"] : ["400", "500", "600"];
  for (const s of steps) add(`zinc-${s}/canvas`, ramp[`--color-zinc-${s}`], t["--pd-canvas"], 4.5);

  // 20: the identity avatar (zinc-50 on zinc-700) — fires on both sides.
  add("zinc-50/zinc-700", ramp["--color-zinc-50"], ramp["--color-zinc-700"], 4.5);

  // 21: graph lanes are continuity markers — non-text, 3:1.
  for (let i = 0; i < 8; i++) add(`graph-${i}/canvas`, graph[`--graph-${i}`], t["--pd-canvas"], 3);

  // 22: the sanctioned raw red.
  add("red/canvas", RED[side], t["--pd-canvas"], 4.5);

  for (const pair of p) {
    if (typeof pair.a === "string" && typeof pair.b === "string") {
      pair.value = ratio(pair.a, pair.b);
    } else {
      pair.value = contrast(parseColor(pair.a), pair.b);
    }
  }
  return p;
}

const PAIRS = new Map(
  PAINTED.map((t) => [t.name, pairsFor(t.tokens, t.ramp, t.graph, t.side, t.hc)]),
);

// --- 0. formula validation ---------------------------------------------------
// The method must reproduce the ratios documented for the shipped theme, or a
// formula regression would silently pass every gate. (PLAN-THEMES §9.1.)
test("the contrast formula reproduces the documented shipped pairs", () => {
  const near = (got, want, label) =>
    assert.ok(Math.abs(got - want) <= 0.05, `${label}: computed ${got.toFixed(2)} vs documented ${want}`);
  near(ratio("#e6eaec", "#0f1113"), 15.63, "dark body on canvas");
  near(ratio("#ffffff", "#9a5b00"), 5.43, "light on-accent over accent");
  near(ratio("#a3adb4", "#14181b"), 7.82, "dark text-2 on surface");
});

// --- 1. completeness ---------------------------------------------------------
test("every theme block defines the complete token contract", () => {
  // The contract is exactly these 34 roles (PLAN-THEMES §1.1 + §1.2 additions);
  // adding a token means teaching this gate about its pair, which is the point.
  assert.equal(TOKEN_NAMES.length, 34, `expected the 34-token contract, got ${TOKEN_NAMES.length}`);
  for (const [name, set] of [["light (:root)", LIGHT_TOKENS], ["dark (.dark)", DARK_TOKENS]]) {
    for (const tok of TOKEN_NAMES) {
      assert.ok(tok in set, `${name} is missing ${tok} (no partial themes)`);
    }
  }
  const blocks = RULES.filter((r) => r.selector.trim().startsWith("[data-theme"));
  for (const b of blocks) {
    const set = tokensOf(declarations(b.body));
    for (const tok of TOKEN_NAMES) {
      assert.ok(tok in set, `${b.selector} is missing ${tok} — every theme sets the whole contract`);
    }
  }
});

// --- 2. liveness (pair 24, the dead-token check) ----------------------------
test("every token has a consumer outside the theme blocks", () => {
  const outside = RULES.filter((r) => !isThemeSelector(r.selector.trim()))
    .map((r) => `${r.selector}{${r.body}}`)
    .join("\n");
  const dead = TOKEN_NAMES.filter((tok) => !outside.includes(`var(${tok})`));
  assert.deepEqual(dead, [], `dead tokens (defined, never consumed): ${dead.join(", ")}`);
});

// --- 3. theme-block purity and drift (pair 26) ------------------------------
test("theme blocks declare only custom properties, and no rule mentions [data-theme]", () => {
  const blocks = RULES.filter((r) => r.selector.trim().startsWith("[data-theme"));
  for (const b of blocks) {
    assert.doesNotMatch(b.body, /[{}]/, `${b.selector} must not nest rules or media queries`);
    for (const decl of b.body.split(";")) {
      const d = decl.trim();
      if (!d) continue;
      assert.match(d, /^--[a-zA-Z0-9-]+\s*:/, `${b.selector}: non-custom-property declaration "${d}"`);
    }
  }
  const sel = RULES.map((r) => r.selector).join("\n");
  const dataThemeMentions = (sel.match(/\[data-theme/g) ?? []).length;
  assert.equal(
    dataThemeMentions,
    blocks.length,
    "`[data-theme` may appear only as a theme block's own selector — no component rule, no per-page override",
  );
});

// --- 4. the gated pairs, per theme ------------------------------------------
for (const t of PAINTED) {
  test(`${t.name}: every gated pair meets its floor`, () => {
    const pairs = PAIRS.get(t.name);
    const failed = pairs.filter((p) => p.value < p.min);
    assert.deepEqual(
      failed.map((p) => `${p.id} ${p.value.toFixed(2)} < ${p.min}`),
      [],
      `${t.name} contrast regressions`,
    );
  });
}

// --- 5. the drift tripwire (±0.05, pair 26) ---------------------------------
// The documented key pairs from PLAN-THEMES §4.1–§4.7 and §5.1 (as built —
// §11/§13 record the folded a11y values that moved `muted` and the two add-fg
// figures re-measured from the alpha blends). A palette edit that stays above
// the gate but moves a documented ratio fails here, so a silent repaint is
// impossible — and the roster is asserted to cover EVERY painted theme, so a
// palette cannot land unmeasured (no exclusions).
//
// The two documented deviations are pinned at their measured values too
// (`line-strong` 1.63/1.56 in the defaults, 3.30/3.25 in the HC palettes;
// `dim` 2.61/3.04 and 4.99/5.04), so neither the deviation nor the HC fix can
// silently drift either way.
const DRIFT = {
  dark: {
    "text/canvas": 15.63,
    "text/surface": 14.75,
    "text-2/canvas": 8.28,
    "muted/surface": 5.14,
    "link/canvas": 15.63,
    "on-accent/accent": 10.51,
    "accent/canvas": 10.49,
    "ok/ok-bg": 6.43,
    "warn/warn-bg": 6.03,
    "danger/danger-bg": 4.51,
    "merged/merged-bg": 4.91,
    "prerelease/prerelease-bg": 5.93,
    "add-fg/canvas": 11.5,
    "del-fg/canvas": 10.49,
    "kw/code-bg": 6.95,
    "line-strong/canvas": 1.63,
    "dim/surface": 2.61,
  },
  light: {
    "text/canvas": 17.77,
    "text-2/canvas": 7.14,
    "muted/surface": 5.08,
    "link/canvas": 17.77,
    "on-accent/accent": 5.43,
    "accent/canvas": 5.43,
    "ok/ok-bg": 4.62,
    "warn/warn-bg": 4.61,
    "danger/danger-bg": 4.69,
    "merged/merged-bg": 5.57,
    "prerelease/prerelease-bg": 5.31,
    "add-fg/canvas": 7.38,
    "del-fg/canvas": 7.89,
    "kw/code-bg": 7.1,
    "line-strong/canvas": 1.56,
    "dim/surface": 3.04,
  },
  "hc-dark": {
    "text/canvas": 17.71,
    "text/surface": 16.71,
    "text-2/canvas": 12.32,
    "muted/surface": 8.18,
    "link/canvas": 17.71,
    "on-accent/accent": 11.77,
    "accent/canvas": 11.75,
    "ok/ok-bg": 6.91,
    "warn/warn-bg": 6.65,
    "danger/danger-bg": 5.63,
    "merged/merged-bg": 6.09,
    "prerelease/prerelease-bg": 6.56,
    "add-fg/canvas": 11.71,
    "del-fg/canvas": 11.36,
    "kw/code-bg": 9.82,
    "line-strong/canvas": 3.3,
    "dim/surface": 4.99,
  },
  "hc-light": {
    "text/canvas": 19.7,
    "text/surface": 18.38,
    "text-2/canvas": 12.58,
    "muted/surface": 7.27,
    "link/canvas": 19.7,
    "on-accent/accent": 7.77,
    "accent/canvas": 7.77,
    "ok/ok-bg": 6.02,
    "warn/warn-bg": 5.79,
    "danger/danger-bg": 5.85,
    "merged/merged-bg": 6.74,
    "prerelease/prerelease-bg": 5.48,
    "add-fg/canvas": 10.85,
    "del-fg/canvas": 9.79,
    "kw/code-bg": 8.98,
    "line-strong/canvas": 3.25,
    "dim/surface": 5.04,
  },
  paper: {
    "text/canvas": 15.1,
    "text/surface": 14.15,
    "text-2/canvas": 7.15,
    "muted/surface": 5.32,
    "link/canvas": 15.1,
    "on-accent/accent": 6.17,
    "accent/canvas": 5.59,
    "ok/ok-bg": 6.2,
    "warn/warn-bg": 5.64,
    "danger/danger-bg": 5.87,
    "merged/merged-bg": 6.86,
    "prerelease/prerelease-bg": 5.4,
    "add-fg/canvas": 10.09,
    "del-fg/canvas": 8.82,
    "kw/code-bg": 8.61,
    "line-strong/canvas": 2.71,
    "dim/surface": 3.0,
  },
  ocean: {
    "text/canvas": 15.17,
    "text/surface": 14.35,
    "text-2/canvas": 9.11,
    "muted/surface": 5.24,
    "link/canvas": 15.17,
    "on-accent/accent": 8.79,
    "accent/canvas": 8.6,
    "ok/ok-bg": 7.05,
    "warn/warn-bg": 6.7,
    "danger/danger-bg": 5.18,
    "merged/merged-bg": 6.18,
    "prerelease/prerelease-bg": 9.39,
    "add-fg/canvas": 11.11,
    "del-fg/canvas": 10.44,
    "kw/code-bg": 8.48,
    "line-strong/canvas": 1.8,
    "dim/surface": 2.96,
  },
  terminal: {
    "text/canvas": 15.21,
    "text/surface": 14.46,
    "text-2/canvas": 9.21,
    "muted/surface": 5.46,
    "link/canvas": 15.21,
    "on-accent/accent": 10.51,
    "accent/canvas": 10.72,
    "ok/ok-bg": 7.4,
    "warn/warn-bg": 5.96,
    "danger/danger-bg": 4.92,
    "merged/merged-bg": 6.18,
    "prerelease/prerelease-bg": 7.08,
    "add-fg/canvas": 11.77,
    "del-fg/canvas": 10.95,
    "kw/code-bg": 8.53,
    "line-strong/canvas": 1.75,
    "dim/surface": 3.09,
  },
};

test("the drift table covers every painted palette (no exclusions)", () => {
  assert.deepEqual(
    Object.keys(DRIFT).sort(),
    PAINTED.map((t) => t.name).sort(),
    "a palette without documented pins is an unmeasured palette",
  );
});

for (const t of PAINTED) {
  test(`${t.name}: documented ratios do not drift (±0.05)`, () => {
    const pairs = PAIRS.get(t.name);
    for (const [id, want] of Object.entries(DRIFT[t.name])) {
      const pair = pairs.find((p) => p.id === id);
      assert.ok(pair, `${t.name}: drift table names unknown pair ${id}`);
      assert.ok(
        Math.abs(pair.value - want) <= 0.05,
        `${t.name} ${id}: computed ${pair.value.toFixed(2)}, documented ${want} — a palette edit must update the plan and this pin together`,
      );
    }
  });
}

// --- 6. ramp monotonicity, scoped per side (pair 24) ------------------------
// Every palette's COMPOSED ramp is asserted on its active side: dark-side
// palettes 400–950, light-side 50–600 (hc-light also 700, which it overrides so
// the ramp stays strict through its darker 600). A whole-ramp check would
// reject correct tables because each theme inherits the other side's steps.
test("the composed zinc ramp is strictly monotonic in every palette, per side", () => {
  const descend = (values, label) => {
    for (let i = 1; i < values.length; i++) {
      assert.ok(
        lum(parseColor(values[i - 1])) > lum(parseColor(values[i])),
        `${label}: step ${i - 1} (${values[i - 1]}) must be lighter than step ${i} (${values[i]})`,
      );
    }
  };
  for (const t of PAINTED) {
    const list = t.side === "dark"
      ? ["400", "500", "600", "700", "800", "900", "950"]
      : ["50", "100", "200", "300", "400", "500", "600"];
    if (t.name === "hc-light") list.push("700");
    descend(list.map((s) => t.ramp[`--color-zinc-${s}`]), `${t.name} ramp`);
  }
});

// --- 6b. the two identity hue rules (PLAN-THEMES §1.3-3/4) ------------------
// Mechanical floor for "amber = action, green = state, links are ink": the
// action accent never lands in the state-green hue band in any palette (that is
// why `terminal` keeps the amber accent instead of a phosphor one), the ok
// state is green-family everywhere, and every link is the theme's ink.
test("every palette keeps green as state, the action accent out of it, and links as ink", () => {
  for (const t of PAINTED) {
    const toks = t.tokens;
    assert.equal(
      toks["--pd-link"].toLowerCase(),
      toks["--pd-text"].toLowerCase(),
      `${t.name}: links must be the theme's ink, never the accent`,
    );
    const okHue = hue(toks["--pd-ok"]);
    assert.ok(okHue >= 140 && okHue <= 180, `${t.name}: --pd-ok ${toks["--pd-ok"]} is not the green state family (hue ${okHue.toFixed(0)})`);
    const accentHue = hue(toks["--pd-accent"]);
    assert.ok(
      accentHue < 140 || accentHue > 180,
      `${t.name}: the action accent ${toks["--pd-accent"]} sits in the state-green hue band — green is state, never action`,
    );
    for (const s of ["ok", "warn", "danger", "merged", "prerelease"]) {
      assert.notEqual(
        toks["--pd-accent"].toLowerCase(),
        toks[`--pd-${s}`].toLowerCase(),
        `${t.name}: the accent must not double as the ${s} state colour`,
      );
    }
  }
});

// --- 6c. every palette's graph halo is its own surface ----------------------
// The commit graph's 2px halo (`box-shadow: 0 0 0 2px var(--graph-ring)`) sits
// on the row ground; a palette that keeps another side's halo draws a ring
// around every lane dot. Pinned for all seven (the defaults already comply).
test("every palette's graph-ring is its own surface", () => {
  for (const t of PAINTED) {
    assert.equal(
      t.graph["--graph-ring"].toLowerCase(),
      t.tokens["--pd-surface"].toLowerCase(),
      `${t.name}: --graph-ring must equal --pd-surface`,
    );
  }
});

// --- 7. the raw-ramp grep (pair 25) ----------------------------------------
function walk(dir, suffix, out = []) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, suffix, out);
    else if (entry.endsWith(suffix)) out.push(full);
  }
  return out;
}

// readTemplate finds a template by basename anywhere under the templates
// tree. A hand-written path pin fails with ENOENT the day the template is
// split into a partial (settings_repo.html → per-tab partials); the markup
// still has exactly one home, so assert that and say where you looked.
function readTemplate(name) {
  const dir = join(root, "internal", "webui", "templates");
  const hits = walk(dir, ".html").filter((p) => p.slice(p.lastIndexOf("/") + 1) === name);
  assert.equal(
    hits.length,
    1,
    `expected exactly one ${name} under internal/webui/templates, found ${hits.length}: ` +
      `${hits.join(", ") || "(none)"} — the template that owns this markup moved or was split; ` +
      "point this test at the file that owns it now",
  );
  return readFileSync(hits[0], "utf8");
}

test("no raw emerald/amber colour utility in templates or Go", () => {
  const files = [
    ...walk(join(root, "internal", "webui", "templates"), ".html"),
    ...readdirSync(join(root, "internal", "webui"))
      .filter((f) => f.endsWith(".go"))
      .map((f) => join(root, "internal", "webui", f)),
  ];
  const offenders = [];
  for (const file of files) {
    const body = readFileSync(file, "utf8");
    const re = /(?:^|[^a-zA-Z0-9_-])(?:[a-z-]+:)*(?:bg|text|border(?:-[lrtbxy])?|ring|fill|stroke|accent|divide|outline|decoration|shadow|from|via|to|placeholder|caret)-(?:emerald|amber)-\d{2,3}/m;
    if (re.test(body)) offenders.push(file.slice(root.length + 1));
  }
  assert.deepEqual(offenders, [], "raw emerald/amber utilities (green/amber are semantic tokens: --pd-ok/--pd-warn/--pd-accent)");
});

test("the built stylesheet keeps utilities as ramp variable references", (t) => {
  const dist = join(root, "web", "dist", "app.css");
  if (!existsSync(dist)) {
    // A build artifact, not a source check: CI builds the web before running
    // the web tier (AGENTS §2 field lesson). Locally, run `make web` first.
    t.diagnostic("web/dist/app.css is absent — run `make web` to assert the built cascade");
    return;
  }
  const built = readFileSync(dist, "utf8");
  assert.match(built, /var\(--color-zinc-\d+\)/, "utilities must compile to ramp variable references, not inlined values");
  assert.doesNotMatch(built, /emerald|amber/i, "the built CSS must carry no emerald/amber utility (the raw ramps are deleted)");
});

// =============================================================================
// Selection machinery (PLAN-THEMES §7, W2)
//
// One registry, three consumers: `web/src/islands/themes.js` (ids, sides,
// swatch previews) is the data; `theme-boot.js` resolves before first paint;
// `theme_menu.html` is the server-rendered radio group; the `[data-theme]`
// blocks in app.css paint. These tests tie them together — a theme added to
// one and not the others fails, and a swatch is never a hand-typed colour.
// =============================================================================
import {
  THEMES,
  SYSTEM,
  DEFAULT_THEME,
  themeById,
  isThemeId,
  resolveTheme,
  resolveChoice,
  applyTheme,
} from "../../src/islands/themes.js";

const REGISTRY = THEMES.map((t) => t.id).sort();

// Every theme the CSS actually paints: the two defaults plus each [data-theme]
// block. W3 adds palettes as data — these assertions extend with them.
function cssThemes() {
  const out = new Map([
    ["light", LIGHT_TOKENS],
    ["dark", DARK_TOKENS],
  ]);
  for (const r of RULES.filter((x) => x.selector.trim().startsWith("[data-theme"))) {
    const m = r.selector.match(/\[data-theme="([^"]+)"\]/);
    assert.ok(m, `unparseable theme selector ${r.selector}`);
    out.set(m[1], tokensOf(declarations(r.body)));
  }
  return out;
}

test("the registry and the CSS theme blocks list exactly the same themes", () => {
  const css = cssThemes();
  assert.deepEqual([...css.keys()].sort(), REGISTRY, "registry ↔ [data-theme] blocks drifted");
  for (const theme of THEMES) {
    const tokens = css.get(theme.id);
    assert.ok(tokens, `${theme.id} has no token block`);
    // The side is derived from the measured canvas, never trusted: a dark-side
    // theme must keep the .dark class so every `dark:` utility fires.
    const side = lum(parseColor(tokens["--pd-canvas"])) > 0.5 ? "light" : "dark";
    assert.equal(theme.side, side, `${theme.id}: registry side ${theme.side} but its canvas is ${side}`);
    if (theme.id === "light") assert.equal(theme.side, "light");
    if (theme.id === "dark") assert.equal(theme.side, "dark");
  }
});

test("every theme carries a complete swatch derived from its own tokens", () => {
  const css = cssThemes();
  for (const theme of THEMES) {
    assert.ok(theme.swatch, `${theme.id} has no swatch preview — the preview is part of the registry`);
    assert.deepEqual(
      Object.keys(theme.swatch).sort(),
      ["accent", "canvas", "text"],
      `${theme.id}: the swatch is exactly canvas / text / accent`,
    );
    const tokens = css.get(theme.id);
    for (const [chip, token] of [
      ["canvas", "--pd-canvas"],
      ["text", "--pd-text"],
      ["accent", "--pd-accent"],
    ]) {
      assert.doesNotThrow(() => parseColor(theme.swatch[chip]), `${theme.id}.${chip} is not a colour`);
      assert.equal(
        theme.swatch[chip].toLowerCase(),
        tokens[token].toLowerCase(),
        `${theme.id}.${chip} swatch ${theme.swatch[chip]} ≠ ${token} ${tokens[token]}`,
      );
    }
  }
});

test("the header menu lists every registry theme plus match-system, and nothing else", () => {
  const menu = readTemplate("theme_menu.html").replace(/\{\{\/\*[\s\S]*?\*\/\}\}/g, ""); // template comments are prose, not markup
  const keys = [...menu.matchAll(/data-theme-option="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(
    [...new Set(keys)].sort(),
    [SYSTEM, ...REGISTRY].sort(),
    "theme_menu.html ↔ registry drifted: a theme added to one must be added to the other",
  );
  assert.equal(keys.length, new Set(keys).size, "duplicate data-theme-option rows");
  assert.equal(
    (menu.match(/role="menuitemradio"/g) ?? []).length,
    keys.length,
    "every row is a menuitemradio (the radio-group model, §8.2)",
  );
  assert.match(menu, /role="menu"/, "the panel is a menu");
  assert.match(menu, /Match system/, "the match-system entry is labelled");
  // The preview is generated from the registry: no colour may be typed here.
  assert.doesNotMatch(menu, /#[0-9a-fA-F]{3,8}\b/, "the menu template must not carry a colour literal");
  // One icon trigger with an accessible name (§4b), pointed at the panel.
  assert.match(menu, /data-menu-trigger/, "the rows need the menus.js trigger");
  assert.match(menu, /aria-label="Theme"/, "the trigger and panel carry a name");
  assert.match(menu, /aria-haspopup="menu"/);
});

test("theme-boot.js is external, parser-blocking markup and carries no palette", () => {
  const boot = readFileSync(join(root, "web", "src", "theme-boot.js"), "utf8");
  assert.doesNotMatch(boot, /#[0-9a-fA-F]{3,8}\b/, "the boot script must not carry colour literals");
  assert.match(boot, /from "\.\/islands\/themes\.js"/, "boot reads the one registry");
  assert.match(boot, /resolveChoice\(/, "storage-denied resolution goes through the shared path");
  assert.match(boot, /applyTheme\(/, "the <html> write is the shared one");
  // Ids live in the registry, not the script (comments are prose, not code).
  const bootCode = boot.replace(/\/\/[^\n]*/g, "");
  for (const id of REGISTRY) {
    assert.ok(
      !bootCode.includes(`"${id}"`),
      `theme-boot.js hardcodes the id ${id}; ids live in the registry`,
    );
  }

  const base = readTemplate("base.html");
  // No-JS floor: the server renders the product default on <html>.
  assert.match(base, /<html lang="en" class="dark" data-theme="dark">/, "SSR default theme missing");
  // Classic script (no type=module/defer/async), from 'self', after the
  // stylesheet: it must run before first paint and CSP stays `script-src 'self'`.
  const tag = base.match(/<script src="\/_ui\/theme-boot\.js"><\/script>/);
  assert.ok(tag, "base.html must load /_ui/theme-boot.js as a classic script");
  assert.ok(
    base.indexOf("/_ui/app.css") < base.indexOf("/_ui/theme-boot.js"),
    "the boot script must follow the stylesheet link",
  );
  assert.ok(
    base.indexOf("/_ui/theme-boot.js") < base.indexOf("/_ui/ui.js"),
    "the boot script must precede the island bundle",
  );
  assert.doesNotMatch(base, /theme-boot\.js"[^>]*(defer|async|type="module")/, "boot must stay parser-blocking");
});

test("theme resolution follows PLAN-THEMES §7.1, with explicit choices winning", () => {
  // §10.5-1: the stored choice always wins, including over OS contrast.
  assert.equal(resolveTheme({ stored: "light", prefersDark: true, prefersContrast: true }), "light");
  assert.equal(resolveTheme({ stored: "dark", prefersDark: false }), "dark");
  // System mode: the OS side; a stored "system" behaves exactly like no key.
  assert.equal(resolveTheme({ stored: SYSTEM, prefersDark: true }), "dark");
  assert.equal(resolveTheme({ stored: SYSTEM, prefersDark: false }), "light");
  assert.equal(resolveTheme({}), DEFAULT_THEME, "no matchMedia / no preference lands on the default");
  assert.equal(resolveTheme({ stored: "banana", prefersDark: false }), "light", "a stale id resolves like system");
  // §7.5: prefers-contrast upgrades system mode to the side's HC palette when
  // that palette exists; W3 adds hc-dark/hc-light as data and this follows.
  const hcDark = isThemeId("hc-dark") ? "hc-dark" : "dark";
  const hcLight = isThemeId("hc-light") ? "hc-light" : "light";
  assert.equal(resolveTheme({ prefersDark: true, prefersContrast: true }), hcDark);
  assert.equal(resolveTheme({ prefersDark: false, prefersContrast: true }), hcLight);
  assert.equal(resolveTheme({ prefersDark: true, prefersContrast: false }), "dark");
  assert.equal(resolveTheme({ stored: "light", prefersDark: true, prefersContrast: true }), "light");
});

test("storage denial resolves like an absent value and never throws", () => {
  const denied = () => {
    throw new Error("SecurityError: storage denied");
  };
  assert.equal(resolveChoice(denied, { prefersDark: false }), "light");
  assert.equal(resolveChoice(denied, { prefersDark: true }), "dark");
  assert.equal(resolveChoice(denied, {}), DEFAULT_THEME);
  assert.equal(resolveChoice(() => "system", { prefersDark: false }), "light");
  assert.equal(resolveChoice(() => "dark", { prefersDark: false }), "dark");
  assert.equal(resolveChoice(() => "hc-nope", { prefersDark: true }), "dark");
});

test("applyTheme writes the id and the side class together, and degrades to the default", () => {
  const stub = () => {
    const classes = new Set();
    const el = {
      attrs: {},
      setAttribute(k, v) {
        el.attrs[k] = v;
      },
      classList: {
        toggle(name, on) {
          if (on) classes.add(name);
          else classes.delete(name);
        },
        contains: (name) => classes.has(name),
      },
    };
    return { doc: { documentElement: el }, el, classes };
  };
  for (const theme of THEMES) {
    const { doc, el, classes } = stub();
    assert.equal(applyTheme(doc, theme.id), theme.id);
    assert.equal(el.attrs["data-theme"], theme.id);
    assert.equal(classes.has("dark"), theme.side === "dark", `${theme.id} side class`);
  }
  const { doc, el, classes } = stub();
  assert.equal(applyTheme(doc, "no-such-theme"), DEFAULT_THEME);
  assert.equal(el.attrs["data-theme"], DEFAULT_THEME);
  assert.ok(classes.has("dark"), "the default is a dark-side theme");
  // Switching sides clears the class as well as setting the attribute.
  applyTheme(doc, "light");
  assert.equal(el.attrs["data-theme"], "light");
  assert.ok(!classes.has("dark"));
});

// -----------------------------------------------------------------------------
// Print and forced colours (PLAN-THEMES §8.3, W2)
// -----------------------------------------------------------------------------
function mediaRule(selector) {
  const rule = RULES.find((r) => r.selector.trim() === selector);
  assert.ok(rule, `missing ${selector}`);
  return rule.body;
}

// The first declaration block after a selector prefix inside a rule body.
function nestedBlock(body, prefix) {
  const i = body.indexOf(prefix);
  assert.ok(i >= 0, `missing nested block ${prefix}`);
  const open = body.indexOf("{", i);
  let depth = 1;
  let j = open + 1;
  while (j < body.length && depth > 0) {
    if (body[j] === "{") depth++;
    else if (body[j] === "}") depth--;
    if (depth === 0) break;
    j++;
  }
  return body.slice(open + 1, j);
}

// The paper palette's deliberate departures from :root: no panel fills, no
// chip grounds, no shadow. Everything else is the measured light palette.
const PRINT_OVERRIDES = {
  "--pd-surface": "#ffffff",
  "--pd-ok-bg": "transparent",
  "--pd-warn-bg": "transparent",
  "--pd-danger-bg": "transparent",
  "--pd-merged-bg": "transparent",
  "--pd-prerelease-bg": "transparent",
  "--pd-info-bg": "transparent",
  "--pd-add-bg": "transparent",
  "--pd-del-bg": "transparent",
  "--pd-sel": "transparent",
  "--pd-shadow": "none",
};

const PRINT_TOKENS = (() => {
  const body = nestedBlock(mediaRule("@media print"), ":root, .dark, [data-theme]");
  const decls = declarations(body);
  // Declaration-only: the paper block may set custom properties and nothing else.
  for (const decl of body.split(";")) {
    const d = decl.trim();
    if (!d) continue;
    assert.match(d, /^--[a-zA-Z0-9-]+\s*:/, `print block: non-custom-property declaration "${d}"`);
  }
  return decls;
})();

test("the print palette is the light palette, token for token, minus its grounds", () => {
  for (const tok of TOKEN_NAMES) {
    const want = tok in PRINT_OVERRIDES ? PRINT_OVERRIDES[tok] : LIGHT_TOKENS[tok];
    assert.equal(
      PRINT_TOKENS[tok],
      want,
      `print ${tok}: ${PRINT_TOKENS[tok]} ≠ ${want} (print must mirror :root so it cannot drift)`,
    );
  }
  // The full light ramp and graph lanes print on paper — a palette's own
  // ramp (paper's sand, ocean's navy, terminal's green, the HC steps) never
  // survives into print, so every theme prints the same measured palette.
  for (const step of ["50", "100", "200", "300", "400", "500", "600", "700", "800", "900", "950"]) {
    assert.equal(
      PRINT_TOKENS[`--color-zinc-${step}`],
      LIGHT_RAMP[`--color-zinc-${step}`],
      `print zinc-${step} must be the light ramp`,
    );
  }
  for (let i = 0; i < 8; i++) {
    assert.equal(PRINT_TOKENS[`--graph-${i}`], LIGHT_GRAPH[`--graph-${i}`]);
  }
  // No colour literal may hide in the print block's rules: paper is the
  // declared palette, not a second one.
  const body = mediaRule("@media print");
  const rest = body.slice(body.indexOf("}") + 1);
  assert.doesNotMatch(rest, /#[0-9a-fA-F]{3,8}\b/, "print rules must not introduce colour literals");
  assert.doesNotMatch(rest, /--pd-[a-zA-Z0-9-]+\s*:/, "print rules must not re-declare a token outside the block");
  assert.match(body, /\.theme-menu\s*,\s*[^{]*\{[^}]*display:\s*none/, "the theme menu is not printed");
  assert.match(body, /\.status-strip\s*,\s*\.progress/, "the operated-tool chrome is not printed");
});

test("the print palette passes the same contrast shape on paper", () => {
  const paper = (tok) => PRINT_TOKENS[tok];
  for (const tok of [
    "--pd-text",
    "--pd-text-2",
    "--pd-muted",
    "--pd-link",
    "--pd-warn",
    "--pd-danger",
    "--pd-ok",
    "--pd-merged",
    "--pd-prerelease",
    "--pd-kw",
    "--pd-add-fg",
    "--pd-del-fg",
  ]) {
    const value = ratio(paper(tok), paper("--pd-canvas"));
    assert.ok(value >= 4.5, `print ${tok}: ${value.toFixed(2)} < 4.5 on paper`);
  }
  assert.ok(ratio(paper("--pd-accent"), paper("--pd-canvas")) >= 3, "print accent ring");
});

test("forced colours let the OS own the palette and keep the structural channels", () => {
  const body = mediaRule("@media (forced-colors: active)");
  assert.match(body, /forced-color-adjust:\s*auto/, "the default must be stated, not silently dropped");
  // No palette may leak into forced-colours: only the shadow is neutralised.
  const pdDecls = [...body.matchAll(/(--pd-[a-zA-Z0-9-]+)\s*:\s*([^;]+);/g)];
  assert.deepEqual(
    pdDecls.map((m) => `${m[1]}:${m[2].trim()}`),
    ["--pd-shadow:none"],
    "forced-colours declares no colour tokens",
  );
  assert.doesNotMatch(body, /#[0-9a-fA-F]{3,8}\b/, "forced-colours must not set colours");
  // Borders and underlines stay meaningful without tints (§6.1).
  assert.match(body, /text-decoration:\s*underline/, "links stay identifiable");
  assert.match(body, /\.btn[^{]*\{[^}]*border:\s*1px solid/, "controls keep a border");
  assert.match(body, /\.diff-num\s*\{[^}]*border/, "the diff gutter keeps its divider");
});
