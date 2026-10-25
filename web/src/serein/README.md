# Serein UI — vendored stylesheet

These files are copied verbatim from the approved design system, not edited here.

| File | Source | SHA-256 |
| --- | --- | --- |
| `serein-ui.css` | `cirrus-ui/dist/serein-ui.css` | `9ef00c4f5fe4454cfc13b3cf451dd004bd84ee23df161c4459d29d7394775db3` |
| `tokens.css` | `cirrus-ui/dist/tokens.css` | `069f3f2b751ce0e5cc7939749eaf432b454c5182655aa4ec25afa7dea968a3ee` |
| `fonts.css` | `cirrus-ui/dist/fonts.css` | copied unmodified |
| `assets/fonts/*.woff2` | `cirrus-ui/dist/assets/fonts/` | copied unmodified |

Upstream: <https://github.com/EdisonJwa/sereinui>, version `1.0.0-rc.1`, Apache-2.0.

## Why a copy instead of a dependency

`serein-ui` is not published to a registry, and `web/` is built by `pnpm --frozen-lockfile`
against a committed lockfile. A `file:` dependency pointing at `cirrus-ui/` would make the
client build depend on a sibling checkout that is deliberately excluded from the production
tree, and `make build` runs `pnpm install --frozen-lockfile` in a clean container where that
sibling does not exist. A copy is reproducible, reviewable and has no install step.

## Rules

1. **Do not edit these files.** `pnpm contrast` and the design-parity expectations read the
   token values from `tokens.css`; changing one there makes the interface drift from the
   approval silently.
2. **Change the design in a new round.** If a value must change, change it upstream in
   `cirrus-ui/src/`, rebuild that package, and re-copy with a new hash in this table.
3. **Application-specific CSS belongs elsewhere.** Everything WebCloud adds on top —
   the one thing a generic framework cannot know — lives in `web/src/styles/*.css` and is
   scoped under `.serein-root` so it composes with, rather than overrides, the framework.

`test:vendor` re-hashes these files and fails when the copy no longer matches the table.
