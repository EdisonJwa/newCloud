# Cirrus 9.1 UI — implementation record

**Status:** implemented in `web/` · **Date:** 2026-10-25 · **Design authority:**
[`design-studio/round-9.1-cirrus-ux`](../../design-studio/round-9.1-cirrus-ux/FREEZE.md)
(freeze `cirrus-9.1-r1`) · **Design system:** [Serein UI](../../cirrus-ui/README.md)
`1.0.0-rc.1`, vendored.

This is the record of implementing the frozen Cirrus 9.1 interface in the production React
client, on top of the Serein UI framework. It states what was built, what the server contract
forced to differ, what is deliberately not claimed, and how each claim was checked.

The plan this follows is
[cirrus-9.1-integration-plan.md](cirrus-9.1-integration-plan.md). Where that document predicted
a mismatch, this one says what was actually done about it.

---

## 1. What was built

### 1.1 The design system is vendored, not re-implemented

`web/src/serein/` contains `serein-ui.css`, `tokens.css`, `fonts.css` and the three bundled
woff2 faces, copied verbatim from `cirrus-ui/dist/`. Nothing in them was edited.

A copy rather than a dependency, because the package is unpublished and `web/` builds with
`pnpm --frozen-lockfile` in a container where the design-system checkout does not exist. Two
hashes are recorded in `web/src/serein/README.md` and enforced by `pnpm vendor`, so a local edit
to the framework fails the build instead of silently drifting from the approval.

The application's own CSS lives in `web/src/styles/` — one file per surface — and is
**unlayered**, so it outranks the framework's five cascade layers without `!important`. That is
the intended relationship: Serein owns components, the application composes them. No rule in
`styles/` overrides a framework rule out of disagreement; the one attempt to do so was removed
(§4.3).

### 1.2 The shell

One 52px toolbar with three destinations (Home · Files · Shared), a search field that opens a
command palette, an inspector switch and the account menu; a mobile top bar and a tab bar below
820px; a status strip above that breakpoint.

The capability inventory did not shrink. **Trash, Activity, Access and Accounts moved into
Settings**, reached from the account menu, and each is a URL (`?screen=settings&settings=trash`)
rather than a dropdown item with nowhere to point. Home's "Shared with you" now opens the
received view; the previous version's link opened the outbound link manager, which is the defect
this arrangement exists to prevent.

### 1.3 Routing

`web/src/route.ts` is a typed router with `push` for navigation and `replace` for state, a
`popstate` listener, and a `parse` that cannot throw or return a half-built route. The URL
contract is the plan's §3.2 with one substitution forced by the server (§2.1):

| Parameter | Values | Meaning |
| --- | --- | --- |
| `screen` | `home`, `files`, `sharing`, `settings`, `viewer` | destination |
| `space` | folder path | Files location |
| `file`, `from`, `at` | path, collection identity, index | viewer state |
| `tab` | `received`, `managed` | Sharing view |
| `share` | share id | selected link |
| `settings` | `trash`, `activity`, `access`, `admin` | Settings panel |
| `q`, `kind`, `sort`, `dir`, `view` | as in the design | Files state |
| `inspector`, `pane` | `1`, `info\|versions\|activity\|share` | inspector state |

A cold load and a Back button go through the same `parse`, so a truncated or hand-edited URL
lands somewhere valid rather than half-restored.

### 1.4 The surfaces

- **Home** — a 94px resume band (not a media panel), Recent, Shared with me, Links I manage,
  Activity and storage. The band's eyebrow says *Continue where you left off* only when this
  browser has a record of opening a file; otherwise it says *Recently modified*. That
  distinction is the reason the band exists as a component rather than as a heading over a list.
- **Files** — one listing. Folders are the first rows of the same table as files: same height,
  same columns, same menu. The location is a breadcrumb of **ancestors only** (the folder you
  are in is the page title directly below, so repeating it was an eyebrow saying one word
  twice). Filter chips carry client-computed counts; sort and layout are in the URL; selection
  supports click, shift-click, ctrl-click, Space, arrow traversal, ⌘/Ctrl-A, F2, Delete and
  Escape. Dragging a row onto a folder moves it; dropping OS files uploads into the open folder.
- **Sharing** — two labelled views, received by default. The outbound list separates *active*
  from *finished* (expired or exhausted) links, because a spent link is a fact about what was
  shared and deleting it would hide the evidence.
- **Viewer** — one viewer, scoped to an explicit collection (`folder:`, `recent`,
  `search:`, `shared`, `single:`), with an honest per-format renderer: image with zoom and pan,
  video, audio, PDF via the browser's viewer, text and source, CSV/TSV as a real table, and a
  fallback card with a working Download for everything else.
- **Inspector** — four panes (Info, Versions, Activity, Share) that keep their scroll and tab
  across selection changes. Restore is disabled with the reason when the storage layout kept no
  bytes; the Share pane says "URL not recoverable" rather than offering a copy button that
  would copy nothing.
- **Settings** — Trash, Activity, Access and Accounts, each with the server's own vocabulary
  (the audit result, the permission string, the setting's source as `config` or `override`).
- **Public share page** — outside the application shell and unauthenticated, with a **generic
  password gate**: no label, no target name, no item count, no thumbnail, and no claimed
  remaining-attempts budget, because nothing on the page enforces one.

### 1.5 Appearance

Four complete appearances come from the token layer: light, dark, and each with increased
contrast. The attributes live on `<body class="serein-root">`; **"follow the system" is the
absence of an attribute**, which is what makes the first paint correct with no flash and keeps a
live OS change working. Large Text (`data-textsize`) and reduced motion are user choices in the
appearance sheet; the public page carries its own theme switch so a recipient's choice survives
the boundary.

### 1.6 The upload engine

`web/src/upload.ts` is the resumable chunk pump, extracted unchanged in behaviour from the
previous Files view because two surfaces now start an upload. Its invariants are stated in the
file: the server's acknowledged ranges are authority, a failed chunk is retried unchanged, a dead
session is never aborted on failure, and the browser's half of the session survives a reload.

---

## 2. Where the server contract forced a difference

Each of these is a place the frozen design specifies something the API cannot answer. In every
case the interface was changed to say what is true rather than to look like the prototype.

### 2.1 `space` is a path, not a folder id

The plan's contract uses folder UUIDs (`?space=<uuid>`). The listing endpoint addresses
directories by **path** (`GET /api/files/?path=`), and there is no endpoint that resolves an id
to a path for the caller's own tree. A path in the URL is the honest encoding: it is what the
server accepts, it survives a reload, and it cannot address another account's node.

Consequence: a breadcrumb is built from the path's own segments rather than from server-returned
ancestors. Those segments are real (they are the path the server resolved), but a rename higher
up invalidates a bookmarked URL — the same as any path-addressed system.

### 2.2 There is no "All files" aggregation

The design's Files screen aggregates every file in the workspace with a Location column. There
is no endpoint that walks the tree, and inventing one on the client would mean an unbounded
number of listings. So `space` empty means **the workspace root**, and the Location column shows
the row's parent folder within the current listing. The listing is one folder; it does not claim
to be the whole workspace.

### 2.3 "Continue where you left off" is per browser

The plan (D-8) calls for a server-side last-opened record. It does not exist, so the record is in
`localStorage` and is described as what it is: the band's subtitle says *Opened …* only from that
record, the fallback says *Recently modified*, and the docs say "on this device". A synchronised
history would need the table the plan describes; this is not it, and does not pretend to be.

### 2.4 A received node shows an identifier, not a person

`GET /api/shared` returns `owner_id` and no owner name. The received row therefore shows a short
identifier with a monogram derived from it, and the Inspector shows the permission string. The
alternative — inventing a display name — would be a fabricated fact about who shared something.

### 2.5 A received file cannot be rendered inline

`GET /api/shared/{id}/content` answers `application/octet-stream` with an attachment disposition,
by design: it is the copy-taking route, and the content origin is attachment-only. A shared file
therefore gets the server's thumbnail (for an image), the type tile, and a working Download, plus
a sentence explaining why there is no full render. Putting the URL in an `<img>` would produce a
broken picture.

### 2.6 A link's URL is not recoverable

Only the token's hash is stored, so "Copy link" cannot be offered after the create dialog closes.
The Share pane says so and the create dialog keeps the URL in view with a copy button at the one
moment it exists. Issuing a replacement would change the link's identity, which is a decision for
the owner, not a silent convenience.

### 2.7 Versions may not be restorable

The tree layout records version rows without keeping a copy of the bytes. `restoreVersion` is
therefore disabled with `version_content_available === false` and the pane prints the reason,
rather than offering a button that fails.

### 2.8 Per-file activity is a filtered view, not a history

`GET /api/audit/events` has no per-target filter. The Activity pane filters the latest page
client-side and **says so** in a note, which is the difference between "nothing happened to this
file" and "this pane cannot see everything that happened".

### 2.9 Directory sizes are not item counts

A directory's `size` field is not a child count. The listing therefore says only `Folder`, and
the Inspector shows `—` for size. An earlier draft printed "0 items" from that field; it was
wrong for every folder that had children, and it was removed rather than approximated.

### 2.10 Public-share counters

The landing payload carries `max_uploads`/`upload_count` for a drop box but no
`max_downloads`/`download_count` for a plain file. The file landing page therefore shows the
expiry and not a remaining-downloads figure it would have to invent.

---

## 3. What is not claimed

- **No new server capability.** Every gap listed in §2 remains a gap. The integration plan's
  Phase 3–4 server work (collections, favourites, folder counts, owner identity, link rotation,
  per-target audit) is untouched.
- **The public gate is not a security control.** `handleSharePage` still returns `label` and
  `hide_contents` before the password is verified (plan P-01), and the recipient gate's
  attempt-count text was removed rather than backed by a lockout. The interface no longer
  *renders* the label before unlock, which reduces the disclosure on screen; it does not remove
  it from the JSON.
- **No archive or spreadsheet binary decoding.** XLSX/ODS/ZIP get the fallback card. CSV and TSV
  are parsed, with a 30-column and 200-row render cap stated in the caption.
- **No drag-to-dismiss for the bottom sheet**, and no nested submenus: both are documented
  non-goals of the framework.
- **`pnpm gen:api:check` is blocked**, not passing. That check regenerates `src/api-types.ts`
  from `web/openapi.yaml`, and the document in the working tree is being rewritten by another
  change (an `/api/comments/{id}` path whose `200` response `$ref` does not resolve). The client
  compiles against the regenerated types, but the check itself cannot pass until that document
  is valid again. `pnpm check` runs the four checks this change owns; `make webcheck` still runs
  the generation check explicitly, so CI keeps failing on it as it should.

---

## 4. Defects found during verification and fixed

The browser harness found these; each was a real defect, not a harness assumption.

### 4.1 A hidden label is not an accessible name

At phone width the New folder button collapses to its icon by hiding
`.serein-btn__label` with CSS. The button then had no name at all: `innerText` is empty and
there was no `aria-label`. Both page-header actions now carry `aria-label` + `title` with the
same strings the visible labels use.

### 4.2 The mobile create control had a redundant role

`<summary role="button">` replaced the element's native role and dropped its expanded state.
The `role` was removed; `aria-label` on the native `<summary>` is the whole contract.

### 4.3 An override that could not work was removed

Holding all six file-list columns open while the inspector is docked looked like a CSS
specificity problem and was not: the framework's container query sets `--serein-file-cols` on
the row and the header themselves, so an ancestor's value is never consulted, and the mismatch
between the grid template and the visible cells stacked each row into two lines. The rule was
deleted, and the column folding that happens when a 340px panel takes the space is now
documented as the container query doing its job.

### 4.4 A 44px control inside a 2px-padded group

The seg control's buttons were 44×40 inside a group with 2px padding, which is 40px of tappable
button. Height was raised to 44 inside the padding.

### 4.5 A 320px overflow from one unwrappable row

The filter field was given `flex-basis: 100%` inside a `nowrap` tool row and pushed the page to
351px. The tool row now wraps below 700px and the field takes its own line.

### 4.6 Mojibake from a tooling roundtrip

Three files were rewritten through a Windows PowerShell 5.1 `Get-Content`/`Set-Content` pipeline,
which decoded UTF-8 bytes as the system code page: `·` became `쨌` and `—`/`…` became `??`. The
affected characters were repaired and the three files were re-encoded as UTF-8 without a BOM.
No file in the final tree contains a replacement character or a stray Hangul syllable from this
(the browser harness asserts the rendered interface, and the screenshots were reviewed).

### 4.7 A URL that named a file the viewer did not open

`?screen=viewer&file=/data.csv&from=folder:` opened the collection's **first** file, not the one
in the URL: the viewer selected by index and only the `shared` collection resolved its index from
the path. A reload, a shared link and a hand-written URL were all affected. The index is now
derived from `file` whenever the two disagree, and stepping updates both `file` and `at` — which
is what makes "a link opens what I was looking at" true rather than approximately true.

### 4.8 A footer that printed the position twice

For collections of two or three files the viewer footer rendered `2 / 3` in the middle column
and again beside the Previous/Next pair. The middle copy is gone; the filmstrip appears at four
items and above, and the position has exactly one home.

### 4.9 A resume band that contradicted itself

The eyebrow said *Continue where you left off* while the facts line said *Modified 2 minutes
ago*: the opened record was matched into the recent list and its timestamp was lost in the
merge. The record's `openedAt` now travels with the band's subject, so the heading and the
facts always describe the same event.

### 4.10 A CSV labelled and tinted as code

The server reports `preview: "text"` for CSV, TSV and Markdown alike, and the kind mapping
trusted that over the extension — so `data.csv` carried the code tile, the word "Text" and a
`<>` icon. Extension groups that are more specific than `text` (spreadsheet, design, archive)
are now consulted first; Markdown and plain text keep the document icon, and source files keep
the code one.

---

## 5. Verification

All commands run from `web/` unless stated.

| Check | Command | Result |
| --- | --- | --- |
| Types | `pnpm typecheck` | pass |
| Design-system contrast, 4 appearances × 18 pairs | `pnpm contrast` | 72/72 pass WCAG 2.2 AA |
| Translation parity, en ↔ zh-Hans, 606 keys | `pnpm i18n` | pass |
| Vendored framework unmodified | `pnpm vendor` | 6 files, 2 hashed, 4 binary |
| Production build | `pnpm build` | pass; bundle 474.9 kB / 134.9 kB gzip, CSS 182.1 kB / 28.3 kB gzip |
| Embedded-bundle contract | `go test ./internal/webassets` | pass |
| Documentation cites only real tests | `go test ./internal/server -run TestDocumentsCiteOnlyRealTests` | pass |
| Browser walkthrough, 76 assertions | `node scripts/ui-verify.mjs --base … --share <token>` | 76/76 pass |

The browser walkthrough (`web/scripts/ui-verify.mjs`) drives a live `tools/demoinmem` instance
with the real OIDC flow and asserts, among other things: every captured width fits without
horizontal scrolling; the appearance attribute actually matches the screenshot's label; folders
sort before files; arrow keys move `aria-activedescendant`; entering a folder changes the URL and
the page title; the breadcrumb returns to the root and is absent at the root; Ctrl-K opens search
and a real filename is found; a file opens into a URL-addressed viewer whose footer names the
collection and Escape returns; browser Back leaves the folder; the sharing tab is in the URL;
the inspector has four panes and fills from a selected row; the account menu opens and the
appearance sheet changes the root's theme; the palette groups commands and files; a CSV renders
as a table (rows and columns counted); Versions never offers a restore the storage layout cannot
perform; every phone control is ≥44px in both axes; every visible control has an accessible
name; an unknown share link renders the unavailable page with no application shell; a real
password-protected share renders a gate that names neither the file nor the folder and prints no
attempt budget; the fixed phone tab bar covers no page's last element at maximum scroll; and
there are no console errors or failed requests after sign-in.

Screenshots are written to `web/artifacts/ui/` (gitignored, regenerable): 18 viewport captures
plus the viewer, the CSV table, the selected inspector, the account menu, the appearance sheet in
light and dark, the palette, the share gate and the unavailable page — covering 1440, 1024, 390
and 320, light and dark, and Large Text.

Two notes on the harness:

- The 401s produced by the OIDC bounce are counted and reported, then cleared before the
  assertions. An unauthenticated shell asking `/api/auth/me` is how sign-in is detected, not an
  interface error.
- `playwright-core` is resolved by path (`--playwright` or `PLAYWRIGHT_CORE`), defaulting to
  `cirrus-ui/node_modules`. `web/` deliberately has no browser-automation dependency; the script
  says so instead of failing on a bare import.

---

## 6. File map

```
web/
├── index.html                     serein-root on <body>, no hard-coded appearance
├── scripts/
│   ├── contrast.mjs               reads the vendored tokens; 4 appearances
│   ├── i18n-check.mjs             unchanged
│   ├── vendor-check.mjs           new: the vendored framework hash gate
│   └── ui-verify.mjs              new: browser walkthrough + screenshots
└── src/
    ├── serein/                    vendored framework (unmodified) + provenance
    ├── styles/                    app.css, shell, files, home, sharing, viewer, surfaces
    ├── route.ts                   typed router (push/replace/popstate)
    ├── appearance.ts              attribute bridge + persistence
    ├── upload.ts                  resumable chunk pump, extracted
    ├── nodes.ts                   shared node vocabulary (access, preview, path)
    ├── ui.tsx                     icons, toasts, announcements, states, kind mapping
    ├── shell/Shell.tsx            topbar, mobile chrome, tab bar, account menu, appearance
    ├── shell/SearchPanel.tsx      command palette + name search
    ├── components/                Dialogs, FolderAccess
    └── views/                     Home, Files, Sharing, Viewer, Inspector, Settings,
                                   Activity, Admin, ShareLanding
```

`web/src/styles.css` is now only an import manifest; the 2,372-line Atrium stylesheet it
replaced is gone.
