# ADR-0007: S3 storage backend

**Status:** Accepted · **Date:** 2026-10-21

newCloud implements the S3 backend as a `blob.Store` in `internal/blob/s3`, wired into the existing `blobstore` so every write path continues to use one content service. The implementation uses minio-go, hashes bytes before upload, reuses minio-go's seekable object for reads, enables TLS unless it is explicitly disabled, and requires the bucket to exist. It is verified against an in-process fake by default and against a moto server in CI, but not against a real S3 service; it builds on the two storage-layout axes defined in [0003-storage-layout.md](0003-storage-layout.md).

## Problem

`storage.backend: s3` appeared in the configuration from the start, but no implementation existed: startup failed with an error. That was better than a half-working backend, but it still left a gap between the product promise and the capability. A gap written into an example configuration is read as support.

## Decision

Implement `blob.Store` (`Put` / `Open` / `Delete` / `Stat`) in `internal/blob/s3`, connected to the existing `blobstore`. The change adds **no** second write path: the web UI, WebDAV, sharing, versions, and audit still share one content service. S3 changes only where the bytes are stored.

The two axes remain unchanged:

| Axis | Values | Behavior under S3 |
|---|---|---|
| `backend` | local / s3 | Changes where bytes are stored |
| `layout` | blob / tree | `tree` combined with S3 is rejected: it is a filesystem mirror by definition |

## Why minio-go rather than aws-sdk-go-v2

Both clients connect to AWS. minio-go was chosen because the product's users mostly run self-hosted storage: MinIO, Ceph, R2, B2, and Wasabi. minio-go is written for these endpoints, with an explicit endpoint, path-style addressing, no region-resolution surprises, and a much smaller dependency tree. It is also the client used by rclone. The project direction calls for mature libraries over a custom protocol implementation; both are mature libraries, and the difference is fit.

## Four specific decisions

**Hash before upload.** `Put` reads the bytes once to compute SHA-256 and compares the digest with the requested content address before uploading. The cost is one extra read of the staging file. The benefit is that content whose **source** does not match its address never enters the bucket. It is not a transport-integrity check: the hash is computed on the input before minio-go sends it, and nothing reads the object back after the upload. A fault that corrupts bytes in flight can therefore store an object whose bytes do not match its key. The integrity scrub and the restore drill are what detect that later. See [Limitations](../guides/limitations.md).

The reverse order—upload first, delete after a failed check—would leave the bad object in place at exactly the moment something went wrong, when nobody is usually watching.

**`Open` reuses minio-go's `Object` directly.** The object implements `io.ReadSeeker`, and `Seek` becomes a Range request, which is exactly what `http.ServeContent` needs. Writing a custom seekable reader over Range requests would reimplement the one thing the library already gets right.

**TLS is on by default.** `secure` is a `*bool`, and the zero value (unset) is treated as `true`. Sending object data in plaintext must be an explicit choice, not a default inherited from a zero value. A test guards this: in the wiring test, forgetting `secure: false` makes the request go to `https://` and fail immediately.

**The bucket must exist in advance.** Creating a bucket requires account-level permissions that a storage service should not have. A mistyped bucket name should fail at startup, not silently create a second bucket.

## Validation

| Layer | Method |
|---|---|
| Client contract | `internal/blob/s3/s3_test.go`: round trip, seek (including reverse), hash-mismatch rejection without writing to disk, idempotency, size mismatch, delete and missing-object semantics, 17 MiB multipart upload |
| Interchangeable content layer | `TestS3Conformance`: runs the **same** `layouttest` conformance suite (17 subtests: overwrite, versions, delete, list, cross-owner rejection, and others) against the S3 backend |
| Wiring | `cmd/newcloud`: `openStorage` actually constructs a blob-mode store and accepts bytes; `tree`+`s3` and unknown backends are rejected |
| Configuration | Endpoint required, keys must appear in pairs, example configuration validates |

S3 tests skip when `TEST_S3_ENDPOINT` is not set, so `go test ./...` still works on machines without object storage. By default the suite runs against an in-process fake. CI starts a moto server and points the environment variable at it, so the same tests also exercise a real HTTP endpoint.

## Boundary: what was not proven

**No interoperability test has run against real AWS S3 or MinIO.** Validation uses an in-process fake and, in CI, a moto server. Both implement the S3 API; neither is the real service. They can prove that the client's request shapes, multipart uploads, ranges, and error-code handling are correct. They cannot prove the following:

- eventual-consistency behavior of a real server
- retries of large multipart uploads under real network jitter
- subtle differences in `UserMetadata` and path-style handling on R2, B2, or Wasabi
- coexistence of bucket policies, lifecycle rules, and server-side encryption with this client

Each item needs acceptance against a real backend. Until then, the README and this document describe the backend as "implemented and verified only against a simulator," not as "supports AWS S3."

## Not implemented

- Server-side encryption (SSE-S3 / SSE-KMS): bucket policy should own this. Forcing an extra client-side layer would conflict with server-side encryption.
- Resumable multipart uploads: one `Put` either succeeds completely or leaves no visible object, which is sufficient. Resuming across requests is a different order of magnitude.
- A direct Range proxy to S3 that bypasses the application: it would bypass authorization, so it is not done.
