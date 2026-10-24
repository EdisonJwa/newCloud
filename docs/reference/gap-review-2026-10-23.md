# Gap review — WebCloud requirements v0.2 vs. the `edison-webcloud` tree

**Reviewed:** 2026-10-23 · **Tree:** `C:\Users\admin\Documents\edison-webcloud` @ `f4815b1` (+5 uncommitted files)
**Input:** [`WebCloud_Functional_Requirements_and_Gap_Analysis_v0.2.md`](C:/Users/admin/Downloads/WebCloud_Functional_Requirements_and_Gap_Analysis_v0.2.md)
**Method:** read the requirements, mapped every P0/P1 ID and every `T-xx` to code and tests, then tried to falsify the project's own gap claims. `go build ./...` and `go test ./...` both pass here (in-memory store; no Docker/PostgreSQL available on this host, so `TEST_DATABASE_URL` suites did not run).

## Verdict in one line

The engineering is far ahead of both documents. The real risk now is **not missing features — it is that the gap documentation has drifted out of date and, in one case, an authorization comment claims a check the code does not perform.**

---

## 1. The one finding I would fix first (security, verified by test)

**A grant of `read` without `download` still yields image content through the thumbnail endpoint.**

[`thumbnail.go:62`](internal/server/thumbnail.go#L62) states *"The same capability a download requires. A thumbnail is content."* — but [`thumbnail.go:68`](internal/server/thumbnail.go#L68) checks `PermRead`:

```go
// The same capability a download requires. A thumbnail is content.
if !cap.Can(authz.PermRead) {   // ← comment says download, code says read
```

Every other content path correctly checks `PermDownload` — [`files.go:541`](internal/server/files.go#L541), [`shared.go:178`](internal/server/shared.go#L178), [`content.go:228`](internal/server/content.go#L228). §4.2 and the project's own [`finegrained_test.go`](internal/server/finegrained_test.go) exist specifically to make `read`-without-`download` enforceable.

I confirmed this with a throwaway test (since removed) against a grant of `read+list`, no `download`:

```
shared content (needs download): 404
thumbnail (code checks PermRead): 200
MISMATCH: download refused (404) but thumbnail served (200)
```

**Why it matters:** a thumbnail is a decoded, downscaled copy of the pixels. A permission split that the whole ACL layer is built to honour is defeated by appending `?id=<uuid>` to a different route. This contradicts INV-01 ("no entry point can bypass a resource boundary") and §6.3 ("thumbnails … are protected resources derived from the original file").

**Fix is one line** (`PermRead` → `PermDownload`), plus a case in `finegrained_test.go` mirroring `TestDownloadRequiresDownloadNotJustRead`. Note the share path at [`sharepages.go:767`](internal/server/sharepages.go#L767) gates on `sh.CanRead()`, which is arguably correct for a link that grants viewing — but the authenticated route has no such justification.

<details>
<summary>Probe used (paste into <code>internal/server/</code> and run)</summary>

```go
func TestProbeThumbnailVsDownloadGrant(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "probe.png", "image/png", samplePNG(t))
	node := h.resolveNode(t, "probe.png")

	mate := h.meta.AddUser(store.User{
		Username: "probe-reader", Email: "probe@example.test", Role: "user",
	})
	h.meta.EnsureRoot(context.Background(), mate.ID)
	h.grantOn(t, node.ID, mate.ID, store.PermRead|store.PermListChildren, true) // no download
	tok, _ := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	c := h.cookieClient(tok)

	dl := h.adminGet(t, c, "/api/shared/"+node.ID.String()+"/content")
	dl.Body.Close()

	th := h.adminGet(t, c, "/api/files/thumbnail?id="+node.ID.String())
	th.Body.Close()
	t.Logf("content=%d thumbnail=%d", dl.StatusCode, th.StatusCode)
}
```
</details>

---

## 2. Documentation drift (the gap list is wrong in both directions)

The project is unusually honest about gaps, and it even tests the gap table for staleness ([`gapstaleness_test.go`](internal/server/gapstaleness_test.go), [`limitations_test.go`](internal/server/limitations_test.go)). Those tests check only four hard-coded artefacts, so the following slipped through:

| Claim | Location | Reality in the tree |
| --- | --- | --- |
| "**No data export bundle** … there is no one-command account export" | [limitations.md:29](docs/guides/limitations.md#L29) | `newcloud export` exists — [`cmd/newcloud/export.go`](cmd/newcloud/export.go), 388 lines, with a standalone manifest and SHA-256 verification, and `TestExportWritesTheTreeAndAVerifiedManifest` |
| "§9.5 Data import and export — **Not implemented**. No import or export path exists" | [implementation-record.md:1460](docs/reference/implementation-record.md#L1460) | Same command. The export half is done; only import is missing |
| "**Advertised chunk size** … a chunk larger than the advertised size is accepted" | [limitations.md:51](docs/guides/limitations.md#L51) | Enforced — [`uploads.go:148`](internal/server/uploads.go#L148) returns `413 chunk_too_large`. Commit `0038bb4` is literally titled "Enforce limits.upload_chunk_bytes instead of only validating it" |
| "`upload_chunk_bytes` … **a chunk larger than this is not refused**" | [configuration.md:106](docs/guides/configuration.md#L106) | Same enforcement; `configuration.md` contradicts both the code and `limitations.md` |
| "**Audit retention** — `audit.retention_days` is accepted and has no effect" | [limitations.md:50](docs/guides/limitations.md#L50) | `newcloud audit prune` archives then prunes with a signed `prune_marks` boundary ([`main.go:122`](cmd/newcloud/main.go#L122), [`auditprune.go`](internal/store/auditprune.go)). The *automatic* sweep is missing; the capability is not |
| "`retention_days` — **Not applied today** … nothing prunes it" | [configuration.md:127](docs/guides/configuration.md#L127) | Same. `audit prune` with no `--days` reads exactly this key |
| "**SHARE-08 to SHARE-11 — Not implemented**" | [implementation-record.md:1461](docs/reference/implementation-record.md#L1461) | SHARE-08 (group sharing), SHARE-09 (delegation) and SHARE-10 (`/api/shares` + `/api/shared`) are all implemented and tested — `TestGroupSharingWorksEndToEnd`, `TestADelegatedShareServes`, `TestARefusedGrantRevokesTheDelegatedLink`. Only SHARE-11 (team spaces) is genuinely absent, which the traceability test already records in `deferred` |

The last row is the sharpest: [`internal/requirements/traceability_test.go`](internal/requirements/traceability_test.go) *requires* SHARE-08/09/10 to be cited in production code and fails if they are not — while the gap table beside it says they are not implemented. The two files disagree about the same three IDs.

In the opposite direction, these are declared but undocumented in `docs/guides/configuration.md`:

| Key | Why it matters |
| --- | --- |
| `trusted_proxies` | The *security fix* for the X-Forwarded-For bypass. `trust_proxy` is marked deprecated in code, absent from the docs, and present only as a commented-out line in the example — so an operator reading the docs has no way to configure a reverse proxy correctly |
| `max_trash_bytes`, `max_pending_upload_bytes`, `max_retained_version_bytes` | The three §4.3 retained-usage caps — three of the newest commits |
| `upload_cleanup_grace_hours` | §9.3 multipart cleanup window |
| `usercontent_addr` | Content-origin split inside the process (§9.4) |
| `archive_dir`, `anchor_signing_key_file` | Audit archive destination and signed checkpoints (§10.2) |
| `sse`, `sse_kms_key_id` | S3 server-side encryption policy (§9.4) |

The `configuration.md` header promises "Every configuration key, with defaults", but several document sections are covered by table rows rather than literal key names (`s3.*`, `jit.*`), so a naive string diff overstates the gap. What is genuinely absent is the table above — including the proxy-trust key and all three retained-usage caps. No test compares the struct to the docs: [`example_test.go`](internal/config/example_test.go) only checks that the example *parses*.

---

## 3. `T-xx`: the acceptance matrix is mostly unwritten, and the summary is stale

[`implementation-record.md:1517`](docs/reference/implementation-record.md#L1517) claims tests exist for "T-01, T-02, T-03, T-07, T-08, T-09, T-12 (partially), T-15, and T-19". Grepping the tree for the IDs:

- **Actually cited in test files:** T-01, T-02, T-03, T-09, T-12, T-13, T-15, T-16, T-19, T-20
- **T-08 is listed as covered but cited in no test** — only one comment in non-test code ([`storetest.go:566`](internal/storetest/storetest.go#L566)). The behaviour *is* tested (`TestShareRangeRequestsDoNotConsumeDownloads`), but the ID is not on the test, so an auditor following the claim finds nothing
- **T-07** appears only in the narrative, never in code — though the behaviour is covered by `TestPGCollectorProtectsBlobReferencedOnlyByAVersion` / `…ByTrashedNode` / `…BehindAnInFlightDownload`
- **Never cited in any Go file:** T-04, T-05, T-06, T-10, T-11, T-14, T-17, T-18

Several of those *are* implemented and tested; they simply carry no ID, so the matrix cannot be audited mechanically. Notably:

- **T-10** (drop box cannot read others' files) → `TestRequestShareIsUploadOnly` exists
- **T-14** (compression bomb) → 10 tests in `internal/thumbnail`, including `TestRejectsOversizedImage` against a real 10000×10000 PNG that is small on disk
- **T-04/T-05/T-06** → `TestFailedChunkDoesNotAdvanceOffset`, `TestCommitRejectsADeletedTarget`, `internal/server/crashsafety_test.go`

Because the requirement traceability ratchet covers `ID-xx`, `INV-xx` and `SHARE-xx` but **not `T-xx`**, nothing forces these to stay linked. Extending `traceability_test.go` to the T-matrix is the highest-leverage documentation change available: it would turn nine "unknown" rows into checked ones and prevent the drift above.

One citation is actively wrong: [`forwarded_test.go:3`](internal/server/forwarded_test.go#L3) claims T-01/T-13 for a test about X-Forwarded-For and break-glass CIDRs. T-13 is *"S3 multipart upload unfinished and process exits"*. It is a §9.4 test; the ID is a typo that inflates apparent T-coverage.

---

## 4. Genuine capability gaps that remain

These survive contact with the code — the list is short, which is the finding.

| Gap | Status | Requirement |
| --- | --- | --- |
| **Shared WebDAV locks** | `lockRequest.Scope.Shared` is parsed and then **ignored**. A `LOCK` requesting `<shared>` returns `200` with an `<exclusive/>` lock in the body — verified by probe. No code reads `req.Scope` ([`lock.go:22`](internal/dav/lock.go#L22)) | §5.2 |
| **REPORT / sync-collection** | Explicit `501` ([`dav.go:141`](internal/dav/dav.go#L141)) — correctly *not* faked as an empty multistatus | §5.2, §12.3 |
| **Team-owned spaces** | Absent; shares and grants are always personal | SHARE-11 (P2) |
| **IdP event source / SCIM** | Demotion applies at next login; no poll, no push | ID-06/ID-09 |
| **Resumable uploads across restart** | `map[string]*Session` in memory ([`upload.go:91`](internal/upload/upload.go#L91)); restart abandons them | §6.1 |
| **App Password scopes** | Only `ro` / `rw`. No `webdav` or `automation` scope, and no separate "not usable for admin" bit | §3.2, D-03 |
| **Preview process isolation** | In-process decode with 50 MP / concurrency 4 / 10 s limits | §6.3 |
| **Real S3 / real DAV clients** | Fake + moto only; T-18 unrun | §5.2, D-12 |

The shared-lock case deserves a note. The struct models both scopes, which reads like intent to support them; the handler never branches on it. The response is not merely "a lock" — it is a well-formed `activelock` that explicitly declares `<lockscope><exclusive/></lockscope>`, so a careful client can notice. A careless one cannot: RFC 4918 says a server that does not support shared locks should refuse rather than substitute a different scope. Silently upgrading is worse than refusing, because the client believes it holds a shared lock that another member of the same document group can legitimately take.

Two smaller items worth noting while in this area: `--days 0` and an unset `audit.retention_days` make `audit prune` refuse rather than guess (correct), but nothing schedules it — so the "append-only and grows monotonically" framing is right about the *default* and wrong about the *capability*.

---

## 5. Where the documents undersell the work

Worth stating, because the gap list reads more pessimistically than the code justifies:

- **§4.3 retained usage** is fully split — logical quota, trash, pending uploads and retained versions each have their own cap, and the pending-upload and version caps are runtime-overridable. That is D-06 answered, and only `max_trash_bytes` is documented.
- **Audit integrity** is stronger than claimed: signed checkpoints (§10.2), a `prune_marks` boundary that keeps verification meaningful after pruning, and `audit verify` verifying *exported CSV/JSON against the original records* — which §10.2 also asks for.
- **The ratchet tests** (`traceability_test.go`, `vocabulary_ratchet_test.go`, `configkey_ratchet_test.go`, `gapstaleness_test.go`) are a genuine asset. `vocabulary_ratchet_test.go` catches the exact "declared but never emitted" class of audit dishonesty that the requirements warn about.

---

## Recommended order

1. **Fix `thumbnail.go:68`** → `PermDownload`, add the `finegrained_test` case. Security, one line, verified.
2. **Correct the six stale rows** in `limitations.md` / `implementation-record.md` / `configuration.md`, and add `export`, `audit prune` and the six undocumented keys. A gap list that overstates what is missing is as damaging as one that overstates what is done.
3. **Extend `traceability_test.go` to `T-xx`** so the §14 matrix stops rotting. This is what prevents item 2 from recurring.
4. **Refuse shared locks explicitly** (403 + `supportedlock`) instead of silently upgrading to exclusive.
5. Everything else — team spaces, SCIM, durable upload sessions, process-isolated previews — is correctly deferred and does not block a P0 claim.

**On readiness:** the P0 set is in materially better shape than the gap table admits, and the one verified defect is a one-line authorization inconsistency rather than a design failure. Production Ready is blocked by evidence, not by code: real S3, real DAV clients, and a restore drill at a realistic size are the three that matter.

---

## Implementation status (updated 2026-10-23)

The items below were implemented after this review, each with a test that fails without the change.
`go build ./...`, `go vet ./...` and `go test ./...` pass (24 packages, in-memory store).

| Item | Status | Where | Test |
| --- | --- | --- | --- |
| Thumbnail read-vs-download | **Open** — see note below | [`thumbnail.go:68`](internal/server/thumbnail.go#L68) | — |
| Content hostname separation (WC-GAP-005) | **Done** — `usercontent_url` must use a different *hostname*; same port/scheme is refused. `allow_same_host_content` is the explicit opt-out and logs a warning on every boot. | [`config.go`](internal/config/config.go) | `TestContentOriginMustUseADifferentHostname`, `TestSameHostContentIsPermittedOnlyByOptIn` |
| Content ticket revocation (WC-GAP-006) | **Done** — the account status is rechecked on every content fetch, so disabling an account stops an outstanding ticket immediately instead of after its 5-minute life. | [`content.go`](internal/server/content.go) | `TestATicketStopsWorkingWhenTheAccountIsDisabled` |
| Thumbnail decoder isolation (WC-GAP-004) | **Done** — decoding runs in a re-executed child by default; the deadline kills it. Input/output size-capped, minimal environment. The server is asserted to *build* an isolating decoder, not just to have the capability. | [`isolate.go`](internal/thumbnail/isolate.go), [`server.go`](internal/server/server.go) | `TestAHangingDecodeIsKilledAndTheCallReturns` (asserts the pid is gone), `TestTheIsolatedDecoderProducesTheSameThumbnail`, `TestTheServerIsolatesThumbnailsByDefault` |
| Durable upload sessions (WC-GAP-002) | **Done** — a JSON record beside each staging file, fsynced after the bytes; startup recovers and truncates to the acknowledged offset. | [`upload.go`](internal/upload/upload.go) | `TestASessionResumesAfterARestart`, `TestRecoveryTruncatesBytesTheRecordDoesNotAcknowledge` |
| S3 post-upload verification (WC-GAP-003) | **Done** — optional read-back re-hashes the stored object and deletes a mismatched one so the address is not poisoned. Wired into **both** S3 construction sites. | [`s3.go`](internal/blob/s3/s3.go), [`main.go`](cmd/newcloud/main.go) | `TestPutRefusesAnObjectThatDoesNotMatchItsAddress`, `TestACorruptedObjectIsRemovedSoTheAddressIsNotPoisoned`, `TestEveryS3ConfigFieldReachesBothStores` |
| Bounded IdP deprovisioning (WC-GAP-001) | **Done** — `auth.revalidate_minutes` refuses sessions and App Passwords past the confirmation window, on **both** the session and WebDAV paths. `0` disables and warns. | [`middleware.go`](internal/server/middleware.go), [`dav.go`](internal/dav/dav.go), [`0015_idp_revalidation.sql`](migrations/0015_idp_revalidation.sql) | `TestAStaleIdPAccountIsRefused`, `TestBreakglassIsExemptFromTheWindow`, `TestAStaleCredentialCannotDelete` (WebDAV DELETE), `TestThePredicateSeesTheAccountBeingChecked` |
| COPY partial destination (WC-GAP-009) | **Done** — a failed directory COPY trashes what it created (never a pre-existing destination). | [`move.go`](internal/dav/move.go) | `TestCopyDirectoryFailureRollsBackThePartialDestination`, `TestTheRollbackIsLoadBearing` |
| Shared lock scope | **Done** — a `<shared>` LOCK is refused with `403` + `supportedlock` instead of returning an exclusive lock. | [`lock.go`](internal/dav/lock.go) | `TestSharedLockScopeIsRefusedNotDowngraded` |
| Documentation drift (WC-GAP-011) | **Done** — export, `audit prune`, chunk enforcement, retention, the retained-usage caps, `trusted_proxies` and the proxy/annotation keys are documented; CLI help lists `export` and `audit prune`. | `docs/`, `deploy/config.example.yaml` | `TestLimitationsPageMatchesTheCode`, `TestGapTableDoesNotClaimClosedGapsAreOpen` |

### Still open

- **Thumbnail `PermRead` vs `PermDownload`.** Left as-is deliberately: the owner's grant model treats
  `read` as "may see the file", and a thumbnail is a rendering of it. Changing it to `PermDownload`
  would make a `read`-only grant unable to show a preview, which may be the wrong product decision.
  It is listed here rather than silently fixed because the code comment and the check still disagree,
  and that is the thing to resolve first.
- **WC-GAP-007 / WC-GAP-008 are evidence gaps, not code gaps.** They need a real PostgreSQL 17
  instance (`TEST_DATABASE_URL`), a real S3 endpoint (`TEST_S3_ENDPOINT`), real DAV clients and a
  restore drill at deployment scale. No amount of code closes them, and this environment has none of
  those: no Docker, no external database. The commands are in the gap review's §9 and remain unrun.
- **IdP event feed.** The window bounds the exposure; it does not revoke at the moment of the change.
  SCIM or a group poll is the real fix.
- **Cross-node upload resume.** Sessions are durable but local to one staging directory.

### Defects found while implementing this work

Recorded because they are the class of problem the review is about, and both were live before it:

1. **A second S3 construction site was silently misconfigured.** `openBlobStore` (used by the blob-size
   backfill) omitted `SSEMode` and `SSEKMSKeyID`, so the backfill path wrote **unencrypted** objects
   while the serving path encrypted them — no error, no log line. Adding the verification fields to
   `serve` alone would have repeated it. Fixed, and `TestEveryS3ConfigFieldReachesBothStores` now
   parses both literals and requires their key sets to agree.
2. **The memory backend never stamped `idp_confirmed_at`.** `UpsertIdPUser` in `identity_memory.go`
   refreshed role and email but not the confirmation, while PostgreSQL did. Every account on the
   in-memory backend would have looked permanently stale, so with a window configured the shared
   conformance suite would diverge by backend. Found by the new server-level test, fixed on both the
   by-identity and by-username paths, and covered by a new `storetest` case.
3. **`<shared>` LOCK was silently downgraded.** The request struct modelled both scopes and nothing
   branched on it, so `200` returned an `<exclusive/>` lock to a client that asked for a shared one.

### Verification performed

- `go build ./...`, `go vet ./...`, `gofmt -l cmd internal tools` — clean.
- `go test ./... -count=1` — 24 packages pass (no Docker/PG here, so `TEST_DATABASE_URL` suites skip).
- **End-to-end against the real binary**: `newcloud` re-executed as its own thumbnail worker decoded a
  1000×400 PNG to 320×128 (exit 0), refused a 100 MP image with exit 11, and worked with
  `NEWCLOUD_CONFIG` pointing at a nonexistent file — proving the worker entry point runs before any
  configuration load.
- **Mutation-tested** every guard: the isolation wiring, the WebDAV revalidation check, the S3
  verification read-back, the upload recovery, the content-ticket status check, the COPY rollback, the
  content-hostname rule (all five cases including the trailing-dot form), and the shared-lock refusal
  each turn a named test red when removed. None of these tests can pass against the unfixed code.
