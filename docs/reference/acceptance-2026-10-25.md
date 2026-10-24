# Acceptance record — 2026-10-25

What this document is: the evidence that the project is finished, for the parts that can be finished
here. Each check names the command that produces it, so a reader can re-run any of it rather than
trusting prose. Anything not verified is listed as such at the end.

The review that drove the work is [review-2026-10-24.md](review-2026-10-24.md); its defect register is
the source of truth for "fixed", and this record is the source of truth for "verified".

## Verified

| Check | Command | Result |
| --- | --- | --- |
| Full suite, both store backends | `TEST_DATABASE_URL=... go test ./... -count=1 -p 1` | pass |
| Full suite under the race detector | `TEST_DATABASE_URL=... go test -race -count=1 -p 1 ./...` | pass, no data races |
| WebDAV phase-0 reproductions | `go test -tags phase0red ./internal/dav -run TestPhase0 -v` | 5 of 8 pass (`TreePutStaleIfMatch`, `TreeCopyDoesNotOverwrite`, `DeleteReleasesTheTreeLivePath`, `FailedCopyDoesNotUndoALock`, `ADeleteDoesNotMoveAConcurrentWritersBytes`); the other 3 are the layout boundary below |
| Binary smoke (migrate twice, stamped version, schema complete) | `go test ./cmd/newcloud -run 'TestTheShippedBinaryMigratesAndReportsItsVersion'` | pass |
| Release pipeline, six platforms, SBOM, checksums | `make release VERSION=v9.9.9-acceptance` | six binaries + `sbom.cdx.json` + `SHA256SUMS`; the Windows binary answers `newcloud v9.9.9-acceptance` |
| Path validators and the Content-Range parser, fuzzed | `make fuzz` | 25M, 5.8M and 4M executions: no counterexample, no panic. These are the parsers between user input and the filesystem, and none of the three had a fuzz target before this record |
| Audit chain, including across the hash-version change | `newcloud audit verify`; `TestBothVerifiersAgreeOnAChainSpanningTheHashVersion` | both verification paths agree and accept a mixed chain |
| Group spaces end to end | `TestGroupSpacesAreCreatedDiscoveredAndUsed` | a manager creates a space, a viewer discovers and opens it by id, an outsider sees nothing |
| Delete journal and recovery | `TestADeleteRecordsAndClosesItsJournalEntry`, `TestRecoveryResolvesAFinishedDeleteAndEscalatesALiveOne` | every delete records and closes an operation; recovery resolves finished ones and escalates the rest |

## Bounded by design

These are not defects waiting to be fixed; they are properties of a chosen design, documented so they
are not rediscovered as surprises.

- **The tree layout's three red reproductions.** They simulate bytes moved outside the store. On a
  layout whose content lives at its path, a reader that was not told about the move cannot follow it,
  and no journalling changes that. The product's answer is to detect and report it: `500
  content_missing` (never `404`), a counter, a `critical` alert, and a runbook section. The blob
  layout does not have the problem because content is addressed by hash.
- **Group spaces are creator-bound.** A space's root lives under the account that created it, because
  one root per owner is the schema's invariant. Deleting that account takes the space with it.
- **Deleting a group turns its spaces into ordinary folders.** `ON DELETE SET NULL`; the files
  survive and stop being discoverable through membership. No administrative action destroys data.

## Blocked, with reasons

| Item | Why it cannot be finished here |
| --- | --- |
| LDAP / AD login | needs a dependency that cannot be fetched in this environment; writing an untested integration would be worse than documenting the gap |
| OpenAPI drift (M7) | `web/openapi.yaml` is being rewritten by the UI session; editing it here would collide |
| Helm chart | no `helm` binary to validate templates with; a chart that has never been templated is not a deliverable |

## What is not verified

- **A chain written by a previous build.** The audit upgrade test seals its version-1 row at runtime
  with the current code, so it proves the two readers agree, not that an old writer's bytes verify.
  See limitations.md.
- **The three blocked items above.** No claim is made about them.

## Re-running everything

```sh
TEST_DATABASE_URL=postgres://test@127.0.0.1:15433/newcloud?sslmode=disable \
  go test ./... -count=1 -p 1
TEST_DATABASE_URL=... go test -race -count=1 -p 1 ./...
go test -tags phase0red ./internal/dav -run TestPhase0 -v
make release VERSION=v0.0.0-check
```