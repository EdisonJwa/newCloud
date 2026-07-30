// Contrast audit.
//
// WCAG 2.2 AA is a claim this project makes in docs/0006 and in the README. A claim about
// colour is cheap to assert and easy to get wrong by eye, so it is measured here instead.
//
// The pairs below are the ones the interface actually relies on. Anything that only draws a
// decorative divider is deliberately absent: WCAG 1.4.11 governs visual information needed to
// identify a control, not every hairline on the page.
//
// Run with: node scripts/contrast.mjs   (exit code 1 if any pair misses its target)

import { readFileSync } from "node:fs";

const css = readFileSync(new URL("../src/styles.css", import.meta.url), "utf8");

/** Extract a custom-property block by selector. */
function tokens(selector) {
  const start = css.indexOf(selector + " {");
  if (start < 0) throw new Error(`selector not found: ${selector}`);
  const end = css.indexOf("\n}", start);
  const body = css.slice(start, end);
  const out = {};
  for (const line of body.split("\n")) {
    const m = line.match(/^\s*(--[a-z0-9-]+):\s*([^;]+);/);
    if (m) out[m[1]] = m[2].trim();
  }
  return out;
}

const THEMES = {
  light: tokens(":root"),
  dark: tokens('[data-theme="dark"]'),
  hc: tokens('[data-theme="hc"]'),
};

function parseColor(value) {
  const hex = value.match(/^#([0-9a-f]{6})$/i);
  if (hex) {
    const n = parseInt(hex[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1];
  }
  const rgba = value.match(/rgba?\(([^)]+)\)/);
  if (rgba) {
    const parts = rgba[1].split(",").map((s) => parseFloat(s.trim()));
    return [parts[0], parts[1], parts[2], parts[3] ?? 1];
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
  return [
    fg[0] * a + bg[0] * (1 - a),
    fg[1] * a + bg[1] * (1 - a),
    fg[2] * a + bg[2] * (1 - a),
    1,
  ];
}

function contrast(fgValue, bgValue, backdrop) {
  const base = parseColor(bgValue);
  const fg = over(parseColor(fgValue), backdrop ? over(parseColor(backdrop), base) : base);
  const bg = backdrop ? over(parseColor(bgValue), parseColor(backdrop)) : base;
  const l1 = luminance(fg);
  const l2 = luminance(bg);
  const [hi, lo] = l1 > l2 ? [l1, l2] : [l2, l1];
  return (hi + 0.05) / (lo + 0.05);
}

// [foreground, background, minimum, what it is]
const PAIRS = [
  ["--ink", "--bg", 4.5, "body text on the page"],
  ["--ink", "--surface", 4.5, "body text on a panel"],
  ["--ink-2", "--surface", 4.5, "secondary text"],
  ["--ink-3", "--surface", 4.5, "metadata and column labels"],
  ["--ink-3", "--bg", 4.5, "metadata on the page background"],
  ["--ink-3", "--surface-2", 4.5, "metadata on a sunken row"],
  ["--accent", "--surface", 4.5, "links and the accent text"],
  ["--danger", "--surface", 4.5, "destructive actions"],
  ["--warn", "--warn-wash", 4.5, "the offline banner"],
  ["--ink-inv", "--accent", 4.5, "primary button label"],
  ["--focus", "--bg", 3.0, "the focus ring (UI, 1.4.11)"],
  ["--line-control", "--surface", 3.0, "input and button borders (1.4.11)"],
];

let failures = 0;
const rows = [];

for (const [name, theme] of Object.entries(THEMES)) {
  for (const [fg, bg, min, why] of PAIRS) {
    if (!theme[fg] || !theme[bg]) {
      rows.push([name, fg, bg, "missing token", min, why]);
      failures += 1;
      continue;
    }
    const ratio = contrast(theme[fg], theme[bg]);
    const pass = ratio >= min;
    if (!pass) failures += 1;
    rows.push([name, fg, bg, ratio.toFixed(2), min, pass ? "pass" : "FAIL", why]);
  }
}

const width = [6, 16, 16, 7, 5, 6];
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
console.log("\nall contrast pairs meet their WCAG 2.2 AA target");
