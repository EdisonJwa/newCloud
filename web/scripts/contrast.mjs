// Contrast audit.
//
// WCAG 2.2 AA is a claim this project makes in the documentation. A claim about colour is
// cheap to assert and easy to get wrong by eye, so it is measured here instead.
//
// ## What changed with the Serein UI integration
//
// The palette is no longer declared in this application: it is `src/serein/tokens.css`, the
// vendored design-system token layer, which defines four complete appearances as four
// assignments of one semantic role set. So this script reads the tokens from that file rather
// than from a local `:root` block, and it measures all four appearances rather than three.
//
// The pairs below are the ones the interface actually relies on. Anything that only draws a
// decorative divider is deliberately absent: WCAG 1.4.11 governs visual information needed to
// identify a control, not every hairline on the page.
//
// Two pairs are new and are the reason this audit is worth re-running:
//
//  * `--serein-text-tertiary` on `--serein-fill-subtle` — the row subtitle sits on a sunken
//    surface in the empty-state and picker chrome, and tertiary text on a fill is the lowest
//    contrast pairing in the whole system.
//  * `--serein-accent-ink` on `--serein-accent` — the primary button. A dark-appearance accent
//    is a light blue, so its "ink" is dark; getting that backwards produces a button whose
//    label is invisible in one appearance only.
//
// Run with: node scripts/contrast.mjs   (exit 1 if any pair misses its target)

import { readFileSync } from "node:fs";

const css = readFileSync(new URL("../src/serein/tokens.css", import.meta.url), "utf8");

/** Extract the custom properties from a selector block, following `var()` one level. */
function tokens(selector, source = css) {
  const start = source.indexOf(selector + " {");
  if (start < 0) throw new Error(`selector not found: ${selector}`);
  // A block ends at the first line that is exactly "}" — the token file is formatted that way,
  // and a naive indexOf("}") would stop at the first nested rule.
  const end = source.indexOf("\n}", start);
  if (end < 0) throw new Error(`unterminated block: ${selector}`);
  const body = source.slice(start, end);
  const out = {};
  for (const line of body.split("\n")) {
    const m = line.match(/^\s*(--[a-z0-9-]+):\s*([^;]+);/);
    if (m) out[m[1]] = m[2].trim();
  }
  return out;
}

/**
 * resolve follows a `var(--x)` reference.
 *
 * The token file aliases several roles (`--serein-divider: color-mix(...)`,
 * `--serein-divider-strong: var(--serein-border)`), and a checker that treated an alias as an
 * unparsable colour would silently skip the pairs that matter most.
 */
function resolve(value, theme) {
  const alias = value.match(/^var\((--[a-z0-9-]+)\)$/);
  if (alias) {
    const next = theme[alias[1]];
    if (next === undefined) throw new Error(`unresolved alias ${alias[1]}`);
    return resolve(next, theme);
  }
  return value;
}

const THEMES = {
  light: tokens('.serein-root', stripMedia(css, "@media (prefers-color-scheme: dark)")),
  dark: tokens('.serein-root[data-theme="dark"]'),
  "light-hc": {
    ...tokens('.serein-root', stripMedia(css, "@media (prefers-color-scheme: dark)")),
    ...tokens('.serein-root[data-theme="light"][data-contrast="more"]'),
  },
  "dark-hc": {
    ...tokens('.serein-root[data-theme="dark"]'),
    ...tokens('.serein-root[data-theme="dark"][data-contrast="more"]'),
  },
};

/**
 * stripMedia removes the system-preference block for `prefers-color-scheme: dark`.
 *
 * The plain `.serein-root` selector is the light appearance, and the dark one inside the media
 * query is the same assignments as `[data-theme="dark"]`. Reading the light block without the
 * media query is what makes "light" mean light on a machine set to dark.
 */
function stripMedia(source, marker) {
  const start = source.indexOf(marker);
  if (start < 0) return source;
  // The block is balanced: find the closing brace at the same depth.
  let depth = 0;
  let i = source.indexOf("{", start);
  const from = i;
  for (; i < source.length; i++) {
    if (source[i] === "{") depth++;
    else if (source[i] === "}") {
      depth--;
      if (depth === 0) break;
    }
  }
  return source.slice(0, from) + source.slice(i + 1);
}

function parseColor(value) {
  const hex = value.match(/^#([0-9a-f]{6})$/i);
  if (hex) {
    const n = parseInt(hex[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1];
  }
  const short = value.match(/^#([0-9a-f]{3})$/i);
  if (short) {
    const [r, g, b] = short[1].split("");
    return [parseInt(r + r, 16), parseInt(g + g, 16), parseInt(b + b, 16), 1];
  }
  const rgba = value.match(/^rgba?\(([^)]+)\)$/);
  if (rgba) {
    const parts = rgba[1].split(/[,/]/).map((s) => parseFloat(s.trim()));
    return [parts[0], parts[1], parts[2], parts[3] ?? 1];
  }
  const mixed = value.match(/^color-mix\(in srgb,\s*(.+?)\s+(\d+)%,\s*(.+?)\)$/);
  if (mixed) {
    // One level of color-mix is enough for the pairs below; a nested one is an error rather
    // than a silently-skipped row.
    const a = parseColor(resolve(mixed[1], current));
    const weight = parseInt(mixed[2], 10) / 100;
    const b = parseColor(resolve(mixed[3], current));
    return [
      a[0] * weight + b[0] * (1 - weight),
      a[1] * weight + b[1] * (1 - weight),
      a[2] * weight + b[2] * (1 - weight),
      a[3] * weight + b[3] * (1 - weight),
    ];
  }
  throw new Error(`unsupported colour: ${value}`);
}

function luminance([r, g, b]) {
  const channel = (v) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

/** Composite an alpha colour over a backdrop. */
function over(fg, bg) {
  const a = fg[3];
  if (a >= 1) return fg;
  return [fg[0] * a + bg[0] * (1 - a), fg[1] * a + bg[1] * (1 - a), fg[2] * a + bg[2] * (1 - a), 1];
}

let current = {};

function contrast(fgName, bgName) {
  // The pair table names tokens, not values, so the lookup happens here where the current
  // appearance is in scope. Alias chains are followed by `resolve`.
  const bg = parseColor(resolve(current[bgName], current));
  const fg = over(parseColor(resolve(current[fgName], current)), bg);
  const l1 = luminance(fg);
  const l2 = luminance(bg);
  const [hi, lo] = l1 > l2 ? [l1, l2] : [l2, l1];
  return (hi + 0.05) / (lo + 0.05);
}

// [foreground, background, minimum, what it is]
const PAIRS = [
  ["--serein-text", "--serein-canvas", 4.5, "body text on the page"],
  ["--serein-text", "--serein-surface", 4.5, "body text on a panel"],
  ["--serein-text-secondary", "--serein-surface", 4.5, "secondary text on a panel"],
  ["--serein-text-secondary", "--serein-canvas", 4.5, "secondary text on the page"],
  ["--serein-text-tertiary", "--serein-surface", 4.5, "metadata and column labels"],
  ["--serein-text-tertiary", "--serein-canvas", 4.5, "metadata on the page background"],
  ["--serein-text-tertiary", "--serein-fill-subtle", 4.5, "row subtitle on a sunken surface"],
  ["--serein-accent", "--serein-surface", 4.5, "links and accent text"],
  ["--serein-danger", "--serein-surface", 4.5, "destructive actions"],
  ["--serein-danger", "--serein-danger-soft", 4.5, "the denied badge"],
  ["--serein-success", "--serein-success-soft", 4.5, "the restored badge"],
  ["--serein-warning", "--serein-warning-soft", 4.5, "the offline banner"],
  ["--serein-info", "--serein-info-soft", 4.5, "informational callouts"],
  ["--serein-accent-ink", "--serein-accent", 4.5, "primary button label"],
  ["--serein-danger-ink", "--serein-danger", 4.5, "danger button label"],
  ["--serein-text-inverse", "--serein-text", 4.5, "the drag ghost and skip link"],
  ["--serein-focus-ring", "--serein-canvas", 3.0, "the focus ring (UI, 1.4.11)"],
  ["--serein-border-strong", "--serein-surface", 3.0, "input and button borders (1.4.11)"],
];

let failures = 0;
const rows = [];

for (const [name, theme] of Object.entries(THEMES)) {
  current = theme;
  for (const [fg, bg, min, why] of PAIRS) {
    if (!theme[fg] || !theme[bg]) {
      rows.push([name, fg, bg, "missing token", min, "FAIL", why]);
      failures += 1;
      continue;
    }
    let ratio;
    try {
      ratio = contrast(fg, bg);
    } catch (err) {
      rows.push([name, fg, bg, `unparsable: ${err.message}`, min, "FAIL", why]);
      failures += 1;
      continue;
    }
    const pass = ratio >= min;
    if (!pass) failures += 1;
    rows.push([name, fg, bg, ratio.toFixed(2), min, pass ? "pass" : "FAIL", why]);
  }
}

const width = [10, 26, 26, 8, 5, 6];
const pad = (s, w) => String(s).padEnd(w);
console.log(
  pad("theme", width[0]) +
    pad("foreground", width[1]) +
    pad("background", width[2]) +
    pad("ratio", width[3]) +
    pad("min", width[4]) +
    pad("", width[5]) +
    "what",
);
for (const row of rows) {
  console.log(
    pad(row[0], width[0]) +
      pad(row[1], width[1]) +
      pad(row[2], width[2]) +
      pad(row[3], width[3]) +
      pad(row[4], width[4]) +
      pad(row[5], width[5]) +
      row[6],
  );
}

if (failures > 0) {
  console.error(`\n${failures} contrast pair(s) below target`);
  process.exit(1);
}
console.log(
  `\nall ${rows.length} contrast pairs meet their WCAG 2.2 AA target across ${Object.keys(THEMES).length} appearances`,
);
