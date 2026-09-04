# newCloud documentation

This directory is the documentation hub for newCloud. It is organized by task, not by when a
document was written.

## New to newCloud

| Document | What it answers |
| --- | --- |
| [Quick start](guides/quickstart.md) | Running locally and signing in |
| [Architecture](reference/architecture.md) | Components and request flow |
| [Configuration](guides/configuration.md) | Each configuration key and its default |
| [Deployment](guides/deployment.md) | A production deployment that does not lose data |
| [Limitations](guides/limitations.md) | What is deliberately not implemented |

## Operating a deployment

| Document | What it covers |
| --- | --- |
| [Operations](guides/operations.md) | Health checks, metrics, alerts, audit verification, backups, key rotation |
| [Restore drill runbook](../deploy/backup/restore-drill.md) | Backup order, restore order, PITR, and the drill record |
| [Audit failure policy](../deploy/audit-failure-policy.md) | Why audited writes fail closed, and what clients see |
| [OIDC key rotation](../deploy/oidc-key-rotation.md) | Discovery, rotation, and the nine verification-failure outcomes |
| [Alert runbook](../deploy/monitoring/runbook.md) | What each alert means and the first three things to check |

## Reference

| Document | What it covers |
| --- | --- |
| [Functional requirements](reference/functional-requirements.md) | The requirements baseline and the gap analysis that drove the build |
| [Implementation record](reference/implementation-record.md) | Which contracts are implemented, which tests pin them, and what remains open |
| [Interface design](reference/interface-design.md) | The Atlas direction: layout, tokens, components, and interaction rules |
| [Interface redesign: Atrium](reference/interface-atrium.md) | Why the first two directions failed, and the structure that replaced them |
| [Browser-only failures](reference/browser-only-failures.md) | Two defects that only appeared in a real browser, and the guards added afterward |

## Decision records

ADRs are immutable once accepted. A change of decision is a new ADR that supersedes the old one;
the old one stays in the repository with its original date.

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](adr/0001-scope-and-language.md) | Product scope, non-goals, and Go as the implementation language | Accepted |
| [0002](adr/0002-backend-skeleton.md) | Backend skeleton, the two credential systems, and WebDAV implementation strategy | Accepted |
| [0003](adr/0003-storage-layout.md) | Optional `blob` and `tree` storage layouts behind one interface | Accepted |
| [0004](adr/0004-metadata-store.md) | Metadata store contract and transaction boundaries | Accepted |
| [0007](adr/0007-s3-storage.md) | S3-compatible object storage as a second backend | Accepted |
| [0008](adr/0008-resumable-uploads.md) | Resumable chunked uploads without tus or S3 multipart | Accepted |
| [0012](adr/0012-cursor-contract.md) | Cursor pagination for directory listings | Accepted |

## Conventions

- **Documents cite tests.** Any test name in backticks must exist in `cmd/` or `internal/`.
  `TestDocumentsCiteOnlyRealTests` enforces it.
- **Status tables are current.** A gap row that claims something is missing after it has been
  implemented is a defect. `TestGapTableDoesNotClaimClosedGapsAreOpen` and
  `TestGapTableDoesNotClaimMeasuredThingsAreUnmeasured` check the gap table in the implementation
  record.
- **Links are relative.** Documentation moves as a tree; absolute repository paths break on forks
  and in rendered views.
- **Code fences carry a language.** Commands must be executable as written.
- **Numbers carry units and dates.** A measurement without a date is not evidence.
