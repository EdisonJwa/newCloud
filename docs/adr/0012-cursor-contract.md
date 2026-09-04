# ADR-0012: Cursor pagination contract

**Status:** Accepted · **Date:** 2026-10-21

T-19 requires that a large-directory listing permit no privilege escalation and that duplicate and omission behavior follow a stated cursor-consistency contract. The decision is an opaque, name-based cursor: each page names a position in the name order rather than a count, clients echo the token rather than construct it, and no row is ever returned twice during a walk. This document is that contract.

## What a cursor is

The listing endpoint pages by an **opaque `cursor`** returned with the previous page:

```http
GET /api/files?path=photos&limit=100
→ { "children": [...], "cursor": "IMG_2043.jpg", "truncated": true }

GET /api/files?path=photos&limit=100&cursor=IMG_2043.jpg
```

`truncated` is kept for clients that only understand the older shape; `cursor` is what actually lets them continue rather than merely notice.

The cursor is the last name on the page. The next page is `name > cursor`, ordered by name. It is opaque to clients: it happens to be a filename today and may become an opaque token later, so clients must echo it back rather than construct it.

## Why not an offset

An offset (`?page=3`) over a name sort **silently duplicates and drops rows** whenever a sibling is created or deleted between requests — the exact failure T-19 describes. Concretely: delete row 1 of page 1 and everything shifts up, so page 2 repeats a row. Insert at the front and page 2 skips one. A name cursor does not have this property, because it names a position rather than counting one.

## The contract

Given a directory listing in progress while the directory changes, the walk guarantees:

| Event | Guarantee |
| --- | --- |
| **Insert** a name **after** the cursor | Seen later in the same walk. |
| **Insert** a name **before** the cursor | Not seen in this walk; visible on the next full listing. Never a duplicate. |
| **Delete** an already-seen row | Nothing lost. |
| **Delete** a not-yet-seen row | It is not returned. Nothing else is affected. |
| **Rename** a not-yet-seen row to a name **after** the cursor | Seen later in the same walk. |
| **Rename** a not-yet-seen row to a name **before** the cursor | **Not seen in this walk.** Still exists; the next listing shows it. |
| **Rename** an already-seen row | Not returned again. |
| Any of the above | **No row is ever returned twice.** |

The final row is the guarantee that matters. A duplicate can make a client act twice — delete a file it already deleted, re-upload something it already uploaded, or, worse, process the same share download twice. A row missed until the next listing is merely delayed.

### The rename-across-cursor limitation, stated plainly

This is the one case that loses a row for the duration of a walk, and it is inherent to a name-sorted cursor: the cursor *is* a name, so moving a row across it moves the row to the other side of the walk's position.

It cannot be fixed by switching to offsets, which trades a rare omission for frequent duplicates. It could be fixed with a snapshot (`WHERE modified_at < $txn_start`), which is a schema and index change this implementation does not make today. Until then the limitation is documented, tested, and pinned: `RenameAcrossCursorIsTheDocumentedLimitation` fails deliberately if the behavior ever changes, so the contract cannot drift away from the code.

## No privilege escalation across pages

The authorization check runs **per request, on the directory being listed**, before any page is read:

- The actor is re-resolved from their session on every request; a cursor is not a capability and carries no authority.
- `PermListChildren` is checked on the target directory for each page. Revoking access between page 1 and page 2 makes page 2 fail with 404, not return the remaining rows.
- A cursor from one directory used against another is just a name; it cannot select rows that the second directory's permission check did not already allow.

There is no server-side page state to go stale: the cursor contains nothing but a position, so there is no session to hijack.

## Where this is enforced

| Property | Test |
| --- | --- |
| Insert after cursor is seen | `CursorContractUnderMutation/InsertAfterCursorIsSeen` |
| Insert before cursor is not duplicated | `CursorContractUnderMutation/InsertBeforeCursorIsNotDuplicated` |
| Delete does not break the walk | `CursorContractUnderMutation/DeleteDoesNotBreakTheWalk` |
| Rename limitation is stable | `CursorContractUnderMutation/RenameAcrossCursorIsTheDocumentedLimitation` |
| Static pagination is stable | `ChildrenAfterPaginatesStably` |
| A cursor does not outlive its permission | `TestCursorDoesNotOutlivePermission` |
| The HTTP walk sees each row once | `TestCursorPagingWalksEveryRowExactlyOnce` |

Both backends run the same suite through `internal/storetest`, because the walk is implemented twice (a SQL `name > $2` and an in-memory filter) and a contract that holds on one backend only is not a contract.

## What is not covered

- **Snapshot isolation.** A walk is not pinned to a transaction or a timestamp; it sees the directory as it is at each request. Requesting a true repeatable-read listing is a larger change.
- **Sort order stability across locales.** Ordering is byte-wise (`ORDER BY name` in PostgreSQL, `sort.Strings` in memory), so it does not follow a client's locale collation. This is deliberate: the cursor must mean the same thing on every request, and a locale-dependent order would break that.
- **Cursor expiry.** Cursors do not expire, because they carry no state to expire.
