# Interface redesign: Atrium

**Status:** Selected and implemented · **Supersedes:** [Interface design direction](interface-design.md), which is retained as history

**Scope:** `web/` (React + TypeScript + Vite) and `internal/webassets`

## Why the first two attempts failed

The first two rounds changed the **skin and the column count**, not the **structural metaphor**. All three attempts shared one skeleton:

```text
left navigation + main list + right detail panel
```

That is the skeleton of every file manager: Finder, Explorer, Drive, Dropbox, Nextcloud. Inside that skeleton, no change of palette, no change of typeface, and no panel that appears on demand alters the first impression: another cloud drive. That is the failure in one sentence, and it is the reason this document exists.

**Conclusion: the interface no longer starts from "file list + panel".**

## Direction exploration

Three directions are structurally incompatible with each other; they are not different skins on one skeleton.

### D1: Launcher (command first)

The interface is a single input field that always holds focus; every feature is a result set of that field. Reference points: Spotlight, Raycast, and Notion search results.

- Strengths: extremely fast for heavy users; simple to implement; strong character.
- Weaknesses: **poor discoverability** — someone who does not know which features exist has no starting point; spatial sense is lost while browsing large directories; ordinary users, the majority of this product's users, must first learn what to type.
- Assessment: usability 2 / density 3 / accessibility 4 / cost low / distinctiveness 5.

### D2: Stream (time first)

The home page is a time-ordered stream of objects (recently changed, shared, accessed). The file tree is a secondary entry reached mainly through search and filters. Reference points: Dropbox Recents and the "Recent" view in Photos.

- Strengths: **expresses the product claim directly** (traceability); no learning cost; optimal for the most common case, coming back to continue where the last session stopped.
- Weaknesses: weak for deep directory browsing; rename and move have no natural place; tends to slide into a dashboard.
- Assessment: usability 4 / density 3 / accessibility 4 / cost medium / distinctiveness 4.

### D3: Columns (space first)

Each column represents one level of the path, and the path itself is visible. No breadcrumb, no sidebar. Reference points: the Finder column view and Miller columns.

- Strengths: **spatial memory** — position is the path and is easier to remember than a breadcrumb; hierarchy depth is visible at a glance; keyboard navigation is natural (left and right between levels, up and down between items); completely different from "list + panel".
- Weaknesses: a wide, shallow directory (500 files on one level) makes the columns cramped and requires horizontal scrolling; on phones it becomes a push stack and degrades to ordinary level-by-level navigation.
- Assessment: usability 4 / density 5 / accessibility 4 / cost medium / distinctiveness 5.

### Assessment summary

| Direction   | Usability | Density | Accessibility | Cost   | Distinctiveness | Verdict                                                    |
| ----------- | --------- | ------- | ------------- | ------ | --------------- | ---------------------------------------------------------- |
| D1 Launcher | 2         | 3       | 4             | low    | 5               | Absorbed as the command palette (⌘K), not as the main body |
| D2 Stream   | 4         | 3       | 4             | medium | 4               | Absorbed as the **home page**                              |
| D3 Columns  | 4         | 5       | 4             | medium | 5               | Absorbed as **file browsing**                              |

### Selected: a combination of D2 and D3 — Atrium

This is not a compromise. It answers two different questions with two different interfaces:

- "What happened last time, and what has been shared?" goes to **Home (Stream)**, object first and ordered by time.
- "The location is known; go get the item" goes to **Files (Columns)**, space first and hierarchy visible.

Those two questions competed for the same area in a single interface, and that was the real problem in the earlier versions. Once separated, neither has to compromise. The Launcher of D1 is reduced to the command palette and serves the heavy-user case of knowing the name.

## Structure and the four decisive decisions

### No permanent sidebar

Navigation is **a set of sections in the top bar**. This is the most immediate difference from every earlier version: the interface has no left column, so content receives the full width.

The reason: in a file manager a sidebar carries three duties at once — location, hierarchy, and tree. Once hierarchy is expressed by the column view, the sidebar is left with section switching alone, and paying 216px for one job is not worthwhile.

### The files page is a column view, not "list + breadcrumb"

```text
┌───────────┬───────────┬───────────┬────────────────────┐
│ Contracts │ 2026      │           │  Preview           │
│ Drafts  ● │ Q1.pdf    │           │  Name / properties │
│ Images ⇢2 │ Q2.pdf ●  │           │  Versions / shares │
└───────────┴───────────┴───────────┴────────────────────┘
    level 1     level 2     selected       details
```

- Clicking a folder truncates every column to its right and appends a new column.
- Clicking a file turns the rightmost column into details (preview, properties, versions, shares).
- There is no breadcrumb: the path is the columns on the left, because each column header is the name of that level.
- Keyboard: `←/→` change column, `↑/↓` change item, `Enter` enters or opens, `Backspace` goes back.

### The home page is real data, not a dashboard

Three regions, each actionable, with no decorative cards:

1. **Continue** — recently modified files with real thumbnails; a click opens the preview directly.
2. **Outgoing shares** — link cards showing type, uses so far, and expiry; revocable in place.
3. **This space** — used and total quota, file count, and the most recent activity.

### Preview is immersive, not a dialog with a title bar

Full-bleed rendering, metadata compressed into one thin strip, sibling files from the same directory as a filmstrip at the bottom, `←/→` to switch. This is the only way to look at a photo through to the end inside a file manager.

## Visual language

The first two versions used warm paper tones and monospace metadata and read like developer tools. This version:

- **A cool neutral base with ink text**, closer to white paper than to cream.
- **A single accent color: deep emerald green** (light `#0B6E4F` / dark `#4ADE80`). Semantically it means "stored, verified", which fits a product built on hash-chained audit, and it avoids the blue that is ubiquitous in this category.
- **All metadata is monospace**: sizes, dates, counts, paths. That single decision provides precision and column alignment together.
- **Color only carries meaning**: file type, state, and share state. Selection and focus use ink rather than color, so a destructive action cannot rely on red alone; it also carries an icon and text (WCAG 1.4.1).
- Near right angles (2–4px), 1px rules for layering, shadows only on overlays.
- Motion of 120–160ms, transform and opacity only, respecting `prefers-reduced-motion`.

### Three themes

Light, dark, and high contrast. High contrast is not inverted color: a pure black background, white text, a solid border on every row, and a yellow focus ring; borders replace surface differences.

## Information architecture

```text
Atrium
├── Home      recent activity, outgoing shares, space usage
├── Files     column view (core)
│   ├── columns = levels
│   ├── rightmost column = details (preview / properties / versions / shares)
│   └── drag-and-drop upload / ⌘K / keyboard navigation
├── Shared    link list, revocable
├── Activity  audit timeline, filterable, exportable
└── Access    App Password / WebDAV
```

The share landing page `/s/<token>` stands outside the shell and has no session.

## Responsive rules

| Width    | Files page                       | Top bar                              |
| -------- | -------------------------------- | ------------------------------------ |
| ≥ 1200   | 3–4 columns side by side         | Full                                 |
| 760–1200 | 2 columns + detail overlay       | Section labels collapse to icons     |
| < 760    | Single column stack, back button | Section icons + current section name |

On phones the column view degrades to **level-by-level entry**: one level at a time, with back returning to the previous level. The semantics match Miller columns; horizontal expansion is folded into a vertical stack.

## States

| State           | Presentation                                                                                                          |
| --------------- | --------------------------------------------------------------------------------------------------------------------- |
| Empty directory | Explanation + upload primary action; no empty table                                                                   |
| Loading         | Skeleton at the height of the final rows, appearing only after 400ms                                                  |
| Error           | Explanation + retry + error code                                                                                      |
| No permission   | States that there is no permission without hinting whether the object exists (consistent with the backend 404 policy) |
| Offline         | Banner in the top bar; write operations disabled with a reason                                                        |
| Partial failure | Summary + view failed items                                                                                           |

## Accessibility and internationalization

- In the column view each column is `role="listbox"` and `aria-activedescendant` tracks the current item; the column itself is `role="group"` with an `aria-label`.
- All icon buttons must have an `aria-label`.
- Toasts also write into an `aria-live="polite"` region: a visual-only notice is a silent failure for screen-reader users.
- The focus ring is 2px and is never clipped; focus moves with the column.
- WCAG 2.2 AA: body text ≥ 4.5:1, UI boundaries ≥ 3:1, touch targets ≥ 24px (44px on mobile).
- All layout uses logical properties (`padding-inline`, `inset-inline-start`); `dir=rtl` mirrors the whole interface.
- i18n: key tables plus `Intl.PluralRules` / `Intl.DateTimeFormat` / `Intl.NumberFormat`, with no string concatenation.
- `en` and `zh-Hans` are built in; the language is switchable and stored locally.

## Technical stack decisions

The requested direction was TypeScript + React + Vite, with permission to keep the existing frontend if it was suitable.

**Decision: adopt React + TypeScript + Vite.**

The reason is not that React is better. Frontend complexity had reached the point where manual DOM manipulation is inappropriate: the column view maintains several lists incrementally by path, loads each column independently, migrates focus between columns, and shares selection between the detail panel and the preview. Written as imperative DOM code, that is 3000+ lines and hard to reason about. The maintenance problems of the earlier versions came from exactly this.

Costs and mitigations:

| Cost                          | Mitigation                                                                                                 |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------- |
| Introduces a build step       | The build output is committed to the repository; `go build` still produces a deployable binary in one step |
| npm dependency tree           | **Zero runtime dependencies**: the output is static JS and CSS; npm exists only at development time        |
| Frontend and binary can drift | The output is embedded in the binary; `make web` regenerates it                                            |

`internal/webassets` correspondingly embeds `web/dist`, which Vite writes directly to that directory, removing the copy step.

### API types

The REST contract is mirrored by hand in `web/src/types.ts`. **No** OpenAPI code generation was introduced: for 15 endpoints, the maintenance cost of a generator and schema validation is higher than the benefit it provides. If the number of endpoints grows significantly, or a second consumer appears (mobile, CLI), adding `openapi.yaml` plus generation has a clear payoff. The tradeoff is recorded at the top of `web/src/types.ts`.

## Acceptance

- Top-bar navigation, no left sidebar.
- The files page is a column view; the path is expressed by columns, with no breadcrumb.
- Three regions of real data on the home page.
- Immersive preview with a sibling-file filmstrip.
- Three themes, two languages, fully keyboard reachable.
- No horizontal scrolling and no truncation at 360 / 768 / 1280 / 1920 widths.
- Screenshots cover home, files, preview, shares, activity, and mobile.
- Compared with the pre-redesign screenshots, the structural difference must be visible at a glance.

The structure described here is the actual implementation of this round; screenshots are in the commit history.

## Implementation record

### New backend capability

The "Continue" region on the home page needs recently modified files across the whole tree. Walking directories row by row is infeasible at real scale, and listing only the root directory would be a lie, so one capability was added:

- `store.RecentReader` (optional interface, same pattern as `MarkersFor`): `RecentFiles(ctx, owner, limit)`.
- The PG implementation uses one recursive CTE to fetch nodes and the ancestor chain together, then aggregates the path; the depth limit of 64 prevents a loop when the parent chain is damaged.
- The in-memory implementation sorts by `ModifiedAt` and handles ties explicitly; otherwise the home page order could change on every refresh.
- `GET /api/files/recent?limit=`; a backend without the capability returns an empty list instead of an error, so the home page shows an empty state, which is honest.
- The audit metadata of `share.create` gains `path`. Without it the log only says `share.create 18711bd6`, and an untraceable record is no record.

### Three defects exposed during implementation

**Ambiguous `id` in SQL.** The first version of recent put both `recent` and `nodes` in the FROM clause, and the `id` on both sides made Postgres report `column reference "id" is ambiguous` directly. The in-memory backend tests were all green because they cannot test SQL. `recent_pg_test.go` was added (it requires `TEST_DATABASE_URL`) and covers ordering, nested paths, trash exclusion, owner isolation, and limit. The lesson is not to write more tests; it is that **a simulated implementation cannot validate a query**.

**An entire column disappeared on mobile.** The phone layout shows only the deepest column through `.col:not(.active)`. While removing an unused variable, the computation that carried the `active` class was deleted with it, so every column was hidden and only the toolbar remained. The regression was invisible in the 1440px screenshot and was exposed by the 390px one: responsive work must actually be screenshotted at a narrow viewport.

**CSP disabled inline styles again.** The React `style` prop writes the `style` attribute, which `style-src 'self'` blocks in the same way. The previous round had already hit this, and after the framework change it became a trap again: progress bar widths, skeleton widths, and several alignments all failed silently. The handling stayed the same — do not change the CSP, change the code:

- Dynamic-width progress bars became native `<progress>` elements, with `value` driving rendering.
- Skeleton sizes and column widths became CSS selectors.
- Project convention: **no `style` attribute**; this is written in the comments of `styles.css` and `ui.tsx`.

### Structural comparison with the previous design

|                     | 0005 (Atlas)                | This round (Atrium)                                          |
| ------------------- | --------------------------- | ------------------------------------------------------------ |
| Navigation          | Left sidebar + tabs         | Top-bar sections, no sidebar                                 |
| Hierarchy           | Breadcrumb + single list    | Column view, the path is the columns                         |
| Home                | Straight into the file list | Recent / outgoing shares / space, three regions of real data |
| Preview             | Inside the right panel      | Full-screen immersive + filmstrip                            |
| Details             | Permanent third column      | Appears on demand as the last column                         |
| Technology          | Hand-written DOM, no build  | React + TS + Vite                                            |

### Still not done

- Cross-directory full-text search (no backend index; the command palette searches only recent files, and the documentation states that scope).
- Offline write queue (needs a local persistence layer).
- Cross-object sharing (the backend models shares per single object).
- `ja` / `ar` / `he` language packs (`dir` and logical properties are ready; translations are missing).
- Bulk download (the backend has no archive endpoint; faking bulk download with N separate downloads is worse than not doing it).

## Round 2: grid, sorting, multi-selection, and the API contract

### Grid and sorting

The list/grid toggle applies only to the **deepest column**. The columns on the left are navigation; turning them into a grid only loses readability while drilling down. Sorting applies to all columns, and **folders always sort first**, independent of the sort key — mixing folders into files by size would destroy the reason the column view exists.

Two implementation details are worth recording:

- Keyboard movement uses the same `arrange()` as rendering. Earlier the keyboard used `filtered()` and rendering used another function; as soon as the two drifted, the arrow keys jumped outside the order on screen.
- Folders do not show a size in the grid. The first version fed the child count into the byte formatter, so a folder holding 7 items displayed "7 B" — a wrong number, not a formatting defect.

### Multi-selection

Selection belongs only to the deepest column: a bulk operation needs one unambiguous target directory. Supported: Shift range selection, the platform modifier to toggle one item, Space to toggle, ⌘A to select all, Esc to clear, and Delete to delete. With more than one item selected, the detail panel yields to the selection bar, because the panel describes one object and the interest at that moment is N.

### API contract switched to generated

The previous round hand-wrote the types in `types.ts` and argued in the documentation that 15 endpoints did not justify a generator. That judgment was wrong when the client and the schema are both hand-written: if both are wrong in the same way, the compiler keeps agreeing with the wrong shape. Now:

- `web/openapi.yaml` is the single source of truth and covers every endpoint and schema the client uses.
- `pnpm gen:api` generates `src/api-types.ts`; `src/types.ts` only re-exports renamed types.
- `pnpm gen:api:check` regenerates and runs `git diff`; it runs in CI and fails when the output is stale.
- `make webcheck` is the local equivalent of the CI `web` job.

Type checking passed immediately after generation, which shows that the contract already matched the client's expectations. More importantly, it turned "consistent" from an assumption into a verifiable fact, and the next server-side field change fails at compile time instead of returning `undefined` at runtime.

### Third-round addition: the client itself is generated

The above made only the types generated; the request functions were still hand-written, so a wrong path, a misspelled parameter name, or a missing query was beyond the compiler. Now `api.ts` uses the generated route types through `openapi-fetch`:

```ts
list: (path: string) => call<Listing>(client.GET("/api/files/", { params: { query: { path } } })),
```

A path that does not exist, a wrong parameter name, and a mismatched response type are all compile errors. An endpoint travels from the document to the page through one definition in `openapi.yaml`.

Two places **deliberately** do not use the generated client, with the reasons recorded in the same file:

- **Upload**: the body is a stream and the caller must be able to pass an `AbortSignal` to cancel; failures still become the same `APIError`, so the caller cannot tell the difference.
- **Share landing page**: unauthenticated with no session; the same URL returns HTML for navigation and JSON for fetch, so the premise of an authenticated JSON API does not apply.

### A real error caught by the check

In this round `pnpm gen:api:check` reported a failure: the committed `api-types.ts` had no `/api/files/search`, while `openapi.yaml` did. The cause was a timing problem between that generation run and the YAML write; the output lagged the document, and only typecheck had been run, which does not regenerate.

This is exactly why the check exists. It catches mistakes of this kind during CI rather than at runtime, where the failure would surface as a 404.

### RTL result and one artifact

RTL measures as **correctly mirrored**: with `dir=rtl` the detail panel moves to the left, navigation runs right to left, and column headers and numeric alignment all flip; the measurement is `detail 340 + collist 1078 = 1418`, with no horizontal overflow.

The first RTL screenshot looked broken — the whole interface shrank to the right with a blank left side. The cause was not CSS but the screenshot tool: with `captureBeyondViewport: true`, `Page.captureScreenshot` used a canvas of the wrong width under RTL in Chrome. With `false`, the screenshot matched the measurement. The application is a fixed-height shell whose page does not scroll, so `captureBeyondViewport` should never have been enabled.

The lesson belongs to the same class as the CSP incident: **when a screenshot and a measurement disagree, suspect the tool first** — but once the tool is confirmed sound, what was measured is the truth.

### Race detection

This machine has no C compiler, so `go test -race` cannot run; `make race` detects this and skips, while CI runs it unconditionally on Linux. This round did not run `-race` locally, and that cannot be counted as verified.

## Round 3: real search, and measured contrast

### Search

The previous round's "search" was really two weaker things: filtering inside the current folder, and matching recent files in the command palette. The first is not search (a file outside the current folder cannot be found), and the second covers only items touched recently. Both are easily marketed as search, so this round added actual search:

- `store.Searcher` (optional interface): matches by **name** across the whole tree.
- PG uses `strpos(lower(name), lower($2)) > 0` rather than `LIKE`. A query is data, not a pattern — splicing a user's `%` or `_` into LIKE silently changes semantics. `strpos` has no metacharacters and needs no escaping.
- Fewer than two characters returns empty immediately: one character matches most of the store, which is noise rather than a result.
- Results carry the path and provenance markers, and the command palette can jump to them directly.
- The command palette now debounces and then queries the server; recent files appear only for an empty query.

The scope is the **name**, not the contents. Content search would require maintaining an index on the write path, which is far more expensive than this feature is worth; a content index that silently misses files is worse than an honest name search. This is stated in both the endpoint description and the documentation.

There is no `lower(name)` index, so this is a sequential scan with a limit. That is acceptable at the current scale; if it stops being acceptable, the next step is a `pg_trgm` migration, not a rewrite of the query.

### Contrast is measured, not asserted

This document and the [README](../../README.md) have always claimed WCAG 2.2 AA. Color claims are the easiest to get wrong, because the eye can barely distinguish 3.5:1 from 4.6:1. A script was added at `web/scripts/contrast.mjs`: it parses the tokens in `styles.css`, computes the pairs the interface actually relies on with the WCAG relative luminance formula, fails when a value is below target, and is wired into `pnpm contrast`, `make webcheck`, and CI.

Its first run found two failures:

- `--ink-3` (metadata, column labels, file size, captions) measured 3.4–3.5:1 in the light theme and 4.1–4.3:1 in the dark theme, **both below 4.5:1**. It is 10–11px body text, so the large-text exemption does not apply. It was darkened and lightened until it passed.
- Control borders reused `--line-strong` at about 1.6:1. As the only visual cue for where an input is, that is UI information governed by 1.4.11 and needs 3:1. `--line-control` is now dedicated to control borders, while decorative separators keep `--line` / `--line-strong` — the requirements differ, and sharing one token necessarily gets one side wrong.

This is the gap between "looks fine" and "passes": adding a 10px gray label shows no visible problem, and measuring it gives 3.4:1.

## Comparison with the pre-redesign build

The comparison does not rely on memory: same day, same sample data, same browser window (1440×1000). The old build was checked out and built separately with `git worktree add <dir> 30a341b`; both versions were captured with their own `tools/demoshot`.

|                          | Before the redesign (Atlas, 30a341b)  | After the redesign (Atrium)                                                                    |
| ------------------------ | ------------------------------------- | ---------------------------------------------------------------------------------------------- |
| First screen             | Lands directly on the file list       | Home: recent / outgoing shares / space usage                                                   |
| Navigation               | 216px left sidebar                    | Top-bar sections, no sidebar                                                                   |
| Hierarchy                | Breadcrumb + single list              | Column view, the path is the columns                                                           |
| Details                  | Permanent third column or overlay     | Appears on demand as the last column; a selection bar when multiple items are selected         |
| View                     | List / grid toggle (last column)      | List / grid (last column) + sorting                                                            |
| Search                   | Filter for the current folder         | Name search across the tree (⌘K)                                                               |
| Preview                  | Inline preview in the panel           | Full-screen immersive + sibling filmstrip                                                      |
| Bulk                     | None                                  | Shift/⌘ multi-selection + bulk delete                                                          |
| Technology               | Hand-written DOM, no build            | React + TS + Vite, types generated from OpenAPI                                                |

The visible structural difference is not palette or font size: the old first screen was a vertical strip on the left plus a table in the middle, while the new first screen has no list at all and the entire left side is empty. On the files page, the old build only ever has one table, while the new one grows a second and third column while drilling down.

The commands below regenerate either version:

```bash
# old build
git worktree add ../newcloud-before 30a341b
cd ../newcloud-before && go run ./tools/demo -data ./demo-run
go run ./tools/demoshot -base https://localhost:8443 -out ./demo-run/before <share-token>

# current build
go run ./tools/demo -data ./demo-run
go run ./tools/demoshot -base https://localhost:8443 -out ./demo-run/after <share-token>
```

Screenshots are not committed: they are build artifacts, and committing them produces unreadable binary diffs on every style change. The capture scripts are in the repository, and the commands are above.
