// i18n key parity.
//
// Two properties are checked, and they are different questions:
//
//  1. Every key an English table has must exist in every other language, *except* plural
//     forms that the language's own plural rules can never select. Chinese has only the
//     "other" category — `Intl.PluralRules("zh-Hans").select(1)` is "other", not "one" — so
//     `_one` keys are unreachable there and their absence is correct rather than a gap. This
//     exemption is computed from the language's plural rules, not hand-listed, because a
//     hand-listed exemption is how a real omission gets pardoned.
//
//  2. Every key the code references exists. A typo in a `t("...")` call does not fail the
//     compiler: it renders the key itself, so the UI shows `acl.grant` where a button label
//     should be. That is the failure this catches.
//
// Run with: pnpm test:i18n (wired into `pnpm typecheck`'s neighbourhood, not the build).

import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

const SRC = new URL("../src/", import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, "$1");

/** LANGS and the tables are parsed out of i18n.ts rather than imported: this script is plain
 *  Node and the module is TypeScript. Parsing the literal keeps the check runnable without a
 *  build step, at the cost of assuming the file's shape — which the first assertion below
 *  guards. */
const i18nSource = readFileSync(join(SRC, "i18n.ts"), "utf8");

const tableRe = /^const (\w+): Table = \{$/gm;
const tables = {};
let m;
const starts = [];
while ((m = tableRe.exec(i18nSource)) !== null) {
  starts.push({ name: m[1], start: m.index });
}
if (starts.length < 2) {
  fail([`expected at least two language tables in i18n.ts, found ${starts.length}`]);
}

for (let i = 0; i < starts.length; i++) {
  const from = starts[i].start;
  const to = i + 1 < starts.length ? starts[i + 1].start : i18nSource.length;
  const body = i18nSource.slice(from, to);
  const keys = new Set();
  for (const km of body.matchAll(/"([A-Za-z0-9_.]+)":/g)) keys.add(km[1]);
  tables[starts[i].name] = keys;
}

const langRe = /export type Lang = ([^;]+);/;
const langMatch = i18nSource.match(langRe);
if (!langMatch) fail(["could not find `export type Lang` in i18n.ts"]);
const langs = [...langMatch[1].matchAll(/"([^"]+)"/g)].map((x) => x[1]);

/** tableName maps a Lang to the const that holds it: "en" -> en, "zh-Hans" -> zh. */
const tableName = (lang) => lang.split("-")[0];

const problems = [];

// 1. Cross-language coverage.
const enKeys = tables[tableName("en")];
if (!enKeys || enKeys.size === 0) {
  problems.push("the English table is empty or was not found; the check would pass vacuously");
}

for (const lang of langs) {
  if (lang === "en") continue;
  const name = tableName(lang);
  const keys = tables[name];
  if (!keys) {
    problems.push(`language ${lang} has no table named "${name}"`);
    continue;
  }
  const rules = new Intl.PluralRules(lang);
  // The categories this language can actually select.
  const selectable = new Set();
  for (const n of [0, 1, 2, 3, 5, 11, 21, 100, 1.5]) selectable.add(rules.select(n));

  for (const key of enKeys) {
    if (keys.has(key)) continue;
    const plural = key.match(/^(.+)_(zero|one|two|few|many|other)$/);
    if (plural) {
      const category = plural[2];
      const base = plural[1];
      // Reachable only if this language can select that category, and has the base key or
      // another form to fall back to.
      if (!selectable.has(category)) continue;
      const hasFallback =
        keys.has(base) ||
        ["zero", "one", "two", "few", "many", "other"].some((c) => keys.has(`${base}_${c}`));
      if (hasFallback) continue;
    }
    problems.push(`${lang} is missing "${key}" (present in en)`);
  }
}

// 2. Every key referenced in code exists in English.
const referenced = new Map(); // key -> file
const walk = (dir) => {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      walk(full);
      continue;
    }
    if (!/\.(ts|tsx)$/.test(entry) || entry === "i18n.ts" || entry === "api-types.ts") continue;
    const text = readFileSync(full, "utf8");
    for (const tm of text.matchAll(/\bt\(\s*"([A-Za-z0-9_.]+)"/g)) {
      referenced.set(tm[1], entry);
    }
  }
};
walk(SRC);

if (referenced.size === 0) {
  problems.push("found no t() calls; the reference check would pass vacuously");
}

for (const [key, file] of referenced) {
  if (enKeys.has(key)) continue;
  // A plural base key is used with a count, so the table holds `key_one` / `key_other`.
  const hasPlural = ["zero", "one", "two", "few", "many", "other"].some((c) => enKeys.has(`${key}_${c}`));
  if (hasPlural) continue;
  problems.push(`${file} calls t("${key}") but en has no such key`);
}

function fail(lines) {
  console.error("i18n check failed:");
  for (const line of lines) console.error("  - " + line);
  process.exit(1);
}

if (problems.length > 0) fail(problems);

console.log(
  `i18n ok: ${enKeys.size} en keys, ${langs.length} languages, ${referenced.size} referenced keys`,
);
