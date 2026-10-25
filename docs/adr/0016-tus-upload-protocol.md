# ADR-0016: tus as the resumable upload protocol

**Status:** Proposed · **Date:** 2026-10-25 · **Supersedes:** ADR-0008's wire protocol (its durability
machinery is kept) · **Amends:** `core-scope-2026-10-25.md` §1

> **Numbering note.** `core-scope-2026-10-25.md` asks for this decision to be filed as ADR-0015. That
> number was taken between the spec being written and this change landing: `0015` is now the
> Nextcloud client-compatibility ADR. The decision is unchanged; only the number moved, and it is
> recorded here rather than silently renumbered, because a document that cites ADR-0015 for tus would
> point at the wrong page.

## Context

ADR-0008 chose a bespoke chunked-upload protocol: `POST` a session, `PUT` chunks with
`Content-Range`, `POST .../complete`, with the server owning the acknowledged set. Its argument was
that tus and S3 multipart "are larger protocols with their own state machines", and that the custom
one covered parallel, out-of-order chunking with resume "without taking on a specification the client
would also have to implement correctly".

The second half of that argument was wrong, and it was wrong in the direction that costs users most.
**Every client that wants to upload a large file already implements a specification**: tus. A browser
application, a mobile app and a command-line tool all have a tus client available as a library; none
of them have a client for this server's protocol. So the choice was not "a small protocol" versus "a
large one" — it was "a protocol nobody implements" versus "one whose client already exists". The
custom implementation had to be written on both sides, and it was written on only one.

The durability side of ADR-0008 was a different matter and was right. The staging file, the fsynced
metadata record written *after* the bytes it describes, restart recovery with its consistency rules,
and the graceful-shutdown flush are protocol-independent guarantees. They cost real work and they
solve the actual problem: an upload that survives a deploy.

## Decision

Adopt **tus Resumable Upload Protocol v1.0.0** as the wire protocol, with the extensions the standard
client libraries actually use:

| Extension | Why |
| --- | --- |
| `creation`, `creation-with-upload` | One round trip for a small file. |
| `creation-defer-length` | A client that does not know the size up front, bounded by `limits.max_file_bytes`. |
| `expiration` | Reuses `limits.upload_session_ttl_hours`; `Upload-Expires` is returned and the `upload-cleanup` duty already enforces it. |
| `termination` | `DELETE` is the abort, with the same effect as the custom endpoint's. |
| `checksum` | `Upload-Checksum: sha256 <base64>`; a mismatch is **460** and the chunk is not acknowledged. |
| `concatenation` | Partial uploads joined by `Upload-Concat: final`. This is how `tus-js-client` implements `parallelUploads`, and it replaces the out-of-order parallelism the custom protocol offered. |

**The durability machinery is unchanged.** A tus session maps onto the existing session manager:
creation is `Create`, `PATCH` is `Append`, `HEAD` is `Get`, `DELETE` is `Abort`, and the upload that
reaches its declared length runs the same `complete` guard set and the same `publishFile`. No second
implementation of staging, fsync ordering, recovery or publish exists.

**The product principles are kept, expressed in tus terms:**

- *Refusals decided before any byte is written.* Creation runs the target-directory authorization,
  the fold-name conflict check and the quota reservation. Finalization re-runs every guard inside the
  statement that changes the row, so an upload that takes hours cannot slip past a check that was
  true when it started.
- *The server owns the acknowledged set.* `Upload-Offset` is authoritative. A `PATCH` naming a stale
  offset is **409** and changes nothing, and the response carries the server's true offset — which is
  what makes resume possible rather than merely refusing the request.
- *A checksum is verified before an offset advances.* The chunk's bytes may land in the staging file,
  but the range is not acknowledged, so a retry sends the same range. The opposite ordering would
  tell the client the corrupt chunk is already there.
- *The row is the identity.* A tus session carries the destination and the filename it was created
  with, and the publish path derives the node from them; the id a client holds addresses the session,
  never the file.

**Parallelism changes shape.** tus core is strictly append-at-offset, so the custom protocol's
out-of-order chunking is gone. Parallelism comes from several partial uploads joined at the end —
which is not a downgrade: it is the mechanism the mainstream client already uses, it makes each
chunk's offset trivial to reason about, and the join is a byte-wise concatenation whose result is
indistinguishable from a sequential upload of the same file.

**The custom endpoints stay for one release, marked deprecated.** Responses carry `Deprecation`,
`Sunset` and `Link: rel="successor-version"`. A changelog entry is read by a person who may not be the
one maintaining the client; a header is read by the code.

## Consequences

Easier:

- A client author writes no upload code: they configure a tus endpoint URL.
- Resume is standard, so the failure modes are known quantities rather than ours.
- The wire protocol and the durable session record are now separable concerns, which is what made
  this swap cheap in the first place.

Harder:

- **Two protocols exist for one release.** Both are tested; both must keep working. The deprecation
  headers are the migration signal, and the `Sunset` date is what a scheduler can read.
- **`sha256` is the only checksum.** It is the one hash this server computes and stores. A client
  that asks for `md5` gets a refusal rather than a "verified" that verified nothing.
- **Concatenation refuses the ambiguous case.** Partials joined into one file must share a
  destination directory; a client that spreads them across directories is answering a question this
  server will not guess at.
- **Deferred length may be declared once, before any byte lands.** Shrinking a total below bytes
  already acknowledged would strand ranges outside the file, and growing it would make the per-file
  limit advisory.

## What is not decided here

- tus **`creation-defer-length` combined with concatenation** (a final upload whose own length is
  deferred) is not supported; the join sums the parts and declares the total itself.
- **Checksum trailers** (`Checksum-Trailer`) are accepted in the sense that an `Upload-Checksum`
  delivered as a trailer is honoured by the same code path, but no client in the ecosystem is known
  to send one, so it is not separately tested.
- The **removal** of the custom endpoints. That is the next release's change, and it needs the
  deprecation window to have passed.
