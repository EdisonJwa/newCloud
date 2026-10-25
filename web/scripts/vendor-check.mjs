// Vendor provenance.
//
// `src/serein/` is a verbatim copy of the design system at `cirrus-ui/dist/`. The copy exists
// because the package is not published to a registry and `web/` builds from a frozen lockfile
// in a container where a sibling checkout does not exist — see `src/serein/README.md`.
//
// A verbatim copy has one failure mode that matters: somebody edits it. A framework edited in
// place drifts from the approval silently, and the next person reads the drift as the design.
// Worse, the contrast audit and the design-parity expectations read token values out of these
// files, so a local tweak makes them assert the tweak rather than the approved values.
//
// So the hashes are recorded and checked. `node scripts/vendor-check.mjs` fails when a vendored
// file changed, and the fix is to change it upstream and re-copy, not to regenerate the hash
// here. The README's table is the record of what was copied and when.

import { createHash } from "node:crypto";
import { readFileSync, statSync } from "node:fs";

const ROOT = new URL("../", import.meta.url);

/**
 * The recorded hashes. `null` means "not hashed, reviewed as a binary asset": the font files
 * are the design system's own licensed faces, copied byte for byte, and hashing them here would
 * duplicate the package manifest without adding a check anyone would read.
 */
const VENDORED = [
  { path: "src/serein/serein-ui.css", sha256: "9ef00c4f5fe4454cfc13b3cf451dd004bd84ee23df161c4459d29d7394775db3" },
  { path: "src/serein/tokens.css", sha256: "069f3f2b751ce0e5cc7939749eaf432b454c5182655aa4ec25afa7dea968a3ee" },
  { path: "src/serein/fonts.css", sha256: null },
  { path: "src/serein/assets/fonts/Inter-latin.woff2", sha256: null },
  { path: "src/serein/assets/fonts/Inter-latinext.woff2", sha256: null },
  { path: "src/serein/assets/fonts/RobotoMono-latin.woff2", sha256: null },
];

const problems = [];

for (const entry of VENDORED) {
  const url = new URL(entry.path, ROOT);
  let bytes;
  try {
    bytes = readFileSync(url);
  } catch {
    problems.push(`${entry.path} is missing`);
    continue;
  }
  if (bytes.length === 0) {
    problems.push(`${entry.path} is empty`);
    continue;
  }
  if (!entry.sha256) continue;
  const digest = createHash("sha256").update(bytes).digest("hex");
  if (digest !== entry.sha256) {
    problems.push(
      `${entry.path} changed\n     recorded ${entry.sha256}\n     actual   ${digest}`,
    );
  }
}

// The application stylesheet must not shadow a framework file name: an `app.css` next to
// `serein-ui.css` is fine, but a second `tokens.css` outside `serein/` would mean two token
// layers and the audit would only read one.
const forbidden = ["src/tokens.css", "src/serein-ui.css", "src/fonts.css"];
for (const path of forbidden) {
  try {
    statSync(new URL(path, ROOT));
    problems.push(`${path} duplicates a vendored file name; keep application CSS under src/styles/`);
  } catch {
    /* expected: the file does not exist */
  }
}

if (problems.length > 0) {
  console.error("vendor check failed:");
  for (const p of problems) console.error("  - " + p);
  console.error(
    "\nDo not regenerate these hashes. Change the design upstream in cirrus-ui/, rebuild it,\n" +
      "re-copy the file, and update the hash and the table in src/serein/README.md.",
  );
  process.exit(1);
}

console.log(
  `vendor ok: ${VENDORED.length} files, ${VENDORED.filter((v) => v.sha256).length} hashed, ` +
    `${VENDORED.filter((v) => !v.sha256).length} binary`,
);
