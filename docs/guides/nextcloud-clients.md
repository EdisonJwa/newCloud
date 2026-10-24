# Nextcloud desktop and mobile clients

newCloud is not a Nextcloud server, but it implements the small part of Nextcloud's client protocol
that the official desktop and mobile clients use to find a server, sign in, and sync. This page is
what an operator or a user needs to connect one, and what does not work.

The design and its boundaries are in [ADR-0015](../adr/0015-nextcloud-client-compatibility.md).

## Connecting a client

### Desktop client

1. Install the Nextcloud desktop client and choose **Add account**.
2. Enter the deployment's public URL, for example `https://cloud.example.com`. The client may append
   `/nextcloud` to what you type; it is using that as a folder name, not as a path — the server URL is
   the origin alone.
3. The client opens your default browser at `/login/v2/flow/<token>`. Sign in there with the normal
   OIDC flow.
4. The browser shows an approval page naming the client (for example *Nextcloud Desktop Client/3.13*)
   and the account. Press **Authorize**.
5. Close the browser window. The client polls the server, receives an App Password, and asks where to
   sync.

The password never reaches the client. The credential it stores is an ordinary App Password, listed
under **Account → App passwords** with the client's name, revocable on its own, and expired by the
same IdP revalidation window as every other App Password. Removing the account in the client revokes
it (`DELETE /ocs/v2.php/core/apppassword`), so the credential does not outlive the configuration.

### Mobile client

The Android and iOS clients use the same exchange. Enter the server URL, tap **Log in**, complete the
browser sign-in, and return to the app. If your deployment sets
[auth.revalidate_minutes](configuration.md), the credential is refused once the window passes and the
app asks you to sign in again — that is the documented bound, not a sync failure.

## What works

| Operation | Notes |
| --- | --- |
| Initial sync and folder listing | PROPFIND with `Depth: 0` and `Depth: 1`. |
| Upload and download | `PUT`/`GET`, Range requests, ETags, conditional writes. |
| Rename, move, delete, create folder | Standard DAV methods, with this product's trash and versions. |
| Modification times | `X-OC-Mtime` is stored, so a file does not come back as "modified" after every upload. |
| Conflict handling | The client's own conflict files (`conflicted copy`) work; the server's arbitration is ETag-based (`412` when a write loses a race). |
| Read-only folders | A folder shared through a resource ACL without write permission refuses writes and the client reports it. It is not signalled through `oc:permissions` (see below), so the client may discover it only when the upload is refused. |

## What does not work, and how the client behaves

These are deliberate boundaries. A client that asks for one of them is told "no" through the
capabilities document and uses its fallback, rather than failing halfway through an operation.

| Missing | Consequence |
| --- | --- |
| Nextcloud chunked uploads (v1 and v2) | Large files are uploaded in one request. The ceiling is `limits.max_file_bytes` (1 TiB by default), so it is not the limit that matters in practice — losing the connection at 90% is, because the file starts again. |
| `REPORT sync-collection` | Incremental sync is not available. Clients do a full PROPFIND walk, which is correct but slower on a very large tree. Favorites lists (which use `REPORT`) are not served. |
| `OC-Checksum` | Checksums are not stored or verified by the server; clients do not send them because the capability is absent. |
| `oc:permissions` | Not reported. The desktop client treats a missing permission set as "no restriction", so a read-only share is enforced on the write path rather than in the client's UI. |
| Nextcloud sharing APIs | The client's sharing tab will not manage shares; use the newCloud web UI. Links created there are ordinary share URLs and open in a browser. |
| `/public.php/dav` | Public links are served by this product's share pages, not by Nextcloud's public DAV endpoint. |
| Office/OnlyOffice integration, Talk, calendars | Not part of this product. |

## Troubleshooting

**The client says the server is unsupported.** Check that the URL reaches `/status.php` and that a
reverse proxy passes it through unchanged. A proxy that rewrites `/status.php` to the single-page app,
or serves it with a captcha or authentication in front, makes the client conclude this is not a
compatible server. `curl https://cloud.example.com/status.php` must return JSON with
`"installed":true`.

**The browser opens but the approval page says the link is invalid or has expired.** Flows live for
twenty minutes and are single-use. Start the account setup again from the client.

**The approval page says the link was already approved by a different account.** The browser was
signed in as somebody else when the link was opened. Start again and sign in as the account that will
own the sync.

**The client keeps asking for a password, or shows 401.** App Passwords are refused once
`auth.revalidate_minutes` has passed without an interactive sign-in, and when the account is disabled.
Sign in through the browser again. The audit log records the credential creation
(`auth.apppassword.create`) with `"via":"nextcloud-login-flow-v2"` and the client's name.

**Uploads fail at a fixed size.** That is `limits.max_file_bytes`; the client is sending one request
per file because chunking is not advertised. Raise the limit and restart the instance.

**Sync is slow on a large account.** The client walks the tree with PROPFIND on every sync because
incremental sync (`REPORT sync-collection`) is not implemented. Keep the number of top-level folders
and files per folder reasonable; the benchmarks in
[benchmarks.md](../reference/benchmarks.md) describe directory-listing cost.

## Operator notes

- The compatibility endpoints are always on; there is no configuration key. `status.php` is anonymous
  by design, and it exposes the protocol version and product name — nothing about accounts, files, or
  the deployment's size.
- The only anonymous write into the database is a login-flow row. It expires after twenty minutes,
  expired rows are swept on every new start, and it cannot produce a credential without an approved
  browser session.
- Issuing a credential this way is audited with the same event as the web UI
  (`auth.apppassword.create`), so the activity list answers "where did this device come from?".
- `newcloud audit` and the App Passwords screen show the client's name as the label. Revoking the
  credential logs the client out at its next request.
