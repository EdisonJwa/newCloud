# Configuration

newCloud reads one YAML file. The path is taken from `NEWCLOUD_CONFIG` and defaults to
`/etc/newcloud/config.yml`. The process validates the file at startup and refuses to start on an
unsafe combination; there is no "start and hope" mode.

```sh
NEWCLOUD_CONFIG=/etc/newcloud/config.yml newcloud serve
```

Values in the file are decoded onto a safe baseline, so an omitted key keeps its default rather
than becoming a zero value. A small set of keys can be overridden from the environment
(`NEWCLOUD_ADDR`, `NEWCLOUD_PUBLIC_URL`, `NEWCLOUD_USERCONTENT_URL`, `NEWCLOUD_OIDC_ISSUER`,
`NEWCLOUD_OIDC_CLIENT_ID`, `NEWCLOUD_OIDC_CLIENT_SECRET`, `NEWCLOUD_SESSION_SECRET`,
`NEWCLOUD_BREAKGLASS_USER`, `NEWCLOUD_SCIM_TOKEN`, `NEWCLOUD_STORAGE_LAYOUT`,
`NEWCLOUD_ALLOW_LOCAL_ADMIN`). The file remains the source of truth for everything else.

Start from [`deploy/config.example.yml`](../../deploy/config.example.yml), which contains the same
keys with inline rationale.

## `server`

| Key | Default | Meaning |
| --- | --- | --- |
| `addr` | `:8080` | Listen address. TLS terminates in front of the process. |
| `public_url` | required | Absolute URL of the authenticated application. Used for OIDC redirects and share links. |
| `usercontent_url` | required | Absolute URL of the content origin. Must differ from `public_url` in scheme, host, or port. |

`public_url` and `usercontent_url` are an origin split, not two deployments. Both point at the same
process; a reverse proxy gives them two public hostnames. Serving user bytes from the application
origin would let a stored-XSS payload in an uploaded file read the session cookie, so the validator
rejects it.

## `auth`

| Key | Default | Meaning |
| --- | --- | --- |
| `issuer` | required | OIDC issuer URL (Pocket ID or any compliant provider). |
| `client_id` | required | OIDC client ID. |
| `client_secret` | required unless `client_secret_file` | OIDC client secret. |
| `client_secret_file` | — | Read the secret from a file. Preferred in containers. |
| `scopes` | `[openid, profile, email, groups]` | Must include `groups`; roles are derived from group names. |
| `group_role_map` | `cloud-admins: admin`, `cloud-users: user` | Map IdP group names to `admin`, `user`, or `guest`. Unknown groups map to `guest`, never `user`. |
| `session_ttl` | `12h` | Browser session lifetime. |
| `session_secret` | required, at least 32 bytes | HMAC key for browser session tickets, share-access cookies, and email verification codes. Rotating it invalidates those; App Passwords and share links are stored as SHA-256 hashes and are unaffected. |
| `jit.mode` | `open` | First-login behavior: `open` provisions immediately, `pending` creates a disabled account for approval, `deny` refuses unknown identities. |
| `jit.allowed_domains` | `[]` | Email domains that may be provisioned at all. Empty means any domain. |
| `allow_local_admin` | `false` | Enable the break-glass local administrator. |
| `local_admin.username` | — | Break-glass username. |
| `local_admin.password_argon2_file` | — | File containing an Argon2id hash, produced by `newcloud breakglass hash`. |
| `local_admin.allowed_cidrs` | required when enabled | Source networks allowed to reach the break-glass login. |

The server refuses to start if `scopes` omits `groups`, because Pocket ID only releases group
membership when the scope is requested and every account would silently become a guest.

The break-glass account is off by default. If enabled, `allowed_cidrs` is mandatory: an
unrestricted local administrator is an internet-reachable root login. Use of the account is
audited, counted in metrics, and alerted.

### JIT provisioning and roles

| Mode | First login by an unknown identity |
| --- | --- |
| `open` | Account created and enabled immediately with the role from `group_role_map`. |
| `pending` | Account created disabled with status `pending`; an administrator approves it in the Accounts page or through `POST /api/admin/users/{id}/status`. |
| `deny` | Login refused. Accounts must be pre-created. |

A role change takes effect at the account's next login. There is no IdP event source or SCIM
endpoint, so group removal is not reflected immediately; the login path re-reads groups and revokes
existing sessions and App Passwords when the role drops.

## `storage`

Two independent choices: `backend` is where bytes physically live, `layout` is how they are
arranged.

| Key | Default | Meaning |
| --- | --- | --- |
| `backend` | `local` | `local` or `s3`. |
| `layout` | `blob` | `blob` (content-addressed) or `tree` (filesystem mirror). `tree` requires `backend: local`. |
| `local.root` | `/var/lib/newcloud/blobs` | Content root. For `blob`, the sharded hash store. For `tree`, the user-visible tree. |
| `upload_dir` | system temporary directory | Staging area for resumable uploads. Keep it off the content root and on a real disk. |
| `s3.endpoint` | required for S3 | Hostname of the S3-compatible endpoint. |
| `s3.bucket` | required for S3 | Bucket name. It must already exist; startup checks connectivity. |
| `s3.region` | — | Signing region. |
| `s3.access_key` / `s3.secret_key` | empty | Set both, or leave both empty to use the standard AWS credential chain. |
| `s3.secure` | `true` | Use HTTPS. Set `false` only on a trusted plaintext network. |
| `s3.prefix` | empty | Key prefix for a bucket shared with other applications. |

`blob` deduplicates identical content and stores a new version as a difference. `tree` mirrors the
user-visible hierarchy and is directly inspectable with `ls` and `rsync`, but every version is a
full copy. Set the layout explicitly: `blob` is the default, but the choice changes the shape of
the data on disk and should be made on purpose.

`local.root` and its `tmp/` directory must be on the same filesystem. Publishing a blob is a
`rename(2)`; across filesystems it degrades to a full copy, which at scale is an outage rather than
a slowdown. Startup performs a real rename probe and refuses to start if it fails.

## `limits`

| Key | Default | Meaning |
| --- | --- | --- |
| `max_file_bytes` | `1099511627776` (1 TiB) | Hard cap for one file. |
| `default_file_quota_bytes` | `10737418240` (10 GiB) | Per-user quota for new accounts. |
| `max_propfind_children` | `100000` | Maximum entries returned for one `PROPFIND`. |
| `upload_chunk_bytes` | `268435456` (256 MiB) | Advertised chunk size for resumable uploads. Validated at startup and reported to clients, but a chunk larger than this is not refused; the bound that is enforced is `max_file_bytes`. |
| `blob_scrub_per_pass` | `0` | Blobs read back and verified per scrub cycle. `0` disables the scrub. |
| `blob_scrub_interval_seconds` | `0` | Scrub period. `0` uses the worker's six-hour sweep cadence. |
| `trash_retention_days` | `0` | How long a deleted item stays restorable. `0` keeps it forever. |
| `version_retention_days` | `0` | How long an old version is kept. `0` keeps it forever. |
| `version_count_limit` | `0` | Per-file version cap. `0` uses the layout default: 10 for `blob`, 3 for `tree`. |
| `upload_session_ttl_hours` | `24` | How long an idle resumable upload may sit before it is swept. |

Retention is the one place where the safe default is "keep everything". A deployment that has not
decided a policy must not silently delete recoverable data. When both retention values are `0`,
startup logs a warning, because "retention configured" and "retention does nothing" look identical
from outside until the disk fills.

The integrity scrub is opt-in because its cost is real: each pass reads stored blobs in full. A few
hundred blobs per cycle covers a multi-terabyte store over days. Without it, a blob corrupted in
place is discovered by a user, or by a restore drill months later.

## `audit`

| Key | Default | Meaning |
| --- | --- | --- |
| `retention_days` | `365` | **Not applied today.** Intended as how long audit events are kept; the audit table is append-only and nothing prunes it, so it grows without bound. Setting this changes nothing. See [Limitations](limitations.md). |
| `anchor_dir` | empty | Directory for daily chain-head checkpoints. Empty disables anchoring and logs a warning. |

`anchor_dir` must be writable by the application but not rewritable with the database credentials.
A WORM bucket, an append-only volume, or a mounted share the application user cannot modify is
appropriate. A directory on the metadata volume makes the anchor decorative: an attacker who can
rewrite the database can rewrite the anchor too.

## `mail`

Mail is required for email shares. Link shares and drop-box links work without it.

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Enable SMTP delivery. |
| `host` | required when enabled | SMTP host. |
| `port` | `587` | SMTP port. |
| `username` | — | SMTP username. |
| `password` / `password_file` | — | SMTP password. The file form keeps the secret out of `docker inspect`. |
| `from` | required when enabled | Envelope sender. |
| `start_tls` | `true` | STARTTLS on the submission port. |
| `implicit_tls` | `false` | Implicit TLS (port 465). Mutually exclusive with `start_tls`. |
| `insecure_skip_verify` | `false` | Skip certificate verification. Only for a private relay with a self-signed certificate; startup warns. |

A half-configured relay is rejected at startup. Accepting email invitations and silently failing
to deliver every one of them is worse than refusing to start.

## Validation summary

The process exits with an invalid-configuration error when:

- `server.public_url` or `server.usercontent_url` is missing, not absolute, or the two share an
  origin;
- `auth.issuer`, `auth.client_id`, or the client secret is missing;
- `auth.scopes` does not include `groups`;
- `auth.session_secret` is shorter than 32 bytes;
- `auth.jit.mode` is not `open`, `pending`, or `deny`;
- `auth.allow_local_admin` is true without a username, password file, and allowed CIDRs;
- `limits.max_file_bytes`, `upload_chunk_bytes`, or `max_propfind_children` is not positive;
- `storage.backend` is not `local` or `s3`;
- `storage.layout` is `tree` with `backend: s3`;
- an S3 bucket or endpoint is missing, or only one of the access key and secret is set;
- mail is enabled without a host or sender, or both TLS modes are selected.

## Example

```yaml
server:
  public_url: "https://cloud.example.com"
  usercontent_url: "https://usercontent.example.com"

auth:
  issuer: "https://id.example.com"
  client_id: "newcloud"
  client_secret_file: "/run/secrets/oidc_client_secret"
  scopes: ["openid", "profile", "email", "groups"]
  group_role_map:
    "cloud-admins": admin
    "cloud-users": user
  session_secret: "REPLACE-WITH-AT-LEAST-32-BYTES-OF-RANDOM"

storage:
  backend: local
  layout: blob
  local:
    root: "/var/lib/newcloud/blobs"
  upload_dir: "/var/lib/newcloud/uploads"

limits:
  trash_retention_days: 30
  version_retention_days: 365
  blob_scrub_per_pass: 200

audit:
  retention_days: 365
  anchor_dir: "/mnt/audit-anchors"
```

Generate the session secret with `openssl rand -base64 48`. Do not reuse it across deployments:
it is the index key for every bearer token the system issues.
