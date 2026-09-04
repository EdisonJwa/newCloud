# ADR-0008: Resumable chunked uploads

**Status:** Accepted · **Date:** 2026-10-21

newCloud keeps single-request uploads and adds resumable chunked endpoints for larger files. Upload sessions are process-local, the server owns the offset, failed chunks do not advance it, and both upload paths publish through `publishFile` so version retention, audit writes, and ETag computation stay on one path. The protocol is deliberately smaller than tus or S3 multipart; a client needs roughly 40 lines of slicing logic.

## Problem

Single-request uploads always worked, but they have a property that cannot be avoided on real networks: **a dropped connection means starting over**. When a 20 GiB file fails at 19 GiB, the user loses time and also confidence that the file can be uploaded at all. For a product that sells TB-scale self-hosted storage, this is not an optimization.

## Decision

Keep single-request uploads and add a set of chunk endpoints:

```text
POST   /api/files/uploads                -> {id, offset, chunk_size}
PUT    /api/files/uploads/{id}           Content-Range: bytes a-b/total
POST   /api/files/uploads/{id}/complete
DELETE /api/files/uploads/{id}
```

Clients use chunked upload above 8 MiB and single-request upload below that. Chunking a small file only adds one session-creation round trip.

### Why neither tus nor S3 multipart

Both are complete protocols, each with a state machine and client implementations. The capability needed here is narrow: sequential chunks plus resumption. Adopting tus would mean clients also have to implement it correctly, while S3 multipart applies only to the S3 backend and is useless for a local disk layout. The cost of a custom protocol is roughly 40 lines of slicing loop in the client—already written and verified end to end in a browser.

### Sessions live in memory, a deliberate boundary

Sessions survive a dropped connection, not a process restart. Persisting sessions would require a table, a migration, a cleanup job, and handling inconsistencies such as "the row exists but the staging file is gone." The vast majority of failures are client disconnects, not server restarts. A clearly stated boundary is more honest than a half-reliable persistent version.

The only residue a crash can leave is orphan files in the staging directory, so `NewManager` cleans them at startup. It removes only file names this package can create (the `part-` prefix) and leaves everything else in the directory untouched. Deleting the wrong file costs far more than leaving one orphan.

## Three specific decisions

**The server owns the offset.** A chunk must start at the server's current offset; a mismatch returns 409 with the true offset. The client resumes from that value instead of guessing. Allowing arbitrary offsets would let retries and new requests write interleaved data, producing a file whose hash does not match.

**Failed requests do not advance the offset.** When a chunk write fails partway, the code calls `Truncate` on the staging file to return it to the original offset. Otherwise the file is left with a hole, and a later resume writes after the hole—producing a file with the correct length and the wrong content. That is the worst kind of corruption because it passes size checks.

**Both write paths share one publish function.** `publishFile` handles version retention and audit writes; both single-request and chunked uploads call it. One test asserts that both produce the same ETag for the same bytes. That is evidence of one path, not merely that both paths work.

## Lock ordering

The session manager has two locks: the manager lock and the per-session lock. The rule is **always manager lock first, then session lock, and never the reverse**. Finish and Abort first take the manager lock to remove the session from the registry, release it, and then take the session lock for file operations.

The original code took the manager lock while holding the session lock in Finish's conflict branch, while Sweep used the opposite order. That is a textbook deadlock, and a test (concurrent chunks) runs in that window. Conflict determination now happens under the manager lock, which is the allowed direction, so an incomplete session never leaves the registry.

## Validation

| Layer | Content |
|---|---|
| Session state machine (14 tests) | Round trip, wrong offset, failed chunk does not advance, only one concurrent chunk accepted, owner isolation, per-user limit, expiration and cleanup, startup cleanup of orphans, refusal to exceed the declared size |
| HTTP protocol (9 tests) | Complete round trip, 409 with the true offset, incomplete upload cannot publish, six malformed `Content-Range` inputs, unknown session always 404, target-directory authorization, over limit, invalid after abort, **both upload types produce the same ETag** |
| Browser end to end | `demoshot -test-chunk` drags in a 20 MiB file and asserts that the server-reported byte count equals the input **exactly** |

The browser check is necessary: unit tests cannot prove that the frontend actually slices the file or that the slices can be reassembled.

## A tooling defect fixed along the way

`jsShowFiles` in `demoshot` still clicked `.rail-nav button` from the old sidebar. After the React redesign, the selector matched nothing, so the drag-and-drop check actually dragged a file on the home page, where there is no drop target, and reported "no new files." A dead selector does not produce an error; it makes later assertions test the wrong page. It now clicks the top bar and fails directly if the element cannot be found.

## Not implemented

- Sessions do not survive a process restart (see above).
- No parallel chunks: one request at a time. Parallelism would require a server-side bitmap and out-of-order assembly. The benefit is not clear when a single connection is bandwidth-limited, and the complexity is another order of magnitude.
- An upload cannot be paused and resumed the next day: sessions expire after 24 hours.
