# Interface design direction

**Status:** Selected and implemented · **Superseded by:** [Interface redesign: Atrium](interface-atrium.md)

> **Implementation note.** This document records the Atlas direction and the prototype that
> preceded the current client. The shipped client is React 19 + TypeScript + Vite, built into
> `internal/webassets/dist` (see [Interface redesign: Atrium](interface-atrium.md)). The
> "no bundler" constraint below describes the earlier prototype, not the current build.

**Scope:** information architecture, visual language, interaction rules, and design tokens of the browser client (`internal/webassets`)

## Goals and constraints

The technical foundation of the product determines what the interface should be, and that belongs in writing. Without it the interface degrades into another cloud drive.

| Foundation fact                                                                              | Requirement for the interface                                             |
| -------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| Hash-chained audit; every read and write is traceable                                        | History and permissions must be **visible inline**, not hidden in dialogs |
| Shares can be limited by password, use count, and expiry, and restricted to named recipients | Share state is a first-class property of an object, not a one-off action  |
| Server-side version history and content-addressed deduplication                              | Versions are the normal case, not an advanced feature                     |
| One instance serves 2000 people or 40 TiB                                                    | Density and scale must hold; looking good for 20 files is not enough      |
| Self-hosted, no third-party CDN                                                              | No external runtime resources; the client is built and embedded           |
| Administrators see metadata but not content                                                  | The interface must not make permissions look stronger than they are       |

Constraints:

- The earlier prototype used three static assets and no bundler. The current client is React 19 + TypeScript + Vite, with build output committed and embedded.
- Inline rendering has a strict type allowlist, content sniffing, and a CSP sandbox (see [ADR-0004](../adr/0004-metadata-store.md)).
- User data enters the DOM only through `textContent`.

## Direction exploration

Four clearly different directions were explored. Scores run from 1 to 5; for cost, a lower score means a more expensive option.

### A. Workbench

Keyboard first, with the command palette as the primary verb. Information density is very high (28px rows), borders are almost absent, and order comes from strict alignment; monospace carries the hierarchy.

- Strengths: high productivity, a distinct character, low implementation cost.
- Weaknesses: a barrier for ordinary users; nearly unusable on touch screens; it collapses outright without responsive work.
- Assessment: usability 3 / density 5 / accessibility 4 / cost 2 / distinctiveness 5.

### B. Gallery

Content is the interface. Thumbnails fill the viewport and chrome fades out during interaction; no list is offered.

- Strengths: an excellent experience for images and media; strong visual impact.
- Weaknesses: weak for text, code, and configuration files; many similar file names are hard to scan; low density.
- Assessment: usability 4 / density 2 / accessibility 4 / cost 2 / distinctiveness 4.

### C. Canvas

Files can be placed freely, and spatial position aids memory, as on a whiteboard.

- Strengths: spatial memory has cognitive support; the strongest character.
- Weaknesses: position is meaningless to a screen reader, which breaks the accessibility floor; unpredictable, which breaks principle 4; very high implementation cost; 40,000 files cannot be placed.
- Assessment: usability 2 / density 2 / accessibility 1 / cost 5 / distinctiveness 5.

### D. Atlas

**Content-driven views, permissions and history visible inline, and a column count that varies with width and selection state.**

- Strengths: serves ordinary users (clear defaults) and heavy users (dense, keyboard, command palette) at the same time; no bottleneck; differentiation comes from visible trust rather than visual decoration.
- Weaknesses: less striking than B and less minimal than A; **the adaptive logic must actually be built, or the result degrades into a fixed three columns**.
- Assessment: usability 5 / density 4 / accessibility 5 / cost 3 / distinctiveness 4.

### Scoring summary

| Direction   | Usability | Density | Accessibility | Cost  | Distinctiveness | Verdict                                             |
| ----------- | --------- | ------- | ------------- | ----- | --------------- | --------------------------------------------------- |
| A Workbench | 3         | 5       | 4             | 2     | 5               | Absorbed as the keyboard and command layer of Atlas |
| B Gallery   | 4         | 2       | 4             | 2     | 4               | Absorbed as a **view** within Atlas                 |
| C Canvas    | 2         | 2       | 1             | 5     | 5               | Rejected: breaks accessibility and predictability   |
| **D Atlas** | **5**     | **4**   | **5**         | **3** | **4**           | **Selected**                                        |

**Why D:** the differentiation of this product is not that it looks cool. It is that it dares to show the truth: who took what and when, whose links point at this file, what the previous version was. Drive and Dropbox hide permissions and history because those are secondary in their model. Here they are the subject. An interface that does not express this visually gives up the product claim.

The keyboard and command layer of A and the media view of B are absorbed as parts of Atlas, not kept as parallel alternatives.

## Selected direction: Atlas

In one sentence: **a quiet, precise workspace where files are the subject and the system's trust guarantees are always present without being loud.**

Three decisive decisions support it.

### Adaptive column count, not a fixed three columns

| Width       | Columns                 | Navigation             | Detail panel                                |
| ----------- | ----------------------- | ---------------------- | ------------------------------------------- |
| < 640px     | 1                       | Drawer (top)           | Full-screen sheet                           |
| 640–1024px  | 2 (icon rail + content) | Icon rail              | Overlay panel                               |
| 1024–1440px | 2 (icon rail + content) | Icon rail (expandable) | Side panel, only when an object is selected |
| > 1440px    | 3                       | Icon rail (expandable) | Side panel on selection, pinnable           |

The key point: **with no object selected, the detail panel takes no space.** That is the fundamental difference from a fixed three-column layout: the panel exists only when there is an object.

### Content-driven view suggestions

The system suggests a view from the directory contents, **states the reason**, and lets the user pin it.

| Directory characteristic         | Suggestion          | Reason chip           |
| -------------------------------- | ------------------- | --------------------- |
| ≥ 60% and ≥ 6 previewable images | Gallery             | `Gallery · 12 images` |
| > 200 items                      | Compact list        | `Compact · 431 items` |
| ≥ 60% text or code               | List (with excerpt) | `List · mostly text`  |
| Everything else                  | List                | `List`                |

Once the user pins a view, automatic switching stops. This is the only honest handling of the tension between content first and user control.

### Provenance and permissions inline

Every row conveys, without any interaction, whether it is shared and through how many links, whether it has versions, and whether it changed recently.

- `⇢3` shared, 3 links
- `⟲` has versions
- no marker = private, no versions

This is the interface expression of the audit capability, and the most substantive difference from competing products.

## Information architecture

```text
Atlas
├── Files (main view)
│   ├── folder hierarchy (tree fixed, view variable)
│   ├── views: list / compact / gallery
│   └── detail panel (on demand)
│       ├── preview
│       ├── properties
│       ├── version history → restore
│       ├── related shares → revoke
│       └── object activity (last 5 entries)
├── Shared
├── Trash
├── Activity (audit timeline)
└── Access (App Password / WebDAV)
```

Principle: **navigation does not change with the view.** The gallery and the list are two projections of the same hierarchy, not two features.

## Layout specifications

### Desktop (≥ 1440px)

```text
┌──────┬────────────────────────────────────────────┬───────────────┐
│ rail │ header: path · search · view · theme       │  details      │
│ 56px │────────────────────────────────────────────│  (on demand)  │
│ exp. │ content: list / compact / gallery          │  320px        │
│ 216px│                                            │  preview      │
│      │                                            │  properties   │
│      │                                            │  versions ⟲   │
│      │                                            │  shares ⇢3    │
│      │ status bar: storage · upload · online      │  activity     │
└──────┴────────────────────────────────────────────┴───────────────┘
```

- The header is a single row, 52px tall: breadcrumb, search, density, view, theme, command palette.
- The status bar is 28px tall and appears only when there is content to show (storage, upload progress, offline).
- The content area scrolls; the table header is sticky.

### Tablet (640–1024px)

- Two columns: icon rail (56px) plus content.
- The detail panel is an overlay `min(360px, 88vw)` wide with a scrim.
- The header wraps: breadcrumb on one row, controls on another.

### Phone (< 640px)

- One column.
- Navigation is a top bar with icons and short labels.
- Details are a full-screen sheet pushed from the bottom and dismissed by swiping down.
- Row height is 48px, satisfying the 44px touch target.
- The "kind" and "modified" columns are hidden; name and size remain.

### RTL

All layout uses logical properties (`padding-inline`, `border-inline-start`, `inset-inline-end`). Mirroring flips the direction instead of adding a second set of styles. Directional icons such as the back arrow are flipped with `transform: scaleX(-1)` under `[dir="rtl"]`.

## Core interaction rules

| Interaction           | Rule                                                              |
| --------------------- | ----------------------------------------------------------------- |
| Single click on a row | Select the row and fill the detail panel immediately              |
| Double click or Enter | Open: enter a folder, preview or download a file                  |
| Shift + click         | Extend the selection in **visual order**                          |
| ⌘/Ctrl + click        | Toggle a single item                                              |
| ⌘/Ctrl + A            | Select all visible items                                          |
| Esc                   | First press clears the selection; second press closes the overlay |
| ↑ ↓ / Home / End      | Move the selection (roving tabindex)                              |
| PageUp / PageDown     | Move one page                                                     |
| Backspace             | Go to the parent directory                                        |
| F2                    | Rename                                                            |
| Delete                | Delete with confirmation, move to trash, allow undo               |
| Space                 | Preview                                                           |
| ⌘/Ctrl + K            | Command palette                                                   |
| /                     | Focus search                                                      |

**Reversibility:** deletion moves the object to the trash, and the toast offers an undo. Revoking a share requires confirmation because it is irreversible. Restoring a version requires confirmation because it overwrites the current one.

**State transparency:** every operation produces one of three kinds of feedback.

1. Optimistic update (rename, selection).
2. Progress (upload, per file).
3. Result toast, optionally carrying an action link.

**No drag-only features:** drag-and-drop upload is an accelerator, not the only path. The keyboard and buttons must be able to do the same thing (WCAG 2.5.7).

## Design tokens

### Surfaces and text

| Token               | Light     | Dark      | High contrast |
| ------------------- | --------- | --------- | ------------- |
| `--surface-base`    | `#FAF9F6` | `#141311` | `#000000`     |
| `--surface-raised`  | `#FFFFFF` | `#1D1B18` | `#000000`     |
| `--surface-sunken`  | `#F1EEE8` | `#0E0D0C` | `#000000`     |
| `--surface-overlay` | `#FFFFFF` | `#242220` | `#000000`     |
| `--text-primary`    | `#171614` | `#F2EEE7` | `#FFFFFF`     |
| `--text-secondary`  | `#54504A` | `#B0A99E` | `#FFFFFF`     |
| `--text-tertiary`   | `#8A837A` | `#7C756B` | `#E6E6E6`     |
| `--border-subtle`   | `#E5E0D7` | `#2A2724` | `#FFFFFF`     |
| `--border-default`  | `#D2CCC0` | `#3D3934` | `#FFFFFF`     |
| `--border-strong`   | `#B8B1A2` | `#575149` | `#FFFFFF`     |

### Interaction and state

| Token           | Light     | Dark      | High contrast |
| --------------- | --------- | --------- | ------------- |
| `--accent`      | `#1F4FD8` | `#8AA5FF` | `#00A3FF`     |
| `--accent-wash` | `#ECEEFB` | `#1B2340` | `#00254D`     |
| `--danger`      | `#B8341F` | `#FF9A82` | `#FF5C5C`     |
| `--success`     | `#1D6B43` | `#6FC594` | `#00E07A`     |
| `--warning`     | `#8A5A00` | `#E0B155` | `#FFC400`     |
| `--focus-ring`  | `#1F4FD8` | `#8AA5FF` | `#FFFF00`     |

Contrast validation: `--text-primary` on `--surface-base` ≥ 14:1; `--text-secondary` ≥ 7:1; `--accent` on base ≥ 6:1; all high-contrast values ≥ 7:1 (focus ring 21:1).

### Spacing and shape

```css
--space-1: 4px   --space-2: 8px   --space-3: 12px
--space-4: 16px  --space-6: 24px  --space-8: 32px  --space-12: 48px
--radius-sm: 2px   --radius: 4px   --radius-lg: 8px
```

Right angles dominate. Only overlays use 8px. **No pill shapes**, except for status markers.

### Type and scale

| Role        | Size / line height                        | Family        |
| ----------- | ----------------------------------------- | ------------- |
| Heading     | 15 / 1.3                                  | UI sans, 650  |
| Body        | 13 / 1.45                                 | UI sans, 400  |
| Metadata    | 11.5 / 1.35                               | **monospace** |
| Micro-label | 10 / 1.2, letter-spacing .09em, uppercase | UI sans, 600  |

**All metadata is monospace**: sizes, dates, counts, hashes, paths. That single decision produces precision and alignment at once, and it relies on no decoration.

### Motion

```css
--motion-fast: 120ms
--motion-base: 160ms
--ease: cubic-bezier(0.16, 1, 0.3, 1)
```

Only `transform` and `opacity` animate. Under `prefers-reduced-motion: reduce` all durations drop to 0ms and only state changes remain.

### Density

- `comfortable`: 36px rows, 12px padding.
- `compact`: 28px rows, 8px padding.

The setting persists to localStorage.

## Component library

| Component         | Variants                                    | States                                           |
| ----------------- | ------------------------------------------- | ------------------------------------------------ |
| `button`          | primary / default / ghost / danger          | default, hover, active, focus, disabled, loading |
| `icon-button`     | —                                           | Same as above, plus required `aria-label`        |
| `segmented`       | —                                           | `aria-pressed` on each item                      |
| `row`             | list / compact                              | default, hover, selected, focused                |
| `tile`            | gallery                                     | default, hover, selected                         |
| `badge`           | file type                                   | accent border for folders, images, and video     |
| `pill`            | status                                      | neutral, ok, danger, accent                      |
| `field`           | text / password / number / email / select   | default, focus, invalid, disabled                |
| `dialog`          | default / preview / sheet                   | —                                                |
| `toast`           | info / error, optionally with an action     | —                                                |
| `progress`        | linear                                      | determinate                                      |
| `skeleton`        | row / block                                 | shimmer (static under reduced motion)            |
| `banner`          | offline / storage / permission              | dismissible                                      |
| `command-palette` | —                                           | empty, no results, grouped                       |
| `empty-state`     | icon + title + description + primary action | —                                                |
| `state-block`     | error / denied / offline                    | includes retry or explanation                    |

Every interactive element is at least 24×24 (44×44 on touch), and the focus ring is 2px and is never clipped by `overflow: hidden`.

## Key flows

### Search

1. `/` or a click focuses the search field.
2. Typing filters the current directory locally and immediately.
3. With no results, an empty state explains that only the current folder is searched and offers "search all files".
4. Esc clears the query and restores the previous view.

Future extension: cross-directory search goes through the server. The interface reserves the switch entry point and does not pretend to support it yet.

### Preview

1. A single click shows a preview well in the detail panel (image thumbnail, first 9 lines of text, or type icon).
2. Double click, Space, or "Open" opens the preview overlay.
3. In the overlay, Esc closes it and ← → move between previewable files in the same directory.
4. A type that cannot be rendered inline gets an explanation and a download action, never a "corrupt" label.

### Upload

1. Three entry points — button, drag and drop, and paste — all target the current directory.
2. During a drag, an overlay shows the target directory name.
3. The queue is serial, with per-file progress and failure reason.
4. One failed file does not discard the rest; the failed item offers retry.
5. The completion summary reads `Uploaded 7, 1 failed`.

### Sharing

1. Select an object, then use "Share" in the detail panel or the context menu.
2. Choose the type (link or drop box), password, expiry, and use limit.
3. After creation, show the URL **once** and copy it. The token is stored only as a hash and cannot be retrieved again; the interface must say so.
4. The detail panel lists every link for the object and allows revocation. **No "copy original link"** action is offered, and the reason is explained.

### Bulk operations

1. Shift/⌘ multi-selection brings up a selection bar at the bottom.
2. It shows `12 selected`, the total size, and the available actions.
3. Bulk delete: one confirmation, executed per item, with per-item failure reports.
4. Partial failure produces a summary plus "view failed items".
5. Esc clears the selection.

## State design

| State                      | Presentation                                                                                                      |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| **Empty directory**        | Icon + "This folder is empty" + upload/create primary actions                                                     |
| **Search with no results** | "No matches" + clear filters                                                                                      |
| **Loading**                | Skeleton rows at the height of the final rows (no jump), shown only after 400ms                                   |
| **Error**                  | Explanation + retry button + error code (no internal paths)                                                       |
| **No permission**          | "No permission to access this object", with **no hint whether it exists**, consistent with the backend 404 policy |
| **Offline**                | Top banner; action buttons are disabled with a stated reason                                                      |
| **Quota full**             | Banner + upload disabled, linking to usage                                                                        |
| **Partial failure**        | Toast summary + expandable details                                                                                |

## Internationalization and accessibility

### Internationalization

- All strings come from the key table. **No string concatenation**, because word order differs by language.
- Plurals use `Intl.PluralRules`; numbers, dates, and relative times use `Intl.*`.
- Language detection runs from `navigator.languages` with a fallback to `en`; a user override persists to localStorage.
- Languages built in: `en` and `zh-Hans`.
- Layout uses logical properties, so `dir` switches globally.
- Date formats are never hard-coded; neither 12/24-hour time nor the first day of the week is assumed.

### Accessibility (WCAG 2.2 AA)

| Criterion                    | Measure                                                                  |
| ---------------------------- | ------------------------------------------------------------------------ |
| 1.4.3 Contrast               | Body text ≥ 4.5:1; every token is validated                              |
| 1.4.11 Non-text contrast     | UI boundaries and states ≥ 3:1                                           |
| 2.1.1 Keyboard               | Every function is reachable by keyboard                                  |
| 2.4.7 Focus visible          | 2px focus ring, never clipped                                            |
| 2.4.11 Focus not obscured    | The focused element scrolls into the viewport automatically              |
| 2.5.7 Dragging movements     | Upload has a button path and does not rely on dragging                   |
| 2.5.8 Target size            | 24×24 minimum; 44×44 on touch                                            |
| 3.2.6 Consistent help        | The help entry point has a fixed location                                |
| 4.1.2 Name, role, value      | Icon buttons require `aria-label`; lists use `role="listbox"` / `option` |
| 4.1.3 Status messages        | Results are announced through `aria-live="polite"`                       |
| 1.3.1 Info and relationships | Properties use `<dl>`; headers use `<th scope>`                          |
| 2.3.3 Animation              | `prefers-reduced-motion` is respected                                    |
| 1.4.13 Content on hover      | Content revealed on hover can also be triggered by focus                 |

In addition, `prefers-contrast: more` switches to the high-contrast theme automatically, and `prefers-reduced-transparency` removes overlay translucency.

## Usability testing and acceptance criteria

### Test plan

Five to eight participants: three lightly technical users, three developers or operators, and two keyboard or assistive-technology users. Tasks:

1. Find who downloaded a given file last month, using the activity page.
2. Share an 800MB folder with an external consultant, limited to three days of downloads.
3. Recover an older version that was overwritten.
4. Find a contract among 300 files and preview it read-only.
5. With the keyboard only: enter a subdirectory, preview a file, and go back.
6. Upload a photo from a phone and copy the link.

Record: task completion rate, time, error count, number of assistance requests, and the SUS score.

### Acceptance criteria (measurable)

| Item                  | Criterion                                                                                                                  |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| Task completion rate  | ≥ 90% without assistance                                                                                                   |
| SUS                   | ≥ 80                                                                                                                       |
| Keyboard reachability | All 6 tasks completable with the keyboard alone                                                                            |
| Contrast              | Automated check of every token in all three themes ≥ AA                                                                    |
| Touch targets         | All ≥ 44×44 on mobile                                                                                                      |
| First screen          | A 100-file directory is interactive within 1s (local instance)                                                             |
| Layout                | No horizontal scrolling and no truncated content at 320 / 640 / 1024 / 1440 / 2560 widths                                  |
| Screen reader         | The file list can be traversed and announces name, size, date, and share state under NVDA + Firefox and VoiceOver + Safari |
| Reduced motion        | No positional transitions when enabled                                                                                     |
| RTL                   | The layout mirrors under `dir="rtl"` with no misalignment                                                                  |
| Internationalization  | Switching en/zh produces no concatenation artifacts, no overflow, and correct plurals                                      |
| State coverage        | Empty, loading, error, no permission, and offline each have a dedicated interface                                          |

## Implementation status

Landed with this design:

- Token system and three themes (light / dark / high contrast), switchable by hand and following the system.
- Adaptive column count of 1 / 2 / 3 columns; the detail panel takes space only while something is selected (`body.has-inspector`).
- Content-driven view suggestions with a reason chip and pinning.
- Density switch (comfortable / compact), persisted.
- Command palette (⌘K) mixing commands and files, with subsequence matching.
- Five state interfaces: empty / loading skeleton / error / no permission / offline.
- i18n key tables (en / zh-Hans), `Intl.PluralRules` plurals, and `Intl.DateTimeFormat` dates.
- Accessibility: roving tabindex + `aria-activedescendant`, `aria-live` announcements, focus ring, `prefers-reduced-motion`, `prefers-contrast`, and a required `aria-label` on icon buttons.
- Logical properties and `dir` support.
- Inline provenance markers (`share_count` / `has_versions`) from two batch queries, not N+1.

### Three issues found during implementation

**CSP silently disabled every inline style.** The stylesheet policy is `style-src 'self'` without `'unsafe-inline'`. As a result, `style="..."` attributes remain in the DOM but never take effect, including the flex layout of `#body`, upload progress bar widths, and several table alignments. The symptom was a layout that inexplicably failed to fill the screen, while inspecting the DOM showed the attributes present. This explained why the status bar never reached the bottom.

The fix was not to relax the CSP, because inline styles are a channel for CSS data exfiltration and the policy is deliberate. Instead all 20 inline styles became semantic classes, and the progress bar became a native `<progress>` element, whose `value` attribute drives rendering without any style injection. The `style` option was also removed from `el()` so the mistake is not repeated.

Semantic classes brought a side benefit: styles live in the stylesheet, so RTL and the high-contrast theme only override classes instead of every element.

**Lost focus broke the arrow keys.** The list uses `role="listbox"` + `aria-activedescendant`, and rows are not Tab stops. Rendering rebuilds the DOM, so after a row click focus falls back to `<body>` and the arrow keys have nowhere to go. The fix records that focus must be restored before rendering and puts focus back on the new listbox afterwards. The defect was caught by an interaction assertion in the browser; a static screenshot cannot show it.

**Provenance markers needed backend support.** The design requires every row to show whether it is shared and whether it has versions. Calling `SharesForNode` and `Versions` per row means 500 round trips for one directory. The implementation moved to a `MarkersFor` batch interface (two aggregate queries) implemented in both backends. The server probes for the optional interface; a backend that cannot answer loses one decoration without affecting the list.

Not implemented, each corresponding to a backend capability outside the scope of this design:

- Cross-directory full-text search (the backend has no index).
- An offline write queue (needs a local persistence layer).
- Multi-object sharing across a selection (the backend models shares per single object).
- `ja` / `ar` / `he` language packs (`dir` and logical properties are ready; translations are missing).
