# ADR-0015: Nextcloud client compatibility

**Status:** Accepted · **Date:** 2026-10-25

The official Nextcloud desktop and mobile clients can sign in to newCloud and sync against it. That
needs more than RFC 4918: four endpoints and two path shapes are part of the clients' bootstrap, and a
server that omits them is invisible no matter how correct its WebDAV is. This ADR records the narrow
compatibility layer that was added, what it deliberately does not emulate, and the security properties
that had to be preserved while adding an anonymous path that can end in a working credential.

## Problem

`/dav/<user>/` is a standards-compliant WebDAV endpoint, and a client that can be pointed at an
arbitrary URL will use it. The Nextcloud clients cannot be pointed at one. Before they sync anything
they:

1. probe `/status.php` and refuse a server whose version string they cannot parse;
2. read capabilities from `/ocs/v1.php/cloud/capabilities` (or `v2`) and refuse to proceed without
   the OCS envelope;
3. authenticate through **login flow v2** — an anonymous `POST /index.php/login/v2`, a browser
   approval, and a poll that returns an App Password — because they are not designed to collect a
   password themselves;
4. sync against `/remote.php/dav/files/<user>/`, not `/dav/<user>/`.

Before this change the limitations page said, accurately, that there was no compatibility mode. The
cost of that honesty was that the clients the product exists to interoperate with could not connect at
all.

## Decision

Add a compatibility layer, not an emulation. Every endpoint below is implemented; nothing else is
advertised, and the capabilities document says what is missing so clients choose their fallback path
instead of failing halfway through an operation.

| Surface | Implemented in | Notes |
| --- | --- | --- |
| `GET /status.php` | `internal/nccompat` | Anonymous. Reports productname `newCloud` and protocol version `33.0.0`. |
| `GET /ocs/{v1,v2}.php/cloud/capabilities` | `internal/nccompat` | XML by default, JSON with `?format=json`; requires `OCS-APIRequest: true`. |
| `GET /ocs/{v1,v2}.php/cloud/user` | `internal/nccompat` | Returns the namespace segment as `id`, display name, email, quota. |
| `DELETE /ocs/{v1,v2}.php/core/apppassword` | `internal/nccompat` | Revokes the credential the request authenticated with, so removing the account stops the client's access. |
| `POST /index.php/login/v2` | `internal/nccompat` | Starts the flow; returns the poll token and browser URL. |
| `GET/POST /login/v2/flow/{token}` | `internal/nccompat` | Consent page, authenticated by the ordinary OIDC session. |
| `POST /login/v2/poll` | `internal/nccompat` | 404 until approved; one 200 with the credential; 404 afterwards. |
| `/remote.php/dav/files/{user}/...` | `internal/dav` | Same handler and authorization as `/dav`; hrefs stay on the requested path. |
| `/remote.php/webdav/...` | `internal/dav` | Legacy shape; the account comes from the credential. |
| `X-OC-Mtime` on `PUT` | `internal/dav` | Stored as the node's modification time; `X-OC-MTime: accepted` replies. |

### Why the paths are parsed, not rewritten

A rewrite from `/remote.php/dav/files/<user>/x` to `/dav/<user>/x` is one line and wrong. The DAV
handler builds the `href` of every PROPFIND entry from the request's own escaped path; after a rewrite
the client would receive `/dav/...` hrefs while it asked for `/remote.php/dav/...`. Its discovery code
strips the prefix it knows and then matches relative paths — an href outside that prefix is not a
file in the folder it is listing, and the client reports an invalid response rather than syncing.
`splitPath` therefore understands all three shapes, and `resourceHref` continues to derive the answer
from the request.

The same parsing change closed a latent mismatch: the comment on `splitPath` had claimed since the
beginning that the URL's user segment was checked against the authenticated actor, while the code
discarded it. The bytes were always resolved in the caller's own namespace, so it was not a
cross-account read — but a PUT to `/remote.php/dav/files/someone-else/file` would have written the
caller's own file and reported success against another account's name. A URL that names a different
namespace is now a `404`, in `resolvePath`, `resolveDestination`, `PUT` and `MKCOL` alike.

PROPFIND and LOCK answers now carry **absolute paths** (`/remote.php/dav/files/alice/x`) instead of
full URLs. The client's discovery parser compares every `href` with the path of the request URL and
rejects the whole listing when one does not start with it; a `https://host` prefix fails that test.
It is also the sturdier form behind a reverse proxy, where the configured public URL and the `Host`
the client used can differ. Nextcloud's own server answers the same way.

### A listing is one transaction, not one per entry

The clients ask for about twenty non-DAV properties on every entry. The handler used to answer each
entry by opening its own transaction for dead properties, which turned a 500-entry folder into 500
round trips before the first byte of the response. The listing now reads its children first and the
dead properties of the whole listing in a single query (`DeadPropsFor`, `= ANY($1::uuid[])` on
PostgreSQL), then streams. A failed child read can also now be a proper error response instead of a
status entry appended to a half-written multistatus.

### The two tokens of login flow v2

The client receives two independent secrets from `POST /index.php/login/v2`:

- the **poll token**, which it keeps and later presents to collect the credential;
- the **login token**, which becomes the URL it opens in the user's browser.

They are separate on purpose. If the browser URL carried the poll token, a browser-history sync, a
shared screen, or an extension would be able to redeem the credential the user was approving for a
different application. Only the SHA-256 of either token is stored, for the same reason App Password
secrets are hashed: a metadata dump must not yield a usable capability.

### The credential is minted at the poll, not at the approval

Approval only records the account. The App Password is created by the first successful poll, in a
request that also writes its audit event (`IssueAudited`). Creating it at approval time would mean
holding a usable secret for a client that may never finish the flow, and storing it would defeat the
reason App Passwords are stored as hashes.

The poll deletes the flow row (`DELETE … RETURNING`), so a duplicated poll cannot mint a second
credential. The approval is a conditional `UPDATE` that only succeeds while the flow is unapproved, so
a second account following the same browser link gets `409` instead of a credential for the wrong
user. Both predicates are in the statement that changes the row, not in a read-then-write pair in the
handler.

Approval is a POST from the consent page. The session cookie is `SameSite=Lax`, so a cross-site form
cannot carry it, and an `Origin` header naming another host is refused explicitly rather than relied
on the cookie jar alone.

The one failure mode this ordering leaves is a database error between consuming the flow and creating
the credential: the client cannot poll again and must restart from the browser. That is logged at
`error` because it is otherwise invisible, and the alternative — issuing first and compensating on a
lost race — can leave two live credentials instead of none.

### What is deliberately absent

The capabilities response is a promise. These are not promised, and the client degrades rather than
failing:

| Not implemented | Client behaviour |
| --- | --- |
| WebDAV chunking v1 and v2 (`files.bigfilechunking`, `dav.chunking`) | `false` / omitted, so large files are sent as one PUT. The ceiling is `limits.max_file_bytes`, and a deployment that syncs multi-GB files must raise it. |
| `REPORT sync-collection` | `501`, unchanged from ADR-0002. The desktop client discovers with PROPFIND. |
| `OC-Checksum` storage and verification | The header is ignored and checksums are not advertised, so clients do not send them. |
| `oc:permissions` on PROPFIND | Omitted. The desktop client treats a missing permission set as "no restriction"; ACL-granted read-only folders are still refused on the write path. |
| OCS sharing, user management, theming apps | Not offered. The web UI is the sharing surface. |
| `/public.php/dav` | Public links are served by this product's own share pages, not by Nextcloud's public DAV endpoint. |
| Browser session on the OCS endpoints | OCS authenticates with an App Password only. The project's rule that WebDAV never accepts a session cookie extends to the API surface that exists to serve WebDAV clients. |

### Reported version

`status.php` and the capabilities document report protocol version **33.0.0**. This is a protocol
version, not a claim about the software: every client compares it with the oldest server it supports
and refuses anything lower. The features actually offered are the ones in the capabilities document,
which is where clients are specified to look.

## Consequences

- Nextcloud desktop and mobile clients can be pointed at a newCloud URL, sign in with SSO in the
  browser, and sync. No password is typed into the client and no plaintext credential is stored.
- Every issued credential is a normal App Password: listed in the UI with the client's name,
  revocable individually, subject to the same IdP revalidation window, and killable by disabling the
  account.
- A deployment that wants large-file sync must raise `limits.max_file_bytes`; there is no resumable
  Nextcloud upload path.
- The anonymous start endpoint is bounded by its twenty-minute TTL and by sweeping expired rows on
  every start, not by a per-IP rate limit. An attacker can create rows but cannot mint a credential:
  minting requires a browser session and an explicit approval.

## Validation

| Property | Test |
| --- | --- |
| status.php is anonymous and carries the protocol version | `TestNextcloudStatusIsAnonymousAndCarriesTheProtocolVersion` |
| OCS requires the CSRF header and a credential | `TestNextcloudOCSRequiresTheCSRFHeaderAndACredential` |
| Capabilities advertise no chunking and use the right envelope per version | `TestNextcloudCapabilitiesSayOnlyWhatIsImplemented` |
| The `id` is the DAV namespace segment; quota reports -3 for unlimited | `TestNextcloudOCSUserNamesTheNamespaceAndReportsQuota` |
| Removing the account revokes its credential | `TestNextcloudAccountRemovalRevokesTheCredential` |
| The whole handshake issues a credential that works over WebDAV | `TestNextcloudLoginFlowV2IssuesAWorkingCredential` |
| A second account cannot take over an approved flow | `TestNextcloudLoginFlowCannotBeTakenOverByAnotherAccount` |
| An abandoned flow creates no credential | `TestAnAbandonedLoginFlowCreatesNoCredential` |
| Approval, single use, expiry and the sweep on both store backends | `TestConformance/LoginFlows` |
| Nextcloud paths answer with their own hrefs | `TestNextcloudFilesPathAnswersWithItsOwnHrefs` |
| The legacy path reads the same tree | `TestNextcloudLegacyWebdavPathAndFilesPathAreTheSameTree` |
| A URL naming another account is refused | `TestNextcloudPathNamingAnotherAccountIsRefused` |
| Unknown DAV subtrees do not become relative names | `TestNextcloudPathRoutesUnknownDavSubtreesToNotFound` |
| `X-OC-Mtime` is stored, accepted, and ignored when malformed | `TestNextcloudUploadHonoursXOCMtime` |
| MOVE accepts a Nextcloud Destination and refuses a foreign namespace | `TestNextcloudMoveAcceptsANextcloudDestination` |
| A listing reads dead properties in one query on both backends | `TestConformance/DeadProps/BulkReadAgreesWithPerNodeRead` |

Not verified: an actual Nextcloud desktop or mobile build has not been run against this tree in this
environment. The endpoint shapes and the handshake are pinned by the tests above; client-version
behaviour is inferred from the protocol documentation and remains on the T-18 list in the
implementation record.
