# ADR-0008: Resumable chunked uploads

**Status:** Accepted · **Date:** 2026-10-21

newCloud keeps single-request uploads and adds durable, resumable chunked endpoints for larger files. Sessions survive a crash or a graceful restart, chunks may arrive in parallel and out of order, and both upload paths publish through `publishFile` so version retention, audit writes, and ETag computation stay on one path. The protocol is deliberately smaller than tus or S3 multipart; a client needs a slicing loop and a small range set, not a state-machine implementation.

## Problem

Single-request uploads always worked, but they have a property that cannot be avoided on real networks: **a dropped connection means starting over**. When a 20 GiB file fails at 19 GiB, the user loses time and also confidence that the file can be uploaded at all. For a product that sells TB-scale self-hosted storage, this is not an optimization.

## Decision

Keep single-request uploads and add a set of chunk endpoints:

```text
POST   /api/files/uploads                -> {id, offset, chunk_size, ranges}
GET    /api/files/uploads/{id}           -> {id, offset, chunk_size, ranges}
PUT    /api/files/uploads/{id}           Content-Range: bytes a-b/total
POST   /api/files/uploads/{id}/complete
DELETE /api/files/uploads/{id}
```

Clients use chunked upload above 8 MiB and single-request upload below that. Chunking a small file only adds one session-creation round trip.

`offset` is the contiguous acknowledged prefix: `[0, offset)` is present. `ranges` is the full sorted set of half-open acknowledged intervals. A sequential client can ignore `ranges` and resume from `offset`; a parallel client uses `ranges` to find holes, including holes below the highest accepted byte. `GET` is the authoritative resume state because a browser's `localStorage` record is only a hint.

### Why neither tus nor S3 multipart

Both are complete protocols, each with a state machine and client implementations. The capability needed here is narrow: sequential chunks plus resumption. Adopting tus would mean clients also have to implement it correctly, while S3 multipart applies only to the S3 backend and is useless for a local disk layout. The cost of a custom protocol is roughly 40 lines of slicing loop in the client—already written and verified end to end in a browser.

### Sessions are durable in the staging directory

Each session writes a JSON record beside its staging file. The record names every acknowledged range (`ranges`) and the contiguous prefix (`offset`). The bytes are fsynced before the record that acknowledges them, so a restart never resumes at an offset for bytes that might not have landed. A record written by the old sequential-only version has no `ranges` field; recovery treats it as `[0, offset)`, so an upgrade in the middle of an upload resumes rather than discarding the transfer.

A crash can leave only unacknowledged bytes at the tail or an orphan in the staging directory. `NewManager` truncates the tail past the highest acknowledged range and removes only file names this package creates (the `part-` prefix), leaving everything else in the directory untouched. It does **not** truncate at the contiguous offset: with out-of-order ranges that would delete acknowledged bytes beyond a leading hole.

## Three specific decisions

**The server owns the acknowledged SET, not a cursor.** A chunk may start anywhere inside the declared size. It is accepted if it does not overlap an acknowledged range or another chunk currently being written; an overlap returns 409 with the contiguous prefix and the full range set. Clients resume from those values instead of guessing. `Finish` requires the contiguous prefix to reach the declared total, so a hole can never be published.

**Overlapping writes are reserved, not raced.** Before reading the body, `Append` reserves the range under the session lock and increments a writer count. The copy itself runs outside the lock and uses `WriteAt`, so disjoint chunks really are concurrent rather than queued behind one `Seek`+`Write`. A failed chunk is not rolled back by truncation — truncating would destroy later out-of-order ranges — it simply never becomes acknowledged, and the retry overwrites it. A failed local write (for example a closed handle) invalidates the session; a network failure does not.

**Both write paths share one publish function.** `publishFile` handles version retention and audit writes; both single-request and chunked uploads call it. One test asserts that both produce the same ETag for the same bytes. That is evidence of one path, not merely that both paths work.

## Lock ordering

The session manager has two locks: the manager lock and the per-session lock. The rule is **always manager lock first, then session lock, and never the reverse**. Finish and Abort first take the manager lock to remove the session from the registry, release it, and then take the session lock for file operations.

Parallel chunks add a third constraint: an `Append` must not hold the session lock while its request body is being read, because that would serialise the network. It reserves a range, releases the lock, and finalises under the lock. Teardown (Finish, Abort, Sweep, Close, Shutdown) sets `closing` under the session lock to stop new reservations, then waits on a condition variable for the writer count to reach zero before closing the handle. If teardown took the manager lock and waited there, every other session operation would block behind an unrelated slow upload, so it releases the manager lock before waiting.

The original code took the manager lock while holding the session lock in Finish's conflict branch, while Sweep used the opposite order. That is a textbook deadlock, and a test (concurrent chunks) runs in that window. Conflict determination now happens under the manager lock, which is the allowed direction, so an incomplete session never leaves the registry.

## Validation

| Layer | Content |
|---|---|
| Session state machine | Round trip, out-of-order assembly, eight disjoint chunks in parallel, overlap conflict with the acknowledged set, failed chunk does not advance, only one concurrent claim on one range, owner isolation, per-user limit, expiration and cleanup, startup cleanup of orphans, refusal to exceed the declared size, zero-length chunk reserves nothing |
| Durability | Restart with a hole, out-of-order ranges restored, recovery truncates only past the highest acknowledged range, graceful shutdown lets an in-flight writer finish and persist, repeated restart cycles, legacy records without `ranges` treated as `[0, Offset)` |
| HTTP protocol | Complete round trip, out-of-order chunk plus status, 409 with the true range set, incomplete upload cannot publish, malformed `Content-Range` inputs, unknown session always 404, target-directory authorization, over limit, invalid after abort, **both upload types produce the same ETag** |
| Browser end to end | `demoshot -test-chunk` drags in a 20 MiB file and asserts that the server-reported byte count equals the input **exactly**; the browser sends up to three chunks at once |

The browser check is necessary: unit tests cannot prove that the frontend actually slices the file or that the slices can be reassembled.

## A tooling defect fixed along the way

`jsShowFiles` in `demoshot` still clicked `.rail-nav button` from the old sidebar. After the React redesign, the selector matched nothing, so the drag-and-drop check actually dragged a file on the home page, where there is no drop target, and reported "no new files." A dead selector does not produce an error; it makes later assertions test the wrong page. It now clicks the top bar and fails directly if the element cannot be found.

## Not implemented

- A chunk that overlaps an acknowledged range is refused rather than merged byte-by-byte. The client is expected to split its work at chunk boundaries; accepting a partial overlap would mean deciding which of two bodies owns the shared bytes.
- The acknowledged set is a sorted interval list, not a bitmap. That is exact and compact for the tens of thousands of chunks a very large file produces, but `Finish` still checks the contiguous prefix rather than scanning a persisted bitmap.
- Cross-node resume is unavailable: sessions are local to one `upload_dir`, because the staging file and its record are a pair on that filesystem.
- An upload cannot be paused and resumed the next day: sessions expire after 24 hours by default (`limits.upload_session_ttl_hours`).
